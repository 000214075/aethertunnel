# Cross-compiles the release matrix into dist\ and writes SHA256SUMS.
#
# Windows equivalent of scripts/build-release.sh, for machines without make or a
# POSIX shell. Builds the same six platforms.
#
# Usage:  powershell -ExecutionPolicy Bypass -File scripts\build-release.ps1 [-Version v3.1.0]

param(
    [string]$Version = ""
)

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

if (-not $Version) {
    $Version = (git describe --tags --always --dirty 2>$null)
    if (-not $Version) { $Version = "dev" }
}
$Commit = (git rev-parse --short HEAD 2>$null)
if (-not $Commit) { $Commit = "unknown" }
$Date = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

$out = "dist"
if (Test-Path $out) { Remove-Item -Recurse -Force $out }
New-Item -ItemType Directory -Path $out | Out-Null

$platforms = @(
    @{ os = "linux";   arch = "amd64" },
    @{ os = "linux";   arch = "arm64" },
    @{ os = "darwin";  arch = "amd64" },
    @{ os = "darwin";  arch = "arm64" },
    @{ os = "windows"; arch = "amd64" },
    @{ os = "windows"; arch = "arm64" }
)

Write-Host "AetherTunnel $Version ($Commit, $Date)`n"

$env:CGO_ENABLED = "0"
foreach ($platform in $platforms) {
    $env:GOOS = $platform.os
    $env:GOARCH = $platform.arch
    $suffix = ""
    if ($platform.os -eq "windows") { $suffix = ".exe" }

    foreach ($target in @("server", "client")) {
        $pkg = "."
        if ($target -eq "client") { $pkg = "./client" }
        $name = "aethertunnel-$target-$($platform.os)-$($platform.arch)$suffix"
        $ldflags = "-s -w -X main.version=$Version -X main.buildTime=$Date -X main.gitCommit=$Commit"
        go build -trimpath -ldflags $ldflags -o (Join-Path $out $name) $pkg
        if ($LASTEXITCODE -ne 0) { throw "build failed: $name" }
        Write-Host ("  {0,-44} ok" -f $name)
    }
}
Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue

$sums = Get-ChildItem $out -File | Sort-Object Name | ForEach-Object {
    "{0}  {1}" -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name
}
$sums | Set-Content (Join-Path $out "SHA256SUMS") -Encoding ASCII

Write-Host "`nchecksums:" -ForegroundColor Cyan
$sums | ForEach-Object { Write-Host $_ }
Write-Host "`n$($sums.Count) artifacts in $out\"
