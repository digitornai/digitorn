<#
.SYNOPSIS
  Digitorn Installer for Windows
  Usage: iex "& { $(irm https://github.com/digitornai/digitorn/releases/latest/download/install.ps1) }"
#>
param(
    [string]$Version = "latest"
)

$ErrorActionPreference = "Stop"
$Repo = "digitornai/digitorn"

# ── Detection ──
function Detect-Platform {
    $os = "windows"
    $arch = switch ([System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture) {
        "X64"   { "amd64" }
        "Arm64" { "arm64" }
        default { throw "Unsupported architecture: $([System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture)" }
    }
    return "${os}_${arch}"
}

function Get-LatestVersion {
    $url = "https://api.github.com/repos/$Repo/releases/latest"
    $release = Invoke-RestMethod -Uri $url -UseBasicParsing
    return $release.tag_name
}

# ── Paths ──
$Platform = Detect-Platform
$os, $arch = $Platform.Split("_")
$InstallRoot = Join-Path $env:LOCALAPPDATA "digitorn"
$BinDir = Join-Path $env:LOCALAPPDATA "digitorn\bin"
$ConfigDir = Join-Path $env:APPDATA "digitorn"

Write-Host "► Digitorn Installer" -ForegroundColor Cyan
Write-Host "  Platform: $os/$arch"

$VersionTag = if ($Version -eq "latest") { Get-LatestVersion } else { $Version }
Write-Host "  Version:  $VersionTag" -ForegroundColor Cyan

$VerDir = $VersionTag.TrimStart("v")
$InstallPath = Join-Path $InstallRoot $VerDir

# ── Download ──
if (Test-Path (Join-Path $InstallPath "digitorn.exe")) {
    Write-Host "✓ Digitorn $VersionTag already installed at $InstallPath" -ForegroundColor Green
} else {
    $Asset = "digitorn-${VerDir}-${os}-${arch}.tar.gz"
    $DownloadUrl = "https://github.com/$Repo/releases/download/$VersionTag/$Asset"

    Write-Host "► Downloading $Asset..." -ForegroundColor Cyan
    $tmp = Join-Path $env:TEMP "digitorn-install"
    New-Item -ItemType Directory -Force -Path $tmp | Out-Null

    try {
        Invoke-WebRequest -Uri $DownloadUrl -OutFile (Join-Path $tmp "digitorn.tar.gz") -UseBasicParsing
    } catch {
        throw "Failed to download $DownloadUrl`nCheck https://github.com/$Repo/releases for available releases."
    }

    New-Item -ItemType Directory -Force -Path $InstallPath | Out-Null
    tar xzf (Join-Path $tmp "digitorn.tar.gz") -C $InstallPath --strip-components=1
    Remove-Item -Recurse -Force $tmp

    Write-Host "✓ Digitorn $VersionTag installed at $InstallPath" -ForegroundColor Green
}

# ── Current symlink (directory junction) ──
$CurrentLink = Join-Path $InstallRoot "current"
if (Test-Path $CurrentLink) { Remove-Item -Force -Recurse $CurrentLink }
New-Item -ItemType Junction -Path $CurrentLink -Target $InstallPath | Out-Null

# ── Copy user-facing binaries to BinDir ──
New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
$binaries = @("digitorn.exe", "digitornd.exe", "digitorn-tui.exe")
foreach ($b in $binaries) {
    $src = Join-Path $CurrentLink $b
    $dst = Join-Path $BinDir $b
    if (Test-Path $src) {
        Copy-Item -Force $src $dst
    }
}
Write-Host "✓ Binaries copied to $BinDir" -ForegroundColor Green

# ── Add BinDir to User PATH if needed ──
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$binInPath = ($userPath -split ";") | Where-Object { $_ -eq $BinDir }
if (-not $binInPath) {
    $newPath = if ($userPath) { "$userPath;$BinDir" } else { $BinDir }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    Write-Host "✓ Added $BinDir to PATH (restart your terminal)" -ForegroundColor Green
}

# ── Config ──
$ConfigFile = Join-Path $ConfigDir "config.yaml"
if (-not (Test-Path $ConfigFile)) {
    New-Item -ItemType Directory -Force -Path $ConfigDir | Out-Null
    $example = Join-Path $CurrentLink ".digitorn.yaml.example"
    if (Test-Path $example) {
        Copy-Item $example $ConfigFile
        Write-Host "✓ Config created at $ConfigFile" -ForegroundColor Green
    }
} else {
    Write-Host "✓ Config already exists at $ConfigFile" -ForegroundColor Green
}

# ── Service ──
$digitorndBin = Join-Path $BinDir "digitornd.exe"
if (Test-Path $digitorndBin) {
    Write-Host "► Installing Windows service..." -ForegroundColor Cyan
    & $digitorndBin -config $ConfigFile install 2>$null
    if ($LASTEXITCODE -eq 0) {
        Write-Host "✓ Service registered" -ForegroundColor Green
    } else {
        Write-Host "⚠ Service registration skipped (run as Administrator: digitornd -config $ConfigFile install)" -ForegroundColor Yellow
    }
}

# ── Done ──
Write-Host ""
Write-Host "✓ Digitorn $VersionTag is ready!" -ForegroundColor Green
Write-Host ""
Write-Host "  Commands:"
Write-Host "    digitorn chat          Launch the TUI"
Write-Host "    digitorn list          List installed apps"
Write-Host "    digitorn upgrade       Check for updates"
Write-Host "    digitornd status       Daemon status"
Write-Host ""
Write-Host "  Next steps:"
Write-Host "    1. Edit config: $ConfigFile"
Write-Host "    2. Start daemon: digitornd -config $ConfigFile run"
Write-Host "    3. Open TUI:     digitorn chat"
Write-Host ""
