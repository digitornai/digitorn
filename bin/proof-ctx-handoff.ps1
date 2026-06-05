# Live proof: the compaction HANDOFF is a powerful, structured, framed system
# message — produced by the real summary brain through the real daemon — so the
# agent can continue as if the compaction never happened.
#
# Isolated: boots its OWN daemon on :28003 in a temp workspace. Does NOT touch
# the running daemon (:8000) or any CLI session.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
function Info($m){ Write-Host "[proof] $m" -ForegroundColor Cyan }
function Ok($m){ Write-Host "[proof] $m" -ForegroundColor Green }
function Warn($m){ Write-Host "[proof] $m" -ForegroundColor Yellow }
function Fail($m){ Write-Host "[proof] $m" -ForegroundColor Red }

$repo = "C:\Users\ASUS\Documents\digitorn_go"
Set-Location $repo

# JWT from credentials.json — read, never print.
$cred = "$env:USERPROFILE\.digitorn\credentials.json"
if (-not (Test-Path $cred)) { Fail "no credentials.json"; exit 1 }
$cj = Get-Content $cred -Raw | ConvertFrom-Json
$jwt = $cj.access_token; if (-not $jwt) { $jwt = $cj.token }; if (-not $jwt) { $jwt = $cj.jwt }
if (-not $jwt) { Fail "no token in credentials.json"; exit 1 }
Ok "JWT loaded (len=$($jwt.Length))"

$gateway = "http://127.0.0.1:8002/v1"
$env:DIGITORN_WORKERS__LLM__GATEWAY_URL = $gateway
$base = "http://127.0.0.1:28003"
$ws = "C:\Users\ASUS\AppData\Local\Temp\digitorn-ctxsummary-proof"

# Clean workspace.
if (Test-Path $ws) { Remove-Item -Recurse -Force $ws }
New-Item -ItemType Directory -Force -Path "$ws\sessions","$ws\apps","$ws\bin" | Out-Null

# Build daemon + llm worker with current code INTO the isolated workspace —
# the running daemon (:8000) locks bin\digitornd.exe / worker, so we never
# touch those. resolveWorkerBinary finds the worker alongside the daemon exe.
Info "building digitornd + worker-llm into isolated workspace ..."
& go build -o "$ws\bin\digitornd.exe" "./cmd/digitornd"; if ($LASTEXITCODE) { Fail "build daemon"; exit 1 }
& go build -o "$ws\bin\digitorn-worker-llm.exe" "./cmd/digitorn-worker-llm"; if ($LASTEXITCODE) { Fail "build worker"; exit 1 }
Ok "binaries built (isolated)"

$log = "$ws\daemon.log"
$daemon = Start-Process -FilePath "$ws\bin\digitornd.exe" -ArgumentList "-config","$repo\bin\config-ctxsummary-proof.yaml" `
  -RedirectStandardOutput $log -RedirectStandardError "$ws\daemon.err.log" -PassThru -NoNewWindow

$exit = 1
try {
  Info "waiting for daemon ..."
  $deadline = (Get-Date).AddSeconds(20); $ready=$false
  while ((Get-Date) -lt $deadline) { try { $null = Invoke-RestMethod "$base/health" -TimeoutSec 1; $ready=$true; break } catch {}; Start-Sleep -Milliseconds 250 }
  if (-not $ready) { Fail "daemon not ready"; Get-Content $log -Tail 40 | % { Write-Host "  $_" }; throw "not ready" }
  Ok "daemon ready"

  $hdr = @{ "Authorization"="Bearer $jwt"; "X-User-ID"="proof-user"; "Content-Type"="application/json" }

  Info "installing chat-ctx-summary ..."
  $inst = Invoke-RestMethod -Method POST "$base/api/apps/install" -Headers $hdr -Body (@{ source="$repo\bin\test-apps\chat-ctx-summary" } | ConvertTo-Json)
  Ok "installed app_id=$($inst.app_id) byok=$($inst.byok)"
  $app = $inst.app_id

  $sess = Invoke-RestMethod -Method POST "$base/api/apps/$app/sessions" -Headers $hdr -Body "{}"
  $sid = $sess.session_id
  Ok "session_id=$sid"

  # Drive turns that build a real, coherent task so the handoff has something
  # meaningful to capture (mission/plan/progress), and enough volume to cross
  # the 0.2 trigger of the 16k window.
  $prompts = @(
    "Mission: build a tiny CLI calculator in Go. First, write a file calc.go with an add(a,b int) int function and a main that prints add(2,3). Tell me your plan in one line, then do it.",
    "Now add a subtract(a,b int) int function to calc.go and call it from main, printing subtract(10,4). Keep the existing add.",
    "Add multiply and divide functions too. For divide, guard against division by zero by returning 0. Update main to demo all four operations.",
    "Read calc.go back and summarize in 2 lines what the program currently does.",
    "Now add a small table of test cases in a comment block at the top of calc.go listing the expected results of each operation you implemented.",
    "Refactor: move all four operations into a single switch-based func apply(op string, a, b int) int and have main call apply for each demo. Keep behavior identical.",
    "Read calc.go again and confirm the four operations still produce the same demo outputs as before.",
    "What is the next logical improvement you would make to this calculator? Answer in one sentence."
  )

  $compacted = $false
  for ($i=0; $i -lt $prompts.Count; $i++) {
    Info ("turn {0}/{1}: {2}" -f ($i+1), $prompts.Count, $prompts[$i].Substring(0,[Math]::Min(60,$prompts[$i].Length)))
    $null = Invoke-RestMethod -Method POST "$base/api/apps/$app/sessions/$sid/messages" -Headers $hdr -Body (@{ content=$prompts[$i] } | ConvertTo-Json)
    # Wait for the assistant to finish this turn (poll history length growth).
    $td = (Get-Date).AddSeconds(90); $done=$false; $lastLen=0
    while ((Get-Date) -lt $td) {
      Start-Sleep -Milliseconds 800
      try { $h = Invoke-RestMethod -Method GET "$base/api/apps/$app/sessions/$sid/history" -Headers $hdr } catch { continue }
      $n = if ($h.messages) { @($h.messages).Count } else { 0 }
      # turn done heuristic: last message is assistant and count stable for a beat
      if ($n -gt 0) {
        $last = @($h.messages)[-1]
        if ($last.role -eq "assistant" -and $n -eq $lastLen) { $done=$true; break }
        $lastLen = $n
      }
    }
    # Check disk for a summarize compaction so far.
    $ev = Get-ChildItem -Path "$ws\sessions" -Recurse -Filter "events.jsonl" -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($ev) {
      $hit = Select-String -Path $ev.FullName -Pattern '"type":"context_compacted"' -SimpleMatch -ErrorAction SilentlyContinue
      if ($hit) { $compacted=$true; Ok "compaction detected after turn $($i+1)"; break }
    }
  }

  Info "stopping daemon to flush ..."
  Stop-Process -Id $daemon.Id -Force -ErrorAction SilentlyContinue
  $daemon | Wait-Process -Timeout 6 -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 500

  $ev = Get-ChildItem -Path "$ws\sessions" -Recurse -Filter "events.jsonl" -ErrorAction SilentlyContinue | Select-Object -First 1
  if (-not $ev) { Fail "no events.jsonl found"; throw "no events" }
  Info "events file: $($ev.FullName)"

  $lines = Get-Content $ev.FullName
  $compLines = $lines | Where-Object { $_ -match '"type":"context_compacted"' }
  if (-not $compLines) {
    Fail "NO context_compacted event — compaction never fired (trigger not crossed)"
    Warn "daemon log tail:"; Get-Content $log -Tail 30 | % { Write-Host "  $_" }
    throw "no compaction"
  }

  $outFile = "$ws\handoff-dump.txt"
  "" | Out-File -FilePath $outFile -Encoding utf8
  $idx=0
  foreach ($cl in $compLines) {
    $idx++
    $obj = $cl | ConvertFrom-Json
    $cc = $obj.ctx_compact
    "===== context_compacted #$idx =====" | Out-File -Append $outFile -Encoding utf8
    "strategy        : $($cc.strategy)"        | Out-File -Append $outFile -Encoding utf8
    "messages_dropped: $($cc.messages_dropped)" | Out-File -Append $outFile -Encoding utf8
    "cutoff_seq      : $($cc.cutoff_seq)"       | Out-File -Append $outFile -Encoding utf8
    "tokens_before   : $($cc.tokens_before)"    | Out-File -Append $outFile -Encoding utf8
    "tokens_freed    : $($cc.tokens_freed)"     | Out-File -Append $outFile -Encoding utf8
    "new_context_tok : $($cc.new_context_tokens)" | Out-File -Append $outFile -Encoding utf8
    "----- HANDOFF (CtxCompact.Summary, what the LLM receives) -----" | Out-File -Append $outFile -Encoding utf8
    $cc.summary | Out-File -Append $outFile -Encoding utf8
    "" | Out-File -Append $outFile -Encoding utf8
  }

  Ok "==============================================="
  Ok (" PROOF: {0} compaction(s); dump -> {1}" -f @($compLines).Count, $outFile)
  Ok "==============================================="
  Get-Content $outFile | ForEach-Object { Write-Host $_ }
  $exit = 0
}
finally {
  try { Stop-Process -Id $daemon.Id -Force -ErrorAction SilentlyContinue } catch {}
}
exit $exit
