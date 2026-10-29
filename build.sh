#!/usr/bin/env sh
# Cross-compile static single binaries into dist/.
set -eu
cd "$(dirname "$0")"
# Version: $VERSION, else the git tag, else the default in main.go.
VERSION="${VERSION:-$(git describe --tags 2>/dev/null || true)}"
LDFLAGS="-s -w"
if [ -n "$VERSION" ]; then LDFLAGS="$LDFLAGS -X main.version=${VERSION#v}"; fi
# Commit and build date for the About screen (plain "go build" in the repo
# also gets them from Go's VCS stamping).
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || true)"
if [ -n "$COMMIT" ]; then LDFLAGS="$LDFLAGS -X main.commit=$COMMIT"; fi
LDFLAGS="$LDFLAGS -X main.buildDate=$(date -u +%Y-%m-%d)"
rm -rf dist && mkdir -p dist
go vet ./...
go test ./...
# Linux first (desktop, server, Raspberry Pi); macOS as a bonus.
for target in linux/amd64 linux/arm64 darwin/arm64; do
  os="${target%/*}"; arch="${target#*/}"
  out="dist/indirgen-${os}-${arch}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" -o "$out" .
  echo "built $out"
done
# Self-update and release checks require this file: the app refuses to
# install a release it cannot verify.
(cd dist && if command -v sha256sum >/dev/null; then sha256sum indirgen-*; else shasum -a 256 indirgen-*; fi > SHA256SUMS)
echo "wrote dist/SHA256SUMS"
# The one-line installer travels with each release ("…/releases/latest/download/install.sh").
cp install.sh dist/install.sh
