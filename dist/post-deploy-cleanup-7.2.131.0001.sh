#!/usr/bin/env bash
set -euo pipefail
STAMP=20260813231846
VERSION=7.2.131.0001
rm -f /tmp/cliproxy-source-${VERSION}-${STAMP}.tar.gz
rm -rf /tmp/cliproxy-build-${VERSION}-${STAMP}
rm -rf /tmp/cliproxy-stage-${VERSION}-${STAMP}
rm -f /tmp/cli-proxy-api-${VERSION}-linux-arm64.tar.gz
rm -rf /tmp/cliproxy-deploy-*
rm -f /tmp/remote-build-7.2.131.0001.sh
rm -f /tmp/safe-deploy-7.2.131.0001.sh
rm -f /tmp/post-deploy-verify-7.2.131.0001.sh
rm -f /tmp/post-deploy-plugin-check.sh
rm -f /tmp/post-deploy-cleanup-7.2.131.0001.sh
rm -f /tmp/cliproxy-deploy-nohup.out
echo CLEANED=1
echo '=== leftover /tmp cliproxy ==='
ls -1 /tmp/cliproxy-* /tmp/cli-proxy-api-* /tmp/remote-build-* /tmp/safe-deploy-* /tmp/post-deploy-* 2>/dev/null || echo none
echo '=== kept backups ==='
ls -1t /opt/cli-proxy-api/backups/pre-custom-*.tar.gz 2>/dev/null | sed -n '1,3p'
ls -1t /opt/cli-proxy-api/cli-proxy-api.bak.custom.* 2>/dev/null | sed -n '1,3p'
ls -1t /opt/cli-proxy-api/config.yaml.bak.custom.* 2>/dev/null | sed -n '1,3p'
