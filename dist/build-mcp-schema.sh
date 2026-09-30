set -euo pipefail
DEPLOY_ID=cpa-mcp-schema-patch-219a2d86-dirty-20260727104546
SRC_PKG=/tmp/$DEPLOY_ID-source.tar.gz
BUILD_DIR=/tmp/$DEPLOY_ID-build
STAGE=/tmp/$DEPLOY_ID-stage

rm -rf "$BUILD_DIR" "$STAGE"
mkdir -p "$BUILD_DIR" "$STAGE/plugins/linux/arm64"
tar -xzf "$SRC_PKG" -C "$BUILD_DIR"

export GOTOOLCHAIN=auto
test -d "$BUILD_DIR/plugins/src/cpa-mcp-schema-patch"
cd "$BUILD_DIR/plugins/src/cpa-mcp-schema-patch"
env GOOS=linux GOARCH=arm64 CGO_ENABLED=1 GOTOOLCHAIN=auto go build -trimpath -buildmode=c-shared -o "$STAGE/plugins/linux/arm64/cpa-mcp-schema-patch.so"
test -s "$STAGE/plugins/linux/arm64/cpa-mcp-schema-patch.so"
ls -lh "$STAGE/plugins/linux/arm64"