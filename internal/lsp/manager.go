package lsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Wait() error
	Kill() error
}
type ProcessFactory interface {
	Start(context.Context, string, []string, string) (Process, error)
}
type ExecFactory struct{}
type execProcess struct {
	*exec.Cmd
	in  io.WriteCloser
	out io.ReadCloser
	log io.Closer
}

func (p *execProcess) Stdin() io.WriteCloser { return p.in }
func (p *execProcess) Stdout() io.ReadCloser { return p.out }
func (p *execProcess) Kill() error           { return p.Process.Kill() }
func (p *execProcess) Wait() error {
	err := p.Cmd.Wait()
	if p.log != nil {
		_ = p.log.Close()
		p.log = nil
	}
	return err
}
func (ExecFactory) Start(ctx context.Context, path string, args []string, logPath string) (Process, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	var err error
	var logFile io.Closer
	if logPath != "" {
		if err = os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		cmd.Stderr = f
		logFile = f
	}
	cleanup := func(err error) (Process, error) {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, err
	}
	in, e := cmd.StdinPipe()
	if e != nil {
		return cleanup(e)
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return cleanup(e)
	}
	if e = cmd.Start(); e != nil {
		return cleanup(e)
	}
	return &execProcess{Cmd: cmd, in: in, out: out, log: logFile}, nil
}

type ProjectConfig struct {
	// CacheDir is a durable, project-unique compilation database directory: it
	// contains compile_commands.json. clangd's supported --background-index puts
	// shards in CacheDir/.cache/clangd/index. Do not share CacheDir across projects.
	ID, Clangd, CompilationDatabase, CacheDir, RootURI string
	Threads                                            int
	IdleTimeout                                        time.Duration
}
type Manager struct {
	factory  ProcessFactory
	now      func() time.Time
	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
}
type session struct {
	config    ProjectConfig
	process   Process
	client    *Client
	refs      int
	timer     *time.Timer
	failures  int
	nextStart time.Time
	starting  chan struct{}
	cancel    context.CancelFunc
}

func NewManager(factory ProcessFactory) *Manager {
	return NewManagerWithClock(factory, time.Now)
}

// NewManagerWithClock injects time for deterministic restart-backoff tests.
func NewManagerWithClock(factory ProcessFactory, now func() time.Time) *Manager {
	if factory == nil {
		factory = ExecFactory{}
	}
	if now == nil {
		now = time.Now
	}
	return &Manager{factory: factory, now: now, sessions: map[string]*session{}}
}
func (m *Manager) Client(ctx context.Context, cfg ProjectConfig) (*Client, error) {
	if cfg.Threads < 1 {
		cfg.Threads = 2
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, ErrClosed
		}
		s := m.sessions[cfg.ID]
		if s != nil && s.client != nil {
			if s.timer != nil {
				s.timer.Stop()
				s.timer = nil
			}
			s.refs++
			c := s.client
			m.mu.Unlock()
			return c, nil
		}
		if s != nil && s.starting != nil {
			wait := s.starting
			m.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if s == nil {
			s = &session{}
			m.sessions[cfg.ID] = s
		}
		if !s.nextStart.IsZero() && m.now().Before(s.nextStart) {
			m.mu.Unlock()
			return nil, errors.New("clangd restart is backing off")
		}
		s.config = cfg
		s.starting = make(chan struct{})
		wait := s.starting
		sessionCtx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		m.mu.Unlock()
		p, err := m.start(sessionCtx, cfg)
		m.mu.Lock()
		if m.closed || m.sessions[cfg.ID] != s {
			close(wait)
			s.starting = nil
			m.mu.Unlock()
			if p != nil {
				_ = p.Kill()
				_ = p.Wait()
			}
			cancel()
			return nil, ErrClosed
		}
		if err == nil {
			s.process = p
			s.client = NewClient(p.Stdout(), p.Stdin(), ClientOptions{})
			err = s.client.Initialize(ctx, cfg.RootURI)
			if err == nil {
				s.refs = 1
				go m.watch(cfg.ID, s, p)
			}
		}
		if err != nil {
			s.failures++
			s.nextStart = m.now().Add(backoff(s.failures))
			if p != nil {
				_ = p.Kill()
				_ = p.Wait()
			}
			if s.client != nil {
				s.client.Close()
			}
			cancel()
			s.client = nil
			s.process = nil
		}
		close(wait)
		s.starting = nil
		c := s.client
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}
func (m *Manager) start(ctx context.Context, cfg ProjectConfig) (Process, error) {
	if cfg.CacheDir == "" {
		return nil, errors.New("clangd cache directory is required")
	}
	if cfg.CompilationDatabase != "" {
		path := filepath.Clean(cfg.CompilationDatabase)
		if filepath.Base(path) != "compile_commands.json" || filepath.Clean(filepath.Dir(path)) != filepath.Clean(cfg.CacheDir) {
			return nil, errors.New("compilation database must be CacheDir/compile_commands.json")
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("compilation database is not a regular file")
		}
	}
	if err := os.MkdirAll(filepath.Join(cfg.CacheDir, ".cache", "clangd", "index"), 0700); err != nil {
		return nil, err
	}
	args := []string{"--compile-commands-dir=" + cfg.CacheDir, "--background-index", "--j=" + itoa(cfg.Threads), "--log=error"}
	return m.factory.Start(ctx, cfg.Clangd, args, filepath.Join(cfg.CacheDir, "clangd.log"))
}
func itoa(i int) string { return fmt.Sprintf("%d", i) }
func backoff(f int) time.Duration {
	if f > 5 {
		f = 5
	}
	return time.Second * time.Duration(1<<(f-1))
}
func (m *Manager) watch(id string, s *session, p Process) {
	_ = p.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[id] != s {
		return
	}
	if s.client != nil {
		s.client.Close()
	}
	s.client = nil
	s.process = nil
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.failures++
	s.nextStart = m.now().Add(backoff(s.failures))
}
func (m *Manager) Release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.refs == 0 {
		return
	}
	s.refs--
	if s.refs == 0 {
		s.timer = time.AfterFunc(s.config.IdleTimeout, func() { m.stop(id, s) })
	}
}
func (m *Manager) stop(id string, s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[id] != s || s.refs != 0 {
		return
	}
	if s.client != nil {
		_ = s.client.Shutdown(context.Background())
		s.client.Close()
	}
	if s.process != nil {
		_ = s.process.Kill()
	}
	if s.cancel != nil {
		s.cancel()
	}
	delete(m.sessions, id)
}
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	ss := m.sessions
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	for _, s := range ss {
		if s.timer != nil {
			s.timer.Stop()
		}
		if s.client != nil {
			_ = s.client.Shutdown(context.Background())
			s.client.Close()
		}
		if s.process != nil {
			_ = s.process.Kill()
		}
		if s.cancel != nil {
			s.cancel()
		}
	}
}
