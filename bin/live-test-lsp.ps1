# Live LSP worker end-to-end test (GATEWAY mode)
#
# Proves the full chain: a real agent edits a .go file -> the lsp_diagnose hook
# fires -> lsp.notify_change is routed by the daemon to a digitorn-worker
# subprocess (the lsp-pool) -> that worker drives gopls -> gopls's real
# diagnostic comes back through the LLM. The proof token is gopls's exact
# error wording, which the model can only produce by actually reaching gopls.
#
# Usage : powershell -File bin\live-test-lsp.ps1

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Log-Info ($msg) { Write-Host "[live-lsp] $msg" -ForegroundColor Cyan }
function Log-Ok   ($msg) { Write-Host "[live-lsp] $msg" -ForegroundColor Green }
function Log-Warn ($msg) { Write-Host "[live-lsp] $msg" -ForegroundColor Yellow }
function Log-Fail ($msg) { Write-Host "[live-lsp] $msg" -ForegroundColor Red }
function Abort($msg, $code = 1) { Log-Fail $msg; exit $code }

function Invoke-Rest($method, $url, $headers, $body) {
    try {
        if ($null -eq $body) { return Invoke-RestMethod -Method $method -Uri $url -Headers $headers }
        return Invoke-RestMethod -Method $method -Uri $url -Headers $headers -Body $body
    } catch {
        $resp = $_.Exception.Response
        $status = if ($resp) { [int]$resp.StatusCode } else { 0 }
        $respBody = ""
        if ($resp) { try { $respBody = (New-Object System.IO.StreamReader($resp.GetResponseStream())).ReadToEnd() } catch {} }
        throw "HTTP $status from $method $url : $respBody"
    }
}

$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $repoRoot
Log-Info "repo root : $repoRoot"

if (-not $env:DIGITORN_DEV_JWT) {
    $credPath = Join-Path $env:USERPROFILE ".digitorn\credentials.json"
    if (Test-Path $credPath) {
        $cred = Get-Content $credPath -Raw | ConvertFrom-Json
        if ($cred.access_token) { $env:DIGITORN_DEV_JWT = $cred.access_token }
    }
}
if (-not $env:DIGITORN_DEV_JWT) { Abort "no JWT : set DIGITORN_DEV_JWT or populate ~/.digitorn/credentials.json" }
Log-Ok "JWT present (length=$($env:DIGITORN_DEV_JWT.Length))"

if (-not (Get-Command gopls -ErrorAction SilentlyContinue)) { Abort "gopls not on PATH (go install golang.org/x/tools/gopls@latest)" }
Log-Ok "gopls present"

$gatewayURL = $env:DIGITORN_GATEWAY_URL
if (-not $gatewayURL) { $gatewayURL = "http://127.0.0.1:8002/v1" }
Log-Info "gateway : $gatewayURL"

$bundleDir = Join-Path $repoRoot "bin\test-apps\lsp-probe"
if (-not (Test-Path (Join-Path $bundleDir "app.yaml"))) { Abort "bundle not found at $bundleDir" }

Log-Info "building digitornd / digitorn-worker-llm / digitorn-worker ..."
& go build -o (Join-Path $repoRoot "bin\digitornd.exe") "./cmd/digitornd"; if ($LASTEXITCODE -ne 0) { Abort "build digitornd failed" }
& go build -o (Join-Path $repoRoot "bin\digitorn-worker-llm.exe") "./cmd/digitorn-worker-llm"; if ($LASTEXITCODE -ne 0) { Abort "build worker-llm failed" }
& go build -o (Join-Path $repoRoot "bin\digitorn-worker.exe") "./cmd/digitorn-worker"; if ($LASTEXITCODE -ne 0) { Abort "build digitorn-worker failed" }
Log-Ok "binaries built"

$base = "C:\Users\ASUS\AppData\Local\Temp\digitorn-live-lsp"
if (Test-Path $base) { Remove-Item -Recurse -Force $base }
New-Item -ItemType Directory -Force -Path (Join-Path $base "sessions") | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $base "apps") | Out-Null
$workdir = Join-Path $base "ws"
New-Item -ItemType Directory -Force -Path $workdir | Out-Null
# Pre-seed the workspace with a go.mod so gopls has a real module to analyze.
# WriteAllText emits UTF-8 WITHOUT a BOM — a BOM makes `go list` reject go.mod
# ("unexpected input character '﻿'"), which PowerShell's -Encoding utf8 adds.
[System.IO.File]::WriteAllText((Join-Path $workdir "go.mod"), "module probe`n`ngo 1.21`n")
Log-Ok "workdir seeded with go.mod : $workdir"

$daemonLog = Join-Path $base "daemon.log"
$configPath = Join-Path $repoRoot "bin\config-live-lsp.yaml"
$baseURL = "http://127.0.0.1:28002"
$env:DIGITORN_WORKERS__LLM__GATEWAY_URL = $gatewayURL

Log-Info "starting daemon ..."
$daemon = Start-Process -FilePath (Join-Path $repoRoot "bin\digitornd.exe") `
    -ArgumentList "-config", $configPath `
    -RedirectStandardOutput $daemonLog `
    -RedirectStandardError (Join-Path $base "daemon.err.log") `
    -PassThru -NoNewWindow

$exitCode = 1
try {
    Log-Info "waiting for daemon ..."
    $deadline = (Get-Date).AddSeconds(15); $ready = $false
    while ((Get-Date) -lt $deadline) {
        try { if (Invoke-RestMethod -Uri "$baseURL/health" -TimeoutSec 1 -ErrorAction Stop) { $ready = $true; break } } catch {}
        Start-Sleep -Milliseconds 250
    }
    if (-not $ready) { Get-Content $daemonLog -Tail 50 | ForEach-Object { Write-Host "  $_" }; throw "daemon not ready" }
    Log-Ok "daemon ready"
    # Give the lsp worker pool a moment to spawn (it starts in the background).
    Start-Sleep -Seconds 3

    $headers = @{ "Authorization" = "Bearer $env:DIGITORN_DEV_JWT"; "X-User-ID" = "live-test-user"; "Content-Type" = "application/json" }

    Log-Info "installing lsp-probe ..."
    $installResp = Invoke-Rest "POST" "$baseURL/api/apps/install" $headers (@{ source = $bundleDir } | ConvertTo-Json)
    if ($installResp.app_id -ne "lsp-probe") { throw "install : app_id=$($installResp.app_id)" }
    Log-Ok "installed app_id=$($installResp.app_id)"

    $sessBody = @{ workdir = ($workdir -replace '\\','/') } | ConvertTo-Json
    $sessResp = Invoke-Rest "POST" "$baseURL/api/apps/lsp-probe/sessions" $headers $sessBody
    $sid = $sessResp.session_id
    if (-not $sid) { throw "no session_id" }
    Log-Ok "session_id=$sid workdir=$($sessResp.workdir)"

    $question = "Begin now. Follow your instructions exactly."
    $msgResp = Invoke-Rest "POST" "$baseURL/api/apps/lsp-probe/sessions/$sid/messages" $headers (@{ content = $question } | ConvertTo-Json)
    Log-Ok "message persisted seq=$($msgResp.seq) - turn kicked"

    Log-Info "polling history for the gopls diagnostic (max 120s) ..."
    $deadline = (Get-Date).AddSeconds(120); $assistantMsg = $null
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 800
        try { $hist = Invoke-Rest "GET" "$baseURL/api/apps/lsp-probe/sessions/$sid/history" $headers $null } catch { continue }
        if ($hist.messages) {
            foreach ($m in $hist.messages) {
                if ($m.role -eq "assistant" -and $m.content -and ($m.content -match "cannot use" -or $m.content -match "as int value")) { $assistantMsg = $m; break }
            }
        }
        if ($assistantMsg) { break }
    }

    Log-Info "--- daemon log : lsp worker pool + hook evidence ---"
    Get-Content $daemonLog | Select-String -Pattern "lsp-pool","lsp","notify_change","worker pool","ProxyModule","hook" | Select-Object -First 20 | ForEach-Object { Write-Host "  $_" }

    if (-not $assistantMsg) {
        Log-Fail "no assistant reply carrying a gopls diagnostic within 120s"
        Log-Warn "full history :"
        try { (Invoke-Rest "GET" "$baseURL/api/apps/lsp-probe/sessions/$sid/history" $headers $null).messages | ForEach-Object { Write-Host "  [$($_.role)] $($_.content)" } } catch {}
        Log-Warn "daemon log tail :"; Get-Content $daemonLog -Tail 80 | ForEach-Object { Write-Host "  $_" }
        throw "no proof reply"
    }

    Log-Ok "assistant reply :"
    Write-Host ("  REPLY: " + $assistantMsg.content) -ForegroundColor White
    Log-Ok "==============================================="
    Log-Ok " LSP WORKER E2E : PASS"
    Log-Ok " A real agent edited main.go; the diagnostic was"
    Log-Ok " produced by gopls inside the digitorn-worker"
    Log-Ok " (lsp-pool) and returned through the LLM."
    Log-Ok "==============================================="
    $exitCode = 0
}
finally {
    Log-Info "stopping daemon (pid=$($daemon.Id))"
    try { Stop-Process -Id $daemon.Id -Force -ErrorAction SilentlyContinue; $daemon | Wait-Process -Timeout 5 -ErrorAction SilentlyContinue } catch {}
}
exit $exitCode
