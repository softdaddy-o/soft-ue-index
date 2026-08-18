package mcpserver

import (
	"bufio"
	"bytes"
	"context"
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

// FrameProxyCaps configures frame bounds for bidirectional stdio-to-pipe forwarding.
type FrameProxyCaps struct {
	MaxRequestBytes   int
	MaxRequestIDBytes int
	MaxResponseBytes  int
}

// ProxyFrames forwards newline-delimited JSON-RPC frames between two streams with
// explicit frame caps on request and response directions.
// - requestReader/requestWriter carry client -> server traffic and validate IDs/args.
// - responseReader/responseWriter carry server -> client traffic with response caps.
// Context cancellation closes any closeable endpoints and stops forwarding.
func ProxyFrames(ctx context.Context, requestReader io.Reader, requestWriter io.Writer, responseReader io.Reader, responseWriter io.Writer, caps FrameProxyCaps) error {
	caps = normalizeFrameCaps(caps)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type proxyResult struct {
		err error
	}
	results := make(chan proxyResult, 2)
	go func() {
		results <- proxyResult{err: proxyFrames(ctx, requestReader, requestWriter, caps.MaxRequestBytes, caps.MaxRequestIDBytes, "request")}
	}()
	go func() {
		results <- proxyResult{err: proxyFramesResponse(ctx, responseReader, responseWriter, caps.MaxResponseBytes)}
	}()
	var first proxyResult
	select {
	case first = <-results:
	case <-ctx.Done():
		first = proxyResult{err: ctx.Err()}
	}
	cancel()
	// Closing the client input and both pipe-facing endpoints unblocks the
	// opposite copier. Never close responseWriter: in stdio mode it is stdout.
	for _, endpoint := range []any{requestReader, requestWriter, responseReader} {
		if closer, ok := endpoint.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	second := <-results
	if !isEOFOrCanceled(first.err) {
		return first.err
	}
	// The other copier is deliberately interrupted by closing the pipe-facing
	// endpoints. Preserve bounded-frame failures, but treat transport-specific
	// closed-handle errors as normal shutdown.
	if errors.Is(second.err, ErrRequestFrameTooLarge) || errors.Is(second.err, ErrResponseFrameTooLarge) {
		return second.err
	}
	if ctx.Err() != nil && !errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return nil
}

func normalizeFrameCaps(caps FrameProxyCaps) FrameProxyCaps {
	if caps.MaxRequestBytes <= 0 {
		caps.MaxRequestBytes = defaultMaxRequestBytes
	}
	if caps.MaxResponseBytes <= 0 {
		caps.MaxResponseBytes = defaultMaxBytes
	}
	if caps.MaxRequestIDBytes <= 0 {
		caps.MaxRequestIDBytes = defaultMaxRequestIDBytes
	}
	return caps
}

func isEOFOrCanceled(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled)
}

func proxyFrames(ctx context.Context, requestReader io.Reader, requestWriter io.Writer, maxRequestBytes, maxRequestIDBytes int, mode string) error {
	reader := newBoundedFrameReader(requestReader, maxRequestBytes)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := reader.readFrame(ctx)
		if err != nil {
			return mapFrameError(err, mode)
		}
		if len(line) == 0 {
			continue
		}
		if err := validateRequestID(line, maxRequestIDBytes); err != nil {
			return ErrRequestFrameTooLarge
		}
		if err := writeAll(ctx, requestWriter, line); err != nil {
			return err
		}
	}
}

func proxyFramesResponse(ctx context.Context, responseReader io.Reader, responseWriter io.Writer, maxResponseBytes int) error {
	reader := newBoundedFrameReader(responseReader, maxResponseBytes)
	writer := newBoundedJSONWriter(responseWriter, maxResponseBytes)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := reader.readFrame(ctx)
		if err != nil {
			return mapFrameError(err, "response")
		}
		if len(line) == 0 {
			continue
		}
		if _, err := writer.Write(line); err != nil {
			return err
		}
	}
}

func mapFrameError(err error, mode string) error {
	if errors.Is(err, errFrameTooLarge) {
		if mode == "response" {
			return ErrResponseFrameTooLarge
		}
		return ErrRequestFrameTooLarge
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	return err
}

type boundedFrameReader struct {
	reader   *bufio.Reader
	maxFrame int
}

var errFrameTooLarge = errors.New("MCP frame is too large or unterminated")

func newBoundedFrameReader(r io.Reader, maxFrame int) *boundedFrameReader {
	return &boundedFrameReader{reader: bufio.NewReaderSize(r, maxFrame+1), maxFrame: maxFrame}
}

func (r *boundedFrameReader) readFrame(ctx context.Context) ([]byte, error) {
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		line, err := r.reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(line) > r.maxFrame {
			return nil, errFrameTooLarge
		}
		if err != nil {
			if err == io.EOF {
				if len(line) == 0 {
					return nil, io.EOF
				}
				return nil, errFrameTooLarge
			}
			return nil, err
		}
		return line, nil
	}
}

func writeAll(ctx context.Context, writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}

func validateRequestID(line []byte, maxStringID int) error {
	trimmed := bytes.TrimSpace(line)
	// The MVP supports one JSON-RPC request per newline frame. Reject batches
	// before the SDK can fan them out into independent asynchronous handlers.
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return ErrRequestFrameTooLarge
	}
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
