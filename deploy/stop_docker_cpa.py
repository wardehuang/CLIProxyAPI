#!/usr/bin/env python3
"""Stop only the hook-min-v2 Docker service on Oracle 01."""

from __future__ import annotations

import shlex
import subprocess
import sys
from pathlib import Path

SSH_KEY_PATH = Path("E:/Files/SSH Key/oracle-ssh-key-2026-05-16.key")
SSH_PORT = 27312
SSH_USER = "ubuntu"
SSH_HOST = "163.192.9.157"
REMOTE_TARGET = f"{SSH_USER}@{SSH_HOST}"
REMOTE_ROOT = "/opt/cli-proxy-api-hook-min-v2"
COMPOSE_FILE = f"{REMOTE_ROOT}/docker-compose.18458.yml"
COMPOSE_PROJECT = "cli-proxy-api-hook-min-v2"
CONTAINER_NAME = "cli-proxy-api-hook-min-v2"
PORT = 18458


def run_remote(command: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [
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
            command,
        ],
        text=True,
        encoding="utf-8",
        errors="replace",
        check=False,
    )


def main() -> int:
    if not SSH_KEY_PATH.is_file():
        print(f"SSH key does not exist: {SSH_KEY_PATH}", file=sys.stderr)
        return 1

    q = shlex.quote
    command = f"""set -euo pipefail
COMPOSE={q(COMPOSE_FILE)}
PROJECT={q(COMPOSE_PROJECT)}
CONTAINER={q(CONTAINER_NAME)}
PORT={PORT}

if [ ! -f "$COMPOSE" ]; then
  printf 'ERROR: Docker compose file is missing: %s\\n' "$COMPOSE" >&2
  exit 1
fi

sudo docker compose -p "$PROJECT" -f "$COMPOSE" stop

if ss -ltn 2>/dev/null | grep -F ":$PORT" >/dev/null; then
  printf 'ERROR: Docker port %s is still listening\\n' "$PORT" >&2
  exit 1
fi

printf 'STOP_RESULT=success\\n'
printf 'CONTAINER=%s\\n' "$CONTAINER"
printf 'PORT=%s\\n' "$PORT"
"""
    result = run_remote(command)
    if result.returncode != 0:
        return result.returncode
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
