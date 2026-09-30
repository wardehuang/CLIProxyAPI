set -euo pipefail
VERSION='7.2.130.0001'
COMMIT='7c592279-dirty'
BUILT_AT='2026-08-13T00:28:08Z'
SRC_PKG='/tmp/cliproxy-source-7.2.130.0001-20260813082807.tar.gz'
BUILD_DIR='/tmp/cliproxy-build-7.2.130.0001-20260813082807'
STAGE='/tmp/cliproxy-stage-7.2.130.0001-20260813082807'
OUT='/tmp/cli-proxy-api-7.2.130.0001-linux-arm64.tar.gz'

echo "BUILD_START VERSION=$VERSION COMMIT=$COMMIT"
rm -rf "$BUILD_DIR" "$STAGE"
mkdir -p "$BUILD_DIR" "$STAGE"
tar -xzf "$SRC_PKG" -C "$BUILD_DIR"
cd "$BUILD_DIR"
echo "GO_VERSION=$(go version)"
echo "BUILDING..."
LDFLAGS="-s -w -X main.Version=$VERSION -X main.Commit=$COMMIT -X main.BuildDate=$BUILT_AT"
env GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -trimpath -ldflags "$LDFLAGS" -o "$STAGE/cli-proxy-api" ./cmd/server
cp -f LICENSE README.md README_CN.md config.example.yaml "$STAGE/"
chmod 755 "$STAGE/cli-proxy-api"
tar -czf "$OUT" -C "$STAGE" cli-proxy-api LICENSE README.md README_CN.md config.example.yaml
echo "VERSION_CHECK:"
"$STAGE/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version'
test -s "$STAGE/config.example.yaml"
ls -lh "$OUT" "$STAGE/cli-proxy-api"
printf 'REMOTE_PACKAGE=%s\n' "$OUT"
echo BUILD_OK