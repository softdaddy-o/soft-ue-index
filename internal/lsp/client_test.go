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
