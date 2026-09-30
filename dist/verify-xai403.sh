#!/bin/bash
set -euo pipefail
echo "active=$(systemctl is-active cli-proxy-api)"
echo '--- last 3 min plugin lines ---'
sudo journalctl -u cli-proxy-api --since "3 min ago" --no-pager | grep -E 'pluginhost:|cpa-xai' || true
echo '--- verify config entry ---'
sudo python3 - <<'PY'
import yaml
from pathlib import Path
data=yaml.safe_load(Path("/opt/cli-proxy-api/config.yaml").read_text()) or {}
print(data.get("plugins",{}).get("configs",{}).get("cpa-xai-403-priority"))
PY
echo '--- so files ---'
ls -lh /opt/cli-proxy-api/plugins/linux/arm64/cpa-*.so