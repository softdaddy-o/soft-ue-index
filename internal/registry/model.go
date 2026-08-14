// Package registry persists the user's registered Unreal projects.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Ready reports whether the generated compilation database is present and has
// the expected JSON shape. Entries are decoded one at a time, so even a large
// Unreal database is validated with bounded memory and without regeneration.
func (p Project) Ready() bool {
	if p.Generation.CacheDir == "" || p.Generation.CompilationDatabase == "" {
		return false
	}
	cache, err := os.Stat(p.Generation.CacheDir)
	if err != nil || !cache.IsDir() {
		return false
	}
	db, err := os.Open(p.Generation.CompilationDatabase)
	if err != nil {
		return false
	}
	defer db.Close()
	info, err := db.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	decoder := json.NewDecoder(db)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return false
	}
	entries := 0
	for decoder.More() {
		var entry struct {
			Directory string   `json:"directory"`
			File      string   `json:"file"`
			Command   string   `json:"command"`
			Arguments []string `json:"arguments"`
		}
		if decoder.Decode(&entry) != nil || entry.Directory == "" || entry.File == "" || (entry.Command == "" && len(entry.Arguments) == 0) {
			return false
		}
		entries++
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim(']') || entries == 0 {
		return false
	}
	var trailing any
	return decoder.Decode(&trailing) == io.EOF
}

const CurrentVersion = 1

var (
	ErrUnsupportedVersion = errors.New("unsupported registry version")
	ErrDuplicateProject   = errors.New("duplicate project")
)

// Registry is the versioned per-user project registry.
type Registry struct {
	Version  int       `json:"version"`
	Projects []Project `json:"projects"`
}

// Project describes one stable project registration.
type Project struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	UProject      string          `json:"uproject"`
	Target        string          `json:"target"`
	Platform      string          `json:"platform"`
	Configuration string          `json:"configuration"`
	WatchEnabled  bool            `json:"watchEnabled"`
	Engine        Engine          `json:"engine"`
	Toolchain     Toolchain       `json:"toolchain"`
	Generation    GenerationState `json:"generation"`
}

// Engine identifies the engine selected for a project.
type Engine struct {
	Root    string `json:"root"`
	Version string `json:"version"`
}

// Toolchain records the clangd selected for a project.
type Toolchain struct {
	ClangdPath    string `json:"clangdPath"`
	ClangdVersion string `json:"clangdVersion"`
}

// GenerationState records the last valid compilation database generation.
type GenerationState struct {
	CompilationDatabase string    `json:"compilationDatabase"`
	CacheDir            string    `json:"cacheDir"`
	LastFingerprint     string    `json:"lastFingerprint"`
	LastGeneratedAt     time.Time `json:"lastGeneratedAt"`
	InvalidationReason  string    `json:"invalidationReason"`
}

func normalizeAndValidate(registry Registry) (Registry, error) {
	if registry.Version != CurrentVersion {
		return Registry{}, fmt.Errorf("%w: %d", ErrUnsupportedVersion, registry.Version)
	}
	if registry.Projects == nil {
		registry.Projects = []Project{}
	}
	ids := make(map[string]struct{}, len(registry.Projects))
	paths := make(map[string]struct{}, len(registry.Projects))
	for i := range registry.Projects {
		project := &registry.Projects[i]
		var err error
		if project.UProject, err = normalizePath(project.UProject); err != nil {
			return Registry{}, err
		}
		if project.Engine.Root, err = normalizePath(project.Engine.Root); err != nil {
			return Registry{}, err
		}
		if project.Toolchain.ClangdPath, err = normalizePath(project.Toolchain.ClangdPath); err != nil {
			return Registry{}, err
		}
		if project.Generation.CompilationDatabase, err = normalizePath(project.Generation.CompilationDatabase); err != nil {
			return Registry{}, err
		}
		if project.Generation.CacheDir, err = normalizePath(project.Generation.CacheDir); err != nil {
			return Registry{}, err
		}
		if project.ID == "" {
			return Registry{}, errors.New("project ID is required")
		}
		if _, exists := ids[project.ID]; exists {
			return Registry{}, fmt.Errorf("%w: ID %q", ErrDuplicateProject, project.ID)
		}
		comparisonPath := strings.ToLower(project.UProject)
		if _, exists := paths[comparisonPath]; exists {
			return Registry{}, fmt.Errorf("%w: path %q", ErrDuplicateProject, project.UProject)
		}
		ids[project.ID] = struct{}{}
		paths[comparisonPath] = struct{}{}
	}
	return registry, nil
}

func normalizePath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("normalize path %q: %w", path, err)
	}
	return resolvePathIdentity(abs), nil
}

func resolvePathIdentity(path string) string {
	current := path
	missing := make([]string, 0)
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(path)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
