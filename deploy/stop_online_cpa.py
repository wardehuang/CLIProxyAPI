#!/usr/bin/env python3
from __future__ import annotations

import base64
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

SCRIPT_PATH = Path(__file__).resolve()
SCRIPT_DIRECTORY = SCRIPT_PATH.parent
LOG_FILE_PATH = SCRIPT_DIRECTORY / "stop_online_cpa.log"

SSH_KEY_PATH = Path("E:/Files/SSH Key/oracle-ssh-key-2026-05-16.key")
SSH_PORT = 27312
SSH_USER = "ubuntu"
SSH_HOST = "163.192.9.157"
REMOTE_TARGET = f"{SSH_USER}@{SSH_HOST}"

REMOTE_SCRIPT = r"""
set -euo pipefail
STOP_TIMEOUT_SECONDS=60
STOP_POLL_INTERVAL_SECONDS=1
HTTP_URL='http://127.0.0.1:18457/'
LISTEN_PORT=18457

printf '== before ==\n'
systemctl is-active cli-proxy-api || true
printf 'MainPID=%s\n' "$(systemctl show -p MainPID --value cli-proxy-api || true)"
printf 'NRestarts=%s\n' "$(systemctl show -p NRestarts --value cli-proxy-api || true)"
/opt/cli-proxy-api/cli-proxy-api --help 2>&1 | sed -n '1p' || true

printf '== listen before ==\n'
ss -ltnp 2>/dev/null | sed -n "/:${LISTEN_PORT}/p" || true

printf '== stop ==\n'
sudo systemctl stop cli-proxy-api

printf '== wait stopped ==\n'
stopped=0
elapsed=0
active_state=unknown
main_pid=unknown
while [ "$elapsed" -lt "$STOP_TIMEOUT_SECONDS" ]; do
  active_state="$(systemctl is-active cli-proxy-api 2>/dev/null || true)"
  main_pid="$(systemctl show -p MainPID --value cli-proxy-api 2>/dev/null || true)"
  listening=0
  if ss -ltn 2>/dev/null | grep -F ":${LISTEN_PORT}" >/dev/null; then
    listening=1
  fi
  if [ "$active_state" != "active" ] && [ "${main_pid:-0}" = "0" ] && [ "$listening" -eq 0 ]; then
    stopped=1
    break
  fi
  sleep "$STOP_POLL_INTERVAL_SECONDS"
  elapsed=$((elapsed + STOP_POLL_INTERVAL_SECONDS))
done
printf 'stopped=%s elapsed_seconds=%s active=%s MainPID=%s\n' "$stopped" "$elapsed" "$active_state" "$main_pid"

printf '== after ==\n'
systemctl is-active cli-proxy-api || true
printf 'MainPID=%s\n' "$(systemctl show -p MainPID --value cli-proxy-api || true)"
printf 'SubState=%s\n' "$(systemctl show -p SubState --value cli-proxy-api || true)"
printf 'ActiveState=%s\n' "$(systemctl show -p ActiveState --value cli-proxy-api || true)"

printf '== listen after ==\n'
ss -ltnp 2>/dev/null | sed -n "/:${LISTEN_PORT}/p" || true

printf '== leftover ==\n'
pgrep -a -x cli-proxy-api || true

printf '== http ==\n'
http_status="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "$HTTP_URL" 2>/dev/null || true)"
printf 'HTTP_STATUS=%s\n' "${http_status:-000}"

printf '== recent logs ==\n'
sudo journalctl -u cli-proxy-api --no-pager -n 20 || true

if [ "$stopped" != "1" ]; then
  printf 'ERROR: cli-proxy-api still running after %ss\n' "$STOP_TIMEOUT_SECONDS" >&2
  exit 1
fi
if pgrep -x cli-proxy-api >/dev/null 2>&1; then
  printf 'ERROR: leftover cli-proxy-api process after stop\n' >&2
  exit 1
fi
if ss -ltn 2>/dev/null | grep -F ":${LISTEN_PORT}" >/dev/null; then
  printf 'ERROR: port %s still listening after stop\n' "$LISTEN_PORT" >&2
  exit 1
fi
""".strip()


def reset_log_file() -> None:
    if LOG_FILE_PATH.exists():
        LOG_FILE_PATH.unlink()
    LOG_FILE_PATH.touch()


def write_log(message: str) -> None:
    with LOG_FILE_PATH.open("a", encoding="utf-8", newline="") as log_file:
        log_file.write(message)
        if not message.endswith("\n"):
            log_file.write("\n")


def print_and_log(message: str) -> None:
    print(message)
    write_log(message)


def build_ssh_command() -> list[str]:
    encoded_remote_script = base64.b64encode(REMOTE_SCRIPT.encode("utf-8")).decode("ascii")
    remote_command = f"printf %s {encoded_remote_script} | base64 -d | bash"
    return [
        "ssh",
        "-i",
        str(SSH_KEY_PATH),
        "-p",
        str(SSH_PORT),
        REMOTE_TARGET,
        remote_command,
    ]


def stop_online_cpa_service() -> int:
    reset_log_file()
    started_at = datetime.now(timezone.utc).isoformat()
    print_and_log(f"stop_online_cpa started_at={started_at}")
    print_and_log(f"log_file={LOG_FILE_PATH}")

    completed_process = subprocess.run(
        build_ssh_command(),
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        check=False,
    )

    if completed_process.stdout:
        write_log("\n== stdout ==\n")
        write_log(completed_process.stdout)
        print(completed_process.stdout, end="")

    if completed_process.stderr:
        write_log("\n== stderr ==\n")
        write_log(completed_process.stderr)
        print(completed_process.stderr, end="", file=sys.stderr)

    finished_at = datetime.now(timezone.utc).isoformat()
    print_and_log(f"stop_online_cpa finished_at={finished_at}")
    print_and_log(f"exit_code={completed_process.returncode}")
    return completed_process.returncode


if __name__ == "__main__":
    raise SystemExit(stop_online_cpa_service())
