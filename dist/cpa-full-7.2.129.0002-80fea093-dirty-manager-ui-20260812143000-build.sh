set -euo pipefail
DEPLOY_ID='cpa-full-7.2.129.0002-80fea093-dirty-manager-ui-20260812143000'
VERSION='7.2.129.0002'
COMMIT='80fea093-dirty'
BUILT_AT='2026-08-12T06:30:00Z'
SOURCE_ARCHIVE="/tmp/${DEPLOY_ID}-source.tar.gz"
BUILD_DIRECTORY="/tmp/${DEPLOY_ID}-build"
STAGE_DIRECTORY="/tmp/${DEPLOY_ID}-stage"
rm -rf "$BUILD_DIRECTORY" "$STAGE_DIRECTORY"
mkdir -p "$BUILD_DIRECTORY" "$STAGE_DIRECTORY/plugins/linux/arm64"
tar -xzf "$SOURCE_ARCHIVE" -C "$BUILD_DIRECTORY"
(
  cd "$BUILD_DIRECTORY"
  env GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.Version=$VERSION -X main.Commit=$COMMIT -X main.BuildDate=$BUILT_AT" -o "$STAGE_DIRECTORY/cli-proxy-api" ./cmd/server
)
test -s "$STAGE_DIRECTORY/cli-proxy-api"
chmod 755 "$STAGE_DIRECTORY/cli-proxy-api"
if [ -f "$BUILD_DIRECTORY/config.example.yaml" ]; then
  cp "$BUILD_DIRECTORY/config.example.yaml" "$STAGE_DIRECTORY/config.example.yaml"
fi
for plugin in cpa-codex-openai-context cpa-prompt-cache-usage cpa-compact-route-rewriter cpa-antigravity-priority-scheduler cpa-strip-visible-files cpa-xai-ip-switcher; do
  test -d "$BUILD_DIRECTORY/plugins/src/$plugin"
  (
    cd "$BUILD_DIRECTORY/plugins/src/$plugin"
    env GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o "$STAGE_DIRECTORY/plugins/linux/arm64/$plugin.so"
  )
  test -s "$STAGE_DIRECTORY/plugins/linux/arm64/$plugin.so"
done
"$STAGE_DIRECTORY/cli-proxy-api" --help 2>&1 | grep "CLIProxyAPI Version: $VERSION"
test -s "$STAGE_DIRECTORY/plugins/linux/arm64/cpa-xai-ip-switcher.so"
