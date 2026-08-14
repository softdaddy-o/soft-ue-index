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

Multiple Unreal projects can share this registry. Each project receives its own compilation database, clangd process, cache, and watch state.

## Watch behavior

```powershell
soft-ue-index watch
```

`watch` supervises every registered project. Source-file writes are passed to the existing clangd process so its background index can update without regenerating the database. Creates, deletes, renames, and changes to `.Build.cs`, `.Target.cs`, `.uproject`, or `.uplugin` debounce into one database regeneration per project. Projects can regenerate concurrently, while repeated changes to the same project are serialized and get one follow-up run.

Run `generate <project>` after changing compilation-relevant settings when `watch` is not running.

## MCP setup

Start the server with `soft-ue-index mcp`. It uses stdio only: stdout is reserved for JSON-RPC and diagnostics go to stderr.

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

The first query starts clangd lazily and opens one safe project translation unit to trigger background indexing. Project symbols and engine declarations included by that unit become available first. A cold Unreal Engine index can continue filling in the background; later sessions reuse persistent shards from the project's per-user cache. Source or build-rule changes handled by `watch` update only the affected work instead of rebuilding the entire index from scratch.

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

All registry data, compilation databases, caches, and generation logs remain local to the current user. The MCP server is read-only and has no network listener. To unregister a project, run `soft-ue-index remove <project>`. To remove all local data, close `watch` and `mcp`, then delete the two per-user `soft-ue-index` directories described above.

## License

Licensed under the [Apache License 2.0](LICENSE).
