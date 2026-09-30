set -euo pipefail
APP=/opt/cli-proxy-api
VERSION='7.2.112.0001'
STAMP='20260731165928'

echo '=== VERIFY ==='
printf 'SERVICE='; systemctl is-active cli-proxy-api
printf 'PID='; systemctl show -p MainPID --value cli-proxy-api
printf 'VERSION='; "$APP/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version' || true
printf 'CONFIG_SIZE='; stat -c '%s' "$APP/config.yaml"
printf 'AUTH_EXISTS='; test -d "$APP/auths" && echo yes || echo no
printf 'GITSTORE_EXISTS='; test -d "$APP/gitstore" && echo yes || echo no
printf 'STATIC_FILE_COUNT='; find "$APP/static" -maxdepth 1 -type f 2>/dev/null | wc -l
printf 'LOG_FILE_COUNT='; find "$APP/logs" -maxdepth 1 -type f 2>/dev/null | wc -l
printf 'CONFIG_EXAMPLE_SIZE='; stat -c '%s' "$APP/config.example.yaml"
printf 'BACKUP_SIZE='; stat -c '%s' "$APP/backups"/pre-custom-7.2.112.0001-20260731090246.tar.gz
echo 'BACKUP_TOP='
tar -tzf "$APP/backups"/pre-custom-7.2.112.0001-20260731090246.tar.gz | sed -n '1,20p'
echo 'JOURNAL_SNIP='
journalctl -u cli-proxy-api.service -n 40 --no-pager | grep -Ei 'error|failed|panic|CLIProxyAPI|Version' || true
# plugin header check without secrets
echo 'MGMT_HEADERS='
curl --max-time 5 -sS -D - -o /dev/null http://127.0.0.1:18457/v0/management/config 2>&1 | grep -Ei 'HTTP/|X-|plugin|Plugin' | head -20 || true

echo '=== CLEAN OLD BACKUPS KEEP 3 ==='
ls -1t "$APP/backups"/pre-custom-*.tar.gz 2>/dev/null | sed -n '4,$p' | while IFS= read -r old; do rm -f -- "$old" && echo "rm data $old"; done
ls -1t "$APP"/cli-proxy-api.bak.custom.* 2>/dev/null | sed -n '4,$p' | while IFS= read -r old; do rm -f -- "$old" && echo "rm bin $old"; done
ls -1t "$APP"/config.yaml.bak.custom.* 2>/dev/null | sed -n '4,$p' | while IFS= read -r old; do rm -f -- "$old" && echo "rm cfg $old"; done
echo 'KEPT_DATA='
ls -1t "$APP/backups"/pre-custom-*.tar.gz 2>/dev/null | sed -n '1,3p'
echo 'KEPT_BIN='
ls -1t "$APP"/cli-proxy-api.bak.custom.* 2>/dev/null | sed -n '1,3p'
echo 'KEPT_CFG='
ls -1t "$APP"/config.yaml.bak.custom.* 2>/dev/null | sed -n '1,3p'

echo '=== CLEAN TEMP ==='
rm -rf "/tmp/cliproxy-source-$VERSION-$STAMP.tar.gz" \
  "/tmp/cliproxy-build-$VERSION-$STAMP" \
  "/tmp/cliproxy-stage-$VERSION-$STAMP" \
  "/tmp/cli-proxy-api-$VERSION-linux-arm64.tar.gz" \
  /tmp/cliproxy-deploy-* \
  /tmp/cliproxy-remote-deploy.sh
echo 'TEMP_GONE'
ls /tmp/cliproxy-* 2>/dev/null || echo 'no cliproxy tmp left'
ls /tmp/cli-proxy-api-*-linux-arm64.tar.gz 2>/dev/null || echo 'no deploy pkg left'