package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/softdaddy-o/soft-ue-index/internal/cli"
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
	if err := os.MkdirAll(filepath.Join(healthy, "Source"), 0o700); err != nil {
		t.Fatal(err)
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
