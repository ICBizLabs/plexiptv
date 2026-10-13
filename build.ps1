# Builds release binaries into .\dist for Windows and Linux.
#   powershell -ExecutionPolicy Bypass -File build.ps1 [-Version 1.0.0]
param([string]$Version = "dev")
$ErrorActionPreference = "Stop"
$env:CGO_ENABLED = "0"
New-Item -ItemType Directory -Force -Path dist | Out-Null
$targets = @(
    @{ os = "windows"; arch = "amd64"; ext = ".exe" },
    @{ os = "linux";   arch = "amd64"; ext = "" },
    @{ os = "linux";   arch = "arm64"; ext = "" }
)
foreach ($t in $targets) {
    $env:GOOS = $t.os; $env:GOARCH = $t.arch
    $out = "dist/plexiptv-$($t.os)-$($t.arch)$($t.ext)"
    Write-Host "building $out"
    go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $out ./cmd/plexiptv
}
Remove-Item Env:GOOS, Env:GOARCH
