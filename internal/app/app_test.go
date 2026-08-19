package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/softdaddy-o/soft-ue-index/internal/cli"
	"github.com/softdaddy-o/soft-ue-index/internal/compdb"
	"github.com/softdaddy-o/soft-ue-index/internal/diagnostics"
	"github.com/softdaddy-o/soft-ue-index/internal/lsp"
	"github.com/softdaddy-o/soft-ue-index/internal/mcpserver"
	"github.com/softdaddy-o/soft-ue-index/internal/registry"
	"github.com/softdaddy-o/soft-ue-index/internal/testutil"
	"github.com/softdaddy-o/soft-ue-index/internal/unreal"
)

func TestListRendersEmptyRegistryAsJSON(t *testing.T) {
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := New(Dependencies{Store: store, Output: &out})
	if err := a.Run(context.Background(), cli.Command{Name: "list", JSON: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "[]" {
		t.Fatalf("got %q", got)
	}
}

func TestAddGenerateListAndStatusUseOneRegisteredProject(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := New(Dependencies{Store: store, Output: &out, Discover: func(r unreal.ProjectRequest) (unreal.Project, error) {
		r.AssociationSource = unreal.MapAssociationSource{"UE_5.8": env.EngineRoot}
		return unreal.Discover(r)
	}, Generate: func(_ context.Context, p registry.Project) (registry.Project, error) {
		p.Generation.CompilationDatabase = "ready/compile_commands.json"
		return p, nil
	}})
	if err := a.Run(context.Background(), cli.Command{Name: "add", ProjectPath: env.UProject}); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background(), cli.Command{Name: "generate", ProjectName: "game"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.Run(context.Background(), cli.Command{Name: "status", ProjectName: "game", JSON: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"compilationDatabase":`) || !strings.Contains(out.String(), "compile_commands.json") {
		t.Fatalf("status %q", out.String())
	}
}

func TestRemoveUnknownProjectFails(t *testing.T) {
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := New(Dependencies{Store: store})
	if err := a.Run(context.Background(), cli.Command{Name: "remove", ProjectName: "missing"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestStableIDAvoidsPortableCaseCollision(t *testing.T) {
	if got := stableID("Game", []registry.Project{{ID: "GAME"}}); got != "game-2" {
		t.Fatalf("stableID=%q", got)
	}
}

func TestWatchRejectsSecondOwner(t *testing.T) {
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(t.TempDir(), "watch.lock")
	started := make(chan struct{})
	releaseWatch := make(chan struct{})
	first := New(Dependencies{Store: store, WatchLockPath: lockPath, Watch: func(context.Context, []registry.Project) error {
		close(started)
		<-releaseWatch
		return nil
	}})
	done := make(chan error, 1)
	go func() { done <- first.Run(context.Background(), cli.Command{Name: "watch"}) }()
	<-started

	second := New(Dependencies{Store: store, WatchLockPath: lockPath, Watch: func(context.Context, []registry.Project) error {
		t.Fatal("second watcher started")
		return nil
	}})
	if err := second.Run(context.Background(), cli.Command{Name: "watch"}); err == nil || err.Error() != "watch already running" {
		t.Fatalf("second watch error=%v", err)
	}
	close(releaseWatch)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDoctorDefaultReturnsStructuredChecks(t *testing.T) {
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := New(Dependencies{Store: store, Output: &out})
	if err := a.Run(context.Background(), cli.Command{Name: "doctor", JSON: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"checks"`) || !strings.Contains(out.String(), `"engine"`) {
		t.Fatalf("doctor %q", out.String())
	}
}

func TestDoctorDoesNotLetHealthyProjectMaskBrokenProject(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brokenRoot := filepath.Join(t.TempDir(), "broken-engine")
	projects := []registry.Project{
		{ID: "healthy", Name: "Healthy", UProject: env.UProject, Engine: registry.Engine{Root: env.EngineRoot}},
		{ID: "broken", Name: "Broken", UProject: filepath.Join(t.TempDir(), "Broken.uproject"), Engine: registry.Engine{Root: brokenRoot}},
	}
	if err := store.Save(context.Background(), registry.Registry{Version: registry.CurrentVersion, Projects: projects}); err != nil {
		t.Fatal(err)
	}
	reportAny, err := New(Dependencies{Store: store}).doctorReal(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	report := reportAny.(diagnostics.Report)
	engine := report.Checks[0]
	if engine.Status != diagnostics.Fail || !strings.Contains(engine.Detail, "broken (Broken)") || strings.Contains(engine.Detail, brokenRoot) {
		t.Fatalf("engine check=%+v", engine)
	}
}

func TestDoctorDiscoversCompatibleClangdBeforeGenerationAndRejectsStaleRegistryPath(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	testutil.WriteFile(t, filepath.Join(env.EngineRoot, "Engine", "Config", "Windows", "Windows_SDK.json"), `{"PreferredClangVersions":["20.1.0-20.999"],"MinimumClangVersion":"20.1.0"}`)
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := registry.Project{ID: "game", Name: "Game", UProject: env.UProject, Engine: registry.Engine{Root: env.EngineRoot}, Toolchain: registry.Toolchain{ClangdPath: "stale-clangd.exe"}}
	if err := store.Save(context.Background(), registry.Registry{Version: registry.CurrentVersion, Projects: []registry.Project{project}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := New(Dependencies{
		Store:       store,
		Output:      &out,
		Environment: appEnvironment{"LLVM_PATH": "C:/llvm"},
		Files:       appFiles{files: map[string]bool{"C:/llvm/clangd.exe": true}},
		Runner:      appRunner{outputs: map[string]string{"C:/llvm/clangd.exe": "clangd version 20.1.8"}},
	})
	if err := a.Run(context.Background(), cli.Command{Name: "doctor", JSON: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"code":"llvm","summary":"LLVM / clangd","detail":"A compatible clangd was found."`) {
		t.Fatalf("doctor did not execute discovered clangd: %s", out.String())
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Projects[0].Toolchain.ClangdPath != filepath.FromSlash("C:/llvm/clangd.exe") {
		t.Fatalf("stale clangd was not replaced: %#v", got.Projects[0].Toolchain)
	}
}

func TestDoctorFindsNestedUnrealBuildArtifacts(t *testing.T) {
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, "Intermediate", "Build", "Win64", "Game", "Inc", "Nested.generated.h"), "// generated\n")
	testutil.WriteFile(t, filepath.Join(root, "Intermediate", "Build", "Win64", "Game", "Compile.rsp"), "// response\n")
	if !hasBuildArtifact(root, ".generated.h") || !hasBuildArtifact(root, ".rsp") {
		t.Fatal("nested Unreal build artifacts were not found")
	}
	if hasBuildArtifact(t.TempDir(), ".rsp") {
		t.Fatal("missing build artifacts reported as present")
	}
}

func TestFileURIEscapesWindowsAndUnicodePaths(t *testing.T) {
	if got, want := fileURI(`C:\Work Dir\a#한.cpp`), "file:///C:/Work%20Dir/a%23%ED%95%9C.cpp"; got != want {
		t.Fatalf("Windows URI = %q, want %q", got, want)
	}
	if got, want := fileURI(`/tmp/space # 한.cpp`), "file:///tmp/space%20%23%20%ED%95%9C.cpp"; got != want {
		t.Fatalf("POSIX URI = %q, want %q", got, want)
	}
}

func TestLSPQueryResultsCannotEscapeProjectAndEngineRoots(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "Project")
	engineRoot := filepath.Join(root, "Engine")
	outsideRoot := filepath.Join(root, "Outside")
	for _, dir := range []string{projectRoot, engineRoot, outsideRoot, filepath.Join(projectRoot, "Intermediate", "Generated")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	projectFile := filepath.Join(projectRoot, "Source.cpp")
	engineFile := filepath.Join(engineRoot, "Core.cpp")
	generatedFile := filepath.Join(projectRoot, "Intermediate", "Generated", "Thing.gen.cpp")
	outsideFile := filepath.Join(outsideRoot, "secret.cpp")
	for _, path := range []string{projectFile, engineFile, generatedFile, outsideFile} {
		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	p := registry.Project{UProject: filepath.Join(projectRoot, "Game.uproject"), Engine: registry.Engine{Root: engineRoot}}
	allowed := []string{fileURI(projectFile), fileURI(engineFile), fileURI(generatedFile)}
	for _, uri := range allowed {
		if !lspResultURIAllowed(p, uri) {
			t.Fatalf("allowed URI rejected: %s", uri)
		}
	}
	for _, uri := range []string{fileURI(outsideFile), "https://example.invalid/source.cpp", "not a URI", "file://server/share/source.cpp", ""} {
		if lspResultURIAllowed(p, uri) {
			t.Fatalf("forbidden URI accepted: %q", uri)
		}
	}

	symbols := filterLSPSymbols(p, []lsp.Symbol{{Name: "project", Location: lsp.Location{URI: allowed[0]}}, {Name: "outside", Location: lsp.Location{URI: fileURI(outsideFile)}}, {Name: "malformed", Location: lsp.Location{URI: "not a URI"}}})
	if len(symbols) != 1 || symbols[0].Name != "project" {
		t.Fatalf("symbols leaked: %#v", symbols)
	}
	locations := filterLSPLocations(p, []lsp.Location{{URI: allowed[0]}, {URI: allowed[1]}, {URI: fileURI(outsideFile)}, {URI: "https://example.invalid/source.cpp"}})
	if len(locations) != 2 {
		t.Fatalf("definition/reference/implementation locations leaked: %#v", locations)
	}
	items := filterLSPCallItems(p, []lsp.CallHierarchyItem{{Name: "generated", URI: allowed[2]}, {Name: "outside", URI: fileURI(outsideFile)}})
	if len(items) != 1 || items[0].Name != "generated" {
		t.Fatalf("call hierarchy items leaked: %#v", items)
	}
	insideCall := lsp.CallHierarchyCall{From: &lsp.CallHierarchyItem{Name: "engine", URI: allowed[1]}, To: &lsp.CallHierarchyItem{Name: "project", URI: allowed[0]}}
	escapeCall := lsp.CallHierarchyCall{From: &lsp.CallHierarchyItem{Name: "outside", URI: fileURI(outsideFile)}, To: &lsp.CallHierarchyItem{Name: "project", URI: allowed[0]}}
	calls := filterLSPCalls(p, []lsp.CallHierarchyCall{insideCall, escapeCall})
	if len(calls) != 1 || calls[0].From.Name != "engine" {
		t.Fatalf("call hierarchy calls leaked: %#v", calls)
	}
}

type countingLSPFactory struct{ starts atomic.Int32 }

func (f *countingLSPFactory) Start(context.Context, string, []string, string) (lsp.Process, error) {
	f.starts.Add(1)
	return nil, errors.New("unexpected clangd start")
}

func TestLSPQueriesRejectCorruptRegistryCacheBeforeFilesystemEffects(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "Project")
	engineRoot := filepath.Join(root, "Engine")
	for _, dir := range []string{projectRoot, engineRoot} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	p := registry.Project{ID: "game", UProject: filepath.Join(projectRoot, "Game.uproject"), Engine: registry.Engine{Root: engineRoot}, Toolchain: registry.Toolchain{ClangdPath: "clangd.exe"}}
	for _, cache := range []string{projectRoot, engineRoot} {
		t.Run(filepath.Base(cache), func(t *testing.T) {
			db := filepath.Join(cache, compdb.DatabaseName)
			if err := os.WriteFile(db, []byte("[]"), 0644); err != nil {
				t.Fatal(err)
			}
			candidate := p
			candidate.Generation = registry.GenerationState{CacheDir: cache, CompilationDatabase: db}
			factory := &countingLSPFactory{}
			manager := lsp.NewManager(factory)
			defer manager.Close()
			if _, _, err := (lspQueries{manager: manager}).client(context.Background(), candidate); err == nil {
				t.Fatal("corrupt registry cache was accepted")
			}
			if got := factory.starts.Load(); got != 0 {
				t.Fatalf("clangd process starts=%d", got)
			}
			if _, err := os.Stat(filepath.Join(cache, ".cache")); !os.IsNotExist(err) {
				t.Fatalf("manager wrote inside registry-controlled cache: %v", err)
			}
		})
	}
}

func TestValidateProjectCacheRejectsSourceDirectoryWithoutWriting(t *testing.T) {
	source := t.TempDir()
	p := registry.Project{ID: fmt.Sprintf("malicious-%d", time.Now().UnixNano()), Generation: registry.GenerationState{CacheDir: source, CompilationDatabase: filepath.Join(source, "compile_commands.json")}}
	if err := validateProjectCache(p); err == nil {
		t.Fatal("malicious cache was accepted")
	}
	if _, err := os.Stat(filepath.Join(source, ".cache")); !os.IsNotExist(err) {
		t.Fatalf("validation wrote into source: %v", err)
	}
}

type appEnvironment map[string]string

func (e appEnvironment) LookupEnv(name string) (string, bool) { value, ok := e[name]; return value, ok }

type appFiles struct{ files map[string]bool }

func (f appFiles) Exists(path string) bool { return f.files[path] }
func (f appFiles) Glob(pattern string) []string {
	paths := []string{}
	for path := range f.files {
		if matched, _ := filepath.Match(pattern, path); matched {
			paths = append(paths, path)
		}
	}
	return paths
}

type appRunner struct{ outputs map[string]string }

type recordingSourceChanges struct{ changes chan string }

func (r recordingSourceChanges) SourceFileChanged(id, path string) error {
	r.changes <- id + ":" + path
	return nil
}
func (r recordingSourceChanges) SourceFileCreated(id, path string) error {
	r.changes <- id + ":created:" + path
	return nil
}
func (r recordingSourceChanges) SourceFileRemoved(id, path string) error {
	r.changes <- id + ":removed:" + path
	return nil
}

func (r appRunner) Run(name string, _ ...string) (string, error) {
	if output, ok := r.outputs[name]; ok {
		return output, nil
	}
	return "", fmt.Errorf("not executable: %s", name)
}

// TestEndToEndTwoProjectsShareEngine exercises the real application routing
// around injectable external effects. The filesystem watcher is deliberately
// real: a Build.cs write must regenerate only its owning project.
func TestEndToEndTwoProjectsShareEngine(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	second := filepath.Join(env.Root, "Other", "Other.uproject")
	testutil.WriteFile(t, second, `{"EngineAssociation":"UE_5.8"}`)
	testutil.WriteFile(t, filepath.Join(filepath.Dir(second), "Source", "OtherEditor.Target.cs"), "// target\n")
	buildA := filepath.Join(filepath.Dir(env.UProject), "Source", "Game.Build.cs")
	buildB := filepath.Join(filepath.Dir(second), "Source", "Other.Build.cs")
	testutil.WriteFile(t, buildA, "// initial\n")
	testutil.WriteFile(t, buildB, "// initial\n")
	projectSymbol := filepath.Join(filepath.Dir(env.UProject), "Source", "Game.cpp")
	engineSymbol := filepath.Join(env.EngineRoot, "Engine", "Source", "Runtime", "Core", "Private", "Shared.cpp")
	testutil.WriteFile(t, projectSymbol, "void ProjectSymbol() {}\n")
	testutil.WriteFile(t, engineSymbol, "void EngineSymbol() {}\n")

	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	generated := make(chan string, 8)
	a := New(Dependencies{
		Store:  store,
		Output: &out,
		Discover: func(r unreal.ProjectRequest) (unreal.Project, error) {
			r.AssociationSource = unreal.MapAssociationSource{"UE_5.8": env.EngineRoot}
			return unreal.Discover(r)
		},
		Generate: func(_ context.Context, p registry.Project) (registry.Project, error) {
			generated <- p.ID
			p.Generation.CompilationDatabase = filepath.Join(filepath.Dir(p.UProject), "cache", "compile_commands.json")
			p.Generation.LastGeneratedAt = time.Now().UTC()
			return p, nil
		},
	})
	ctx := context.Background()
	for _, path := range []string{env.UProject, second} {
		if err := a.Run(ctx, cli.Command{Name: "add", ProjectPath: path, JSON: true}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"game", "other"} {
		if err := a.Run(ctx, cli.Command{Name: "generate", ProjectName: id, JSON: true}); err != nil {
			t.Fatal(err)
		}
		mustGenerate(t, generated, id)
	}
	out.Reset()
	if err := a.Run(ctx, cli.Command{Name: "list", JSON: true}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, `"id":"game"`) || !strings.Contains(got, `"id":"other"`) {
		t.Fatalf("list JSON = %q", got)
	}
	out.Reset()
	if err := a.Run(ctx, cli.Command{Name: "list"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "game\tGame") || !strings.Contains(got, "other\tOther") {
		t.Fatalf("list human = %q", got)
	}
	for _, id := range []string{"game", "other"} {
		out.Reset()
		if err := a.Run(ctx, cli.Command{Name: "status", ProjectName: id, JSON: true}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "compile_commands.json") {
			t.Fatalf("status %s = %q", id, out.String())
		}
	}

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	watchDone := make(chan error, 1)
	registryBefore, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sourceChanges := recordingSourceChanges{changes: make(chan string, 1)}
	go func() { watchDone <- a.watchRealWithSink(watchCtx, registryBefore.Projects, sourceChanges) }()
	// Give fsnotify time to attach its recursive source watches before mutating.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(buildA, []byte("// changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGenerate(t, generated, "game")
	if err := os.WriteFile(projectSymbol, []byte("void UpdatedProjectSymbol() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case change := <-sourceChanges.changes:
		if change != "game:"+projectSymbol {
			t.Fatalf("source change=%q", change)
		}
	case <-time.After(time.Second):
		t.Fatal("source write was not forwarded to the live index")
	}
	select {
	case got := <-generated:
		t.Fatalf("unexpected generation after one project mutation: %s", got)
	case <-time.After(700 * time.Millisecond):
	}
	cancelWatch()
	if err := <-watchDone; err != nil {
		t.Fatal(err)
	}
	registryAfter, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if registryAfter.Projects[0].Generation.LastGeneratedAt.Equal(registryBefore.Projects[0].Generation.LastGeneratedAt) {
		t.Fatal("mutated project status was not updated")
	}
	if !registryAfter.Projects[1].Generation.LastGeneratedAt.Equal(registryBefore.Projects[1].Generation.LastGeneratedAt) {
		t.Fatal("unmutated project status changed")
	}

	queries := &e2eQueries{projectURI: fileURI(projectSymbol), engineURI: fileURI(engineSymbol)}
	server := mcpserver.New(mcpserver.Dependencies{Projects: store, Queries: queries}).MCPServer("test")
	left, right := mcp.NewInMemoryTransports()
	sessionCtx, cancelSession := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelSession()
	serverSession, err := server.Connect(sessionCtx, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	clientSession, err := client.Connect(sessionCtx, right, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	for _, query := range []string{"project", "engine"} {
		result, err := clientSession.CallTool(sessionCtx, &mcp.CallToolParams{Name: "search_symbols", Arguments: map[string]any{"project_id": "game", "query": query}})
		if err != nil || result.IsError {
			t.Fatalf("MCP %s: result=%#v err=%v", query, result, err)
		}
		if !strings.Contains(fmt.Sprint(result), "Symbol") {
			t.Fatalf("MCP %s returned no symbol: %#v", query, result)
		}
	}
	if got, want := strings.Join(queries.seen, ","), "game:project,game:engine"; got != want {
		t.Fatalf("unexpected MCP project routing: got %q, want %q", got, want)
	}
}

func TestWatchRealKeepsHealthyProjectWhenAnotherCannotBeWatched(t *testing.T) {
	root := t.TempDir()
	healthy := filepath.Join(root, "healthy")
	for _, path := range []string{filepath.Join(healthy, "Source"), filepath.Join(healthy, "Engine", "Source")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var stderr bytes.Buffer
	a := New(Dependencies{ErrorOutput: &stderr, Generate: func(context.Context, registry.Project) (registry.Project, error) {
		t.Fatal("unexpected generation")
		return registry.Project{}, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.watchReal(ctx, []registry.Project{
			{ID: "bad", UProject: filepath.Join(root, "missing", "Bad.uproject"), Engine: registry.Engine{Root: filepath.Join(root, "missing-engine")}},
			{ID: "good", UProject: filepath.Join(healthy, "Good.uproject"), Engine: registry.Engine{Root: healthy}},
		})
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("healthy watcher was aborted: %v", err)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "watch project bad") {
		t.Fatalf("missing isolated error: %q", stderr.String())
	}
}

func TestProjectGenerationLockSerializesAppInstances(t *testing.T) {
	id := fmt.Sprintf("lock-test-%d", time.Now().UnixNano())
	a, b := New(Dependencies{}), New(Dependencies{})
	entered, releaseFirst := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- a.withProjectGenerationLock(context.Background(), id, func() error { close(entered); <-releaseFirst; return nil })
	}()
	<-entered
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- b.withProjectGenerationLock(context.Background(), id, func() error { close(secondEntered); return nil })
	}()
	select {
	case <-secondEntered:
		t.Fatal("second app bypassed project lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("second app did not acquire project lock")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestExecRunnerBoundsOutputAndTimeout(t *testing.T) {
	if os.Getenv("SOFT_UE_INDEX_PROBE_HELPER") == "1" {
		if len(os.Args) > 0 && strings.Contains(strings.Join(os.Args, " "), "noisy") {
			fmt.Print(strings.Repeat("x", 4096))
			return
		}
		time.Sleep(5 * time.Second)
		return
	}
	t.Setenv("SOFT_UE_INDEX_PROBE_HELPER", "1")
	runner := execRunner{Timeout: 2 * time.Second, MaxCapture: 128}
	out, err := runner.Run(os.Args[0], "-test.run=TestExecRunnerBoundsOutputAndTimeout", "--", "noisy")
	if err != nil || len(out) != 128 {
		t.Fatalf("noisy output len=%d err=%v", len(out), err)
	}
	runner.Timeout = 100 * time.Millisecond
	_, err = runner.Run(os.Args[0], "-test.run=TestExecRunnerBoundsOutputAndTimeout", "--", "hung")
	if err == nil || err.Error() != "toolchain probe timed out" {
		t.Fatalf("timeout error=%v", err)
	}
}

func mustGenerate(t *testing.T, generated <-chan string, want string) {
	t.Helper()
	select {
	case got := <-generated:
		if got != want {
			t.Fatalf("generated %q, want %q", got, want)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("timed out waiting for %q generation", want)
	}
}

type e2eQueries struct {
	projectURI, engineURI string
	seen                  []string
}

func (q *e2eQueries) Symbols(_ context.Context, p registry.Project, query string, _ int) ([]lsp.Symbol, error) {
	q.seen = append(q.seen, p.ID+":"+query)
	if query == "engine" {
		return []lsp.Symbol{{Name: "EngineSymbol", Location: lsp.Location{URI: q.engineURI}}}, nil
	}
	return []lsp.Symbol{{Name: "ProjectSymbol", Location: lsp.Location{URI: q.projectURI}}}, nil
}
func (*e2eQueries) Locations(context.Context, registry.Project, string, mcpserver.TextPosition, int) ([]lsp.Location, error) {
	return nil, nil
}
func (*e2eQueries) DocumentSymbols(context.Context, registry.Project, string, int) ([]lsp.DocumentSymbol, error) {
	return nil, nil
}
func (*e2eQueries) Hover(context.Context, registry.Project, mcpserver.TextPosition) (*lsp.HoverResult, error) {
	return nil, nil
}
func (*e2eQueries) PrepareCallHierarchy(context.Context, registry.Project, mcpserver.TextPosition) ([]lsp.CallHierarchyItem, error) {
	return nil, nil
}
func (*e2eQueries) IncomingCalls(context.Context, registry.Project, lsp.CallHierarchyItem) ([]lsp.CallHierarchyCall, error) {
	return nil, nil
}
func (*e2eQueries) OutgoingCalls(context.Context, registry.Project, lsp.CallHierarchyItem) ([]lsp.CallHierarchyCall, error) {
	return nil, nil
}
func (*e2eQueries) IndexState(context.Context, registry.Project) (mcpserver.IndexState, error) {
	return mcpserver.IndexState{}, nil
}

// --- Regression coverage: Symbols() must not wait for background-index
// completion. scriptedProcess/scriptedFactory drive a real *lsp.Manager
// through a hand-scripted clangd conversation over a net.Pipe, mirroring
// the pattern used in internal/lsp's own manager tests (those helpers are
// unexported there, so the minimal framing is reproduced here).

type scriptedProcess struct {
	conn net.Conn
	done chan struct{}
	once sync.Once
}

func (p *scriptedProcess) Stdin() io.WriteCloser { return p.conn }
func (p *scriptedProcess) Stdout() io.ReadCloser { return p.conn }
func (p *scriptedProcess) Wait() error           { <-p.done; return nil }
func (p *scriptedProcess) Kill() error {
	p.once.Do(func() { close(p.done); _ = p.conn.Close() })
	return nil
}

type scriptedFactory struct {
	mu   sync.Mutex
	peer net.Conn
}

func (f *scriptedFactory) Start(context.Context, string, []string, string) (lsp.Process, error) {
	a, b := net.Pipe()
	f.mu.Lock()
	f.peer = b
	f.mu.Unlock()
	return &scriptedProcess{conn: a, done: make(chan struct{})}, nil
}

// waitForPeer, writeLSPFrame, and readLSPFrame return errors rather than
// taking a *testing.T and calling Fatal directly: they are driven from a
// background goroutine in TestSymbolsDoesNotWaitForBackgroundIndexCompletion,
// and t.Fatal/FailNow is only safe to call from the goroutine running the
// test itself.
func (f *scriptedFactory) waitForPeer(timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		peer := f.peer
		f.mu.Unlock()
		if peer != nil {
			return peer, nil
		}
		time.Sleep(time.Millisecond)
	}
	return nil, errors.New("factory never started a process")
}

func writeLSPFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return fmt.Errorf("write frame header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("write frame body: %w", err)
	}
	return nil
}

func readLSPFrame(r *bufio.Reader) (map[string]any, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read frame header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if rest, ok := strings.CutPrefix(strings.ToLower(line), "content-length:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return nil, fmt.Errorf("bad content-length %q: %w", line, err)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("frame had no content-length header")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("read frame body: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("unmarshal frame: %w", err)
	}
	return m, nil
}

// newScriptedProject registers project id under the real per-user cache
// directory (matching validateProjectCache) so lspQueries.client accepts
// it, and cleans up afterward. It intentionally does not reuse a real
// project's ID (e.g. any locally registered "elpis") so it cannot collide
// with actual daemon state on this machine.
func newScriptedProject(t *testing.T, id string) registry.Project {
	t.Helper()
	cache, err := projectCache(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	db := filepath.Join(cache, compdb.DatabaseName)
	if err := os.WriteFile(db, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	return registry.Project{
		ID:         id,
		UProject:   filepath.Join(root, "Game.uproject"),
		Toolchain:  registry.Toolchain{ClangdPath: "fake"},
		Generation: registry.GenerationState{CacheDir: cache, CompilationDatabase: db},
	}
}

func TestSymbolsDoesNotWaitForBackgroundIndexCompletion(t *testing.T) {
	f := &scriptedFactory{}
	manager := lsp.NewManager(f)
	defer manager.Close()
	p := newScriptedProject(t, fmt.Sprintf("readiness-regression-%d", time.Now().UnixNano()))

	type symbolsResult struct {
		items []lsp.Symbol
		err   error
	}
	symbolsDone := make(chan symbolsResult, 1)
	go func() {
		items, err := (lspQueries{manager: manager}).Symbols(context.Background(), p, "Foo", 10)
		symbolsDone <- symbolsResult{items, err}
	}()

	// Drives the fake clangd side of the conversation on its own goroutine,
	// bounded by a read deadline on the pipe, so a regression (Symbols()
	// blocking instead of issuing workspace/symbol) fails this test cleanly
	// instead of hanging it -- as it did when manually verified against the
	// pre-fix behavior, where workspace/symbol was never sent at all.
	driverDone := make(chan error, 1)
	go func() {
		peer, err := f.waitForPeer(5 * time.Second)
		if err != nil {
			driverDone <- err
			return
		}
		_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(peer)

		initReq, err := readLSPFrame(r)
		if err != nil {
			driverDone <- fmt.Errorf("read initialize: %w", err)
			return
		}
		if initReq["method"] != "initialize" {
			driverDone <- fmt.Errorf("first message = %v, want initialize", initReq)
			return
		}
		if err := writeLSPFrame(peer, map[string]any{"jsonrpc": "2.0", "id": initReq["id"], "result": map[string]any{}}); err != nil {
			driverDone <- err
			return
		}
		initializedNotif, err := readLSPFrame(r)
		if err != nil {
			driverDone <- fmt.Errorf("read initialized: %w", err)
			return
		}
		if initializedNotif["method"] != "initialized" {
			driverDone <- fmt.Errorf("second message = %v, want initialized", initializedNotif)
			return
		}

		// Start (and never finish) a background-index progress cycle. Under
		// the old design (WaitForIndexReady gating Symbols), this alone
		// would have blocked the call above until its context expired --
		// and Symbols() here uses context.Background(), which never does.
		if err := writeLSPFrame(peer, map[string]any{
			"jsonrpc": "2.0", "method": "$/progress",
			"params": map[string]any{"token": "x", "value": map[string]any{"kind": "begin"}},
		}); err != nil {
			driverDone <- err
			return
		}

		workspaceSymbolReq, err := readLSPFrame(r)
		if err != nil {
			driverDone <- fmt.Errorf("read workspace/symbol (Symbols() may be blocking on index readiness instead of issuing the RPC): %w", err)
			return
		}
		if workspaceSymbolReq["method"] != "workspace/symbol" {
			driverDone <- fmt.Errorf("third message = %v, want workspace/symbol", workspaceSymbolReq)
			return
		}
		driverDone <- writeLSPFrame(peer, map[string]any{"jsonrpc": "2.0", "id": workspaceSymbolReq["id"], "result": []any{}})
	}()

	select {
	case err := <-driverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("fake clangd conversation stalled")
	}
	select {
	case got := <-symbolsDone:
		if got.err != nil {
			t.Fatalf("Symbols() error = %v, want nil (must not wait for index completion)", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Symbols() did not return after workspace/symbol was answered")
	}
}
