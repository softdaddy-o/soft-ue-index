package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

var ErrRequestFrameTooLarge = errors.New("MCP request frame exceeds configured limit")

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
			Name string `json:"name"`
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

type stdoutWriteCloser struct{ io.Writer }

func (stdoutWriteCloser) Close() error { return nil }
