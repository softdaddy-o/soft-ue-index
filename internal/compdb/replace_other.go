//go:build !windows

package compdb

import "os"

func replaceFile(source, destination string) error { return os.Rename(source, destination) }
