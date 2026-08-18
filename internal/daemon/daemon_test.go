package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestControlFrameHasStablePrefix(t *testing.T) {
	if got := commandFrame(CmdStatus); got != "mcp-control:status" {
		t.Fatalf("control frame %q", got)
	}
}

func TestClientStatusWritesControlFrame(t *testing.T) {
	ctx := context.Background()
	serverConn, clientConn := net.Pipe()
	serverErr := make(chan error, 1)
	go func() {
		line, err := readLine(serverConn)
		if err != nil {
			serverErr <- err
			return
		}
		if strings.TrimSpace(line) != commandFrame(CmdStatus) {
			serverErr <- errors.New("unexpected control frame")
			return
		}
		err = sendLine(serverConn, "ready")
		serverErr <- err
	}()

	client := NewClient("dummy")
	client.Dialer = func(context.Context, string) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	got, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got != "ready" {
		t.Fatalf("status response = %q", got)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestClientStopWritesControlFrame(t *testing.T) {
	ctx := context.Background()
	serverConn, clientConn := net.Pipe()
	serverErr := make(chan error, 1)
	go func() {
		line, err := readLine(serverConn)
		if err != nil {
			serverErr <- err
			return
		}
		if strings.TrimSpace(line) != commandFrame(CmdStop) {
			serverErr <- errors.New("unexpected control frame")
			return
		}
		err = sendLine(serverConn, "ok")
		serverErr <- err
	}()

	client := NewClient("dummy")
	client.Dialer = func(context.Context, string) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestClientMCPRequiresAcknowledgement(t *testing.T) {
	ctx := context.Background()
	serverConn, clientConn := net.Pipe()
	serverErr := make(chan error, 1)
	go func() {
		line, err := readLine(serverConn)
		if err != nil {
			serverErr <- err
			return
		}
		if strings.TrimSpace(line) != commandFrame(CmdMCP) {
			serverErr <- errors.New("unexpected control frame")
			return
		}
		err = sendLine(serverConn, "ok")
		serverErr <- err
	}()

	client := NewClient("dummy")
	client.Dialer = func(context.Context, string) (io.ReadWriteCloser, error) {
		return clientConn, nil
	}
	conn, err := client.MCP(ctx)
	if err != nil {
		t.Fatalf("mcp: %v", err)
	}
	_ = conn.Close()
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestEnsureRunningStartsOnceAndWaitsForReady(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "issue-resource-daemon-startup.lock")
	callCount := atomic.Int32{}
	statusReady := atomic.Bool{}
	c := NewClient("daemon-test")
	c.Dialer = func(ctx context.Context, _ string) (io.ReadWriteCloser, error) {
		call := callCount.Add(1)
		if call == 1 {
			return nil, errors.New("not ready")
		}
		serverConn, clientConn := net.Pipe()
		go func() {
			line, err := readLine(serverConn)
			if err != nil {
				return
			}
			if !strings.HasPrefix(strings.TrimSpace(line), controlPrefix) {
				return
			}
			if statusReady.Load() {
				_ = sendLine(serverConn, "ready")
			} else {
				_ = serverConn.Close()
			}
		}()
		return clientConn, nil
	}
	readySignal := func() {
		statusReady.Store(true)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		readySignal()
	}()
	startCalled := atomic.Bool{}
	err := EnsureRunning(context.Background(), RunConfig{
		Start:           func(context.Context) error { startCalled.Store(true); return nil },
		Dialer:          c.Dialer,
		PipePath:        "daemon-test",
		StartupLockPath: lockPath,
		Timeout:         2 * time.Second,
	})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !startCalled.Load() {
		t.Fatalf("start callback not invoked")
	}
	if callCount.Load() < 2 {
		t.Fatalf("expected multiple control probes, got %d", callCount.Load())
	}
}
