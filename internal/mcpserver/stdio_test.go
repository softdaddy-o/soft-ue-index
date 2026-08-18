package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBoundedJSONWriterNeverEmitsOversizedFrame(t *testing.T) {
	var out bytes.Buffer
	w := newBoundedJSONWriter(&out, 32)
	if _, err := w.Write([]byte(`{"jsonrpc":"2.0","id":1}` + "\n")); err != nil {
		t.Fatal(err)
	}
	before := out.String()
	if _, err := w.Write([]byte(`{"result":"` + strings.Repeat("x", 40) + `"}` + "\n")); !errors.Is(err, ErrResponseFrameTooLarge) {
		t.Fatalf("error=%v", err)
	}
	if out.String() != before {
		t.Fatalf("oversized frame leaked: %q", out.String())
	}
}

func TestBoundedJSONWriterRejectsUnderlyingShortWrite(t *testing.T) {
	w := newBoundedJSONWriter(shortWriter{}, 64)
	if _, err := w.Write([]byte("{}\n")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error=%v", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestBoundedReaderRejectsReflectedUnknownNamesBeforeSDK(t *testing.T) {
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"` + strings.Repeat("m", 60000) + `"}` + "\n",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + strings.Repeat("t", 60000) + `"}}` + "\n",
	} {
		reader := newBoundedJSONReader(strings.NewReader(frame), 65536, 128)
		_, err := reader.Read(make([]byte, 65536))
		if !errors.Is(err, ErrRequestFrameTooLarge) {
			t.Fatalf("error=%v", err)
		}
	}
}

func TestBoundedReaderPreservesValidInitializeAndToolCall(t *testing.T) {
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}` + "\n",
	} {
		got, err := io.ReadAll(newBoundedJSONReader(strings.NewReader(frame), 65536, 128))
		if err != nil || string(got) != frame {
			t.Fatalf("got=%q err=%v", got, err)
		}
	}
}

func TestBoundedReaderRejectsMalformedToolFloodBeforeDispatch(t *testing.T) {
	frame := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_projects","arguments":{"max_items":"wrong"}}}` + "\n"
	before := runtime.NumGoroutine()
	for range 1000 {
		reader := newBoundedJSONReader(strings.NewReader(frame), 1024, 128)
		if _, err := io.ReadAll(reader); !errors.Is(err, ErrRequestFrameTooLarge) {
			t.Fatalf("error=%v", err)
		}
	}
	if delta := runtime.NumGoroutine() - before; delta > 1 {
		t.Fatalf("malformed ingress spawned goroutines: delta=%d", delta)
	}
}

func TestBoundedReaderRejectsJSONRPCBatchBeforeDispatch(t *testing.T) {
	batch := `[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}]` + "\n"
	reader := newBoundedJSONReader(strings.NewReader(batch), 4096, 128)
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrRequestFrameTooLarge) {
		t.Fatalf("batch error=%v", err)
	}
}

func TestProxyFramesPreservesSplitAndMultipleFrames(t *testing.T) {
	requestIn := &splitReader{parts: [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}`),
		[]byte("}\n"),
		[]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}`),
		[]byte("\n"),
	}}
	responseIn, responseFeed := io.Pipe()
	go func() { _, _ = responseFeed.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")) }()
	requestOut := &bytes.Buffer{}
	responseOut := &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ProxyFrames(ctx, requestIn, requestOut, responseIn, responseOut, FrameProxyCaps{
		MaxRequestBytes:   128,
		MaxRequestIDBytes: 128,
		MaxResponseBytes:  128,
	}); err != nil {
		t.Fatalf("proxy=%v", err)
	}
	gotReq := requestOut.String()
	if !strings.HasSuffix(gotReq, "\n") || strings.Count(gotReq, "\n") != 2 {
		t.Fatalf("bad request forwarding: %q", gotReq)
	}
	if gotReq != `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"+`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}`+"\n" {
		t.Fatalf("request frame corruption: %q", gotReq)
	}
	if got := responseOut.String(); got != `{"jsonrpc":"2.0","id":1,"result":{}}`+"\n" {
		t.Fatalf("response frame corruption: %q", got)
	}
}

func TestProxyFramesRejectsOversizedRequestFrame(t *testing.T) {
	requestIn := strings.NewReader(strings.Repeat("x", 2048) + "\n")
	responseIn, _ := io.Pipe()
	requestOut := &bytes.Buffer{}
	responseOut := &bytes.Buffer{}
	caps := FrameProxyCaps{
		MaxRequestBytes:   16,
		MaxRequestIDBytes: 128,
		MaxResponseBytes:  128,
	}
	err := ProxyFrames(context.Background(), requestIn, requestOut, responseIn, responseOut, caps)
	if !errors.Is(err, ErrRequestFrameTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestProxyFramesRejectsOversizedResponseFrame(t *testing.T) {
	requestIn, _ := io.Pipe()
	responseIn := strings.NewReader(strings.Repeat("x", 1024) + "\n")
	requestOut := &bytes.Buffer{}
	responseOut := &bytes.Buffer{}
	caps := FrameProxyCaps{
		MaxRequestBytes:   32,
		MaxRequestIDBytes: 128,
		MaxResponseBytes:  16,
	}
	err := ProxyFrames(context.Background(), requestIn, requestOut, responseIn, responseOut, caps)
	if !errors.Is(err, ErrResponseFrameTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestProxyFramesRejectsUnterminatedFrames(t *testing.T) {
	requestIn := strings.NewReader(`{"jsonrpc":"2.0","id":1}`)
	responseIn := strings.NewReader(`{"jsonrpc":"2.0","id":1}`)
	requestOut := &bytes.Buffer{}
	responseOut := &bytes.Buffer{}
	caps := FrameProxyCaps{
		MaxRequestBytes:   1024,
		MaxRequestIDBytes: 128,
		MaxResponseBytes:  1024,
	}
	if err := ProxyFrames(context.Background(), requestIn, requestOut, responseIn, responseOut, caps); !(errors.Is(err, ErrRequestFrameTooLarge) || errors.Is(err, ErrResponseFrameTooLarge)) {
		t.Fatalf("frame err=%v", err)
	}
}

func TestProxyFramesCancelsActiveForwarding(t *testing.T) {
	reqR, reqW := io.Pipe()
	resR, resW := io.Pipe()
	requestOut := &bytes.Buffer{}
	responseOut := &bytes.Buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		err := ProxyFrames(ctx, reqR, requestOut, resR, responseOut, FrameProxyCaps{
			MaxRequestBytes:   32,
			MaxRequestIDBytes: 128,
			MaxResponseBytes:  32,
		})
		done <- err
	}()
	cancel()
	_ = reqW.Close()
	_ = resW.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && err != nil {
			t.Fatalf("got=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not cancel")
	}
}

func TestProxyFramesReturnsWhenClientInputClosesAndDaemonSideStaysOpen(t *testing.T) {
	daemonRead, daemonWrite := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- ProxyFrames(context.Background(), strings.NewReader(""), daemonWrite, daemonRead, &bytes.Buffer{}, FrameProxyCaps{})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not terminate after client EOF")
	}
}

type splitReader struct {
	parts [][]byte
	index int
	off   int
}

func (r *splitReader) Read(p []byte) (int, error) {
	if r.index >= len(r.parts) {
		return 0, io.EOF
	}
	part := r.parts[r.index][r.off:]
	n := copy(p, part)
	r.off += n
	if r.off >= len(part) {
		r.off = 0
		r.index++
	}
	return n, nil
}
