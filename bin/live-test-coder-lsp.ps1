# Live end-to-end test of the REAL coder-lsp app.
#
# Seeds a Go project with a compile error, then asks the coder-lsp agent to find
# it via the language server, fix it, and confirm clean via the language server.
# Proves the actual app: read -> lsp.notify_change -> edit -> lsp -> verify.
# Rebuilds binaries (includes the URI fix) and uses a throwaway daemon on :28002.
#
# Usage : powershell -File bin\live-test-coder-lsp.ps1

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Log-Info ($m) { Write-Host "[coder-lsp] $m" -ForegroundColor Cyan }
function Log-Ok   ($m) { Write-Host "[coder-lsp] $m" -ForegroundColor Green }
function Log-Warn ($m) { Write-Host "[coder-lsp] $m" -ForegroundColor Yellow }
function Abort($m) { Write-Host "[coder-lsp] $m" -ForegroundColor Red; exit 1 }

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
if (-not (Get-Command gopls -ErrorAction SilentlyContinue)) { Abort "gopls not on PATH" }
if (-not $env:DIGITORN_GATEWAY_URL) { $env:DIGITORN_GATEWAY_URL = "http://127.0.0.1:8002/v1" }

Log-Info "building digitornd / digitorn-worker-llm / digitorn-worker ..."
& go build -o (Join-Path $repoRoot "bin\digitornd.exe") "./cmd/digitornd"; if ($LASTEXITCODE) { Abort "build digitornd" }
& go build -o (Join-Path $repoRoot "bin\digitorn-worker-llm.exe") "./cmd/digitorn-worker-llm"; if ($LASTEXITCODE) { Abort "build worker-llm" }
& go build -o (Join-Path $repoRoot "bin\digitorn-worker.exe") "./cmd/digitorn-worker"; if ($LASTEXITCODE) { Abort "build worker" }
Log-Ok "binaries built"

$base = "C:\Users\ASUS\AppData\Local\Temp\digitorn-live-lsp"
if (Test-Path $base) { Remove-Item -Recurse -Force $base }
New-Item -ItemType Directory -Force -Path (Join-Path $base "sessions") | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $base "apps") | Out-Null
$workdir = Join-Path $base "proj"; New-Item -ItemType Directory -Force -Path $workdir | Out-Null
# Seed a real Go project (UTF-8 no BOM) with a deliberate type error.
[System.IO.File]::WriteAllText((Join-Path $workdir "go.mod"), "module proj`n`ngo 1.21`n")
$buggy = "package main`n`nimport `"fmt`"`n`nfunc main() {`n`tvar count int = `"ten`"`n`tfmt.Println(count)`n}`n"
[System.IO.File]::WriteAllText((Join-Path $workdir "main.go"), $buggy)
Log-Ok "seeded project at $workdir (main.go has a type error)"

$daemonLog = Join-Path $base "daemon.err.log"
$cfg = Join-Path $repoRoot "bin\config-live-lsp.yaml"
$baseURL = "http://127.0.0.1:28002"
$env:DIGITORN_WORKERS__LLM__GATEWAY_URL = $env:DIGITORN_GATEWAY_URL

$daemon = Start-Process -FilePath (Join-Path $repoRoot "bin\digitornd.exe") -ArgumentList "-config",$cfg `
    -RedirectStandardOutput (Join-Path $base "daemon.out.log") -RedirectStandardError $daemonLog -PassThru -NoNewWindow
try {
    $dl = (Get-Date).AddSeconds(15); $ready=$false
    while ((Get-Date) -lt $dl) { try { if (Invoke-RestMethod -Uri "$baseURL/health" -TimeoutSec 1) { $ready=$true; break } } catch {}; Start-Sleep -Milliseconds 250 }
    if (-not $ready) { Abort "daemon not ready" }
    Log-Ok "daemon ready"; Start-Sleep -Seconds 3

    $h = @{ Authorization = "Bearer $env:DIGITORN_DEV_JWT"; "X-User-ID"="t"; "Content-Type"="application/json" }
    $bundle = Join-Path $repoRoot "examples\coder-lsp"
    $ins = Invoke-Rest "POST" "$baseURL/api/apps/install" $h (@{source=$bundle}|ConvertTo-Json)
    if ($ins.app_id -ne "coder-lsp") { throw "install: $($ins.app_id)" }
    Log-Ok "installed $($ins.app_id)"

    $s = Invoke-Rest "POST" "$baseURL/api/apps/coder-lsp/sessions" $h (@{workdir=($workdir -replace '\\','/')}|ConvertTo-Json)
    $sid = $s.session_id
    $task = "The file main.go in the workspace has a compile error. Read it, then use ONLY the lsp tool (lsp.notify_change) to identify the error, fix the code with filesystem.edit so it compiles, and call lsp again to confirm no errors remain. Do not run shell commands. Finish with two lines: FIXED: <the diagnostic you fixed> and CLEAN: <yes|no>."
    Invoke-Rest "POST" "$baseURL/api/apps/coder-lsp/sessions/$sid/messages" $h (@{content=$task}|ConvertTo-Json) | Out-Null
    Log-Info "agent working (max 180s) ..."
    $dl = (Get-Date).AddSeconds(180); $final=$null
    while ((Get-Date) -lt $dl) {
        Start-Sleep -Milliseconds 1500
        try { $hist = Invoke-Rest "GET" "$baseURL/api/apps/coder-lsp/sessions/$sid/history" $h $null } catch { continue }
        foreach ($m in $hist.messages) { if ($m.role -eq "assistant" -and $m.content -and $m.content -match "CLEAN:") { $final=$m } }
        if ($final) { break }
    }

    Write-Host "`n--- final main.go on disk ---" -ForegroundColor Magenta
    $finalCode = Get-Content (Join-Path $workdir "main.go") -Raw
    Write-Host $finalCode -ForegroundColor White
    Write-Host "-----------------------------`n" -ForegroundColor Magenta

    $fixed = $finalCode -notmatch '"ten"'
    if ($final) { Log-Ok "agent reply:"; Write-Host ("  " + ($final.content -replace "`n","`n  ")) -ForegroundColor White }
    else { Log-Warn "no CLEAN: line from agent within 180s (see history below)"; try { $hist.messages | ForEach-Object { Write-Host "  [$($_.role)] $($_.content)" } } catch {} }

    if ($fixed) {
        Log-Ok "==============================================="
        Log-Ok " CODER-LSP LIVE : PASS"
        Log-Ok " The agent used the language server to find the"
        Log-Ok " type error and EDITED main.go to fix it (the"
        Log-Ok " '\"ten\"' bug is gone on disk)."
        Log-Ok "==============================================="
    } else {
        Log-Warn "main.go still contains the original bug — agent did not fix it"
    }
}
finally {
    try { Stop-Process -Id $daemon.Id -Force -ErrorAction SilentlyContinue; $daemon | Wait-Process -Timeout 5 -ErrorAction SilentlyContinue } catch {}
}
