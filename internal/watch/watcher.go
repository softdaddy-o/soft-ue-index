package watch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// ProjectRoots contains roots relevant to one project. Engine roots are shared safely.
type ProjectRoots struct{ ID, ProjectRoot, EngineRoot string }

// Watcher owns one fsnotify watcher and translates events into coordinated invalidations.
type Watcher struct {
	native      *fsnotify.Watcher
	coordinator *Coordinator
	dirs        sync.Map
	done        chan struct{}
	once        sync.Once
	mu          sync.RWMutex
	projects    []ProjectRoots
}

func NewWatcher(coordinator *Coordinator) (*Watcher, error) {
	n, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{native: n, coordinator: coordinator, done: make(chan struct{})}
	go w.loop()
	return w, nil
}

func (w *Watcher) AddProject(project ProjectRoots) error {
	if project.ID == "" || project.ProjectRoot == "" {
		return fmt.Errorf("project ID and root are required")
	}
	project.ProjectRoot = filepath.Clean(project.ProjectRoot)
	project.EngineRoot = filepath.Clean(project.EngineRoot)
	if err := w.addDir(project.ProjectRoot); err != nil {
		return err
	}
	if err := w.addRelevantTrees(project.ProjectRoot); err != nil {
		return err
	}
	if project.EngineRoot != "" {
		if err := w.addRelevantTrees(project.EngineRoot); err != nil {
			return err
		}
	}
	w.mu.Lock()
	w.projects = append(w.projects, project)
	w.mu.Unlock()
	return nil
}
func (w *Watcher) addRelevantTrees(root string) error {
	for _, name := range []string{"Source", "Plugins", "Config"} {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err == nil {
			if err := w.addTree(path); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
func (w *Watcher) addTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && IgnoredDirectory(path) {
			return filepath.SkipDir
		}
		return w.addDir(path)
	})
}
func (w *Watcher) addDir(path string) error {
	path = filepath.Clean(path)
	if IgnoredDirectory(path) {
		return nil
	}
	if _, loaded := w.dirs.LoadOrStore(path, struct{}{}); loaded {
		return nil
	}
	if err := w.native.Add(path); err != nil {
		w.dirs.Delete(path)
		return err
	}
	return nil
}
func (w *Watcher) loop() {
	for {
		select {
		case <-w.done:
			return
		case event, ok := <-w.native.Events:
			if !ok {
				return
			}
			if IgnoredDirectory(event.Name) {
				continue
			}
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					_ = w.addTree(event.Name)
					continue
				}
			}
			if RequiresCompDB(event.Name, event.Op) && w.coordinator != nil {
				for _, id := range w.projectsFor(event.Name) {
					w.coordinator.Invalidate(id)
				}
			}
		case <-w.native.Errors: // errors are transient; keep unrelated projects alive.
		}
	}
}
func (w *Watcher) projectsFor(path string) []string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	ids := make([]string, 0, len(w.projects))
	for _, p := range w.projects {
		if contains(p.ProjectRoot, path) || (p.EngineRoot != "" && contains(p.EngineRoot, path)) {
			ids = append(ids, p.ID)
		}
	}
	return ids
}
func contains(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !filepath.IsAbs(relative)
}
func (w *Watcher) Close() error {
	var err error
	w.once.Do(func() { close(w.done); err = w.native.Close() })
	return err
}
