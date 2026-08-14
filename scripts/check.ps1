[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$files = Get-ChildItem -Recurse -Filter *.go | ForEach-Object FullName
$unformatted = & gofmt -l $files
if ($unformatted) {
    $unformatted
    throw 'gofmt reported unformatted files'
}
go vet ./...
if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
go test ./...
if ($LASTEXITCODE -ne 0) { throw 'go test failed' }
go build -trimpath ./cmd/soft-ue-index
if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
