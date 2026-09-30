#!/usr/bin/env bash
set -euo pipefail

APP=/opt/cli-proxy-api
DEPLOY_ID=cpa-plugins-66e43fef-dirty-20260813233027
STAGE=/tmp/${DEPLOY_ID}-stage
TS=$(date +%Y%m%d%H%M%S)
BACKUP_DIR="$APP/backups"
DEPLOY_LOG_DIR="$APP/deploy-logs"
PLUGIN_BACKUP="$BACKUP_DIR/plugins-pre-$DEPLOY_ID-$TS.tar.gz"
CONFIG_BACKUP="$BACKUP_DIR/config.yaml.bak.plugins.$DEPLOY_ID.$TS"
LOG="$DEPLOY_LOG_DIR/$DEPLOY_ID.log"
LATEST_RESULT=/tmp/cpa-plugin-deploy-latest.result
HEALTH_TIMEOUT_SECONDS=90
CONFIG_STATUS=not-run
FAILURE_REASON=

mkdir -p "$BACKUP_DIR" "$DEPLOY_LOG_DIR" "$APP/plugins/linux/arm64"
exec >> "$LOG" 2>&1

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

write_result() {
  {
    printf 'DEPLOY_ID=%s\n' "$DEPLOY_ID"
    printf 'LOG=%s\n' "$LOG"
    printf 'STATUS=%s\n' "${1:-unknown}"
    printf 'PLUGIN_BACKUP=%s\n' "${PLUGIN_BACKUP:-}"
    printf 'CONFIG_BACKUP=%s\n' "${CONFIG_BACKUP:-}"
    printf 'CONFIG_STATUS=%s\n' "${CONFIG_STATUS:-not-run}"
    printf 'FAILURE_REASON=%s\n' "${FAILURE_REASON:-}"
  } > "$LATEST_RESULT"
}

rollback() {
  log "plugin deploy failed; rolling back from $PLUGIN_BACKUP"
  sudo systemctl stop cli-proxy-api || true
  if [ -s "$PLUGIN_BACKUP" ]; then
    sudo tar -xzf "$PLUGIN_BACKUP" -C "$APP"
    log "restored plugins from $PLUGIN_BACKUP"
  fi
  if [ -f "$CONFIG_BACKUP" ]; then
    sudo cp -a "$CONFIG_BACKUP" "$APP/config.yaml"
    log "restored config from $CONFIG_BACKUP"
  fi
  sudo systemctl start cli-proxy-api || true
  sudo systemctl is-active cli-proxy-api || true
}

on_error() {
  FAILURE_REASON="${FAILURE_REASON:-plugin-deploy-failed}"
  log "ERROR: $FAILURE_REASON"
  rollback
  write_result failed
  exit 1
}

trap 'on_error' ERR

log "plugin deploy start id=$DEPLOY_ID"

for plugin in cpa-codex-openai-context cpa-prompt-cache-usage cpa-compact-route-rewriter cpa-antigravity-priority-scheduler cpa-strip-visible-files cpa-xai-ip-switcher cpa-claude-mem-adapter; do
  test -s "$STAGE/plugins/linux/arm64/$plugin.so"
done

if [ -d "$APP/plugins" ]; then
  sudo tar -czf "$PLUGIN_BACKUP" -C "$APP" plugins
fi
log "plugin backup=$PLUGIN_BACKUP"

if [ -f "$APP/config.yaml" ]; then
  sudo cp -a "$APP/config.yaml" "$CONFIG_BACKUP"
fi

for plugin in cpa-codex-openai-context cpa-prompt-cache-usage cpa-compact-route-rewriter cpa-antigravity-priority-scheduler cpa-strip-visible-files cpa-xai-ip-switcher cpa-claude-mem-adapter; do
  sudo install -m 755 "$STAGE/plugins/linux/arm64/$plugin.so" "$APP/plugins/linux/arm64/$plugin.so"
  log "installed $plugin.so"
done

if python3 - <<'PY' >/dev/null 2>&1
import yaml
PY
then
  CONFIG_STATUS=$(python3 - "$APP/config.yaml" <<'PY'
import sys
import yaml

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as f:
    cfg = yaml.safe_load(f) or {}

defaults = {
    "enabled": True,
    "dir": "plugins",
    "configs": {
        "cpa-codex-openai-context": {
            "enabled": True,
            "priority": 0,
        },
        "cpa-compact-route-rewriter": {
            "enabled": True,
            "priority": 0,
            "codex-compact-model": "gpt-5.4-mini",
            "antigravity-compact-model": "gemini-3.1-flash-lite",
        },
        "cpa-prompt-cache-usage": {
            "enabled": True,
            "priority": 10,
        },
        "cpa-antigravity-priority-scheduler": {
            "enabled": True,
            "priority": 20,
            "strategy": "fill-first",
        },
        "cpa-strip-visible-files": {
            "enabled": True,
        },
        "cpa-xai-ip-switcher": {
            "enabled": True,
            "priority": 0,
            "database_path": "/opt/cli-proxy-api/plugin-data/cpa-xai-ip-switcher/ip-switcher.sqlite3",
            "worker_count": 0,
        },
        "cpa-claude-mem-adapter": {
            "enabled": True,
            "debug": False,
        },
    },
}

def fill_missing(current, default):
    changed = False
    if not isinstance(current, dict):
        return default, True
    merged = dict(current)
    for key, value in default.items():
        if key not in merged:
            merged[key] = value
            changed = True
        elif isinstance(value, dict):
            child, child_changed = fill_missing(merged[key], value)
            merged[key] = child
            changed = changed or child_changed
    return merged, changed

plugins = cfg.get("plugins")
if not isinstance(plugins, dict):
    cfg["plugins"] = defaults
    changed = True
else:
    filled, changed = fill_missing(plugins, defaults)
    cfg["plugins"] = filled

if changed:
    with open(path, "w", encoding="utf-8") as f:
        yaml.safe_dump(cfg, f, allow_unicode=False, sort_keys=False)
    print("filled-missing")
else:
    print("already-complete")
PY
)
else
  CONFIG_STATUS=skipped-no-python-yaml
fi
log "config status=$CONFIG_STATUS"

sudo systemctl restart cli-proxy-api
log "service restart issued"

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
log "plugin deploy succeeded"
write_result active
exit 0
