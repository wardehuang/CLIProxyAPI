set -euo pipefail

APP="/opt/cli-proxy-api"
PKG="/tmp/cli-proxy-api-7.2.113.0001-linux-arm64.tar.gz"
VERSION="7.2.113.0001"
DEPLOY_ID="7.2.113.0001-20260802190000"
LOG_DIR="$APP/deploy-logs"
DEPLOY_LOG="$LOG_DIR/deploy-$DEPLOY_ID.log"
TS="$(date +%Y%m%d%H%M%S)"
TMP="/tmp/cliproxy-deploy-$DEPLOY_ID"
BACKUP_DIR="$APP/backups"
DATA_BACKUP="$BACKUP_DIR/pre-custom-$VERSION-$TS.tar.gz"
BIN_BACKUP="$APP/cli-proxy-api.bak.custom.$VERSION.$TS"
CONFIG_BACKUP="$APP/config.yaml.bak.custom.$VERSION.$TS"
CONFIG_MERGE_STATUS="not-run"

sudo install -d -m 755 "$LOG_DIR" "$BACKUP_DIR"
sudo touch "$DEPLOY_LOG"
sudo chown "$(id -u):$(id -g)" "$DEPLOY_LOG"
exec > >(tee -a "$DEPLOY_LOG") 2>&1

rollback() {
  exit_status=$?
  trap - ERR
  set +e
  echo "DEPLOY_FAILED=1"
  echo "FAILED_COMMAND=$BASH_COMMAND"

  if [ -f "$BIN_BACKUP" ]; then
    sudo install -m 755 "$BIN_BACKUP" "$APP/cli-proxy-api"
  fi

  if [ -f "$CONFIG_BACKUP" ]; then
    sudo cp -a "$CONFIG_BACKUP" "$APP/config.yaml"
  fi

  sudo systemctl start cli-proxy-api
  echo "ROLLBACK_SERVICE=$(systemctl is-active cli-proxy-api || true)"
  exit "$exit_status"
}

require_version() {
  binary_path="$1"
  version_output="$("$binary_path" --help 2>&1)"

  case "$version_output" in
    *"CLIProxyAPI Version: $VERSION"*) ;;
    *)
      printf '%s\n' "$version_output"
      return 1
      ;;
  esac
}

trap rollback ERR
rm -rf "$TMP"
mkdir -p "$TMP"
tar -xzf "$PKG" -C "$TMP"
chmod 755 "$TMP/cli-proxy-api"
require_version "$TMP/cli-proxy-api"
test -s "$TMP/config.example.yaml"

sudo systemctl stop cli-proxy-api
cd "$APP"
items=()
for path_name in cli-proxy-api config.yaml auths gitstore objectstore pgstore static logs plugins .env LICENSE README.md README_CN.md config.example.yaml; do
  if [ -e "$path_name" ]; then
    items+=("$path_name")
  fi
done
tar -czf "$DATA_BACKUP" "${items[@]}"
sudo cp -a "$APP/cli-proxy-api" "$BIN_BACKUP"

if [ -f "$APP/config.yaml" ]; then
  sudo cp -a "$APP/config.yaml" "$CONFIG_BACKUP"
fi

sudo install -m 755 "$TMP/cli-proxy-api" "$APP/cli-proxy-api.new"
sudo mv "$APP/cli-proxy-api.new" "$APP/cli-proxy-api"
sudo cp -f "$TMP/LICENSE" "$TMP/README.md" "$TMP/README_CN.md" "$TMP/config.example.yaml" "$APP/"

if [ -f "$CONFIG_BACKUP" ]; then
  if python3 - <<'PY' >/dev/null 2>&1
import yaml
PY
  then
    python3 - "$APP/config.example.yaml" "$CONFIG_BACKUP" "$TMP/config.yaml.merged" <<'PY'
import sys
import yaml

example_path, old_path, output_path = sys.argv[1:]
with open(example_path, "r", encoding="utf-8") as example_file:
    default_configuration = yaml.safe_load(example_file) or {}
with open(old_path, "r", encoding="utf-8") as old_file:
    old_configuration = yaml.safe_load(old_file) or {}

def merge_configuration(default_value, old_value):
    if isinstance(default_value, dict) and isinstance(old_value, dict):
        merged_value = dict(default_value)
        for key, value in old_value.items():
            merged_value[key] = merge_configuration(default_value.get(key), value)
        return merged_value
    return old_value

merged_configuration = merge_configuration(default_configuration, old_configuration)
with open(output_path, "w", encoding="utf-8") as output_file:
    yaml.safe_dump(merged_configuration, output_file, allow_unicode=False, sort_keys=False)
PY
    sudo cp "$TMP/config.yaml.merged" "$APP/config.yaml"
    CONFIG_MERGE_STATUS="merged-with-old-values"
  else
    sudo cp -a "$CONFIG_BACKUP" "$APP/config.yaml"
    CONFIG_MERGE_STATUS="skipped-no-python-yaml-config-kept"
  fi
else
  sudo cp "$APP/config.example.yaml" "$APP/config.yaml"
  CONFIG_MERGE_STATUS="created-from-example"
fi

sudo systemctl start cli-proxy-api
HEALTH_TIMEOUT_SECONDS=90
HEALTHY=0
for ((attempt=1; attempt<=HEALTH_TIMEOUT_SECONDS; attempt++)); do
  if systemctl is-active --quiet cli-proxy-api && curl --max-time 3 -sS -o /dev/null http://127.0.0.1:18457/v0/management/config; then
    HEALTHY=1
    break
  fi
  sleep 1
done

if [ "$HEALTHY" -ne 1 ]; then
  echo "HEALTH_TIMEOUT_SECONDS=$HEALTH_TIMEOUT_SECONDS"
  exit 1
fi

require_version "$APP/cli-proxy-api"
python3 - "$BACKUP_DIR" "$APP" <<'PY'
import glob
import os
import sys

backup_directory, application_directory = sys.argv[1:]
patterns = [
    os.path.join(backup_directory, "pre-custom-*.tar.gz"),
    os.path.join(application_directory, "cli-proxy-api.bak.custom.*"),
    os.path.join(application_directory, "config.yaml.bak.custom.*"),
]

for pattern in patterns:
    for stale_path in sorted(glob.glob(pattern), key=os.path.getmtime, reverse=True)[3:]:
        os.remove(stale_path)
        print(f"REMOVED_BACKUP={stale_path}")
PY

rm -rf "$TMP"
trap - ERR
printf "STATUS=active\n"
printf "DATA_BACKUP=%s\n" "$DATA_BACKUP"
printf "BINARY_BACKUP=%s\n" "$BIN_BACKUP"
printf "CONFIG_BACKUP=%s\n" "$CONFIG_BACKUP"
printf "CONFIG_MERGE_STATUS=%s\n" "$CONFIG_MERGE_STATUS"
printf "DEPLOY_LOG=%s\n" "$DEPLOY_LOG"
printf "VERSION_LINE="
"$APP/cli-proxy-api" --help 2>&1
