#!/usr/bin/env bash
set -euo pipefail
echo '=== REMAINING_PLUGINS ==='
pid=$(systemctl show -p MainPID --value cli-proxy-api)
journalctl -u cli-proxy-api _PID="$pid" --no-pager | grep -E 'plugin loaded plugin_id=cpa-|plugin registered plugin_id=cpa-|error|failed|panic' || true
echo '=== CONFIG_KEYS ==='
python3 - <<'PY'
import yaml
with open("/opt/cli-proxy-api/config.yaml", encoding="utf-8") as f:
    cfg = yaml.safe_load(f) or {}
with open("/opt/cli-proxy-api/backups/config.yaml.bak.plugins.cpa-plugins-66e43fef-dirty-20260813233027.20260813153253", encoding="utf-8") as f:
    old = yaml.safe_load(f) or {}

def walk(prefix, value, out):
    if isinstance(value, dict):
        for k, v in value.items():
            walk("%s.%s" % (prefix, k) if prefix else str(k), v, out)
    else:
        out.add(prefix)

new_keys, old_keys = set(), set()
walk("plugins", cfg.get("plugins") or {}, new_keys)
walk("plugins", old.get("plugins") or {}, old_keys)
added = sorted(new_keys - old_keys)
print("added_key_count=%d" % len(added))
for k in added:
    print("added=%s" % k)
print("plugins.enabled=%s" % bool((cfg.get("plugins") or {}).get("enabled")))
configs = (cfg.get("plugins") or {}).get("configs") or {}
for name in [
    "cpa-codex-openai-context",
    "cpa-prompt-cache-usage",
    "cpa-compact-route-rewriter",
    "cpa-antigravity-priority-scheduler",
    "cpa-strip-visible-files",
    "cpa-xai-ip-switcher",
    "cpa-claude-mem-adapter",
]:
    item = configs.get(name)
    if not isinstance(item, dict):
        print("%s=MISSING" % name)
        continue
    print("%s.enabled=%s keys=%s" % (name, item.get("enabled"), ",".join(sorted(item.keys()))))
PY
echo '=== MAIN_STAT ==='
stat -c 'main_mtime=%y size=%s' /opt/cli-proxy-api/cli-proxy-api
