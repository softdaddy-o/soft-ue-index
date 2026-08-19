package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxIndexSeedBytes = 4 << 20
const maxSeedDatabaseBytes = 128 << 20

var errNoIndexSeed = errors.New("no readable compilation database seed file")

type indexSeed struct {
	Path string
	Text []byte
}

type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Wait() error
	Kill() error
}
type ProcessFactory interface {
	Start(context.Context, string, []string, string) (Process, error)
}
type onceWaitProcess struct {
	Process
	once sync.Once
	err  error
}

func (p *onceWaitProcess) Wait() error {
	p.once.Do(func() { p.err = p.Process.Wait() })
	return p.err
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
	// IndexGraceTimeout overrides how long a session's Client waits, after
	// telling clangd what to initially index, for a workDoneProgress before
	// assuming the index was already warm. See ClientOptions.IndexGraceTimeout.
	IndexGraceTimeout time.Duration
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
	startErr  error
	cancel    context.CancelFunc
	stale     bool
}

// SessionState describes a project session's clangd process/index readiness
// without starting one, so callers like project_status can report it
// instantly instead of blocking on (or triggering) a cold start.
type SessionState struct {
	// Phase is one of "absent" (no session has been requested yet),
	// "starting" (process launching or clangd initializing), "indexing"
	// (initialized, background index in progress), "ready", or "degraded"
	// (the last start attempt failed and a restart is backing off).
	Phase   string
	Message string
}

// SessionState reads the current phase of a project's session, if any. It
// never blocks on session startup and never starts a new session.
func (m *Manager) SessionState(id string) SessionState {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return SessionState{Phase: "absent"}
	}
	if s.client != nil {
		return SessionState{Phase: s.client.IndexPhase(), Message: s.client.IndexMessage()}
	}
	if s.starting != nil {
		return SessionState{Phase: "starting"}
	}
	if s.startErr != nil {
		return SessionState{Phase: "degraded", Message: s.startErr.Error()}
	}
	return SessionState{Phase: "absent"}
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
		cfg.Threads = 1
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
		if s != nil && (s.stale || s.config != cfg) {
			s.stale = true
			if s.refs != 0 {
				m.mu.Unlock()
				return nil, errors.New("project configuration changed; retry request")
			}
			m.retireLocked(cfg.ID, s)
			m.mu.Unlock()
			continue
		}
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
		if s == nil {
			sessionCtx, cancel := context.WithCancel(context.Background())
			s = &session{config: cfg, starting: make(chan struct{}), cancel: cancel}
			m.sessions[cfg.ID] = s
			wait := s.starting
			m.mu.Unlock()
			go m.startSession(cfg.ID, s, sessionCtx)
			if err := waitForStartup(ctx, wait); err != nil {
				return nil, err
			}
			return m.startupResult(ctx, cfg.ID, s)
		}
		if s.starting != nil {
			wait := s.starting
			m.mu.Unlock()
			if err := waitForStartup(ctx, wait); err != nil {
				return nil, err
			}
			return m.startupResult(ctx, cfg.ID, s)
		}
		if !s.nextStart.IsZero() && m.now().Before(s.nextStart) {
			m.mu.Unlock()
			return nil, errors.New("clangd restart is backing off")
		}
		sessionCtx, cancel := context.WithCancel(context.Background())
		s.config = cfg
		s.startErr = nil
		s.starting = make(chan struct{})
		s.cancel = cancel
		wait := s.starting
		m.mu.Unlock()
		go m.startSession(cfg.ID, s, sessionCtx)
		if err := waitForStartup(ctx, wait); err != nil {
			return nil, err
		}
		return m.startupResult(ctx, cfg.ID, s)
	}
}

func waitForStartup(ctx context.Context, wait <-chan struct{}) error {
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) startupResult(ctx context.Context, id string, s *session) (*Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.closed || m.sessions[id] != s {
		return nil, ErrClosed
	}
	if s.client != nil {
		if s.timer != nil {
			s.timer.Stop()
			s.timer = nil
		}
		s.refs++
		return s.client, nil
	}
	if s.startErr != nil {
		return nil, s.startErr
	}
	return nil, errors.New("clangd restart is backing off")
}

func (m *Manager) startSession(id string, s *session, sessionCtx context.Context) {
	if err := sessionCtx.Err(); err != nil {
		_, _ = m.finishStartup(id, s, nil, nil, err)
		return
	}
	p, err := m.start(sessionCtx, s.config)
	if err != nil {
		_, _ = m.finishStartup(id, s, nil, nil, err)
		return
	}
	// Publish the process before initializing it so Close can cancel and kill
	// a server that never answers initialize, without waiting on this goroutine.
	m.mu.Lock()
	if m.closed || m.sessions[id] != s {
		m.mu.Unlock()
		cleanupStartup(p, nil, nil)
		return
	}
	s.process = p
	m.mu.Unlock()
	client := NewClient(p.Stdout(), p.Stdin(), ClientOptions{IndexGraceTimeout: s.config.IndexGraceTimeout})
	err = client.Initialize(sessionCtx, s.config.RootURI)
	if err == nil && s.config.CompilationDatabase != "" {
		var seed indexSeed
		seed, err = selectIndexSeed(s.config.CompilationDatabase, s.config.RootURI, maxIndexSeedBytes)
		if err == nil {
			uri := pathURI(seed.Path)
			err = client.DidOpen(uri, "cpp", string(seed.Text))
		} else if errors.Is(err, errNoIndexSeed) {
			err = nil
		}
	}
	if err == nil {
		// Arm the grace-timeout fallback only now: clangd has just been
		// told everything it will initially see (initialize plus any
		// seed didOpen), so the fallback's clock starts from the moment
		// indexing could actually begin, not from process startup.
		client.StartIndexGraceWindow()
	}
	_, _ = m.finishStartup(id, s, p, client, err)
}

func selectIndexSeed(database, rootURI string, maxBytes int64) (indexSeed, error) {
	rootURL, err := url.Parse(rootURI)
	if err != nil || rootURL.Scheme != "file" {
		return indexSeed{}, errors.New("project root URI must be a file URI")
	}
	rootPath := filepath.FromSlash(rootURL.Path)
	if len(rootPath) >= 3 && (rootPath[0] == '/' || rootPath[0] == '\\') && rootPath[2] == ':' {
		rootPath = rootPath[1:]
	}
	root, err := filepath.Abs(rootPath)
	if err != nil {
		return indexSeed{}, err
	}
	f, err := os.Open(database)
	if err != nil {
		return indexSeed{}, err
	}
	defer f.Close()
	if info, statErr := f.Stat(); statErr != nil || info.Size() > maxSeedDatabaseBytes {
		return indexSeed{}, errors.New("compilation database exceeds seed scan limit")
	}
	d := json.NewDecoder(io.LimitReader(f, maxSeedDatabaseBytes+1))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('[') {
		return indexSeed{}, errors.New("compilation database must be an array")
	}
	count := 0
	var candidate string
	var candidateSize int64
	for ; d.More(); count++ {
		if count >= 100000 {
			return indexSeed{}, errors.New("compilation database seed scan limit exceeded")
		}
		var entry struct{ Directory, File string }
		if err := d.Decode(&entry); err != nil {
			return indexSeed{}, err
		}
		path := entry.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(entry.Directory, path)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > maxBytes || !isSeedSource(path) {
			continue
		}
		if withinRoot(path, root) {
			if candidate == "" || info.Size() < candidateSize {
				candidate, candidateSize = path, info.Size()
			}
		}
	}
	if candidate != "" {
		return readIndexSeed(candidate, maxBytes)
	}
	if count > 0 {
		return indexSeed{}, errors.New("compilation database has no safe readable project seed")
	}
	return indexSeed{}, errNoIndexSeed
}

func isSeedSource(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".c", ".cc", ".cpp", ".cxx":
	default:
		return false
	}
	for _, segment := range strings.Split(strings.ToLower(filepath.ToSlash(path)), "/") {
		if segment == "proto" || segment == "generated" || segment == "thirdparty" {
			return false
		}
	}
	return !strings.HasSuffix(strings.ToLower(path), ".gen.cpp")
}

func readIndexSeed(path string, maxBytes int64) (indexSeed, error) {
	f, err := os.Open(path)
	if err != nil {
		return indexSeed{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return indexSeed{}, err
	}
	if int64(len(b)) > maxBytes {
		return indexSeed{}, errors.New("index seed source exceeds read limit")
	}
	return indexSeed{Path: path, Text: b}, nil
}

func withinRoot(path, root string) bool {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !(len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator))
}

func pathURI(path string) string {
	p := filepath.ToSlash(path)
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// finishStartup is the only normal completion path for a session startup. It
// closes the shared waiter exactly once and never performs I/O while m.mu is held.
func (m *Manager) finishStartup(id string, s *session, p Process, client *Client, startErr error) (*Client, error) {
	m.mu.Lock()
	stale := m.closed || m.sessions[id] != s
	if stale {
		finishStartupLocked(s)
		cancel := s.cancel
		s.cancel = nil
		m.mu.Unlock()
		cleanupStartup(p, client, cancel)
		return nil, ErrClosed
	}
	if startErr != nil {
		s.failures++
		s.nextStart = m.now().Add(backoff(s.failures))
		s.startErr = startErr
		s.process, s.client = nil, nil
		cancel := s.cancel
		s.cancel = nil
		finishStartupLocked(s)
		m.mu.Unlock()
		cleanupStartup(p, client, cancel)
		return nil, startErr
	}
	s.client = client
	s.startErr = nil
	s.refs = 0
	m.scheduleIdleLocked(id, s)
	finishStartupLocked(s)
	m.mu.Unlock()
	go m.watch(id, s, p)
	return client, nil
}

func (m *Manager) scheduleIdleLocked(id string, s *session) {
	if s.timer == nil {
		s.timer = time.AfterFunc(s.config.IdleTimeout, func() { m.stop(id, s) })
	}
}

func finishStartupLocked(s *session) {
	if s.starting != nil {
		close(s.starting)
		s.starting = nil
	}
}

func cleanupStartup(p Process, client *Client, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	if client != nil {
		client.Close()
	}
	if p != nil {
		_ = p.Kill()
		_ = p.Wait()
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
	// clang-tidy is on by default in clangd and runs its clang-analyzer-*
	// static-analysis checks even with no .clang-tidy file present. Those
	// checks build a CFG and symbolically execute paths per translation
	// unit -- expensive on Unreal's macro/template-heavy headers, and pure
	// overhead here since this project only wants navigation and search,
	// not lint diagnostics. Disabling it is a low-memory default alongside
	// --j=1 and --pch-storage=disk above.
	args := []string{"--compile-commands-dir=" + cfg.CacheDir, "--background-index", "--background-index-priority=background", "--pch-storage=disk", "--clang-tidy=0", "--j=" + itoa(cfg.Threads), "--log=error"}
	p, err := m.factory.Start(ctx, cfg.Clangd, args, filepath.Join(cfg.CacheDir, "clangd.log"))
	if err != nil {
		return nil, err
	}
	return &onceWaitProcess{Process: p}, nil
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
		if s.stale {
			m.retireLocked(id, s)
			return
		}
		m.scheduleIdleLocked(id, s)
	}
}

// Invalidate retires a project session once active requests release it.
func (m *Manager) Invalidate(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return
	}
	s.stale = true
	if s.refs == 0 {
		m.retireLocked(id, s)
	}
}

func (m *Manager) retireLocked(id string, s *session) {
	if m.sessions[id] != s {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.client != nil {
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

// SourceFileChanged forwards an ordinary source write to an existing project
// session without starting clangd solely for a filesystem notification.
func (m *Manager) SourceFileChanged(id, path string) error {
	return m.sourceFileChanged(id, path, 2)
}

func (m *Manager) SourceFileCreated(id, path string) error {
	return m.sourceFileChanged(id, path, 1)
}

func (m *Manager) sourceFileChanged(id, path string, watchedType int) error {
	m.mu.Lock()
	s := m.sessions[id]
	var client *Client
	if s != nil {
		client = s.client
	}
	m.mu.Unlock()
	if client == nil {
		return nil
	}
	seed, err := readIndexSeed(path, maxIndexSeedBytes)
	if err != nil {
		return err
	}
	return client.SourceFileChanged(pathURI(seed.Path), string(seed.Text), watchedType)
}

// SourceFileRemoved forwards a deletion only to an already-running session.
func (m *Manager) SourceFileRemoved(id, path string) error {
	m.mu.Lock()
	s := m.sessions[id]
	var client *Client
	if s != nil {
		client = s.client
	}
	m.mu.Unlock()
	if client == nil {
		return nil
	}
	return client.SourceFileRemoved(pathURI(path))
}

func (m *Manager) stop(id string, s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[id] != s || s.refs != 0 {
		return
	}
	if s.client != nil {
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
	type cleanup struct {
		process Process
		client  *Client
		cancel  context.CancelFunc
	}
	cleanups := make([]cleanup, 0, len(ss))
	for _, s := range ss {
		if s.timer != nil {
			s.timer.Stop()
		}
		finishStartupLocked(s)
		cleanups = append(cleanups, cleanup{s.process, s.client, s.cancel})
		s.process, s.client, s.cancel = nil, nil, nil
	}
	m.mu.Unlock()
	for _, item := range cleanups {
		cleanupStartup(item.process, item.client, item.cancel)
	}
}
