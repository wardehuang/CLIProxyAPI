#!/usr/bin/env bash
set -euo pipefail
VERSION=7.2.131.0001
COMMIT=66e43fef-dirty
BUILT_AT=2026-08-13T15:18:47Z
STAMP=20260813231846
SRC_PKG=/tmp/cliproxy-source-${VERSION}-${STAMP}.tar.gz
BUILD_DIR=/tmp/cliproxy-build-${VERSION}-${STAMP}
STAGE=/tmp/cliproxy-stage-${VERSION}-${STAMP}
OUT=/tmp/cli-proxy-api-${VERSION}-linux-arm64.tar.gz

echo "PYTHON_YAML=$(python3 -c 'import yaml; print(yaml.__version__)' 2>/dev/null || echo missing)"
echo "SRC_PKG_SIZE=$(stat -c '%s' "$SRC_PKG")"

rm -rf "$BUILD_DIR" "$STAGE"
mkdir -p "$BUILD_DIR" "$STAGE"
tar -xzf "$SRC_PKG" -C "$BUILD_DIR"
cd "$BUILD_DIR"

env GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.Version=$VERSION -X main.Commit=$COMMIT -X main.BuildDate=$BUILT_AT" -o "$STAGE/cli-proxy-api" ./cmd/server

cp -f LICENSE README.md README_CN.md config.example.yaml "$STAGE/"
chmod 755 "$STAGE/cli-proxy-api"
tar -czf "$OUT" -C "$STAGE" cli-proxy-api LICENSE README.md README_CN.md config.example.yaml

echo "VERSION_LINE=$("$STAGE/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version')"
test -s "$STAGE/config.example.yaml"
echo "BIN_SIZE=$(stat -c '%s' "$STAGE/cli-proxy-api")"
echo "PKG_SIZE=$(stat -c '%s' "$OUT")"
echo "REMOTE_PACKAGE=$OUT"
