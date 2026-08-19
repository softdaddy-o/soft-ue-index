//go:build !windows

package mcpserver

// isInvalidPathSyntax's Windows-specific classification (ERROR_INVALID_NAME)
// has no equivalent here; the project targets Windows (see README
// prerequisites), so this stub exists only so the package still builds and
// its tests fail loudly -- not silently misclassify -- on another OS.
func isInvalidPathSyntax(error) bool {
	return false
}
