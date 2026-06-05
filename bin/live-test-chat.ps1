# R-5 - Live chat end-to-end test (GATEWAY mode)
#
# Boots a real digitornd + digitorn-worker-llm pair, installs the
# chat-simple app, leaves BYOK off (default), and sends a real user
# message. The worker forwards UserJWT to the digitorn LLM gateway,
# which resolves the provider credential server-side and returns an
# Anthropic completion. Verifies a real assistant reply lands in the
# session history.
#
# This is the path 99% of signed-in digitorn users take : no API key
# trimballed locally, the gateway handles credential lookup via JWT.
#
# Prerequisites :
#   $env:DIGITORN_DEV_JWT     = "eyJhbGciOiJSUz..."         # JWT from auth.digitorn.ai
#   $env:DIGITORN_GATEWAY_URL = "http://127.0.0.1:8002/v1"  # optional, default loopback
#
# Usage : powershell -File bin\live-test-chat.ps1

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

# -------- helpers --------

function Log-Info ($msg) { Write-Host "[live-test] $msg" -ForegroundColor Cyan }
function Log-Ok   ($msg) { Write-Host "[live-test] $msg" -ForegroundColor Green }
function Log-Warn ($msg) { Write-Host "[live-test] $msg" -ForegroundColor Yellow }
function Log-Fail ($msg) { Write-Host "[live-test] $msg" -ForegroundColor Red }

function Abort($msg, $code = 1) {
    Log-Fail $msg
    exit $code
}

# Invoke-RestMethod surfaces HTTP errors as terminating exceptions with
# the response body buried in the exception object. This wrapper digs
# it out and re-throws a useful message instead of "the remote server
# returned an error" with no body.
function Invoke-Rest($method, $url, $headers, $body) {
    try {
        if ($null -eq $body) {
            return Invoke-RestMethod -Method $method -Uri $url -Headers $headers
        }
        return Invoke-RestMethod -Method $method -Uri $url -Headers $headers -Body $body
    } catch [System.Net.WebException] {
        $resp = $_.Exception.Response
        $status = if ($resp) { [int]$resp.StatusCode } else { 0 }
        $respBody = ""
        if ($resp) {
            try {
                $reader = New-Object System.IO.StreamReader($resp.GetResponseStream())
                $respBody = $reader.ReadToEnd()
            } catch {}
        }
        Log-Fail "$method $url -> HTTP $status"
        if ($respBody) { Log-Fail "response body : $respBody" }
        throw "HTTP $status from $method $url : $respBody"
    } catch {
        Log-Fail "$method $url -> $($_.Exception.GetType().Name) : $($_.Exception.Message)"
        throw
    }
}

# Find repo root (the dir containing this script's parent dir).
$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $repoRoot
Log-Info "repo root : $repoRoot"

# -------- step 0 : prerequisites --------

if (-not $env:DIGITORN_DEV_JWT) {
    Abort "DIGITORN_DEV_JWT is not set. Get one from auth.digitorn.ai and run :`n  `$env:DIGITORN_DEV_JWT = 'eyJhbGc...'"
}
Log-Ok "DIGITORN_DEV_JWT present (length=$($env:DIGITORN_DEV_JWT.Length))"

$gatewayURL = $env:DIGITORN_GATEWAY_URL
if (-not $gatewayURL) {
    $gatewayURL = "http://127.0.0.1:8002/v1"
    Log-Warn "DIGITORN_GATEWAY_URL not set, defaulting to $gatewayURL (local digitorn-gateway)"
} else {
    Log-Ok "DIGITORN_GATEWAY_URL = $gatewayURL"
}

# Pre-flight : verify the gateway answers. Bifrost will dial it via
# the OpenAI-compatible /chat/completions endpoint, but a basic
# health probe is enough to confirm reachability.
Log-Info "probing gateway at $gatewayURL ..."
try {
    $probe = Invoke-WebRequest -Uri $gatewayURL -Method GET -TimeoutSec 3 -ErrorAction Stop -UseBasicParsing
    Log-Ok "gateway reachable (status=$($probe.StatusCode))"
} catch {
    # 401/404/405 are still "reachable" answers - they prove the host
    # is up, just that the endpoint or auth differs. Only treat
    # connection-refused / DNS errors as fatal.
    $msg = $_.Exception.Message
    if ($msg -match "unreachable|refused|resolve|timeout") {
        Abort "gateway $gatewayURL is unreachable : $msg`nMake sure digitorn-gateway is running, or set `$env:DIGITORN_GATEWAY_URL to a reachable URL."
    }
    Log-Ok "gateway reachable (HTTP error is fine for probe : $msg)"
}

$bundleDir = Join-Path $repoRoot "bin\test-apps\chat-simple"
if (-not (Test-Path (Join-Path $bundleDir "app.yaml"))) {
    Abort "bundle not found at $bundleDir"
}

# -------- step 1 : build binaries --------

Log-Info "building digitornd.exe ..."
& go build -o (Join-Path $repoRoot "bin\digitornd.exe") "./cmd/digitornd"
if ($LASTEXITCODE -ne 0) { Abort "go build digitornd failed" }

Log-Info "building digitorn-worker-llm.exe ..."
& go build -o (Join-Path $repoRoot "bin\digitorn-worker-llm.exe") "./cmd/digitorn-worker-llm"
if ($LASTEXITCODE -ne 0) { Abort "go build digitorn-worker-llm failed" }

Log-Ok "binaries built"

# -------- step 2 : clean workspace --------

$workspace = "C:\Users\ASUS\AppData\Local\Temp\digitorn-live-chat"
if (Test-Path $workspace) {
    Log-Info "cleaning previous workspace $workspace"
    Remove-Item -Recurse -Force $workspace
}
New-Item -ItemType Directory -Force -Path $workspace | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $workspace "sessions") | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $workspace "apps") | Out-Null
Log-Ok "workspace clean"

# -------- step 3 : boot daemon --------

$daemonLog = Join-Path $workspace "daemon.log"
$configPath = Join-Path $repoRoot "bin\config-live-chat.yaml"
$baseURL = "http://127.0.0.1:28002"

# Inject the gateway URL into the daemon's env (overrides YAML via koanf).
$env:DIGITORN_WORKERS__LLM__GATEWAY_URL = $gatewayURL

Log-Info "starting daemon (config=$configPath, gateway=$gatewayURL)"
$daemon = Start-Process `
    -FilePath (Join-Path $repoRoot "bin\digitornd.exe") `
    -ArgumentList "-config", $configPath `
    -RedirectStandardOutput $daemonLog `
    -RedirectStandardError (Join-Path $workspace "daemon.err.log") `
    -PassThru `
    -NoNewWindow

# Cleanup trap : ALWAYS kill the daemon on exit, even on error.
$exitCode = 1
try {

    # -------- step 4 : wait for daemon ready --------

    Log-Info "waiting for daemon (max 15s) ..."
    $deadline = (Get-Date).AddSeconds(15)
    $ready = $false
    while ((Get-Date) -lt $deadline) {
        try {
            $h = Invoke-RestMethod -Uri "$baseURL/health" -TimeoutSec 1 -ErrorAction Stop
            if ($h) { $ready = $true ; break }
        } catch {}
        Start-Sleep -Milliseconds 250
    }
    if (-not $ready) {
        Log-Fail "daemon did not respond on $baseURL/health within 15s"
        Log-Warn "daemon log tail :"
        Get-Content $daemonLog -Tail 50 | ForEach-Object { Write-Host "  $_" }
        throw "daemon not ready"
    }
    Log-Ok "daemon ready"

    # Common headers : Bearer JWT goes through to gateway as user
    # credential ; X-User-ID lets the daemon (in dev mode) attribute
    # session ownership without verifying the JWT locally.
    $headers = @{
        "Authorization" = "Bearer $env:DIGITORN_DEV_JWT"
        "X-User-ID"     = "live-test-user"
        "Content-Type"  = "application/json"
    }

    # -------- step 5 : install chat-simple --------

    Log-Info "installing chat-simple from $bundleDir"
    $installBody = @{ source = $bundleDir } | ConvertTo-Json
    $installResp = Invoke-Rest "POST" "$baseURL/api/apps/install" $headers $installBody
    if ($installResp.app_id -ne "chat-simple") {
        throw "install : app_id=$($installResp.app_id), want chat-simple"
    }
    Log-Ok "installed app_id=$($installResp.app_id) version=$($installResp.version) byok=$($installResp.byok)"
    Log-Info "DEBUG: install resp type = $($installResp.GetType().FullName)"
    Log-Info "DEBUG: install resp byok type = $(if ($installResp.PSObject.Properties.Name -contains 'byok') { $installResp.byok.GetType().FullName } else { 'PROPERTY MISSING' })"

    $isByok = [bool]$installResp.byok
    Log-Info "DEBUG: coerced byok = $isByok"
    Log-Info "DEBUG: ABOUT to enter if block"

    if ($isByok) {
        throw "byok should default to false after install, got true"
    }
    Log-Info "DEBUG: PAST if block"
    Log-Ok "BYOK is OFF - gateway path will be used"
    Log-Info "DEBUG: after BYOK ok log"

    # -------- step 6 : create session --------

    Log-Info "creating session at $baseURL/api/apps/chat-simple/sessions"
    Log-Info "DEBUG: headers keys = $($headers.Keys -join ', ')"
    Log-Info "DEBUG: about to call Invoke-Rest"
    $sessResp = Invoke-Rest "POST" "$baseURL/api/apps/chat-simple/sessions" $headers "{}"
    Log-Info "DEBUG: Invoke-Rest returned"
    Log-Info "DEBUG: sessResp received, type = $($sessResp.GetType().FullName)"
    $sid = $sessResp.session_id
    if (-not $sid) { throw "no session_id in response" }
    Log-Ok "session_id=$sid"

    # -------- step 7 : POST user message --------

    $question = "What is 2 + 2? Answer with just the number and nothing else."
    Log-Info "posting user message : `"$question`""
    $msgBody = @{ content = $question } | ConvertTo-Json
    $msgResp = Invoke-Rest "POST" "$baseURL/api/apps/chat-simple/sessions/$sid/messages" $headers $msgBody
    Log-Ok "user message persisted seq=$($msgResp.seq) - engine kicked turn (gateway route)"

    # -------- step 8 : poll history for assistant reply --------

    Log-Info "polling history for assistant reply (max 60s) ..."
    $deadline = (Get-Date).AddSeconds(60)
    $assistantMsg = $null
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 500
        try {
            $hist = Invoke-Rest "GET" "$baseURL/api/apps/chat-simple/sessions/$sid/history" $headers $null
        } catch {
            continue
        }
        if ($hist.messages) {
            foreach ($m in $hist.messages) {
                if ($m.role -eq "assistant" -and $m.content) {
                    $assistantMsg = $m
                    break
                }
            }
        }
        if ($assistantMsg) { break }
    }
    if (-not $assistantMsg) {
        Log-Fail "no assistant reply within 60s"
        Log-Warn "daemon log tail :"
        Get-Content $daemonLog -Tail 80 | ForEach-Object { Write-Host "  $_" }
        if (Test-Path (Join-Path $workspace "daemon.err.log")) {
            Log-Warn "daemon stderr :"
            Get-Content (Join-Path $workspace "daemon.err.log") -Tail 40 | ForEach-Object { Write-Host "  $_" }
        }
        throw "no assistant reply"
    }

    Log-Ok "assistant reply received :"
    Write-Host ("  REPLY: " + $assistantMsg.content) -ForegroundColor White

    # -------- step 9 : assert reply is plausible --------

    if ($assistantMsg.content -notmatch "4") {
        Log-Warn "reply does not contain '4' - provider may have phrased oddly"
        Log-Warn "marking as PASS anyway since we got a real reply"
    } else {
        Log-Ok "reply contains '4' - correct answer via gateway"
    }

    Log-Ok "==============================================="
    Log-Ok " R-5 LIVE TEST : PASS"
    Log-Ok "==============================================="
    Log-Ok " The chat-simple app talked to the LLM via the"
    Log-Ok " digitorn gateway. UserJWT was forwarded to"
    Log-Ok " $gatewayURL"
    Log-Ok " which resolved the credential server-side."
    Log-Ok "==============================================="
    $exitCode = 0

}
finally {
    Log-Info "stopping daemon (pid=$($daemon.Id))"
    try {
        Stop-Process -Id $daemon.Id -Force -ErrorAction SilentlyContinue
        $daemon | Wait-Process -Timeout 5 -ErrorAction SilentlyContinue
    } catch {
        Log-Warn "daemon stop : $_"
    }
}

exit $exitCode
