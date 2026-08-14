package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeProcess struct {
	conn net.Conn
	done chan struct{}
	once sync.Once
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.conn }
func (p *fakeProcess) Stdout() io.ReadCloser { return p.conn }
func (p *fakeProcess) Wait() error           { <-p.done; return nil }
func (p *fakeProcess) Kill() error {
	p.once.Do(func() { close(p.done); _ = p.conn.Close() })
	return nil
}

type fakeFactory struct {
	mu   sync.Mutex
	n    int
	args [][]string
	last *fakeProcess
}
type slowFactory struct {
	base  fakeFactory
	ready chan struct{}
}

func (f *slowFactory) Start(_ context.Context, path string, args []string, log string) (Process, error) {
	<-f.ready
	return f.base.Start(context.Background(), path, args, log)
}

func (f *fakeFactory) Start(_ context.Context, _ string, args []string, _ string) (Process, error) {
	f.mu.Lock()
	f.n++
	f.args = append(f.args, append([]string(nil), args...))
	f.mu.Unlock()
	a, b := net.Pipe()
	p := &fakeProcess{conn: a, done: make(chan struct{})}
	f.last = p
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
			if m.Method == "initialize" || m.Method == "shutdown" {
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
	f := &slowFactory{ready: make(chan struct{})}
	m := NewManager(f)
	cfg := ProjectConfig{ID: "race", Clangd: "fake", CacheDir: t.TempDir(), RootURI: "file:///x"}
	result := make(chan error, 1)
	go func() { _, e := m.Client(context.Background(), cfg); result <- e }()
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		started := m.sessions["race"] != nil && m.sessions["race"].starting != nil
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
	close(f.ready)
	if e := <-result; e != ErrClosed {
		t.Fatalf("got %v", e)
	}
	f.base.mu.Lock()
	p := f.base.last
	f.base.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("detached process")
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
