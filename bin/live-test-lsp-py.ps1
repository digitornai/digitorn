# Live LSP Python worker end-to-end test (GATEWAY mode)
#
# A real agent edits a .py file; the lsp_diagnose hook + lsp.notify_change route
# to a digitorn-worker (lsp-pool) which drives pyright; pyright's real diagnostic
# comes back through the LLM. Rebuilds binaries (so it includes the URI fix) and
# uses its own throwaway daemon on :28002 — does not touch your :8000 daemon.
#
# Usage : powershell -File bin\live-test-lsp-py.ps1

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Log-Info ($m) { Write-Host "[live-lsp-py] $m" -ForegroundColor Cyan }
function Log-Ok   ($m) { Write-Host "[live-lsp-py] $m" -ForegroundColor Green }
function Log-Warn ($m) { Write-Host "[live-lsp-py] $m" -ForegroundColor Yellow }
function Abort($m) { Write-Host "[live-lsp-py] $m" -ForegroundColor Red; exit 1 }

function Invoke-Rest($method, $url, $headers, $body) {
    try {
        if ($null -eq $body) { return Invoke-RestMethod -Method $method -Uri $url -Headers $headers }
        return Invoke-RestMethod -Method $method -Uri $url -Headers $headers -Body $body
    } catch {
        $resp = $_.Exception.Response; $rb = ""
        if ($resp) { try { $rb = (New-Object System.IO.StreamReader($resp.GetResponseStream())).ReadToEnd() } catch {} }
        throw "HTTP from $method $url : $rb"
    }
}

$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $repoRoot

if (-not $env:DIGITORN_DEV_JWT) {
    $cp = Join-Path $env:USERPROFILE ".digitorn\credentials.json"
    if (Test-Path $cp) { $env:DIGITORN_DEV_JWT = (Get-Content $cp -Raw | ConvertFrom-Json).access_token }
}
if (-not $env:DIGITORN_DEV_JWT) { Abort "no JWT" }
if (-not (Get-Command pyright-langserver -ErrorAction SilentlyContinue)) { Abort "pyright-langserver not on PATH (pip install pyright)" }
if (-not $env:DIGITORN_GATEWAY_URL) { $env:DIGITORN_GATEWAY_URL = "http://127.0.0.1:8002/v1" }

Log-Info "building digitornd / digitorn-worker-llm / digitorn-worker ..."
& go build -o (Join-Path $repoRoot "bin\digitornd.exe") "./cmd/digitornd"; if ($LASTEXITCODE) { Abort "build digitornd" }
& go build -o (Join-Path $repoRoot "bin\digitorn-worker-llm.exe") "./cmd/digitorn-worker-llm"; if ($LASTEXITCODE) { Abort "build worker-llm" }
& go build -o (Join-Path $repoRoot "bin\digitorn-worker.exe") "./cmd/digitorn-worker"; if ($LASTEXITCODE) { Abort "build worker" }
Log-Ok "binaries built (includes the URI fix)"

$base = "C:\Users\ASUS\AppData\Local\Temp\digitorn-live-lsp"
if (Test-Path $base) { Remove-Item -Recurse -Force $base }
New-Item -ItemType Directory -Force -Path (Join-Path $base "sessions") | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $base "apps") | Out-Null
$workdir = Join-Path $base "ws"; New-Item -ItemType Directory -Force -Path $workdir | Out-Null

$daemonLog = Join-Path $base "daemon.err.log"
$cfg = Join-Path $repoRoot "bin\config-live-lsp.yaml"
$baseURL = "http://127.0.0.1:28002"
$env:DIGITORN_WORKERS__LLM__GATEWAY_URL = $env:DIGITORN_GATEWAY_URL

$daemon = Start-Process -FilePath (Join-Path $repoRoot "bin\digitornd.exe") -ArgumentList "-config",$cfg `
    -RedirectStandardOutput (Join-Path $base "daemon.out.log") -RedirectStandardError $daemonLog -PassThru -NoNewWindow
try {
    $dl = (Get-Date).AddSeconds(15); $ready = $false
    while ((Get-Date) -lt $dl) { try { if (Invoke-RestMethod -Uri "$baseURL/health" -TimeoutSec 1) { $ready=$true; break } } catch {}; Start-Sleep -Milliseconds 250 }
    if (-not $ready) { Abort "daemon not ready" }
    Log-Ok "daemon ready"; Start-Sleep -Seconds 3

    $h = @{ Authorization = "Bearer $env:DIGITORN_DEV_JWT"; "X-User-ID"="t"; "Content-Type"="application/json" }
    $bundle = Join-Path $repoRoot "bin\test-apps\lsp-py-probe"
    $ins = Invoke-Rest "POST" "$baseURL/api/apps/install" $h (@{source=$bundle}|ConvertTo-Json)
    if ($ins.app_id -ne "lsp-py-probe") { throw "install: $($ins.app_id)" }
    Log-Ok "installed $($ins.app_id)"

    $s = Invoke-Rest "POST" "$baseURL/api/apps/lsp-py-probe/sessions" $h (@{workdir=($workdir -replace '\\','/')}|ConvertTo-Json)
    $sid = $s.session_id
    Invoke-Rest "POST" "$baseURL/api/apps/lsp-py-probe/sessions/$sid/messages" $h (@{content="Begin now. Follow your instructions exactly."}|ConvertTo-Json) | Out-Null
    Log-Info "polling for pyright diagnostic (max 120s) ..."
    $dl = (Get-Date).AddSeconds(120); $msg=$null
    while ((Get-Date) -lt $dl) {
        Start-Sleep -Milliseconds 800
        try { $hist = Invoke-Rest "GET" "$baseURL/api/apps/lsp-py-probe/sessions/$sid/history" $h $null } catch { continue }
        foreach ($m in $hist.messages) { if ($m.role -eq "assistant" -and $m.content -and ($m.content -match "is not defined" -or $m.content -match "Pyright" -or $m.content -match "not defined")) { $msg=$m; break } }
        if ($msg) { break }
    }
    if (-not $msg) {
        Log-Warn "no pyright diagnostic in reply within 120s. Full history:"
        try { (Invoke-Rest "GET" "$baseURL/api/apps/lsp-py-probe/sessions/$sid/history" $h $null).messages | ForEach-Object { Write-Host "  [$($_.role)] $($_.content)" } } catch {}
        Log-Warn "daemon log (turn/lsp/error):"; Get-Content $daemonLog | Select-String -Pattern "turn failed","lsp","pyright","error" | Select-Object -Last 15 | ForEach-Object { Write-Host "  $_" }
        throw "no proof reply"
    }
    Log-Ok "assistant reply :"; Write-Host ("  REPLY: " + $msg.content) -ForegroundColor White
    Log-Ok "==============================================="
    Log-Ok " LSP PYTHON WORKER E2E : PASS"
    Log-Ok " A real agent edited buggy.py; pyright (in the"
    Log-Ok " digitorn-worker) produced the diagnostic and it"
    Log-Ok " came back through the LLM."
    Log-Ok "==============================================="
}
finally {
    try { Stop-Process -Id $daemon.Id -Force -ErrorAction SilentlyContinue; $daemon | Wait-Process -Timeout 5 -ErrorAction SilentlyContinue } catch {}
}
