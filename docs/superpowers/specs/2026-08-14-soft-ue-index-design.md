# soft-ue-index Design

## Purpose

`soft-ue-index` is a lightweight Windows CLI for building and maintaining accurate code indexes for Unreal Engine 5.8 source and Unreal projects. It generates Clang compilation databases through UnrealBuildTool, runs compatible `clangd` instances on demand, watches only the files that can invalidate compilation commands, and exposes compact read-only semantic navigation tools over MCP.

The first release targets Windows and Unreal Engine 5.8 source builds. It supports multiple projects and multiple engine installations without writing generated configuration or indexes into project repositories by default.

## Goals

- Generate a compilation database that contains both project and relevant engine translation units.
- Preserve `clangd` disk indexes across sessions and update them incrementally.
- Avoid regenerating the compilation database for ordinary source-content edits.
- Detect the project changes that do require database regeneration.
- Manage multiple Unreal projects from one per-user registry and one watcher process.
- Expose bounded, token-efficient, read-only code intelligence to MCP clients.
- Provide actionable diagnostics for UnrealBuildTool, .NET, LLVM, MSVC, SDK, generated-header, and indexing failures.
- Ship as a single Go executable with no Go runtime installation required.

## Non-goals for the First Release

- Supporting Unreal Engine versions earlier than 5.8.
- Supporting Linux or macOS.
- Replacing an editor, compiler, debugger, or UnrealBuildTool.
- Editing code through MCP, including rename, code actions, formatting, or quick fixes.
- Parsing Blueprints, assets, or reflected runtime data.
- Combining different projects into one semantic index when their build environments differ.
- Bundling Unreal Engine source, headers, binaries, SDKs, or proprietary fixtures.
- Running as a Windows service.

## User Experience

The binary is named `soft-ue-index` and provides these primary commands:

```powershell
soft-ue-index doctor
soft-ue-index add <path-to-project.uproject>
soft-ue-index list
soft-ue-index generate MyGame
soft-ue-index watch
soft-ue-index status MyGame
soft-ue-index mcp
soft-ue-index remove MyGame
```

`doctor` checks host-wide and project-specific prerequisites. `add` discovers the project, engine, targets, and compatible toolchain. `generate` creates and validates a compilation database without replacing the last known-good database on failure. `watch` observes every enabled registered project from one foreground process. `mcp` starts a stdio MCP server and starts project-specific `clangd` processes only when a semantic request needs them.

## Architecture

The application is divided into focused packages:

- **CLI:** Parses commands and renders human-readable or JSON output.
- **Registry:** Loads, validates, migrates, locks, and atomically writes per-user configuration.
- **Unreal discovery:** Reads `.uproject`, the Windows Unreal Engine association registry, `Build.version`, target rules, and UnrealBuildTool locations.
- **Toolchain discovery:** Finds Unreal's bundled .NET runtime, reads the engine's allowed LLVM versions, locates `clangd`, and checks MSVC and Windows SDK prerequisites.
- **Compilation database generator:** Constructs and runs UnrealBuildTool commands, validates generated JSON and translation-unit coverage, and atomically promotes valid output.
- **Watch coordinator:** Classifies filesystem changes, debounces invalidations per project, and serializes generation work for each project.
- **clangd manager:** Starts one `clangd` workspace per active project, retains disk caches, tracks readiness, and stops idle processes.
- **MCP server:** Maps compact read-only MCP tools to LSP requests and bounds every response.
- **Diagnostics:** Produces structured error codes, remediation hints, status reports, and privacy-safe logs.

Package boundaries keep Unreal-specific discovery and command construction independent from process execution, filesystem watching, LSP transport, and MCP transport so each can be tested without a real engine installation.

## Per-user Storage

State is stored below the Windows local application-data directory:

```text
%LOCALAPPDATA%/soft-ue-index/
├── config.json
├── projects.json
├── logs/
└── cache/
    ├── engines/
    └── projects/
```

The registry records:

- Stable project ID and display name.
- Absolute `.uproject` path.
- Discovered engine root, association, and version.
- Editor target, platform, and configuration.
- Selected `clangd` executable and version.
- Compilation-database and index-cache locations.
- Last successful generation fingerprint and timestamp.
- Watch enablement and last invalidation reason.

Registry writes use a lock and replace-on-success semantics. Paths are normalized for comparison while retaining a display form suitable for Windows commands. Project repositories remain unchanged unless a future explicit option requests project-local output.

## Project and Engine Discovery

Adding a project performs these steps:

1. Validate that the supplied file is a readable `.uproject` document.
2. Read `EngineAssociation`.
3. Resolve the association through the Windows Unreal Engine builds registry, unless an engine root was explicitly supplied.
4. Verify that the engine contains `Build.version` and UnrealBuildTool.
5. Require Unreal Engine 5.8 for the first release.
6. Locate an editor target, preferring an exact project editor target and reporting ambiguity instead of guessing.
7. Find Unreal's bundled .NET runtime before considering a machine-wide runtime.
8. Read the engine's Windows SDK configuration to determine supported LLVM versions.

Explicit command-line options override discovery, but discovered values are recorded so subsequent commands are deterministic.

## Compilation Database Generation

The generator invokes UnrealBuildTool's `GenerateClangDatabase` mode through the engine's bundled .NET runtime. It supplies the selected editor target, project, Win64 platform, Development configuration, Clang compiler, explicit output directory, and `-NoExecCodeGenActions` for fast refreshes after generated sources already exist.

The initial generation performs prerequisite checks and can instruct the user to run a normal editor build when generated sources are missing. The tool does not silently trigger a full engine or project build.

Generation uses a staging directory. Before promotion, validation confirms:

- The output parses as a JSON compilation database.
- Every entry has a source file, working directory, and command.
- At least one project translation unit is present.
- At least one engine translation unit is present when engine indexing is requested.
- Referenced compiler and response-file paths exist where applicable.
- The database is non-empty and newer than the generation start time.

Only a validated database replaces the previous known-good version.

## Incremental Update Policy

Ordinary content edits to existing `.cpp`, `.cc`, `.cxx`, `.h`, `.hpp`, and `.inl` files do not regenerate the compilation database. `clangd` observes those edits and updates affected translation units incrementally.

The watcher invalidates the compilation database for:

- Addition, deletion, or rename of C/C++ source and header files.
- Changes to `.Build.cs` and `.Target.cs` files.
- Changes to `.uproject` and `.uplugin` files.
- Changes to explicitly tracked toolchain or engine-association metadata.

Events are debounced per project. Only one generation may run for a project at a time. A change arriving during generation schedules one follow-up pass rather than launching concurrent UnrealBuildTool processes.

Builds remain responsible for refreshing UnrealHeaderTool-generated headers. When a build changes generated files, `clangd` observes those changes and reindexes affected units. If generated headers are missing or stale enough to make parsing unreliable, status and diagnostics report that a normal editor build is required.

## Multiple-project Behavior

One `watch` process monitors every enabled registered project. Each project has an independent queue, compilation database, cache, status, and failure state. Failure in one project does not stop other projects.

Projects sharing an engine reuse:

- Engine discovery metadata.
- Compatible LLVM installation discovery.
- Immutable engine version and toolchain checks.

Projects do not share a live semantic workspace or compilation database. Plugin sets, target rules, macros, generated headers, and module dependencies can make identical engine source parse differently for different projects. Correctness takes priority over avoiding all duplicate disk index entries.

## clangd Lifecycle

`soft-ue-index` uses a compatible installed `clangd`; it does not embed LLVM. `doctor` reports the preferred and accepted versions from the selected Unreal Engine installation. The first release provides exact installation guidance but does not install system software without an explicit user command.

The MCP server starts a project-specific `clangd` process lazily. Processes use the validated compilation database and a persistent cache under the project's per-user cache directory. The manager waits for index readiness where required, records process and memory status, restarts crashed processes with bounded backoff, and stops idle processes after a configurable timeout.

Response limits are enforced above LSP so a valid but enormous reference result cannot exhaust an agent's context.

## MCP Interface

The MCP transport is stdio in the first release. Tools are read-only:

- `list_projects`
- `project_status`
- `search_symbols`
- `find_definition`
- `find_references`
- `find_implementations`
- `document_symbols`
- `hover`
- `call_hierarchy`
- `read_symbol_source`

Every semantic call identifies a registered project. Results use normalized file paths, exact ranges, concise signatures, and optional bounded source context. Defaults favor small results; callers may increase limits only up to configured maximums. Truncation is explicit and includes continuation guidance where the underlying operation supports it.

## Diagnostics and Errors

Errors have a stable code, short summary, technical detail, and remediation. Important classes include:

- Project or engine association cannot be resolved.
- Unsupported Unreal version.
- UnrealBuildTool or bundled .NET runtime is missing.
- No compatible LLVM installation is available.
- MSVC or Windows SDK is unavailable or incompatible.
- Generated headers or response files are missing.
- Compilation database lacks project or engine coverage.
- File watching is unavailable or degraded.
- `clangd` fails, crashes, or never becomes ready.
- Registry or cache state is corrupt or locked.

Logs redact environment values and avoid recording credentials. Paths necessary to diagnose local operation can appear in local logs, but public reports produced by an explicit export command will sanitize user and machine-specific path prefixes.

## Testing

Unit tests cover registry migration and atomic writes, path normalization, `.uproject` and `Build.version` parsing, engine association resolution, target selection, LLVM compatibility, UnrealBuildTool command construction, compilation-database validation, change classification, debounce behavior, and error rendering.

Integration tests use temporary fake engine and project trees, a fake UnrealBuildTool process, and a controllable fake LSP server. They verify multi-project isolation, failed-generation rollback, coalesced watch events, lazy process startup, idle shutdown, crash recovery, response bounding, and MCP request mapping.

Windows CI runs all tests that do not require Epic Games content. An opt-in local integration suite accepts paths to a legally obtained Unreal Engine 5.8 source build and sample project. No Unreal source, generated engine data, proprietary response files, or internal project names are committed to the public repository.

## Distribution

GitHub Actions produces signed-checksum Windows x64 archives containing the single executable, license, and concise setup documentation. Releases document the supported Unreal and LLVM versions. Installation begins with downloading an archive and placing the executable on `PATH`; package-manager manifests can be added after the first stable release.

## Security and Privacy

- MCP tools are read-only in the first release.
- File operations are restricted to registered project, engine, and per-user state roots.
- External processes use argument arrays rather than shell-composed commands.
- Registry and cache paths are not trusted until canonicalized and validated.
- No telemetry is collected.
- No project source is uploaded.
- Logs and exported diagnostics never include credentials or full environment dumps.

## Acceptance Criteria

The first release is successful when a user with a Windows Unreal Engine 5.8 source build can:

1. Register two or more projects, including projects that share an engine.
2. Generate validated compilation databases containing project and engine translation units.
3. Restart the tool and reuse prior registry and index state.
4. Edit an existing source file without regenerating the compilation database.
5. Add a source file or change build metadata and observe one debounced database refresh.
6. Query a project symbol and an engine symbol through MCP.
7. Retrieve definitions and bounded reference results from both project and engine source.
8. Keep one project's watcher and queries operating when another project fails generation.
9. Diagnose missing generated headers, toolchains, or engine coverage with a concrete remediation.
