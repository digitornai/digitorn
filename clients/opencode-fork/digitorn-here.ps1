# Launch the opencode-fork TUI (digitorn daemon) with the AGENT working in the
# directory you call this from — while Bun still loads opencode's bunfig/tsconfig
# (JSX = Solid) from the package dir.
#
#   Usage (from any project folder):
#     C:\Users\ASUS\Documents\digitorn_go\clients\opencode-fork\digitorn-here.ps1
#     ...\digitorn-here.ps1 -App chat-simple -Url http://localhost:8000
#
# The daemon must already be running. The launch dir is sent as the session
# workdir (DIGITORN_CWD) — the daemon honors an absolute client workdir.
param(
  [string]$App = "claude-code",
  [string]$Url = "http://localhost:8000"
)
$ErrorActionPreference = "Stop"

$proj = (Get-Location).Path
$pkg = "C:\Users\ASUS\Documents\digitorn_go\clients\opencode-fork\packages\opencode"

$env:DIGITORN_CWD = $proj
$env:DIGITORN_URL = $Url
$env:DIGITORN_APP = $App

Write-Host "▶ project (agent workdir): $proj" -ForegroundColor Cyan
Write-Host "▶ app: $App   daemon: $Url" -ForegroundColor DarkGray

Push-Location $pkg
try {
  & bun run dev
} finally {
  Pop-Location
}
