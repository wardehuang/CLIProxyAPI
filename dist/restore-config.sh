set -euo pipefail
APP=/opt/cli-proxy-api
CUR=$APP/config.yaml
BAK=$APP/backups/config-pre-cpa-mcp-schema-patch-219a2d86-dirty-20260727112506-20260727032601.yaml
TS=$(date +%Y%m%d%H%M%S)
BROKEN_SAVE=$APP/backups/config-broken-before-restore-$TS.yaml

test -s "$BAK"
sudo cp "$CUR" "$BROKEN_SAVE"
echo "saved broken to $BROKEN_SAVE"

python3 - <<'PY'
from pathlib import Path
import re

bak = Path("/opt/cli-proxy-api/backups/config-pre-cpa-mcp-schema-patch-219a2d86-dirty-20260727112506-20260727032601.yaml")
text = bak.read_text(encoding="utf-8")

block_re = re.compile(r"(?ms)^([ \t]*cpa-mcp-schema-patch:\n(?:[ \t]+.+\n)*)")
m = block_re.search(text)
if not m:
    raise SystemExit("cpa-mcp-schema-patch block not found in backup")
block = m.group(1)
if "inject-missing:" not in block:
    if re.search(r"(?m)^([ \t]+)only-empty:\s*.+$", block):
        block2 = re.sub(
            r"(?m)^([ \t]+)only-empty:\s*.+$",
            lambda mm: mm.group(0) + "\n" + mm.group(1) + "inject-missing: true",
            block,
            count=1,
        )
    else:
        block2 = re.sub(
            r"(?m)^([ \t]+)enabled:\s*.+$",
            lambda mm: mm.group(0) + "\n" + mm.group(1) + "inject-missing: true",
            block,
            count=1,
        )
    text = text[: m.start(1)] + block2 + text[m.end(1) :]
    print("added inject-missing: true")
else:
    print("inject-missing already present")

Path("/opt/cli-proxy-api/config.yaml").write_text(text, encoding="utf-8")
print("restored config.yaml bytes", len(text))
PY

python3 - <<'PY'
import yaml
from pathlib import Path
data = yaml.safe_load(Path("/opt/cli-proxy-api/config.yaml").read_text(encoding="utf-8"))
print("yaml OK top_keys_count", len(data.keys()))
plugins = data.get("plugins", {}).get("configs", {})
print("plugin_count", len(plugins))
print("has_strip", "cpa-strip-visible-files" in plugins)
print("has_mcp", "cpa-mcp-schema-patch" in plugins)
print("mcp_cfg", plugins.get("cpa-mcp-schema-patch"))
print("has_request_log", "request-log" in data)
print("has_commercial", "commercial-mode" in data)
print("has_api_keys", bool(data.get("api-keys")))
print("port", data.get("port"))
PY

sudo systemctl restart cli-proxy-api
sleep 3
sudo systemctl is-active cli-proxy-api
sudo journalctl -u cli-proxy-api --since "1 min ago" --no-pager | grep -E "plugin loaded|plugin registered|cpa-mcp|cpa-strip|error|Error|fail" | head -50
wc -c /opt/cli-proxy-api/config.yaml