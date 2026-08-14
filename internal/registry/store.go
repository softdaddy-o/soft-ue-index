package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	registryFileName = "registry.json"
	lockFileName     = "registry.lock"
	lockRetryDelay   = 10 * time.Millisecond
	lockRetries      = 20
)

var ErrLockUnavailable = errors.New("registry lock unavailable")

var defaultRoot = DefaultRoot

// Store provides locked, atomic access to a registry rooted in dir.
type Store struct {
	dir     string
	promote func(source, destination string) error
}

// NewStore creates a store rooted at dir. An empty dir selects DefaultRoot.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		var err error
		dir, err = defaultRoot()
		if err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir, promote: promoteFile}, nil
}

// DefaultRoot returns the platform-specific directory used for this registry.
func DefaultRoot() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil || root == "" {
		root, err = os.UserCacheDir()
	}
	if err != nil || root == "" {
		if err == nil {
			err = errors.New("user configuration and cache directories are empty")
		}
		return "", fmt.Errorf("resolve registry root: %w", err)
	}
	return filepath.Join(root, "soft-ue-index"), nil
}

// Load reads and validates the registry. A missing registry is an empty v1 registry.
func (s *Store) Load(ctx context.Context) (Registry, error) {
	if err := ctx.Err(); err != nil {
		return Registry{}, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Registry{}, fmt.Errorf("create registry directory: %w", err)
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return Registry{}, err
	}
	defer unlock()
	contents, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return Registry{Version: CurrentVersion, Projects: []Project{}}, nil
	}
	if err != nil {
		return Registry{}, fmt.Errorf("read registry: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var registry Registry
	if err := decoder.Decode(&registry); err != nil {
		return Registry{}, fmt.Errorf("decode registry: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return Registry{}, err
	}
	return normalizeAndValidate(registry)
}

// Save validates registry and promotes it only after a complete, synced temporary write.
func (s *Store) Save(ctx context.Context, registry Registry) error {
	registry, err := normalizeAndValidate(registry)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create registry directory: %w", err)
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(s.dir, ".registry-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary registry: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary registry permissions: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(registry); err != nil {
		temporary.Close()
		return fmt.Errorf("encode registry: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary registry: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary registry: %w", err)
	}
	if err := s.promote(temporaryPath, s.path()); err != nil {
		return fmt.Errorf("promote registry: %w", err)
	}
	return nil
}

func (s *Store) path() string { return filepath.Join(s.dir, registryFileName) }

func (s *Store) lock(ctx context.Context) (func(), error) {
	path := filepath.Join(s.dir, lockFileName)
	for attempt := 0; attempt < lockRetries; attempt++ {
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err == nil {
			err = lockFile(file)
			if err == nil {
				return func() { _ = unlockFile(file); _ = file.Close() }, nil
			}
			_ = file.Close()
		}
		if !errors.Is(err, errLockContended) {
			return nil, fmt.Errorf("lock registry: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockRetryDelay):
		}
	}
	return nil, ErrLockUnavailable
}

func requireEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("registry contains trailing JSON data")
	}
	return nil
}
