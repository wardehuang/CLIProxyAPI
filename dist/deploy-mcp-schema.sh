set -euo pipefail
APP=/opt/cli-proxy-api
DEPLOY_ID=cpa-mcp-schema-patch-219a2d86-dirty-20260727104546
STAGE=/tmp/$DEPLOY_ID-stage
BUILD_DIR=/tmp/$DEPLOY_ID-build
TS=$(date +%Y%m%d%H%M%S)
BACKUP_DIR="$APP/backups"
DEPLOY_LOG_DIR="$APP/deploy-logs"
PLUGIN_BACKUP="$BACKUP_DIR/plugins-pre-$DEPLOY_ID-$TS.tar.gz"
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
  sudo systemctl start cli-proxy-api || true
  sudo systemctl is-active cli-proxy-api || true
}
trap 'rollback' ERR

if [ -d "$APP/plugins" ]; then
  sudo tar -czf "$PLUGIN_BACKUP" -C "$APP" plugins
fi

test -s "$STAGE/plugins/linux/arm64/$PLUGIN.so"
sudo install -m 755 "$STAGE/plugins/linux/arm64/$PLUGIN.so" "$APP/plugins/linux/arm64/$PLUGIN.so"

# Seed examples into mcp-schemas if empty-ish for this tree
if [ -d "$BUILD_DIR/plugins/src/$PLUGIN/examples" ]; then
  sudo mkdir -p "$APP/mcp-schemas"
  sudo cp -a "$BUILD_DIR/plugins/src/$PLUGIN/examples/." "$APP/mcp-schemas/"
  echo "seeded mcp-schemas from examples"
  find "$APP/mcp-schemas" -type f -name '*.json' | sort
fi

# Merge plugin config without overwriting existing keys under this plugin
python3 - <<'PY'
import pathlib
import re
cfg_path = pathlib.Path("/opt/cli-proxy-api/config.yaml")
text = cfg_path.read_text(encoding="utf-8")
plugin_id = "cpa-mcp-schema-patch"
snippet = """    cpa-mcp-schema-patch:
      enabled: true
      priority: 5
      debug: false
      schemas-dir: mcp-schemas
      only-empty: true
      logs-dir: logs
      detail-log: auto
"""
if re.search(r"(?m)^\s*cpa-mcp-schema-patch\s*:", text):
    print("config: cpa-mcp-schema-patch already present, left untouched")
else:
    # Find plugins.configs block and append
    m = re.search(r"(?m)^(\s*)configs\s*:\s*$", text)
    if not m:
        # append full plugins section tail
        if re.search(r"(?m)^plugins\s*:", text):
            text = text.rstrip() + "\n  configs:\n" + snippet + "\n"
        else:
            text = text.rstrip() + "\n\nplugins:\n  enabled: true\n  dir: plugins\n  configs:\n" + snippet + "\n"
        cfg_path.write_text(text, encoding="utf-8")
        print("config: appended plugins.configs entry")
    else:
        # Insert after first configs: line - find end of configs section is hard; append right after configs:
        lines = text.splitlines(keepends=True)
        out = []
        inserted = False
        for i, line in enumerate(lines):
            out.append(line)
            if not inserted and re.match(r"^\s*configs\s*:\s*$", line):
                out.append(snippet if snippet.endswith("\n") else snippet + "\n")
                inserted = True
        cfg_path.write_text("".join(out), encoding="utf-8")
        print("config: inserted cpa-mcp-schema-patch under configs")
PY

sudo systemctl restart cli-proxy-api
sleep 3
sudo systemctl is-active cli-proxy-api

trap - ERR
echo "plugin deploy succeeded"
printf 'PLUGIN_BACKUP=%s\n' "$PLUGIN_BACKUP"
printf 'DEPLOY_LOG=%s\n' "$LOG"
ls -lh "$APP/plugins/linux/arm64"/$PLUGIN.so