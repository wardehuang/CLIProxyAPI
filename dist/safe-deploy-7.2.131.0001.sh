#!/usr/bin/env bash
set -euo pipefail

APP=/opt/cli-proxy-api
PKG=/tmp/cli-proxy-api-7.2.131.0001-linux-arm64.tar.gz
VERSION=7.2.131.0001
TS=$(date +%Y%m%d%H%M%S)
DEPLOY_ID="custom-${VERSION}-${TS}"
LOG_DIR="$APP/deploy-logs"
LOG="$LOG_DIR/${DEPLOY_ID}.log"
TMP=/tmp/cliproxy-deploy-$TS
BACKUP_DIR="$APP/backups"
DATA_BACKUP="$BACKUP_DIR/pre-custom-$VERSION-$TS.tar.gz"
BIN_BACKUP="$APP/cli-proxy-api.bak.custom.$VERSION.$TS"
CONFIG_BACKUP="$APP/config.yaml.bak.custom.$VERSION.$TS"
CONFIG_MERGE_STATUS=not-run
PLUGIN_STATUS=unknown
HEALTH_TIMEOUT_SECONDS=90
RESULT_FILE="/tmp/cliproxy-deploy-result-$TS.txt"
LATEST_RESULT=/tmp/cliproxy-deploy-latest.result

mkdir -p "$LOG_DIR" "$BACKUP_DIR" "$TMP"
exec >> "$LOG" 2>&1

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
    printf 'REMOVED_DATA_BACKUPS=%s\n' "${REMOVED_DATA_BACKUPS:-}"
    printf 'REMOVED_BINARY_BACKUPS=%s\n' "${REMOVED_BINARY_BACKUPS:-}"
    printf 'KEPT_DATA_BACKUPS=%s\n' "${KEPT_DATA_BACKUPS:-}"
    printf 'KEPT_BINARY_BACKUPS=%s\n' "${KEPT_BINARY_BACKUPS:-}"
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
  VERSION_LINE=$("$APP/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version' || true)
  write_result failed
  exit 1
}

trap 'on_error' ERR

log "deploy start id=$DEPLOY_ID version=$VERSION"

if [ ! -f "$PKG" ]; then
  FAILURE_REASON="package-missing"
  exit 1
fi

tar -xzf "$PKG" -C "$TMP"
chmod +x "$TMP/cli-proxy-api"
VERSION_LINE=$("$TMP/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version' || true)
if ! printf '%s\n' "$VERSION_LINE" | grep -q "CLIProxyAPI Version: $VERSION"; then
  FAILURE_REASON="package-version-check-failed"
  log "package version check failed: $VERSION_LINE"
  exit 1
fi
log "package version ok: $VERSION_LINE"

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
log "data backup=$DATA_BACKUP"

cp -a "$APP/cli-proxy-api" "$BIN_BACKUP"
if [ -f "$APP/config.yaml" ]; then
  cp -a "$APP/config.yaml" "$CONFIG_BACKUP"
fi
log "binary backup=$BIN_BACKUP"

install -m 755 "$TMP/cli-proxy-api" "$APP/cli-proxy-api.new"
mv "$APP/cli-proxy-api.new" "$APP/cli-proxy-api"
cp -f "$TMP/LICENSE" "$TMP/README.md" "$TMP/README_CN.md" "$TMP/config.example.yaml" "$APP/"
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

PLUGIN_STATUS=$(python3 - <<'PY'
import yaml
with open("/opt/cli-proxy-api/config.yaml", "r", encoding="utf-8") as f:
    cfg = yaml.safe_load(f) or {}
plugins = cfg.get("plugins") or {}
enabled = bool(plugins.get("enabled"))
switcher = ((plugins.get("configs") or {}).get("cpa-xai-ip-switcher") or {})
switcher_enabled = bool(switcher.get("enabled"))
print(f"plugins.enabled={enabled} cpa-xai-ip-switcher.enabled={switcher_enabled}")
PY
)
log "plugin status=$PLUGIN_STATUS"

sudo systemctl start cli-proxy-api
log "service start issued"

HEALTHY=0
for ((attempt=1; attempt<=HEALTH_TIMEOUT_SECONDS; attempt++)); do
  if systemctl is-active --quiet cli-proxy-api && curl --max-time 3 -sS -o /dev/null http://127.0.0.1:18457/v0/management/config; then
    HEALTHY=1
    log "health ok at ${attempt}s"
    break
  fi
  sleep 1
done

if [ "$HEALTHY" -ne 1 ]; then
  FAILURE_REASON="health-check-timeout-${HEALTH_TIMEOUT_SECONDS}s"
  exit 1
fi

trap - ERR

VERSION_LINE=$("$APP/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version' || true)
if ! printf '%s\n' "$VERSION_LINE" | grep -q "CLIProxyAPI Version: $VERSION"; then
  FAILURE_REASON="installed-version-mismatch"
  on_error
fi

REMOVED_DATA_BACKUPS=$(ls -1t "$BACKUP_DIR"/pre-custom-*.tar.gz 2>/dev/null | sed -n '4,$p' | while IFS= read -r old; do rm -f -- "$old" && printf '%s ' "$old"; done)
REMOVED_BINARY_BACKUPS=$(ls -1t "$APP"/cli-proxy-api.bak.custom.* 2>/dev/null | sed -n '4,$p' | while IFS= read -r old; do rm -f -- "$old" && printf '%s ' "$old"; done)
ls -1t "$APP"/config.yaml.bak.custom.* 2>/dev/null | sed -n '4,$p' | while IFS= read -r old; do rm -f -- "$old"; done
KEPT_DATA_BACKUPS=$(ls -1t "$BACKUP_DIR"/pre-custom-*.tar.gz 2>/dev/null | sed -n '1,3p' | paste -sd ' ' -)
KEPT_BINARY_BACKUPS=$(ls -1t "$APP"/cli-proxy-api.bak.custom.* 2>/dev/null | sed -n '1,3p' | paste -sd ' ' -)

rm -rf "$TMP"
log "deploy success version=$VERSION_LINE"
write_result active
exit 0
