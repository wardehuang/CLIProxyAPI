#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   bash remote-build-cpa-main.sh <version> <commit> <built_at_utc> <source_tarball> <out_binary_tarball>
# Builds Linux arm64 CGO binary on Oracle host from uploaded source tarball.

export PATH=/usr/local/go/bin:${PATH:-}

VERSION="${1:?version}"
COMMIT="${2:?commit}"
BUILT_AT="${3:?built_at}"
SRC_TGZ="${4:?source_tarball}"
OUT_TGZ="${5:?out_binary_tarball}"

VERSION=$(printf '%s' "$VERSION" | tr -d '\r\n')
COMMIT=$(printf '%s' "$COMMIT" | tr -d '\r\n')
BUILT_AT=$(printf '%s' "$BUILT_AT" | tr -d '\r\n')
SRC_TGZ=$(printf '%s' "$SRC_TGZ" | tr -d '\r\n')
OUT_TGZ=$(printf '%s' "$OUT_TGZ" | tr -d '\r\n')

TS=$(date +%Y%m%d%H%M%S)
WORKDIR=/tmp/cliproxy-build-${VERSION}-${TS}
mkdir -p "$WORKDIR/src"
trap 'rm -rf "$WORKDIR"' EXIT

echo "== build meta =="
echo "VERSION=$VERSION"
echo "COMMIT=$COMMIT"
echo "BUILT_AT=$BUILT_AT"
echo "SRC_TGZ=$SRC_TGZ"
echo "OUT_TGZ=$OUT_TGZ"
go version
echo "CGO probe:"
CGO_ENABLED=1 go env CGO_ENABLED CC GOARCH GOOS

if [ ! -f "$SRC_TGZ" ]; then
  echo "missing source tarball: $SRC_TGZ" >&2
  exit 1
fi

tar -xzf "$SRC_TGZ" -C "$WORKDIR/src"

# spot-check no CR in critical files
python3 - <<PY
from pathlib import Path
root = Path("$WORKDIR/src")
rels = [
    "cmd/server/main.go",
    "go.mod",
    "config.example.yaml",
    "internal/runtime/executor/openai_compat_executor.go",
    "internal/runtime/executor/helps/openai_compat_protocol.go",
]
for rel in rels:
    p = root / rel
    data = p.read_bytes()
    if b"\r" in data:
        raise SystemExit(f"CR present in {rel}")
    print(f"remote spot-check ok: {rel} bytes={len(data)}")
PY

cd "$WORKDIR/src"
LDFLAGS="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILT_AT}"
echo "building..."
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o "$WORKDIR/cli-proxy-api" ./cmd/server

chmod 755 "$WORKDIR/cli-proxy-api"
VERSION_LINE=$("$WORKDIR/cli-proxy-api" --help 2>&1 | tr -d '\r' | grep 'CLIProxyAPI Version' || true)
echo "VERSION_LINE=$VERSION_LINE"
if ! printf '%s\n' "$VERSION_LINE" | tr -d '\r' | grep -Fq "CLIProxyAPI Version: ${VERSION}, Commit: ${COMMIT}, BuiltAt: ${BUILT_AT}"; then
  echo "version mismatch" >&2
  exit 1
fi

STAGE="$WORKDIR/stage"
mkdir -p "$STAGE"
cp -a "$WORKDIR/cli-proxy-api" "$STAGE/cli-proxy-api"
cp -a LICENSE README.md README_CN.md config.example.yaml "$STAGE/" 2>/dev/null || true
tar -czf "$OUT_TGZ" -C "$STAGE" .
ls -la "$OUT_TGZ"
echo "BUILD_OK=$OUT_TGZ"
