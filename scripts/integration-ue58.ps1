[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)] [string] $UProject,
    [Parameter(Mandatory = $true)] [string] $ProjectSymbol,
    [Parameter(Mandatory = $true)] [string] $EngineSymbol,
    [string] $Engine,
    [string] $Clangd,
    [string] $Executable = 'soft-ue-index.exe'
)

$ErrorActionPreference = 'Stop'

function Invoke-Tool([string[]] $Arguments) {
    $null = & $Executable @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) { throw "soft-ue-index failed with exit code $LASTEXITCODE" }
}

function Convert-FileUriToPath([string] $Uri) {
    return ([Uri]$Uri).LocalPath
}

function Assert-UnderRoot([string] $Uri, [string] $Root, [string] $Kind) {
    $path = Convert-FileUriToPath $Uri
    if (-not $path.StartsWith($Root, [StringComparison]::OrdinalIgnoreCase)) { throw "MCP $Kind result was outside its expected source root" }
}

function Invoke-McpSmoke([string] $ProjectID, [string] $ProjectRoot, [string] $EngineRoot, [string] $ProjectQuery, [string] $EngineQuery) {
    $psi = [Diagnostics.ProcessStartInfo]::new()
    $psi.FileName = $Executable
    $psi.ArgumentList.Add('mcp')
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $process = [Diagnostics.Process]::Start($psi)
    try {
        function Send-Request([hashtable] $Request) {
            $process.StandardInput.WriteLine(($Request | ConvertTo-Json -Compress -Depth 10))
            $process.StandardInput.Flush()
            $line = $process.StandardOutput.ReadLine()
            if ([string]::IsNullOrWhiteSpace($line)) { throw 'MCP server returned no response' }
            return $line | ConvertFrom-Json
        }
        $null = Send-Request @{ jsonrpc = '2.0'; id = 1; method = 'initialize'; params = @{ protocolVersion = '2025-06-18'; capabilities = @{}; clientInfo = @{ name = 'integration-smoke'; version = '1' } } }
        $process.StandardInput.WriteLine('{"jsonrpc":"2.0","method":"notifications/initialized"}')
        $process.StandardInput.Flush()
        $projects = Send-Request @{ jsonrpc = '2.0'; id = 2; method = 'tools/call'; params = @{ name = 'list_projects'; arguments = @{ max_items = 20 } } }
        if (-not $projects.result) { throw 'MCP list_projects did not return a result' }
        $id = 3
        foreach ($case in @(@{ Query = $ProjectQuery; Root = $ProjectRoot; Kind = 'project' }, @{ Query = $EngineQuery; Root = $EngineRoot; Kind = 'engine' })) {
            $response = Send-Request @{ jsonrpc = '2.0'; id = $id; method = 'tools/call'; params = @{ name = 'search_symbols'; arguments = @{ project_id = $ProjectID; query = $case.Query; max_items = 10 } } }
            $items = @($response.result.structuredContent.items)
            if ($items.Count -eq 0) { throw "MCP search_symbols returned no $($case.Kind) result" }
            $symbol = $items | Where-Object { (Convert-FileUriToPath $_.location.uri).StartsWith($case.Root, [StringComparison]::OrdinalIgnoreCase) } | Select-Object -First 1
            if (-not $symbol) { throw "MCP search_symbols did not return a $($case.Kind) source result" }
            Assert-UnderRoot $symbol.location.uri $case.Root $case.Kind
            $position = @{ path = (Convert-FileUriToPath $symbol.location.uri); line = $symbol.location.range.start.line; character = $symbol.location.range.start.character }
            $definition = Send-Request @{ jsonrpc = '2.0'; id = ($id + 100); method = 'tools/call'; params = @{ name = 'find_definition'; arguments = @{ project_id = $ProjectID; position = $position; max_items = 10 } } }
            $definitions = @($definition.result.structuredContent.items)
            if ($definitions.Count -eq 0) { throw "MCP find_definition returned no $($case.Kind) result" }
            Assert-UnderRoot $definitions[0].uri $case.Root $case.Kind
            $references = Send-Request @{ jsonrpc = '2.0'; id = ($id + 200); method = 'tools/call'; params = @{ name = 'find_references'; arguments = @{ project_id = $ProjectID; position = $position; max_items = 10 } } }
            if (-not $references.result) { throw "MCP find_references failed for $($case.Kind) symbol" }
            $id++
        }
    } finally {
        if (-not $process.HasExited) { $process.Kill($true) }
        $process.Dispose()
    }
}

if (-not (Test-Path -LiteralPath $UProject -PathType Leaf)) { throw "UProject was not found" }
if (-not (Get-Command $Executable -ErrorAction SilentlyContinue) -and -not (Test-Path -LiteralPath $Executable -PathType Leaf)) { throw "Executable was not found: $Executable" }
if ($Engine) {
    $versionPath = Join-Path $Engine 'Engine/Build/Build.version'
    if (-not (Test-Path -LiteralPath $versionPath -PathType Leaf)) { throw 'Engine is not a valid Unreal root' }
    $version = Get-Content -Raw -LiteralPath $versionPath | ConvertFrom-Json
    if ($version.MajorVersion -ne 5 -or $version.MinorVersion -ne 8) { throw 'Engine is not Unreal Engine 5.8' }
}
if ($Clangd) {
    if (-not (Test-Path -LiteralPath $Clangd -PathType Leaf)) { throw 'clangd executable was not found' }
    $null = & $Clangd --version 2>&1
    if ($LASTEXITCODE -ne 0) { throw 'clangd version check failed' }
}

$start = Get-Date
$projects = (& $Executable list --json | ConvertFrom-Json)
$project = $projects | Where-Object { $_.uproject -eq (Resolve-Path -LiteralPath $UProject).Path } | Select-Object -First 1
if (-not $project) {
    Invoke-Tool @('add', $UProject)
    $projects = (& $Executable list --json | ConvertFrom-Json)
    $project = $projects | Where-Object { $_.uproject -eq (Resolve-Path -LiteralPath $UProject).Path } | Select-Object -First 1
}
if (-not $project) { throw 'Registered project was not found' }
if ($Engine -and $project.engine.root -ne (Resolve-Path -LiteralPath $Engine).Path) { throw 'Registered engine does not match the supplied engine' }
Invoke-Tool @('doctor', '--json')
$project = (& $Executable status $project.id --json | ConvertFrom-Json)
if ($Clangd -and $project.toolchain.clangdPath -ne (Resolve-Path -LiteralPath $Clangd).Path) { throw 'Doctor did not select the supplied compatible clangd' }
Invoke-Tool @('generate', $project.id)

$database = $project.generation.compilationDatabase
$project = (& $Executable status $project.id --json | ConvertFrom-Json)
$database = $project.generation.compilationDatabase
if (-not (Test-Path -LiteralPath $database -PathType Leaf)) { throw 'Compilation database was not generated' }
$entries = Get-Content -Raw -LiteralPath $database | ConvertFrom-Json
$projectRoot = (Resolve-Path -LiteralPath (Split-Path -Parent $UProject)).Path
$engineRoot = $project.engine.root
$projectCount = @($entries | Where-Object { $_.file.StartsWith($projectRoot, [StringComparison]::OrdinalIgnoreCase) }).Count
$engineCount = @($entries | Where-Object { $_.file.StartsWith($engineRoot, [StringComparison]::OrdinalIgnoreCase) }).Count
if ($projectCount -eq 0 -or $engineCount -eq 0) { throw 'Compilation database does not cover both project and engine translation units' }
Invoke-McpSmoke $project.id $projectRoot $engineRoot $ProjectSymbol $EngineSymbol
$elapsed = [Math]::Round(((Get-Date) - $start).TotalSeconds, 2)
Write-Output ("integration passed: project_tus={0}; engine_tus={1}; seconds={2}" -f $projectCount, $engineCount, $elapsed)
