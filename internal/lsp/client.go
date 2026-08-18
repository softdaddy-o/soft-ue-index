// Package lsp speaks the small, bounded subset of the Language Server Protocol used by soft-ue-index.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

var ErrClosed = errors.New("LSP client is closed")

// ErrIndexBuilding is returned when a caller's context expires while
// WaitForIndexReady is still waiting on clangd's background index. It is
// deliberately distinct from context.DeadlineExceeded so callers can surface
// one actionable message instead of a generic timeout.
var ErrIndexBuilding = errors.New("background index is still building")

type ProtocolError struct {
	Code    int
	Message string
}

func (e *ProtocolError) Error() string { return fmt.Sprintf("LSP error %d: %s", e.Code, e.Message) }

type ClientOptions struct {
	MaxMessageBytes int
	RequestTimeout  time.Duration
	Notification    func(string, json.RawMessage)
	// IndexGraceTimeout bounds how long the client waits, after startup, for
	// clangd to announce a workDoneProgress before assuming no background
	// indexing is needed (an already-warm index, or a server that never
	// advertises progress). It does not bound an indexing run that has
	// already announced itself: that phase only ends on its own "end" event
	// or on the caller's own context.
	IndexGraceTimeout time.Duration
}
type Client struct {
	r         *bufio.Reader
	rawReader io.Reader
	w         io.Writer
	max       int
	timeout   time.Duration
	notify    func(string, json.RawMessage)
	mu        sync.Mutex
	pending   map[uint64]chan wireMessage
	done      chan struct{}
	closed    bool
	next      atomic.Uint64
	wg        sync.WaitGroup
	requests  chan wireMessage
	writes    chan writeRequest
	closeOnce sync.Once
	openMu    sync.Mutex
	opened    map[string]int

	// progressMu guards the background-index readiness state derived from
	// clangd's window/workDoneProgress requests and $/progress notifications.
	progressMu   sync.Mutex
	activeTokens map[string]struct{}
	indexMessage string
	indexReady   bool
	sawProgress  bool
	readyCh      chan struct{}
	graceTimeout time.Duration
	graceTimer   *time.Timer
}
type writeRequest struct {
	value any
	done  chan error
}

func NewClient(reader io.Reader, writer io.Writer, options ClientOptions) *Client {
	if options.MaxMessageBytes <= 0 {
		options.MaxMessageBytes = defaultMaxMessageBytes
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 30 * time.Second
	}
	if options.IndexGraceTimeout <= 0 {
		options.IndexGraceTimeout = 2 * time.Second
	}
	c := &Client{r: bufio.NewReader(reader), rawReader: reader, w: writer, max: options.MaxMessageBytes, timeout: options.RequestTimeout, notify: options.Notification, pending: make(map[uint64]chan wireMessage), done: make(chan struct{}), requests: make(chan wireMessage, 32), writes: make(chan writeRequest, 32), opened: make(map[string]int), activeTokens: make(map[string]struct{}), readyCh: make(chan struct{}), graceTimeout: options.IndexGraceTimeout}
	c.wg.Add(3)
	go c.readLoop()
	go c.requestLoop()
	go c.writeLoop()
	return c
}
func (c *Client) readLoop() {
	defer c.wg.Done()
	defer c.terminate()
	for {
		body, err := readFrame(c.r, c.max)
		if err != nil {
			return
		}
		var m wireMessage
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		if m.Method != "" && len(m.ID) == 0 {
			if m.Method == "$/progress" {
				c.handleProgress(m.Params)
				continue
			}
			if c.notify != nil {
				c.notify(m.Method, m.Params)
			}
			continue
		}
		if m.Method != "" && len(m.ID) != 0 {
			select {
			case c.requests <- m:
			case <-c.done:
				return
			default:
				// Queue saturation is explicitly reported; send is serialized and Close
				// closes the transport to unblock a misbehaving peer.
				_ = c.send(wireMessage{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: -32000, Message: "server request queue full"}})
			}
			continue
		}
		var id uint64
		if json.Unmarshal(m.ID, &id) != nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			default:
			}
		}
	}
}
func (c *Client) requestLoop() {
	defer c.wg.Done()
	for {
		select {
		case m := <-c.requests:
			c.respondServerRequest(m)
		case <-c.done:
			return
		}
	}
}
func (c *Client) writeLoop() {
	defer c.wg.Done()
	for {
		select {
		case request := <-c.writes:
			err := writeFrame(c.w, request.value)
			request.done <- err
			if err != nil {
				c.terminate()
				return
			}
		case <-c.done:
			return
		}
	}
}
func (c *Client) respondServerRequest(m wireMessage) {
	if m.Method == "workspace/configuration" {
		var p struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(m.Params, &p)
		result := make([]any, len(p.Items))
		_ = c.send(wireMessage{JSONRPC: "2.0", ID: m.ID, Result: mustJSON(result)})
		return
	}
	if m.Method == "window/workDoneProgress/create" {
		// Acknowledge unconditionally: the corresponding $/progress
		// notifications are what drive index-readiness state, not this create.
		_ = c.send(wireMessage{JSONRPC: "2.0", ID: m.ID, Result: mustJSON(nil)})
		return
	}
	_ = c.send(wireMessage{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: -32601, Message: "Method not found"}})
}
func (c *Client) terminate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	for id := range c.pending {
		delete(c.pending, id)
	}
	if c.graceTimer != nil {
		c.graceTimer.Stop()
	}
}
func (c *Client) sendContext(ctx context.Context, value any) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	request := writeRequest{value: value, done: make(chan error, 1)}
	select {
	case c.writes <- request:
	case <-ctx.Done():
		c.closeTransport()
		c.terminate()
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		c.closeTransport()
		c.terminate()
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	}
}
func (c *Client) send(value any) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	return c.sendContext(ctx, value)
}
func (c *Client) Notify(method string, params any) error {
	return c.send(wireMessage{JSONRPC: "2.0", Method: method, Params: mustJSON(params)})
}
func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	id := c.next.Add(1)
	response := make(chan wireMessage, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.pending[id] = response
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.sendContext(ctx, wireMessage{JSONRPC: "2.0", ID: mustJSON(id), Method: method, Params: mustJSON(params)}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		_ = c.sendContext(cancelCtx, wireMessage{JSONRPC: "2.0", Method: "$/cancelRequest", Params: mustJSON(map[string]any{"id": id})})
		cancel()
		return ctx.Err()
	case m, ok := <-response:
		if !ok {
			return ErrClosed
		}
		if m.Error != nil {
			return &ProtocolError{m.Error.Code, m.Error.Message}
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	case <-c.done:
		return ErrClosed
	}
}
func (c *Client) Initialize(ctx context.Context, rootURI string) error {
	var r any
	// window.workDoneProgress lets clangd report background-index progress via
	// window/workDoneProgress/create + $/progress, which IndexPhase and
	// WaitForIndexReady depend on.
	capabilities := map[string]any{"window": map[string]any{"workDoneProgress": true}}
	if err := c.Call(ctx, "initialize", map[string]any{"processId": nil, "rootUri": rootURI, "capabilities": capabilities}, &r); err != nil {
		return err
	}
	return c.Notify("initialized", map[string]any{})
}

// handleProgress updates index-readiness state from a $/progress
// notification. Any workDoneProgress token is treated as background
// indexing: clangd does not otherwise use server-initiated workDoneProgress.
func (c *Client) handleProgress(raw json.RawMessage) {
	var p struct {
		Token json.RawMessage `json:"token"`
		Value struct {
			Kind    string `json:"kind"`
			Title   string `json:"title"`
			Message string `json:"message"`
		} `json:"value"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	token := string(p.Token)
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	c.sawProgress = true
	switch p.Value.Kind {
	case "begin":
		c.activeTokens[token] = struct{}{}
		c.indexMessage = firstNonEmpty(p.Value.Title, p.Value.Message)
	case "report":
		if msg := firstNonEmpty(p.Value.Message, p.Value.Title); msg != "" {
			c.indexMessage = msg
		}
	case "end":
		delete(c.activeTokens, token)
		if len(c.activeTokens) == 0 {
			c.markIndexReadyLocked()
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// markIndexReadyLocked closes readyCh at most once. Callers must hold progressMu.
func (c *Client) markIndexReadyLocked() {
	if !c.indexReady {
		c.indexReady = true
		c.indexMessage = ""
		close(c.readyCh)
	}
}

// StartIndexGraceWindow arms the grace-timeout fallback that marks the index
// ready if clangd never announces a workDoneProgress. Callers must invoke
// this exactly once, only after telling clangd everything it initially
// needs to index (after Initialize and any seed DidOpen): arming it any
// earlier would let the timer expire before indexing was ever triggered,
// wrongly reporting "ready" on a session that has not started indexing yet.
// It is a no-op if the index is already known to be ready.
func (c *Client) StartIndexGraceWindow() {
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	if c.graceTimer != nil || c.indexReady {
		return
	}
	c.graceTimer = time.AfterFunc(c.graceTimeout, func() {
		c.progressMu.Lock()
		if !c.sawProgress && len(c.activeTokens) == 0 {
			c.markIndexReadyLocked()
		}
		c.progressMu.Unlock()
	})
}

// IndexPhase reports the client's current view of clangd's background index
// without blocking: "ready" once indexing has finished (or was never
// needed), "indexing" while a workDoneProgress is active, and "starting"
// immediately after startup, before the first progress signal or grace
// timeout arrives.
func (c *Client) IndexPhase() string {
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	switch {
	case c.indexReady:
		return "ready"
	case len(c.activeTokens) > 0:
		return "indexing"
	default:
		return "starting"
	}
}

// IndexMessage returns clangd's latest progress title/message while indexing,
// or "" once ready or before any progress has been observed.
func (c *Client) IndexMessage() string {
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	return c.indexMessage
}

// WaitForIndexReady blocks until the background index is ready (or was never
// needed) or ctx is done. It never sleeps a fixed duration: readiness is
// signaled by closing readyCh from handleProgress or the startup grace timer.
func (c *Client) WaitForIndexReady(ctx context.Context) error {
	c.progressMu.Lock()
	ready := c.indexReady
	ch := c.readyCh
	c.progressMu.Unlock()
	if ready {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrIndexBuilding, ctx.Err())
	case <-c.done:
		return ErrClosed
	}
}
func (c *Client) Shutdown(ctx context.Context) error {
	var r any
	err := c.Call(ctx, "shutdown", nil, &r)
	if err == nil {
		_ = c.Notify("exit", nil)
	}
	return err
}
func (c *Client) Close() {
	c.terminate()
	c.closeTransport()
	c.wg.Wait()
}
func (c *Client) closeTransport() {
	c.closeOnce.Do(func() {
		if closer, ok := c.rawReader.(io.Closer); ok {
			_ = closer.Close()
		}
		if closer, ok := c.w.(io.Closer); ok {
			_ = closer.Close()
		}
	})
}

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Location and the result types below normalize clangd's LSP unions for callers.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}
type LocationLink struct {
	TargetURI            string `json:"targetUri"`
	TargetRange          Range  `json:"targetRange"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}
type Symbol struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
}
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}
type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}
type HoverResult struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

func (h *HoverResult) UnmarshalJSON(data []byte) error {
	var raw struct {
		Contents json.RawMessage `json:"contents"`
		Range    *Range          `json:"range"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	h.Range = raw.Range
	var markup MarkupContent
	if json.Unmarshal(raw.Contents, &markup) == nil && markup.Value != "" {
		h.Contents = markup
		return nil
	}
	var text string
	if json.Unmarshal(raw.Contents, &text) == nil {
		h.Contents = MarkupContent{Kind: "plaintext", Value: text}
		return nil
	}
	var many []any
	if err := json.Unmarshal(raw.Contents, &many); err != nil {
		return err
	}
	b, _ := json.Marshal(many)
	h.Contents = MarkupContent{Kind: "plaintext", Value: string(b)}
	return nil
}

type CallHierarchyItem struct {
	Name           string `json:"name"`
	Kind           int    `json:"kind"`
	URI            string `json:"uri"`
	Range          Range  `json:"range"`
	SelectionRange Range  `json:"selectionRange"`
}
type CallHierarchyCall struct {
	From       *CallHierarchyItem `json:"from,omitempty"`
	To         *CallHierarchyItem `json:"to,omitempty"`
	FromRanges []Range            `json:"fromRanges,omitempty"`
}
type Limits struct{ MaxItems int }

func limit[T any](l Limits, items []T) []T {
	if l.MaxItems > 0 && len(items) > l.MaxItems {
		return items[:l.MaxItems]
	}
	return items
}
func limitDocumentSymbols(items []DocumentSymbol, max int) []DocumentSymbol {
	if max <= 0 {
		return items
	}
	remaining := max
	var visit func([]DocumentSymbol) []DocumentSymbol
	visit = func(in []DocumentSymbol) []DocumentSymbol {
		out := make([]DocumentSymbol, 0, len(in))
		for _, v := range in {
			if remaining == 0 {
				break
			}
			remaining--
			v.Children = visit(v.Children)
			out = append(out, v)
		}
		return out
	}
	return visit(items)
}
func decodeLocations(data json.RawMessage) ([]Location, error) {
	if string(data) == "null" {
		return nil, nil
	}
	var many []Location
	if json.Unmarshal(data, &many) == nil {
		return many, nil
	}
	var one Location
	if json.Unmarshal(data, &one) == nil && one.URI != "" {
		return []Location{one}, nil
	}
	var links []LocationLink
	if err := json.Unmarshal(data, &links); err != nil {
		return nil, err
	}
	out := make([]Location, 0, len(links))
	for _, x := range links {
		out = append(out, Location{URI: x.TargetURI, Range: x.TargetSelectionRange})
	}
	return out, nil
}

type TextDocumentPosition struct {
	URI      string   `json:"uri"`
	Position Position `json:"position"`
}

func (c *Client) WorkspaceSymbol(ctx context.Context, query string, out any) error {
	return c.Call(ctx, "workspace/symbol", map[string]any{"query": query}, out)
}
func (c *Client) WorkspaceSymbols(ctx context.Context, query string, limits Limits) ([]Symbol, error) {
	var out []Symbol
	err := c.WorkspaceSymbol(ctx, query, &out)
	return limit(limits, out), err
}
func (c *Client) Definitions(ctx context.Context, p TextDocumentPosition, limits Limits) ([]Location, error) {
	var raw json.RawMessage
	err := c.Definition(ctx, p, &raw)
	out, decodeErr := decodeLocations(raw)
	if err == nil {
		err = decodeErr
	}
	return limit(limits, out), err
}
func (c *Client) ReferenceLocations(ctx context.Context, p TextDocumentPosition, limits Limits) ([]Location, error) {
	var raw json.RawMessage
	err := c.References(ctx, p, &raw)
	out, decodeErr := decodeLocations(raw)
	if err == nil {
		err = decodeErr
	}
	return limit(limits, out), err
}
func (c *Client) Implementations(ctx context.Context, p TextDocumentPosition, limits Limits) ([]Location, error) {
	var raw json.RawMessage
	err := c.Implementation(ctx, p, &raw)
	out, decodeErr := decodeLocations(raw)
	if err == nil {
		err = decodeErr
	}
	return limit(limits, out), err
}
func (c *Client) DocumentSymbols(ctx context.Context, uri string, limits Limits) ([]DocumentSymbol, error) {
	var out []DocumentSymbol
	err := c.DocumentSymbol(ctx, uri, &out)
	return limitDocumentSymbols(out, limits.MaxItems), err
}
func (c *Client) HoverResult(ctx context.Context, p TextDocumentPosition) (*HoverResult, error) {
	var out HoverResult
	err := c.Hover(ctx, p, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) CallHierarchy(ctx context.Context, p TextDocumentPosition, limits Limits) ([]CallHierarchyItem, error) {
	var out []CallHierarchyItem
	err := c.prepareCallHierarchy(ctx, p, &out)
	return limit(limits, out), err
}
func (c *Client) Definition(ctx context.Context, p TextDocumentPosition, out any) error {
	if err := c.ensureDocumentOpen(p.URI); err != nil {
		return err
	}
	return c.Call(ctx, "textDocument/definition", map[string]any{"textDocument": map[string]string{"uri": p.URI}, "position": p.Position}, out)
}
func (c *Client) References(ctx context.Context, p TextDocumentPosition, out any) error {
	if err := c.ensureDocumentOpen(p.URI); err != nil {
		return err
	}
	return c.Call(ctx, "textDocument/references", map[string]any{"textDocument": map[string]string{"uri": p.URI}, "position": p.Position, "context": map[string]bool{"includeDeclaration": true}}, out)
}
func (c *Client) Implementation(ctx context.Context, p TextDocumentPosition, out any) error {
	if err := c.ensureDocumentOpen(p.URI); err != nil {
		return err
	}
	return c.Call(ctx, "textDocument/implementation", map[string]any{"textDocument": map[string]string{"uri": p.URI}, "position": p.Position}, out)
}
func (c *Client) DocumentSymbol(ctx context.Context, uri string, out any) error {
	if err := c.ensureDocumentOpen(uri); err != nil {
		return err
	}
	return c.Call(ctx, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]string{"uri": uri}}, out)
}
func (c *Client) Hover(ctx context.Context, p TextDocumentPosition, out any) error {
	if err := c.ensureDocumentOpen(p.URI); err != nil {
		return err
	}
	return c.Call(ctx, "textDocument/hover", map[string]any{"textDocument": map[string]string{"uri": p.URI}, "position": p.Position}, out)
}

// PrepareCallHierarchy returns the starting call hierarchy items at a source
// position. IncomingCalls and OutgoingCalls return the protocol's typed call
// records, whose From/To endpoint is selected by the caller.
func (c *Client) PrepareCallHierarchy(ctx context.Context, p TextDocumentPosition) ([]CallHierarchyItem, error) {
	var out []CallHierarchyItem
	err := c.prepareCallHierarchy(ctx, p, &out)
	return out, err
}
func (c *Client) prepareCallHierarchy(ctx context.Context, p TextDocumentPosition, out any) error {
	if err := c.ensureDocumentOpen(p.URI); err != nil {
		return err
	}
	return c.Call(ctx, "textDocument/prepareCallHierarchy", map[string]any{"textDocument": map[string]string{"uri": p.URI}, "position": p.Position}, out)
}
func (c *Client) IncomingCalls(ctx context.Context, item CallHierarchyItem) ([]CallHierarchyCall, error) {
	var out []CallHierarchyCall
	err := c.Call(ctx, "callHierarchy/incomingCalls", map[string]any{"item": item}, &out)
	return out, err
}
func (c *Client) OutgoingCalls(ctx context.Context, item CallHierarchyItem) ([]CallHierarchyCall, error) {
	var out []CallHierarchyCall
	err := c.Call(ctx, "callHierarchy/outgoingCalls", map[string]any{"item": item}, &out)
	return out, err
}
func (c *Client) DidOpen(uri, languageID, text string) error {
	c.openMu.Lock()
	defer c.openMu.Unlock()
	if _, ok := c.opened[uri]; ok {
		return nil
	}
	if err := c.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": languageID, "version": 1, "text": text}}); err != nil {
		return err
	}
	c.opened[uri] = 1
	return nil
}
func (c *Client) DidClose(uri string) error {
	c.openMu.Lock()
	defer c.openMu.Unlock()
	if err := c.Notify("textDocument/didClose", map[string]any{"textDocument": map[string]string{"uri": uri}}); err != nil {
		return err
	}
	delete(c.opened, uri)
	return nil
}

// SourceFileChanged refreshes an open document's in-memory AST. Closed files
// are delegated to clangd's background index through the watched-files event.
func (c *Client) SourceFileChanged(uri, text string, watchedType ...int) error {
	c.openMu.Lock()
	defer c.openMu.Unlock()
	version, open := c.opened[uri]
	if !open {
		changeType := 2
		if len(watchedType) > 0 && watchedType[0] == 1 {
			changeType = 1
		}
		return c.Notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []map[string]any{{"uri": uri, "type": changeType}}})
	}
	version++
	if err := c.Notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]string{{"text": text}},
	}); err != nil {
		return err
	}
	c.opened[uri] = version
	return nil
}

// SourceFileRemoved forgets any open in-memory document before telling clangd
// that the backing file disappeared.
func (c *Client) SourceFileRemoved(uri string) error {
	c.openMu.Lock()
	defer c.openMu.Unlock()
	if _, open := c.opened[uri]; open {
		if err := c.Notify("textDocument/didClose", map[string]any{"textDocument": map[string]string{"uri": uri}}); err != nil {
			return err
		}
		delete(c.opened, uri)
	}
	return c.Notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []map[string]any{{"uri": uri, "type": 3}}})
}

func (c *Client) ensureDocumentOpen(uri string) error {
	c.openMu.Lock()
	defer c.openMu.Unlock()
	if _, ok := c.opened[uri]; ok {
		return nil
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Host != "" {
		return errors.New("document URI must be a local file URI")
	}
	path := filepath.FromSlash(u.Path)
	if len(path) >= 3 && (path[0] == '/' || path[0] == '\\') && path[2] == ':' {
		path = path[1:]
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("open document: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("document is not a regular file")
	}
	if info.Size() > maxIndexSeedBytes {
		return errors.New("document exceeds read limit")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open document: %w", err)
	}
	b, readErr := io.ReadAll(io.LimitReader(f, maxIndexSeedBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(b) > maxIndexSeedBytes {
		return errors.New("document exceeds read limit")
	}
	if err := c.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "cpp", "version": 1, "text": string(b)}}); err != nil {
		return err
	}
	c.opened[uri] = 1
	return nil
}
