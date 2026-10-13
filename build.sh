#!/usr/bin/env sh
# Builds release binaries into ./dist for Windows and Linux.
#   ./build.sh [version]    (default: the version in cmd/plexiptv/main.go)
set -e
LDFLAGS="-s -w"
[ -n "$1" ] && LDFLAGS="$LDFLAGS -X main.version=$1"
export CGO_ENABLED=0
mkdir -p dist
for target in windows/amd64 linux/amd64 linux/arm64; do
	os="${target%/*}"
	arch="${target#*/}"
	ext=""
	[ "$os" = windows ] && ext=".exe"
	out="dist/plexiptv-$os-$arch$ext"
	echo "building $out"
	GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/plexiptv
done
