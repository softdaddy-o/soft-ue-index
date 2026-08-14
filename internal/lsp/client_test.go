package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSourceWriteChangesOpenDocumentWithMonotonicVersions(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{})
	defer c.Close()
	uri := "file:///game/Source/Foo.cpp"
	messages := make(chan wireMessage, 3)
	go func() {
		r := bufio.NewReader(b)
		for range 3 {
			body, _ := readFrame(r, 4096)
			var message wireMessage
			_ = json.Unmarshal(body, &message)
			messages <- message
		}
	}()
	if err := c.DidOpen(uri, "cpp", "old"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"new", "newest"} {
		if err := c.SourceFileChanged(uri, text); err != nil {
			t.Fatal(err)
		}
	}
	<-messages // didOpen
	for i, wantText := range []string{"new", "newest"} {
		wantVersion := i + 2
		message := <-messages
		if message.Method != "textDocument/didChange" {
			t.Fatalf("method=%s", message.Method)
		}
		var params struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params.TextDocument.URI != uri || params.TextDocument.Version != wantVersion || len(params.ContentChanges) != 1 || params.ContentChanges[0].Text != wantText {
			t.Fatalf("didChange=%+v", params)
		}
	}
}

func TestSourceWriteNotifiesWatchedFileWhenDocumentIsClosed(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{})
	defer c.Close()
	uri := "file:///game/Source/Foo.cpp"
	done := make(chan wireMessage, 1)
	go func() {
		body, _ := readFrame(bufio.NewReader(b), 4096)
		var message wireMessage
		_ = json.Unmarshal(body, &message)
		done <- message
	}()
	if err := c.SourceFileChanged(uri, "new"); err != nil {
		t.Fatal(err)
	}
	message := <-done
	if message.Method != "workspace/didChangeWatchedFiles" || !strings.Contains(string(message.Params), uri) || !strings.Contains(string(message.Params), `"type":2`) {
		t.Fatalf("message=%+v params=%s", message, message.Params)
	}
}

func TestSourceRemovalClosesOpenDocumentAndNotifiesDeletion(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{})
	defer c.Close()
	uri := "file:///game/Source/Foo.cpp"
	messages := make(chan wireMessage, 3)
	go func() {
		r := bufio.NewReader(b)
		for range 3 {
			body, _ := readFrame(r, 4096)
			var message wireMessage
			_ = json.Unmarshal(body, &message)
			messages <- message
		}
	}()
	if err := c.DidOpen(uri, "cpp", "old"); err != nil {
		t.Fatal(err)
	}
	if err := c.SourceFileRemoved(uri); err != nil {
		t.Fatal(err)
	}
	<-messages
	closeMessage, watchedMessage := <-messages, <-messages
	if closeMessage.Method != "textDocument/didClose" {
		t.Fatalf("first removal message=%s", closeMessage.Method)
	}
	if watchedMessage.Method != "workspace/didChangeWatchedFiles" || !strings.Contains(string(watchedMessage.Params), `"type":3`) {
		t.Fatalf("watched message=%+v params=%s", watchedMessage, watchedMessage.Params)
	}
}

func TestTerminateDoesNotCloseDetachedResponseChannel(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{})
	response := make(chan wireMessage, 1)
	c.mu.Lock()
	c.pending[1] = response
	c.mu.Unlock()
	c.terminate()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("late response panicked after termination: %v", recovered)
		}
	}()
	response <- wireMessage{}
	c.Close()
}

func TestCallTimeoutIncludesBlockedTransportWrite(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{RequestTimeout: 20 * time.Millisecond})
	defer c.Close()
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "blocked", nil, nil) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Call blocked beyond its request timeout while writing")
	}
}

func TestNotificationTimeoutClosesBlockedTransport(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{RequestTimeout: 20 * time.Millisecond})
	defer c.Close()
	done := make(chan error, 1)
	go func() { done <- c.DidOpen("file:///blocked.cpp", "cpp", "text") }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("notification blocked beyond request timeout")
	}
}

func TestDocumentRequestOpensFileOnceBeforeConcurrentDefinitions(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{})
	defer c.Close()
	path := filepath.Join(t.TempDir(), "with space.cpp")
	if err := os.WriteFile(path, []byte("int value;"), 0o600); err != nil {
		t.Fatal(err)
	}
	uri := pathURI(path)
	methods := make(chan string, 3)
	go func() {
		r := bufio.NewReader(b)
		for range 3 {
			body, _ := readFrame(r, 4096)
			var message wireMessage
			_ = json.Unmarshal(body, &message)
			methods <- message.Method
			if len(message.ID) != 0 {
				_, _ = b.Write(frameBytes(wireMessage{JSONRPC: "2.0", ID: message.ID, Result: mustJSON(json.RawMessage(`[]`))}))
			}
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Definitions(context.Background(), TextDocumentPosition{URI: uri}, Limits{})
			errs <- err
		}()
	}
	wg.Wait()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	got := []string{<-methods, <-methods, <-methods}
	if got[0] != "textDocument/didOpen" || got[1] != "textDocument/definition" || got[2] != "textDocument/definition" {
		t.Fatalf("methods=%v", got)
	}
}

func TestEnsureDocumentOpenRejectsUnsafeOrOversizedFiles(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{})
	defer c.Close()
	if err := c.ensureDocumentOpen("https://example.invalid/file.cpp"); err == nil {
		t.Fatal("expected non-file URI rejection")
	}
	path := filepath.Join(t.TempDir(), "large.cpp")
	if err := os.WriteFile(path, make([]byte, maxIndexSeedBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.ensureDocumentOpen(pathURI(path)); err == nil {
		t.Fatal("expected size rejection")
	}
	c.openMu.Lock()
	_, marked := c.opened[pathURI(path)]
	c.openMu.Unlock()
	if marked {
		t.Fatal("failed open was tracked")
	}
}

func TestClientCorrelatesSplitConcurrentResponses(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	c := NewClient(clientSide, clientSide, ClientOptions{})
	defer c.Close()
	go func() {
		r := bufio.NewReader(serverSide)
		var first, second wireMessage
		b, _ := readFrame(r, 1024)
		_ = json.Unmarshal(b, &first)
		b, _ = readFrame(r, 1024)
		_ = json.Unmarshal(b, &second)
		_, _ = serverSide.Write(frameBytes(wireMessage{JSONRPC: "2.0", ID: second.ID, Result: mustJSON("second")}))
		f := frameBytes(wireMessage{JSONRPC: "2.0", ID: first.ID, Result: mustJSON("first")})
		_, _ = serverSide.Write(f[:7])
		_, _ = serverSide.Write(f[7:])
	}()
	var a, b string
	done := make(chan error, 2)
	go func() { done <- c.Call(context.Background(), "a", nil, &a) }()
	go func() { done <- c.Call(context.Background(), "b", nil, &b) }()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if (a != "first" && a != "second") || (b != "first" && b != "second") || a == b {
		t.Fatalf("got %q %q", a, b)
	}
}
func TestClientCancelsTimeout(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{})
	defer c.Close()
	got := make(chan wireMessage, 1)
	go func() {
		r := bufio.NewReader(b)
		body, _ := readFrame(r, 1024)
		var request wireMessage
		_ = json.Unmarshal(body, &request)
		body, _ = readFrame(r, 1024)
		var cancel wireMessage
		_ = json.Unmarshal(body, &cancel)
		got <- cancel
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "slow", nil, nil); err == nil {
		t.Fatal("expected timeout")
	}
	select {
	case m := <-got:
		if m.Method != "$/cancelRequest" {
			t.Fatal(m.Method)
		}
	case <-time.After(time.Second):
		t.Fatal("no cancellation")
	}
}

func TestClientCapsLaterCallerDeadlineAtRequestTimeout(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{RequestTimeout: 20 * time.Millisecond})
	defer c.Close()
	go func() {
		r := bufio.NewReader(b)
		_, _ = readFrame(r, 1024)
		_, _ = readFrame(r, 1024) // $/cancelRequest
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := c.Call(ctx, "slow", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 150*time.Millisecond {
		t.Fatalf("request used caller deadline instead of client timeout: %v", elapsed)
	}
}
func TestClientRejectsOversizedFrame(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{MaxMessageBytes: 4})
	defer c.Close()
	go func() { _, _ = b.Write([]byte("Content-Length: 5\r\n\r\n12345")) }()
	time.Sleep(10 * time.Millisecond)
	if err := c.Call(context.Background(), "x", nil, nil); err == nil {
		t.Fatal("expected closed")
	}
}

func TestReadFrameRejectsOversizedHeader(t *testing.T) {
	_, err := readFrame(bufio.NewReader(strings.NewReader("X: "+strings.Repeat("a", 9000)+"\r\n")), 1024)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("got %v", err)
	}
}
func TestLimitsTruncateTypedResults(t *testing.T) {
	got := limit(Limits{MaxItems: 1}, []Symbol{{Name: "a"}, {Name: "b"}})
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("%#v", got)
	}
}

func TestCallHierarchyTypedPrimitivesDecodeProtocolRecords(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	c := NewClient(clientSide, clientSide, ClientOptions{})
	defer c.Close()
	path := filepath.Join(t.TempDir(), "a.cpp")
	if err := os.WriteFile(path, []byte("void a();"), 0o600); err != nil {
		t.Fatal(err)
	}
	uri := pathURI(path)
	go func() {
		r := bufio.NewReader(serverSide)
		_, _ = readFrame(r, 4096) // textDocument/didOpen
		for _, result := range []string{
			`[{"name":"root","uri":"file:///a.cpp","range":{"start":{},"end":{}},"selectionRange":{"start":{},"end":{}}}]`,
			`[{"from":{"name":"caller","uri":"file:///b.cpp","range":{"start":{},"end":{}},"selectionRange":{"start":{},"end":{}}}}]`,
			`[{"to":{"name":"callee","uri":"file:///c.cpp","range":{"start":{},"end":{}},"selectionRange":{"start":{},"end":{}}}}]`,
		} {
			body, _ := readFrame(r, 4096)
			var request wireMessage
			_ = json.Unmarshal(body, &request)
			_, _ = serverSide.Write(frameBytes(wireMessage{JSONRPC: "2.0", ID: request.ID, Result: mustJSON(json.RawMessage(result))}))
		}
	}()
	position := TextDocumentPosition{URI: uri}
	roots, err := c.PrepareCallHierarchy(context.Background(), position)
	if err != nil || len(roots) != 1 || roots[0].Name != "root" {
		t.Fatalf("prepare=%#v err=%v", roots, err)
	}
	incoming, err := c.IncomingCalls(context.Background(), roots[0])
	if err != nil || len(incoming) != 1 || incoming[0].From == nil || incoming[0].From.Name != "caller" {
		t.Fatalf("incoming=%#v err=%v", incoming, err)
	}
	outgoing, err := c.OutgoingCalls(context.Background(), roots[0])
	if err != nil || len(outgoing) != 1 || outgoing[0].To == nil || outgoing[0].To.Name != "callee" {
		t.Fatalf("outgoing=%#v err=%v", outgoing, err)
	}
}
func TestDecodeLocationUnionsAndDocumentLimit(t *testing.T) {
	for _, raw := range []string{`{"uri":"file:///a","range":{"start":{},"end":{}}}`, `[{"targetUri":"file:///b","targetRange":{"start":{},"end":{}},"targetSelectionRange":{"start":{},"end":{}}}]`, `null`} {
		got, err := decodeLocations(json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		if raw != "null" && len(got) != 1 {
			t.Fatalf("%s: %#v", raw, got)
		}
	}
	got := limitDocumentSymbols([]DocumentSymbol{{Name: "a", Children: []DocumentSymbol{{Name: "b"}}}, {Name: "c"}}, 2)
	if len(got) != 1 || len(got[0].Children) != 1 {
		t.Fatalf("%#v", got)
	}
}
func TestHoverUnion(t *testing.T) {
	var h HoverResult
	if err := json.Unmarshal([]byte(`{"contents":"text"}`), &h); err != nil || h.Contents.Value != "text" {
		t.Fatalf("%#v %v", h, err)
	}
}
func TestClientRepliesToServerConfigurationRequest(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a, a, ClientOptions{})
	defer c.Close()
	done := make(chan error, 1)
	go func() {
		r := bufio.NewReader(b)
		body, _ := readFrame(r, 1024)
		var q wireMessage
		_ = json.Unmarshal(body, &q)
		_, _ = b.Write(frameBytes(wireMessage{JSONRPC: "2.0", ID: mustJSON(77), Method: "workspace/configuration", Params: mustJSON(map[string]any{"items": []any{map[string]any{}}})}))
		body, _ = readFrame(r, 1024)
		var reply wireMessage
		_ = json.Unmarshal(body, &reply)
		if string(reply.ID) != "77" {
			done <- errors.New("bad server reply")
			return
		}
		_, _ = b.Write(frameBytes(wireMessage{JSONRPC: "2.0", ID: q.ID, Result: mustJSON([]any{})}))
		done <- nil
	}()
	var out []Symbol
	if err := c.WorkspaceSymbol(context.Background(), "x", &out); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestServerRequestFloodReturnsOneBoundedResponsePerRequest(t *testing.T) {
	a, b := net.Pipe()
	c := NewClient(a, a, ClientOptions{})
	const count = 80
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for i := 0; i < count; i++ {
			_, _ = b.Write(frameBytes(wireMessage{JSONRPC: "2.0", ID: mustJSON(i), Method: "workspace/configuration", Params: mustJSON(map[string]any{"items": []any{}})}))
		}
	}()
	// Let the bounded queue fill while the server deliberately withholds reads.
	time.Sleep(20 * time.Millisecond)
	r := bufio.NewReader(b)
	seen := map[int]bool{}
	overloaded := false
	for len(seen) < count {
		body, err := readFrame(r, 1024*1024)
		if err != nil {
			t.Fatal(err)
		}
		var reply wireMessage
		if err := json.Unmarshal(body, &reply); err != nil {
			t.Fatal(err)
		}
		var id int
		if err := json.Unmarshal(reply.ID, &id); err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate reply %d", id)
		}
		seen[id] = true
		if reply.Error != nil && reply.Error.Code == -32000 {
			overloaded = true
		}
	}
	<-writeDone
	if !overloaded {
		t.Fatal("expected bounded queue overload")
	}
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close blocked by flood")
	}
	_ = b.Close()
}
