package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const defaultMaxMessageBytes = 8 << 20
const defaultMaxHeaderBytes = 32 << 10

var ErrMessageTooLarge = errors.New("LSP message exceeds size limit")

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func writeFrame(w io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body))
	if err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func readFrame(r *bufio.Reader, max int) ([]byte, error) {
	if max <= 0 {
		max = defaultMaxMessageBytes
	}
	length := -1
	headerBytes := 0
	for {
		line, err := readHeaderLine(r, 8<<10)
		if err != nil {
			return nil, err
		}
		headerBytes += len(line)
		if headerBytes > defaultMaxHeaderBytes {
			return nil, ErrMessageTooLarge
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid LSP header %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(parts[0]), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil || length < 0 {
				return nil, fmt.Errorf("invalid Content-Length")
			}
		}
	}
	if length < 0 {
		return nil, errors.New("missing Content-Length")
	}
	if length > max {
		return nil, ErrMessageTooLarge
	}
	body := make([]byte, length)
	_, err := io.ReadFull(r, body)
	return body, err
}

// readHeaderLine uses ReadSlice chunks so a peer cannot make ReadString allocate an unbounded line.
func readHeaderLine(r *bufio.Reader, limit int) (string, error) {
	var line bytes.Buffer
	for {
		chunk, err := r.ReadSlice('\n')
		if line.Len()+len(chunk) > limit {
			return "", ErrMessageTooLarge
		}
		line.Write(chunk)
		if err == nil {
			return line.String(), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
	}
}

func frameBytes(value any) []byte { var b bytes.Buffer; _ = writeFrame(&b, value); return b.Bytes() }
