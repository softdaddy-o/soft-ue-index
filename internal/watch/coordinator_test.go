package watch

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeGenerator struct {
	mu          sync.Mutex
	started     chan string
	release     chan struct{}
	calls       []string
	active, max int
	errs        map[string]error
}

func newFakeGenerator() *fakeGenerator {
	return &fakeGenerator{started: make(chan string, 16), release: make(chan struct{}, 16), errs: make(map[string]error)}
}
func (g *fakeGenerator) Generate(_ context.Context, id string) error {
	g.mu.Lock()
	g.calls = append(g.calls, id)
	g.active++
	if g.active > g.max {
		g.max = g.active
	}
	g.mu.Unlock()
	g.started <- id
	<-g.release
	g.mu.Lock()
	g.active--
	g.mu.Unlock()
	return g.errs[id]
}

type fakeTimer struct{ stopped bool }

func (t *fakeTimer) Stop() bool {
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

type fakeClock struct {
	mu      sync.Mutex
	pending []struct {
		timer *fakeTimer
		fn    func()
	}
	added chan struct{}
}

func newFakeClock() *fakeClock { return &fakeClock{added: make(chan struct{}, 32)} }
func (c *fakeClock) AfterFunc(_ time.Duration, fn func()) Timer {
	c.mu.Lock()
	t := &fakeTimer{}
	c.pending = append(c.pending, struct {
		timer *fakeTimer
		fn    func()
	}{t, fn})
	c.mu.Unlock()
	c.added <- struct{}{}
	return t
}
func (c *fakeClock) FireNext(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	for len(c.pending) > 0 {
		next := c.pending[0]
		c.pending = c.pending[1:]
		if !next.timer.stopped {
			c.mu.Unlock()
			next.fn()
			return
		}
	}
	c.mu.Unlock()
	t.Fatal("no live timer pending")
}
func (g *fakeGenerator) count(id string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, v := range g.calls {
		if v == id {
			n++
		}
	}
	return n
}

func TestCoordinatorDebouncesAndSchedulesOneFollowup(t *testing.T) {
	g := newFakeGenerator()
	clock := newFakeClock()
	c := NewCoordinatorWithOptions(g, 1, 5*time.Millisecond, CoordinatorOptions{Clock: clock})
	defer c.Close()
	for range 5 {
		c.Invalidate("a")
	}
	for range 5 {
		<-clock.added
	}
	clock.FireNext(t)
	select {
	case id := <-g.started:
		if id != "a" {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("generation did not start")
	}
	c.Invalidate("a")
	c.Invalidate("a")
	g.release <- struct{}{}
	select {
	case <-clock.added:
	case <-time.After(time.Second):
		t.Fatal("followup was not scheduled")
	}
	clock.FireNext(t)
	select {
	case id := <-g.started:
		if id != "a" {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("followup did not start")
	}
	g.release <- struct{}{}
	if got := g.count("a"); got != 2 {
		t.Fatalf("calls=%d, want 2", got)
	}
}

func TestCoordinatorReportsFailureAndContinuesOtherProjects(t *testing.T) {
	g := newFakeGenerator()
	g.errs["a"] = context.DeadlineExceeded
	results := make(chan Result, 2)
	c := NewCoordinatorWithOptions(g, 2, 0, CoordinatorOptions{Result: func(result Result) { results <- result }})
	defer c.Close()
	c.Invalidate("a")
	c.Invalidate("b")
	<-g.started
	<-g.started
	g.release <- struct{}{}
	g.release <- struct{}{}
	got := map[string]error{}
	for range 2 {
		result := <-results
		got[result.ProjectID] = result.Err
	}
	if got["a"] != context.DeadlineExceeded {
		t.Fatalf("a result=%v", got["a"])
	}
	if got["b"] != nil {
		t.Fatalf("b result=%v", got["b"])
	}
}

func TestCoordinatorSerializesProjectAndBoundsGlobalConcurrency(t *testing.T) {
	g := newFakeGenerator()
	c := NewCoordinator(g, 2, time.Millisecond)
	defer c.Close()
	c.Invalidate("a")
	c.Invalidate("b")
	c.Invalidate("c")
	<-g.started
	<-g.started
	g.mu.Lock()
	if g.max != 2 {
		t.Fatalf("max=%d", g.max)
	}
	g.mu.Unlock()
	g.release <- struct{}{}
	g.release <- struct{}{}
	select {
	case <-g.started:
	case <-time.After(time.Second):
		t.Fatal("third project did not run")
	}
	g.release <- struct{}{}
}
