package watch

import "github.com/fsnotify/fsnotify"

type WatchSpec struct {
	Root      string
	Recursive bool
}

type rootBackend interface {
	Add(WatchSpec) error
	Remove(string) error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
}
