package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/softdaddy-o/soft-ue-index/internal/cli"
	"github.com/softdaddy-o/soft-ue-index/internal/engineindex"
	"github.com/softdaddy-o/soft-ue-index/internal/registry"
)

// newReadyProject registers project id with a real, on-disk compile_commands.json
// (so p.Ready() is true, unlike newScriptedProject which the LSP query tests use)
// under a throwaway per-user cache directory.
func newReadyProject(t *testing.T, id, dbContent string) registry.Project {
	t.Helper()
	t.Setenv("LocalAppData", t.TempDir())
	cache, err := projectCache(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(cache, "compile_commands.json")
	if err := os.WriteFile(db, []byte(dbContent), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	engineRoot := t.TempDir()
	return registry.Project{
		ID:         id,
		UProject:   filepath.Join(root, "Game.uproject"),
		Engine:     registry.Engine{Root: engineRoot},
		Toolchain:  registry.Toolchain{ClangdPath: "fake-clangd"},
		Generation: registry.GenerationState{CacheDir: cache, CompilationDatabase: db, LastFingerprint: "deadbeef", LastGeneratedAt: time.Now()},
	}
}

func storeWithProject(t *testing.T, p registry.Project) *registry.Store {
	t.Helper()
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(context.Background(), func(r *registry.Registry) error {
		r.Projects = append(r.Projects, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestIndexEngineReportsContentionInsteadOfBlocking guards against an
// earlier version of this lock (registry.AcquireFileLock's unbounded retry
// loop) that blocked a second index-engine invocation, with no output at
// all, for as long as the first one held the lock -- which for a real
// clangd-indexer run against a real engine is measured in hours. A
// contended lock must be reported synchronously, the way `watch` already
// reports "watch already running" rather than hanging.
func TestIndexEngineReportsContentionInsteadOfBlocking(t *testing.T) {
	p := newReadyProject(t, "game", "[]")
	store := storeWithProject(t, p)
	root, err := engineIndexCache(filepath.Join(".locks", engineRootLockKey(p.Engine.Root)))
	if err != nil {
		t.Fatal(err)
	}
	release, acquired, err := registry.TryAcquireFileLock(filepath.Join(root, "index-engine.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("failed to acquire the lock in the test setup itself")
	}
	defer release()

	a := New(Dependencies{Store: store, IndexEngine: func(context.Context, registry.Project) (IndexEngineResult, error) {
		t.Fatal("IndexEngine must not run while the engine-root lock is held elsewhere")
		return IndexEngineResult{}, nil
	}})

	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background(), cli.Command{Name: "index-engine", ProjectName: "game"}) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "already in progress") {
			t.Fatalf("error = %v, want a clear contention message", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("index-engine blocked instead of reporting contention -- regressed to the old unbounded-retry lock")
	}
}

func TestIndexEngineRejectsAProjectThatIsNotYetGenerated(t *testing.T) {
	p := registry.Project{ID: "game"}
	store := storeWithProject(t, p)
	a := New(Dependencies{Store: store, IndexEngine: func(context.Context, registry.Project) (IndexEngineResult, error) {
		t.Fatal("IndexEngine must not run for a project that isn't generated yet")
		return IndexEngineResult{}, nil
	}})
	err := a.Run(context.Background(), cli.Command{Name: "index-engine", ProjectName: "game"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "generate") {
		t.Fatalf("error %q should point the user at generate", err)
	}
}

func TestIndexEngineDispatchesAndPrintsHumanReadableResult(t *testing.T) {
	p := newReadyProject(t, "game", "[]")
	store := storeWithProject(t, p)
	var out bytes.Buffer
	var gotProject registry.Project
	a := New(Dependencies{Store: store, Output: &out, IndexEngine: func(_ context.Context, p registry.Project) (IndexEngineResult, error) {
		gotProject = p
		return IndexEngineResult{EngineEntries: 42, Key: "abc123", IndexPath: `C:\cache\engine.idx`, FragmentPath: `D:\Engine\.clangd`}, nil
	}})
	if err := a.Run(context.Background(), cli.Command{Name: "index-engine", ProjectName: "game"}); err != nil {
		t.Fatal(err)
	}
	if gotProject.ID != "game" {
		t.Fatalf("IndexEngine called with project %q, want %q", gotProject.ID, "game")
	}
	text := out.String()
	if !strings.Contains(text, "42") || !strings.Contains(text, `C:\cache\engine.idx`) || !strings.Contains(text, `D:\Engine\.clangd`) {
		t.Fatalf("human output missing expected fields: %q", text)
	}
}

func TestIndexEngineJSONOutputRoundTrips(t *testing.T) {
	p := newReadyProject(t, "game", "[]")
	store := storeWithProject(t, p)
	var out bytes.Buffer
	a := New(Dependencies{Store: store, Output: &out, IndexEngine: func(context.Context, registry.Project) (IndexEngineResult, error) {
		return IndexEngineResult{EngineEntries: 7, Key: "k", IndexPath: "idx", FragmentPath: "frag"}, nil
	}})
	if err := a.Run(context.Background(), cli.Command{Name: "index-engine", ProjectName: "game", JSON: true}); err != nil {
		t.Fatal(err)
	}
	var result IndexEngineResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v (%q)", err, out.String())
	}
	if result.EngineEntries != 7 || result.Key != "k" {
		t.Fatalf("result=%#v", result)
	}
}

func TestIndexEngineSurfacesHandlerError(t *testing.T) {
	p := newReadyProject(t, "game", "[]")
	store := storeWithProject(t, p)
	a := New(Dependencies{Store: store, IndexEngine: func(context.Context, registry.Project) (IndexEngineResult, error) {
		return IndexEngineResult{}, engineindex.ErrNoEngineEntries
	}})
	err := a.Run(context.Background(), cli.Command{Name: "index-engine", ProjectName: "game"})
	if !errors.Is(err, engineindex.ErrNoEngineEntries) {
		t.Fatalf("error = %v, want ErrNoEngineEntries", err)
	}
}

// TestIndexEngineRealStopsAtErrNoEngineEntriesBeforeTouchingTheFilesystem
// exercises indexEngineReal's actual wiring (Split -> the early-exit guard)
// with no fake runner involved -- a database with no engine-scoped entries
// must fail before any staging directory, cache directory, or fragment file
// is created.
func TestIndexEngineRealStopsAtErrNoEngineEntriesBeforeTouchingTheFilesystem(t *testing.T) {
	p := newReadyProject(t, "game", "[]")
	a := New(Dependencies{})
	_, err := a.indexEngineReal(context.Background(), p)
	if !errors.Is(err, engineindex.ErrNoEngineEntries) {
		t.Fatalf("error = %v, want ErrNoEngineEntries", err)
	}
	if _, statErr := os.Stat(engineindex.FragmentPath(p.Engine.Root)); !os.IsNotExist(statErr) {
		t.Fatalf("no fragment should have been written, stat err=%v", statErr)
	}
}

// TestIndexEngineRealReachesFindIndexerWithCorrectlySplitEntries exercises
// indexEngineReal's full wiring up through locating clangd-indexer -- the
// one real external dependency this repo's automated tests cannot invoke
// (clangd-indexer is not installed in CI; it was verified manually against
// a real 15-TU fixture this session, see the PR description). A database
// with engine entries but a Toolchain.ClangdPath that has no clangd-indexer
// next to it, and nothing on PATH, must fail specifically at FindIndexer --
// proving Split/Key/staging all ran correctly to get there, and that
// nothing partial (fragment, cache dir contents) leaked out on that
// failure.
func TestIndexEngineRealReachesFindIndexerWithCorrectlySplitEntries(t *testing.T) {
	engineRoot := t.TempDir()
	projectRoot := t.TempDir()
	entries := `[
		{"directory":"` + jsonEscape(engineRoot) + `","file":"` + jsonEscape(filepath.Join(engineRoot, "Engine", "Source", "Core.cpp")) + `","arguments":["clang-cl","Core.cpp"]},
		{"directory":"` + jsonEscape(projectRoot) + `","file":"` + jsonEscape(filepath.Join(projectRoot, "Game.cpp")) + `","arguments":["clang-cl","Game.cpp"]}
	]`
	p := newReadyProject(t, "game", entries)
	p.Engine.Root = engineRoot
	p.UProject = filepath.Join(projectRoot, "Game.uproject")
	// Deterministic regardless of what's actually installed on the host
	// running this test: FindIndexer's PATH fallback must not accidentally
	// find a real clangd-indexer and let this test's bogus fixture (files
	// that don't exist, a "clang-cl" compiler argument that isn't a real
	// path) run through it for real.
	t.Setenv("PATH", t.TempDir())

	a := New(Dependencies{})
	_, err := a.indexEngineReal(context.Background(), p)
	if err == nil {
		t.Fatal("expected an error: no real clangd-indexer is available in this test environment")
	}
	if !strings.Contains(err.Error(), "clangd-indexer") {
		t.Fatalf("expected the error to name the missing clangd-indexer binary, got: %v", err)
	}
	if _, statErr := os.Stat(engineindex.FragmentPath(engineRoot)); !os.IsNotExist(statErr) {
		t.Fatalf("no fragment should have been written when FindIndexer fails, stat err=%v", statErr)
	}
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}
