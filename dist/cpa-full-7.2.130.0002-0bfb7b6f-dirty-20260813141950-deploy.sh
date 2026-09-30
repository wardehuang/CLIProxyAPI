set -euo pipefail
APP=/opt/cli-proxy-api
DEPLOY_ID=cpa-full-7.2.130.0002-0bfb7b6f-dirty-20260813141950
VERSION=7.2.130.0002
STAGE=/tmp/cpa-full-7.2.130.0002-0bfb7b6f-dirty-20260813141950-stage
TIMESTAMP=$(date +%Y%m%d%H%M%S)
BACKUP_DIR="$APP/backups"
DEPLOY_LOG_DIR="$APP/deploy-logs"
BACKUP="$BACKUP_DIR/pre-$DEPLOY_ID-$TIMESTAMP.tar.gz"
LOG="$DEPLOY_LOG_DIR/$DEPLOY_ID.log"
mkdir -p "$BACKUP_DIR" "$DEPLOY_LOG_DIR" "$APP/plugins/linux/arm64"
exec > >(sudo tee -a "$LOG") 2>&1
echo "deploy_id=$DEPLOY_ID"
echo "version=$VERSION"
echo "backup=$BACKUP"
rollback() {
  echo "deploy failed; rolling back from $BACKUP"
  sudo systemctl stop cli-proxy-api || true
  if [ -s "$BACKUP" ]; then sudo tar -xzf "$BACKUP" -C "$APP"; fi
  sudo systemctl start cli-proxy-api || true
  sudo systemctl is-active cli-proxy-api || true
}
trap rollback ERR
sudo tar -czf "$BACKUP" -C "$APP" cli-proxy-api config.example.yaml plugins 2>/tmp/$DEPLOY_ID-backup-warn.log || true
sudo systemctl stop cli-proxy-api
sudo install -m 755 "$STAGE/cli-proxy-api" "$APP/cli-proxy-api"
if [ -f "$STAGE/config.example.yaml" ]; then sudo install -m 644 "$STAGE/config.example.yaml" "$APP/config.example.yaml"; fi
for plugin in cpa-codex-openai-context cpa-prompt-cache-usage cpa-compact-route-rewriter cpa-antigravity-priority-scheduler cpa-strip-visible-files cpa-xai-ip-switcher; do
  sudo install -m 755 "$STAGE/plugins/linux/arm64/$plugin.so" "$APP/plugins/linux/arm64/$plugin.so"
done
sudo systemctl start cli-proxy-api
sleep 3
sudo systemctl is-active cli-proxy-api
"$APP/cli-proxy-api" --help 2>&1 | grep "CLIProxyAPI Version: $VERSION"
trap - ERR
echo "deploy succeeded"
printf 'BACKUP=%s\n' "$BACKUP"
printf 'DEPLOY_LOG=%s\n' "$LOG"
ls -lh "$APP/cli-proxy-api" "$APP/plugins/linux/arm64"/cpa-*.so