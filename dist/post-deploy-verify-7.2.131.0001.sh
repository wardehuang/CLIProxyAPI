#!/usr/bin/env bash
set -euo pipefail
APP=/opt/cli-proxy-api
BACKUP=/opt/cli-proxy-api/backups/pre-custom-7.2.131.0001-20260813152132.tar.gz
printf 'SERVICE=%s\n' "$(systemctl is-active cli-proxy-api)"
printf 'PID=%s\n' "$(systemctl show -p MainPID --value cli-proxy-api)"
printf 'VERSION='
"$APP/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version' || true
printf 'CONFIG_SIZE=%s\n' "$(stat -c '%s' "$APP/config.yaml")"
printf 'AUTH_EXISTS='; test -d "$APP/auths" && echo yes || echo no
printf 'GITSTORE_EXISTS='; test -d "$APP/gitstore" && echo yes || echo no
printf 'STATIC_FILE_COUNT=%s\n' "$(find "$APP/static" -maxdepth 1 -type f 2>/dev/null | wc -l)"
printf 'LOG_FILE_COUNT=%s\n' "$(find "$APP/logs" -maxdepth 1 -type f 2>/dev/null | wc -l)"
printf 'PLUGIN_FILE_COUNT=%s\n' "$(find "$APP/plugins" -maxdepth 1 -type f 2>/dev/null | wc -l)"
printf 'CONFIG_EXAMPLE_SIZE=%s\n' "$(stat -c '%s' "$APP/config.example.yaml")"
printf 'BACKUP_SIZE=%s\n' "$(stat -c '%s' "$BACKUP")"
echo '=== BACKUP_HEAD ==='
tar -tzf "$BACKUP" | sed -n '1,20p'
echo '=== PLUGIN_SO ==='
ls -la "$APP/plugins" 2>/dev/null || echo none
echo '=== HEALTH_HEADERS ==='
curl --max-time 5 -sS -D - -o /dev/null http://127.0.0.1:18457/v0/management/config | tr -d '\r'
echo '=== JOURNAL ==='
journalctl -u cli-proxy-api.service -n 120 --no-pager | grep -Ei 'error|failed|panic|CLIProxyAPI|plugin|cpa-xai' || true
echo '=== DEPLOY_LOG_TAIL ==='
tail -n 40 /opt/cli-proxy-api/deploy-logs/custom-7.2.131.0001-20260813152132.log
