package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	controlPrefix     = "mcp-control:"
	controlMCP        = "mcp"
	controlStatus     = "status"
	controlStop       = "stop"
	defaultPollWindow = 5 * time.Second
)

// ControlCommand identifies a daemon control plane request.
type ControlCommand string

const (
	CmdMCP    ControlCommand = controlMCP
	CmdStatus ControlCommand = controlStatus
	CmdStop   ControlCommand = controlStop
)

// Client is a small control-plane API client for the running daemon.
// A Dialer is injectable for tests.
type Client struct {
	Timeout  time.Duration
	PipePath string
	Dialer   func(ctx context.Context, pipePath string) (io.ReadWriteCloser, error)
}

// NewClient returns a client with defaults derived from host config.
func NewClient(pipePath string) *Client {
	return &Client{Timeout: 2 * time.Second, PipePath: pipePath}
}

// Status asks the daemon for status and returns the text payload.
func (c *Client) Status(ctx context.Context) (string, error) {
	resp, err := c.control(ctx, CmdStatus)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(resp)), nil
}

// Stop requests daemon shutdown.
func (c *Client) Stop(ctx context.Context) error {
	_, err := c.control(ctx, CmdStop)
	return err
}

// MCP requests ownership of the control connection for interactive MCP transport.
// The returned connection remains open after the control preface is acknowledged.
func (c *Client) MCP(ctx context.Context) (io.ReadWriteCloser, error) {
	conn, err := c.conn(ctx)
	if err != nil {
		return nil, err
	}
	c.setControlDeadline(ctx, conn)
	if _, err := fmt.Fprintf(conn, "%s\n", commandFrame(CmdMCP)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	line, err := readControlLine(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if strings.TrimSpace(line) != "ok" {
		_ = conn.Close()
		return nil, fmt.Errorf("mcp control failed: %s", strings.TrimSpace(line))
	}
	clearDeadline(conn)
	return conn, nil
}

// EnsureRunning ensures a daemon instance is available using bounded control calls and
// a startup lock to avoid concurrent launch races.
func EnsureRunning(ctx context.Context, cfg RunConfig) error {
	client, err := cfg.client()
	if err != nil {
		return err
	}
	pipePath, err := cfg.effectivePipePath()
	if err != nil {
		return err
	}
	client.PipePath = pipePath
	if cfg.Start == nil {
		return errors.New("missing start callback")
	}
	if err := cfg.normalize(); err != nil {
		return err
	}

	if _, err := client.Status(ctx); err == nil {
		return nil
	}

	release, acquired, err := cfg.startupLock()
	if err != nil {
		return err
	}
	owner := acquired
	if acquired {
		defer release()
		if _, err := client.Status(ctx); err == nil {
			return nil
		}
		if err := cfg.Start(ctx); err != nil {
			return err
		}
	}

	timeout := cfg.timeout()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if !owner {
				return errors.New("daemon startup timed out while another process is starting")
			}
			return errors.New("daemon failed to become ready after startup")
		}
		checkCtx, cancel := context.WithTimeout(ctx, minDuration(150*time.Millisecond, remaining))
		_, err := client.Status(checkCtx)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *Client) control(ctx context.Context, command ControlCommand) ([]byte, error) {
	conn, err := c.conn(ctx)
	if err != nil {
		return nil, err
	}
	c.setControlDeadline(ctx, conn)
	if _, err := fmt.Fprintf(conn, "%s\n", commandFrame(command)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	resp, err := readLine(conn)
	_ = conn.Close()
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(resp, "err:") {
		return nil, errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "err:")))
	}
	return []byte(resp), nil
}

type deadlineConnection interface{ SetDeadline(time.Time) error }

func (c *Client) setControlDeadline(ctx context.Context, conn io.ReadWriteCloser) {
	deadline := time.Now().Add(c.Timeout)
	if c.Timeout <= 0 {
		deadline = time.Now().Add(2 * time.Second)
	}
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if timed, ok := conn.(deadlineConnection); ok {
		_ = timed.SetDeadline(deadline)
	}
}

func clearDeadline(conn io.ReadWriteCloser) {
	if timed, ok := conn.(deadlineConnection); ok {
		_ = timed.SetDeadline(time.Time{})
	}
}

func readControlLine(r io.Reader) (string, error) {
	const maxControlLine = 256
	line := make([]byte, 0, 32)
	var one [1]byte
	for len(line) < maxControlLine {
		n, err := r.Read(one[:])
		if n == 1 {
			line = append(line, one[0])
			if one[0] == '\n' {
				return string(line), nil
			}
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("daemon control response is too large")
}

func (c *Client) conn(ctx context.Context) (io.ReadWriteCloser, error) {
	if c.PipePath == "" {
		return nil, errors.New("missing pipe path")
	}
	dial := c.Dialer
	if dial == nil {
		dial = dialPipe
	}
	conn, err := dial(ctx, c.PipePath)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func commandFrame(command ControlCommand) string {
	return controlPrefix + string(command)
}

func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		return "", err
	}
	return line, nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func AcquireLifetimeLock() (func(), error) {
	path, err := DefaultDaemonLockPath()
	if err != nil {
		return nil, err
	}
	release, acquired, err := tryAcquireFileLock(path)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, errors.New("daemon is already running")
	}
	return release, nil
}
