#!/bin/sh
# Reproducible cross-compile of the single `openite` binary into ./dist, plus SHA256SUMS. Needs Go 1.21+.
#   ./build.sh            # version from git tag or "dev"
#   VERSION=0.3.0 ./build.sh
set -e
cd "$(dirname "$0")"
VERSION=${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}
rm -rf dist && mkdir dist
for t in windows/amd64 windows/arm64 linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64; do
  os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=".exe"
  echo "building $os/$arch ($VERSION)"
  # CGO off + -trimpath + no VCS stamping => the same source gives the same bytes on any machine
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=$VERSION" \
    -o "dist/openite-$os-$arch$ext" ./cmd/openite
done
cd dist && (sha256sum openite-* 2>/dev/null || shasum -a 256 openite-*) > SHA256SUMS && cat SHA256SUMS
