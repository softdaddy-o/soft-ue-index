// Package watch detects Unreal changes that require compilation database renewal.
package watch

import (
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// RequiresCompDB reports whether an event changes UBT's compilation model.
// Ordinary edits to existing source files are deliberately left to clangd.
func RequiresCompDB(path string, op fsnotify.Op) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(p)
	if strings.HasSuffix(base, ".build.cs") || strings.HasSuffix(base, ".target.cs") || strings.HasSuffix(base, ".uproject") || strings.HasSuffix(base, ".uplugin") {
		return true
	}
	if !isSourceFile(base) {
		return false
	}
	return op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0
}

func isSourceFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx", ".inl":
		return true
	}
	return false
}

// IgnoredDirectory reports generated/cache paths that must never be watched.
func IgnoredDirectory(path string) bool {
	for _, segment := range strings.Split(strings.ToLower(filepath.ToSlash(path)), "/") {
		switch segment {
		case "binaries", "intermediate", "saved", "deriveddatacache", ".git":
			return true
		}
	}
	return false
}
