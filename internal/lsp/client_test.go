package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

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
	go func() {
		r := bufio.NewReader(serverSide)
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
	position := TextDocumentPosition{URI: "file:///a.cpp"}
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
