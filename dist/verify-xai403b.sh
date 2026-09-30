#!/bin/bash
set -euo pipefail
echo "pid=$(systemctl show -p MainPID --value cli-proxy-api)"
echo "active=$(systemctl is-active cli-proxy-api)"
echo "ActiveEnterTimestamp=$(systemctl show -p ActiveEnterTimestamp --value cli-proxy-api)"
echo '--- full journal since deploy restart for current pid ---'
pid=$(systemctl show -p MainPID --value cli-proxy-api)
sudo journalctl -u cli-proxy-api _PID=$pid --no-pager | grep -E 'pluginhost:|cpa-xai|error|fail|warn' | head -n 100 || true
echo '--- any cpa-xai ever ---'
sudo journalctl -u cli-proxy-api --since "20 min ago" --no-pager | grep -F 'cpa-xai' || echo 'NO_XAI_LINES'