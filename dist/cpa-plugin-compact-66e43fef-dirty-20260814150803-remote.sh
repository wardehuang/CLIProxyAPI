set -euo pipefail
DEPLOY_ID=cpa-plugin-compact-66e43fef-dirty-20260814150803
SRC_PKG=/tmp/$DEPLOY_ID-source.tar.gz
BUILD_DIR=/tmp/$DEPLOY_ID-build
STAGE=/tmp/$DEPLOY_ID-stage
PLUGIN=cpa-compact-route-rewriter

rm -rf "$BUILD_DIR" "$STAGE"
mkdir -p "$BUILD_DIR" "$STAGE/plugins/linux/arm64"
tar -xzf "$SRC_PKG" -C "$BUILD_DIR"

test -d "$BUILD_DIR/plugins/src/$PLUGIN"
( cd "$BUILD_DIR/plugins/src/$PLUGIN" && env GOOS=linux GOARCH=arm64 CGO_ENABLED=1 GOTOOLCHAIN=auto /usr/local/go/bin/go build -trimpath -buildmode=c-shared -o "$STAGE/plugins/linux/arm64/$PLUGIN.so" )
test -s "$STAGE/plugins/linux/arm64/$PLUGIN.so"
ls -lh "$STAGE/plugins/linux/arm64/$PLUGIN.so"

APP=/opt/cli-proxy-api
TS=$(date +%Y%m%d%H%M%S)
BACKUP_DIR="$APP/backups"
DEPLOY_LOG_DIR="$APP/deploy-logs"
PLUGIN_BACKUP="$BACKUP_DIR/plugins-pre-$DEPLOY_ID-$TS.tar.gz"
LOG="$DEPLOY_LOG_DIR/$DEPLOY_ID.log"

mkdir -p "$BACKUP_DIR" "$DEPLOY_LOG_DIR" "$APP/plugins/linux/arm64"
exec > >(sudo tee -a "$LOG") 2>&1

echo "deploy_id=$DEPLOY_ID"
echo "plugin=$PLUGIN"
echo "plugin_backup=$PLUGIN_BACKUP"

rollback() {
  echo "plugin deploy failed; rolling back from $PLUGIN_BACKUP"
  sudo systemctl stop cli-proxy-api || true
  if [ -s "$PLUGIN_BACKUP" ]; then
    sudo tar -xzf "$PLUGIN_BACKUP" -C "$APP"
  fi
  sudo systemctl start cli-proxy-api || true
  sudo systemctl is-active cli-proxy-api || true
}
trap 'rollback' ERR

if [ -d "$APP/plugins" ]; then
  sudo tar -czf "$PLUGIN_BACKUP" -C "$APP" plugins
fi

sudo install -m 755 "$STAGE/plugins/linux/arm64/$PLUGIN.so" "$APP/plugins/linux/arm64/$PLUGIN.so"

sudo systemctl restart cli-proxy-api
sleep 3
sudo systemctl is-active cli-proxy-api

trap - ERR
echo "plugin deploy succeeded"
printf 'PLUGIN_BACKUP=%s\n' "$PLUGIN_BACKUP"
printf 'DEPLOY_LOG=%s\n' "$LOG"
ls -lh "$APP/plugins/linux/arm64/$PLUGIN.so"

pid=$(systemctl show -p MainPID --value cli-proxy-api)
sudo journalctl -u cli-proxy-api _PID=$pid --no-pager | grep -E 'pluginhost:|cpa-compact-route-rewriter' || true

rm -rf "$BUILD_DIR" "$STAGE" "$SRC_PKG"
echo "cleanup done"