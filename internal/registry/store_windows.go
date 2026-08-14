//go:build windows

package registry

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	movefileReplaceExisting = 0x00000001
	movefileWriteThrough    = 0x00000008
)

var (
	errLockContended = errors.New("registry lock contended")
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	lockFileEx       = kernel32.NewProc("LockFileEx")
	unlockFileEx     = kernel32.NewProc("UnlockFileEx")
	moveFileEx       = kernel32.NewProc("MoveFileExW")
)

func lockFile(file *os.File) error {
	overlapped := syscall.Overlapped{}
	r1, _, err := lockFileEx.Call(file.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r1 != 0 {
		return nil
	}
	if err == syscall.Errno(32) || err == syscall.Errno(33) {
		return errLockContended
	}
	return fmt.Errorf("LockFileEx: %w", err)
}

func unlockFile(file *os.File) error {
	overlapped := syscall.Overlapped{}
	r1, _, err := unlockFileEx.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r1 != 0 {
		return nil
	}
	return fmt.Errorf("UnlockFileEx: %w", err)
}

func promoteFile(source, destination string) error {
	sourcePtr, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPtr, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	r1, _, callErr := moveFileEx.Call(uintptr(unsafe.Pointer(sourcePtr)), uintptr(unsafe.Pointer(destinationPtr)), movefileReplaceExisting|movefileWriteThrough)
	if r1 != 0 {
		return nil
	}
	return fmt.Errorf("MoveFileExW: %w", callErr)
}
