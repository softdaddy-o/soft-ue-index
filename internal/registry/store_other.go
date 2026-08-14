//go:build !windows

package registry

import (
	"errors"
	"os"
	"syscall"
)

var errLockContended = errors.New("registry lock contended")

func lockFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errLockContended
		}
		return err
	}
	return nil
}
func unlockFile(file *os.File) error               { return syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }
func promoteFile(source, destination string) error { return os.Rename(source, destination) }
