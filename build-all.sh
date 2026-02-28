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

build_one linux amd64  "$APP-linux-amd64"
build_one darwin arm64 "$APP-darwin-arm64"

echo

echo "Build completed:"
ls -lh "$OUT_DIR/$APP-linux-amd64" "$OUT_DIR/$APP-darwin-arm64"

echo

echo "SHA256:"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$OUT_DIR/$APP-linux-amd64" "$OUT_DIR/$APP-darwin-arm64"
else
  shasum -a 256 "$OUT_DIR/$APP-linux-amd64" "$OUT_DIR/$APP-darwin-arm64"
fi
