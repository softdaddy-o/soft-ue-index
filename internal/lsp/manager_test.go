package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeProcess struct {
	conn  net.Conn
	done  chan struct{}
	once  sync.Once
	waits atomic.Int32
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.conn }
func (p *fakeProcess) Stdout() io.ReadCloser { return p.conn }
func (p *fakeProcess) Wait() error           { p.waits.Add(1); <-p.done; return nil }
func (p *fakeProcess) Kill() error {
	p.once.Do(func() { close(p.done); _ = p.conn.Close() })
	return nil
}

type fakeFactory struct {
	mu      sync.Mutex
	n       int
	args    [][]string
	methods []wireMessage
	last    *fakeProcess
}
type slowFactory struct {
	base  fakeFactory
	ready chan struct{}
}

func (f *slowFactory) Start(ctx context.Context, path string, args []string, log string) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-f.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.base.Start(ctx, path, args, log)
}

func (f *fakeFactory) Start(_ context.Context, _ string, args []string, _ string) (Process, error) {
	a, b := net.Pipe()
	p := &fakeProcess{conn: a, done: make(chan struct{})}
	f.mu.Lock()
	f.n++
	f.args = append(f.args, append([]string(nil), args...))
	f.last = p
	f.mu.Unlock()
	go func() {
		r := bufio.NewReader(b)
		defer b.Close()
		for {
			body, err := readFrame(r, 1024*1024)
			if err != nil {
				return
			}
			var m wireMessage
			_ = json.Unmarshal(body, &m)
			f.mu.Lock()
			f.methods = append(f.methods, m)
			f.mu.Unlock()
			if m.Method == "initialize" || m.Method == "shutdown" || m.Method == "textDocument/documentSymbol" {
				_, _ = b.Write(frameBytes(wireMessage{JSONRPC: "2.0", ID: m.ID, Result: mustJSON(map[string]any{})}))
			}
		}
	}()
	return p, nil
}
func TestManagerSharesSessionAndClosesIdle(t *testing.T) {
	f := &fakeFactory{}
	m := NewManager(f)
	defer m.Close()
	dir := t.TempDir()
	cfg := ProjectConfig{ID: "game", Clangd: "fake", CacheDir: dir, RootURI: "file:///game", IdleTimeout: time.Millisecond}
	a, err := m.Client(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Client(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("sessions differ")
	}
	f.mu.Lock()
	n := f.n
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("starts=%d", n)
	}
	if f.args[0][0] != "--compile-commands-dir="+dir {
		t.Fatalf("args=%v", f.args[0])
	}
	foundBackgroundPriority := false
	for _, arg := range f.args[0] {
		if arg == "--background-index-priority=background" {
			foundBackgroundPriority = true
		}
	}
	if !foundBackgroundPriority {
		t.Fatalf("background index priority missing from args=%v", f.args[0])
	}
	if !containsArg(f.args[0], "--j=1") || !containsArg(f.args[0], "--pch-storage=disk") || !containsArg(f.args[0], "--clang-tidy=0") {
		t.Fatalf("low-memory defaults missing from args=%v", f.args[0])
	}
	m.Release("game")
	m.Release("game")
	time.Sleep(20 * time.Millisecond)
	m.mu.Lock()
	_, ok := m.sessions["game"]
	m.mu.Unlock()
	if ok {
		t.Fatal("idle session remains")
	}
}

// waitForMethods polls fakeFactory's recorded methods until at least want
// have been recorded or a bounded deadline passes, returning whatever was
// recorded either way -- net.Pipe writes complete as soon as the other side
// has read the bytes, not once fakeFactory's reader goroutine has finished
// unmarshalling and appending them to f.methods, so a caller-side check right
// after a write can observe fewer messages than were actually sent (flaked
// under -race in CI: the reader goroutine lagged past an immediate read).
//
// This only asserts "recorded within the deadline", not "recorded by the
// time the triggering call returned" -- the transport-level ordering (e.g.
// didOpen fully written before Client() returns) still holds via net.Pipe's
// synchronous writes, but a regression that defers recording further (for
// example, moving a seed didOpen into a goroutine fired after a session is
// published) would still pass here as long as it lands inside the deadline.
func waitForMethods(t *testing.T, f *fakeFactory, want int) []wireMessage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var messages []wireMessage
	for {
		f.mu.Lock()
		messages = append([]wireMessage(nil), f.methods...)
		f.mu.Unlock()
		if len(messages) >= want {
			return messages
		}
		if time.Now().After(deadline) {
			t.Logf("waitForMethods: deadline hit with %d/%d methods -- may be recorder lag rather than a missing message", len(messages), want)
			return messages
		}
		time.Sleep(time.Millisecond)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestManagerRetiresStaleProjectConfigurationBeforeRestart(t *testing.T) {
	f := &fakeFactory{}
	m := NewManager(f)
	defer m.Close()
	first := ProjectConfig{ID: "game", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///old"}
	if _, err := m.Client(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	m.Invalidate("game")
	changed := first
	changed.CacheDir = t.TempDir()
	changed.RootURI = "file:///new"
	if _, err := m.Client(context.Background(), changed); err == nil {
		t.Fatal("changed configuration bypassed active stale session")
	}
	m.Release("game")
	if _, err := m.Client(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	starts := f.n
	f.mu.Unlock()
	if starts != 2 {
		t.Fatalf("clangd starts=%d, want 2", starts)
	}
	m.Release("game")
}

func TestManagerWaitsForProcessExactlyOnceOnClose(t *testing.T) {
	f := &fakeFactory{}
	m := NewManager(f)
	if _, err := m.Client(context.Background(), ProjectConfig{ID: "game", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///game"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	p := f.last
	f.mu.Unlock()
	m.Close()
	time.Sleep(20 * time.Millisecond)
	if got := p.waits.Load(); got != 1 {
		t.Fatalf("Wait calls=%d, want 1", got)
	}
}

func TestManagerCreatesIsolatedPersistentShardRoots(t *testing.T) {
	f := &fakeFactory{}
	m := NewManager(f)
	defer m.Close()
	base := t.TempDir()
	for _, id := range []string{"a", "b"} {
		dir := filepath.Join(base, id)
		c, e := m.Client(context.Background(), ProjectConfig{ID: id, Clangd: "fake", CacheDir: dir, RootURI: "file:///x"})
		if e != nil {
			t.Fatal(e)
		}
		m.Release(id)
		if _, e = os.Stat(filepath.Join(dir, ".cache", "clangd", "index")); e != nil {
			t.Fatal(e)
		}
		_ = c
	}
	if filepath.Join(base, "a") == filepath.Join(base, "b") {
		t.Fatal("not isolated")
	}
}

func TestSelectIndexSeedPrefersProjectFileAndBoundsReads(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	engine := filepath.Join(root, "engine")
	cache := filepath.Join(root, "cache")
	for _, dir := range []string{project, engine, cache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	engineFile := filepath.Join(engine, "Engine.cpp")
	projectFile := filepath.Join(project, "Game.cpp")
	if err := os.WriteFile(engineFile, []byte("engine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectFile, []byte("project"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := []map[string]string{{"directory": engine, "file": engineFile}, {"directory": project, "file": projectFile}}
	b, _ := json.Marshal(db)
	if err := os.WriteFile(filepath.Join(cache, "compile_commands.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	seed, err := selectIndexSeed(filepath.Join(cache, "compile_commands.json"), fileURIForTest(project), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if seed.Path != projectFile || string(seed.Text) != "project" {
		t.Fatalf("seed=%+v", seed)
	}
	if _, err := selectIndexSeed(filepath.Join(cache, "compile_commands.json"), fileURIForTest(project), 3); err == nil {
		t.Fatal("expected bounded source read failure")
	}
}

func TestManagerOpensSeedAfterInitializeBeforePublishing(t *testing.T) {
	f := &fakeFactory{}
	m := NewManager(f)
	defer m.Close()
	root, cache := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "Seed.cpp")
	if err := os.WriteFile(source, []byte("int Seed;"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal([]map[string]string{{"directory": root, "file": source, "command": "clang-cl Seed.cpp"}})
	db := filepath.Join(cache, "compile_commands.json")
	if err := os.WriteFile(db, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(context.Background(), ProjectConfig{ID: "seed", Clangd: "fake", CacheDir: cache, CompilationDatabase: db, RootURI: fileURIForTest(root)}); err != nil {
		t.Fatal(err)
	}
	messages := waitForMethods(t, f, 3)
	if len(messages) < 3 || messages[0].Method != "initialize" || messages[1].Method != "initialized" || messages[2].Method != "textDocument/didOpen" {
		t.Fatalf("startup methods=%v", messages)
	}
	var params struct {
		TextDocument struct {
			URI, LanguageID, Text string
			Version               int
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(messages[2].Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.TextDocument.URI != pathURI(source) || params.TextDocument.LanguageID != "cpp" || params.TextDocument.Version != 1 || params.TextDocument.Text != "int Seed;" {
		t.Fatalf("didOpen=%+v", params)
	}
}

func TestSelectIndexSeedPrefersSmallNonGeneratedSource(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	proto := filepath.Join(root, "Proto", "Large.cpp")
	small := filepath.Join(root, "Small.cpp")
	if err := os.MkdirAll(filepath.Dir(proto), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proto, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, []byte("small"), 0o600); err != nil {
		t.Fatal(err)
	}
	database, _ := json.Marshal([]map[string]string{{"directory": root, "file": proto}, {"directory": root, "file": small}})
	db := filepath.Join(cache, "compile_commands.json")
	if err := os.WriteFile(db, database, 0o600); err != nil {
		t.Fatal(err)
	}
	seed, err := selectIndexSeed(db, fileURIForTest(root), 1024)
	if err != nil || seed.Path != small {
		t.Fatalf("seed=%+v err=%v", seed, err)
	}
}

func TestManagerSourceWriteRefreshesOnlyMatchingLiveSession(t *testing.T) {
	f := &fakeFactory{}
	m := NewManager(f)
	defer m.Close()
	root, cache := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "Seed.cpp")
	if err := os.WriteFile(source, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal([]map[string]string{{"directory": root, "file": source, "command": "clang-cl Seed.cpp"}})
	db := filepath.Join(cache, "compile_commands.json")
	if err := os.WriteFile(db, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(context.Background(), ProjectConfig{ID: "game", Clangd: "fake", CacheDir: cache, CompilationDatabase: db, RootURI: fileURIForTest(root)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.SourceFileChanged("other", source); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	before := len(f.methods)
	f.mu.Unlock()
	if err := m.SourceFileChanged("game", source); err != nil {
		t.Fatal(err)
	}
	messages := waitForMethods(t, f, before+1)
	if len(messages) != before+1 || messages[len(messages)-1].Method != "textDocument/didChange" || !strings.Contains(string(messages[len(messages)-1].Params), `"text":"new"`) {
		t.Fatalf("messages=%v", messages[before:])
	}
}

func fileURIForTest(path string) string {
	p := filepath.ToSlash(path)
	if len(p) > 1 && p[1] == ':' {
		p = "/" + p
	}
	return "file://" + p
}

func TestManagerCrashBackoffAndRestart(t *testing.T) {
	f := &fakeFactory{}
	now := time.Unix(100, 0)
	m := NewManagerWithClock(f, func() time.Time { return now })
	defer m.Close()
	cfg := ProjectConfig{ID: "game", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///game"}
	_, err := m.Client(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	p := m.sessions["game"].process
	m.mu.Unlock()
	_ = p.Kill()
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		s := m.sessions["game"]
		failed := s.client == nil
		delay := s.nextStart.Sub(now)
		m.mu.Unlock()
		if failed {
			if delay != time.Second {
				t.Fatalf("delay=%v", delay)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("crash not observed")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = m.Client(context.Background(), cfg); err == nil {
		t.Fatal("expected restart backoff")
	}
	now = now.Add(time.Second)
	if _, err := m.Client(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	starts := f.n
	f.mu.Unlock()
	if starts != 2 {
		t.Fatalf("starts=%d", starts)
	}
}

func TestManagerSharesSimultaneousStartup(t *testing.T) {
	f := &slowFactory{ready: make(chan struct{})}
	m := NewManager(f)
	defer m.Close()
	cfg := ProjectConfig{ID: "same", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///same"}
	out := make(chan *Client, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() { c, e := m.Client(context.Background(), cfg); out <- c; errs <- e }()
	}
	time.Sleep(10 * time.Millisecond)
	close(f.ready)
	var clients []*Client
	for range 2 {
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
		clients = append(clients, <-out)
	}
	if clients[0] != clients[1] {
		t.Fatal("startup was not shared")
	}
	f.base.mu.Lock()
	n := f.base.n
	f.base.mu.Unlock()
	if n != 1 {
		t.Fatalf("starts=%d", n)
	}
}

func TestManagerAcceptsCompilationDatabaseFileAndRejectsMismatch(t *testing.T) {
	f := &fakeFactory{}
	m := NewManager(f)
	defer m.Close()
	dir := t.TempDir()
	db := filepath.Join(dir, "compile_commands.json")
	if err := os.WriteFile(db, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := ProjectConfig{ID: "file", Clangd: "fake", CacheDir: dir, CompilationDatabase: db, RootURI: "file:///x"}
	if _, err := m.Client(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	m.Release("file")
	cfg.ID = "bad"
	cfg.CompilationDatabase = filepath.Join(t.TempDir(), "compile_commands.json")
	if _, err := m.Client(context.Background(), cfg); err == nil {
		t.Fatal("expected mismatch rejection")
	}
}
func TestCallerCancellationDoesNotOwnSharedProcess(t *testing.T) {
	f := &fakeFactory{}
	m := NewManager(f)
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := ProjectConfig{ID: "owned", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///x"}
	if _, err := m.Client(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := m.Client(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	n := f.n
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("caller cancellation restarted process: %d", n)
	}
}
func TestCloseDuringStartupReturnsClosedAndKillsProcess(t *testing.T) {
	f := &silentInitializeFactory{started: make(chan struct{})}
	m := NewManager(f)
	cfg := ProjectConfig{ID: "race", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///x"}
	result := make(chan error, 1)
	go func() { _, e := m.Client(context.Background(), cfg); result <- e }()
	select {
	case <-f.started:
	case <-time.After(time.Second):
		t.Fatal("startup did not begin")
	}
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		started := m.sessions["race"] != nil && m.sessions["race"].process != nil
		m.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	m.Close()
	if e := <-result; e != ErrClosed {
		t.Fatalf("got %v", e)
	}
	f.mu.Lock()
	p := f.process
	f.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("detached process")
	}
}

func TestCloseBeforeFactoryStartsDoesNotCreateProcess(t *testing.T) {
	f := &slowFactory{ready: make(chan struct{})}
	m := NewManager(f)
	result := make(chan error, 1)
	go func() {
		_, err := m.Client(context.Background(), ProjectConfig{ID: "before", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///x"})
		result <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		started := m.sessions["before"] != nil && m.sessions["before"].starting != nil
		m.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	m.Close()
	select {
	case err := <-result:
		if err != ErrClosed {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Client did not return")
	}
	close(f.ready)
	time.Sleep(10 * time.Millisecond)
	f.base.mu.Lock()
	p := f.base.last
	f.base.mu.Unlock()
	if p != nil {
		t.Fatal("factory created a process after Close")
	}
}
func TestCloseWakesAllStartupWaiters(t *testing.T) {
	f := &slowFactory{ready: make(chan struct{})}
	m := NewManager(f)
	cfg := ProjectConfig{ID: "wait", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///x"}
	errs := make(chan error, 2)
	for range 2 {
		go func() { _, e := m.Client(context.Background(), cfg); errs <- e }()
	}
	time.Sleep(10 * time.Millisecond)
	m.Close()
	close(f.ready)
	for range 2 {
		if e := <-errs; e != ErrClosed {
			t.Fatalf("got %v", e)
		}
	}
}

type silentInitializeFactory struct {
	mu      sync.Mutex
	started chan struct{}
	process *fakeProcess
}

func (f *silentInitializeFactory) Start(_ context.Context, _ string, _ []string, _ string) (Process, error) {
	a, b := net.Pipe()
	p := &fakeProcess{conn: a, done: make(chan struct{})}
	f.mu.Lock()
	f.process = p
	f.mu.Unlock()
	close(f.started)
	go func() { _, _ = io.Copy(io.Discard, b); _ = b.Close() }()
	return p, nil
}

func TestCloseReturnsPromptlyWhileInitializeNeverResponds(t *testing.T) {
	f := &silentInitializeFactory{started: make(chan struct{})}
	m := NewManager(f)
	result := make(chan error, 1)
	go func() {
		_, err := m.Client(context.Background(), ProjectConfig{ID: "silent", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///x"})
		result <- err
	}()
	select {
	case <-f.started:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	start := time.Now()
	m.Close()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Close blocked for %v", elapsed)
	}
	select {
	case err := <-result:
		if err != ErrClosed {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Client did not return after Close")
	}
	f.mu.Lock()
	p := f.process
	f.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("silent server was not killed")
	}
}

type gatedInitializeFactory struct {
	mu      sync.Mutex
	started chan struct{}
	respond chan struct{}
	process *fakeProcess
}

func (f *gatedInitializeFactory) Start(_ context.Context, _ string, _ []string, _ string) (Process, error) {
	a, b := net.Pipe()
	p := &fakeProcess{conn: a, done: make(chan struct{})}
	f.mu.Lock()
	f.process = p
	f.mu.Unlock()
	close(f.started)
	go func() {
		defer b.Close()
		body, err := readFrame(bufio.NewReader(b), 1024*1024)
		if err != nil {
			return
		}
		var request wireMessage
		if json.Unmarshal(body, &request) != nil {
			return
		}
		<-f.respond
		_, _ = b.Write(frameBytes(wireMessage{JSONRPC: "2.0", ID: request.ID, Result: mustJSON(map[string]any{})}))
		_, _ = io.Copy(io.Discard, b)
	}()
	return p, nil
}

func TestFirstCallerCancellationDoesNotOwnSharedStartup(t *testing.T) {
	f := &gatedInitializeFactory{started: make(chan struct{}), respond: make(chan struct{})}
	m := NewManager(f)
	defer m.Close()
	cacheDir := t.TempDir()
	canceled, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := m.Client(canceled, ProjectConfig{ID: "gated", Clangd: "fake", CacheDir: cacheDir, RootURI: "file:///x"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	select {
	case <-f.started:
	case <-time.After(time.Second):
		t.Fatal("startup did not continue after caller cancellation")
	}
	f.mu.Lock()
	p := f.process
	f.mu.Unlock()
	select {
	case <-p.done:
		t.Fatal("first caller cancellation killed shared process")
	default:
	}
	result := make(chan error, 1)
	go func() {
		_, err := m.Client(context.Background(), ProjectConfig{ID: "gated", Clangd: "fake", CacheDir: cacheDir, RootURI: "file:///x"})
		result <- err
	}()
	close(f.respond)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("second caller did not receive shared startup: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second caller did not complete")
	}
	m.Close()
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("Close did not kill shared process")
	}
}

func TestCanceledStartupSchedulesIdleShutdownAndClaimCancelsIt(t *testing.T) {
	newManager := func(f *gatedInitializeFactory) *Manager {
		return NewManagerWithClock(f, func() time.Time { return time.Unix(100, 0) })
	}
	t.Run("unclaimed startup stops after idle timeout", func(t *testing.T) {
		f := &gatedInitializeFactory{started: make(chan struct{}), respond: make(chan struct{})}
		m := newManager(f)
		defer m.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, _ = m.Client(ctx, ProjectConfig{ID: "idle", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///x", IdleTimeout: 30 * time.Millisecond})
		<-f.started
		close(f.respond)
		f.mu.Lock()
		p := f.process
		f.mu.Unlock()
		select {
		case <-p.done:
		case <-time.After(time.Second):
			t.Fatal("unclaimed startup was not stopped after idle timeout")
		}
	})
	t.Run("claim before idle timeout keeps session alive", func(t *testing.T) {
		f := &gatedInitializeFactory{started: make(chan struct{}), respond: make(chan struct{})}
		m := newManager(f)
		defer m.Close()
		cacheDir := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, _ = m.Client(ctx, ProjectConfig{ID: "claim", Clangd: "fake", CacheDir: cacheDir, RootURI: "file:///x", IdleTimeout: 80 * time.Millisecond})
		<-f.started
		result := make(chan error, 1)
		go func() {
			_, err := m.Client(context.Background(), ProjectConfig{ID: "claim", Clangd: "fake", CacheDir: cacheDir, RootURI: "file:///x", IdleTimeout: 80 * time.Millisecond})
			result <- err
		}()
		close(f.respond)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		p := f.process
		f.mu.Unlock()
		time.Sleep(120 * time.Millisecond)
		select {
		case <-p.done:
			t.Fatal("claim did not cancel the pending idle shutdown")
		default:
		}
		m.Release("claim")
		select {
		case <-p.done:
		case <-time.After(time.Second):
			t.Fatal("released session was not stopped")
		}
	})
}

// scriptedFactory hands the test exclusive, synchronous control of the
// fake clangd side of the pipe: unlike fakeFactory it runs no background
// auto-responder goroutine, so exactly one goroutine (the test) ever writes
// to the peer connection.
type scriptedFactory struct {
	mu   sync.Mutex
	n    int
	peer net.Conn
}

func (f *scriptedFactory) Start(_ context.Context, _ string, _ []string, _ string) (Process, error) {
	f.mu.Lock()
	f.n++
	a, b := net.Pipe()
	f.peer = b
	f.mu.Unlock()
	return &fakeProcess{conn: a, done: make(chan struct{})}, nil
}

func (f *scriptedFactory) spawns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// TestIndexGraceWindowStartsAfterStartupNotAtClientConstruction is a
// regression test: the grace timer must not start ticking at NewClient
// (before Initialize/DidOpen even reach clangd), or a slow cold startup
// (large compilation database scan, slow clangd initialize) can let it
// expire and mark the index "ready" before indexing was ever triggered.
func TestIndexGraceWindowStartsAfterStartupNotAtClientConstruction(t *testing.T) {
	f := &scriptedFactory{}
	m := NewManager(f)
	defer m.Close()
	const grace = 30 * time.Millisecond
	cfg := ProjectConfig{ID: "game", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///game", IndexGraceTimeout: grace}

	clientDone := make(chan error, 1)
	go func() {
		_, err := m.Client(context.Background(), cfg)
		clientDone <- err
	}()

	deadline := time.Now().Add(time.Second)
	var peer net.Conn
	for peer == nil && time.Now().Before(deadline) {
		f.mu.Lock()
		peer = f.peer
		f.mu.Unlock()
		if peer == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if peer == nil {
		t.Fatal("factory never started a process")
	}
	r := bufio.NewReader(peer)
	body, err := readFrame(r, 1<<20)
	if err != nil {
		t.Fatalf("read initialize: %v", err)
	}
	var initReq wireMessage
	if err := json.Unmarshal(body, &initReq); err != nil || initReq.Method != "initialize" {
		t.Fatalf("first request=%s (err=%v), want initialize", body, err)
	}

	// Hold the initialize reply well past the grace timeout. SessionState
	// cannot observe the Client's internal state yet at this point (the
	// session only exposes s.client, and therefore IndexPhase, once
	// startup finishes) -- the bug's effect is only observable the instant
	// startup completes below, not during this window.
	time.Sleep(grace * 5)

	if _, err := peer.Write(frameBytes(wireMessage{JSONRPC: "2.0", ID: initReq.ID, Result: mustJSON(map[string]any{})})); err != nil {
		t.Fatalf("write initialize result: %v", err)
	}
	body, err = readFrame(r, 4096)
	if err != nil {
		t.Fatalf("read initialized notification: %v", err)
	}
	var initializedNotif wireMessage
	if err := json.Unmarshal(body, &initializedNotif); err != nil || initializedNotif.Method != "initialized" {
		t.Fatalf("second message=%s (err=%v), want initialized notification", body, err)
	}
	if err := <-clientDone; err != nil {
		t.Fatalf("Client startup failed: %v", err)
	}
	defer m.Release("game")

	// The discriminating check: if the grace window had (wrongly) started
	// ticking at client construction, it already fired during the sleep
	// above -- unobservable until now -- and SessionState would report
	// "ready" the instant the session becomes visible, before indexing
	// could ever have been triggered or signaled.
	if got := m.SessionState("game").Phase; got == "ready" {
		t.Fatal("index reported ready immediately at startup, before indexing could have been triggered")
	}

	// The grace window starts only now (no compilation database, so no
	// seed didOpen follows initialize); wait past it and confirm it still
	// converges to ready on its own.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if m.SessionState("game").Phase == "ready" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("phase never reached ready after startup, got %q", m.SessionState("game").Phase)
}

func TestManagerSessionStateReflectsIndexPhaseWithoutBlockingOrExtraSpawn(t *testing.T) {
	f := &scriptedFactory{}
	m := NewManager(f)
	defer m.Close()
	cfg := ProjectConfig{ID: "game", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///game"}

	if got := m.SessionState("game").Phase; got != "absent" {
		t.Fatalf("phase before any session=%q, want absent", got)
	}

	clientDone := make(chan error, 1)
	go func() {
		_, err := m.Client(context.Background(), cfg)
		clientDone <- err
	}()

	// Answer initialize directly: this test drives clangd's side of the
	// protocol itself instead of using fakeFactory's auto-responder, so it
	// can inject $/progress notifications afterward without racing it.
	deadline := time.Now().Add(time.Second)
	var peer net.Conn
	for peer == nil && time.Now().Before(deadline) {
		f.mu.Lock()
		peer = f.peer
		f.mu.Unlock()
		if peer == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if peer == nil {
		t.Fatal("factory never started a process")
	}
	r := bufio.NewReader(peer)
	body, err := readFrame(r, 1<<20)
	if err != nil {
		t.Fatalf("read initialize: %v", err)
	}
	var initReq wireMessage
	if err := json.Unmarshal(body, &initReq); err != nil || initReq.Method != "initialize" {
		t.Fatalf("first request=%s (err=%v), want initialize", body, err)
	}
	if _, err := peer.Write(frameBytes(wireMessage{JSONRPC: "2.0", ID: initReq.ID, Result: mustJSON(map[string]any{})})); err != nil {
		t.Fatalf("write initialize result: %v", err)
	}
	// net.Pipe is synchronous: the client's "initialized" notification, sent
	// right after a successful initialize handshake, blocks its writeLoop
	// until read even though the client itself does not wait for it.
	body, err = readFrame(r, 4096)
	if err != nil {
		t.Fatalf("read initialized notification: %v", err)
	}
	var initializedNotif wireMessage
	if err := json.Unmarshal(body, &initializedNotif); err != nil || initializedNotif.Method != "initialized" {
		t.Fatalf("second message=%s (err=%v), want initialized notification", body, err)
	}

	if err := <-clientDone; err != nil {
		t.Fatalf("Client startup failed: %v", err)
	}
	defer m.Release("game")

	if got := m.SessionState("game").Phase; got != "starting" {
		t.Fatalf("phase right after startup, before any progress signal=%q, want starting", got)
	}

	settlePeer := func(id int) {
		t.Helper()
		if _, err := peer.Write(frameBytes(wireMessage{JSONRPC: "2.0", ID: mustJSON(id), Method: "workspace/configuration", Params: mustJSON(map[string]any{"items": []any{}})})); err != nil {
			t.Fatalf("write settle request: %v", err)
		}
		if _, err := readFrame(r, 4096); err != nil {
			t.Fatalf("read settle reply: %v", err)
		}
	}

	if _, err := peer.Write(frameBytes(wireMessage{JSONRPC: "2.0", Method: "$/progress", Params: mustJSON(map[string]any{"token": "x", "value": map[string]any{"kind": "begin"}})})); err != nil {
		t.Fatalf("write progress begin: %v", err)
	}
	settlePeer(1)
	if got := m.SessionState("game").Phase; got != "indexing" {
		t.Fatalf("phase during indexing=%q, want indexing", got)
	}

	if _, err := peer.Write(frameBytes(wireMessage{JSONRPC: "2.0", Method: "$/progress", Params: mustJSON(map[string]any{"token": "x", "value": map[string]any{"kind": "end"}})})); err != nil {
		t.Fatalf("write progress end: %v", err)
	}
	settlePeer(2)
	if got := m.SessionState("game").Phase; got != "ready" {
		t.Fatalf("phase after indexing finished=%q, want ready", got)
	}

	if got := f.spawns(); got != 1 {
		t.Fatalf("clangd spawns=%d, want exactly 1 (project_status must not start a session)", got)
	}
}

func TestExecFactoryClosesLogOnStartFailure(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "private.log")
	_, err := ExecFactory{}.Start(context.Background(), filepath.Join(dir, "missing.exe"), nil, log)
	if err == nil {
		t.Fatal("expected start failure")
	}
	if err := os.Rename(log, filepath.Join(dir, "renamed.log")); err != nil {
		t.Fatalf("log remains open: %v", err)
	}
}
