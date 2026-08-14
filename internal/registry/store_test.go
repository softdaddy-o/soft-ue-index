package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProjectReadyRequiresUsableCompilationDatabase(t *testing.T) {
	cache := t.TempDir()
	db := filepath.Join(cache, "compile_commands.json")
	p := Project{Generation: GenerationState{CacheDir: cache, CompilationDatabase: db}}
	if p.Ready() {
		t.Fatal("missing database reported ready")
	}
	if err := os.WriteFile(db, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if p.Ready() {
		t.Fatal("invalid database reported ready")
	}
	if err := os.WriteFile(db, []byte(`[{"directory":"C:/project","file":"Foo.cpp","command":"clang Foo.cpp"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if !p.Ready() {
		t.Fatal("valid database reported not ready")
	}
	if err := os.WriteFile(db, []byte(`[{"directory":"C:/project","file":"Foo.cpp","command":"clang Foo.cpp"} garbage`), 0600); err != nil {
		t.Fatal(err)
	}
	if p.Ready() {
		t.Fatal("database malformed after opening array reported ready")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := mustStore(t, dir)
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
	got, err := mustStore(t, t.TempDir()).Load(context.Background())
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
	_, err := mustStore(t, dir).Load(context.Background())
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, registryFileName), []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := mustStore(t, dir).Load(context.Background())
	if err == nil {
		t.Fatal("Load succeeded")
	}
}

func TestSaveRejectsDuplicateIDsAndPaths(t *testing.T) {
	dir := t.TempDir()
	s := mustStore(t, dir)
	project := filepath.Join(dir, "Game.uproject")
	for _, projects := range [][]Project{{{ID: "same", UProject: project}, {ID: "same", UProject: filepath.Join(dir, "Other.uproject")}}, {{ID: "one", UProject: project}, {ID: "two", UProject: filepath.Join(dir, ".", "Game.uproject")}}} {
		err := s.Save(context.Background(), Registry{Version: CurrentVersion, Projects: projects})
		if !errors.Is(err, ErrDuplicateProject) {
			t.Fatalf("got %v", err)
		}
	}
}

func TestSaveReturnsLockUnavailableDuringContention(t *testing.T) {
	dir := t.TempDir()
	s := mustStore(t, dir)
	unlock, err := s.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	started := time.Now()
	err = s.Save(context.Background(), Registry{Version: CurrentVersion})
	if !errors.Is(err, ErrLockUnavailable) {
		t.Fatalf("got %v", err)
	}
	if time.Since(started) < lockRetryDelay {
		t.Fatal("Save did not retry lock acquisition")
	}
}

func TestLoadWaitsForAdvisoryLock(t *testing.T) {
	dir := t.TempDir()
	s := mustStore(t, dir)
	if err := s.Save(context.Background(), Registry{Version: CurrentVersion}); err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := s.Load(context.Background()); result <- err }()
	select {
	case err := <-result:
		t.Fatalf("Load returned while lock held: %v", err)
	case <-time.After(lockRetryDelay):
	}
	unlock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestParallelLoadAndSaveKeepRegistryReadable(t *testing.T) {
	dir := t.TempDir()
	store := mustStore(t, dir)
	if err := store.Save(context.Background(), Registry{Version: CurrentVersion}); err != nil {
		t.Fatal(err)
	}

	var workers sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		workers.Add(2)
		go func() {
			defer workers.Done()
			_, err := store.Load(context.Background())
			errs <- err
		}()
		go func() {
			defer workers.Done()
			errs <- store.Save(context.Background(), Registry{Version: CurrentVersion})
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpdatePreservesConcurrentMutationsFromSeparateStores(t *testing.T) {
	dir := t.TempDir()
	first, second := mustStore(t, dir), mustStore(t, dir)
	if err := first.Save(context.Background(), Registry{Version: CurrentVersion}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, pair := range []struct {
		store *Store
		id    string
	}{{first, "a"}, {second, "b"}} {
		wg.Add(1)
		go func(s *Store, id string) {
			defer wg.Done()
			if err := s.Update(context.Background(), func(r *Registry) error {
				r.Projects = append(r.Projects, Project{ID: id, UProject: filepath.Join(dir, id+".uproject")})
				return nil
			}); err != nil {
				t.Error(err)
			}
		}(pair.store, pair.id)
	}
	wg.Wait()
	got, err := first.Load(context.Background())
	if err != nil || len(got.Projects) != 2 {
		t.Fatalf("registry=%#v err=%v", got, err)
	}
}

func TestSaveRejectsPathsDifferingOnlyByCase(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "Game.uproject")
	err := mustStore(t, dir).Save(context.Background(), Registry{Version: CurrentVersion, Projects: []Project{{ID: "one", UProject: project}, {ID: "two", UProject: strings.ToUpper(project)}}})
	if !errors.Is(err, ErrDuplicateProject) {
		t.Fatalf("got %v", err)
	}
}

func TestSaveRejectsPathsWithSameSymlinkIdentity(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "Game.uproject")
	if err := os.WriteFile(project, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "Game-alias.uproject")
	if err := os.Symlink(project, alias); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	err := mustStore(t, dir).Save(context.Background(), Registry{Version: CurrentVersion, Projects: []Project{{ID: "one", UProject: project}, {ID: "two", UProject: alias}}})
	if !errors.Is(err, ErrDuplicateProject) {
		t.Fatalf("got %v", err)
	}
}

func TestSaveRejectsMissingPathsBeneathSameSymlinkedDirectory(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(dir, "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	err := mustStore(t, dir).Save(context.Background(), Registry{Version: CurrentVersion, Projects: []Project{{ID: "one", UProject: filepath.Join(realDir, "Missing.uproject")}, {ID: "two", UProject: filepath.Join(aliasDir, "Missing.uproject")}}})
	if !errors.Is(err, ErrDuplicateProject) {
		t.Fatalf("got %v", err)
	}
}

func TestNewStorePropagatesDefaultRootFailure(t *testing.T) {
	original := defaultRoot
	defaultRoot = func() (string, error) { return "", errors.New("injected default root failure") }
	t.Cleanup(func() { defaultRoot = original })
	_, err := NewStore("")
	if err == nil {
		t.Fatal("NewStore succeeded")
	}
}

func TestFailedSavePreservesPreviousRegistry(t *testing.T) {
	dir := t.TempDir()
	s := mustStore(t, dir)
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

func mustStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
