# Does the agent SEE diagnostics from the lsp_diagnose hook after an edit,
# WITHOUT calling lsp itself? Agent has filesystem only. We dump the full
# history so the answer is objective, not inferred.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Log-Info ($m) { Write-Host "[see] $m" -ForegroundColor Cyan }
function Log-Ok   ($m) { Write-Host "[see] $m" -ForegroundColor Green }
function Log-Warn ($m) { Write-Host "[see] $m" -ForegroundColor Yellow }
function Abort($m) { Write-Host "[see] $m" -ForegroundColor Red; exit 1 }

function Invoke-Rest($method, $url, $headers, $body) {
    try {
        if ($null -eq $body) { return Invoke-RestMethod -Method $method -Uri $url -Headers $headers }
        return Invoke-RestMethod -Method $method -Uri $url -Headers $headers -Body $body
    } catch {
        $resp = $_.Exception.Response
        $rb = ""
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
if (-not $env:DIGITORN_GATEWAY_URL) { $env:DIGITORN_GATEWAY_URL = "http://127.0.0.1:8002/v1" }

& go build -o (Join-Path $repoRoot "bin\digitornd.exe") "./cmd/digitornd"; if ($LASTEXITCODE) { Abort "build digitornd" }
& go build -o (Join-Path $repoRoot "bin\digitorn-worker-llm.exe") "./cmd/digitorn-worker-llm"; if ($LASTEXITCODE) { Abort "build worker-llm" }
& go build -o (Join-Path $repoRoot "bin\digitorn-worker.exe") "./cmd/digitorn-worker"; if ($LASTEXITCODE) { Abort "build worker" }
Log-Ok "built"

$base = "C:\Users\ASUS\AppData\Local\Temp\digitorn-live-lsp-see"
if (Test-Path $base) { Remove-Item -Recurse -Force $base }
New-Item -ItemType Directory -Force -Path (Join-Path $base "sessions") | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $base "apps") | Out-Null
$workdir = Join-Path $base "ws"; New-Item -ItemType Directory -Force -Path $workdir | Out-Null
[System.IO.File]::WriteAllText((Join-Path $workdir "go.mod"), "module probe`n`ngo 1.21`n")

$cfg = Join-Path $repoRoot "bin\config-live-lsp-see.yaml"

$log = Join-Path $base "daemon.err.log"
$baseURL = "http://127.0.0.1:28002"
$d = Start-Process -FilePath (Join-Path $repoRoot "bin\digitornd.exe") -ArgumentList "-config",$cfg `
    -RedirectStandardOutput (Join-Path $base "daemon.out.log") -RedirectStandardError $log -PassThru -NoNewWindow
try {
    $dl = (Get-Date).AddSeconds(15); $ready = $false
    while ((Get-Date) -lt $dl) { try { if (Invoke-RestMethod -Uri "$baseURL/health" -TimeoutSec 1) { $ready=$true; break } } catch {}; Start-Sleep -Milliseconds 250 }
    if (-not $ready) { Abort "daemon not ready" }
    Start-Sleep -Seconds 3
    $h = @{ Authorization = "Bearer $env:DIGITORN_DEV_JWT"; "X-User-ID"="t"; "Content-Type"="application/json" }
    $bundle = Join-Path $repoRoot "bin\test-apps\lsp-see-probe"
    $ins = Invoke-Rest "POST" "$baseURL/api/apps/install" $h (@{source=$bundle}|ConvertTo-Json)
    Log-Ok "installed $($ins.app_id)"
    $s = Invoke-Rest "POST" "$baseURL/api/apps/lsp-see-probe/sessions" $h (@{workdir=($workdir -replace '\\','/')}|ConvertTo-Json)
    $sid = $s.session_id
    Invoke-Rest "POST" "$baseURL/api/apps/lsp-see-probe/sessions/$sid/messages" $h (@{content="Begin."}|ConvertTo-Json) | Out-Null
    Log-Info "waiting for turn to finish (max 120s) ..."
    $dl = (Get-Date).AddSeconds(120); $done=$false
    while ((Get-Date) -lt $dl) {
        Start-Sleep -Milliseconds 1000
        try { $hist = Invoke-Rest "GET" "$baseURL/api/apps/lsp-see-probe/sessions/$sid/history" $h $null } catch { continue }
        foreach ($m in $hist.messages) { if ($m.role -eq "assistant" -and $m.content -match "DIAGNOSTICS:") { $done=$true } }
        if ($done) { break }
    }
    Write-Host "`n================ FULL HISTORY ================" -ForegroundColor Magenta
    foreach ($m in $hist.messages) {
        $c = "$($m.content)"
        if ($c.Length -gt 400) { $c = $c.Substring(0,400) + "..." }
        Write-Host ("[{0}] {1}" -f $m.role, $c) -ForegroundColor White
        if ($m.PSObject.Properties.Name -contains 'tool_calls' -and $m.tool_calls) { Write-Host ("      tool_calls: " + ($m.tool_calls -join ", ")) -ForegroundColor DarkGray }
    }
    Write-Host "================ END HISTORY ================`n" -ForegroundColor Magenta

    $injected = $false
    foreach ($m in $hist.messages) { if ($m.role -ne "assistant" -and "$($m.content)" -match "imported and not used|not used|diagnostic") { $injected = $true } }
    if ($injected) { Log-Ok "VERDICT: a diagnostic message WAS pushed to the agent (non-agent message carries it)" }
    else { Log-Warn "VERDICT: NO diagnostic was pushed by the hook (agent only sees what it fetched itself; here it has no lsp tool)" }

    Write-Host "`n--- daemon log : hook / lsp / error evidence ---" -ForegroundColor DarkCyan
    Get-Content $log | Select-String -Pattern "hook","lsp","notify_change","diagnose","error","warn" | Select-Object -Last 25 | ForEach-Object { Write-Host "  $_" }
}
finally {
    try { Stop-Process -Id $d.Id -Force -ErrorAction SilentlyContinue; $d | Wait-Process -Timeout 5 -ErrorAction SilentlyContinue } catch {}
}
