//go:build !windows

package watch

import (
	"io/fs"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

type fsnotifyRootBackend struct{ watcher *fsnotify.Watcher }

func newRootBackend() (rootBackend, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &fsnotifyRootBackend{watcher: w}, nil
}

func (b *fsnotifyRootBackend) Add(spec WatchSpec) error {
	if !spec.Recursive {
		return b.watcher.Add(spec.Root)
	}
	return filepath.WalkDir(spec.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		if path != spec.Root && IgnoredDirectory(path) {
			return filepath.SkipDir
		}
		return b.watcher.Add(path)
	})
}

func (b *fsnotifyRootBackend) Remove(root string) error      { return b.watcher.Remove(root) }
func (b *fsnotifyRootBackend) Events() <-chan fsnotify.Event { return b.watcher.Events }
func (b *fsnotifyRootBackend) Errors() <-chan error          { return b.watcher.Errors }
func (b *fsnotifyRootBackend) Close() error                  { return b.watcher.Close() }
