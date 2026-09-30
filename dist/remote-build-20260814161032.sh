set -euo pipefail
VERSION=7.2.131.0003
COMMIT=66e43fef-dirty
BUILT_AT=2026-08-14T08:10:32Z
STAMP=20260814161032
SRC_PKG=/tmp/cliproxy-source-$VERSION-$STAMP.tar.gz
BUILD_DIR=/tmp/cliproxy-build-$VERSION-$STAMP
STAGE=/tmp/cliproxy-stage-$VERSION-$STAMP
OUT=/tmp/cli-proxy-api-$VERSION-linux-arm64.tar.gz

rm -rf "$BUILD_DIR" "$STAGE"
mkdir -p "$BUILD_DIR" "$STAGE"
tar -xzf "$SRC_PKG" -C "$BUILD_DIR"
cd "$BUILD_DIR"

grep -n 'LatencyMs:' internal/redisqueue/plugin.go
if grep -q 'latency = time.Duration(generation' internal/redisqueue/plugin.go; then
  echo 'BAD: latency rewrite still present'; exit 2
fi
echo 'OK: latency not rewritten to generation'

env GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.Version=$VERSION -X main.Commit=$COMMIT -X main.BuildDate=$BUILT_AT" -o "$STAGE/cli-proxy-api" ./cmd/server

cp -f LICENSE README.md README_CN.md config.example.yaml "$STAGE/"
chmod 755 "$STAGE/cli-proxy-api"
tar -czf "$OUT" -C "$STAGE" cli-proxy-api LICENSE README.md README_CN.md config.example.yaml
"$STAGE/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version'
test -s "$STAGE/config.example.yaml"
printf 'REMOTE_PACKAGE=%s\n' "$OUT"