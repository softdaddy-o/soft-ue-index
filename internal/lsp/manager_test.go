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
}
type slowFactory struct {
	base  fakeFactory
	ready chan struct{}
}

func (f *slowFactory) Start(ctx context.Context, path string, args []string, log string) (Process, error) {
	select {
	case <-f.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.base.Start(ctx, path, args, log)
}

func (f *fakeFactory) Start(_ context.Context, _ string, args []string, _ string) (Process, error) {
	f.mu.Lock()
	f.n++
	f.args = append(f.args, append([]string(nil), args...))
	f.mu.Unlock()
	a, b := net.Pipe()
	p := &fakeProcess{conn: a, done: make(chan struct{})}
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
