package mcpserver

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
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
