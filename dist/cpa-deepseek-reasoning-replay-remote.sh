#!/usr/bin/env bash
set -euo pipefail

DEPLOY_ID="${DEPLOY_ID:?DEPLOY_ID required}"
PLUGIN="${PLUGIN:-cpa-deepseek-reasoning-replay}"
SRC_PKG="/tmp/${DEPLOY_ID}-source.tar.gz"
BUILD_DIR="/tmp/${DEPLOY_ID}-build"
STAGE="/tmp/${DEPLOY_ID}-stage"
APP="/opt/cli-proxy-api"
TS="$(date +%Y%m%d%H%M%S)"
BACKUP_DIR="${APP}/backups"
DEPLOY_LOG_DIR="${APP}/deploy-logs"
PLUGIN_BACKUP="${BACKUP_DIR}/plugins-pre-${DEPLOY_ID}-${TS}.tar.gz"
CONFIG_BACKUP="${BACKUP_DIR}/config.yaml.bak.plugins.${DEPLOY_ID}.${TS}"
LOG="${DEPLOY_LOG_DIR}/${DEPLOY_ID}.log"
GO_BIN="${GO_BIN:-/usr/local/go/bin/go}"
TARGET_SO="${APP}/plugins/linux/arm64/${PLUGIN}.so"
STAGE_SO="${STAGE}/plugins/linux/arm64/${PLUGIN}.so"
export BUILD_DIR DEPLOY_ID PLUGIN

mkdir -p "${BACKUP_DIR}" "${DEPLOY_LOG_DIR}" "${APP}/plugins/linux/arm64"
exec > >(sudo tee -a "${LOG}") 2>&1

echo "deploy_id=${DEPLOY_ID}"
echo "plugin=${PLUGIN}"
echo "plugin_backup=${PLUGIN_BACKUP}"
echo "config_backup=${CONFIG_BACKUP}"
echo "log=${LOG}"
echo "go_version=$(${GO_BIN} version | tr -d '\r')"
echo "gcc_version=$(gcc --version | head -1 | tr -d '\r')"

rm -rf "${BUILD_DIR}" "${STAGE}"
mkdir -p "${BUILD_DIR}" "${STAGE}/plugins/linux/arm64"
tar -xzf "${SRC_PKG}" -C "${BUILD_DIR}"
test -d "${BUILD_DIR}/plugins/src/${PLUGIN}"
test -f "${BUILD_DIR}/go.mod"

python3 - <<'PY'
from pathlib import Path
import os
root = Path(os.environ["BUILD_DIR"])
paths = [
    root / "go.mod",
    root / "plugins/src/cpa-deepseek-reasoning-replay/main.go",
    root / "plugins/src/cpa-deepseek-reasoning-replay/go.mod",
]
bad = []
for path in paths:
    data = path.read_bytes()
    if b"\r\n" in data or (b"\r" in data and not data.endswith(b"\r")):
        # allow lone CR only if none; any CR is bad for Go sources
        if b"\r" in data:
            bad.append(str(path))
if bad:
    raise SystemExit("CRLF found in: " + ", ".join(bad))
print("lf_spotcheck=ok")
PY

(
  cd "${BUILD_DIR}/plugins/src/${PLUGIN}"
  env GOOS=linux GOARCH=arm64 CGO_ENABLED=1 GOTOOLCHAIN=auto "${GO_BIN}" test .
  env GOOS=linux GOARCH=arm64 CGO_ENABLED=1 GOTOOLCHAIN=auto "${GO_BIN}" build -trimpath -buildmode=c-shared -o "${STAGE_SO}" .
)
test -s "${STAGE_SO}"
ls -lh "${STAGE_SO}"
file "${STAGE_SO}" | tr -d '\r'

rollback() {
  echo "plugin deploy failed; rolling back"
  sudo systemctl stop cli-proxy-api || true
  if [ -s "${PLUGIN_BACKUP}" ]; then
    sudo tar -xzf "${PLUGIN_BACKUP}" -C "${APP}"
  fi
  if [ -s "${CONFIG_BACKUP}" ]; then
    sudo cp -a "${CONFIG_BACKUP}" "${APP}/config.yaml"
  fi
  sudo systemctl start cli-proxy-api || true
  sudo systemctl is-active cli-proxy-api || true
}
trap 'rollback' ERR

if [ -d "${APP}/plugins" ]; then
  sudo tar -czf "${PLUGIN_BACKUP}" -C "${APP}" plugins
fi
if [ -f "${APP}/config.yaml" ]; then
  sudo cp -a "${APP}/config.yaml" "${CONFIG_BACKUP}"
fi

# Ensure plugin config exists and debug is enabled for this diagnostic build.
CONFIG_STATUS="$(python3 - <<'PY'
from pathlib import Path
import re

path = Path("/opt/cli-proxy-api/config.yaml")
text = path.read_text(encoding="utf-8")
needle = "cpa-deepseek-reasoning-replay:"
if needle not in text:
    block = """    cpa-deepseek-reasoning-replay:
      enabled: true
      priority: 10
      debug: true
      pad_placeholder: \" \"
      max_entries: 4096
      ttl_seconds: 3600
      model_substrings:
        - deepseek
"""
    lines = text.splitlines(keepends=True)
    insert_at = None
    for i, line in enumerate(lines):
        if line.startswith("    gemini-cli:"):
            insert_at = i
            break
    if insert_at is None:
        for i, line in enumerate(lines):
            if line.strip() == "configs:" and i > 0 and "plugins" in "".join(lines[max(0, i-5):i]):
                insert_at = i + 1
                break
    if insert_at is None:
        raise SystemExit("unable to locate plugins.configs insertion point")
    prefix = lines[:insert_at]
    suffix = lines[insert_at:]
    if prefix and not prefix[-1].endswith("\n"):
        prefix[-1] = prefix[-1] + "\n"
    if not block.endswith("\n"):
        block += "\n"
    path.write_text("".join(prefix) + block + "".join(suffix), encoding="utf-8")
    print("inserted-debug-true")
    raise SystemExit(0)

# Existing block: force enabled/debug true without touching unrelated keys.
pattern = re.compile(
    r"(?ms)^(?P<indent>[ \t]*)cpa-deepseek-reasoning-replay:\n(?P<body>(?:(?P=indent)[ \t]+.*\n)*)"
)
match = pattern.search(text)
if not match:
    raise SystemExit("plugin config block parse failed")
indent = match.group("indent")
body = match.group("body")
lines = body.splitlines(keepends=True)
out = []
seen_enabled = False
seen_debug = False
for line in lines:
    if re.match(rf"^{re.escape(indent)}[ \t]+enabled\s*:", line):
        out.append(f"{indent}  enabled: true\n")
        seen_enabled = True
        continue
    if re.match(rf"^{re.escape(indent)}[ \t]+debug\s*:", line):
        out.append(f"{indent}  debug: true\n")
        seen_debug = True
        continue
    out.append(line)
if not seen_enabled:
    out.insert(0, f"{indent}  enabled: true\n")
if not seen_debug:
    out.insert(1 if seen_enabled or out else 0, f"{indent}  debug: true\n")
new_block = f"{indent}cpa-deepseek-reasoning-replay:\n" + "".join(out)
text = text[:match.start()] + new_block + text[match.end():]
path.write_text(text, encoding="utf-8")
print("updated-debug-true")
PY
)"
echo "config_status=${CONFIG_STATUS}"

sudo install -m 755 "${STAGE_SO}" "${TARGET_SO}"
cmp -s "${STAGE_SO}" "${TARGET_SO}"
echo "installed_so=$(ls -lh "${TARGET_SO}" | tr -d '\r')"

sudo systemctl restart cli-proxy-api

ok=0
for i in $(seq 1 90); do
  active="$(systemctl is-active cli-proxy-api 2>/dev/null | tr -d '\r' || true)"
  if [ "${active}" = "active" ]; then
    code="$(curl -sS -m 2 -o "/tmp/${DEPLOY_ID}-health.body" -w '%{http_code}' http://127.0.0.1:18457/v1/models || true)"
    code="$(printf '%s' "${code}" | tr -d '\r')"
    if [ "${code}" = "200" ] || [ "${code}" = "401" ] || [ "${code}" = "403" ]; then
      ok=1
      echo "health_http=${code} attempt=${i}"
      break
    fi
  fi
  sleep 1
done
if [ "${ok}" != "1" ]; then
  echo "health check failed"
  sudo systemctl is-active cli-proxy-api || true
  sudo journalctl -u cli-proxy-api -n 100 --no-pager || true
  exit 1
fi

trap - ERR
echo "plugin deploy succeeded"
printf 'PLUGIN_BACKUP=%s\n' "${PLUGIN_BACKUP}"
printf 'CONFIG_BACKUP=%s\n' "${CONFIG_BACKUP}"
printf 'DEPLOY_LOG=%s\n' "${LOG}"
printf 'TARGET_SO=%s\n' "${TARGET_SO}"
printf 'CONFIG_STATUS=%s\n' "${CONFIG_STATUS}"
ls -lh "${TARGET_SO}" | tr -d '\r'

pid="$(systemctl show -p MainPID --value cli-proxy-api | tr -d '\r')"
echo "main_pid=${pid}"
sudo journalctl -u cli-proxy-api _PID="${pid}" --no-pager | grep -E "pluginhost:|${PLUGIN}" || true

echo "plugins_after:"
ls -1 "${APP}/plugins/linux/arm64" | tr -d '\r'

# Verify config entry exists without printing secrets.
python3 - <<'PY'
from pathlib import Path
text = Path("/opt/cli-proxy-api/config.yaml").read_text(encoding="utf-8")
assert "cpa-deepseek-reasoning-replay:" in text
print("config_has_plugin=true")
PY

rm -rf "${BUILD_DIR}" "${STAGE}" "${SRC_PKG}"
echo "cleanup done"
