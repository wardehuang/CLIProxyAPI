#!/usr/bin/env bash
set -euo pipefail

VERSION="${VERSION:?}"
COMMIT="${COMMIT:?}"
BUILT_AT="${BUILT_AT:?}"
DEPLOY_ID="${DEPLOY_ID:?}"
SRC_PKG="${SRC_PKG:?}"

APP=/opt/cli-proxy-api
PLUGIN_DIR="$APP/plugins/linux/arm64"
BUILD_DIR="/tmp/${DEPLOY_ID}-build"
STAGE="/tmp/${DEPLOY_ID}-stage"
PLUGIN_STAGE="/tmp/${DEPLOY_ID}-plugins"
DEPLOY_LOG_DIR="$APP/deploy-logs"
DEPLOY_LOG="$DEPLOY_LOG_DIR/${DEPLOY_ID}.log"
BACKUP_DIR="$APP/backups"
TS="$(date +%Y%m%d%H%M%S)"
BIN_BACKUP="$APP/cli-proxy-api.bak.custom.${VERSION}.${TS}"
CONFIG_EXAMPLE_BACKUP="$APP/config.example.yaml.bak.custom.${VERSION}.${TS}"
PLUGIN_BACKUP="$BACKUP_DIR/plugins-linux-arm64.pre-custom.${VERSION}.${TS}.tar.gz"
DATA_BACKUP="$BACKUP_DIR/pre-custom-${VERSION}-${TS}.tar.gz"

PLUGINS=(
  cpa-codex-openai-context
  cpa-prompt-cache-usage
  cpa-compact-route-rewriter
  cpa-antigravity-priority-scheduler
  cpa-xai-403-priority
  cpa-strip-visible-files
)

mkdir -p "$DEPLOY_LOG_DIR" "$BACKUP_DIR"
exec > >(tee -a "$DEPLOY_LOG") 2>&1

echo "=== CPA FULL DEPLOY START ==="
echo "DEPLOY_ID=$DEPLOY_ID"
echo "VERSION=$VERSION"
echo "COMMIT=$COMMIT"
echo "BUILT_AT=$BUILT_AT"
echo "SRC_PKG=$SRC_PKG"
date -Is

cleanup_temp() {
  rm -rf "$BUILD_DIR" "$STAGE" "$PLUGIN_STAGE" || true
  rm -f "$SRC_PKG" || true
}

rollback() {
  echo "=== ROLLBACK START ==="
  if [[ -f "$BIN_BACKUP" ]]; then
    cp -a "$BIN_BACKUP" "$APP/cli-proxy-api"
    chmod 755 "$APP/cli-proxy-api"
    echo "restored binary from $BIN_BACKUP"
  fi
  if [[ -f "$CONFIG_EXAMPLE_BACKUP" ]]; then
    cp -a "$CONFIG_EXAMPLE_BACKUP" "$APP/config.example.yaml"
    echo "restored config.example.yaml"
  fi
  if [[ -f "$PLUGIN_BACKUP" ]]; then
    mkdir -p "$PLUGIN_DIR"
    tar -xzf "$PLUGIN_BACKUP" -C "$PLUGIN_DIR"
    echo "restored plugins from $PLUGIN_BACKUP"
  fi
  sudo systemctl start cli-proxy-api >/dev/null 2>&1 || true
  sleep 2
  systemctl is-active cli-proxy-api || true
  echo "=== ROLLBACK END ==="
}

on_error() {
  echo "ERROR: deploy failed"
  rollback
  cleanup_temp
  exit 1
}
trap on_error ERR

echo "=== BUILD ==="
rm -rf "$BUILD_DIR" "$STAGE" "$PLUGIN_STAGE"
mkdir -p "$BUILD_DIR" "$STAGE" "$PLUGIN_STAGE"
tar -xzf "$SRC_PKG" -C "$BUILD_DIR"
cd "$BUILD_DIR"

export GOOS=linux
export GOARCH=arm64
export CGO_ENABLED=1

go build -trimpath \
  -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILT_AT}" \
  -o "$STAGE/cli-proxy-api" ./cmd/server

cp -f LICENSE README.md README_CN.md config.example.yaml "$STAGE/" 2>/dev/null || true
chmod 755 "$STAGE/cli-proxy-api"
"$STAGE/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version' || true
if ! "$STAGE/cli-proxy-api" --help 2>&1 | grep -q "CLIProxyAPI Version: ${VERSION}"; then
  echo "package version check failed" >&2
  exit 1
fi

for plugin_id in "${PLUGINS[@]}"; do
  plugin_src="$BUILD_DIR/plugins/src/${plugin_id}"
  if [[ ! -d "$plugin_src" ]]; then
    echo "missing plugin source: $plugin_src" >&2
    exit 1
  fi
  echo "building plugin $plugin_id"
  (
    cd "$plugin_src"
    go mod tidy
    go build -buildmode=c-shared -o "$PLUGIN_STAGE/${plugin_id}.so" .
  )
  test -s "$PLUGIN_STAGE/${plugin_id}.so"
done

echo "=== BACKUP ==="
sudo systemctl stop cli-proxy-api

cd "$APP"
items=()
for p in cli-proxy-api config.yaml config.example.yaml LICENSE README.md README_CN.md plugins; do
  if [[ -e "$p" ]]; then
    items+=("$p")
  fi
done
if ((${#items[@]} > 0)); then
  tar -czf "$DATA_BACKUP" "${items[@]}"
  echo "data backup: $DATA_BACKUP"
fi
cp -a "$APP/cli-proxy-api" "$BIN_BACKUP"
[[ -f "$APP/config.example.yaml" ]] && cp -a "$APP/config.example.yaml" "$CONFIG_EXAMPLE_BACKUP"
if [[ -d "$PLUGIN_DIR" ]]; then
  tar -czf "$PLUGIN_BACKUP" -C "$PLUGIN_DIR" .
  echo "plugin backup: $PLUGIN_BACKUP"
fi

echo "=== INSTALL ==="
install -m 755 "$STAGE/cli-proxy-api" "$APP/cli-proxy-api.new"
mv "$APP/cli-proxy-api.new" "$APP/cli-proxy-api"
[[ -f "$STAGE/LICENSE" ]] && cp -f "$STAGE/LICENSE" "$APP/"
[[ -f "$STAGE/README.md" ]] && cp -f "$STAGE/README.md" "$APP/"
[[ -f "$STAGE/README_CN.md" ]] && cp -f "$STAGE/README_CN.md" "$APP/"
[[ -f "$STAGE/config.example.yaml" ]] && cp -f "$STAGE/config.example.yaml" "$APP/"

mkdir -p "$PLUGIN_DIR"
for plugin_id in "${PLUGINS[@]}"; do
  install -m 755 "$PLUGIN_STAGE/${plugin_id}.so" "$PLUGIN_DIR/${plugin_id}.so"
  echo "installed $PLUGIN_DIR/${plugin_id}.so"
done

echo "=== CONFIG PLUGIN ENTRY ==="
python3 - <<'PY'
from pathlib import Path
import re
import yaml

config_path = Path("/opt/cli-proxy-api/config.yaml")
raw = config_path.read_text(encoding="utf-8")
data = yaml.safe_load(raw) or {}
plugins = data.get("plugins") or {}
configs = plugins.get("configs") or {}
if "cpa-strip-visible-files" in configs:
    print("kept existing cpa-strip-visible-files config")
else:
    # Append only the missing plugin block; do not rewrite whole config.yaml.
    block = (
        "    cpa-strip-visible-files:\n"
        "      enabled: true\n"
        "      priority: 0\n"
        "      detail-log: auto\n"
        "      logs-dir: logs\n"
    )
    if re.search(r"(?m)^\s*configs:\s*$", raw):
        # Insert after configs: line
        raw2, count = re.subn(
            r"(?m)^(?P<indent>\s*)configs:\s*\n",
            lambda m: m.group(0) + block,
            raw,
            count=1,
        )
        if count != 1:
            raise SystemExit("failed to insert under configs")
        config_path.write_text(raw2, encoding="utf-8")
        print("added cpa-strip-visible-files under existing configs")
    else:
        raise SystemExit("plugins.configs section not found; manual config required")
print("plugin ids:", sorted(list((yaml.safe_load(config_path.read_text(encoding='utf-8')) or {}).get('plugins', {}).get('configs', {}).keys())))
PY

echo "=== START ==="
sudo systemctl start cli-proxy-api
sleep 3
if ! systemctl is-active --quiet cli-proxy-api; then
  echo "service not active" >&2
  exit 1
fi
/opt/cli-proxy-api/cli-proxy-api --help 2>&1 | grep 'CLIProxyAPI Version' || true
if ! /opt/cli-proxy-api/cli-proxy-api --help 2>&1 | grep -q "CLIProxyAPI Version: ${VERSION}"; then
  echo "installed version check failed" >&2
  exit 1
fi

echo "=== PLUGIN FILES ==="
ls -lh "$PLUGIN_DIR"/cpa-*.so

echo "=== RECENT LOGS ==="
sudo journalctl -u cli-proxy-api -n 40 --no-pager || true
if [[ -f "$APP/logs/main.log" ]]; then
  grep -E 'plugin|cpa-strip|loaded|error' "$APP/logs/main.log" | tail -n 40 || true
fi

trap - ERR
cleanup_temp
echo "=== CPA FULL DEPLOY SUCCESS ==="
date -Is
echo "DEPLOY_LOG=$DEPLOY_LOG"
echo "BIN_BACKUP=$BIN_BACKUP"
echo "PLUGIN_BACKUP=$PLUGIN_BACKUP"
