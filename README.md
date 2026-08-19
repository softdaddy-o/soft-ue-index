# soft-ue-index

`soft-ue-index` generates and maintains a validated `compile_commands.json` for Windows Unreal Engine 5.8 projects, then exposes clangd code intelligence through a read-only MCP server. It is designed for one or more projects without running a separate database service.

## Prerequisites

- Windows 10 or later.
- A source-enabled Unreal Engine 5.8 installation associated with the project.
- LLVM/clangd compatible with the engine's `Windows_SDK.json` requirements. `doctor` finds LLVM from the normal installation locations, `LLVM_PATH`, and `PATH`.
- Rider project metadata or generated project files/build artifacts sufficient for UnrealBuildTool to produce a clang compilation database.

The program does not edit the `.uproject`, source files, engine source, or build settings. Rider-based generation is read-only. When Rider metadata is unavailable, the UnrealBuildTool fallback may refresh generated files under the project's or engine's `Intermediate` directories as part of its normal project analysis.

## Install

Download the Windows x64 archive from the [Releases](https://github.com/softdaddy-o/soft-ue-index/releases) page and place `soft-ue-index.exe` on `PATH`.

Or install with Go:

```powershell
go install github.com/softdaddy-o/soft-ue-index/cmd/soft-ue-index@latest
```

For a source checkout:

```powershell
go build -o soft-ue-index.exe ./cmd/soft-ue-index
```

## Register and generate

Register a project first, then run `doctor`; project-dependent engine and toolchain checks use the registry. JSON output is useful for automation.

```powershell
soft-ue-index add ./MyGame/MyGame.uproject
soft-ue-index doctor
soft-ue-index doctor --json
soft-ue-index list
soft-ue-index generate mygame
soft-ue-index status mygame --json
```

`generate` first uses existing Rider metadata when it is available, including installed-engine modules that UnrealBuildTool may treat as already compiled. It falls back to UnrealBuildTool otherwise. Use `soft-ue-index generate mygame --engine-scope=full` to include every module represented by Rider's UnrealEditor metadata; the default keeps the engine scope to modules associated with the project target.

Projects are registered in a per-user registry. The default Windows location is `%AppData%/soft-ue-index/registry.json`; generated databases and private generation logs are in `%LocalAppData%/soft-ue-index/projects/`. Paths are canonicalized, so adding the same project through a different case or junction does not create a duplicate registration.

Multiple Unreal projects can share this registry. Each project receives its own compilation database, lazy clangd session, and cache. One per-user daemon shares the bounded watcher and query service across every MCP client.

## Prebuilt engine index (optional)

Engine and plugin code is typically the large majority of a project's compilation database (roughly 95% on the project this feature was developed against, see issue #7) and changes far less often than the project's own source. `index-engine` builds an offline index for just that portion, once, and points clangd at it instead of re-deriving the same state from scratch on every cold session:

```powershell
soft-ue-index generate mygame
soft-ue-index index-engine mygame
```

Requires `generate` to have already produced a compilation database, and `clangd-indexer` to be available: it does not ship with the standard LLVM/clangd release, only with LLVM's separate "clangd indexing tools" archive. `index-engine` looks for it next to the resolved `clangd` binary, then on `PATH`, and fails with an actionable message naming that archive if neither has it.

**This writes a `.clangd` config file directly into the project's engine installation root** (e.g. `D:\UE_5.8\.clangd`) -- the only location from which clangd's config discovery (a walk up from each compiled file's own directory) can scope a rule to just `Engine/` for every project that uses that engine install, since a project's own source tree is typically on a different drive or subtree entirely. This is the one thing in this repository that writes outside its own per-user cache and registry directories, which is why it is a separate, explicit command rather than something `generate` does automatically. The generated fragment is marked with a comment identifying it as soft-ue-index-generated and which project last wrote it; `index-engine` refuses to overwrite a `.clangd` at that location that lacks the marker (hand-authored -- remove or merge it manually first). The same project re-running `index-engine` (see refresh policy below) always updates its own fragment; a *different* project sharing the same engine install is refused unless its entry set happens to match exactly, rather than risk one project silently losing symbol coverage the other didn't need -- remove the existing fragment manually to hand the engine root to a different project.

This tree assumes one user per machine, like the rest of soft-ue-index's per-user cache (see Privacy and removal) -- but the fragment it writes lives at the engine root, which is machine-wide. Two different Windows accounts running `index-engine` against the same engine install are not coordinated with each other.

Refresh policy for v1 is manual: re-run `index-engine <project>` after an engine hotfix or plugin update to rebuild from the current compilation database. There is no automatic staleness detection.

Mechanism (`Index.External.File` mounted for a `PathMatch`-scoped fragment, which implicitly disables live `--background-index` for the matched files) verified against clangd 20.1.8 with a small synthetic fixture; not yet verified at the scale of a real multi-thousand-file engine install.

## Watch behavior

```powershell
soft-ue-index watch
```

`watch` remains a foreground compatibility command and uses the same singleton ownership as the daemon, so the two cannot run together. Windows uses one recursive registration for each existing `Source`, `Plugins`, and `Config` root plus cheap project/engine control roots; nested directories do not allocate individual 64 KiB watch buffers. Source-file writes are passed to an existing clangd process without regenerating the database. Creates, deletes, renames, and changes to `.Build.cs`, `.Target.cs`, `.uproject`, or `.uplugin` debounce into one database regeneration per project.

Run `generate <project>` after changing compilation-relevant settings when `watch` is not running.

## MCP setup

Start a client connection with `soft-ue-index mcp`. The command is a small bounded stdio proxy: stdout is reserved for JSON-RPC, and it starts or connects to one current-user-only named-pipe daemon. The daemon owns all filesystem watches and at most one lazy clangd session per active project, regardless of the number of MCP clients.

Daemon controls are available for diagnostics and upgrades:

```powershell
soft-ue-index daemon status --json
soft-ue-index daemon stop
soft-ue-index daemon run
```

Example client configuration:

```json
{
  "mcpServers": {
    "soft-ue-index": {
      "command": "soft-ue-index.exe",
      "args": ["mcp"]
    }
  }
}
```

The server provides `list_projects`, `project_status`, `search_symbols`, `find_definition`, `find_references`, `find_implementations`, `document_symbols`, `hover`, `call_hierarchy`, and `read_symbol_source`. Every source query requires an explicit registered project and is bounded by result, response, and source-read limits. It only reads the selected project and its associated engine roots.

Malformed or oversized stdio requests are rejected before SDK dispatch and may close the MCP stream without a response. Restart the client connection after such a fail-closed rejection.

The first query starts clangd lazily and opens one safe project translation unit to trigger background indexing. Project symbols and engine declarations included by that unit become available first. A cold Unreal Engine index can continue filling in the background; later requests and MCP clients reuse the daemon session and persistent shards from the project's per-user cache. Source or build-rule changes update only the affected work instead of rebuilding the entire index from scratch.

`project_status` reports `index_state` (`absent`, `starting`, `indexing`, `ready`, or `degraded`) without starting or blocking on a session, so a client can poll it instead of guessing. `search_symbols` does not wait for the background index to finish -- clangd answers from whatever it has already indexed, which is useful long before the index is complete, and on a large project full completion can take hours. Its result carries the same `index_state`, so a short or empty result on a cold session can be told apart from "no such symbol": check `index_state` before concluding the latter. Scope a query to a specific directory with `path_prefix` to keep unrelated engine symbols from crowding out project matches within clangd's own result cap. A per-RPC timeout while the index is still building is reported as an actionable "index is still building, retry shortly" message rather than a bare timeout; a timeout once the index is `ready` is a plain timeout, since the index is no longer the likely cause.

The daemon reconciles project additions, removals, roots, toolchains, and compilation-database identities from the per-user registry without restarting MCP clients.

## Cold-index memory

`--clang-tidy=0` is the shipped default (unconditional, not configurable) precisely because of the measurement below. Disabling clang-tidy's default `clang-analyzer-*` static-analysis checks (real per-TU overhead on Unreal's macro/template-heavy headers, and pure cost here since this server only does navigation/search) processed more translation units while using less memory in the same window:

Measured on a real ~26,800-entry Unreal Engine 5.8 compilation database with `--j=1`, sampling clangd's private memory every 15s over a 20-minute cold-start window from an empty index cache:

| | `--clang-tidy` default (on) | `--clang-tidy=0` (shipped) |
|---|---|---|
| Translation units completed | 105/26,796 | 134/26,796 (+28%) |
| Peak private memory | 2529.5 MB | 2117.5 MB (−16%) |
| Private memory at 20 min | 2268.3 MB | 1890.0 MB (−17%) |

This is a partial-window measurement, not a peak at completion: at `--j=1`, full completion of a database this size is on the order of tens of hours, and `search_symbols` does not need it to finish (see above).

Raising `--j` to use more of the host's cores was also measured, on the same database with `--clang-tidy=0` already applied, over the same 20-minute window: `--j=4` completed only 107/26,796 translation units (vs. 134 at `--j=1`) and reached a higher peak private memory (3731.0 MB vs. 2117.5 MB). More worker threads did not trade memory for throughput here -- it cost both, on this machine and background-index priority setting. `--j` is left at its conservative default of `1` rather than tuned upward; this was not investigated further; the working assumption is contention between clangd's own worker threads and whatever else the host was doing during the run, not a property of `--j=4` in general, and it should be re-measured on a quiet, dedicated host before drawing a firm conclusion.

The MVP targets source/custom Unreal Engine installations registered in the current user's Unreal Engine Builds registry. Launcher-only binary installations are outside the current indexing scope unless they are source-enabled and registered there.

## Local UE 5.8 integration check

The repository includes a non-destructive PowerShell check. It calls `doctor`, registers and generates one project, verifies project and engine translation-unit coverage, and makes bounded MCP queries. It does not build or edit the project.

```powershell
./scripts/integration-ue58.ps1 -UProject ./MyGame/MyGame.uproject -Engine ./UE_5.8 -Clangd ./LLVM/bin/clangd.exe -ProjectSymbol MyGameType -EngineSymbol EngineType
```

`-ProjectSymbol` and `-EngineSymbol` are required: the check proves that each query resolves within the expected project or engine root, then requests a definition and bounded references for both. `-Engine` and `-Clangd` are optional expected values; when supplied, the check validates an Unreal 5.8 engine root and verifies that the project association and `doctor` selected exactly those values.

## Troubleshooting

- `doctor` reports LLVM missing: install a compatible LLVM version, add it to `PATH`, or set `LLVM_PATH` to its installation directory.
- `doctor` reports generated headers or response files missing: generate IDE project files and build the editor target once, then run `generate` again.
- Generation fails: inspect the per-project `ubt.log` beside the generated compilation database. The previous valid database is retained on failure.
- MCP reports a project is not ready: run `generate <project>` and verify `status <project> --json` shows a compilation database and clangd path.
- A cold search returns no engine symbol yet: keep the MCP server running while background indexing fills the per-user cache, then retry. Project symbols and commonly included engine declarations are normally available first.

## Privacy and removal

All registry data, compilation databases, caches, and generation logs remain local to the current user. The MCP proxy and daemon are read-only and have no TCP listener; the named pipe ACL grants access only to the current user and LocalSystem. To unregister a project, run `soft-ue-index remove <project>`. Before removing all local data, run `soft-ue-index daemon stop` and close any foreground `watch`, then delete the two per-user `soft-ue-index` directories described above.

`remove` and the directory deletion above do not touch anything `index-engine` wrote (see "Prebuilt engine index"): the `.clangd` fragment at the engine installation root, and the prebuilt index under the per-user `engines/` cache, both persist independently of any one project's registration, since other projects on the same engine install may still depend on them. Delete `<engineRoot>\.clangd` manually if nothing uses it anymore.

## License

Licensed under the [Apache License 2.0](LICENSE).
