[CmdletBinding()]
param([switch] $SkipRace)

$ErrorActionPreference = 'Stop'
$files = Get-ChildItem -Recurse -Filter *.go | ForEach-Object FullName
$unformatted = & gofmt -l $files
if ($unformatted) {
    $unformatted
    throw 'gofmt reported unformatted files'
}
go mod tidy -diff
if ($LASTEXITCODE -ne 0) { throw 'go.mod or go.sum is not tidy' }
go vet ./...
if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
if (-not $SkipRace) {
    go test -race ./...
    if ($LASTEXITCODE -ne 0) { throw 'go test -race failed; install a Windows C compiler or rerun with -SkipRace for a local non-race check' }
} else {
    Write-Warning 'Race detector explicitly skipped.'
    go test ./...
}
if ($LASTEXITCODE -ne 0) { throw 'go test failed' }
go build -trimpath ./cmd/soft-ue-index
if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
