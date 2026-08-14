package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

type watcherNoopGenerator struct{}

func (watcherNoopGenerator) Generate(context.Context, string) error { return nil }

func TestNativeWatcherErrorIsSurfaced(t *testing.T) {
	w, err := NewWatcher(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	want := errors.New("watch buffer overflow")
	w.native.Errors <- want
	select {
	case got := <-w.Errors():
		if !errors.Is(got, want) {
			t.Fatalf("got %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("native watcher error was discarded")
	}
}

func TestCreatedDirectoryAttachmentFailureIsSurfaced(t *testing.T) {
	root := t.TempDir()
	created := filepath.Join(root, "Source", "NewModule")
	if err := os.MkdirAll(created, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := NewWatcher(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.AddProject(ProjectRoots{ID: "game", ProjectRoot: root}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("attach failed")
	w.attachTree = func(string) error { return want }
	w.handleEvent(fsnotify.Event{Name: created, Op: fsnotify.Create})
	select {
	case got := <-w.Errors():
		if !errors.Is(got, want) {
			t.Fatalf("got %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("tree attachment failure was discarded")
	}
}

func TestAddProjectPreservesEmptyEngineRoot(t *testing.T) {
	root := t.TempDir()
	c := NewCoordinator(nil, 1, time.Hour)
	defer c.Close()
	w, err := NewWatcher(c)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.AddProject(ProjectRoots{ID: "game", ProjectRoot: root}); err != nil {
		t.Fatal(err)
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if got := w.projects[0].EngineRoot; got != "" {
		t.Fatalf("EngineRoot=%q, want empty", got)
	}
}

func TestOrdinarySourceWriteNotifiesMatchingProjectWithoutRegeneration(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "Source")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sourceDir, "Foo.cpp")
	changes := make(chan SourceWrite, 1)
	results := make(chan Result, 1)
	c := NewCoordinatorWithOptions(nil, 1, 0, CoordinatorOptions{Result: func(result Result) { results <- result }})
	defer c.Close()
	w, err := NewWatcherWithOptions(c, WatcherOptions{SourceWrite: func(change SourceWrite) { changes <- change }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.AddProject(ProjectRoots{ID: "game", ProjectRoot: root}); err != nil {
		t.Fatal(err)
	}
	w.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Write})
	select {
	case change := <-changes:
		if change.ProjectID != "game" || change.Path != path {
			t.Fatalf("change=%+v", change)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary source write was ignored")
	}
	select {
	case <-results:
		t.Fatal("ordinary source write regenerated compilation database")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestAtomicSourceReplacementAndRemovalNotifyAndInvalidate(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "Source")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sourceDir, "Foo.cpp")
	changes := make(chan SourceWrite, 4)
	results := make(chan Result, 4)
	c := NewCoordinatorWithOptions(watcherNoopGenerator{}, 1, 0, CoordinatorOptions{Result: func(result Result) { results <- result }})
	defer c.Close()
	w, err := NewWatcherWithOptions(c, WatcherOptions{SourceWrite: func(change SourceWrite) { changes <- change }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.AddProject(ProjectRoots{ID: "game", ProjectRoot: root}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Create})
	if got := <-changes; got.Path != path || got.Removed {
		t.Fatalf("create change=%+v", got)
	}
	if got := <-results; got.ProjectID != "game" {
		t.Fatal(got)
	}
	for len(changes) > 0 {
		<-changes
	}
	for len(results) > 0 {
		<-results
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	w.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Rename})
	for {
		got := <-changes
		if got.Path == path && got.Removed {
			break
		}
	}
	if got := <-results; got.ProjectID != "game" {
		t.Fatal(got)
	}
}

func TestUnrealInstallEngineTreesAreWatchedAndClassified(t *testing.T) {
	project, install := t.TempDir(), t.TempDir()
	engineSource := filepath.Join(install, "Engine", "Source", "Runtime", "Core")
	if err := os.MkdirAll(engineSource, 0o755); err != nil {
		t.Fatal(err)
	}
	changes := make(chan SourceWrite, 1)
	results := make(chan Result, 8)
	c := NewCoordinatorWithOptions(watcherNoopGenerator{}, 1, 0, CoordinatorOptions{Result: func(r Result) { results <- r }})
	defer c.Close()
	w, err := NewWatcherWithOptions(c, WatcherOptions{SourceWrite: func(change SourceWrite) { changes <- change }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.AddProject(ProjectRoots{ID: "game", ProjectRoot: project, EngineRoot: install}); err != nil {
		t.Fatal(err)
	}
	cpp := filepath.Join(engineSource, "Core.cpp")
	w.handleEvent(fsnotify.Event{Name: cpp, Op: fsnotify.Write})
	select {
	case got := <-changes:
		if got.ProjectID != "game" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("engine source write was not routed")
	}
	build := filepath.Join(engineSource, "Core.Build.cs")
	w.handleEvent(fsnotify.Event{Name: build, Op: fsnotify.Write})
	select {
	case got := <-results:
		if got.ProjectID != "game" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("engine Build.cs did not invalidate")
	}
	newDir := filepath.Join(install, "Engine", "Source", "Runtime", "NewModule")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	w.handleEvent(fsnotify.Event{Name: newDir, Op: fsnotify.Create})
	if _, ok := w.dirs.Load(filepath.Clean(newDir)); !ok {
		t.Fatal("new engine source directory was not attached")
	}
	select {
	case <-results:
	case <-time.After(time.Second):
		t.Fatal("new engine directory did not invalidate")
	}
}

func TestCreatedOrMovedInRelevantDirectoryAttachesAndInvalidates(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Source", "Populated")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	g := newFakeGenerator()
	c := NewCoordinator(g, 1, 0)
	defer c.Close()
	w, err := NewWatcher(c)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.AddProject(ProjectRoots{ID: "game", ProjectRoot: root}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []fsnotify.Op{fsnotify.Create, fsnotify.Rename} {
		w.handleEvent(fsnotify.Event{Name: dir, Op: op})
		select {
		case got := <-g.started:
			if got != "game" {
				t.Fatal(got)
			}
			g.release <- struct{}{}
		case <-time.After(time.Second):
			t.Fatalf("%v directory did not invalidate", op)
		}
	}
	if _, watched := w.dirs.Load(filepath.Clean(dir)); !watched {
		t.Fatalf("%q was not watched", dir)
	}
}

func TestRemovedOrRenamedOutRelevantDirectoryInvalidatesAndCleansWatch(t *testing.T) {
	for _, kind := range []string{"Source", "Plugins"} {
		for _, op := range []fsnotify.Op{fsnotify.Remove, fsnotify.Rename} {
			t.Run(kind+"/"+op.String(), func(t *testing.T) {
				root := t.TempDir()
				dir := filepath.Join(root, kind, "Populated", "Nested")
				if err := os.MkdirAll(filepath.Join(root, kind), 0o755); err != nil {
					t.Fatal(err)
				}
				results := make(chan Result, 8)
				c := NewCoordinatorWithOptions(nil, 1, 0, CoordinatorOptions{Result: func(result Result) { results <- result }})
				defer c.Close()
				w, err := NewWatcher(c)
				if err != nil {
					t.Fatal(err)
				}
				defer w.Close()
				if err := w.AddProject(ProjectRoots{ID: "game", ProjectRoot: root}); err != nil {
					t.Fatal(err)
				}
				removed := filepath.Join(root, kind, "Populated")
				w.dirs.Store(filepath.Clean(removed), struct{}{})
				w.dirs.Store(filepath.Clean(dir), struct{}{})
				w.handleEvent(fsnotify.Event{Name: removed, Op: op})
				select {
				case got := <-results:
					if got.ProjectID != "game" {
						t.Fatal(got.ProjectID)
					}
				case <-time.After(time.Second):
					t.Fatal("missing directory did not invalidate")
				}
				if _, ok := w.dirs.Load(filepath.Clean(removed)); ok {
					t.Fatal("stale directory watch remains")
				}
				if _, ok := w.dirs.Load(filepath.Clean(filepath.Join(removed, "Nested"))); ok {
					t.Fatal("stale nested watch remains")
				}
				if err := os.MkdirAll(removed, 0o755); err != nil {
					t.Fatal(err)
				}
				w.handleEvent(fsnotify.Event{Name: removed, Op: fsnotify.Create})
				if _, ok := w.dirs.Load(filepath.Clean(removed)); !ok {
					t.Fatal("replacement directory was not watched")
				}
				select {
				case <-results:
				case <-time.After(time.Second):
					t.Fatal("replacement did not invalidate")
				}
			})
		}
	}
}
