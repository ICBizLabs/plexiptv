# Builds release binaries into .\dist for Windows and Linux.
#   powershell -ExecutionPolicy Bypass -File build.ps1 [-Version 1.1]
#   (default: the version in cmd/plexiptv/main.go)
param([string]$Version = "")
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
    $ldflags = "-s -w"
    if ($Version) { $ldflags += " -X main.version=$Version" }
    go build -trimpath -ldflags $ldflags -o $out ./cmd/plexiptv
}
Remove-Item Env:GOOS, Env:GOARCH
