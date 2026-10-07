#!/usr/bin/env python3
"""Deploy hook-min-v2 as an isolated Docker service on Oracle 01."""

from __future__ import annotations

import copy
import datetime as dt
import io
import shlex
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

REPOSITORY_ROOT = Path(__file__).resolve().parent.parent
SCRIPT_DIRECTORY = Path(__file__).resolve().parent

SSH_KEY_PATH = Path("E:/Files/SSH Key/oracle-ssh-key-2026-05-16.key")
SSH_PORT = 27312
SSH_USER = "ubuntu"
SSH_HOST = "163.192.9.157"
REMOTE_TARGET = f"{SSH_USER}@{SSH_HOST}"

REMOTE_ROOT = "/opt/cli-proxy-api-hook-min-v2"
REMOTE_STAGE_ROOT = "/home/ubuntu/.cpa-docker-deploy"
REMOTE_BACKUP_ROOT = "/opt/cli-proxy-api-backups"
COMPOSE_FILE_NAME = "docker-compose.18458.yml"
COMPOSE_PROJECT = "cli-proxy-api-hook-min-v2"
CONTAINER_NAME = "cli-proxy-api-hook-min-v2"
IMAGE_NAME = "cli-proxy-api-hook-min-v2"
HOST_PORT = 18458
CONTAINER_PORT = 18458
PLUGIN_BUILDER_IMAGE = "golang:1.26-bookworm"
PLUGIN_GOOS = "linux"
PLUGIN_GOARCH = "arm64"

TEXT_NAMES = {"Dockerfile", "Makefile"}
TEXT_SUFFIXES = {
    ".c",
    ".cc",
    ".cpp",
    ".css",
    ".go",
    ".h",
    ".html",
    ".ini",
    ".js",
    ".json",
    ".md",
    ".mod",
    ".ps1",
    ".py",
    ".rs",
    ".sh",
    ".sum",
    ".toml",
    ".ts",
    ".txt",
    ".yaml",
    ".yml",
}


class DeploymentError(RuntimeError):
    """Raised when a local or remote deployment safety check fails."""


def run(command: list[str], *, cwd: Path | None = None, capture: bool = False) -> subprocess.CompletedProcess[str]:
    print("$ " + shlex.join(command))
    return subprocess.run(
        command,
        cwd=cwd,
        text=True,
        encoding="utf-8",
        errors="replace",
        capture_output=capture,
        check=False,
    )


def git_output(*arguments: str) -> str:
    result = run(["git", *arguments], cwd=REPOSITORY_ROOT, capture=True)
    if result.returncode != 0:
        raise DeploymentError(result.stderr.strip() or f"git command failed: {arguments}")
    return result.stdout.strip()


def discover_plugin_names() -> list[str]:
    """Return every tracked Go plugin module under plugins/src."""

    tracked_paths = git_output(
        "ls-tree",
        "-r",
        "--name-only",
        "HEAD",
        "--",
        "plugins/src",
    ).splitlines()
    names = sorted(
        {
            parts[2]
            for path in tracked_paths
            if (parts := path.split("/"))[:2] == ["plugins", "src"]
            and len(parts) == 4
            and parts[3] == "go.mod"
        }
    )
    return names


def shell_array(values: list[str]) -> str:
    """Render a safe Bash array body for the generated remote script."""

    return "\n".join(f"  {shlex.quote(value)}" for value in values)


def require_success(result: subprocess.CompletedProcess[str], description: str) -> None:
    if result.returncode != 0:
        raise DeploymentError(f"{description} failed with exit code {result.returncode}")


def ssh_base() -> list[str]:
    return [
        "ssh",
        "-o",
        "BatchMode=yes",
        "-o",
        "ConnectTimeout=15",
        "-i",
        str(SSH_KEY_PATH),
        "-p",
        str(SSH_PORT),
        REMOTE_TARGET,
    ]


def scp_base() -> list[str]:
    return [
        "scp",
        "-o",
        "BatchMode=yes",
        "-o",
        "ConnectTimeout=15",
        "-i",
        str(SSH_KEY_PATH),
        "-P",
        str(SSH_PORT),
    ]


def run_ssh(command: str) -> None:
    require_success(run([*ssh_base(), command]), "SSH command")


def text_member(member: tarfile.TarInfo) -> bool:
    name = Path(member.name).name
    return name in TEXT_NAMES or Path(name).suffix.lower() in TEXT_SUFFIXES


def build_normalized_archive(destination: Path) -> None:
    result = subprocess.run(
        ["git", "archive", "--format=tar", "--prefix=source/", "HEAD"],
        cwd=REPOSITORY_ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if result.returncode != 0:
        detail = result.stderr.decode("utf-8", errors="replace").strip()
        raise DeploymentError(detail or "git archive failed")

    with tarfile.open(fileobj=io.BytesIO(result.stdout), mode="r:") as source_tar:
        with tarfile.open(destination, mode="w:gz") as output_tar:
            for member in source_tar.getmembers():
                copied_member = copy.copy(member)
                if not member.isfile():
                    output_tar.addfile(copied_member)
                    continue
                source_file = source_tar.extractfile(member)
                if source_file is None:
                    raise DeploymentError(f"cannot read archive member: {member.name}")
                data = source_file.read()
                if text_member(member):
                    data = data.replace(b"\r\n", b"\n").replace(b"\r", b"\n")
                copied_member.size = len(data)
                output_tar.addfile(copied_member, io.BytesIO(data))


def compose_text(commit: str, build_date: str, version: str) -> str:
    return f"""services:
  cli-proxy-api-hook-min-v2:
    image: {IMAGE_NAME}:{version}
    build:
      context: ./source
      dockerfile: Dockerfile
      args:
        VERSION: {version}
        COMMIT: {commit}
        BUILD_DATE: "{build_date}"
    container_name: {CONTAINER_NAME}
    ports:
      - \"{HOST_PORT}:{CONTAINER_PORT}\"
    volumes:
      - ./config.yaml:/CLIProxyAPI/config.yaml
      - ./auths:/home/ubuntu/.cli-proxy-api
      - ./logs:/CLIProxyAPI/logs
      - ./plugins:/CLIProxyAPI/plugins
      - ./plugin-data:/opt/cli-proxy-api/plugin-data
    restart: unless-stopped
"""


def remote_script(
    stage: str,
    commit: str,
    build_date: str,
    plugin_names: list[str],
) -> str:
    q = shlex.quote
    return f"""#!/usr/bin/env bash
set -euo pipefail

ROOT={q(REMOTE_ROOT)}
STAGE={q(stage)}
COMPOSE="$ROOT/{COMPOSE_FILE_NAME}"
PROJECT={q(COMPOSE_PROJECT)}
CONTAINER={q(CONTAINER_NAME)}
SERVICE_PORT={HOST_PORT}
EXPECTED_COMMIT={q(commit)}
BUILD_DATE={q(build_date)}
PLUGIN_SOURCE_ROOT="$ROOT/source/plugins/src"
PLUGIN_BUILD_ROOT="$ROOT/plugins.new"
PLUGIN_RUNTIME_ROOT="$PLUGIN_BUILD_ROOT/{PLUGIN_GOOS}/{PLUGIN_GOARCH}"
PLUGIN_BUILDER_IMAGE={q(PLUGIN_BUILDER_IMAGE)}
PLUGIN_NAMES=(
{shell_array(plugin_names)}
)
BACKUP={q(REMOTE_BACKUP_ROOT)}/hook-min-v2-$(date -u '+%Y%m%d%H%M%S')

printf '== preflight ==\\n'
if ss -ltn 2>/dev/null | grep -F ":$SERVICE_PORT" >/dev/null && [ ! -d "$ROOT" ]; then
  printf 'ERROR: requested Docker port %s is already in use\\n' "$SERVICE_PORT" >&2
  exit 1
fi

if [ -d "$ROOT" ]; then
  sudo mkdir -p "$BACKUP"
  for item in config.yaml auths docker-compose.18458.yml plugin-data plugins; do
    if [ -e "$ROOT/$item" ]; then
      sudo cp -a "$ROOT/$item" "$BACKUP/$item"
    fi
  done
  if [ -f "$COMPOSE" ]; then
    if ! sudo docker compose -p "$PROJECT" -f "$COMPOSE" down; then
      sudo docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    fi
  fi
  sudo rm -rf "$ROOT/source"
else
  sudo mkdir -p "$ROOT"
fi
sudo mkdir -p "$ROOT/source" "$ROOT/logs" "$ROOT/plugin-data" "$ROOT/plugins"
sudo tar -xzf "$STAGE/source.tar.gz" -C "$ROOT"

if [ ! -f "$ROOT/config.yaml" ]; then
  printf 'ERROR: hook-min-v2 config is missing: %s\\n' "$ROOT/config.yaml" >&2
  exit 1
fi
if [ ! -d "$ROOT/auths" ]; then
  printf 'ERROR: hook-min-v2 auth directory is missing: %s\\n' "$ROOT/auths" >&2
  exit 1
fi
if [ "$(sudo find "$ROOT/auths" -maxdepth 1 -type f -name '*.json' | wc -l)" -le 0 ]; then
  printf 'ERROR: hook-min-v2 auth directory is empty\\n' >&2
  exit 1
fi
if ! sudo grep -Eq '^[[:space:]]*port:[[:space:]]*{CONTAINER_PORT}[[:space:]]*$' "$ROOT/config.yaml"; then
  printf 'ERROR: hook-min-v2 config does not expose the expected server port %s\\n' "{CONTAINER_PORT}" >&2
  exit 1
fi

printf '== build all repository plugins ==\\n'
sudo rm -rf "$PLUGIN_BUILD_ROOT"
sudo mkdir -p "$PLUGIN_RUNTIME_ROOT"
PLUGIN_COUNT=0
if [ "${{#PLUGIN_NAMES[@]}}" -eq 0 ]; then
  printf 'PLUGIN_BUILD_SKIPPED reason=no-plugin-modules\\n'
fi
for plugin_name in "${{PLUGIN_NAMES[@]}}"; do
  plugin_source="$PLUGIN_SOURCE_ROOT/$plugin_name"
  if [ ! -d "$plugin_source" ] || [ ! -s "$plugin_source/go.mod" ]; then
    printf 'ERROR: plugin module is missing: %s\\n' "$plugin_source" >&2
    exit 1
  fi
  output_path="$PLUGIN_RUNTIME_ROOT/$plugin_name.so"
  printf 'PLUGIN_BUILD_START name=%s\\n' "$plugin_name"
  sudo docker run --rm \\
    --env CGO_ENABLED=1 \\
    --env GOOS={PLUGIN_GOOS} \\
    --env GOARCH={PLUGIN_GOARCH} \\
    --volume "$ROOT/source:/workspace/source:ro" \\
    --volume "$PLUGIN_BUILD_ROOT:/workspace/output" \\
    --workdir "/workspace/source/plugins/src/$plugin_name" \\
    "$PLUGIN_BUILDER_IMAGE" \\
    go build -buildvcs=false -buildmode=c-shared -trimpath \\
      -o "/workspace/output/{PLUGIN_GOOS}/{PLUGIN_GOARCH}/$plugin_name.so" .
  sudo rm -f "$PLUGIN_RUNTIME_ROOT/$plugin_name.h"
  sudo test -s "$output_path"
  plugin_metadata="$(sudo docker run --rm \\
    --volume "$PLUGIN_BUILD_ROOT:/workspace/output:ro" \\
    "$PLUGIN_BUILDER_IMAGE" \\
    go version -m "/workspace/output/{PLUGIN_GOOS}/{PLUGIN_GOARCH}/$plugin_name.so")"
  printf '%s\\n' "$plugin_metadata"
  for marker in 'GOOS=linux' 'GOARCH=arm64' 'CGO_ENABLED=1' '-buildmode=c-shared'; do
    case "$plugin_metadata" in
      *"$marker"*) ;;
      *) printf 'ERROR: plugin metadata marker missing name=%s marker=%s\\n' "$plugin_name" "$marker" >&2; exit 1 ;;
    esac
  done
  PLUGIN_COUNT=$((PLUGIN_COUNT + 1))
  printf 'PLUGIN_BUILD_DONE name=%s\\n' "$plugin_name"
done
if [ "$PLUGIN_COUNT" -ne "${{#PLUGIN_NAMES[@]}}" ]; then
  printf 'ERROR: plugin count mismatch expected=%s actual=%s\\n' "${{#PLUGIN_NAMES[@]}}" "$PLUGIN_COUNT" >&2
  exit 1
fi
sudo rm -rf "$ROOT/plugins"
sudo mv "$PLUGIN_BUILD_ROOT" "$ROOT/plugins"
PLUGIN_COUNT_AFTER="$(find "$ROOT/plugins/{PLUGIN_GOOS}/{PLUGIN_GOARCH}" -maxdepth 1 -type f -name '*.so' | wc -l)"
if [ "$PLUGIN_COUNT_AFTER" -ne "$PLUGIN_COUNT" ]; then
  printf 'ERROR: installed plugin count mismatch expected=%s actual=%s\\n' "$PLUGIN_COUNT" "$PLUGIN_COUNT_AFTER" >&2
  exit 1
fi

sudo cp "$STAGE/{COMPOSE_FILE_NAME}" "$COMPOSE"
sudo chown -R ubuntu:ubuntu "$ROOT"
sudo docker compose -p "$PROJECT" -f "$COMPOSE" config >/dev/null

printf '== build image ==\\n'
sudo env DOCKER_BUILDKIT=1 docker compose -p "$PROJECT" -f "$COMPOSE" build --pull

printf '== start isolated service ==\\n'
sudo docker compose -p "$PROJECT" -f "$COMPOSE" up -d --force-recreate

printf '== wait for isolated service ==\\n'
ready=0
root_http=000
healthz_http=000
status=unknown
for _ in $(seq 1 300); do
  status="$(sudo docker inspect --format '{{{{.State.Status}}}}' "$CONTAINER" 2>/dev/null || true)"
  root_http="$(curl -sS -o /dev/null -w '%{{http_code}}' --max-time 3 "http://127.0.0.1:$SERVICE_PORT/" 2>/dev/null || true)"
  healthz_http="$(curl -sS -o /dev/null -w '%{{http_code}}' --max-time 3 "http://127.0.0.1:$SERVICE_PORT/healthz" 2>/dev/null || true)"
  if [ "$status" = running ] && [ "$root_http" = 200 ] && [ "$healthz_http" = 200 ]; then
    ready=1
    break
  fi
  if [ "$status" = exited ] || [ "$status" = dead ]; then
    break
  fi
  sleep 1
done

if [ "$ready" != 1 ]; then
  printf 'ERROR: isolated Docker service not ready status=%s root_http=%s healthz_http=%s\\n' "$status" "$root_http" "$healthz_http" >&2
  sudo docker compose -p "$PROJECT" -f "$COMPOSE" ps >&2 || true
  exit 1
fi
if ! ss -ltn 2>/dev/null | grep -F ":$SERVICE_PORT" >/dev/null; then
  printf 'ERROR: Docker port %s is not listening\\n' "$SERVICE_PORT" >&2
  exit 1
fi

printf '%s\\n' "$EXPECTED_COMMIT" | sudo tee "$ROOT/.deploy-revision" >/dev/null
printf '%s\\n' "$BUILD_DATE" | sudo tee "$ROOT/.deploy-time" >/dev/null
sudo docker compose -p "$PROJECT" -f "$COMPOSE" logs --no-color --tail=100 > "$ROOT/docker-deploy.log" 2>&1 || true
sudo rm -rf "$STAGE"

printf 'DEPLOY_RESULT=success\\n'
printf 'PROJECT=%s\\n' "$PROJECT"
printf 'CONTAINER=%s\\n' "$CONTAINER"
printf 'COMMIT=%s\\n' "$EXPECTED_COMMIT"
printf 'PORT=%s\\n' "$SERVICE_PORT"
printf 'PLUGIN_COUNT=%s\\n' "$PLUGIN_COUNT"
printf 'ROOT_HTTP=%s\\n' "$root_http"
printf 'HEALTHZ_HTTP=%s\\n' "$healthz_http"
printf 'AUTH_JSON_COUNT=%s\\n' "$(sudo find "$ROOT/auths" -maxdepth 1 -type f -name '*.json' | wc -l)"
"""


def deploy() -> None:
    if not SSH_KEY_PATH.is_file():
        raise DeploymentError(f"SSH key does not exist: {SSH_KEY_PATH}")
    if git_output("branch", "--show-current") != "hook-min-v2":
        raise DeploymentError("deployment must run from branch hook-min-v2")
    commit = git_output("rev-parse", "HEAD")
    version = git_output("describe", "--tags", "--always")
    plugin_names = discover_plugin_names()
    if git_output("status", "--porcelain=v1", "--untracked-files=no"):
        raise DeploymentError("tracked working-tree changes would not be deployed")
    diff_check = run(["git", "diff", "--check"], cwd=REPOSITORY_ROOT, capture=True)
    require_success(diff_check, "git diff --check")

    build_date = dt.datetime.now(dt.timezone.utc).isoformat()
    deployment_id = f"docker-cpa-18458-{commit[:8]}-{dt.datetime.now(dt.timezone.utc):%Y%m%d%H%M%S}"
    stage = f"{REMOTE_STAGE_ROOT}/{deployment_id}"
    compose = compose_text(commit, build_date, version)

    with tempfile.TemporaryDirectory(prefix="cpa-docker-deploy-") as temporary:
        temporary_path = Path(temporary)
        archive_path = temporary_path / "source.tar.gz"
        compose_path = temporary_path / COMPOSE_FILE_NAME
        remote_script_path = temporary_path / "deploy.sh"
        build_normalized_archive(archive_path)
        compose_path.write_text(compose, encoding="utf-8", newline="\n")
        remote_script_path.write_text(
            remote_script(stage, commit, build_date, plugin_names),
            encoding="utf-8",
            newline="\n",
        )

        run_ssh(f"mkdir -p {shlex.quote(stage)}")
        require_success(
            run([*scp_base(), str(archive_path), f"{REMOTE_TARGET}:{stage}/source.tar.gz"]),
            "source upload",
        )
        require_success(
            run([*scp_base(), str(compose_path), f"{REMOTE_TARGET}:{stage}/{COMPOSE_FILE_NAME}"]),
            "compose upload",
        )
        require_success(
            run([*scp_base(), str(remote_script_path), f"{REMOTE_TARGET}:{stage}/deploy.sh"]),
            "remote script upload",
        )
        run_ssh(f"chmod 700 {shlex.quote(stage + '/deploy.sh')} && bash {shlex.quote(stage + '/deploy.sh')}")

    print("DEPLOY_RESULT=success")
    print(f"BRANCH=hook-min-v2")
    print(f"HEAD={commit}")
    print(f"VERSION={version}")
    print(f"PLUGIN_SOURCE_COUNT={len(plugin_names)}")
    print(f"REMOTE_ROOT={REMOTE_ROOT}")
    print(f"PORT={HOST_PORT}")


def main() -> int:
    try:
        deploy()
    except DeploymentError as exc:
        print(f"DEPLOY_RESULT=failed error={exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("DEPLOY_RESULT=interrupted", file=sys.stderr)
        return 130
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
