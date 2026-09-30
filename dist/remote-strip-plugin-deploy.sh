#!/usr/bin/env bash
set -euo pipefail

PLUGIN_ID=cpa-strip-visible-files
APP=/opt/cli-proxy-api
PLUGIN_DIR="$APP/plugins/linux/arm64"
SRC_PKG="${SRC_PKG:?}"
DEPLOY_ID="${DEPLOY_ID:?}"
BUILD_DIR="/tmp/${DEPLOY_ID}-build"
STAGE="/tmp/${DEPLOY_ID}-plugin"
DEPLOY_LOG="$APP/deploy-logs/${DEPLOY_ID}.log"
BACKUP="$APP/backups/${PLUGIN_ID}.so.bak.${DEPLOY_ID}"

mkdir -p "$APP/deploy-logs" "$APP/backups"
exec > >(tee -a "$DEPLOY_LOG") 2>&1

echo "=== PLUGIN DEPLOY $PLUGIN_ID ==="
date -Is
echo "SRC_PKG=$SRC_PKG"

cleanup() {
  rm -rf "$BUILD_DIR" "$STAGE" || true
  rm -f "$SRC_PKG" || true
}

rollback() {
  echo "=== ROLLBACK ==="
  if [[ -f "$BACKUP" ]]; then
    cp -a "$BACKUP" "$PLUGIN_DIR/${PLUGIN_ID}.so"
    chmod 755 "$PLUGIN_DIR/${PLUGIN_ID}.so"
  fi
  sudo systemctl restart cli-proxy-api || true
  sleep 2
  systemctl is-active cli-proxy-api || true
}

trap 'echo ERROR; rollback; cleanup; exit 1' ERR

rm -rf "$BUILD_DIR" "$STAGE"
mkdir -p "$BUILD_DIR" "$STAGE"
tar -xzf "$SRC_PKG" -C "$BUILD_DIR"
cd "$BUILD_DIR/plugins/src/${PLUGIN_ID}"

export GOOS=linux GOARCH=arm64 CGO_ENABLED=1
go mod tidy
go build -buildmode=c-shared -o "$STAGE/${PLUGIN_ID}.so" .
test -s "$STAGE/${PLUGIN_ID}.so"

mkdir -p "$PLUGIN_DIR"
if [[ -f "$PLUGIN_DIR/${PLUGIN_ID}.so" ]]; then
  cp -a "$PLUGIN_DIR/${PLUGIN_ID}.so" "$BACKUP"
  echo "backup=$BACKUP"
fi
install -m 755 "$STAGE/${PLUGIN_ID}.so" "$PLUGIN_DIR/${PLUGIN_ID}.so"
ls -lh "$PLUGIN_DIR/${PLUGIN_ID}.so"

sudo systemctl restart cli-proxy-api
sleep 3
systemctl is-active --quiet cli-proxy-api
sudo journalctl -u cli-proxy-api --since '30 seconds ago' --no-pager | grep -E "cpa-strip-visible-files|plugin registered|error" | tail -n 20 || true

trap - ERR
cleanup
echo "=== PLUGIN DEPLOY SUCCESS ==="
date -Is
