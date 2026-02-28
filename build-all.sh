#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUT_DIR="$ROOT_DIR/dist"
APP="workspace-sync"
PKG="./cmd/workspace-sync"

mkdir -p "$OUT_DIR"

build_one() {
  local goos="$1"
  local goarch="$2"
  local out="$3"

  echo "==> Building $out ($goos/$goarch)"
  (
    cd "$ROOT_DIR"
    GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
      go build -trimpath -ldflags='-s -w' -o "$OUT_DIR/$out" "$PKG"
  )
}

LINUX_AMD64="$APP-linux-amd64"
DARWIN_ARM64="$APP-darwin-arm64"
WINDOWS_AMD64="$APP-windows-amd64.exe"

build_one linux amd64 "$LINUX_AMD64"
build_one darwin arm64 "$DARWIN_ARM64"
build_one windows amd64 "$WINDOWS_AMD64"

echo
echo "Build completed:"
ls -lh "$OUT_DIR/$LINUX_AMD64" "$OUT_DIR/$DARWIN_ARM64" "$OUT_DIR/$WINDOWS_AMD64"

echo
echo "SHA256:"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$OUT_DIR/$LINUX_AMD64" "$OUT_DIR/$DARWIN_ARM64" "$OUT_DIR/$WINDOWS_AMD64"
else
  shasum -a 256 "$OUT_DIR/$LINUX_AMD64" "$OUT_DIR/$DARWIN_ARM64" "$OUT_DIR/$WINDOWS_AMD64"
fi
