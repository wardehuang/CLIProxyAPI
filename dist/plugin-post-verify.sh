#!/usr/bin/env bash
set -euo pipefail
APP=/opt/cli-proxy-api
printf 'SERVICE=%s\n' "$(systemctl is-active cli-proxy-api)"
printf 'PID=%s\n' "$(systemctl show -p MainPID --value cli-proxy-api)"
printf 'VERSION='
"$APP/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version' || true
echo '=== PLUGIN_SO ==='
ls -lh "$APP/plugins/linux/arm64"/cpa-*.so
echo '=== HEALTH_HEADERS ==='
curl --max-time 5 -sS -D - -o /dev/null http://127.0.0.1:18457/v0/management/config | tr -d '\r'
echo '=== PLUGIN_JOURNAL ==='
pid=$(systemctl show -p MainPID --value cli-proxy-api)
journalctl -u cli-proxy-api _PID="$pid" --no-pager | grep -E 'pluginhost:|cpa-' || true
echo '=== CONFIG_PLUGIN_KEYS ==='
python3 - <<'PY'
import yaml
with open("/opt/cli-proxy-api/config.yaml", encoding="utf-8") as f:
    cfg = yaml.safe_load(f) or {}
plugins = cfg.get("plugins") or {}
print("plugins.enabled=%s" % bool(plugins.get("enabled")))
print("plugins.dir=%s" % plugins.get("dir"))
configs = plugins.get("configs") or {}
expected = [
    "cpa-codex-openai-context",
    "cpa-prompt-cache-usage",
    "cpa-compact-route-rewriter",
    "cpa-antigravity-priority-scheduler",
    "cpa-strip-visible-files",
    "cpa-xai-ip-switcher",
    "cpa-claude-mem-adapter",
]
for name in expected:
    item = configs.get(name)
    if not isinstance(item, dict):
        print("%s=MISSING" % name)
        continue
    keys = ",".join(sorted(item.keys()))
    print("%s.enabled=%s keys=%s" % (name, item.get("enabled"), keys))
PY
echo '=== MAIN_BINARY_UNCHANGED ==='
"$APP/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version' || true
stat -c 'main_mtime=%y size=%s' "$APP/cli-proxy-api"
