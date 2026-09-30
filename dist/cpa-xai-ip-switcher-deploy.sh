set -euo pipefail
DEPLOY_ID=cpa-xai-ip-switcher-801fe4a1-dirty-20260811155716
SRC_PKG=/tmp/$DEPLOY_ID-source.tar.gz
BUILD_DIR=/tmp/$DEPLOY_ID-build
STAGE=/tmp/$DEPLOY_ID-stage
PLUGIN=cpa-xai-ip-switcher
APP=/opt/cli-proxy-api
TS=$(date +%Y%m%d%H%M%S)
BACKUP_DIR="$APP/backups"
DEPLOY_LOG_DIR="$APP/deploy-logs"
PLUGIN_BACKUP="$BACKUP_DIR/plugins-pre-$DEPLOY_ID-$TS.tar.gz"
LOG="$DEPLOY_LOG_DIR/$DEPLOY_ID.log"
DB="$APP/plugin-data/cpa-xai-ip-switcher/ip-switcher.sqlite3"

rm -rf "$BUILD_DIR" "$STAGE"
mkdir -p "$BUILD_DIR" "$STAGE/plugins/linux/arm64" "$BACKUP_DIR" "$DEPLOY_LOG_DIR"
exec > >(sudo tee -a "$LOG") 2>&1
echo "deploy_id=$DEPLOY_ID"
tar -xzf "$SRC_PKG" -C "$BUILD_DIR"
test -d "$BUILD_DIR/plugins/src/$PLUGIN"
( cd "$BUILD_DIR/plugins/src/$PLUGIN" && env GOOS=linux GOARCH=arm64 CGO_ENABLED=1 GOTOOLCHAIN=auto go build -trimpath -buildmode=c-shared -o "$STAGE/plugins/linux/arm64/$PLUGIN.so" )
test -s "$STAGE/plugins/linux/arm64/$PLUGIN.so"
ls -lh "$STAGE/plugins/linux/arm64/$PLUGIN.so"

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
echo "plugin_backup=$PLUGIN_BACKUP"

sudo install -m 755 "$STAGE/plugins/linux/arm64/$PLUGIN.so" "$APP/plugins/linux/arm64/$PLUGIN.so"
sudo systemctl restart cli-proxy-api
sleep 3
sudo systemctl is-active cli-proxy-api
trap - ERR

if [ -f "$DB" ]; then
  BEFORE=$(sudo sqlite3 "$DB" "SELECT COUNT(*) FROM plugin_logs WHERE category='realtime_guard';")
  echo "realtime_guard_logs_before=$BEFORE"
  sudo sqlite3 "$DB" "DELETE FROM plugin_logs WHERE category='realtime_guard';"
  AFTER=$(sudo sqlite3 "$DB" "SELECT COUNT(*) FROM plugin_logs WHERE category='realtime_guard';")
  echo "realtime_guard_logs_after=$AFTER"
else
  echo "db_missing=$DB"
fi

echo "plugin deploy succeeded"
ls -lh "$APP/plugins/linux/arm64/$PLUGIN.so"
rm -rf "$BUILD_DIR" "$STAGE" "$SRC_PKG"
echo CLEAN_OK