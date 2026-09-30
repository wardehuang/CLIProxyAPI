#!/usr/bin/env bash
set -euo pipefail

# Strip possible Windows CR from env vars
VERSION="$(printf '%s' "${VERSION:?}" | tr -d '\r')"
COMMIT="$(printf '%s' "${COMMIT:?}" | tr -d '\r')"
BUILT_AT="$(printf '%s' "${BUILT_AT:?}" | tr -d '\r')"
DEPLOY_ID="$(printf '%s' "${DEPLOY_ID:?}" | tr -d '\r')"
SRC_PKG="$(printf '%s' "${SRC_PKG:?}" | tr -d '\r')"

APP=/opt/cli-proxy-api
BUILD_DIR="/tmp/${DEPLOY_ID}-build"
STAGE="/tmp/${DEPLOY_ID}-stage"
DEPLOY_LOG_DIR="$APP/deploy-logs"
DEPLOY_LOG="$DEPLOY_LOG_DIR/${DEPLOY_ID}.log"
BACKUP_DIR="$APP/backups"
TS="$(date +%Y%m%d%H%M%S)"
BIN_BACKUP="$APP/cli-proxy-api.bak.custom.${VERSION}.${TS}"
CONFIG_EXAMPLE_BACKUP="$APP/config.example.yaml.bak.custom.${VERSION}.${TS}"
DATA_BACKUP="$BACKUP_DIR/pre-custom-${VERSION}-${TS}.tar.gz"

mkdir -p "$DEPLOY_LOG_DIR" "$BACKUP_DIR"
exec > >(tee -a "$DEPLOY_LOG") 2>&1

echo "=== CPA UPDATE DEPLOY START ==="
echo "DEPLOY_ID=$DEPLOY_ID"
echo "VERSION=$VERSION"
echo "COMMIT=$COMMIT"
echo "BUILT_AT=$BUILT_AT"
echo "SRC_PKG=$SRC_PKG"
date -Is

cleanup_temp() {
  rm -rf "$BUILD_DIR" "$STAGE" || true
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
rm -rf "$BUILD_DIR" "$STAGE"
mkdir -p "$BUILD_DIR" "$STAGE"
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

VERSION_LINE="$("$STAGE/cli-proxy-api" --help 2>&1 | grep 'CLIProxyAPI Version' || true)"
echo "STAGE_VERSION_LINE=$VERSION_LINE"
if ! printf '%s' "$VERSION_LINE" | grep -Fq "CLIProxyAPI Version: ${VERSION}"; then
  echo "package version check failed want=${VERSION} got=${VERSION_LINE}" >&2
  exit 1
fi

echo "=== BACKUP ==="
sudo systemctl stop cli-proxy-api

cd "$APP"
items=()
for p in cli-proxy-api config.yaml config.example.yaml LICENSE README.md README_CN.md; do
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

echo "=== INSTALL ==="
install -m 755 "$STAGE/cli-proxy-api" "$APP/cli-proxy-api.new"
mv "$APP/cli-proxy-api.new" "$APP/cli-proxy-api"
[[ -f "$STAGE/LICENSE" ]] && cp -f "$STAGE/LICENSE" "$APP/"
[[ -f "$STAGE/README.md" ]] && cp -f "$STAGE/README.md" "$APP/"
[[ -f "$STAGE/README_CN.md" ]] && cp -f "$STAGE/README_CN.md" "$APP/"
[[ -f "$STAGE/config.example.yaml" ]] && cp -f "$STAGE/config.example.yaml" "$APP/"

# Do not rewrite config.yaml. Only refresh example.
# Optional safe merge of new keys is skipped if no reliable full merge tool; report only.

echo "=== START ==="
sudo systemctl start cli-proxy-api
sleep 3
if ! systemctl is-active --quiet cli-proxy-api; then
  echo "service not active" >&2
  exit 1
fi

INSTALLED_LINE="$(/opt/cli-proxy-api/cli-proxy-api --help 2>&1 | grep 'CLIProxyAPI Version' || true)"
echo "INSTALLED_VERSION_LINE=$INSTALLED_LINE"
if ! printf '%s' "$INSTALLED_LINE" | grep -Fq "CLIProxyAPI Version: ${VERSION}"; then
  echo "installed version check failed want=${VERSION} got=${INSTALLED_LINE}" >&2
  exit 1
fi

echo "=== RECENT LOGS ==="
sudo journalctl -u cli-proxy-api -n 30 --no-pager || true

trap - ERR
cleanup_temp
echo "=== CPA UPDATE DEPLOY SUCCESS ==="
date -Is
echo "DEPLOY_LOG=$DEPLOY_LOG"
echo "BIN_BACKUP=$BIN_BACKUP"
