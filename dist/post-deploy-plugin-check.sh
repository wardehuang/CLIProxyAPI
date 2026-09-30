#!/usr/bin/env bash
set -euo pipefail
echo '=== PLUGIN_SO_TREE ==='
find /opt/cli-proxy-api/plugins -type f -name '*.so' -printf '%p %s\n' 2>/dev/null || true
echo '=== NEW_JOURNAL ==='
journalctl -u cli-proxy-api.service -n 200 --no-pager --since '2026-08-13 15:21:00' | grep -Ei 'error|failed|panic|plugin|cpa-xai|CLIProxyAPI Version|listening|started' || true
echo '=== MERGE_CONFIG_KEYS_ADDED ==='
python3 - <<'PY'
import yaml
with open("/opt/cli-proxy-api/config.example.yaml", encoding="utf-8") as f:
    example = yaml.safe_load(f) or {}
with open("/opt/cli-proxy-api/config.yaml", encoding="utf-8") as f:
    current = yaml.safe_load(f) or {}

def keys(prefix, value, out):
    if isinstance(value, dict):
        for k, v in value.items():
            keys(f"{prefix}.{k}" if prefix else str(k), v, out)
    else:
        out.add(prefix)

ex, cur = set(), set()
keys("", example, ex)
keys("", current, cur)
# only report non-secret-looking added keys
secret_needles = ("token", "secret", "password", "api-key", "apikey", "oauth", "key")
added = sorted(k for k in (ex - cur) if not any(s in k.lower() for s in secret_needles))
print("example_only_nonsecret_count=%d" % len(added))
for k in added[:30]:
    print("example_only=%s" % k)
print("has_request_retry=%s" % ("request-retry" in str(current) or "request_retry" in str(current)))
PY
