package watch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

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
