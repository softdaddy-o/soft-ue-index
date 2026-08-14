// Package testutil provides synthetic Unreal project trees for tests.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// FakeUE58 identifies a synthetic UE 5.8 engine and project tree.
type FakeUE58 struct {
	Root       string
	UProject   string
	EngineRoot string
}

// NewFakeUE58 creates the smallest project and engine tree discovery accepts.
func NewFakeUE58(t *testing.T) FakeUE58 {
	t.Helper()
	root := t.TempDir()
	engineRoot := filepath.Join(root, "EngineRoot")
	uproject := filepath.Join(root, "Game", "Game.uproject")
	writeFile(t, uproject, `{"EngineAssociation":"UE_5.8"}`)
	writeFile(t, filepath.Join(root, "Game", "Source", "GameEditor.Target.cs"), "// synthetic target\n")
	writeFile(t, filepath.Join(engineRoot, "Engine", "Build", "Build.version"), `{"MajorVersion":5,"MinorVersion":8}`)
	writeFile(t, filepath.Join(engineRoot, "Engine", "Binaries", "DotNET", "UnrealBuildTool", "UnrealBuildTool.dll"), "synthetic")
	return FakeUE58{Root: root, UProject: uproject, EngineRoot: engineRoot}
}

// WriteFile writes a synthetic fixture file, creating its parent directory.
func WriteFile(t *testing.T, path, contents string) {
	t.Helper()
	writeFile(t, path, contents)
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
