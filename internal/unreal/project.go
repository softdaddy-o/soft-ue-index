package unreal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrMalformedProject      = errors.New("malformed .uproject")
	ErrEditorTargetNotFound  = errors.New("editor target not found")
	ErrAmbiguousEditorTarget = errors.New("ambiguous editor targets")
)

// ProjectRequest provides the inputs for project discovery. EngineRoot takes
// precedence over EngineAssociation when both are available.
type ProjectRequest struct {
	UProject          string
	EngineRoot        string
	AssociationSource AssociationSource
}

// Project is a project paired with its validated engine and editor target.
type Project struct {
	UProject          string
	EngineAssociation string
	Engine            Engine
	Version           Version
	EditorTarget      string
}

type uproject struct {
	EngineAssociation string `json:"EngineAssociation"`
}

// Discover validates a UE 5.8 project, resolves its engine, and selects its editor target.
func Discover(request ProjectRequest) (Project, error) {
	uprojectPath, err := normalizePath(request.UProject)
	if err != nil {
		return Project{}, err
	}
	if !strings.EqualFold(filepath.Ext(uprojectPath), ".uproject") {
		return Project{}, fmt.Errorf("project path must have a .uproject extension: %s", uprojectPath)
	}
	contents, err := os.ReadFile(uprojectPath)
	if err != nil {
		return Project{}, fmt.Errorf("read .uproject: %w", err)
	}
	var descriptor uproject
	if err := json.Unmarshal(contents, &descriptor); err != nil {
		return Project{}, fmt.Errorf("%w: %v", ErrMalformedProject, err)
	}
	engineRoot := request.EngineRoot
	if engineRoot == "" {
		if descriptor.EngineAssociation == "" || request.AssociationSource == nil {
			return Project{}, fmt.Errorf("%w: %q", ErrAssociationNotFound, descriptor.EngineAssociation)
		}
		var found bool
		engineRoot, found, err = request.AssociationSource.Lookup(descriptor.EngineAssociation)
		if err != nil {
			return Project{}, fmt.Errorf("resolve engine association %q: %w", descriptor.EngineAssociation, err)
		}
		if !found || engineRoot == "" {
			return Project{}, fmt.Errorf("%w: %q", ErrAssociationNotFound, descriptor.EngineAssociation)
		}
	}
	engine, err := discoverEngine(engineRoot)
	if err != nil {
		return Project{}, err
	}
	target, err := discoverEditorTarget(uprojectPath)
	if err != nil {
		return Project{}, err
	}
	return Project{UProject: uprojectPath, EngineAssociation: descriptor.EngineAssociation, Engine: engine, Version: engine.Version, EditorTarget: target}, nil
}

func discoverEditorTarget(uprojectPath string) (string, error) {
	projectName := strings.TrimSuffix(filepath.Base(uprojectPath), filepath.Ext(uprojectPath))
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(uprojectPath), "Source", "*Editor.Target.cs"))
	if err != nil {
		return "", fmt.Errorf("find editor targets: %w", err)
	}
	regularFiles := matches[:0]
	for _, match := range matches {
		info, statErr := os.Stat(match)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return "", fmt.Errorf("inspect editor target %q: %w", match, statErr)
		}
		if info.Mode().IsRegular() {
			regularFiles = append(regularFiles, match)
		}
	}
	matches = regularFiles
	exact := filepath.Join(filepath.Dir(uprojectPath), "Source", projectName+"Editor.Target.cs")
	for _, match := range matches {
		if strings.EqualFold(filepath.Clean(match), filepath.Clean(exact)) {
			return strings.TrimSuffix(filepath.Base(match), ".Target.cs"), nil
		}
	}
	if len(matches) == 0 {
		return "", ErrEditorTargetNotFound
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("%w: %s", ErrAmbiguousEditorTarget, strings.Join(matches, ", "))
	}
	return strings.TrimSuffix(filepath.Base(matches[0]), ".Target.cs"), nil
}
