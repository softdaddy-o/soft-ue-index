//go:build !windows

package registry

import (
	"errors"
	"os"
)

var errLockContended = errors.New("registry lock contended")

func lockFile(*os.File) error                      { return nil }
func unlockFile(*os.File) error                    { return nil }
func promoteFile(source, destination string) error { return os.Rename(source, destination) }
