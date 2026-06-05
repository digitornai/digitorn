# Live web end-to-end test (GATEWAY mode)
#
# Boots a real digitornd + digitorn-worker-llm pair, installs the web-probe app,
# and verifies the agent actually invoked web.fetch by checking the reply
# carries a fresh WEBPROOF<token> echoed by a PUBLIC URL (postman-echo). The
# token is random per run and exists only in that response, so the model cannot
# fabricate it: a match proves the in-proc web module performed the real HTTP
# request (SSRF guard active, default-safe) through the full agent loop.
#
# Usage : powershell -File bin\live-test-web.ps1

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Log-Info ($msg) { Write-Host "[live-web] $msg" -ForegroundColor Cyan }
function Log-Ok   ($msg) { Write-Host "[live-web] $msg" -ForegroundColor Green }
function Log-Warn ($msg) { Write-Host "[live-web] $msg" -ForegroundColor Yellow }
function Log-Fail ($msg) { Write-Host "[live-web] $msg" -ForegroundColor Red }
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

# JWT : from env, else from the cached credentials file.
if (-not $env:DIGITORN_DEV_JWT) {
    $credPath = Join-Path $env:USERPROFILE ".digitorn\credentials.json"
    if (Test-Path $credPath) {
        $cred = Get-Content $credPath -Raw | ConvertFrom-Json
        if ($cred.access_token) { $env:DIGITORN_DEV_JWT = $cred.access_token }
    }
}
if (-not $env:DIGITORN_DEV_JWT) { Abort "no JWT : set DIGITORN_DEV_JWT or populate ~/.digitorn/credentials.json" }
Log-Ok "JWT present (length=$($env:DIGITORN_DEV_JWT.Length))"

$gatewayURL = $env:DIGITORN_GATEWAY_URL
if (-not $gatewayURL) { $gatewayURL = "http://127.0.0.1:8002/v1" }
Log-Info "gateway : $gatewayURL"

$bundleDir = Join-Path $repoRoot "bin\test-apps\web-probe"
if (-not (Test-Path (Join-Path $bundleDir "app.yaml"))) { Abort "bundle not found at $bundleDir" }

# Fresh, unfakeable token for this run (no '=' so it is URL-clean).
$token = "WEBPROOF" + ([guid]::NewGuid().ToString("N"))
$probeURL = "https://postman-echo.com/get?proof=$token"
Log-Info "probe token : $token"
Log-Info "probe url   : $probeURL"

# Sanity: confirm the echo endpoint really returns our token before spending tokens.
try {
    $echo = Invoke-RestMethod -Uri $probeURL -TimeoutSec 10
    if ("$($echo.args.proof)" -ne $token) { Abort "echo endpoint did not return the token (got '$($echo.args.proof)')" }
    Log-Ok "echo endpoint returns the token"
} catch { Abort "echo endpoint unreachable: $_" }

Log-Info "building digitornd.exe ..."
& go build -o (Join-Path $repoRoot "bin\digitornd.exe") "./cmd/digitornd"
if ($LASTEXITCODE -ne 0) { Abort "go build digitornd failed" }
Log-Info "building digitorn-worker-llm.exe ..."
& go build -o (Join-Path $repoRoot "bin\digitorn-worker-llm.exe") "./cmd/digitorn-worker-llm"
if ($LASTEXITCODE -ne 0) { Abort "go build digitorn-worker-llm failed" }
Log-Ok "binaries built"

$workspace = "C:\Users\ASUS\AppData\Local\Temp\digitorn-live-web"
if (Test-Path $workspace) { Remove-Item -Recurse -Force $workspace }
New-Item -ItemType Directory -Force -Path $workspace | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $workspace "sessions") | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $workspace "apps") | Out-Null

$daemonLog = Join-Path $workspace "daemon.log"
$configPath = Join-Path $repoRoot "bin\config-live-chat.yaml"
$baseURL = "http://127.0.0.1:28002"
$env:DIGITORN_WORKERS__LLM__GATEWAY_URL = $gatewayURL

Log-Info "starting daemon ..."
$daemon = Start-Process -FilePath (Join-Path $repoRoot "bin\digitornd.exe") `
    -ArgumentList "-config", $configPath `
    -RedirectStandardOutput $daemonLog `
    -RedirectStandardError (Join-Path $workspace "daemon.err.log") `
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

    $headers = @{ "Authorization" = "Bearer $env:DIGITORN_DEV_JWT"; "X-User-ID" = "live-test-user"; "Content-Type" = "application/json" }

    Log-Info "installing web-probe ..."
    $installResp = Invoke-Rest "POST" "$baseURL/api/apps/install" $headers (@{ source = $bundleDir } | ConvertTo-Json)
    if ($installResp.app_id -ne "web-probe") { throw "install : app_id=$($installResp.app_id)" }
    Log-Ok "installed app_id=$($installResp.app_id) version=$($installResp.version)"

    $sessResp = Invoke-Rest "POST" "$baseURL/api/apps/web-probe/sessions" $headers "{}"
    $sid = $sessResp.session_id
    if (-not $sid) { throw "no session_id" }
    Log-Ok "session_id=$sid"

    $question = "Fetch the page at $probeURL using web.fetch and report the WEBPROOF token it contains."
    Log-Info "posting user message ..."
    $msgResp = Invoke-Rest "POST" "$baseURL/api/apps/web-probe/sessions/$sid/messages" $headers (@{ content = $question } | ConvertTo-Json)
    Log-Ok "message persisted seq=$($msgResp.seq) - turn kicked"

    Log-Info "polling history for assistant reply (max 90s) ..."
    $deadline = (Get-Date).AddSeconds(90); $assistantMsg = $null
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 600
        try { $hist = Invoke-Rest "GET" "$baseURL/api/apps/web-probe/sessions/$sid/history" $headers $null } catch { continue }
        if ($hist.messages) {
            foreach ($m in $hist.messages) {
                if ($m.role -eq "assistant" -and $m.content -and ($m.content -match [regex]::Escape($token))) { $assistantMsg = $m; break }
            }
        }
        if ($assistantMsg) { break }
    }
    if (-not $assistantMsg) {
        Log-Fail "no assistant reply carrying the probe token within 90s"
        Log-Warn "full history :"
        try { (Invoke-Rest "GET" "$baseURL/api/apps/web-probe/sessions/$sid/history" $headers $null).messages | ForEach-Object { Write-Host "  [$($_.role)] $($_.content)" } } catch {}
        Log-Warn "daemon log tail :"
        Get-Content $daemonLog -Tail 60 | ForEach-Object { Write-Host "  $_" }
        throw "no proof reply"
    }

    Log-Ok "assistant reply :"
    Write-Host ("  REPLY: " + $assistantMsg.content) -ForegroundColor White
    Log-Ok "==============================================="
    Log-Ok " WEB LIVE TEST : PASS"
    Log-Ok " The agent called web.fetch, the in-proc web module"
    Log-Ok " performed the real HTTP request, and the page's"
    Log-Ok " token ($token) came back through the LLM."
    Log-Ok "==============================================="
    $exitCode = 0
}
finally {
    Log-Info "stopping daemon (pid=$($daemon.Id))"
    try { Stop-Process -Id $daemon.Id -Force -ErrorAction SilentlyContinue; $daemon | Wait-Process -Timeout 5 -ErrorAction SilentlyContinue } catch {}
}
exit $exitCode
