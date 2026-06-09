<#
.SYNOPSIS
  Build the digitorn daemon + CLI + workers on Windows, WITH the treesitter
  code-intel (grep call-graph enrichment + full repo-map).

  `make` isn't available on Windows, and the Makefile omits `-tags treesitter`
  (so a plain build silently drops the code-intel). This is the canonical
  Windows build.

.EXAMPLE
  .\build.ps1            # build everything
  .\build.ps1 -Run       # build, then (re)launch the daemon
  .\build.ps1 -NoStop    # build without stopping a running daemon (may fail if locked)
#>
param([switch]$Run, [switch]$NoStop)

$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $MyInvocation.MyCommand.Path)

$pkg = 'github.com/mbathepaul/digitorn'
$version = (git describe --tags --always --dirty 2>$null)
if (-not $version) { $version = 'dev' }
$date = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
$ldflags = "-s -w -X $pkg/internal/version.Version=$version -X $pkg/internal/version.BuildDate=$date"
$tags = 'treesitter'

# A running daemon locks its .exe, so go build can't overwrite it. Stop the
# daemon + workers first (unless -NoStop).
if (-not $NoStop) {
  Get-Process digitornd, 'digitorn-worker', 'digitorn-worker-llm', 'digitorn-worker-tokenizer', 'digitorn-worker-embeddings' -ErrorAction SilentlyContinue | Stop-Process -Force
  Start-Sleep -Milliseconds 600
}

$targets = @(
  @{ out = 'bin\digitornd.exe'; src = './cmd/digitornd' },
  @{ out = 'bin\digitorn.exe'; src = './cmd/digitorn' },
  @{ out = 'bin\digitorn-worker.exe'; src = './cmd/digitorn-worker' },
  @{ out = 'bin\digitorn-worker-llm.exe'; src = './cmd/digitorn-worker-llm' },
  @{ out = 'bin\digitorn-worker-embeddings.exe'; src = './cmd/digitorn-worker-embeddings' }
)
foreach ($t in $targets) {
  Write-Host "building $($t.out)" -ForegroundColor Cyan
  & go build -trimpath -tags $tags -ldflags $ldflags -o $t.out $t.src
  if ($LASTEXITCODE -ne 0) { throw "build failed: $($t.src)" }
}
Write-Host "OK - daemon + CLI + workers built (tags: $tags, version: $version)" -ForegroundColor Green

if ($Run) {
  Start-Process -FilePath '.\bin\digitornd.exe' -ArgumentList '-config', '.\bin\config.yaml' `
    -WindowStyle Hidden -RedirectStandardError '.\bin\daemon.err.log' -RedirectStandardOutput '.\bin\daemon.out.log'
  Write-Host "daemon launched (-config .\bin\config.yaml) - logs: bin\daemon.err.log" -ForegroundColor Green
}
