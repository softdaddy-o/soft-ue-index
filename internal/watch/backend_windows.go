//go:build windows

package watch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/windows"
)

const rootWatchBufferSize = 64 * 1024

type watchState uint8

const (
	watchActive watchState = iota
	watchCanceling
	watchClosed
)

type windowsRootWatch struct {
	ov        windows.Overlapped
	root      string
	handle    windows.Handle
	recursive bool
	buffer    []byte
	stopped   chan struct{}
	state     watchState
}

type windowsRootBackend struct {
	port    windows.Handle
	events  chan fsnotify.Event
	errors  chan error
	done    chan struct{}
	mu      sync.Mutex
	watches map[string]*windowsRootWatch
	closeMu sync.Mutex
	closed  bool
	wg      sync.WaitGroup
}

func newRootBackend() (rootBackend, error) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 0)
	if err != nil {
		return nil, os.NewSyscallError("CreateIoCompletionPort", err)
	}
	b := &windowsRootBackend{
		port: port, events: make(chan fsnotify.Event, 256), errors: make(chan error, 1),
		done: make(chan struct{}), watches: make(map[string]*windowsRootWatch),
	}
	b.wg.Add(1)
	go b.readCompletions()
	return b, nil
}

func (b *windowsRootBackend) Add(spec WatchSpec) error {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	if b.closed {
		return errors.New("watch backend is closed")
	}
	root := filepath.Clean(spec.Root)
	b.mu.Lock()
	defer b.mu.Unlock()
	if existing := b.watches[root]; existing != nil && existing.state != watchClosed {
		if existing.recursive == spec.Recursive {
			return nil
		}
		return fmt.Errorf("watch %q already exists with recursive=%t", root, existing.recursive)
	}
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(root), windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return os.NewSyscallError("CreateFile", err)
	}
	if _, err = windows.CreateIoCompletionPort(handle, b.port, 0, 0); err != nil {
		_ = windows.CloseHandle(handle)
		return os.NewSyscallError("CreateIoCompletionPort", err)
	}
	w := &windowsRootWatch{root: root, handle: handle, recursive: spec.Recursive, buffer: make([]byte, rootWatchBufferSize), stopped: make(chan struct{}), state: watchActive}
	b.watches[root] = w
	if err := b.startReadLocked(w); err != nil {
		delete(b.watches, root)
		_ = windows.CloseHandle(handle)
		return err
	}
	return nil
}

func (b *windowsRootBackend) startReadLocked(w *windowsRootWatch) error {
	mask := uint32(windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_SIZE | windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_CREATION)
	if err := windows.ReadDirectoryChanges(w.handle, &w.buffer[0], uint32(len(w.buffer)), w.recursive, mask, nil, &w.ov, 0); err != nil {
		return os.NewSyscallError("ReadDirectoryChanges", err)
	}
	return nil
}

func (b *windowsRootBackend) Remove(root string) error {
	root = filepath.Clean(root)
	b.mu.Lock()
	w := b.watches[root]
	if w == nil || w.state == watchClosed {
		b.mu.Unlock()
		return nil
	}
	if w.state == watchActive {
		w.state = watchCanceling
		err := windows.CancelIoEx(w.handle, &w.ov)
		if err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) {
			w.state = watchActive
			b.mu.Unlock()
			return os.NewSyscallError("CancelIoEx", err)
		}
		// ERROR_NOT_FOUND can mean completion is already queued. The watch must
		// remain alive until readCompletions positively drains that packet.
	}
	stopped := w.stopped
	b.mu.Unlock()
	select {
	case <-stopped:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("cancel watch %q: completion timeout", root)
	}
}

func (b *windowsRootBackend) finishLocked(w *windowsRootWatch) {
	if w.state == watchClosed {
		return
	}
	w.state = watchClosed
	delete(b.watches, w.root)
	_ = windows.CloseHandle(w.handle)
	close(w.stopped)
}

func (b *windowsRootBackend) readCompletions() {
	defer b.wg.Done()
	defer close(b.events)
	defer close(b.errors)
	for {
		var bytes uint32
		var key uintptr
		var ov *windows.Overlapped
		err := windows.GetQueuedCompletionStatus(b.port, &bytes, &key, &ov, windows.INFINITE)
		if ov == nil {
			select {
			case <-b.done:
				return
			default:
				if err != nil {
					b.sendError(os.NewSyscallError("GetQueuedCompletionStatus", err))
				}
				continue
			}
		}
		w := (*windowsRootWatch)(unsafe.Pointer(ov))
		b.mu.Lock()
		if w.state == watchCanceling || errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
			b.finishLocked(w)
			b.mu.Unlock()
			continue
		}
		if errors.Is(err, windows.ERROR_MORE_DATA) {
			w.state = watchCanceling
			b.finishLocked(w)
			b.mu.Unlock()
			b.sendError(errors.New("filesystem notification overflow"))
			continue
		}
		if err != nil {
			w.state = watchCanceling
			b.finishLocked(w)
			b.mu.Unlock()
			b.sendError(os.NewSyscallError("GetQueuedCompletionStatus", err))
			continue
		}
		var events []fsnotify.Event
		if bytes > 0 {
			events = b.parseEventsLocked(w, bytes)
		}
		if w.state == watchActive {
			if readErr := b.startReadLocked(w); readErr != nil {
				w.state = watchCanceling
				b.finishLocked(w)
				b.mu.Unlock()
				b.sendError(readErr)
				continue
			}
		}
		b.mu.Unlock()
		for _, event := range events {
			select {
			case b.events <- event:
			case <-b.done:
				return
			default:
				b.sendError(errors.New("filesystem event buffer overflow"))
			}
		}
	}
}

func (b *windowsRootBackend) parseEventsLocked(w *windowsRootWatch, bytes uint32) []fsnotify.Event {
	events := make([]fsnotify.Event, 0, 4)
	for offset := uint32(0); offset < bytes; {
		raw := (*windows.FileNotifyInformation)(unsafe.Pointer(&w.buffer[offset]))
		name := windows.UTF16ToString(unsafe.Slice(&raw.FileName, raw.FileNameLength/2))
		event := fsnotify.Event{Name: filepath.Join(w.root, name)}
		switch raw.Action {
		case windows.FILE_ACTION_ADDED, windows.FILE_ACTION_RENAMED_NEW_NAME:
			event.Op = fsnotify.Create
		case windows.FILE_ACTION_REMOVED:
			event.Op = fsnotify.Remove
		case windows.FILE_ACTION_MODIFIED:
			event.Op = fsnotify.Write
		case windows.FILE_ACTION_RENAMED_OLD_NAME:
			event.Op = fsnotify.Rename
		}
		if event.Op != 0 {
			events = append(events, event)
		}
		if raw.NextEntryOffset == 0 {
			break
		}
		offset += raw.NextEntryOffset
	}
	return events
}

func (b *windowsRootBackend) sendError(err error) {
	select {
	case b.errors <- err:
	default:
	}
}

func (b *windowsRootBackend) Events() <-chan fsnotify.Event { return b.events }
func (b *windowsRootBackend) Errors() <-chan error          { return b.errors }

func (b *windowsRootBackend) Close() error {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	if b.closed {
		return nil
	}
	b.mu.Lock()
	roots := make([]string, 0, len(b.watches))
	for root := range b.watches {
		roots = append(roots, root)
	}
	b.mu.Unlock()
	var closeErr error
	for _, root := range roots {
		if err := b.Remove(root); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	if closeErr != nil {
		return closeErr
	}
	b.closed = true
	close(b.done)
	_ = windows.PostQueuedCompletionStatus(b.port, 0, 0, nil)
	b.wg.Wait()
	if err := windows.CloseHandle(b.port); err != nil {
		return os.NewSyscallError("CloseHandle", err)
	}
	return nil
}
