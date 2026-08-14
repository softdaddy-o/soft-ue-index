package watch

import (
	"context"
	"sync"
	"time"
)

// Generator renews one project's compilation database.
type Generator interface {
	Generate(context.Context, string) error
}

// Timer and Clock make debouncing deterministic in tests.
type Timer interface{ Stop() bool }
type Clock interface {
	AfterFunc(time.Duration, func()) Timer
}
type realClock struct{}

func (realClock) AfterFunc(delay time.Duration, fn func()) Timer { return time.AfterFunc(delay, fn) }

// Result is reported after each generation, including failures. Callers can persist status.
type Result struct {
	ProjectID string
	Err       error
}
type CoordinatorOptions struct {
	Clock  Clock
	Result func(Result)
}

// Coordinator coalesces invalidations while preventing same-project overlap.
type Coordinator struct {
	ctx       context.Context
	cancel    context.CancelFunc
	generator Generator
	debounce  time.Duration
	clock     Clock
	onResult  func(Result)
	permits   chan struct{}
	mu        sync.Mutex
	projects  map[string]*projectState
	closed    bool
	wg        sync.WaitGroup
}
type projectState struct {
	timer            Timer
	running, pending bool
}

func NewCoordinator(generator Generator, maxConcurrent int, debounce time.Duration) *Coordinator {
	return NewCoordinatorWithOptions(generator, maxConcurrent, debounce, CoordinatorOptions{})
}

func NewCoordinatorWithOptions(generator Generator, maxConcurrent int, debounce time.Duration, options CoordinatorOptions) *Coordinator {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if debounce < 0 {
		debounce = 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	if options.Clock == nil {
		options.Clock = realClock{}
	}
	return &Coordinator{ctx: ctx, cancel: cancel, generator: generator, debounce: debounce, clock: options.Clock, onResult: options.Result, permits: make(chan struct{}, maxConcurrent), projects: make(map[string]*projectState)}
}

// Invalidate records a meaningful filesystem change. It is safe from watcher goroutines.
func (c *Coordinator) Invalidate(projectID string) {
	if projectID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	p := c.projects[projectID]
	if p == nil {
		p = &projectState{}
		c.projects[projectID] = p
	}
	if p.running {
		p.pending = true
		return
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = c.clock.AfterFunc(c.debounce, func() { c.start(projectID) })
}

func (c *Coordinator) start(id string) {
	c.mu.Lock()
	p := c.projects[id]
	if c.closed || p == nil || p.running {
		c.mu.Unlock()
		return
	}
	p.timer = nil
	p.running = true
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		select {
		case c.permits <- struct{}{}:
		case <-c.ctx.Done():
			c.finish(id)
			return
		}
		var err error
		if c.generator != nil {
			err = c.generator.Generate(c.ctx, id)
		}
		<-c.permits
		if c.onResult != nil {
			c.onResult(Result{ProjectID: id, Err: err})
		}
		c.finish(id)
	}()
}
func (c *Coordinator) finish(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.projects[id]
	if p == nil {
		return
	}
	p.running = false
	if p.pending && c.ctx.Err() == nil {
		p.pending = false
		p.timer = c.clock.AfterFunc(c.debounce, func() { c.start(id) })
	}
}

// Close cancels outstanding work and waits for active generations. It is idempotent.
func (c *Coordinator) Close() {
	c.cancel()
	c.mu.Lock()
	c.closed = true
	for _, p := range c.projects {
		if p.timer != nil {
			p.timer.Stop()
		}
	}
	c.mu.Unlock()
	c.wg.Wait()
}
