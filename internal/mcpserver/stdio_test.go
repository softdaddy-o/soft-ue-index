package mcpserver

import (
	"errors"
	"io"
	"strings"
	"testing"
)

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
