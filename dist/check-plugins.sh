#!/bin/bash
set -euo pipefail
python3 <<'PY'
import yaml
from pathlib import Path
data = yaml.safe_load(Path("/opt/cli-proxy-api/config.yaml").read_text()) or {}
plugins = data.get("plugins") or {}
print("plugins_enabled=", plugins.get("enabled"))
print("plugins_dir=", plugins.get("dir"))
cfgs = plugins.get("configs") or {}
print("config_names=", sorted(cfgs.keys()))
print("has_xai_403=", "cpa-xai-403-priority" in cfgs)
for name in sorted(cfgs.keys()):
    cfg = cfgs[name]
    if isinstance(cfg, dict):
        print(name, "enabled=", cfg.get("enabled"), "priority=", cfg.get("priority"))
PY
echo '--- journal ---'
sudo journalctl -u cli-proxy-api -n 150 --no-pager | grep -E 'plugin|cpa-' | tail -n 60 || true