// Package unreal discovers the minimum Unreal Engine metadata needed by the indexer.
package unreal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var (
	ErrAssociationNotFound     = errors.New("engine association not found")
	ErrMalformedEngine         = errors.New("malformed engine metadata")
	ErrUnsupportedVersion      = errors.New("unsupported Unreal Engine version")
	ErrUnrealBuildToolNotFound = errors.New("UnrealBuildTool.dll not found")
)

// AssociationSource resolves an Unreal Engine association such as UE_5.8.
type AssociationSource interface {
	Lookup(association string) (string, bool, error)
}

// MapAssociationSource is an in-memory association source intended for tests and callers
// that already have the association mapping.
type MapAssociationSource map[string]string

func (source MapAssociationSource) Lookup(association string) (string, bool, error) {
	root, found := source[association]
	return root, found, nil
}

// Version is the Unreal Engine major and minor version.
type Version struct {
	Major int
	Minor int
}

// Engine identifies a discovered Unreal Engine installation.
type Engine struct {
	Root                string
	Version             Version
	UnrealBuildToolPath string
}

type buildVersion struct {
	MajorVersion int `json:"MajorVersion"`
	MinorVersion int `json:"MinorVersion"`
}

func discoverEngine(root string) (Engine, error) {
	normalizedRoot, err := normalizePath(root)
	if err != nil {
		return Engine{}, err
	}
	versionPath := filepath.Join(normalizedRoot, "Engine", "Build", "Build.version")
	contents, err := os.ReadFile(versionPath)
	if err != nil {
		return Engine{}, fmt.Errorf("read Build.version: %w", err)
	}
	var parsed buildVersion
	if err := json.Unmarshal(contents, &parsed); err != nil {
		return Engine{}, fmt.Errorf("%w: %v", ErrMalformedEngine, err)
	}
	version := Version{Major: parsed.MajorVersion, Minor: parsed.MinorVersion}
	if version.Major != 5 || version.Minor != 8 {
		return Engine{}, fmt.Errorf("%w: %d.%d", ErrUnsupportedVersion, version.Major, version.Minor)
	}
	ubt := filepath.Join(normalizedRoot, "Engine", "Binaries", "DotNET", "UnrealBuildTool", "UnrealBuildTool.dll")
	info, err := os.Stat(ubt)
	if err != nil || info.IsDir() {
		return Engine{}, fmt.Errorf("%w: %s", ErrUnrealBuildToolNotFound, ubt)
	}
	return Engine{Root: normalizedRoot, Version: version, UnrealBuildToolPath: ubt}, nil
}

func normalizePath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("normalize path %q: %w", path, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	return abs, nil
}
