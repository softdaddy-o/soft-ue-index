package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	want := Registry{Version: CurrentVersion, Projects: []Project{{
		ID: "game-a", Name: "GameA", UProject: filepath.Join(dir, "GameA", "GameA.uproject"),
		Engine:     Engine{Root: filepath.Join(dir, "Engine")},
		Toolchain:  Toolchain{ClangdPath: filepath.Join(dir, "clangd.exe"), ClangdVersion: "20.1.8"},
		Generation: GenerationState{CompilationDatabase: filepath.Join(dir, "cache", "compile_commands.json"), CacheDir: filepath.Join(dir, "cache"), LastFingerprint: "abc", LastGeneratedAt: time.Date(2026, 8, 14, 1, 2, 3, 0, time.UTC), InvalidationReason: "build-file-changed"},
	}}}
	if err := s.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("got %#v, want %#v", got, want)
	}

	contents, err := os.ReadFile(filepath.Join(dir, registryFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "\n  \"version\": 1") {
		t.Fatalf("registry was not indented: %q", contents)
	}
}

func TestLoadMissingFileReturnsEmptyV1Registry(t *testing.T) {
	got, err := NewStore(t.TempDir()).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Registry{Version: CurrentVersion, Projects: []Project{}}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestLoadRejectsUnsupportedSchema(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, registryFileName), []byte(`{"version": 99, "projects": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewStore(dir).Load(context.Background())
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, registryFileName), []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewStore(dir).Load(context.Background())
	if err == nil {
		t.Fatal("Load succeeded")
	}
}

func TestSaveRejectsDuplicateIDsAndPaths(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	project := filepath.Join(dir, "Game.uproject")
	for _, projects := range [][]Project{{{ID: "same", UProject: project}, {ID: "same", UProject: filepath.Join(dir, "Other.uproject")}}, {{ID: "one", UProject: project}, {ID: "two", UProject: filepath.Join(dir, ".", "Game.uproject")}}} {
		err := s.Save(context.Background(), Registry{Version: CurrentVersion, Projects: projects})
		if !errors.Is(err, ErrDuplicateProject) {
			t.Fatalf("got %v", err)
		}
	}
}

func TestSaveWaitsForLockContention(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := os.WriteFile(filepath.Join(dir, lockFileName), []byte("busy"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := s.Save(context.Background(), Registry{Version: CurrentVersion})
	if !errors.Is(err, ErrLockUnavailable) {
		t.Fatalf("got %v", err)
	}
	if time.Since(started) < lockRetryDelay {
		t.Fatal("Save did not retry lock acquisition")
	}
}

func TestFailedSavePreservesPreviousRegistry(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	previous := Registry{Version: CurrentVersion, Projects: []Project{{ID: "old", UProject: filepath.Join(dir, "Old.uproject")}}}
	if err := s.Save(context.Background(), previous); err != nil {
		t.Fatal(err)
	}
	s.promote = func(_, _ string) error { return errors.New("injected promotion failure") }
	err := s.Save(context.Background(), Registry{Version: CurrentVersion, Projects: []Project{{ID: "new", UProject: filepath.Join(dir, "New.uproject")}}})
	if err == nil {
		t.Fatal("Save succeeded")
	}
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(previous, got) {
		t.Fatalf("got %#v, want %#v", got, previous)
	}
}
