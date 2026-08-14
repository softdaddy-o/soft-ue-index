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
	mu sync.Mutex
	n  int
}

func (f *fakeFactory) Start(_ context.Context, _ string, _ []string, _ string) (Process, error) {
	f.mu.Lock()
	f.n++
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
	cfg := ProjectConfig{ID: "game", Clangd: "fake", CompilationDatabase: "db", CacheDir: t.TempDir(), RootURI: "file:///game", IdleTimeout: time.Millisecond}
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
		c, e := m.Client(context.Background(), ProjectConfig{ID: id, Clangd: "fake", CompilationDatabase: dir, CacheDir: filepath.Join(base, "logs", id), RootURI: "file:///x"})
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
