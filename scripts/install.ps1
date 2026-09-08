#
# gemini-api CLI Installation Script for Windows
# This script downloads and installs the latest version of the gemini-api CLI
#
# Usage:
#   iwr -useb https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.ps1 | iex
#   or
#   Invoke-WebRequest -Uri https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.ps1 -UseBasicParsing | Invoke-Expression
#
# Options:
#   $env:GEMINI_INSTALL_DIR - Installation directory (default: $env:LOCALAPPDATA\Programs\gemini-api)
#   $env:GEMINI_VERSION     - Specific version to install (default: latest)
#

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

# Configuration
$Repo = "google-gemini/gemini-api-cli"
$BinaryName = "gemini-api.exe"
$DefaultInstallDir = Join-Path $env:LOCALAPPDATA "Programs\gemini-api"
$InstallDir = if ($env:GEMINI_INSTALL_DIR) { $env:GEMINI_INSTALL_DIR } else { $DefaultInstallDir }
$Version = if ($env:GEMINI_VERSION) { $env:GEMINI_VERSION } else { "latest" }

# Helper functions
function Write-ColorOutput {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Message,
        [string]$Color = "White"
    )
    Write-Host $Message -ForegroundColor $Color
}

function Get-LatestVersion {
    try {
        $response = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -UseBasicParsing
        return $response.tag_name
    }
    catch {
        Write-ColorOutput "Failed to get latest version: $_" -Color Red
        exit 1
    }
}

function Get-Architecture {
    $arch = $env:PROCESSOR_ARCHITECTURE
    switch ($arch) {
        "AMD64" { return "x86_64" }
        "ARM64" { return "arm64" }
        default {
            Write-ColorOutput "Unsupported architecture: $arch" -Color Red
            exit 1
        }
    }
}

function Install-CLI {
    Write-ColorOutput "Installing gemini-api CLI..." -Color Green

    # Detect architecture
    $arch = Get-Architecture
    Write-ColorOutput "Detected Architecture: $arch" -Color Cyan

    # Get version
    if ($Version -eq "latest") {
        $Version = Get-LatestVersion
        Write-ColorOutput "Latest version: $Version" -Color Cyan
    }

    # Construct download URL
    $archiveName = "gemini-api_Windows_$arch.zip"
    $downloadUrl = "https://github.com/$Repo/releases/download/$Version/$archiveName"

    Write-ColorOutput "Downloading from: $downloadUrl" -Color Cyan

    # Create temporary directory
    $tempDir = Join-Path $env:TEMP "gemini-api-install-$(New-Guid)"
    New-Item -ItemType Directory -Path $tempDir -Force | Out-Null

    try {
        # Download archive
        $archivePath = Join-Path $tempDir $archiveName
        try {
            Invoke-WebRequest -Uri $downloadUrl -OutFile $archivePath -UseBasicParsing
        }
        catch {
            Write-ColorOutput "Failed to download from $downloadUrl" -Color Red
            Write-ColorOutput "Error: $_" -Color Red
            exit 1
        }

        Write-ColorOutput "Download complete" -Color Green

        # Extract archive
        Write-ColorOutput "Extracting archive..." -Color Cyan
        Expand-Archive -Path $archivePath -DestinationPath $tempDir -Force

        # Create install directory if it doesn't exist
        if (-not (Test-Path $InstallDir)) {
            Write-ColorOutput "Creating installation directory: $InstallDir" -Color Cyan
            New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        }

        # Install binary
        $binaryPath = Join-Path $InstallDir $BinaryName
        Write-ColorOutput "Installing to $binaryPath..." -Color Cyan

        # Remove existing binary if it exists
        if (Test-Path $binaryPath) {
            Remove-Item $binaryPath -Force
        }

        Copy-Item -Path (Join-Path $tempDir $BinaryName) -Destination $binaryPath -Force

        Write-ColorOutput "gemini-api $Version has been installed to $binaryPath" -Color Green

        # Add to PATH if not already there
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        if ($userPath -notlike "*$InstallDir*") {
            Write-ColorOutput "Adding $InstallDir to your PATH..." -Color Cyan
            [Environment]::SetEnvironmentVariable(
                "Path",
                "$userPath;$InstallDir",
                "User"
            )
            $env:Path = "$env:Path;$InstallDir"
            Write-ColorOutput "Added to PATH. You may need to restart your terminal for changes to take effect." -Color Yellow
        }

        Write-ColorOutput "Installation successful! Run 'gemini-api --help' to get started." -Color Green
        Write-ColorOutput "Note: You may need to restart your terminal or run 'refreshenv' for the PATH changes to take effect." -Color Yellow
    }
    finally {
        # Cleanup
        if (Test-Path $tempDir) {
            Remove-Item $tempDir -Recurse -Force
        }
    }
}

# Main execution
try {
    Install-CLI
}
catch {
    Write-ColorOutput "Installation failed: $_" -Color Red
    exit 1
}
