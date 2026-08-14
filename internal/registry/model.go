// Package registry persists the user's registered Unreal projects.
package registry

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

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
		if _, exists := paths[project.UProject]; exists {
			return Registry{}, fmt.Errorf("%w: path %q", ErrDuplicateProject, project.UProject)
		}
		ids[project.ID] = struct{}{}
		paths[project.UProject] = struct{}{}
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
	return filepath.Clean(abs), nil
}
