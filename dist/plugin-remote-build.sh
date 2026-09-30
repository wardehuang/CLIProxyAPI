#!/usr/bin/env bash
set -euo pipefail
DEPLOY_ID=cpa-plugins-66e43fef-dirty-20260813233027
SRC_PKG=/tmp/${DEPLOY_ID}-source.tar.gz
BUILD_DIR=/tmp/${DEPLOY_ID}-build
STAGE=/tmp/${DEPLOY_ID}-stage
GO_BIN=/usr/local/go/bin/go

echo "GO_VERSION=$($GO_BIN env GOVERSION)"
echo "GOARCH=$($GO_BIN env GOARCH)"
echo "SRC_PKG_SIZE=$(stat -c '%s' "$SRC_PKG")"

rm -rf "$BUILD_DIR" "$STAGE"
mkdir -p "$BUILD_DIR" "$STAGE/plugins/linux/arm64"
tar -xzf "$SRC_PKG" -C "$BUILD_DIR"

export GOTOOLCHAIN=auto
for plugin in cpa-codex-openai-context cpa-prompt-cache-usage cpa-compact-route-rewriter cpa-antigravity-priority-scheduler cpa-strip-visible-files cpa-xai-ip-switcher cpa-claude-mem-adapter; do
  test -d "$BUILD_DIR/plugins/src/$plugin"
  echo "BUILD_START=$plugin"
  ( cd "$BUILD_DIR/plugins/src/$plugin" && env GOOS=linux GOARCH=arm64 CGO_ENABLED=1 GOTOOLCHAIN=auto "$GO_BIN" build -trimpath -buildmode=c-shared -o "$STAGE/plugins/linux/arm64/$plugin.so" )
  test -s "$STAGE/plugins/linux/arm64/$plugin.so"
  echo "BUILD_OK=$plugin size=$(stat -c '%s' "$STAGE/plugins/linux/arm64/$plugin.so")"
done

ls -lh "$STAGE/plugins/linux/arm64"
echo "STAGE=$STAGE"
