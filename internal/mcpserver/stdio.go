package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

var ErrRequestFrameTooLarge = errors.New("MCP request frame exceeds configured limit")
var ErrResponseFrameTooLarge = errors.New("MCP response frame exceeds configured limit")

const maxMethodNameBytes = 128
const maxToolNameBytes = 128

// boundedJSONReader validates newline-delimited JSON-RPC before the SDK sees
// it. In particular, it refuses large string IDs because JSON-RPC responses
// normally echo an ID and could otherwise exceed the response cap.
type boundedJSONReader struct {
	reader      *bufio.Reader
	maxFrame    int
	maxStringID int
	pending     []byte
	firstFrame  bool
}

func newBoundedJSONReader(r io.Reader, maxFrame, maxStringID int) *boundedJSONReader {
	return &boundedJSONReader{reader: bufio.NewReaderSize(r, maxFrame+1), maxFrame: maxFrame, maxStringID: maxStringID, firstFrame: true}
}

func (r *boundedJSONReader) Read(dst []byte) (int, error) {
	if len(r.pending) == 0 {
		line, err := r.reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(line) > r.maxFrame {
			return 0, ErrRequestFrameTooLarge
		}
		if len(line) == 0 {
			return 0, err
		}
		bom := []byte{0xef, 0xbb, 0xbf}
		hasBOM := bytes.Contains(line, bom)
		if hasBOM {
			if !r.firstFrame || !bytes.HasPrefix(line, bom) {
				return 0, ErrRequestFrameTooLarge
			}
			// The raw frame length, including BOM, was checked above. Strip only
			// the single stream-leading BOM before JSON and ID validation.
			line = line[len(bom):]
		}
		r.firstFrame = false
		if validateRequestID(line, r.maxStringID) != nil {
			return 0, ErrRequestFrameTooLarge
		}
		r.pending = line
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
	}
	n := copy(dst, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func validateRequestID(line []byte, maxStringID int) error {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &request); err != nil {
		// Let the SDK handle ordinary malformed JSON-RPC requests; the frame is
		// still bounded and cannot trigger a reflected oversized ID.
		return nil
	}
	if len(request.Method) > maxMethodNameBytes || len(request.Params.Name) > maxToolNameBytes {
		return ErrRequestFrameTooLarge
	}
	if request.Method == "tools/call" && validateToolArguments(request.Params.Name, request.Params.Arguments) != nil {
		return ErrRequestFrameTooLarge
	}
	if len(request.ID) == 0 {
		return nil
	}
	var id string
	if len(request.ID) > maxStringID {
		return ErrRequestFrameTooLarge
	}
	if json.Unmarshal(request.ID, &id) == nil && len(id) > maxStringID {
		return ErrRequestFrameTooLarge
	}
	return nil
}

func validateToolArguments(name string, raw json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var arguments map[string]json.RawMessage
	if json.Unmarshal(raw, &arguments) != nil {
		return ErrRequestFrameTooLarge
	}
	stringFields := map[string]bool{}
	intFields := map[string]bool{}
	objectFields := map[string]bool{}
	switch name {
	case "list_projects":
		intFields["max_items"] = true
	case "project_status":
		stringFields["project_id"] = true
	case "search_symbols":
		stringFields["project_id"], stringFields["query"], intFields["max_items"] = true, true, true
	case "find_definition", "find_references", "find_implementations", "hover":
		stringFields["project_id"], objectFields["position"], intFields["max_items"] = true, true, true
	case "document_symbols":
		stringFields["project_id"], stringFields["path"], intFields["max_items"] = true, true, true
	case "call_hierarchy":
		stringFields["project_id"], stringFields["direction"], objectFields["position"] = true, true, true
		intFields["max_items"], intFields["max_depth"] = true, true
	case "read_symbol_source":
		stringFields["project_id"], stringFields["path"] = true, true
		intFields["start_line"], intFields["end_line"], intFields["max_bytes"] = true, true, true
	default:
		return nil
	}
	for field, value := range arguments {
		if stringFields[field] {
			var typed string
			if json.Unmarshal(value, &typed) != nil {
				return ErrRequestFrameTooLarge
			}
		} else if intFields[field] {
			var typed int
			if json.Unmarshal(value, &typed) != nil {
				return ErrRequestFrameTooLarge
			}
		} else if objectFields[field] && validatePositionArgument(value) != nil {
			return ErrRequestFrameTooLarge
		}
	}
	return nil
}

func validatePositionArgument(raw json.RawMessage) error {
	var position map[string]json.RawMessage
	if json.Unmarshal(raw, &position) != nil {
		return ErrRequestFrameTooLarge
	}
	for _, field := range []string{"line", "character"} {
		if value, ok := position[field]; ok {
			var typed int
			if json.Unmarshal(value, &typed) != nil {
				return ErrRequestFrameTooLarge
			}
		}
	}
	if value, ok := position["path"]; ok {
		var typed string
		if json.Unmarshal(value, &typed) != nil {
			return ErrRequestFrameTooLarge
		}
	}
	return nil
}

type stdoutWriteCloser struct{ io.Writer }

func (stdoutWriteCloser) Close() error { return nil }

type boundedJSONWriter struct {
	mu       sync.Mutex
	writer   io.Writer
	maxFrame int
	pending  []byte
}

func newBoundedJSONWriter(w io.Writer, maxFrame int) *boundedJSONWriter {
	return &boundedJSONWriter{writer: w, maxFrame: maxFrame}
}

func (w *boundedJSONWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, data...)
	for {
		newline := bytes.IndexByte(w.pending, '\n')
		if newline < 0 {
			if len(w.pending) > w.maxFrame {
				w.pending = nil
				return 0, ErrResponseFrameTooLarge
			}
			return len(data), nil
		}
		frameBytes := newline + 1
		if frameBytes > w.maxFrame {
			w.pending = nil
			return 0, ErrResponseFrameTooLarge
		}
		written, err := w.writer.Write(w.pending[:frameBytes])
		if err != nil {
			return 0, err
		}
		if written != frameBytes {
			return 0, io.ErrShortWrite
		}
		w.pending = w.pending[frameBytes:]
	}
}

func (w *boundedJSONWriter) Close() error { return nil }
