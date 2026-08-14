package registry

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AcquireFileLock holds an OS-backed advisory lock until the returned release
// function is called. The lock file is durable; ownership is tied to the open
// handle, so process termination releases it safely.
func AcquireFileLock(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	for {
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open lock: %w", err)
		}
		err = lockFile(file)
		if err == nil {
			return func() { _ = unlockFile(file); _ = file.Close() }, nil
		}
		_ = file.Close()
		if err != errLockContended {
			return nil, fmt.Errorf("acquire lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
