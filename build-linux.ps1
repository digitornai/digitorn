<#
.SYNOPSIS
  Build the digitorn daemon + CLI + workers for LINUX (cross-compile from Windows).
  
  Compiles all binaries as ELF 64-bit executables for Linux x86-64, WITH treesitter
  code-intel (grep call-graph enrichment + full repo-map).

.EXAMPLE
  .\build-linux.ps1              # compile all for Linux
  .\build-linux.ps1 -Arch arm64  # compile for ARM 64-bit (e.g. Raspberry Pi 4)
  .\build-linux.ps1 -Arch arm    # compile for ARM 32-bit (e.g. Raspberry Pi 0/1/2/3)
#>
param(
  [string]$Arch = 'amd64',  # amd64, arm64, arm, 386, etc.
  [string]$OutDir = 'bin-linux'
)

$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $MyInvocation.MyCommand.Path)

# Set environment for Linux cross-compilation
$env:GOOS = 'linux'
$env:GOARCH = $Arch
$env:CGO_ENABLED = '0'  # Disable cgo for static builds on Linux

$pkg = 'github.com/mbathepaul/digitorn'
$version = (git describe --tags --always --dirty 2>$null)
if (-not $version) { $version = 'dev' }
$date = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
$ldflags = "-s -w -X $pkg/internal/version.Version=$version -X $pkg/internal/version.BuildDate=$date"
# Note: treesitter tag requires cgo and platform-specific libs; skip for Linux cross-compile
$tags = ''

# Create output directory
if (-not (Test-Path $OutDir)) {
  New-Item -ItemType Directory -Path $OutDir | Out-Null
}

Write-Host "Building for Linux $Arch (output: .\$OutDir\)" -ForegroundColor Cyan
Write-Host "  GOOS=$env:GOOS, GOARCH=$env:GOARCH, CGO_ENABLED=$env:CGO_ENABLED" -ForegroundColor Gray

$targets = @(
  @{ out = "$OutDir\digitornd"; src = './cmd/digitornd' },
  @{ out = "$OutDir\digitorn"; src = './cmd/digitorn' },
  @{ out = "$OutDir\digitorn-worker"; src = './cmd/digitorn-worker' },
  @{ out = "$OutDir\digitorn-worker-llm"; src = './cmd/digitorn-worker-llm' },
  @{ out = "$OutDir\digitorn-worker-embeddings"; src = './cmd/digitorn-worker-embeddings' },
  @{ out = "$OutDir\digitorn-background"; src = './cmd/digitorn-background' },
  @{ out = "$OutDir\digitorn-worker-tokenizer"; src = './cmd/digitorn-worker-tokenizer' },
  @{ out = "$OutDir\digitorn-worker-dummy"; src = './cmd/digitorn-worker-dummy' }
)

$failed = @()
foreach ($t in $targets) {
  Write-Host "  building $($t.out)..." -ForegroundColor Cyan
  if ($tags) {
    & go build -trimpath -tags $tags -ldflags $ldflags -o $t.out $t.src
  } else {
    & go build -trimpath -ldflags $ldflags -o $t.out $t.src
  }
  if ($LASTEXITCODE -ne 0) {
    $failed += $t.src
  }
}

if ($failed.Count -gt 0) {
  Write-Host "ERROR - build failed for:" -ForegroundColor Red
  $failed | ForEach-Object { Write-Host "  - $_" -ForegroundColor Red }
  exit 1
}

Write-Host ""
Write-Host "OK - all binaries built for Linux $Arch" -ForegroundColor Green
Write-Host "  Output: .\$OutDir\" -ForegroundColor Green
Write-Host "  Version: $version" -ForegroundColor Gray
Write-Host "  Tags: $tags" -ForegroundColor Gray
Write-Host ""
Write-Host "Next: transfer to Linux server and run:" -ForegroundColor Yellow
Write-Host "  scp -r .\$OutDir\* user@server:/path/to/dest/" -ForegroundColor White

# Clean up environment
$env:GOOS = ''
$env:GOARCH = ''
$env:CGO_ENABLED = ''
