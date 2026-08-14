package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

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
