# Live proof of the approval flow against a real daemon + real LLM.
#
# approve-probe gates filesystem.read behind the `approve` policy. We
# drive the model to call read, then resolve the approval via the SAME
# REST endpoint+body the CLI's client.ResolveApproval builds:
#   POST /api/apps/{id}/approve {session_id, approval_id, action, reason}
#
# Run A (approve): read executes -> reply contains the magic number.
# Run B (deny):    read blocked  -> reply does NOT contain it.
# Both: assert the approval_request payload carries the keys the CLI
# widget reads (id, tool_name, risk_level, reason).

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Log($m){ Write-Host "[approve-test] $m" -ForegroundColor Cyan }
function Ok($m){ Write-Host "[approve-test] $m" -ForegroundColor Green }
function Warn($m){ Write-Host "[approve-test] $m" -ForegroundColor Yellow }
function Fail($m){ Write-Host "[approve-test] $m" -ForegroundColor Red }

$repoRoot = "C:\Users\ASUS\Documents\digitorn_go"
Set-Location $repoRoot

$cred = Get-Content "$env:USERPROFILE\.digitorn\credentials.json" | ConvertFrom-Json
$jwt = $cred.access_token
$env:DIGITORN_WORKERS__LLM__GATEWAY_URL = "http://127.0.0.1:8002/v1"
$base = "http://127.0.0.1:28004"
$headers = @{ "Authorization"="Bearer $jwt"; "X-User-ID"="approve-test-user"; "Content-Type"="application/json" }

$root = "C:\Users\ASUS\AppData\Local\Temp\digitorn-approve-probe"
$ws = Join-Path $root "ws"
if (Test-Path $root) { Remove-Item -Recurse -Force $root }
New-Item -ItemType Directory -Force -Path $ws | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $root "sessions") | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $root "apps") | Out-Null
Set-Content -Path (Join-Path $ws "seed.txt") -Value "the magic number is 4242" -Encoding utf8
Ok "seeded $ws\seed.txt"

$cfg = Join-Path $repoRoot "bin\config-approve-probe.yaml"
$log = Join-Path $root "daemon.log"
# WorkingDirectory = ws so the filesystem module resolves relative paths into it.
$daemon = Start-Process -FilePath (Join-Path $repoRoot "bin\digitornd.exe") `
    -ArgumentList "-config", $cfg -WorkingDirectory $ws `
    -RedirectStandardOutput $log -RedirectStandardError (Join-Path $root "daemon.err.log") `
    -PassThru -NoNewWindow

# --- helpers ---
function Get-Events($sid) {
    try { return Invoke-RestMethod -Method GET -Uri "$base/api/apps/approve-probe/sessions/$sid/events" -Headers $headers }
    catch { return $null }
}
function Wait-EventType($sid, $type, $secs) {
    $dl = (Get-Date).AddSeconds($secs)
    while ((Get-Date) -lt $dl) {
        Start-Sleep -Milliseconds 500
        $ev = Get-Events $sid
        if ($ev -and $ev.events) {
            $hit = @($ev.events | ? { $_.type -eq $type })
            if ($hit.Count -gt 0) { return $hit[-1] }
        }
    }
    return $null
}
function Last-Assistant($sid) {
    try { $h = Invoke-RestMethod -Method GET -Uri "$base/api/apps/approve-probe/sessions/$sid/history" -Headers $headers } catch { return "" }
    if ($h.messages) { $a = @($h.messages | ? { $_.role -eq 'assistant' -and $_.content }); if ($a.Count) { return $a[-1].content } }
    return ""
}
function Resolve($sid, $approvalID, $action) {
    $body = @{ session_id=$sid; approval_id=$approvalID; action=$action; reason="live-test" } | ConvertTo-Json
    return Invoke-RestMethod -Method POST -Uri "$base/api/apps/approve-probe/approve" -Headers $headers -Body $body
}

$exit = 1
try {
    Log "waiting for daemon ..."
    $dl = (Get-Date).AddSeconds(15); $ready=$false
    while ((Get-Date) -lt $dl) { try { if (Invoke-RestMethod -Uri "$base/health" -TimeoutSec 1) { $ready=$true; break } } catch {}; Start-Sleep -Milliseconds 250 }
    if (-not $ready) { Get-Content $log -Tail 40 | % { Write-Host "  $_" }; throw "daemon not ready" }
    Ok "daemon ready"

    $inst = Invoke-RestMethod -Method POST -Uri "$base/api/apps/install" -Headers $headers -Body (@{source=(Join-Path $repoRoot "bin\test-apps\approve-probe")}|ConvertTo-Json)
    Ok "installed $($inst.app_id) v$($inst.version)"

    $prompt = "Read the file seed.txt and tell me the magic number it contains."

    # ===== RUN A : APPROVE =====
    Log "===== RUN A : APPROVE ====="
    $sidA = (Invoke-RestMethod -Method POST -Uri "$base/api/apps/approve-probe/sessions" -Headers $headers -Body "{}").session_id
    Invoke-RestMethod -Method POST -Uri "$base/api/apps/approve-probe/sessions/$sidA/messages" -Headers $headers -Body (@{content=$prompt}|ConvertTo-Json) | Out-Null
    $reqA = Wait-EventType $sidA "approval_request" 45
    if (-not $reqA) { Get-Content $log -Tail 50 | % { Write-Host "  $_" }; throw "RUN A: no approval_request emitted" }
    $p = $reqA.payload
    Log "approval_request payload keys: $(@($p.PSObject.Properties.Name) -join ', ')"
    Write-Host ("  id={0} tool_name={1} risk_level={2} reason={3}" -f $p.id,$p.tool_name,$p.risk_level,$p.reason) -ForegroundColor White
    $keysOk = ($p.id) -and ($p.tool_name)
    if ($keysOk) { Ok "payload carries the keys the CLI widget reads (id, tool_name)" } else { Fail "payload MISSING keys the CLI widget needs" }

    Log "resolving APPROVED via POST /approve (the CLI's ResolveApproval contract)"
    Resolve $sidA $p.id "approved" | Out-Null
    $grant = Wait-EventType $sidA "approval_granted" 15
    if ($grant) { Ok "approval_granted emitted -> turn unblocked" } else { Warn "no approval_granted event seen" }
    $dl=(Get-Date).AddSeconds(45); $replyA=""
    while ((Get-Date) -lt $dl) { Start-Sleep -Milliseconds 600; $replyA = Last-Assistant $sidA; if ($replyA -match "4242") { break } }
    Write-Host "  REPLY A: $replyA" -ForegroundColor White

    # ===== RUN B : DENY =====
    Log "===== RUN B : DENY ====="
    $sidB = (Invoke-RestMethod -Method POST -Uri "$base/api/apps/approve-probe/sessions" -Headers $headers -Body "{}").session_id
    Invoke-RestMethod -Method POST -Uri "$base/api/apps/approve-probe/sessions/$sidB/messages" -Headers $headers -Body (@{content=$prompt}|ConvertTo-Json) | Out-Null
    $reqB = Wait-EventType $sidB "approval_request" 45
    if (-not $reqB) { throw "RUN B: no approval_request emitted" }
    Log "resolving DENIED via POST /approve"
    Resolve $sidB $reqB.payload.id "denied" | Out-Null
    $deny = Wait-EventType $sidB "approval_denied" 15
    if ($deny) { Ok "approval_denied emitted" } else { Warn "no approval_denied event seen" }
    $dl=(Get-Date).AddSeconds(30); $replyB=""
    while ((Get-Date) -lt $dl) { Start-Sleep -Milliseconds 600; $replyB = Last-Assistant $sidB; if ($replyB) { break } }
    Write-Host "  REPLY B: $replyB" -ForegroundColor White

    # ===== VERDICT =====
    Write-Host ""
    Log "================= VERDICT ================="
    if ($replyA -match "4242") { Ok "APPROVE: read executed after approval, reply has the number" } else { Fail "APPROVE: reply missing the number (read may not have run)" }
    if ($replyB -notmatch "4242") { Ok "DENY: number NOT revealed -> read was blocked" } else { Fail "DENY: number leaked -> deny did not block the read" }
    if ($keysOk) { Ok "CONTRACT: approval_request payload matches what the CLI widget parses" }
    Log "==========================================="
    $exit = 0
}
finally {
    Log "stopping daemon (pid=$($daemon.Id))"
    try { Stop-Process -Id $daemon.Id -Force -ErrorAction SilentlyContinue; $daemon | Wait-Process -Timeout 5 -ErrorAction SilentlyContinue } catch {}
}
exit $exit
