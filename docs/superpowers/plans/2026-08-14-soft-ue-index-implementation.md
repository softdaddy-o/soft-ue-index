# soft-ue-index Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Windows Go CLI that maintains Unreal Engine 5.8 project and engine compilation databases, incrementally manages clangd, and exposes bounded read-only code intelligence over MCP.

**Architecture:** A thin command layer composes independent registry, Unreal discovery, toolchain, compilation-database, watcher, clangd, and MCP packages. External processes and filesystems sit behind interfaces so unit and integration tests never require Unreal Engine, while an opt-in local suite validates a legally installed UE 5.8 source build.

**Tech Stack:** Go 1.24, standard library, `github.com/fsnotify/fsnotify`, `github.com/modelcontextprotocol/go-sdk/mcp`, Windows Registry API from `golang.org/x/sys/windows/registry`, GitHub Actions.

---

## Planned File Structure

```text
cmd/soft-ue-index/main.go                 executable entry point
internal/app/app.go                       dependency assembly and command routing
internal/cli/cli.go                       argument parsing and output modes
internal/registry/model.go                persisted registry schema
internal/registry/store.go                locked atomic registry storage
internal/unreal/project.go                uproject and target discovery
internal/unreal/engine.go                 engine association and version discovery
internal/toolchain/toolchain.go           bundled .NET, LLVM, MSVC, and SDK checks
internal/compdb/generator.go               UBT command construction and execution
internal/compdb/validate.go                compilation database validation and promotion
internal/watch/classify.go                 invalidating-change classification
internal/watch/coordinator.go              multi-project debounce and serialization
internal/lsp/client.go                     JSON-RPC/LSP transport to clangd
internal/lsp/manager.go                    per-project clangd lifecycle
internal/mcpserver/server.go               read-only bounded MCP tools
internal/diagnostics/diagnostics.go         stable error codes and remediations
internal/testutil/fixtures.go               temporary fake engines and projects
testdata/                                  public synthetic fixtures only
scripts/integration-ue58.ps1               opt-in real-engine verification
.github/workflows/ci.yml                    Windows tests and build
.github/workflows/release.yml               tagged Windows archives and checksums
README.md                                  installation and usage
LICENSE                                    Apache-2.0 license
```

### Task 1: Bootstrap the Go CLI

**Files:**
- Create: `go.mod`
- Create: `cmd/soft-ue-index/main.go`
- Create: `internal/cli/cli.go`
- Create: `internal/cli/cli_test.go`
- Create: `LICENSE`

- [ ] **Step 1: Write the failing CLI tests**

```go
func TestParseRequiresCommand(t *testing.T) {
    _, err := Parse(nil)
    if !errors.Is(err, ErrUsage) { t.Fatalf("got %v", err) }
}

func TestParseAdd(t *testing.T) {
    project := filepath.Join(t.TempDir(), "Game.uproject")
    got, err := Parse([]string{"add", project})
    if err != nil { t.Fatal(err) }
    if got.Name != "add" || got.ProjectPath == "" { t.Fatalf("got %#v", got) }
}
```

- [ ] **Step 2: Run the tests and verify the package does not exist**

Run: `go test ./internal/cli`

Expected: FAIL because the module and CLI package have not been created.

- [ ] **Step 3: Implement the minimal parser and entry point**

Define `Command{Name, ProjectPath, ProjectName, JSON bool}` and support `doctor`, `add`, `list`, `generate`, `watch`, `status`, `mcp`, and `remove`. Return `ErrUsage` for missing or unknown commands. `main` must pass `os.Args[1:]` to `cli.Parse`, write errors to stderr, and return exit code 2 for usage errors and 1 for operational errors.

- [ ] **Step 4: Add module metadata and license**

Initialize module `github.com/softdaddy-o/soft-ue-index`, require Go 1.24, and add the Apache-2.0 license text.

- [ ] **Step 5: Verify and commit**

Run: `go test ./...`

Expected: PASS.

Commit: `feat: bootstrap soft-ue-index CLI`

### Task 2: Implement the Per-user Project Registry

**Files:**
- Create: `internal/registry/model.go`
- Create: `internal/registry/store.go`
- Create: `internal/registry/store_test.go`

- [ ] **Step 1: Write failing round-trip and rollback tests**

```go
func TestStoreRoundTrip(t *testing.T) {
    dir := t.TempDir()
    s := NewStore(dir)
    project := filepath.Join(dir, "GameA", "GameA.uproject")
    want := Registry{Version: 1, Projects: []Project{{ID: "game-a", Name: "GameA", UProject: project}}}
    if err := s.Save(context.Background(), want); err != nil { t.Fatal(err) }
    got, err := s.Load(context.Background())
    if err != nil { t.Fatal(err) }
    if diff := cmp.Diff(want, got); diff != "" { t.Fatal(diff) }
}

func TestFailedSavePreservesPreviousRegistry(t *testing.T) {
    // Inject a rename failure and assert the previously loaded registry is unchanged.
}
```

- [ ] **Step 2: Run the focused test**

Run: `go test ./internal/registry -run 'TestStore'`

Expected: FAIL with undefined registry types.

- [ ] **Step 3: Implement the schema**

Define versioned `Registry`, `Project`, `Engine`, `Toolchain`, and `GenerationState` structures. Store normalized absolute paths, target, platform, configuration, watch state, selected clangd, database path, last fingerprint, timestamp, and invalidation reason. Reject duplicate IDs and paths.

- [ ] **Step 4: Implement atomic storage**

Resolve the default root from `os.UserCacheDir`/`os.UserConfigDir` into `soft-ue-index`, create it with user-only intent, serialize with indentation to a temporary file, flush and close it, then replace the destination. Use an exclusive lock file with bounded retry. Do not overwrite a readable registry after serialization, validation, or promotion failure.

- [ ] **Step 5: Verify corruption and concurrency behavior**

Add tests for missing files, unsupported schema versions, malformed JSON, duplicate projects, and lock contention.

Run: `go test -race ./internal/registry`

Expected: PASS.

Commit: `feat: add atomic per-user project registry`

### Task 3: Discover Unreal Projects, Engines, and Targets

**Files:**
- Create: `internal/unreal/project.go`
- Create: `internal/unreal/engine.go`
- Create: `internal/unreal/discovery_test.go`
- Create: `internal/testutil/fixtures.go`
- Create: `testdata/project/Game.uproject`
- Create: `testdata/project/Source/GameEditor.Target.cs`
- Create: `testdata/engine/Engine/Build/Build.version`

- [ ] **Step 1: Write failing synthetic-discovery tests**

```go
func TestDiscoverUE58Project(t *testing.T) {
    env := testutil.NewFakeUE58(t)
    got, err := Discover(ProjectRequest{UProject: env.UProject, Associations: map[string]string{"UE_5.8": env.EngineRoot}})
    if err != nil { t.Fatal(err) }
    if got.Version.Major != 5 || got.Version.Minor != 8 { t.Fatalf("got %v", got.Version) }
    if got.EditorTarget != "GameEditor" { t.Fatalf("got %q", got.EditorTarget) }
}
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/unreal`

Expected: FAIL with undefined discovery API.

- [ ] **Step 3: Implement project and version parsing**

Decode `.uproject` into a narrow structure containing `EngineAssociation`; decode `Build.version`; verify `MajorVersion == 5 && MinorVersion == 8`; locate `Engine/Binaries/DotNET/UnrealBuildTool/UnrealBuildTool.dll`; enumerate `Source/*Editor.Target.cs`; prefer `<ProjectName>Editor.Target.cs`; return an ambiguity diagnostic otherwise.

- [ ] **Step 4: Implement Windows engine-association access behind an interface**

Define `AssociationSource` and a Windows implementation reading `HKCU\Software\Epic Games\Unreal Engine\Builds`. Tests use an in-memory map. An explicit `--engine` path must override the association source.

- [ ] **Step 5: Add negative cases and verify**

Test missing association, missing engine files, unsupported version, malformed JSON, missing target, ambiguous targets, and forward-slash input.

Run: `go test ./internal/unreal ./internal/testutil`

Expected: PASS.

Commit: `feat: discover Unreal Engine 5.8 projects`

### Task 4: Diagnose the Windows Toolchain

**Files:**
- Create: `internal/toolchain/toolchain.go`
- Create: `internal/toolchain/toolchain_test.go`
- Create: `internal/diagnostics/diagnostics.go`
- Create: `internal/diagnostics/diagnostics_test.go`

- [ ] **Step 1: Write failing compatibility tests**

```go
func TestSelectPreferredClangd(t *testing.T) {
    root := t.TempDir()
    accepted := []Range{{Min: MustVersion("20.1.8"), Max: MustVersion("20.999")}, {Min: MustVersion("19.1.0"), Max: MustVersion("19.999")}}
    got, err := SelectClangd(accepted, []Candidate{{Path: filepath.Join(root, "LLVM19", "clangd.exe"), Version: MustVersion("19.1.7")}, {Path: filepath.Join(root, "LLVM20", "clangd.exe"), Version: MustVersion("20.1.8")}})
    if err != nil { t.Fatal(err) }
    if got.Version.String() != "20.1.8" { t.Fatalf("got %v", got.Version) }
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./internal/toolchain ./internal/diagnostics`

Expected: FAIL with undefined types.

- [ ] **Step 3: Implement deterministic discovery**

Read `Engine/Config/Windows/Windows_SDK.json`, parse `PreferredClangVersions` and minimum version, find the engine's bundled `dotnet.exe`, search explicit configuration, `LLVM_PATH`, PATH, standard LLVM installation, and Visual Studio LLVM locations, and execute `clangd --version` through an injected runner. Choose the first candidate in engine preference order, not the newest arbitrary installation.

- [ ] **Step 4: Implement doctor results**

Return structured checks with status, stable code, summary, detail, and remediation for engine, UBT, bundled .NET, LLVM, MSVC, Windows SDK, generated headers, and response files. Human and JSON renderers must share the same data.

- [ ] **Step 5: Verify**

Run: `go test ./internal/toolchain ./internal/diagnostics`

Expected: PASS.

Commit: `feat: add Unreal toolchain diagnostics`

### Task 5: Generate and Validate Compilation Databases

**Files:**
- Create: `internal/compdb/generator.go`
- Create: `internal/compdb/validate.go`
- Create: `internal/compdb/generator_test.go`
- Create: `internal/compdb/validate_test.go`

- [ ] **Step 1: Write failing command-construction test**

```go
func TestBuildGenerateCommand(t *testing.T) {
    root := t.TempDir()
    ubt := filepath.Join(root, "Engine", "UnrealBuildTool.dll")
    got := BuildCommand(Input{DotNet: filepath.Join(root, "dotnet.exe"), UBTDLL: ubt, UProject: filepath.Join(root, "Game.uproject"), Target: "GameEditor", OutputDir: filepath.Join(root, "staging")})
    assertContainsInOrder(t, got.Args, ubt, "-Mode=GenerateClangDatabase", "GameEditor", "Development", "Win64", "-Compiler=Clang", "-NoExecCodeGenActions")
}
```

- [ ] **Step 2: Write failing validation and rollback tests**

Create databases with only project entries, only engine entries, malformed entries, missing response files, and full coverage. Assert invalid staging data never replaces `compile_commands.json`.

- [ ] **Step 3: Implement process-safe command construction**

Return executable and argument slices rather than a shell string. Supply project, target, configuration, platform, compiler, output directory, and output filename explicitly. Capture bounded stdout/stderr while writing full output to a private log.

- [ ] **Step 4: Implement validation and atomic promotion**

Stream-decode entries, count project and engine translation units by canonical root containment, verify required fields, compiler paths, and `@response` paths, compute a stable fingerprint, and promote with replace-on-success semantics.

- [ ] **Step 5: Verify**

Run: `go test ./internal/compdb`

Expected: PASS.

Commit: `feat: generate validated Unreal compilation databases`

### Task 6: Watch Multiple Projects Efficiently

**Files:**
- Create: `internal/watch/classify.go`
- Create: `internal/watch/coordinator.go`
- Create: `internal/watch/coordinator_test.go`

- [ ] **Step 1: Write failing classification table test**

```go
func TestClassify(t *testing.T) {
    cases := []struct{ path string; op fsnotify.Op; want bool }{
        {`Source\Foo.cpp`, fsnotify.Write, false},
        {`Source\Foo.cpp`, fsnotify.Create, true},
        {`Source\Foo.h`, fsnotify.Remove, true},
        {`Source\Game.Build.cs`, fsnotify.Write, true},
        {`Game.uproject`, fsnotify.Write, true},
        {`Plugin.uplugin`, fsnotify.Write, true},
    }
    // Assert RequiresCompDB for every case.
}
```

- [ ] **Step 2: Write failing debounce/isolation test**

Use a fake clock and generator. Burst five events for project A and one for project B. Assert one generation per project, serialized within a project and concurrent across projects up to the configured global limit.

- [ ] **Step 3: Implement recursive watch management**

Use `fsnotify`, add relevant source, plugin, and project directories, add newly created directories, ignore cache/intermediate output roots, and classify create/remove/rename separately from content writes.

- [ ] **Step 4: Implement coordinator state machine**

Maintain per-project idle, debouncing, running, and pending states. A relevant event during generation sets `pending`; completion schedules exactly one follow-up. Errors update registry status without terminating other projects.

- [ ] **Step 5: Verify under race detector**

Run: `go test -race ./internal/watch`

Expected: PASS.

Commit: `feat: watch multiple Unreal projects incrementally`

### Task 7: Add clangd LSP Transport and Lifecycle

**Files:**
- Create: `internal/lsp/client.go`
- Create: `internal/lsp/manager.go`
- Create: `internal/lsp/client_test.go`
- Create: `internal/lsp/manager_test.go`
- Create: `internal/testutil/fakels/main.go`

- [ ] **Step 1: Write a failing protocol framing test**

Feed a fake LSP server split `Content-Length` frames and concurrent responses. Assert the client correlates IDs, handles notifications, enforces response-size limits, and cancels timed-out requests.

- [ ] **Step 2: Write a failing manager lifecycle test**

Use a fake process factory. Assert the first request starts one process, simultaneous requests share it, a crash restarts with bounded backoff, and idle timeout closes it.

- [ ] **Step 3: Implement LSP client**

Implement initialize/initialized/shutdown/exit, workspace symbol, definition, references, implementation, document symbol, hover, call hierarchy, didOpen, and didClose. Keep framing and typed normalization separate from process management.

- [ ] **Step 4: Implement project manager**

Start `clangd` with the validated compilation-database directory, bounded worker threads, background index, and private log path. Track starting, indexing, ready, failed, and idle states. Persist clangd's disk index under the project cache and never delete it automatically.

- [ ] **Step 5: Verify**

Run: `go test -race ./internal/lsp ./internal/testutil/fakels`

Expected: PASS.

Commit: `feat: manage project clangd language servers`

### Task 8: Expose Bounded Read-only MCP Tools

**Files:**
- Create: `internal/mcpserver/server.go`
- Create: `internal/mcpserver/server_test.go`
- Modify: `go.mod`

- [ ] **Step 1: Write failing MCP tool tests**

Connect an in-memory MCP client using the official Go SDK. Assert `tools/list` exposes only the nine approved read-only tools. Call `find_references` with a fake semantic backend returning 1,000 entries and assert the response is truncated to the configured maximum with `truncated: true`.

- [ ] **Step 2: Run the focused test**

Run: `go test ./internal/mcpserver`

Expected: FAIL because the server is absent.

- [ ] **Step 3: Implement the semantic backend interface and tools**

Define project-explicit inputs and normalized outputs for `list_projects`, `project_status`, `search_symbols`, `find_definition`, `find_references`, `find_implementations`, `document_symbols`, `hover`, `call_hierarchy`, and `read_symbol_source`. Enforce maximum items, context lines, source bytes, call depth, and total serialized bytes.

- [ ] **Step 4: Use stdio transport without logging to stdout**

Run MCP protocol exclusively on stdin/stdout. Send logs to stderr or files. Reject unknown projects and paths outside registered project and engine roots.

- [ ] **Step 5: Verify**

Run: `go test -race ./internal/mcpserver`

Expected: PASS.

Commit: `feat: expose bounded read-only MCP code intelligence`

### Task 9: Assemble Commands and End-to-end Fake Integration

**Files:**
- Create: `internal/app/app.go`
- Create: `internal/app/app_test.go`
- Modify: `cmd/soft-ue-index/main.go`
- Modify: `internal/cli/cli.go`

- [ ] **Step 1: Write failing end-to-end fake-engine test**

Create two fake projects sharing one fake engine. Run `add`, `generate`, `list`, and `status`; start the watcher; mutate build metadata in one project; assert only that project regenerates; start MCP and query fake project and engine symbols.

- [ ] **Step 2: Implement application dependency assembly**

Wire real filesystem, registry association source, runner, watcher, process factory, clock, LSP manager, and MCP server. Commands accept `--json`; human output remains concise. Add SIGINT/SIGTERM cancellation for watch and MCP.

- [ ] **Step 3: Verify operational exit codes**

Use 0 for success, 1 for operational failure, and 2 for usage. Ensure partial multi-project watch failures report status but do not exit until canceled.

- [ ] **Step 4: Run all tests**

Run: `go test -race ./...`

Expected: PASS.

Commit: `feat: assemble soft-ue-index workflows`

### Task 10: Documentation, CI, Release, and Real UE 5.8 Verification

**Files:**
- Create: `README.md`
- Create: `scripts/integration-ue58.ps1`
- Create: `.github/workflows/ci.yml`
- Create: `.github/workflows/release.yml`
- Create: `.gitignore`

- [ ] **Step 1: Write the public setup guide**

Document Windows and UE 5.8 prerequisites, install, `doctor`, multi-project registration, generation, watch semantics, MCP client configuration, cache locations, privacy, troubleshooting, and uninstall. Use generic example paths and no proprietary fixtures.

- [ ] **Step 2: Add an opt-in integration script**

Accept `-UProject`, optional `-Engine`, and `-Clangd`. Run doctor and generation, assert both project and engine translation units, start MCP through a test client, query one supplied project symbol and one supplied engine symbol, and emit a sanitized summary. Never build or edit the supplied project.

- [ ] **Step 3: Add Windows CI**

On pull requests and pushes, run `gofmt -w` verification via a diff check, `go vet ./...`, `go test -race ./...`, and `go build ./cmd/soft-ue-index`. Upload the binary only from trusted branch builds.

- [ ] **Step 4: Add tagged release workflow**

Build Windows x64, package executable/license/README, produce SHA-256 checksums, and attach artifacts to `v*` GitHub releases. Pin action major versions and grant only `contents: write` to the release job.

- [ ] **Step 5: Run local real-engine verification**

Run the opt-in integration script against a UE 5.8 source build and a test project. Expected: validated project and engine coverage, successful engine definition lookup, and bounded engine reference results. Record only timings, counts, versions, and sanitized failure codes in the release notes.

- [ ] **Step 6: Run final verification**

Run:

```powershell
gofmt -w .
go vet ./...
go test -race ./...
go build ./cmd/soft-ue-index
git diff --check
```

Expected: all commands succeed and the worktree is clean except for intended documentation or release metadata.

- [ ] **Step 7: Commit**

Commit: `docs: add setup CI and release automation`

## Plan Self-review

- Every acceptance criterion in the approved design maps to Tasks 3 through 10.
- The public test strategy uses only synthetic fixtures; the real-engine suite is opt-in and path-parameterized.
- Registry, UBT, watcher, clangd, and MCP boundaries are independently testable.
- No task requires modifying an Unreal project or engine installation.
- Compilation database generation and promotion preserve the last known-good database.
- Multi-project correctness is tested before live Unreal verification.
- All MCP operations remain read-only and response-bounded.
