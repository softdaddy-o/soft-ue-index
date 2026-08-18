package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const maxDaemonConnections = 32

// Server handles control plane messages from CLI clients.
type Server struct {
	PipePath        string
	LockPath        string
	OnStatus        func(context.Context) (string, error)
	OnStop          func(context.Context) error
	OnMCP           func(context.Context, io.ReadWriteCloser) error
	OnReady         func()
	LockAlreadyHeld bool
	mu              sync.Mutex
	connections     map[io.ReadWriteCloser]struct{}
	wg              sync.WaitGroup
}

// ListenAndServe starts the daemon control endpoint and blocks until context
// cancellation or stop command.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := s.normalizeDefaults(); err != nil {
		return err
	}
	var release func()
	var err error
	if !s.LockAlreadyHeld {
		var acquired bool
		release, acquired, err = s.startupLock()
		if err != nil {
			return err
		}
		if !acquired {
			return errors.New("daemon is already running")
		}
		defer release()
	}

	listener, err := listenPipe(s.PipePath)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
	}()
	if s.OnReady != nil {
		s.OnReady()
	}
	serverCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-serverCtx.Done()
		_ = listener.Close()
	}()

	limit := make(chan struct{}, maxDaemonConnections)
	var serveErr error
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-serverCtx.Done():
				serveErr = nil
			default:
				serveErr = fmt.Errorf("accept: %w", err)
			}
			break
		}
		select {
		case limit <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		s.track(conn)
		s.wg.Add(1)
		go func() {
			defer func() { <-limit; s.untrack(conn); s.wg.Done() }()
			s.handle(serverCtx, conn, cancel)
		}()
	}
	cancel()
	_ = listener.Close()
	s.closeConnections()
	s.wg.Wait()
	return serveErr
}

func (s *Server) normalizeDefaults() error {
	if s.PipePath == "" {
		path, err := DefaultPipePath()
		if err != nil {
			return err
		}
		s.PipePath = path
	}
	if s.LockPath == "" {
		path, err := DefaultDaemonLockPath()
		if err != nil {
			return err
		}
		s.LockPath = path
	}
	return nil
}

func (s *Server) startupLock() (func(), bool, error) {
	release, acquired, err := tryAcquireFileLock(s.LockPath)
	if err != nil {
		return nil, false, err
	}
	return release, acquired, nil
}

func (s *Server) handle(ctx context.Context, conn io.ReadWriteCloser, cancel context.CancelFunc) {
	defer conn.Close()
	if timed, ok := conn.(deadlineConnection); ok {
		_ = timed.SetDeadline(time.Now().Add(2 * time.Second))
	}
	line, err := readControlLine(conn)
	if err != nil {
		_ = sendError(conn, err)
		return
	}
	command := strings.TrimSpace(strings.TrimPrefix(line, controlPrefix))
	if !strings.HasPrefix(line, controlPrefix) {
		_ = sendError(conn, errors.New("invalid control preface"))
		return
	}

	switch ControlCommand(command) {
	case CmdStatus:
		value := "ready"
		if s.OnStatus != nil {
			if value, err = s.OnStatus(ctx); err != nil {
				_ = sendError(conn, err)
				return
			}
		}
		_ = sendLine(conn, value)
	case CmdStop:
		if s.OnStop != nil {
			if err := s.OnStop(ctx); err != nil {
				_ = sendError(conn, err)
				return
			}
		}
		_ = sendLine(conn, "ok")
		cancel()
	case CmdMCP:
		if err := sendLine(conn, "ok"); err != nil {
			return
		}
		clearDeadline(conn)
		if s.OnMCP == nil {
			return
		}
		_ = s.OnMCP(ctx, conn)
	default:
		_ = sendError(conn, errors.New("unknown control command"))
	}
}

func (s *Server) track(conn io.ReadWriteCloser) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connections == nil {
		s.connections = make(map[io.ReadWriteCloser]struct{})
	}
	s.connections[conn] = struct{}{}
}

func (s *Server) untrack(conn io.ReadWriteCloser) {
	s.mu.Lock()
	delete(s.connections, conn)
	s.mu.Unlock()
}

func (s *Server) closeConnections() {
	s.mu.Lock()
	connections := make([]io.ReadWriteCloser, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func sendError(w io.Writer, err error) error {
	return sendLine(w, "err: "+err.Error())
}

func sendLine(w io.Writer, line string) error {
	_, err := w.Write([]byte(line + "\n"))
	return err
}
