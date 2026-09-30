#!/usr/bin/env bash
set -euo pipefail
APP=/opt/cli-proxy-api
VERSION="${1:?version required}"
DEPLOY_ID="${2:?deploy_id required}"
PKG="/tmp/cli-proxy-api-${VERSION}-linux-arm64.tar.gz"
TS=$(date +%Y%m%d%H%M%S)
TMP="/tmp/cliproxy-deploy-${TS}"
BACKUP_DIR="$APP/backups"
DEPLOY_LOG_DIR="$APP/deploy-logs"
DATA_BACKUP="$BACKUP_DIR/pre-custom-$VERSION-$TS.tar.gz"
BIN_BACKUP="$APP/cli-proxy-api.bak.custom.$VERSION.$TS"
CONFIG_BACKUP="$APP/config.yaml.bak.custom.$VERSION.$TS"
EXAMPLE_BACKUP="$APP/config.example.yaml.bak.custom.$VERSION.$TS"
LOG="$DEPLOY_LOG_DIR/$DEPLOY_ID.log"
CONFIG_MERGE_STATUS=not-run

mkdir -p "$BACKUP_DIR" "$DEPLOY_LOG_DIR" "$TMP"
exec > >(sudo tee -a "$LOG") 2>&1

echo "deploy_id=$DEPLOY_ID"
echo "version=$VERSION"
echo "pkg=$PKG"
echo "ts=$TS"
echo "service_before=$(systemctl is-active cli-proxy-api || true)"

tar -xzf "$PKG" -C "$TMP"
chmod +x "$TMP/cli-proxy-api"
HELP_OUT=$("$TMP/cli-proxy-api" --help 2>&1 || true)
echo "$HELP_OUT" | tr -d '\r' | grep 'CLIProxyAPI Version' || true
if ! printf '%s\n' "$HELP_OUT" | tr -d '\r' | grep -F "CLIProxyAPI Version: $VERSION" >/dev/null; then
  echo "package version check failed" >&2
  exit 1
fi

restart_old() {
  echo "deploy failed; rolling back"
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
  sudo systemctl is-active cli-proxy-api || true
}
trap 'restart_old' ERR

sudo systemctl stop cli-proxy-api
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

install -m 755 "$TMP/cli-proxy-api" "$APP/cli-proxy-api.new"
mv "$APP/cli-proxy-api.new" "$APP/cli-proxy-api"
cp -f "$TMP/LICENSE" "$TMP/README.md" "$TMP/README_CN.md" "$TMP/config.example.yaml" "$APP/"

if [ -f "$CONFIG_BACKUP" ]; then
  if python3 - <<'PY' >/dev/null 2>&1
import yaml
PY
  then
    python3 - "$APP/config.example.yaml" "$CONFIG_BACKUP" "$APP/config.yaml" <<'PY'
import sys
import yaml

example_path, old_path, out_path = sys.argv[1:]

with open(example_path, 'r', encoding='utf-8') as f:
    defaults = yaml.safe_load(f) or {}
with open(old_path, 'r', encoding='utf-8') as f:
    old = yaml.safe_load(f) or {}

def merge(default_value, old_value):
    if isinstance(default_value, dict) and isinstance(old_value, dict):
        merged = dict(default_value)
        for key, value in old_value.items():
            merged[key] = merge(default_value.get(key), value)
        return merged
    return old_value

merged = merge(defaults, old)
with open(out_path, 'w', encoding='utf-8') as f:
    yaml.safe_dump(merged, f, allow_unicode=False, sort_keys=False)
PY
    CONFIG_MERGE_STATUS=merged-with-old-values
  else
    cp -a "$CONFIG_BACKUP" "$APP/config.yaml"
    CONFIG_MERGE_STATUS=skipped-no-python-yaml-config-kept
  fi
else
  cp -n "$APP/config.example.yaml" "$APP/config.yaml" || true
  CONFIG_MERGE_STATUS=created-from-example-if-missing
fi

sudo systemctl start cli-proxy-api
HEALTH_TIMEOUT_SECONDS=90
HEALTHY=0
for ((attempt=1; attempt<=HEALTH_TIMEOUT_SECONDS; attempt++)); do
  if systemctl is-active --quiet cli-proxy-api && curl --max-time 3 -sS -o /dev/null http://127.0.0.1:18457/v0/management/config; then
    HEALTHY=1
    echo "healthy_after_seconds=$attempt"
    break
  fi
  sleep 1
done
if [ "$HEALTHY" -ne 1 ]; then
  echo "health check timed out after ${HEALTH_TIMEOUT_SECONDS}s" >&2
  systemctl is-active cli-proxy-api || true
  journalctl -u cli-proxy-api.service -n 80 --no-pager || true
  exit 1
fi
trap - ERR

printf 'STATUS=active\n'
printf 'DATA_BACKUP=%s\n' "$DATA_BACKUP"
printf 'BINARY_BACKUP=%s\n' "$BIN_BACKUP"
printf 'CONFIG_BACKUP=%s\n' "$CONFIG_BACKUP"
printf 'CONFIG_MERGE_STATUS=%s\n' "$CONFIG_MERGE_STATUS"
printf 'DEPLOY_LOG=%s\n' "$LOG"
printf 'VERSION_LINE='
"$APP/cli-proxy-api" --help 2>&1 | tr -d '\r' | grep 'CLIProxyAPI Version' || true
printf '\nPID=%s\n' "$(systemctl show -p MainPID --value cli-proxy-api)"
printf 'PLUGIN_SO_COUNT=%s\n' "$(ls "$APP/plugins/linux/arm64"/cpa-*.so 2>/dev/null | wc -l)"
printf 'REMOVED_DATA_BACKUPS='
ls -1t "$BACKUP_DIR"/pre-custom-*.tar.gz 2>/dev/null | sed -n '4,$p' | while IFS= read -r old; do rm -f -- "$old" && printf '%s ' "$old"; done
printf '\nREMOVED_BINARY_BACKUPS='
ls -1t "$APP"/cli-proxy-api.bak.custom.* 2>/dev/null | sed -n '4,$p' | while IFS= read -r old; do rm -f -- "$old" && printf '%s ' "$old"; done
printf '\nREMOVED_CONFIG_BACKUPS='
ls -1t "$APP"/config.yaml.bak.custom.* 2>/dev/null | sed -n '4,$p' | while IFS= read -r old; do rm -f -- "$old" && printf '%s ' "$old"; done
printf '\nKEPT_DATA_BACKUPS='
ls -1t "$BACKUP_DIR"/pre-custom-*.tar.gz 2>/dev/null | sed -n '1,3p' | paste -sd ' ' -
printf '\nKEPT_BINARY_BACKUPS='
ls -1t "$APP"/cli-proxy-api.bak.custom.* 2>/dev/null | sed -n '1,3p' | paste -sd ' ' -
printf '\n'
