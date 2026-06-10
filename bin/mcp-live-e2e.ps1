# MCP live end-to-end test - proves a REAL LLM picks and calls an MCP virtual
# tool in a real chat turn, the daemon routes it to a real MCP server subprocess,
# and the result rounds back.
#
# Targets the ALREADY-RUNNING daemon on :8000 (real JWT auth, gateway mode).
# Prereqs: daemon up on :8000, gateway up on :8002, node/npx on PATH,
# ~/.digitorn/credentials.json valid.
#
# Usage: powershell -File bin\mcp-live-e2e.ps1

$ErrorActionPreference = 'Stop'
$base = 'http://127.0.0.1:8000'
$app  = 'mcp-live'
$bundle = Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) 'test-apps\mcp-live'

function Info($m){ Write-Host "[mcp-e2e] $m" -ForegroundColor Cyan }
function Ok($m){ Write-Host "[mcp-e2e] $m" -ForegroundColor Green }
function Warn($m){ Write-Host "[mcp-e2e] $m" -ForegroundColor Yellow }
function Fail($m){ Write-Host "[mcp-e2e] $m" -ForegroundColor Red }

function Rest($method,$url,$headers,$body){
  try {
    if ($null -eq $body) { return Invoke-RestMethod -Method $method -Uri $url -Headers $headers -TimeoutSec 30 }
    return Invoke-RestMethod -Method $method -Uri $url -Headers $headers -Body $body -TimeoutSec 30
  } catch {
    $resp = $_.Exception.Response
    $code = if ($resp) { [int]$resp.StatusCode } else { 0 }
    $rb = ''
    if ($resp) { try { $rb = (New-Object System.IO.StreamReader($resp.GetResponseStream())).ReadToEnd() } catch {} }
    throw "HTTP $code from $method $url : $rb"
  }
}

# --- JWT ---
$cred = Get-Content (Join-Path $env:USERPROFILE '.digitorn\credentials.json') -Raw | ConvertFrom-Json
$tok = $cred.access_token
if (-not $tok) { Fail 'no access_token in credentials.json'; exit 1 }
$H = @{ Authorization = "Bearer $tok"; 'Content-Type' = 'application/json' }
Ok "JWT loaded (len=$($tok.Length))"

# --- gateway pre-flight (warn-only) ---
try { Invoke-WebRequest 'http://127.0.0.1:8002/v1' -Method GET -TimeoutSec 3 -UseBasicParsing -ErrorAction Stop | Out-Null; Ok 'gateway :8002 reachable' }
catch {
  if ($_.Exception.Message -match 'refused|unreachable|resolve') { Warn 'gateway :8002 NOT reachable - the LLM turn will fail until it is up' }
  else { Ok 'gateway :8002 reachable (HTTP error on probe is fine)' }
}

# --- install ---
Info "installing $app from $bundle"
$inst = Rest 'POST' "$base/api/apps/install" $H (@{ source = $bundle } | ConvertTo-Json)
Ok "installed app_id=$($inst.app_id) version=$($inst.version)"

# --- session ---
$sess = Rest 'POST' "$base/api/apps/$app/sessions" $H '{}'
$sid = $sess.session_id
if (-not $sid) { Fail 'no session_id'; exit 1 }
Ok "session_id=$sid"

# --- post message (unique sentinel so we never match a stale run) ---
$sentinel = "MCP-LIVE-OK-$(Get-Random -Minimum 1000 -Maximum 9999)"
$question = "Use the echo MCP tool to echo back this exact string: $sentinel . Then tell me verbatim what the tool returned."
Info "sentinel = $sentinel"
Info 'posting user message'
$msg = Rest 'POST' "$base/api/apps/$app/sessions/$sid/messages" $H (@{ content = $question } | ConvertTo-Json)
Ok "message persisted seq=$($msg.seq) - turn kicked"

# --- poll events for the mcp tool_call + sentinel round-trip (npx first-run can be slow) ---
# Rigorous proof, NOT a substring match on the whole blob:
#  - toolCallSeen : an event whose type/kind mentions "tool" AND references mcp_everything.
#  - echoRoundTrip: the sentinel appears in a NON-user_message event (i.e. it came BACK
#    through the MCP server, not just our outgoing prompt).
#  - turnError    : surface the turn's error reason instead of waiting blind.
Info 'polling /events for the MCP tool_call + echoed sentinel (max 180s) ...'
$deadline = (Get-Date).AddSeconds(180)
$toolCallSeen = $false
$echoRoundTrip = $false
$replySeen = $false
$reply = ''
$turnError = ''
$rawEventsPath = Join-Path $env:TEMP "mcp-e2e-events-$sid.json"
while ((Get-Date) -lt $deadline) {
  Start-Sleep -Milliseconds 1500
  try { $ev = Rest 'GET' "$base/api/apps/$app/sessions/$sid/events" $H $null } catch { continue }
  Set-Content -Path $rawEventsPath -Value ($ev | ConvertTo-Json -Depth 30) -Encoding utf8
  foreach ($e in $ev.events) {
    $tk = "$($e.type) $($e.kind)"
    $blob = ($e.payload | ConvertTo-Json -Depth 20 -Compress)
    if ($tk -match 'tool' -and $blob -match 'mcp_everything') { $toolCallSeen = $true }
    if ($e.type -ne 'user_message' -and $blob -match [regex]::Escape($sentinel)) { $echoRoundTrip = $true }
    if ($e.type -eq 'assistant_message' -and $e.payload.content) { $replySeen = $true; $reply = $e.payload.content }
    if ($e.type -eq 'error' -or ($e.payload.phase -eq 'errored')) { if ($e.payload.reason) { $turnError = $e.payload.reason } elseif ($e.payload.message) { $turnError = $e.payload.message } }
  }
  if (-not $replySeen) {
    try { $hist = Rest 'GET' "$base/api/apps/$app/sessions/$sid/history" $H $null } catch {}
    if ($hist.messages) { foreach($m in $hist.messages){ if ($m.role -eq 'assistant' -and $m.content){ $replySeen = $true; $reply = $m.content } } }
  }
  if ($toolCallSeen -and $echoRoundTrip -and $replySeen) { break }
  if ($turnError) { break }
}

Write-Host ''
Write-Host '================ MCP LIVE E2E RESULT ================'
Write-Host ("  tool_call to MCP server seen : {0}" -f $toolCallSeen)
Write-Host ("  sentinel echoed BACK (result): {0}" -f $echoRoundTrip)
Write-Host ("  assistant final reply        : {0}" -f $replySeen)
if ($replySeen) { Write-Host ("  REPLY: " + $reply) -ForegroundColor White }
if ($turnError) { Write-Host ("  TURN ERROR: " + $turnError) -ForegroundColor Red }
Write-Host ("  raw events dumped to         : {0}" -f $rawEventsPath)
Write-Host '====================================================='
if ($toolCallSeen -and $echoRoundTrip) {
  Ok 'PASS - the live LLM called the MCP virtual tool and the real server echoed the sentinel back.'
  exit 0
} elseif ($turnError) {
  Fail "FAIL - the turn errored before/around the tool call: $turnError"
  exit 1
} else {
  Fail 'FAIL - no MCP tool_call observed. See the dumped events.'
  exit 1
}
