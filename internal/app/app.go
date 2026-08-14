// Package app assembles the command-line workflows from the small domain packages.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	Store       *registry.Store
	Output      io.Writer
	ErrorOutput io.Writer
	WatchResult func(error)
	Discover    func(unreal.ProjectRequest) (unreal.Project, error)
	Generate    func(context.Context, registry.Project) (registry.Project, error)
	Doctor      func(context.Context, *registry.Store) (any, error)
	Watch       func(context.Context, []registry.Project) error
	MCP         func(context.Context, *registry.Store) error
	// Environment, Files, and Runner keep doctor discovery independently testable.
	Environment toolchain.Environment
	Files       toolchain.FileSystem
	Runner      toolchain.Runner
}

type App struct {
	d      Dependencies
	sinkMu sync.Mutex
}

func New(d Dependencies) *App {
	if d.Output == nil {
		d.Output = os.Stdout
	}
	if d.ErrorOutput == nil {
		d.ErrorOutput = os.Stderr
	}
	if d.Discover == nil {
		d.Discover = unreal.Discover
	}
	if d.Store == nil {
		d.Store, _ = registry.NewStore("")
	}
	if d.Environment == nil {
		d.Environment = osEnvironment{}
	}
	if d.Files == nil {
		d.Files = hostFiles{}
	}
	if d.Runner == nil {
		d.Runner = execRunner{}
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
	name := strings.TrimSuffix(filepath.Base(d.UProject), filepath.Ext(d.UProject))
	p := registry.Project{Name: name, UProject: d.UProject, Target: d.EditorTarget, Platform: "Win64", Configuration: "Development", Engine: registry.Engine{Root: d.Engine.Root, Version: fmt.Sprintf("%d.%d", d.Version.Major, d.Version.Minor)}}
	if err := a.d.Store.Update(ctx, func(r *registry.Registry) error {
		p.ID = stableID(name, r.Projects)
		r.Projects = append(r.Projects, p)
		return nil
	}); err != nil {
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
	if e := a.d.Store.Update(ctx, func(r *registry.Registry) error {
		for i := range r.Projects {
			if r.Projects[i].ID == c.ProjectName || r.Projects[i].Name == c.ProjectName {
				r.Projects = append(r.Projects[:i], r.Projects[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("project not found: %s", c.ProjectName)
	}); e != nil {
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
	if e = a.d.Store.Update(ctx, func(latest *registry.Registry) error {
		for j := range latest.Projects {
			if latest.Projects[j].ID == r.Projects[i].ID {
				latest.Projects[j] = p
				return nil
			}
		}
		return fmt.Errorf("project not found: %s", c.ProjectName)
	}); e != nil {
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
	selection, err := toolchain.SelectClangd(config, toolchain.DiscoverCandidates(p.Toolchain.ClangdPath, a.d.Environment, toolchain.WindowsCandidateProvider{Environment: a.d.Environment, FileSystem: a.d.Files}), a.d.Runner)
	if err != nil {
		return p, err
	}
	ubt := filepath.Join(p.Engine.Root, "Engine", "Binaries", "DotNET", "UnrealBuildTool", "UnrealBuildTool.dll")
	cache, err := projectCache(p.ID)
	if err != nil {
		return p, err
	}
	if err = os.MkdirAll(cache, 0o700); err != nil {
		return p, err
	}
	projectRoot := filepath.Dir(p.UProject)
	targetFile := filepath.Join(projectRoot, "Source", p.Target+".Target.cs")
	metadataPresent := compdb.RiderMetadataAvailable(projectRoot, p.Target, targetFile)
	metadataStale := compdb.RiderMetadataStale(projectRoot)
	if metadataPresent && !metadataStale {
		staging, stageErr := os.MkdirTemp(cache, "rider-")
		if stageErr != nil {
			return p, stageErr
		}
		clangCL := filepath.Join(filepath.Dir(selection.Path), "clang-cl.exe")
		rider, stageErr := compdb.SynthesizeRider(compdb.RiderInput{ProjectRoot: projectRoot, EngineRoot: p.Engine.Root, Target: p.Target, TargetFile: targetFile, StagingDir: staging, ClangCL: clangCL})
		if stageErr == nil {
			_, stageErr = compdb.ValidateAndPromote(compdb.ValidationInput{StagingDir: staging, DestinationDir: cache, ProjectRoot: projectRoot, EngineRoot: p.Engine.Root})
		}
		if stageErr == nil {
			p.Generation = registry.GenerationState{CompilationDatabase: filepath.Join(cache, compdb.DatabaseName), CacheDir: cache, LastFingerprint: rider.Fingerprint, LastGeneratedAt: time.Now()}
			p.Toolchain = registry.Toolchain{ClangdPath: selection.Path, ClangdVersion: selection.Version.String()}
			return p, nil
		}
		return p, fmt.Errorf("rider metadata generation: %w", stageErr)
	}
	if _, installedErr := os.Stat(filepath.Join(p.Engine.Root, "Engine", "Build", "InstalledBuild.txt")); installedErr == nil {
		if metadataStale {
			return p, errors.New("rider_metadata_stale: regenerate Rider project metadata before indexing")
		}
		return p, errors.New("rider_metadata_missing: generate Rider project metadata before indexing")
	}
	dotnet, ok := toolchain.FindBundledDotnet(p.Engine.Root, hostFiles{})
	if !ok {
		return p, errors.New("bundled Unreal dotnet was not found")
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
	updates := map[string]registry.Toolchain{}
	for i := range r.Projects {
		p := &r.Projects[i]
		_, engineErr := unreal.ValidateEngine(p.Engine.Root)
		probe.Engine = probe.Engine || engineErr == nil
		ubt := filepath.Join(p.Engine.Root, "Engine", "Binaries", "DotNET", "UnrealBuildTool", "UnrealBuildTool.dll")
		probe.UBT = probe.UBT || a.d.Files.Exists(ubt)
		_, dotnet := toolchain.FindBundledDotnet(p.Engine.Root, a.d.Files)
		probe.Dotnet = probe.Dotnet || dotnet
		configPath := filepath.Join(p.Engine.Root, "Engine", "Config", "Windows", "Windows_SDK.json")
		configData, configErr := os.ReadFile(configPath)
		if configErr == nil {
			config, parseErr := toolchain.ParseSDKConfig(configData)
			if parseErr == nil {
				selection, selectErr := toolchain.SelectClangd(config, toolchain.DiscoverCandidates(p.Toolchain.ClangdPath, a.d.Environment, toolchain.WindowsCandidateProvider{Environment: a.d.Environment, FileSystem: a.d.Files}), a.d.Runner)
				if selectErr == nil {
					probe.LLVM = true
					if p.Toolchain.ClangdPath != selection.Path || p.Toolchain.ClangdVersion != selection.Version.String() {
						p.Toolchain.ClangdPath = selection.Path
						p.Toolchain.ClangdVersion = selection.Version.String()
						updates[p.ID] = p.Toolchain
					}
				}
			}
		}
		projectRoot := filepath.Dir(p.UProject)
		probe.GeneratedHeaders = probe.GeneratedHeaders || hasBuildArtifact(projectRoot, ".generated.h")
		probe.ResponseFiles = probe.ResponseFiles || hasBuildArtifact(projectRoot, ".rsp")
	}
	if len(updates) != 0 {
		if err := store.Update(ctx, func(latest *registry.Registry) error {
			for i := range latest.Projects {
				if tool, ok := updates[latest.Projects[i].ID]; ok {
					latest.Projects[i].Toolchain = tool
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	probe = (diagnostics.WindowsHostProbe{Environment: a.d.Environment, FileSystem: a.d.Files}).Apply(probe)
	return diagnostics.Check(probe), nil
}

const maxDoctorBuildEntries = 10_000
const maxDoctorBuildDepth = 10

// hasBuildArtifact scans the Unreal Intermediate/Build tree with fixed bounds.
// Access errors are non-fatal: doctor reports the artifact missing instead of
// failing the entire multi-project diagnosis on one inaccessible directory.
func hasBuildArtifact(projectRoot, suffix string) bool {
	root := filepath.Join(projectRoot, "Intermediate", "Build")
	entries := 0
	found := false
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || found {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries++
		if entries > maxDoctorBuildEntries {
			return fs.SkipAll
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		depth := strings.Count(filepath.ToSlash(relative), "/")
		if entry.IsDir() && depth >= maxDoctorBuildDepth {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), strings.ToLower(suffix)) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

type osEnvironment struct{}

func (osEnvironment) LookupEnv(k string) (string, bool) { return os.LookupEnv(k) }

type watchGenerator func(context.Context, string) error

func (f watchGenerator) Generate(ctx context.Context, id string) error { return f(ctx, id) }
func (a *App) watchReal(ctx context.Context, projects []registry.Project) error {
	coordinator := uewatch.NewCoordinatorWithOptions(watchGenerator(func(run context.Context, id string) error {
		r, i, err := a.find(run, id)
		if err != nil {
			return err
		}
		p, err := a.d.Generate(run, r.Projects[i])
		if err != nil {
			if persistErr := a.persistWatchFailure(id, err); persistErr != nil {
				return errors.Join(err, persistErr)
			}
			return err
		}
		return a.d.Store.Update(run, func(latest *registry.Registry) error {
			for j := range latest.Projects {
				if latest.Projects[j].ID == id {
					latest.Projects[j] = p
					return nil
				}
			}
			return fmt.Errorf("project not found: %s", id)
		})
	}), 2, 500*time.Millisecond, uewatch.CoordinatorOptions{Result: func(result uewatch.Result) {
		if result.Err != nil {
			a.reportWatchError(result.Err)
		}
	}})
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

const watchPersistAttempts = 3
const watchPersistTimeout = 350 * time.Millisecond

func (a *App) persistWatchFailure(id string, cause error) error {
	var last error
	for attempt := 0; attempt < watchPersistAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), watchPersistTimeout)
		err := a.d.Store.Update(ctx, func(latest *registry.Registry) error {
			for i := range latest.Projects {
				if latest.Projects[i].ID == id {
					latest.Projects[i].Generation.InvalidationReason = cause.Error()
					return nil
				}
			}
			return fmt.Errorf("project not found: %s", id)
		})
		cancel()
		if err == nil {
			return nil
		}
		last = err
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	return fmt.Errorf("persist watch failure state: %w", last)
}

func (a *App) reportWatchError(err error) {
	if err == nil {
		return
	}
	a.sinkMu.Lock()
	defer a.sinkMu.Unlock()
	if a.d.WatchResult != nil {
		a.d.WatchResult(err)
		return
	}
	fmt.Fprintf(a.d.ErrorOutput, "watch: %v\n", err)
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
	return (&url.URL{Scheme: "file", Path: p}).String()
}
