#!/usr/bin/env bash
set -euo pipefail
DEPLOY_ID=cpa-plugins-66e43fef-dirty-20260813233027
rm -rf "/tmp/${DEPLOY_ID}-build" "/tmp/${DEPLOY_ID}-stage" "/tmp/${DEPLOY_ID}-source.tar.gz"
rm -f /tmp/plugin-remote-build.sh /tmp/plugin-safe-deploy.sh
rm -f /tmp/plugin-post-verify.sh /tmp/plugin-post-verify2.sh /tmp/plugin-post-cleanup.sh
rm -f /tmp/cpa-plugin-deploy-nohup.out
echo CLEANED=1
echo '=== leftover ==='
ls -1 /tmp/cpa-plugins-* /tmp/plugin-remote-* /tmp/plugin-safe-* /tmp/plugin-post-* 2>/dev/null || echo none
