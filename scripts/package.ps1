<#
.SYNOPSIS
    Builds the daemon, CLI and every worker into one deployable bundle.

.DESCRIPTION
    digitornd resolves its worker binaries alongside its own executable, so the
    whole bundle must be deployed as a single folder. This script produces that
    folder (and a .zip) under dist/.

.EXAMPLE
    pwsh scripts/package.ps1
    pwsh scripts/package.ps1 -Version 1.0.0
#>
param(
    [string]$Version = "dev",
    [string]$OutDir = "dist"
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

$goos = (& go env GOOS).Trim()
$goarch = (& go env GOARCH).Trim()
$ext = if ($goos -eq "windows") { ".exe" } else { "" }
$ldflags = "-s -w"

$bundle = Join-Path $OutDir "digitorn-$Version-$goos-$goarch"
if (Test-Path $bundle) { Remove-Item -Recurse -Force $bundle }
New-Item -ItemType Directory -Force -Path $bundle | Out-Null

$cmds = [ordered]@{
    "digitornd"                  = "./cmd/digitornd"
    "digitorn"                   = "./cmd/digitorn"
    "digitorn-worker"            = "./cmd/digitorn-worker"
    "digitorn-worker-llm"        = "./cmd/digitorn-worker-llm"
    "digitorn-worker-embeddings" = "./cmd/digitorn-worker-embeddings"
}

foreach ($name in $cmds.Keys) {
    $out = Join-Path $bundle "$name$ext"
    Write-Host "building $name ..."
    & go build -trimpath -ldflags $ldflags -o $out $cmds[$name]
    if ($LASTEXITCODE -ne 0) { throw "build failed: $name" }
}

Copy-Item config.example.yaml (Join-Path $bundle "config.example.yaml")
Copy-Item README.md (Join-Path $bundle "README.md")

$zip = "$bundle.zip"
if (Test-Path $zip) { Remove-Item -Force $zip }
Compress-Archive -Path (Join-Path $bundle "*") -DestinationPath $zip

Write-Host ""
Write-Host "Bundle:  $bundle"
Write-Host "Archive: $zip"
Write-Host ""
Write-Host "Deploy the whole folder, then from an elevated shell:"
Write-Host "  .\digitornd$ext -config <path-to>\config.yaml install"
Write-Host "  .\digitornd$ext start"
