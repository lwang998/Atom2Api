# Atom2Api Windows packaging script.
# Default flavor KEEPS the GitHub update-check links; pass -NoGitHub to build a
# distribution with no GitHub address (update check compiled out).
#
# Examples:
#   .\package.ps1 -Version 1.0.12                # standard package (default)
#   .\package.ps1 -Version 1.0.12 -NoGitHub      # no-GitHub distribution
#   .\package.ps1 -Version 1.0.12 -SkipFrontend -SkipDesktop
#
# Requirements: Go 1.22+, Node.js 20+, and Rust stable (unless -SkipDesktop).
# Output lands in dist\.
#Requires -Version 5.1
param(
  [string]$Version = "",
  [switch]$NoGitHub,
  [switch]$SkipFrontend,
  [switch]$SkipDesktop
)
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot

if (-not $Version) {
  $short = (git -C $root rev-parse --short HEAD)
  if ($LASTEXITCODE -ne 0) { throw 'git rev-parse failed; pass -Version explicitly' }
  $Version = "dev+$short"
}
$suffix = ''
$tagArgs = @()
if ($NoGitHub) {
  $suffix = '_nogithub'
  $tagArgs = @('-tags', 'nogithub')
}

$go = (Get-Command go -ErrorAction SilentlyContinue).Source
if (-not $go -or -not (Test-Path $go)) { $go = Join-Path $env:ProgramFiles 'Go\bin\go.exe' }
if (-not (Test-Path $go)) { throw 'go.exe not found (install Go or add it to PATH)' }

if (-not $SkipFrontend) {
  Push-Location (Join-Path $root 'frontend')
  try {
    npm run build
    if ($LASTEXITCODE -ne 0) { throw 'frontend build failed' }
  } finally { Pop-Location }
}

$serverExe = "atom2api_${Version}${suffix}_windows_amd64.exe"
Push-Location $root
try {
  $env:CGO_ENABLED = '0'; $env:GOOS = 'windows'; $env:GOARCH = 'amd64'
  & $go build @tagArgs -trimpath -ldflags "-s -w -X main.version=$Version" -o (Join-Path 'dist' $serverExe) .
  if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
} finally { Pop-Location }

$shellExe = Join-Path $root 'desktop\src-tauri\target\release\atom2api-desktop.exe'
if (-not $SkipDesktop) {
  New-Item -ItemType Directory -Path (Join-Path $root 'desktop\src-tauri\binaries') -Force | Out-Null
  Copy-Item (Join-Path $root "dist\$serverExe") (Join-Path $root 'desktop\src-tauri\binaries\atom2api-server-x86_64-pc-windows-msvc.exe') -Force
  Push-Location (Join-Path $root 'desktop')
  try {
    $cargoBin = Join-Path $env:USERPROFILE '.cargo\bin'
    if (Test-Path $cargoBin) { $env:PATH = "$cargoBin;$env:PATH" }
    npx tauri build
    if ($LASTEXITCODE -ne 0) { throw 'tauri build failed' }
  } finally { Pop-Location }
}
if (-not (Test-Path $shellExe)) { throw "desktop shell not found at $shellExe; build without -SkipDesktop first" }

$stage = Join-Path $root 'dist\stage'
Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Path $stage | Out-Null
Copy-Item $shellExe (Join-Path $stage 'Atom2Api.exe') -Force
Copy-Item (Join-Path $root "dist\$serverExe") (Join-Path $stage 'atom2api-server.exe') -Force
$notice = Get-Content (Join-Path $root 'packaging\notice-zh.txt') -Raw -Encoding UTF8
if (-not $NoGitHub) {
  $notice += "`r`n`r`n" + (Get-Content (Join-Path $root 'packaging\repository-line.txt') -Raw -Encoding UTF8)
}
Set-Content -Path (Join-Path $stage '使用说明.txt') -Value $notice -Encoding UTF8

$zip = "Atom2Api_${Version}${suffix}_windows_x64.zip"
Remove-Item (Join-Path $root "dist\$zip") -Force -ErrorAction SilentlyContinue
Compress-Archive -Path (Join-Path $stage 'Atom2Api.exe'), (Join-Path $stage 'atom2api-server.exe'), (Join-Path $stage '使用说明.txt') -DestinationPath (Join-Path $root "dist\$zip") -Force
Remove-Item $stage -Recurse -Force
Write-Output "packaged: dist\$zip"
