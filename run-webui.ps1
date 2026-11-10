$ErrorActionPreference = "Stop"
Set-Location -LiteralPath $PSScriptRoot
$env:GOCACHE = Join-Path $PSScriptRoot ".cache\go-build"
go run ./webui/server.go
