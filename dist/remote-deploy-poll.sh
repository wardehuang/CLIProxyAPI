set -e
rm -f /tmp/remote-deploy-run.log
nohup bash /tmp/remote-deploy-7.2.111.0001.sh > /tmp/remote-deploy-run.log 2>&1 &
DPID=$!
echo PID=$DPID
for i in $(seq 1 150); do
  if grep -qE 'STATUS=active|ROLLBACK_TRIGGERED|health check timed out|package version check failed' /tmp/remote-deploy-run.log 2>/dev/null; then
    break
  fi
  if ! kill -0 $DPID 2>/dev/null; then
    sleep 1
    break
  fi
  sleep 2
done
echo '===== DEPLOY LOG ====='
cat /tmp/remote-deploy-run.log
echo '===== SERVICE ====='
systemctl is-active cli-proxy-api || true
/opt/cli-proxy-api/cli-proxy-api --help 2>&1 | grep 'CLIProxyAPI Version' || true
