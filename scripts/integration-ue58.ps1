[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)] [string] $UProject,
    [Parameter(Mandatory = $true)] [string] $ProjectSymbol,
    [Parameter(Mandatory = $true)] [string] $EngineSymbol,
    [string] $Engine,
    [string] $Clangd,
    [ValidateRange(1, 3600)] [int] $TimeoutSeconds = 300,
    [string] $Executable = 'soft-ue-index.exe'
)

$ErrorActionPreference = 'Stop'
$script:Deadline = (Get-Date).AddSeconds($TimeoutSeconds)

function ConvertTo-WindowsCommandLineArgument([string] $Value) {
    if ($null -eq $Value) { return '""' }
    if ($Value.Length -gt 0 -and $Value -notmatch '[\s"]') { return $Value }
    # Quote according to CommandLineToArgvW: double backslashes before quotes
    # and before the closing quote so PS 5.1 can safely use Arguments.
    $escaped = $Value -replace '(\\*)"', '$1$1\\"'
    $escaped = $escaped -replace '(\\+)$', '$1$1'
    return '"' + $escaped + '"'
}
function Set-ProcessArguments([Diagnostics.ProcessStartInfo] $Info, [string[]] $Arguments) {
    $argumentList = $Info.GetType().GetProperty('ArgumentList')
    if ($null -ne $argumentList -and $null -ne $Info.ArgumentList) {
        foreach ($argument in $Arguments) { [void]$Info.ArgumentList.Add($argument) }
        return
    }
    $Info.Arguments = (($Arguments | ForEach-Object { ConvertTo-WindowsCommandLineArgument $_ }) -join ' ')
}
function Invoke-External([string] $FileName, [string[]] $Arguments) {
    $remaining = [Math]::Floor(($script:Deadline - (Get-Date)).TotalSeconds)
    if ($remaining -lt 1) { throw 'overall integration timeout expired' }
    $psi = [Diagnostics.ProcessStartInfo]::new()
    $psi.FileName = $FileName; $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true; $psi.RedirectStandardError = $true
    Set-ProcessArguments $psi $Arguments
    $process = [Diagnostics.Process]::Start($psi)
    $out = $process.StandardOutput.ReadToEndAsync(); $err = $process.StandardError.ReadToEndAsync()
    if (-not $process.WaitForExit([int]$remaining * 1000)) {
        $process.Kill($true); $process.WaitForExit(); $process.Dispose()
        throw 'external command timed out'
    }
    $out.Wait(); $err.Wait(); $code = $process.ExitCode; $text = $out.Result; $process.Dispose()
    if ($code -ne 0) { throw "external command failed with exit code $code" }
    return $text
}
function Invoke-Tool([string[]] $Arguments) {
    $null = Invoke-External $Executable $Arguments
}

function Convert-FileUriToPath([string] $Uri) {
    return ([Uri]$Uri).LocalPath
}

function Test-UnderRoot([string] $Path, [string] $Root) {
    try {
        if (Test-Path -LiteralPath $Path) { $path = (Resolve-Path -LiteralPath $Path).Path } else { $path = [IO.Path]::GetFullPath($Path) }
        if (Test-Path -LiteralPath $Root) { $root = (Resolve-Path -LiteralPath $Root).Path } else { $root = [IO.Path]::GetFullPath($Root) }
    } catch { return $false }
    [char[]]$separators = @([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
    $root = $root.TrimEnd($separators)
    if ([string]::Equals($path.TrimEnd($separators), $root, [StringComparison]::OrdinalIgnoreCase)) { return $true }
    foreach ($separator in $separators | Select-Object -Unique) {
        if ($path.StartsWith($root + $separator, [StringComparison]::OrdinalIgnoreCase)) { return $true }
    }
    return $false
}
function Assert-UnderRoot([string] $Uri, [string] $Root, [string] $Kind) {
    if (-not (Test-UnderRoot (Convert-FileUriToPath $Uri) $Root)) { throw "MCP $Kind result was outside its expected source root" }
}

function Invoke-McpSmoke([string] $ProjectID, [string] $ProjectRoot, [string] $EngineRoot, [string] $ProjectQuery, [string] $EngineQuery) {
    $psi = [Diagnostics.ProcessStartInfo]::new()
    $psi.FileName = $Executable
    Set-ProcessArguments $psi @('mcp')
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $utf8NoBom = [Text.UTF8Encoding]::new($false)
    if ($psi.PSObject.Properties.Name -contains 'StandardInputEncoding') { $psi.StandardInputEncoding = $utf8NoBom }
    if ($psi.PSObject.Properties.Name -contains 'StandardOutputEncoding') { $psi.StandardOutputEncoding = $utf8NoBom }
    if ($psi.PSObject.Properties.Name -contains 'StandardErrorEncoding') { $psi.StandardErrorEncoding = $utf8NoBom }
    $process = [Diagnostics.Process]::Start($psi)
    $stderr = $process.StandardError.ReadToEndAsync()
    try {
        function Write-McpLine([string] $Json) {
            $bytes = ([Text.UTF8Encoding]::new($false)).GetBytes($Json + "`n")
            $process.StandardInput.BaseStream.Write($bytes, 0, $bytes.Length)
            $process.StandardInput.BaseStream.Flush()
        }
        function Send-Request([hashtable] $Request) {
            Write-McpLine ($Request | ConvertTo-Json -Compress -Depth 10)
            $read = $process.StandardOutput.ReadLineAsync()
            $remaining = [Math]::Floor(($script:Deadline - (Get-Date)).TotalSeconds)
            if ($remaining -lt 1 -or -not $read.Wait([int]$remaining * 1000)) { throw 'MCP request timed out' }
            $line = $read.Result
            if ([string]::IsNullOrWhiteSpace($line)) {
                if ($process.HasExited) { throw "MCP server exited with code $($process.ExitCode)" }
                throw 'MCP server returned no response'
            }
            return $line | ConvertFrom-Json
        }
        $null = Send-Request @{ jsonrpc = '2.0'; id = 1; method = 'initialize'; params = @{ protocolVersion = '2025-06-18'; capabilities = @{}; clientInfo = @{ name = 'integration-smoke'; version = '1' } } }
        Write-McpLine '{"jsonrpc":"2.0","method":"notifications/initialized"}'
        $projects = Send-Request @{ jsonrpc = '2.0'; id = 2; method = 'tools/call'; params = @{ name = 'list_projects'; arguments = @{ max_items = 20 } } }
        if (-not $projects.result) { throw 'MCP list_projects did not return a result' }
        $id = 3
        foreach ($case in @(@{ Query = $ProjectQuery; Root = $ProjectRoot; Kind = 'project' }, @{ Query = $EngineQuery; Root = $EngineRoot; Kind = 'engine' })) {
            $response = Send-Request @{ jsonrpc = '2.0'; id = $id; method = 'tools/call'; params = @{ name = 'search_symbols'; arguments = @{ project_id = $ProjectID; query = $case.Query; max_items = 10 } } }
            $items = @($response.result.structuredContent.items)
            if ($items.Count -eq 0) { throw "MCP search_symbols returned no $($case.Kind) result" }
            $symbol = $items | Where-Object { Test-UnderRoot (Convert-FileUriToPath $_.location.uri) $case.Root } | Select-Object -First 1
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
        $process.WaitForExit()
        $stderr.Wait($TimeoutSeconds * 1000) | Out-Null
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
    $null = Invoke-External $Clangd @('--version')
}

$start = Get-Date
$projects = (Invoke-External $Executable @('list', '--json') | ConvertFrom-Json)
$project = $projects | Where-Object { $_.uproject -eq (Resolve-Path -LiteralPath $UProject).Path } | Select-Object -First 1
if (-not $project) {
    Invoke-Tool @('add', $UProject)
    $projects = (Invoke-External $Executable @('list', '--json') | ConvertFrom-Json)
    $project = $projects | Where-Object { $_.uproject -eq (Resolve-Path -LiteralPath $UProject).Path } | Select-Object -First 1
}
if (-not $project) { throw 'Registered project was not found' }
if ($Engine -and $project.engine.root -ne (Resolve-Path -LiteralPath $Engine).Path) { throw 'Registered engine does not match the supplied engine' }
Invoke-Tool @('doctor', '--json')
$project = (Invoke-External $Executable @('status', $project.id, '--json') | ConvertFrom-Json)
if ($Clangd -and $project.toolchain.clangdPath -ne (Resolve-Path -LiteralPath $Clangd).Path) { throw 'Doctor did not select the supplied compatible clangd' }
Invoke-Tool @('generate', $project.id)

$database = $project.generation.compilationDatabase
$project = (Invoke-External $Executable @('status', $project.id, '--json') | ConvertFrom-Json)
$database = $project.generation.compilationDatabase
if (-not (Test-Path -LiteralPath $database -PathType Leaf)) { throw 'Compilation database was not generated' }
$entries = Get-Content -Raw -LiteralPath $database | ConvertFrom-Json
$projectRoot = (Resolve-Path -LiteralPath (Split-Path -Parent $UProject)).Path
$engineRoot = $project.engine.root
$projectCount = @($entries | Where-Object { Test-UnderRoot $_.file $projectRoot }).Count
$engineCount = @($entries | Where-Object { Test-UnderRoot $_.file $engineRoot }).Count
if (Test-UnderRoot (Join-Path ([IO.Path]::GetDirectoryName($projectRoot)) (([IO.Path]::GetFileName($projectRoot)) + '-sibling/test.cpp')) $projectRoot) { throw 'root containment regression' }
if ($projectCount -eq 0 -or $engineCount -eq 0) { throw 'Compilation database does not cover both project and engine translation units' }
Invoke-McpSmoke $project.id $projectRoot $engineRoot $ProjectSymbol $EngineSymbol
$elapsed = [Math]::Round(((Get-Date) - $start).TotalSeconds, 2)
Write-Output ("integration passed: project_tus={0}; engine_tus={1}; seconds={2}" -f $projectCount, $engineCount, $elapsed)
