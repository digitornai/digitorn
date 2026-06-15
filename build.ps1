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
$baseTags = 'treesitter'
# treesitter (code-intel) and onnx (real embeddings) both use cgo.
$env:CGO_ENABLED = '1'

# A running daemon locks its .exe, so go build can't overwrite it. Stop the
# daemon + workers first (unless -NoStop).
if (-not $NoStop) {
  Get-Process digitornd, 'digitorn-worker', 'digitorn-worker-llm', 'digitorn-worker-tokenizer', 'digitorn-worker-embeddings' -ErrorAction SilentlyContinue | Stop-Process -Force
  Start-Sleep -Milliseconds 600
}

$targets = @(
  @{ out = 'bin\digitornd.exe'; src = './cmd/digitornd'; tags = $baseTags },
  @{ out = 'bin\digitorn.exe'; src = './cmd/digitorn'; tags = $baseTags },
  @{ out = 'bin\digitorn-worker.exe'; src = './cmd/digitorn-worker'; tags = $baseTags },
  @{ out = 'bin\digitorn-worker-llm.exe'; src = './cmd/digitorn-worker-llm'; tags = $baseTags },
  # The embeddings worker MUST carry -tags onnx : without it there is no real
  # ONNX backend, so DefaultModel fails and the worker crash-loops (exit 3) in
  # onnx mode. This is the canonical reason semantic search silently dies.
  @{ out = 'bin\digitorn-worker-embeddings.exe'; src = './cmd/digitorn-worker-embeddings'; tags = "$baseTags onnx" }
)
foreach ($t in $targets) {
  Write-Host "building $($t.out) (tags: $($t.tags))" -ForegroundColor Cyan
  & go build -trimpath -tags $t.tags -ldflags $ldflags -o $t.out $t.src
  if ($LASTEXITCODE -ne 0) { throw "build failed: $($t.src)" }
}
Write-Host "OK - daemon + CLI + workers built (embeddings: +onnx, version: $version)" -ForegroundColor Green

if ($Run) {
  Start-Process -FilePath '.\bin\digitornd.exe' -ArgumentList '-config', '.\bin\config.yaml' `
    -WindowStyle Hidden -RedirectStandardError '.\bin\daemon.err.log' -RedirectStandardOutput '.\bin\daemon.out.log'
  Write-Host "daemon launched (-config .\bin\config.yaml) - logs: bin\daemon.err.log" -ForegroundColor Green
}
