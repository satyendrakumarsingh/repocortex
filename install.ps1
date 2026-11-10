# RepoCortex installer for Windows.
#
# Checksum verification is mandatory and has no bypass flag, matching
# install.sh. Invoke-WebRequest marks downloads with the Mark-of-the-Web, so
# the archive is unblocked after verification, not before.
[CmdletBinding()]
param(
    [string]$Dir = "$env:LOCALAPPDATA\Programs\cortex",
    [string]$Version = "latest"
)

$ErrorActionPreference = "Stop"
$repo = "satyendrakumarsingh/repocortex"

if ($Version -eq "latest") {
    $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest"
    $Version = $rel.tag_name
}
if (-not $Version) { throw "could not resolve the latest release tag" }

$archive = "cortex_${Version}_windows_amd64.zip"
$base = "https://github.com/$repo/releases/download/$Version"
$tmp = Join-Path $env:TEMP ("cortex-" + [guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

try {
    Write-Host "downloading $archive ($Version)"
    Invoke-WebRequest -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
    Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile (Join-Path $tmp "SHA256SUMS")

    Write-Host "verifying checksum"
    $actual = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $archive)).Hash.ToLower()
    $line = Select-String -Path (Join-Path $tmp "SHA256SUMS") -Pattern ([regex]::Escape($archive)) | Select-Object -First 1
    if (-not $line) { throw "$archive is absent from SHA256SUMS" }
    $expected = ($line.Line -split '\s+')[0].ToLower()
    if ($actual -ne $expected) {
        throw "checksum mismatch for ${archive}: expected $expected, actual $actual"
    }

    Unblock-File -Path (Join-Path $tmp $archive)
    Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force

    New-Item -ItemType Directory -Path $Dir -Force | Out-Null
    $src = Join-Path $tmp "cortex_${Version}_windows_amd64\cortex.exe"
    if (-not (Test-Path $src)) {
        $src = Join-Path $tmp "cortex_${Version}_windows_amd64\cortex.exe"
    }
    Copy-Item -Path $src -Destination (Join-Path $Dir "cortex.exe") -Force
    Copy-Item -Path $src -Destination (Join-Path $Dir "repocortex.exe") -Force

    Write-Host "installed $Dir\cortex.exe (with repocortex.exe alias)"
    & (Join-Path $Dir "cortex.exe") version

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($userPath -notlike "*$Dir*") {
        [Environment]::SetEnvironmentVariable("Path", "$userPath;$Dir", "User")
        Write-Host "added $Dir to your user PATH — open a new terminal to pick it up"
    }

    Write-Host ""
    Write-Host "Next: cd into a repository and run"
    Write-Host "  cortex init; cortex index; cortex resolve"
    Write-Host "  cortex install     # register with your coding agent"
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
