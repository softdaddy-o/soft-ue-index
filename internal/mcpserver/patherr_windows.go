//go:build windows

package mcpserver

import (
	"errors"
	"syscall"
)

// isInvalidPathSyntax reports whether err is Windows' ERROR_INVALID_NAME
// (123): the path contains a character Windows never allows (e.g. '*',
// '?') rather than pointing somewhere missing or out of bounds. Confirmed
// by direct probe: filepath.EvalSymlinks(`...\Sou*rce`) returns
// *fs.PathError{Err: syscall.Errno(123)}. This errno is Windows-specific
// (on Linux the same numeric value is ENOMEDIUM, a real and unrelated
// error), so the check is isolated behind this build tag rather than
// compared unconditionally in the portable server.go, matching the
// existing errno-check split in internal/registry/store_windows.go.
func isInvalidPathSyntax(err error) bool {
	return errors.Is(err, syscall.Errno(123))
}
