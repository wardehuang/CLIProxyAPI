#!/bin/bash
set -euo pipefail
CFG=/opt/cli-proxy-api/config.yaml
TS=$(date +%Y%m%d%H%M%S)
sudo cp -a "$CFG" "/opt/cli-proxy-api/backups/config.yaml.pre-xai403-$TS"
sudo python3 <<'PY'
from pathlib import Path
import yaml

cfg_path = Path("/opt/cli-proxy-api/config.yaml")
text = cfg_path.read_text()
data = yaml.safe_load(text) or {}
plugins = data.setdefault("plugins", {})
if "enabled" not in plugins:
    plugins["enabled"] = True
if "dir" not in plugins:
    plugins["dir"] = "plugins"
configs = plugins.setdefault("configs", {})
name = "cpa-xai-403-priority"
existing = configs.get(name)
if not isinstance(existing, dict):
    configs[name] = {"enabled": True, "priority": 30}
    changed = True
else:
    changed = False
    if "enabled" not in existing:
        existing["enabled"] = True
        changed = True
    if "priority" not in existing:
        existing["priority"] = 30
        changed = True
    configs[name] = existing

if not changed:
    print("config_unchanged=1")
else:
    # preserve formatting as much as practical: dump whole file via yaml
    # but only when we had to add missing keys
    out = yaml.safe_dump(data, sort_keys=False, allow_unicode=True)
    cfg_path.write_text(out)
    print("config_updated=1")
print("has_xai_403=", name in configs)
print("xai_cfg=", configs.get(name))
PY
sudo systemctl restart cli-proxy-api
sleep 3
sudo systemctl is-active cli-proxy-api
echo '--- recent journal ---'
sudo journalctl -u cli-proxy-api --since "2 min ago" --no-pager | grep -E 'plugin|cpa-|loaded|error|fail|Usage' | tail -n 80 || true