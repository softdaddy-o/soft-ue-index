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
}

func newFakeGenerator() *fakeGenerator {
	return &fakeGenerator{started: make(chan string, 16), release: make(chan struct{}, 16)}
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
	return nil
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
	c := NewCoordinator(g, 1, 5*time.Millisecond)
	defer c.Close()
	for range 5 {
		c.Invalidate("a")
	}
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
	case id := <-g.started:
		if id != "a" {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("followup did not start")
	}
	g.release <- struct{}{}
	time.Sleep(25 * time.Millisecond)
	if got := g.count("a"); got != 2 {
		t.Fatalf("calls=%d, want 2", got)
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
