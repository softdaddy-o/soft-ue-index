// Package app assembles the command-line workflows from the small domain packages.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/softdaddy-o/soft-ue-index/internal/cli"
	"github.com/softdaddy-o/soft-ue-index/internal/compdb"
	"github.com/softdaddy-o/soft-ue-index/internal/diagnostics"
	"github.com/softdaddy-o/soft-ue-index/internal/lsp"
	"github.com/softdaddy-o/soft-ue-index/internal/mcpserver"
	"github.com/softdaddy-o/soft-ue-index/internal/registry"
	"github.com/softdaddy-o/soft-ue-index/internal/toolchain"
	"github.com/softdaddy-o/soft-ue-index/internal/unreal"
	uewatch "github.com/softdaddy-o/soft-ue-index/internal/watch"
)

// Dependencies allow tests and embedders to replace effects without replacing
// command routing. New supplies the real per-user store when Store is omitted.
type Dependencies struct {
	Store    *registry.Store
	Output   io.Writer
	Discover func(unreal.ProjectRequest) (unreal.Project, error)
	Generate func(context.Context, registry.Project) (registry.Project, error)
	Doctor   func(context.Context, *registry.Store) (any, error)
	Watch    func(context.Context, []registry.Project) error
	MCP      func(context.Context, *registry.Store) error
}

type App struct{ d Dependencies }

func New(d Dependencies) *App {
	if d.Output == nil {
		d.Output = os.Stdout
	}
	if d.Discover == nil {
		d.Discover = unreal.Discover
	}
	if d.Store == nil {
		d.Store, _ = registry.NewStore("")
	}
	a := &App{d: d}
	if a.d.Generate == nil {
		a.d.Generate = a.generateReal
	}
	if a.d.Doctor == nil {
		a.d.Doctor = a.doctorReal
	}
	if a.d.Watch == nil {
		a.d.Watch = a.watchReal
	}
	return a
}

func (a *App) Run(ctx context.Context, command cli.Command) error {
	if a.d.Store == nil {
		return errors.New("open project registry")
	}
	switch command.Name {
	case "list":
		return a.list(ctx, command.JSON)
	case "add":
		return a.add(ctx, command)
	case "remove":
		return a.remove(ctx, command)
	case "status":
		return a.status(ctx, command)
	case "generate":
		return a.generate(ctx, command)
	case "doctor":
		return a.doctor(ctx, command.JSON)
	case "watch":
		return a.watch(ctx)
	case "mcp":
		return a.mcp(ctx)
	default:
		return fmt.Errorf("unknown command %q", command.Name)
	}
}

func (a *App) load(ctx context.Context) (registry.Registry, error) { return a.d.Store.Load(ctx) }
func (a *App) save(ctx context.Context, r registry.Registry) error { return a.d.Store.Save(ctx, r) }

func (a *App) list(ctx context.Context, asJSON bool) error {
	r, err := a.load(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		return a.writeJSON(r.Projects)
	}
	for _, p := range r.Projects {
		fmt.Fprintf(a.d.Output, "%s\t%s\n", p.ID, p.Name)
	}
	return nil
}
func (a *App) add(ctx context.Context, c cli.Command) error {
	d, err := a.d.Discover(unreal.ProjectRequest{UProject: c.ProjectPath, AssociationSource: unreal.WindowsAssociationSource{}})
	if err != nil {
		return err
	}
	r, err := a.load(ctx)
	if err != nil {
		return err
	}
	name := strings.TrimSuffix(filepath.Base(d.UProject), filepath.Ext(d.UProject))
	p := registry.Project{ID: stableID(name, r.Projects), Name: name, UProject: d.UProject, Target: d.EditorTarget, Platform: "Win64", Configuration: "Development", Engine: registry.Engine{Root: d.Engine.Root, Version: fmt.Sprintf("%d.%d", d.Version.Major, d.Version.Minor)}}
	r.Projects = append(r.Projects, p)
	if err := a.save(ctx, r); err != nil {
		return err
	}
	if c.JSON {
		return a.writeJSON(p)
	}
	fmt.Fprintf(a.d.Output, "added %s\n", p.ID)
	return nil
}
func stableID(name string, projects []registry.Project) string {
	base := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "-"))
	if base == "" {
		base = "project"
	}
	id := base
	for n := 2; ; n++ {
		found := false
		for _, p := range projects {
			if p.ID == id {
				found = true
				break
			}
		}
		if !found {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
}
func (a *App) find(ctx context.Context, id string) (registry.Registry, int, error) {
	r, e := a.load(ctx)
	if e != nil {
		return r, -1, e
	}
	for i := range r.Projects {
		if r.Projects[i].ID == id || r.Projects[i].Name == id {
			return r, i, nil
		}
	}
	return r, -1, fmt.Errorf("project not found: %s", id)
}
func (a *App) remove(ctx context.Context, c cli.Command) error {
	r, i, e := a.find(ctx, c.ProjectName)
	if e != nil {
		return e
	}
	r.Projects = append(r.Projects[:i], r.Projects[i+1:]...)
	if e = a.save(ctx, r); e != nil {
		return e
	}
	if c.JSON {
		return a.writeJSON(map[string]string{"removed": c.ProjectName})
	}
	fmt.Fprintf(a.d.Output, "removed %s\n", c.ProjectName)
	return nil
}
func (a *App) status(ctx context.Context, c cli.Command) error {
	r, i, e := a.find(ctx, c.ProjectName)
	if e != nil {
		return e
	}
	p := r.Projects[i]
	if c.JSON {
		return a.writeJSON(p)
	}
	ready := "not generated"
	if p.Generation.CompilationDatabase != "" {
		ready = "ready"
	}
	fmt.Fprintf(a.d.Output, "%s: %s\n", p.ID, ready)
	return nil
}
func (a *App) generate(ctx context.Context, c cli.Command) error {
	r, i, e := a.find(ctx, c.ProjectName)
	if e != nil {
		return e
	}
	p, e := a.d.Generate(ctx, r.Projects[i])
	if e != nil {
		return e
	}
	r.Projects[i] = p
	if e = a.save(ctx, r); e != nil {
		return e
	}
	if c.JSON {
		return a.writeJSON(p)
	}
	fmt.Fprintf(a.d.Output, "generated %s\n", p.ID)
	return nil
}
func (a *App) doctor(ctx context.Context, asJSON bool) error {
	v, e := a.d.Doctor(ctx, a.d.Store)
	if e != nil {
		return e
	}
	if asJSON {
		return a.writeJSON(v)
	}
	if report, ok := v.(diagnostics.Report); ok {
		_, e := fmt.Fprintln(a.d.Output, diagnostics.RenderHuman(report))
		return e
	}
	fmt.Fprintln(a.d.Output, "toolchain checks complete")
	return nil
}
func (a *App) watch(ctx context.Context) error {
	r, e := a.load(ctx)
	if e != nil {
		return e
	}
	return a.d.Watch(ctx, r.Projects)
}
func (a *App) mcp(ctx context.Context) error {
	if a.d.MCP != nil {
		return a.d.MCP(ctx, a.d.Store)
	}
	manager := lsp.NewManager(nil)
	defer manager.Close()
	return mcpserver.New(mcpserver.Dependencies{Projects: a.d.Store, Queries: lspQueries{manager: manager}}).RunStdio(ctx, "dev")
}
func (a *App) writeJSON(v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = fmt.Fprintln(a.d.Output, string(b))
	return e
}

type hostFiles struct{}

func (hostFiles) Exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
func (hostFiles) Glob(pattern string) []string { paths, _ := filepath.Glob(pattern); return paths }

func (a *App) generateReal(ctx context.Context, p registry.Project) (registry.Project, error) {
	sdk, err := os.ReadFile(filepath.Join(p.Engine.Root, "Engine", "Config", "Windows", "Windows_SDK.json"))
	if err != nil {
		return p, fmt.Errorf("read Windows SDK configuration: %w", err)
	}
	config, err := toolchain.ParseSDKConfig(sdk)
	if err != nil {
		return p, err
	}
	selection, err := toolchain.SelectClangd(config, toolchain.DiscoverCandidates(p.Toolchain.ClangdPath, osEnvironment{}, toolchain.WindowsCandidateProvider{Environment: osEnvironment{}, FileSystem: hostFiles{}}), execRunner{})
	if err != nil {
		return p, err
	}
	dotnet, ok := toolchain.FindBundledDotnet(p.Engine.Root, hostFiles{})
	if !ok {
		return p, errors.New("bundled Unreal dotnet was not found")
	}
	ubt := filepath.Join(p.Engine.Root, "Engine", "Binaries", "DotNET", "UnrealBuildTool", "UnrealBuildTool.dll")
	cache, err := projectCache(p.ID)
	if err != nil {
		return p, err
	}
	if err = os.MkdirAll(cache, 0o700); err != nil {
		return p, err
	}
	staging, err := compdb.Generate(ctx, compdb.ExecRunner{}, compdb.Input{DotNet: dotnet, UBTDLL: ubt, UProject: p.UProject, Target: p.Target, Configuration: p.Configuration, Platform: p.Platform}, cache, filepath.Join(cache, "ubt.log"))
	if err != nil {
		return p, err
	}
	result, err := compdb.ValidateAndPromote(compdb.ValidationInput{StagingDir: staging, DestinationDir: cache, ProjectRoot: filepath.Dir(p.UProject), EngineRoot: p.Engine.Root})
	if err != nil {
		return p, err
	}
	p.Generation = registry.GenerationState{CompilationDatabase: filepath.Join(cache, compdb.DatabaseName), CacheDir: cache, LastFingerprint: result.Fingerprint, LastGeneratedAt: time.Now()}
	p.Toolchain = registry.Toolchain{ClangdPath: selection.Path, ClangdVersion: selection.Version.String()}
	return p, nil
}
func projectCache(id string) (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, id)
	return filepath.Join(root, "soft-ue-index", "projects", safe), nil
}

func (a *App) doctorReal(ctx context.Context, store *registry.Store) (any, error) {
	r, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	probe := diagnostics.Probe{}
	for _, p := range r.Projects {
		probe.Engine = probe.Engine || p.Engine.Root != ""
		ubt := filepath.Join(p.Engine.Root, "Engine", "Binaries", "DotNET", "UnrealBuildTool", "UnrealBuildTool.dll")
		probe.UBT = probe.UBT || hostFiles{}.Exists(ubt)
		_, dotnet := toolchain.FindBundledDotnet(p.Engine.Root, hostFiles{})
		probe.Dotnet = probe.Dotnet || dotnet
		if p.Toolchain.ClangdPath != "" {
			probe.LLVM = true
		}
		probe.GeneratedHeaders = probe.GeneratedHeaders || hasGlob(filepath.Join(filepath.Dir(p.UProject), "Intermediate", "Build", "**", "*.generated.h"))
		probe.ResponseFiles = probe.ResponseFiles || hasGlob(filepath.Join(filepath.Dir(p.UProject), "Intermediate", "Build", "**", "*.rsp"))
	}
	probe = (diagnostics.WindowsHostProbe{Environment: osEnvironment{}, FileSystem: hostFiles{}}).Apply(probe)
	return diagnostics.Check(probe), nil
}
func hasGlob(pattern string) bool { matches, _ := filepath.Glob(pattern); return len(matches) > 0 }

type osEnvironment struct{}

func (osEnvironment) LookupEnv(k string) (string, bool) { return os.LookupEnv(k) }

type watchGenerator func(context.Context, string) error

func (f watchGenerator) Generate(ctx context.Context, id string) error { return f(ctx, id) }
func (a *App) watchReal(ctx context.Context, projects []registry.Project) error {
	coordinator := uewatch.NewCoordinator(watchGenerator(func(run context.Context, id string) error {
		r, i, err := a.find(run, id)
		if err != nil {
			return err
		}
		p, err := a.d.Generate(run, r.Projects[i])
		if err != nil {
			r.Projects[i].Generation.InvalidationReason = err.Error()
			_ = a.save(context.Background(), r)
			return err
		}
		r.Projects[i] = p
		return a.save(run, r)
	}), 2, 500*time.Millisecond)
	defer coordinator.Close()
	w, err := uewatch.NewWatcher(coordinator)
	if err != nil {
		return err
	}
	defer w.Close()
	for _, p := range projects {
		if err := w.AddProject(uewatch.ProjectRoots{ID: p.ID, ProjectRoot: filepath.Dir(p.UProject), EngineRoot: p.Engine.Root}); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return nil
}

// execRunner remains intentionally small so diagnostics commands never invoke a shell.
type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, error) {
	b, e := exec.Command(name, args...).CombinedOutput()
	return string(b), e
}

// lspQueries is the small translation boundary between registry-backed
// projects and MCP's path-oriented request model.
type lspQueries struct{ manager *lsp.Manager }

func (q lspQueries) client(ctx context.Context, p registry.Project) (*lsp.Client, func(), error) {
	if p.Toolchain.ClangdPath == "" {
		return nil, func() {}, errors.New("clangd is not configured for project")
	}
	c, e := q.manager.Client(ctx, lsp.ProjectConfig{ID: p.ID, Clangd: p.Toolchain.ClangdPath, CompilationDatabase: p.Generation.CompilationDatabase, CacheDir: p.Generation.CacheDir, RootURI: fileURI(filepath.Dir(p.UProject))})
	return c, func() { q.manager.Release(p.ID) }, e
}
func (q lspQueries) Symbols(ctx context.Context, p registry.Project, s string, n int) ([]lsp.Symbol, error) {
	c, done, e := q.client(ctx, p)
	defer done()
	if e != nil {
		return nil, e
	}
	return c.WorkspaceSymbols(ctx, s, lsp.Limits{MaxItems: n})
}
func (q lspQueries) Locations(ctx context.Context, p registry.Project, kind string, v mcpserver.TextPosition, n int) ([]lsp.Location, error) {
	c, done, e := q.client(ctx, p)
	defer done()
	if e != nil {
		return nil, e
	}
	pos := lsp.TextDocumentPosition{URI: fileURI(v.Path), Position: lsp.Position{Line: v.Line, Character: v.Character}}
	switch kind {
	case "definition":
		return c.Definitions(ctx, pos, lsp.Limits{MaxItems: n})
	case "references":
		return c.ReferenceLocations(ctx, pos, lsp.Limits{MaxItems: n})
	default:
		return c.Implementations(ctx, pos, lsp.Limits{MaxItems: n})
	}
}
func (q lspQueries) DocumentSymbols(ctx context.Context, p registry.Project, path string, n int) ([]lsp.DocumentSymbol, error) {
	c, done, e := q.client(ctx, p)
	defer done()
	if e != nil {
		return nil, e
	}
	return c.DocumentSymbols(ctx, fileURI(path), lsp.Limits{MaxItems: n})
}
func (q lspQueries) Hover(ctx context.Context, p registry.Project, v mcpserver.TextPosition) (*lsp.HoverResult, error) {
	c, done, e := q.client(ctx, p)
	defer done()
	if e != nil {
		return nil, e
	}
	return c.HoverResult(ctx, lsp.TextDocumentPosition{URI: fileURI(v.Path), Position: lsp.Position{Line: v.Line, Character: v.Character}})
}
func (q lspQueries) PrepareCallHierarchy(ctx context.Context, p registry.Project, v mcpserver.TextPosition) ([]lsp.CallHierarchyItem, error) {
	c, done, e := q.client(ctx, p)
	defer done()
	if e != nil {
		return nil, e
	}
	return c.PrepareCallHierarchy(ctx, lsp.TextDocumentPosition{URI: fileURI(v.Path), Position: lsp.Position{Line: v.Line, Character: v.Character}})
}
func (q lspQueries) IncomingCalls(ctx context.Context, p registry.Project, i lsp.CallHierarchyItem) ([]lsp.CallHierarchyCall, error) {
	c, done, e := q.client(ctx, p)
	defer done()
	if e != nil {
		return nil, e
	}
	return c.IncomingCalls(ctx, i)
}
func (q lspQueries) OutgoingCalls(ctx context.Context, p registry.Project, i lsp.CallHierarchyItem) ([]lsp.CallHierarchyCall, error) {
	c, done, e := q.client(ctx, p)
	defer done()
	if e != nil {
		return nil, e
	}
	return c.OutgoingCalls(ctx, i)
}
func fileURI(path string) string {
	p := filepath.ToSlash(path)
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + p
	}
	return "file://" + p
}
