# Generic MCP server end-to-end test. Installs a test app that wires ONE real
# MCP server, sends a prompt, and asserts the agent called an mcp tool AND the
# real server's output (the Expect substring) rounds back through a tool_result.
#
# Usage:
#   powershell -File bin\mcp-srv-e2e.ps1 -App mcp-fs -Dir bin\test-apps\mcp-fs `
#       -Prompt "Read SENTINEL_FS.txt and report its contents." -Expect "digitorn-mcp-filesystem-OK-9931"

param(
  [Parameter(Mandatory)][string]$App,
  [Parameter(Mandatory)][string]$Dir,
  [Parameter(Mandatory)][string]$Prompt,
  [Parameter(Mandatory)][string]$Expect,
  [int]$MaxSeconds = 180
)

$ErrorActionPreference = 'Stop'
$base = 'http://127.0.0.1:8000'
$bundle = Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) ('..\' + $Dir) | Resolve-Path | Select-Object -ExpandProperty Path

function Info($m){ Write-Host "[mcp-srv] $m" -ForegroundColor Cyan }
function Ok($m){ Write-Host "[mcp-srv] $m" -ForegroundColor Green }
function Fail($m){ Write-Host "[mcp-srv] $m" -ForegroundColor Red }

function Rest($method,$url,$headers,$body){
  try {
    if ($null -eq $body) { return Invoke-RestMethod -Method $method -Uri $url -Headers $headers -TimeoutSec 30 }
    return Invoke-RestMethod -Method $method -Uri $url -Headers $headers -Body $body -TimeoutSec 30
  } catch {
    $resp = $_.Exception.Response; $code = if ($resp) { [int]$resp.StatusCode } else { 0 }; $rb=''
    if ($resp) { try { $rb=(New-Object System.IO.StreamReader($resp.GetResponseStream())).ReadToEnd() } catch {} }
    throw "HTTP $code from $method $url : $rb"
  }
}

$tok = (Get-Content (Join-Path $env:USERPROFILE '.digitorn\credentials.json') -Raw | ConvertFrom-Json).access_token
$H = @{ Authorization = "Bearer $tok"; 'Content-Type' = 'application/json' }

Info "installing $App from $bundle"
$inst = Rest 'POST' "$base/api/apps/install" $H (@{ source = $bundle } | ConvertTo-Json)
Ok "installed $($inst.app_id) v$($inst.version)"
$sid = (Rest 'POST' "$base/api/apps/$App/sessions" $H '{}').session_id
if (-not $sid) { Fail 'no session'; exit 1 }
Ok "session $sid"
$null = Rest 'POST' "$base/api/apps/$App/sessions/$sid/messages" $H (@{ content = $Prompt } | ConvertTo-Json)
Info "prompt sent; polling (max ${MaxSeconds}s)..."

$deadline = (Get-Date).AddSeconds($MaxSeconds)
$toolCall=$false; $expectBack=$false; $reply=''; $turnErr=''; $toolNames=@()
$dump = Join-Path $env:TEMP "mcp-srv-$App-$sid.json"
while ((Get-Date) -lt $deadline) {
  Start-Sleep -Milliseconds 1500
  try { $ev = Rest 'GET' "$base/api/apps/$App/sessions/$sid/events" $H $null } catch { continue }
  Set-Content -Path $dump -Value ($ev | ConvertTo-Json -Depth 30) -Encoding utf8
  foreach ($e in $ev.events) {
    $blob = ($e.payload | ConvertTo-Json -Depth 20 -Compress)
    if ("$($e.type)" -match 'tool' -and $blob -match 'mcp_') { $toolCall=$true; if ($e.payload.name){ $toolNames += $e.payload.name } }
    if ($e.type -eq 'tool_result' -and $blob -match [regex]::Escape($Expect)) { $expectBack=$true }
    if ($e.type -eq 'assistant_message' -and $e.payload.content) { $reply=$e.payload.content }
    if ($e.type -eq 'error' -or $e.payload.phase -eq 'errored') { $turnErr = "$($e.payload.reason)$($e.payload.message)" }
  }
  if ($toolCall -and $expectBack) { break }
  if ($turnErr) { break }
}

Write-Host ''
Write-Host "============ MCP SERVER E2E : $App ============"
Write-Host ("  mcp tool_call seen        : {0}  ({1})" -f $toolCall, (($toolNames | Select-Object -Unique) -join ', '))
Write-Host ("  expected output round-trip: {0}  (looking for '{1}')" -f $expectBack, $Expect)
if ($reply) { Write-Host ("  REPLY: " + ($reply -replace '\s+',' ')) -ForegroundColor White }
if ($turnErr) { Write-Host ("  TURN ERROR: " + $turnErr) -ForegroundColor Red }
Write-Host ("  events: {0}" -f $dump)
Write-Host '==============================================='
if ($toolCall -and $expectBack) { Ok "PASS - $App"; exit 0 }
elseif ($turnErr) { Fail "FAIL - turn errored: $turnErr"; exit 1 }
else { Fail "FAIL - no round-trip (tool_call=$toolCall, output_back=$expectBack)"; exit 1 }
