set -euo pipefail
APP=/opt/cli-proxy-api
DEPLOY_ID=cpa-mcp-schema-patch-219a2d86-dirty-20260727112506
STAGE=/tmp/$DEPLOY_ID-stage
BUILD_DIR=/tmp/$DEPLOY_ID-build
TS=$(date +%Y%m%d%H%M%S)
BACKUP_DIR="$APP/backups"
DEPLOY_LOG_DIR="$APP/deploy-logs"
PLUGIN_BACKUP="$BACKUP_DIR/plugins-pre-$DEPLOY_ID-$TS.tar.gz"
CONFIG_BACKUP="$BACKUP_DIR/config-pre-$DEPLOY_ID-$TS.yaml"
LOG="$DEPLOY_LOG_DIR/$DEPLOY_ID.log"
PLUGIN=cpa-mcp-schema-patch

mkdir -p "$BACKUP_DIR" "$DEPLOY_LOG_DIR" "$APP/plugins/linux/arm64" "$APP/mcp-schemas"
exec > >(sudo tee -a "$LOG") 2>&1

echo "deploy_id=$DEPLOY_ID"
echo "plugin_backup=$PLUGIN_BACKUP"

rollback() {
  echo "plugin deploy failed; rolling back from $PLUGIN_BACKUP"
  sudo systemctl stop cli-proxy-api || true
  if [ -s "$PLUGIN_BACKUP" ]; then
    sudo tar -xzf "$PLUGIN_BACKUP" -C "$APP"
  fi
  if [ -s "$CONFIG_BACKUP" ]; then
    sudo cp "$CONFIG_BACKUP" "$APP/config.yaml"
  fi
  sudo systemctl start cli-proxy-api || true
  sudo systemctl is-active cli-proxy-api || true
}
trap 'rollback' ERR

if [ -d "$APP/plugins" ]; then
  sudo tar -czf "$PLUGIN_BACKUP" -C "$APP" plugins
fi
sudo cp "$APP/config.yaml" "$CONFIG_BACKUP"

test -s "$STAGE/plugins/linux/arm64/$PLUGIN.so"
sudo install -m 755 "$STAGE/plugins/linux/arm64/$PLUGIN.so" "$APP/plugins/linux/arm64/$PLUGIN.so"

# Refresh schemas from examples
if [ -d "$BUILD_DIR/plugins/src/$PLUGIN/examples" ]; then
  sudo mkdir -p "$APP/mcp-schemas"
  sudo cp -a "$BUILD_DIR/plugins/src/$PLUGIN/examples/." "$APP/mcp-schemas/"
  echo "seeded mcp-schemas from examples"
  find "$APP/mcp-schemas" -type f -name '*.json' | sort
fi

# Ensure plugin config has inject-missing without wiping other plugin configs
python3 - <<'PY'
from pathlib import Path
import re
cfg_path = Path("/opt/cli-proxy-api/config.yaml")
text = cfg_path.read_text(encoding="utf-8")
plugin_block = """    cpa-mcp-schema-patch:
      enabled: true
      priority: 5
      debug: false
      schemas-dir: mcp-schemas
      only-empty: true
      inject-missing: true
      logs-dir: logs
      detail-log: auto
"""
# Replace existing cpa-mcp-schema-patch block if present
pattern = re.compile(r"(?ms)^[ \t]*cpa-mcp-schema-patch:\n(?:[ \t]+.+\n)*")
if pattern.search(text):
    text2 = pattern.sub(plugin_block if plugin_block.endswith("\n") else plugin_block + "\n", text, count=1)
    cfg_path.write_text(text2, encoding="utf-8")
    print("config: replaced cpa-mcp-schema-patch block")
else:
    m = re.search(r"(?m)^[ \t]*configs\s*:\s*$", text)
    if m:
        lines = text.splitlines(keepends=True)
        out = []
        inserted = False
        for line in lines:
            out.append(line)
            if not inserted and re.match(r"^[ \t]*configs\s*:\s*$", line):
                out.append(plugin_block if plugin_block.endswith("\n") else plugin_block + "\n")
                inserted = True
        cfg_path.write_text("".join(out), encoding="utf-8")
        print("config: inserted cpa-mcp-schema-patch under configs")
    else:
        text = text.rstrip() + "\n\nplugins:\n  enabled: true\n  dir: plugins\n  configs:\n" + plugin_block + "\n"
        cfg_path.write_text(text, encoding="utf-8")
        print("config: appended plugins section")
PY

sudo systemctl restart cli-proxy-api
sleep 3
sudo systemctl is-active cli-proxy-api

trap - ERR
echo "plugin deploy succeeded"
printf 'PLUGIN_BACKUP=%s\n' "$PLUGIN_BACKUP"
printf 'CONFIG_BACKUP=%s\n' "$CONFIG_BACKUP"
printf 'DEPLOY_LOG=%s\n' "$LOG"
ls -lh "$APP/plugins/linux/arm64"/$PLUGIN.so