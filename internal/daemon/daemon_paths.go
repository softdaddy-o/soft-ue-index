package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultPipePath returns the per-user Windows named pipe endpoint.
func DefaultPipePath() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return pipePath(u.Username, u.HomeDir), nil
}

// DefaultStartupLockPath returns the per-user startup lock path used for singleton ownership.
func DefaultStartupLockPath() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(defaultRoot(), defaultLockBase(u.Username, u.HomeDir), "startup.lock"), nil
}

func DefaultDaemonLockPath() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(defaultRoot(), defaultLockBase(u.Username, u.HomeDir), "daemon.lock"), nil
}

func stableUserTag(username string, home string) string {
	normalized := fmt.Sprintf("%s|%s|%s", strings.ToLower(strings.TrimSpace(username)), strings.ToLower(strings.TrimSpace(home)), runtime.GOOS)
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])[:12]
}

func defaultPipeBase(username string, home string) string {
	return fmt.Sprintf("soft-ue-index-daemon-%s", stableUserTag(username, home))
}

func defaultLockBase(username string, home string) string {
	return filepath.Join("soft-ue-index", "daemon", stableUserTag(username, home))
}

func defaultRoot() string {
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		return cache
	}
	return filepath.Join(os.TempDir(), "soft-ue-index")
}

func pipePath(username string, home string) string {
	return `\\.\pipe\` + defaultPipeBase(username, home)
}
