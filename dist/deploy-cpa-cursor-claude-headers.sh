#!/usr/bin/env bash
set -euo pipefail

DEPLOY_ID="${1:?deploy id required}"
SRC_PKG="/tmp/${DEPLOY_ID}-source.tar.gz"
BUILD_DIR="/tmp/${DEPLOY_ID}-build"
STAGE="/tmp/${DEPLOY_ID}-stage"
PLUGIN="cpa-cursor-claude-headers"
APP="/opt/cli-proxy-api"
TS="$(date +%Y%m%d%H%M%S)"
BACKUP_DIR="${APP}/backups"
DEPLOY_LOG_DIR="${APP}/deploy-logs"
PLUGIN_BACKUP="${BACKUP_DIR}/plugins-pre-${DEPLOY_ID}-${TS}.tar.gz"
LOG="${DEPLOY_LOG_DIR}/${DEPLOY_ID}.log"

mkdir -p "${DEPLOY_LOG_DIR}" "${BACKUP_DIR}"
exec > >(sudo tee -a "${LOG}") 2>&1

echo "=== build+deploy ${DEPLOY_ID} ==="
echo "start=$(date -Is)"

test -s "${SRC_PKG}"
rm -rf "${BUILD_DIR}" "${STAGE}"
mkdir -p "${BUILD_DIR}" "${STAGE}/plugins/linux/arm64"
tar -xzf "${SRC_PKG}" -C "${BUILD_DIR}"
test -d "${BUILD_DIR}/plugins/src/${PLUGIN}"

export GOTOOLCHAIN=auto
export GOOS=linux
export GOARCH=arm64
export CGO_ENABLED=1
which go
go version
which gcc || true

(
  cd "${BUILD_DIR}/plugins/src/${PLUGIN}"
  go build -trimpath -buildmode=c-shared -o "${STAGE}/plugins/linux/arm64/${PLUGIN}.so" .
)
test -s "${STAGE}/plugins/linux/arm64/${PLUGIN}.so"
ls -lh "${STAGE}/plugins/linux/arm64/${PLUGIN}.so"

rollback() {
  echo "plugin deploy failed; rolling back from ${PLUGIN_BACKUP}"
  sudo systemctl stop cli-proxy-api || true
  if [ -s "${PLUGIN_BACKUP}" ]; then
    sudo tar -xzf "${PLUGIN_BACKUP}" -C "${APP}"
  fi
  sudo systemctl start cli-proxy-api || true
  sudo systemctl is-active cli-proxy-api || true
}
trap 'rollback' ERR

mkdir -p "${APP}/plugins/linux/arm64"
if [ -d "${APP}/plugins" ]; then
  sudo tar -czf "${PLUGIN_BACKUP}" -C "${APP}" plugins
  echo "plugin_backup=${PLUGIN_BACKUP}"
fi

sudo install -m 755 "${STAGE}/plugins/linux/arm64/${PLUGIN}.so" "${APP}/plugins/linux/arm64/${PLUGIN}.so"
ls -lh "${APP}/plugins/linux/arm64/${PLUGIN}.so"

# Ensure plugins.configs entry exists without overwriting existing values.
python3 - <<'PY'
import pathlib
import re

cfg_path = pathlib.Path("/opt/cli-proxy-api/config.yaml")
text = cfg_path.read_text(encoding="utf-8")
plugin_name = "cpa-cursor-claude-headers"
if re.search(rf"(?m)^\s*{re.escape(plugin_name)}\s*:", text):
    print("config: plugin entry already present, leave unchanged")
else:
    bak = pathlib.Path(f"/opt/cli-proxy-api/backups/config.yaml.pre-{plugin_name}")
    bak.parent.mkdir(parents=True, exist_ok=True)
    bak.write_text(text, encoding="utf-8")
    snippet = (
        f"    {plugin_name}:\n"
        f"      enabled: true\n"
        f"      priority: 5\n"
        f"      debug: false\n"
    )
    match = re.search(r"(?m)^(\s*)configs:\s*$", text)
    if match:
        index = match.end()
        text = text[:index] + "\n" + snippet + text[index:]
        cfg_path.write_text(text, encoding="utf-8")
        print("config: inserted plugin entry under plugins.configs")
    else:
        block = (
            "\nplugins:\n"
            "  enabled: true\n"
            "  dir: plugins\n"
            "  configs:\n"
            + snippet
        )
        cfg_path.write_text(text.rstrip() + block + "\n", encoding="utf-8")
        print("config: appended plugins.configs block")
PY

sudo systemctl restart cli-proxy-api
sleep 4
sudo systemctl is-active cli-proxy-api
trap - ERR
echo "plugin deploy succeeded"
ls -lh "${APP}/plugins/linux/arm64"/cpa-*.so
pid="$(systemctl show -p MainPID --value cli-proxy-api)"
echo "pid=${pid}"
sudo journalctl -u cli-proxy-api _PID="${pid}" --no-pager -n 300 | grep -E "pluginhost:|cpa-cursor-claude-headers|loaded plugin|request interceptor|failed to load" || true
echo "end=$(date -Is)"
echo "DEPLOY_LOG=${LOG}"
echo "PLUGIN_BACKUP=${PLUGIN_BACKUP}"
