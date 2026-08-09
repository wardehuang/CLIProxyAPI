#!/usr/bin/env bash
set -Eeo pipefail
APP=/opt/cli-proxy-api
VERSION=7.2.123.0001
PKG=/tmp/cli-proxy-api-7.2.123.0001-linux-arm64.tar.gz
SRC_PKG=/tmp/cliproxy-source-7.2.123.0001-20260808111127.tar.gz
BUILD_DIR=/tmp/cliproxy-build-7.2.123.0001-20260808111127
STAGE=/tmp/cliproxy-stage-7.2.123.0001-20260808111127
DEPLOY_ID="${VERSION}-$(date +%Y%m%d%H%M%S)"
TS=$(date +%Y%m%d%H%M%S)
TMP=/tmp/cliproxy-deploy-$DEPLOY_ID
BACKUP_DIR="$APP/backups"
DEPLOY_LOG_DIR="$APP/deploy-logs"
LOG="$DEPLOY_LOG_DIR/$DEPLOY_ID.log"
DATA_BACKUP="$BACKUP_DIR/pre-custom-$VERSION-$TS.tar.gz"
BIN_BACKUP="$APP/cli-proxy-api.bak.custom.$VERSION.$TS"
CONFIG_BACKUP="$APP/config.yaml.bak.custom.$VERSION.$TS"
ROLLBACK_CONFIG_EXAMPLE_BACKUP="$APP/config.example.yaml.bak.custom.$VERSION.$TS"
MERGED_CONFIG="$APP/config.yaml.merge.$DEPLOY_ID"
CONFIG_MERGE_STATUS=not-run
OLD_SERVICE_STATE=unknown
ROLLBACK_REQUIRED=0
ROLLBACK_DONE=0
PLUGIN_SO_COUNT_BEFORE=0

mkdir -p "$DEPLOY_LOG_DIR"
exec > >(tee -a "$LOG") 2>&1

echo "DEPLOY_ID=$DEPLOY_ID"
echo "VERSION=$VERSION"
echo "STARTED_AT=$(date -Is)"

cleanup_temp() {
  set +e
  rm -rf "$TMP" "$BUILD_DIR" "$STAGE"
  rm -f "$PKG" "$SRC_PKG" "$MERGED_CONFIG"
  set -e
}

rollback() {
  if [ "$ROLLBACK_REQUIRED" -ne 1 ] || [ "$ROLLBACK_DONE" -eq 1 ]; then
    return 0
  fi
  set +e
  echo "ROLLBACK=starting"
  if [ -f "$BIN_BACKUP" ]; then
    install -m 755 "$BIN_BACKUP" "$APP/cli-proxy-api"
  fi
  if [ -f "$CONFIG_BACKUP" ]; then
    cp -a "$CONFIG_BACKUP" "$APP/config.yaml"
  fi
  if [ -f "$ROLLBACK_CONFIG_EXAMPLE_BACKUP" ]; then
    cp -a "$ROLLBACK_CONFIG_EXAMPLE_BACKUP" "$APP/config.example.yaml"
  fi
  sudo systemctl stop cli-proxy-api >/dev/null 2>&1 || true
  if [ "$OLD_SERVICE_STATE" = "active" ]; then
    sudo systemctl start cli-proxy-api >/dev/null 2>&1 || true
  fi
  echo "ROLLBACK_SERVICE=$(systemctl is-active cli-proxy-api 2>/dev/null || true)"
  ROLLBACK_DONE=1
  echo "ROLLBACK=completed"
}

handle_error() {
  status=$?
  trap - ERR
  echo "DEPLOY_ERROR_STATUS=$status"
  rollback
  cleanup_temp
  echo "DEPLOY_RESULT=rolled-back"
  exit "$status"
}

trap handle_error ERR

if [ ! -f "$PKG" ]; then
  echo "package missing: $PKG" >&2
  false
fi
mkdir -p "$BACKUP_DIR" "$TMP"
tar -xzf "$PKG" -C "$TMP"
chmod 755 "$TMP/cli-proxy-api"
PACKAGE_HELP_OUTPUT=$("$TMP/cli-proxy-api" --help 2>&1)
case "$PACKAGE_HELP_OUTPUT" in
  *"CLIProxyAPI Version: $VERSION,"*) ;;
  *) echo "package version check failed" >&2; false ;;
esac
for required_file in LICENSE README.md README_CN.md config.example.yaml; do
  if [ ! -s "$TMP/$required_file" ]; then
    echo "package file missing: $required_file" >&2
    false
  fi
done

OLD_SERVICE_STATE=$(systemctl is-active cli-proxy-api || true)
if [ "$OLD_SERVICE_STATE" != "active" ]; then
  echo "service is not active before deployment: $OLD_SERVICE_STATE" >&2
  false
fi
if [ -d "$APP/plugins" ]; then
  PLUGIN_SO_COUNT_BEFORE=$(find "$APP/plugins" -maxdepth 1 -type f -name '*.so' 2>/dev/null | wc -l)
fi

echo "SERVICE_BEFORE=$OLD_SERVICE_STATE"
echo "PLUGIN_SO_COUNT_BEFORE=$PLUGIN_SO_COUNT_BEFORE"
ROLLBACK_REQUIRED=1
sudo systemctl stop cli-proxy-api
cd "$APP"
items=()
for path in cli-proxy-api config.yaml .env auths gitstore objectstore pgstore static logs plugins LICENSE README.md README_CN.md config.example.yaml; do
  if [ -e "$path" ]; then
    items+=("$path")
  fi
done
tar -czf "$DATA_BACKUP" "${items[@]}"
cp -a "$APP/cli-proxy-api" "$BIN_BACKUP"
if [ -f "$APP/config.yaml" ]; then
  cp -a "$APP/config.yaml" "$CONFIG_BACKUP"
fi
if [ -f "$APP/config.example.yaml" ]; then
  cp -a "$APP/config.example.yaml" "$ROLLBACK_CONFIG_EXAMPLE_BACKUP"
fi

install -m 755 "$TMP/cli-proxy-api" "$APP/cli-proxy-api.new"
mv "$APP/cli-proxy-api.new" "$APP/cli-proxy-api"
cp -f "$TMP/LICENSE" "$TMP/README.md" "$TMP/README_CN.md" "$TMP/config.example.yaml" "$APP/"

if [ -f "$CONFIG_BACKUP" ]; then
  if python3 -c 'import yaml' >/dev/null 2>&1; then
    python3 - "$APP/config.example.yaml" "$CONFIG_BACKUP" "$MERGED_CONFIG" <<'PY'
import sys
import yaml

example_path, old_path, output_path = sys.argv[1:]
with open(example_path, 'r', encoding='utf-8') as file_handle:
    defaults = yaml.safe_load(file_handle) or {}
with open(old_path, 'r', encoding='utf-8') as file_handle:
    old_values = yaml.safe_load(file_handle) or {}

def merge(default_value, old_value):
    if isinstance(default_value, dict) and isinstance(old_value, dict):
        merged = dict(default_value)
        for key, value in old_value.items():
            merged[key] = merge(default_value.get(key), value)
        return merged
    return old_value

merged = merge(defaults, old_values)
with open(output_path, 'w', encoding='utf-8') as file_handle:
    yaml.safe_dump(merged, file_handle, allow_unicode=False, sort_keys=False)
PY
    mv "$MERGED_CONFIG" "$APP/config.yaml"
    CONFIG_MERGE_STATUS=merged-with-old-values
  else
    cp -a "$CONFIG_BACKUP" "$APP/config.yaml"
    CONFIG_MERGE_STATUS=skipped-no-python-yaml-config-kept
  fi
else
  cp -n "$APP/config.example.yaml" "$APP/config.yaml" || true
  CONFIG_MERGE_STATUS=created-from-example-if-missing
fi

echo "CONFIG_MERGE_STATUS=$CONFIG_MERGE_STATUS"
sudo systemctl start cli-proxy-api
HEALTH_TIMEOUT_SECONDS=90
HEALTHY=0
for ((attempt=1; attempt<=HEALTH_TIMEOUT_SECONDS; attempt++)); do
  if systemctl is-active --quiet cli-proxy-api && curl --max-time 3 -sS -o /dev/null http://127.0.0.1:18457/v0/management/config; then
    HEALTHY=1
    break
  fi
  if (( attempt % 10 == 0 )); then
    echo "HEALTH_CHECK_ATTEMPT=$attempt/$HEALTH_TIMEOUT_SECONDS"
  fi
  sleep 1
done
if [ "$HEALTHY" -ne 1 ]; then
  echo "health check timed out after ${HEALTH_TIMEOUT_SECONDS}s" >&2
  false
fi

HELP_OUTPUT=$("$APP/cli-proxy-api" --help 2>&1)
case "$HELP_OUTPUT" in
  *"CLIProxyAPI Version: $VERSION,"*) ;;
  *) echo "post-deployment version check failed" >&2; false ;;
esac
VERSION_LINE=$(printf '%s\n' "$HELP_OUTPUT" | awk '/CLIProxyAPI Version:/ {print; exit}')
PLUGIN_HEADER=$(curl --max-time 3 -sS -D - -o /dev/null http://127.0.0.1:18457/v0/management/config | tr -d '\r' | awk -F': ' 'tolower($1)=="x-cpa-support-plugin" {value=$2} END {print value}')
if [ "$PLUGIN_HEADER" != "1" ]; then
  echo "plugin support header check failed: $PLUGIN_HEADER" >&2
  false
fi
PLUGIN_SO_COUNT_AFTER=0
if [ -d "$APP/plugins" ]; then
  PLUGIN_SO_COUNT_AFTER=$(find "$APP/plugins" -maxdepth 1 -type f -name '*.so' 2>/dev/null | wc -l)
fi
if [ "$PLUGIN_SO_COUNT_AFTER" != "$PLUGIN_SO_COUNT_BEFORE" ]; then
  echo "plugin file count changed: before=$PLUGIN_SO_COUNT_BEFORE after=$PLUGIN_SO_COUNT_AFTER" >&2
  false
fi
JOURNAL_ERROR_COUNT=$(journalctl -u cli-proxy-api.service -n 120 --no-pager 2>/dev/null | grep -Eic 'error|failed|panic' || true)

trap - ERR
ROLLBACK_REQUIRED=0
cleanup_temp
for pattern in "$BACKUP_DIR"/pre-custom-*.tar.gz "$APP"/cli-proxy-api.bak.custom.* "$APP"/config.yaml.bak.custom.* "$APP"/config.example.yaml.bak.custom.*; do
  for old_backup in $(ls -1t $pattern 2>/dev/null | sed -n '4,$p'); do
    rm -f -- "$old_backup" || true
  done
done
printf 'DEPLOY_RESULT=success\n'
printf 'STATUS=%s\n' "$(systemctl is-active cli-proxy-api)"
printf 'PID=%s\n' "$(systemctl show -p MainPID --value cli-proxy-api)"
printf 'VERSION_LINE=%s\n' "$VERSION_LINE"
printf 'PLUGIN_SUPPORT_HEADER=%s\n' "$PLUGIN_HEADER"
printf 'PLUGIN_SO_COUNT_BEFORE=%s\n' "$PLUGIN_SO_COUNT_BEFORE"
printf 'PLUGIN_SO_COUNT_AFTER=%s\n' "$PLUGIN_SO_COUNT_AFTER"
printf 'JOURNAL_ERROR_COUNT=%s\n' "$JOURNAL_ERROR_COUNT"
printf 'DATA_BACKUP=%s\n' "$DATA_BACKUP"
printf 'BINARY_BACKUP=%s\n' "$BIN_BACKUP"
printf 'CONFIG_BACKUP=%s\n' "$CONFIG_BACKUP"
printf 'CONFIG_EXAMPLE_BACKUP=%s\n' "$ROLLBACK_CONFIG_EXAMPLE_BACKUP"
printf 'DEPLOY_LOG=%s\n' "$LOG"
printf 'CLEANED_SOURCE=%s\n' "$SRC_PKG"
printf 'CLEANED_PACKAGE=%s\n' "$PKG"
printf 'CLEANED_BUILD_DIR=%s\n' "$BUILD_DIR"
printf 'CLEANED_STAGE=%s\n' "$STAGE"
printf 'CLEANED_DEPLOY_DIR=%s\n' "$TMP"
printf 'FINISHED_AT=%s\n' "$(date -Is)"
