#!/usr/bin/env sh
# Builds release binaries into ./dist for Windows and Linux.
#   ./build.sh [version]
set -e
VERSION="${1:-dev}"
export CGO_ENABLED=0
mkdir -p dist
for target in windows/amd64 linux/amd64 linux/arm64; do
	os="${target%/*}"
	arch="${target#*/}"
	ext=""
	[ "$os" = windows ] && ext=".exe"
	out="dist/plexiptv-$os-$arch$ext"
	echo "building $out"
	GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out" ./cmd/plexiptv
done
