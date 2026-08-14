// Package lsp speaks the small, bounded subset of the Language Server Protocol used by soft-ue-index.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

var ErrClosed = errors.New("LSP client is closed")

type ProtocolError struct {
	Code    int
	Message string
}

func (e *ProtocolError) Error() string { return fmt.Sprintf("LSP error %d: %s", e.Code, e.Message) }

type ClientOptions struct {
	MaxMessageBytes int
	RequestTimeout  time.Duration
	Notification    func(string, json.RawMessage)
}
type Client struct {
	r         *bufio.Reader
	rawReader io.Reader
	w         io.Writer
	max       int
	timeout   time.Duration
	notify    func(string, json.RawMessage)
	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[uint64]chan wireMessage
	done      chan struct{}
	closed    bool
	next      atomic.Uint64
	wg        sync.WaitGroup
}

func NewClient(reader io.Reader, writer io.Writer, options ClientOptions) *Client {
	if options.MaxMessageBytes <= 0 {
		options.MaxMessageBytes = defaultMaxMessageBytes
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 15 * time.Second
	}
	c := &Client{r: bufio.NewReader(reader), rawReader: reader, w: writer, max: options.MaxMessageBytes, timeout: options.RequestTimeout, notify: options.Notification, pending: make(map[uint64]chan wireMessage), done: make(chan struct{})}
	c.wg.Add(1)
	go c.readLoop()
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
			if c.notify != nil {
				c.notify(m.Method, m.Params)
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
func (c *Client) terminate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
}
func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
}
func (c *Client) send(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return writeFrame(c.w, value)
}
func (c *Client) Notify(method string, params any) error {
	return c.send(wireMessage{JSONRPC: "2.0", Method: method, Params: mustJSON(params)})
}
func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
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
	if err := c.send(wireMessage{JSONRPC: "2.0", ID: mustJSON(id), Method: method, Params: mustJSON(params)}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		_ = c.Notify("$/cancelRequest", map[string]any{"id": id})
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
	if err := c.Call(ctx, "initialize", map[string]any{"processId": nil, "rootUri": rootURI, "capabilities": map[string]any{}}, &r); err != nil {
		return err
	}
	return c.Notify("initialized", map[string]any{})
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
	if closer, ok := c.rawReader.(io.Closer); ok {
		_ = closer.Close()
	}
	if closer, ok := c.w.(io.Closer); ok {
		_ = closer.Close()
	}
	c.wg.Wait()
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
	var out []Location
	err := c.Definition(ctx, p, &out)
	return limit(limits, out), err
}
func (c *Client) ReferenceLocations(ctx context.Context, p TextDocumentPosition, limits Limits) ([]Location, error) {
	var out []Location
	err := c.References(ctx, p, &out)
	return limit(limits, out), err
}
func (c *Client) Implementations(ctx context.Context, p TextDocumentPosition, limits Limits) ([]Location, error) {
	var out []Location
	err := c.Implementation(ctx, p, &out)
	return limit(limits, out), err
}
func (c *Client) DocumentSymbols(ctx context.Context, uri string, limits Limits) ([]DocumentSymbol, error) {
	var out []DocumentSymbol
	err := c.DocumentSymbol(ctx, uri, &out)
	return limit(limits, out), err
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
	err := c.PrepareCallHierarchy(ctx, p, &out)
	return limit(limits, out), err
}
func (c *Client) Definition(ctx context.Context, p TextDocumentPosition, out any) error {
	return c.Call(ctx, "textDocument/definition", map[string]any{"textDocument": map[string]string{"uri": p.URI}, "position": p.Position}, out)
}
func (c *Client) References(ctx context.Context, p TextDocumentPosition, out any) error {
	return c.Call(ctx, "textDocument/references", map[string]any{"textDocument": map[string]string{"uri": p.URI}, "position": p.Position, "context": map[string]bool{"includeDeclaration": true}}, out)
}
func (c *Client) Implementation(ctx context.Context, p TextDocumentPosition, out any) error {
	return c.Call(ctx, "textDocument/implementation", map[string]any{"textDocument": map[string]string{"uri": p.URI}, "position": p.Position}, out)
}
func (c *Client) DocumentSymbol(ctx context.Context, uri string, out any) error {
	return c.Call(ctx, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]string{"uri": uri}}, out)
}
func (c *Client) Hover(ctx context.Context, p TextDocumentPosition, out any) error {
	return c.Call(ctx, "textDocument/hover", map[string]any{"textDocument": map[string]string{"uri": p.URI}, "position": p.Position}, out)
}
func (c *Client) PrepareCallHierarchy(ctx context.Context, p TextDocumentPosition, out any) error {
	return c.Call(ctx, "textDocument/prepareCallHierarchy", map[string]any{"textDocument": map[string]string{"uri": p.URI}, "position": p.Position}, out)
}
func (c *Client) IncomingCalls(ctx context.Context, item any, out any) error {
	return c.Call(ctx, "callHierarchy/incomingCalls", map[string]any{"item": item}, out)
}
func (c *Client) OutgoingCalls(ctx context.Context, item any, out any) error {
	return c.Call(ctx, "callHierarchy/outgoingCalls", map[string]any{"item": item}, out)
}
func (c *Client) DidOpen(uri, languageID, text string) error {
	return c.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": languageID, "version": 1, "text": text}})
}
func (c *Client) DidClose(uri string) error {
	return c.Notify("textDocument/didClose", map[string]any{"textDocument": map[string]string{"uri": uri}})
}
