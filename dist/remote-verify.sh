set -euo pipefail
APP=/opt/cli-proxy-api
BACKUP=/opt/cli-proxy-api/backups/pre-custom-7.2.130.0001-20260813003149.tar.gz
printf 'SERVICE=%s\n' "$(systemctl is-active cli-proxy-api)"
printf 'PID=%s\n' "$(systemctl show -p MainPID --value cli-proxy-api)"
printf 'VERSION=%s\n' "$("$APP/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version' || true)"
printf 'CONFIG_SIZE=%s\n' "$(stat -c '%s' "$APP/config.yaml")"
printf 'CONFIG_EXAMPLE_SIZE=%s\n' "$(stat -c '%s' "$APP/config.example.yaml")"
printf 'AUTH_EXISTS=%s\n' "$(test -d "$APP/auths" && echo yes || echo no)"
printf 'GITSTORE_EXISTS=%s\n' "$(test -d "$APP/gitstore" && echo yes || echo no)"
printf 'PLUGINS_EXISTS=%s\n' "$(test -d "$APP/plugins" && echo yes || echo no)"
printf 'PLUGIN_SO_COUNT=%s\n' "$(find "$APP/plugins" -name '*.so' 2>/dev/null | wc -l)"
printf 'STATIC_FILE_COUNT=%s\n' "$(find "$APP/static" -maxdepth 1 -type f 2>/dev/null | wc -l)"
printf 'LOG_FILE_COUNT=%s\n' "$(find "$APP/logs" -maxdepth 1 -type f 2>/dev/null | wc -l)"
printf 'BACKUP_SIZE=%s\n' "$(stat -c '%s' "$BACKUP")"
echo 'BACKUP_TOP:'
tar -tzf "$BACKUP" | sed -n '1,20p'
echo 'JOURNAL_SNIP:'
journalctl -u cli-proxy-api.service -n 80 --no-pager | grep -Ei 'error|failed|panic|CLIProxyAPI|plugin|xai-ip' || true
echo 'MGMT_HTTP:'
curl --max-time 5 -sS -o /dev/null -w 'code=%{http_code}\n' http://127.0.0.1:18457/v0/management/config || true
# plugin enable flags without secrets
python3 - <<'PY'
import yaml
with open('/opt/cli-proxy-api/config.yaml','r',encoding='utf-8') as f:
    cfg=yaml.safe_load(f) or {}
plugins=cfg.get('plugins') or {}
print('plugins.enabled=', plugins.get('enabled'))
configs=plugins.get('configs') or {}
xai=configs.get('cpa-xai-ip-switcher') or {}
print('cpa-xai-ip-switcher.enabled=', xai.get('enabled'))
PY
# cleanup temps
rm -f /tmp/cliproxy-source-7.2.130.0001-20260813082807.tar.gz
rm -rf /tmp/cliproxy-build-7.2.130.0001-20260813082807
rm -rf /tmp/cliproxy-stage-7.2.130.0001-20260813082807
rm -f /tmp/cli-proxy-api-7.2.130.0001-linux-arm64.tar.gz
rm -rf /tmp/cliproxy-deploy-20260813003149
rm -f /tmp/cliproxy-remote-build-20260813082807.sh
rm -f /tmp/cliproxy-remote-deploy-20260813082807.sh
echo CLEANUP_OK
ls /tmp/cliproxy-* 2>/dev/null || echo 'no leftover cliproxy temps'
ls /tmp/cli-proxy-api-7.2.130* 2>/dev/null || echo 'no leftover pkg'