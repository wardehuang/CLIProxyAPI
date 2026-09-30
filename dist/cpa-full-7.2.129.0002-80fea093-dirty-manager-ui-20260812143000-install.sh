set -euo pipefail
APPLICATION_DIRECTORY=/opt/cli-proxy-api
DEPLOY_ID='cpa-full-7.2.129.0002-80fea093-dirty-manager-ui-20260812143000'
VERSION='7.2.129.0002'
STAGE_DIRECTORY="/tmp/${DEPLOY_ID}-stage"
TIMESTAMP=$(date +%Y%m%d%H%M%S)
BACKUP_DIRECTORY="${APPLICATION_DIRECTORY}/backups"
DEPLOY_LOG_DIRECTORY="${APPLICATION_DIRECTORY}/deploy-logs"
BACKUP_ARCHIVE="${BACKUP_DIRECTORY}/pre-${DEPLOY_ID}-${TIMESTAMP}.tar.gz"
DEPLOY_LOG="${DEPLOY_LOG_DIRECTORY}/${DEPLOY_ID}.log"
mkdir -p "$BACKUP_DIRECTORY" "$DEPLOY_LOG_DIRECTORY" "${APPLICATION_DIRECTORY}/plugins/linux/arm64"
exec > >(sudo tee -a "$DEPLOY_LOG") 2>&1
echo "deploy_id=${DEPLOY_ID}"
echo "version=${VERSION}"
echo "backup=${BACKUP_ARCHIVE}"
rollback() {
  echo "deployment failed; restoring ${BACKUP_ARCHIVE}"
  sudo systemctl stop cli-proxy-api || true
  if [ -s "$BACKUP_ARCHIVE" ]; then
    sudo tar -xzf "$BACKUP_ARCHIVE" -C "$APPLICATION_DIRECTORY"
  fi
  sudo systemctl start cli-proxy-api || true
  sudo systemctl is-active cli-proxy-api || true
}
trap rollback ERR
sudo tar -czf "$BACKUP_ARCHIVE" -C "$APPLICATION_DIRECTORY" cli-proxy-api config.yaml plugins
sudo systemctl stop cli-proxy-api
sudo install -m 755 "$STAGE_DIRECTORY/cli-proxy-api" "${APPLICATION_DIRECTORY}/cli-proxy-api"
if [ -f "$STAGE_DIRECTORY/config.example.yaml" ]; then
  sudo install -m 644 "$STAGE_DIRECTORY/config.example.yaml" "${APPLICATION_DIRECTORY}/config.example.yaml"
fi
for plugin in cpa-codex-openai-context cpa-prompt-cache-usage cpa-compact-route-rewriter cpa-antigravity-priority-scheduler cpa-strip-visible-files cpa-xai-ip-switcher; do
  sudo install -m 755 "$STAGE_DIRECTORY/plugins/linux/arm64/${plugin}.so" "${APPLICATION_DIRECTORY}/plugins/linux/arm64/${plugin}.so"
done
sudo systemctl start cli-proxy-api
sleep 3
sudo systemctl is-active cli-proxy-api
"${APPLICATION_DIRECTORY}/cli-proxy-api" --help 2>&1 | grep "CLIProxyAPI Version: ${VERSION}"
test -s "${APPLICATION_DIRECTORY}/plugins/linux/arm64/cpa-xai-ip-switcher.so"
trap - ERR
echo "deployment succeeded"
echo "backup=${BACKUP_ARCHIVE}"
echo "deploy_log=${DEPLOY_LOG}"
