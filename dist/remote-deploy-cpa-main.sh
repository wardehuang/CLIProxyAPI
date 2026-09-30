#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   bash remote-deploy-cpa-main.sh <version> <commit> <package_tarball>
# Deploys main binary only. Preserves plugins/auths/config operational values.

APP=/opt/cli-proxy-api
VERSION=$(printf '%s' "${1:?version}" | tr -d '\r\n')
COMMIT=$(printf '%s' "${2:?commit}" | tr -d '\r\n')
PKG=$(printf '%s' "${3:?package}" | tr -d '\r\n')
TS=$(date +%Y%m%d%H%M%S)
DEPLOY_ID="custom-${VERSION}-${TS}"
LOG_DIR="$APP/deploy-logs"
LOG="$LOG_DIR/cpa-main-${VERSION}-${TS}.log"
TMP=/tmp/cliproxy-deploy-${VERSION}-${TS}
BACKUP_DIR="$APP/backups"
DATA_BACKUP="$BACKUP_DIR/pre-custom-${VERSION}-${TS}.tar.gz"
BIN_BACKUP="$APP/cli-proxy-api.bak.custom.${VERSION}.${TS}"
CONFIG_BACKUP="$APP/config.yaml.bak.custom.${VERSION}.${TS}"
CONFIG_MERGE_STATUS=not-run
PLUGIN_STATUS=unknown
HEALTH_TIMEOUT_SECONDS=90
RESULT_FILE="/tmp/cliproxy-deploy-result-${TS}.txt"
LATEST_RESULT=/tmp/cliproxy-deploy-latest.result

mkdir -p "$LOG_DIR" "$BACKUP_DIR" "$TMP"
exec > >(tee -a "$LOG") 2>&1

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

write_result() {
  {
    printf 'DEPLOY_ID=%s\n' "$DEPLOY_ID"
    printf 'LOG=%s\n' "$LOG"
    printf 'STATUS=%s\n' "${1:-unknown}"
    printf 'DATA_BACKUP=%s\n' "${DATA_BACKUP:-}"
    printf 'BINARY_BACKUP=%s\n' "${BIN_BACKUP:-}"
    printf 'CONFIG_BACKUP=%s\n' "${CONFIG_BACKUP:-}"
    printf 'CONFIG_MERGE_STATUS=%s\n' "${CONFIG_MERGE_STATUS:-not-run}"
    printf 'PLUGIN_STATUS=%s\n' "${PLUGIN_STATUS:-unknown}"
    printf 'VERSION_LINE=%s\n' "${VERSION_LINE:-}"
    printf 'SERVICE_STATE=%s\n' "${SERVICE_STATE:-}"
    printf 'HEALTH_CODE=%s\n' "${HEALTH_CODE:-}"
    printf 'PID=%s\n' "${PID:-}"
    printf 'FAILURE_REASON=%s\n' "${FAILURE_REASON:-}"
  } | tee "$RESULT_FILE" > "$LATEST_RESULT"
}

restart_old() {
  log "rollback start"
  if [ -f "$BIN_BACKUP" ]; then
    cp -a "$BIN_BACKUP" "$APP/cli-proxy-api"
    chmod 755 "$APP/cli-proxy-api"
    log "restored binary from $BIN_BACKUP"
  fi
  if [ -f "$CONFIG_BACKUP" ]; then
    cp -a "$CONFIG_BACKUP" "$APP/config.yaml"
    log "restored config from $CONFIG_BACKUP"
  fi
  sudo systemctl start cli-proxy-api >/dev/null 2>&1 || true
  log "rollback systemctl start issued"
}

on_error() {
  FAILURE_REASON="${FAILURE_REASON:-deploy-failed}"
  log "ERROR: $FAILURE_REASON"
  restart_old
  VERSION_LINE=$("$APP/cli-proxy-api" --help 2>&1 | tr -d '\r' | grep 'CLIProxyAPI Version' || true)
  SERVICE_STATE=$(systemctl is-active cli-proxy-api || true)
  write_result failed
  exit 1
}

trap 'on_error' ERR

log "deploy start id=$DEPLOY_ID version=$VERSION commit=$COMMIT pkg=$PKG"

if [ ! -f "$PKG" ]; then
  FAILURE_REASON="package-missing"
  exit 1
fi

tar -xzf "$PKG" -C "$TMP"
chmod +x "$TMP/cli-proxy-api"
VERSION_LINE=$("$TMP/cli-proxy-api" --help 2>&1 | tr -d '\r' | grep 'CLIProxyAPI Version' || true)
log "package version: $VERSION_LINE"
if ! printf '%s\n' "$VERSION_LINE" | tr -d '\r' | grep -Fq "CLIProxyAPI Version: ${VERSION}, Commit: ${COMMIT}"; then
  FAILURE_REASON="package-version-check-failed"
  exit 1
fi

# plugin inventory before
PLUGIN_BEFORE=$(find "$APP/plugins" -type f -name '*.so' 2>/dev/null | sort | wc -l | tr -d ' ')
log "plugins before count=$PLUGIN_BEFORE"
find "$APP/plugins" -type f -name '*.so' 2>/dev/null | sort | sed 's/^/plugin-before: /' || true

sudo systemctl stop cli-proxy-api
log "service stopped"

cd "$APP"
items=()
for p in cli-proxy-api config.yaml auths gitstore objectstore pgstore static logs plugins .env LICENSE README.md README_CN.md config.example.yaml; do
  if [ -e "$p" ]; then
    items+=("$p")
  fi
done
tar -czf "$DATA_BACKUP" "${items[@]}"
log "data backup=$DATA_BACKUP size=$(stat -c%s "$DATA_BACKUP")"

cp -a "$APP/cli-proxy-api" "$BIN_BACKUP"
if [ -f "$APP/config.yaml" ]; then
  cp -a "$APP/config.yaml" "$CONFIG_BACKUP"
fi
log "binary backup=$BIN_BACKUP"

install -m 755 "$TMP/cli-proxy-api" "$APP/cli-proxy-api.new"
mv "$APP/cli-proxy-api.new" "$APP/cli-proxy-api"
for f in LICENSE README.md README_CN.md config.example.yaml; do
  if [ -f "$TMP/$f" ]; then
    cp -f "$TMP/$f" "$APP/$f"
  fi
done
log "binary and docs replaced"

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
log "config merge status=$CONFIG_MERGE_STATUS"

PLUGIN_AFTER=$(find "$APP/plugins" -type f -name '*.so' 2>/dev/null | sort | wc -l | tr -d ' ')
if [ "$PLUGIN_BEFORE" = "$PLUGIN_AFTER" ]; then
  PLUGIN_STATUS="preserved count=${PLUGIN_AFTER} (not rebuilt)"
else
  PLUGIN_STATUS="count-changed before=${PLUGIN_BEFORE} after=${PLUGIN_AFTER}"
fi
log "plugin status=$PLUGIN_STATUS"
find "$APP/plugins" -type f -name '*.so' 2>/dev/null | sort | sed 's/^/plugin-after: /' || true

sudo systemctl start cli-proxy-api
log "service start issued"

deadline=$((SECONDS + HEALTH_TIMEOUT_SECONDS))
HEALTH_CODE=000
MGMT_CODE=000
SERVICE_STATE=unknown
while [ $SECONDS -lt $deadline ]; do
  SERVICE_STATE=$(systemctl is-active cli-proxy-api 2>/dev/null || true)
  # Root path is the unauthenticated liveness probe. Management often returns 401/403
  # when remote-management.secret-key is set; that still means the server is up.
  HEALTH_CODE=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18457/ || true)
  HEALTH_CODE=$(printf '%s' "$HEALTH_CODE" | tr -d '\r')
  MGMT_CODE=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18457/v0/management/config || true)
  MGMT_CODE=$(printf '%s' "$MGMT_CODE" | tr -d '\r')
  log "poll state=$SERVICE_STATE root=$HEALTH_CODE management=$MGMT_CODE"
  if [ "$SERVICE_STATE" = "active" ] && [ "$HEALTH_CODE" = "200" ]; then
    break
  fi
  sleep 2
done

if [ "$SERVICE_STATE" != "active" ] || [ "$HEALTH_CODE" != "200" ]; then
  FAILURE_REASON="health-timeout state=${SERVICE_STATE} root=${HEALTH_CODE} management=${MGMT_CODE}"
  exit 1
fi

VERSION_LINE=$("$APP/cli-proxy-api" --help 2>&1 | tr -d '\r' | grep 'CLIProxyAPI Version' || true)
if ! printf '%s\n' "$VERSION_LINE" | tr -d '\r' | grep -Fq "CLIProxyAPI Version: ${VERSION}, Commit: ${COMMIT}"; then
  FAILURE_REASON="running-version-mismatch: ${VERSION_LINE}"
  exit 1
fi

PID=$(systemctl show -p MainPID --value cli-proxy-api | tr -d '\r')
log "deploy ok version=$VERSION_LINE pid=$PID health=$HEALTH_CODE"
log "journal:"
journalctl -u cli-proxy-api -n 40 --no-pager || true

# cleanup only uniquely named temp artifacts for this deploy
rm -rf "$TMP"
rm -f "$PKG" || true
write_result success
log "result written $RESULT_FILE"
cat "$RESULT_FILE"
