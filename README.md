# soft-ue-index

`soft-ue-index` generates and maintains a small, validated `compile_commands.json` for Windows Unreal Engine 5.8 projects, then exposes clangd code intelligence through a read-only MCP server. It is designed for one or more projects without indexing an entire engine in a separate database service.

## Prerequisites

- Windows 10 or later.
- A source-enabled Unreal Engine 5.8 installation associated with the project.
- LLVM/clangd compatible with the engine's `Windows_SDK.json` requirements. `doctor` finds LLVM from the normal installation locations, `LLVM_PATH`, and `PATH`.
- Generated project files/build artifacts sufficient for UnrealBuildTool to produce a clang compilation database.

The program uses Unreal's bundled .NET runtime and UnrealBuildTool. It does not modify the `.uproject`, engine, source files, or build settings.

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

Run `doctor` first. JSON output is useful for automation.

```powershell
soft-ue-index doctor
soft-ue-index doctor --json
soft-ue-index add ./MyGame/MyGame.uproject
soft-ue-index list
soft-ue-index generate mygame
soft-ue-index status mygame --json
```

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

## Local UE 5.8 integration check

The repository includes a non-destructive PowerShell check. It calls `doctor`, registers and generates one project, verifies project and engine translation-unit coverage, and makes bounded MCP queries. It does not build or edit the project.

```powershell
./scripts/integration-ue58.ps1 -UProject ./MyGame/MyGame.uproject -ProjectSymbol MyGameType -EngineSymbol EngineType
```

`-ProjectSymbol` and `-EngineSymbol` are required: the check proves that each query resolves within the expected project or engine root, then requests a definition and bounded references for both. Engine selection comes from the project's Unreal association; `doctor` selects a compatible clangd.

## Troubleshooting

- `doctor` reports LLVM missing: install a compatible LLVM version, add it to `PATH`, or set `LLVM_PATH` to its installation directory.
- `doctor` reports generated headers or response files missing: generate IDE project files and build the editor target once, then run `generate` again.
- Generation fails: inspect the per-project `ubt.log` beside the generated compilation database. The previous valid database is retained on failure.
- MCP reports a project is not ready: run `generate <project>` and verify `status <project> --json` shows a compilation database and clangd path.

## Privacy and removal

All registry data, compilation databases, caches, and generation logs remain local to the current user. The MCP server is read-only and has no network listener. To unregister a project, run `soft-ue-index remove <project>`. To remove all local data, close `watch` and `mcp`, then delete the two per-user `soft-ue-index` directories described above.

## License

Licensed under the [Apache License 2.0](LICENSE).
