#Requires -Version 5.0
<#
.SYNOPSIS
    sshmng one-click install script for Windows.

.DESCRIPTION
    Downloads the latest sshmng release for Windows, extracts sshmng.exe,
    places it on PATH, and optionally runs 'sshmng install --yes'.

.PARAMETER Yes
    Also run 'sshmng install --yes' after placing the binary.

.PARAMETER InstallDir
    Override install directory (default: $env:USERPROFILE\bin).

.EXAMPLE
    # Default install (places binary, updates user PATH):
    irm https://raw.githubusercontent.com/jim58246/sshmng/main/install.ps1 | iex

.EXAMPLE
    # Install + run 'sshmng install --yes' (download-and-invoke pattern):
    $installer = "$env:TEMP\sshmng-install.ps1"
    irm https://raw.githubusercontent.com/jim58246/sshmng/main/install.ps1 -OutFile $installer
    & $installer -Yes
    Remove-Item $installer
#>
[CmdletBinding()]
param(
    [switch]$Yes,
    [string]$InstallDir = ""
)

$ErrorActionPreference = "Stop"

$Owner = "jim58246"
$Repo = "sshmng"

# --- detect arch ---
if ($env:PROCESSOR_ARCHITECTURE -eq "AMD64") {
    $Arch = "amd64"
} elseif ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") {
    $Arch = "arm64"
} else {
    throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE"
}

Write-Host ">> Platform: windows/$Arch"

# --- fetch latest version ---
Write-Host ">> Fetching latest release..."
$Release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Owner/$Repo/releases/latest" -UseBasicParsing
$Latest = $Release.tag_name
if (-not $Latest) {
    throw "Failed to fetch latest version from GitHub API"
}
Write-Host ">> Latest release: $Latest"

# --- download archive ---
$Archive = "sshmng-$Latest-windows-$Arch.zip"
$Url = "https://github.com/$Owner/$Repo/releases/download/$Latest/$Archive"
$TempDir = Join-Path $env:TEMP "sshmng-install-$(New-Guid)"
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null
try {
    Write-Host ">> Downloading $Url"
    Invoke-WebRequest -Uri $Url -OutFile (Join-Path $TempDir $Archive) -UseBasicParsing

    # --- extract ---
    Write-Host ">> Extracting..."
    Expand-Archive -Path (Join-Path $TempDir $Archive) -DestinationPath $TempDir -Force
    $Binary = Get-ChildItem -Path $TempDir -Filter "sshmng.exe" -Recurse | Select-Object -First 1
    if (-not $Binary) {
        throw "Binary 'sshmng.exe' not found in archive"
    }
    $Binary = $Binary.FullName

    # --- pick install dir ---
    if (-not $InstallDir) {
        $InstallDir = Join-Path $env:USERPROFILE "bin"
    }
    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }

    # --- place binary ---
    $Target = Join-Path $InstallDir "sshmng.exe"
    Move-Item $Binary $Target -Force
    Write-Host ">> Installed: $Target"

    # --- PATH check (user-level, persistent) ---
    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($UserPath -notlike "*$InstallDir*") {
        $NewPath = if ($UserPath) { "$UserPath;$InstallDir" } else { $InstallDir }
        [Environment]::SetEnvironmentVariable("Path", $NewPath, "User")
        Write-Host ">> Added $InstallDir to user PATH (restart shell to take effect)"
    }

    # --- optional: run install ---
    if ($Yes) {
        Write-Host ""
        Write-Host ">> Running 'sshmng install --yes'..."
        & $Target install --yes
    } else {
        Write-Host ""
        Write-Host "Next steps:"
        Write-Host "  1. sshmng install        # create ~/.sshmng/ + inject into AI Agents"
        Write-Host "  2. sshmng doctor         # verify setup"
        Write-Host "  3. Restart your AI Agent"
        Write-Host ""
        Write-Host "Tip: re-run with -Yes to auto-run 'sshmng install --yes'."
    }
}
finally {
    Remove-Item -Recurse -Force $TempDir -ErrorAction SilentlyContinue
}

Write-Host ""
Write-Host "Done."
