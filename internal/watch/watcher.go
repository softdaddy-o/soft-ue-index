package watch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// ProjectRoots contains roots relevant to one project. Engine roots are shared safely.
type ProjectRoots struct{ ID, ProjectRoot, EngineRoot string }
type SourceWrite struct{ ProjectID, Path string }
type WatcherOptions struct{ SourceWrite func(SourceWrite) }

// Watcher owns one fsnotify watcher and translates events into coordinated invalidations.
type Watcher struct {
	native        *fsnotify.Watcher
	coordinator   *Coordinator
	onSourceWrite func(SourceWrite)
	attachTree    func(string) error
	dirs          sync.Map
	done          chan struct{}
	errors        chan error
	once          sync.Once
	failOnce      sync.Once
	mu            sync.RWMutex
	projects      []ProjectRoots
}

func NewWatcher(coordinator *Coordinator) (*Watcher, error) {
	return NewWatcherWithOptions(coordinator, WatcherOptions{})
}

func NewWatcherWithOptions(coordinator *Coordinator, options WatcherOptions) (*Watcher, error) {
	n, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{native: n, coordinator: coordinator, onSourceWrite: options.SourceWrite, done: make(chan struct{}), errors: make(chan error, 1)}
	w.attachTree = w.addTree
	go w.loop()
	return w, nil
}

func (w *Watcher) AddProject(project ProjectRoots) error {
	if project.ID == "" || project.ProjectRoot == "" {
		return fmt.Errorf("project ID and root are required")
	}
	var err error
	if project.ProjectRoot, err = canonicalRoot(project.ProjectRoot); err != nil {
		return err
	}
	if project.EngineRoot != "" {
		if project.EngineRoot, err = canonicalRoot(project.EngineRoot); err != nil {
			return err
		}
		project.EngineRoot = filepath.Join(project.EngineRoot, "Engine")
	}
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
func canonicalRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
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
			w.handleEvent(event)
		case err, ok := <-w.native.Errors:
			if !ok {
				return
			}
			w.fail(err)
			return
		}
	}
}
func (w *Watcher) handleEvent(event fsnotify.Event) {
	if IgnoredDirectory(event.Name) {
		return
	}
	ids := w.projectsFor(event.Name)
	if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() && w.isRelevantDirectory(event.Name) {
			if err := w.attachTree(event.Name); err != nil {
				w.fail(err)
				return
			}
			if w.coordinator != nil {
				for _, id := range ids {
					w.coordinator.Invalidate(id)
				}
			}
			return
		}
	}
	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && w.isWatchedDirectory(event.Name) {
		w.removeTree(event.Name)
		if w.isRelevantDirectory(event.Name) && w.coordinator != nil {
			for _, id := range ids {
				w.coordinator.Invalidate(id)
			}
		}
		return
	}
	if RequiresCompDB(event.Name, event.Op) && w.coordinator != nil {
		for _, id := range ids {
			w.coordinator.Invalidate(id)
		}
		return
	}
	if event.Op&fsnotify.Write != 0 && isSourceFile(filepath.Base(event.Name)) && w.onSourceWrite != nil {
		for _, id := range ids {
			w.onSourceWrite(SourceWrite{ProjectID: id, Path: event.Name})
		}
	}
}
func (w *Watcher) fail(err error) {
	if err == nil {
		return
	}
	w.failOnce.Do(func() { w.errors <- err })
}

func (w *Watcher) Errors() <-chan error { return w.errors }

func (w *Watcher) isWatchedDirectory(path string) bool {
	_, ok := w.dirs.Load(filepath.Clean(path))
	return ok
}
func (w *Watcher) removeTree(root string) {
	root = filepath.Clean(root)
	w.dirs.Range(func(key, _ any) bool {
		path := key.(string)
		if contains(root, path) {
			_ = w.native.Remove(path)
			w.dirs.Delete(path)
		}
		return true
	})
}
func (w *Watcher) isRelevantDirectory(path string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, p := range w.projects {
		for _, root := range []string{p.ProjectRoot, p.EngineRoot} {
			if root == "" {
				continue
			}
			rel, err := filepath.Rel(root, path)
			if err != nil || rel == ".." || filepath.IsAbs(rel) {
				continue
			}
			first := strings.ToLower(strings.Split(filepath.ToSlash(rel), "/")[0])
			if first == "source" || first == "plugins" || first == "config" {
				return true
			}
		}
	}
	return false
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
	if err != nil || filepath.IsAbs(relative) || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
func (w *Watcher) Close() error {
	var err error
	w.once.Do(func() { close(w.done); err = w.native.Close() })
	return err
}
