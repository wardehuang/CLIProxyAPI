set -euo pipefail
APP=/opt/cli-proxy-api
VERSION=7.2.131.0003
PKG=/tmp/cli-proxy-api-$VERSION-linux-arm64.tar.gz
TS=$(date +%Y%m%d%H%M%S)
DEPLOY_ID="custom-$VERSION-$TS"
TMP=/tmp/cliproxy-deploy-$TS
BACKUP_DIR="$APP/backups"
DATA_BACKUP="$BACKUP_DIR/pre-$DEPLOY_ID.tar.gz"
BIN_BACKUP="$APP/cli-proxy-api.bak.$DEPLOY_ID"
CONFIG_BACKUP="$APP/config.yaml.bak.$DEPLOY_ID"
EXAMPLE_BACKUP="$APP/config.example.yaml.bak.$DEPLOY_ID"
LOG_DIR="$APP/deploy-logs"
LOG_FILE="$LOG_DIR/$DEPLOY_ID.log"
HEALTH_WAIT_SEC=90
CONFIG_MERGE_STATUS=not-run

mkdir -p "$BACKUP_DIR" "$TMP" "$LOG_DIR"
exec > >(tee -a "$LOG_FILE") 2>&1
echo "=== deploy start $DEPLOY_ID ==="
date -u
echo "PKG=$PKG"

tar -xzf "$PKG" -C "$TMP"
chmod +x "$TMP/cli-proxy-api"
if ! "$TMP/cli-proxy-api" --help 2>&1 | grep -q "CLIProxyAPI Version: $VERSION"; then
  echo "package version check failed" >&2
  "$TMP/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version' || true
  exit 1
fi

restart_old() {
  echo "=== rollback to previous binary ==="
  if [ -f "$BIN_BACKUP" ]; then
    cp -a "$BIN_BACKUP" "$APP/cli-proxy-api"
    chmod 755 "$APP/cli-proxy-api"
  fi
  if [ -f "$CONFIG_BACKUP" ]; then
    cp -a "$CONFIG_BACKUP" "$APP/config.yaml"
  fi
  if [ -f "$EXAMPLE_BACKUP" ]; then
    cp -a "$EXAMPLE_BACKUP" "$APP/config.example.yaml"
  fi
  sudo systemctl start cli-proxy-api >/dev/null 2>&1 || true
}
trap 'echo DEPLOY_FAILED; restart_old; exit 1' ERR

echo "=== stop service ==="
sudo systemctl stop cli-proxy-api

echo "=== backup ==="
cd "$APP"
items=()
for p in cli-proxy-api config.yaml auths gitstore objectstore pgstore static logs plugins .env LICENSE README.md README_CN.md config.example.yaml; do
  if [ -e "$p" ]; then
    items+=("$p")
  fi
done
tar -czf "$DATA_BACKUP" "${items[@]}"
cp -a "$APP/cli-proxy-api" "$BIN_BACKUP"
[ -f "$APP/config.yaml" ] && cp -a "$APP/config.yaml" "$CONFIG_BACKUP"
[ -f "$APP/config.example.yaml" ] && cp -a "$APP/config.example.yaml" "$EXAMPLE_BACKUP"

echo "=== install binary and docs ==="
install -m 755 "$TMP/cli-proxy-api" "$APP/cli-proxy-api.new"
mv "$APP/cli-proxy-api.new" "$APP/cli-proxy-api"
cp -f "$TMP/LICENSE" "$TMP/README.md" "$TMP/README_CN.md" "$TMP/config.example.yaml" "$APP/"
CONFIG_MERGE_STATUS=skipped-preserve-existing

echo "=== start service ==="
sudo systemctl start cli-proxy-api

echo "=== health wait ${HEALTH_WAIT_SEC}s ==="
ready=0
deadline=$((SECONDS + HEALTH_WAIT_SEC))
while [ $SECONDS -lt $deadline ]; do
  if systemctl is-active --quiet cli-proxy-api; then
    code=$(curl -s -o /tmp/cpa-health-body.txt -w '%{http_code}' --max-time 3 http://127.0.0.1:18457/v0/management/config || true)
    if [ "$code" = "200" ] || [ "$code" = "401" ] || [ "$code" = "403" ]; then
      echo "health ok http=$code elapsed=$((HEALTH_WAIT_SEC - (deadline - SECONDS)))s"
      ready=1
      break
    fi
    echo "waiting active but http=$code"
  else
    echo "waiting systemctl not active"
  fi
  sleep 2
done

if [ "$ready" != "1" ]; then
  echo "health check failed after ${HEALTH_WAIT_SEC}s" >&2
  systemctl is-active cli-proxy-api || true
  journalctl -u cli-proxy-api -n 40 --no-pager || true
  exit 1
fi

echo "=== version check ==="
/opt/cli-proxy-api/cli-proxy-api --help 2>&1 | grep 'CLIProxyAPI Version'
systemctl is-active cli-proxy-api

code=$(curl -s -D /tmp/cpa-headers.txt -o /dev/null --max-time 5 http://127.0.0.1:18457/ || true)
echo "root_http=$code"
grep -iE 'x-cpa-|x-server-' /tmp/cpa-headers.txt || true

if [ -d "$APP/plugins/linux/arm64" ]; then
  ls -la "$APP/plugins/linux/arm64" | sed -n '1,30p'
fi

echo "CONFIG_MERGE_STATUS=$CONFIG_MERGE_STATUS"
echo "DATA_BACKUP=$DATA_BACKUP"
echo "BIN_BACKUP=$BIN_BACKUP"
echo "LOG_FILE=$LOG_FILE"
echo "DEPLOY_OK=$DEPLOY_ID"

rm -rf "$TMP" /tmp/cliproxy-build-$VERSION-* /tmp/cliproxy-stage-$VERSION-* /tmp/cliproxy-source-$VERSION-*.tar.gz /tmp/cli-proxy-api-$VERSION-linux-arm64.tar.gz || true
echo "=== cleanup done ==="