#!/usr/bin/env python3
"""Deploy the CPA main binary and all repository-managed plugins.

This script is the single local entry point for the production rollout used by
this repository. It packages the current tracked working tree, normalizes text
files to LF, builds the Linux ARM64 CGO artifacts on Oracle 01, creates a full
rollback archive, installs atomically, verifies the live service, downloads the
remote evidence logs, and removes transient build files.

Usage from the repository root:

    python deploy/deploy_cpa_full.py

Safety properties:

* The latest stable upstream release tag is fetched and its merge status is logged;
  an unmerged tag does not block deployment.
* The version base is the latest stable tag. A remote locked sequence allocates
  the next four-digit build number, so every build attempt receives a unique
  version even when an earlier attempt fails.
* Only Git-tracked files are packaged; untracked non-ignored files are refused.
* Local runtime config, environment files, auth files, and deploy logs are never
  copied into the source archive.
* All source text is normalized to LF and verified before upload and after
  extraction.
* The live service stays online during compilation. It is stopped only for the
  verified backup and atomic install window.
* Any failure after the service stop restores the main binary, configuration,
  every plugin, plugin data, and every auth JSON from the verified archive.
* After a successful deployment, Go build/module caches are removed. Every older
  file and directory under ``/opt/cli-proxy-api/backups`` is deleted, leaving
  only the rollback archive created by this run. Failure paths do not prune
  backups or Go caches.
* The local master log, remote deployment log, and service journal are saved
  beside this script under ``deploy/``.
* The script never commits, pushes, merges, or rewrites Git history.
"""

from __future__ import annotations

import datetime as dt
import hashlib
import io
import logging
import os
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Iterable, Sequence


REPOSITORY_ROOT = Path(__file__).resolve().parent.parent
SCRIPT_DIR = Path(__file__).resolve().parent

DEFAULT_SSH_KEY = Path("E:/Files/SSH Key/oracle-ssh-key-2026-05-16.key")
DEFAULT_HOST = "163.192.9.157"
DEFAULT_USER = "ubuntu"
DEFAULT_SSH_PORT = 27312
DEFAULT_SERVICE_PORT = 18457

REMOTE_APP = "/opt/cli-proxy-api"
REMOTE_AUTH_DIR = "/home/ubuntu/.cli-proxy-api"
REMOTE_SERVICE = "cli-proxy-api.service"
REMOTE_STAGE_ROOT = "/home/ubuntu/.cpa-deploy"
REMOTE_GO = "/usr/local/go/bin/go"

MANAGED_PLUGINS = (
    "cpa-antigravity-priority-scheduler",
    "cpa-codex-openai-context",
    "cpa-compact-route-rewriter",
    "cpa-prompt-cache-usage",
    "cpa-strip-visible-files",
    "cpa-xai-ip-switcher",
)

EXPECTED_PLUGIN_IDS = (
    "codexcomp",
    *MANAGED_PLUGINS,
    "gemini-cli",
)

TEXT_SUFFIXES = {
    ".c",
    ".cc",
    ".conf",
    ".cpp",
    ".css",
    ".go",
    ".h",
    ".html",
    ".ini",
    ".java",
    ".js",
    ".json",
    ".jsx",
    ".md",
    ".mod",
    ".proto",
    ".py",
    ".rs",
    ".sh",
    ".sql",
    ".sum",
    ".toml",
    ".ts",
    ".tsx",
    ".txt",
    ".xml",
    ".yaml",
    ".yml",
}
TEXT_NAMES = {
    ".dockerignore",
    ".gitattributes",
    ".gitignore",
    "Dockerfile",
    "LICENSE",
    "Makefile",
}
FORBIDDEN_SOURCE_PATHS = {
    ".env",
    "config.yaml",
}
FORBIDDEN_SOURCE_PREFIXES = (
    "auths/",
    "deploy/",
)
ALLOWED_SOURCE_PATHS = {
    "deploy/deploy_cpa_full.py",
}
STABLE_TAG_PATTERN = re.compile(r"^v(\d+\.\d+\.\d+)$")
VERSION_PATTERN = re.compile(r"^(\d+\.\d+\.\d+)\.(\d{4})$")


class DeploymentError(RuntimeError):
    """Raised when a deployment safety or verification condition fails."""


@dataclass(frozen=True)
class DeploymentConfig:
    """Fixed production endpoint configuration for the zero-argument script."""

    ssh_key: Path = DEFAULT_SSH_KEY
    host: str = DEFAULT_HOST
    user: str = DEFAULT_USER
    ssh_port: int = DEFAULT_SSH_PORT
    service_port: int = DEFAULT_SERVICE_PORT


@dataclass(frozen=True)
class TrackedEntry:
    """One tracked working-tree path with its Git index mode."""

    path: str
    mode: str


@dataclass(frozen=True)
class SourcePackage:
    """Metadata for the normalized source package uploaded to the server."""

    archive: Path
    archive_sha256: str
    source_sha256: str
    revision: str
    tracked_file_count: int
    archive_member_count: int
    normalized_text_count: int


@dataclass(frozen=True)
class DeploymentPaths:
    """Local and remote paths derived from one deployment identifier."""

    deployment_id: str
    version: str
    master_log: Path
    remote_log: Path
    journal_log: Path
    remote_base: str
    remote_archive: str
    remote_script: str


LOGGER = logging.getLogger("cpa_full_deploy")
_STEP_NUMBER = 0


def configure_logging(log_path: Path) -> None:
    """Write every local and remote output line to console and the master log."""

    log_path.parent.mkdir(parents=True, exist_ok=True)
    LOGGER.setLevel(logging.INFO)
    LOGGER.handlers.clear()
    formatter = logging.Formatter(
        fmt="%(asctime)s %(levelname)s %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S%z",
    )
    console = logging.StreamHandler(sys.stdout)
    console.setFormatter(formatter)
    file_handler = logging.FileHandler(log_path, encoding="utf-8")
    file_handler.setFormatter(formatter)
    LOGGER.addHandler(console)
    LOGGER.addHandler(file_handler)


def step(title: str) -> None:
    """Emit a clear numbered phase boundary in the master log."""

    global _STEP_NUMBER
    _STEP_NUMBER += 1
    LOGGER.info("=" * 78)
    LOGGER.info("STEP %02d: %s", _STEP_NUMBER, title)
    LOGGER.info("=" * 78)


def display_command(command: Sequence[str]) -> str:
    """Format a command for logs without invoking a shell."""

    return " ".join(shlex.quote(str(part)) for part in command)


def run_logged(
    command: Sequence[str],
    *,
    cwd: Path | None = None,
    check: bool = True,
    capture: bool = False,
) -> subprocess.CompletedProcess[str]:
    """Run one command, streaming or capturing all output into the master log."""

    command_list = [str(part) for part in command]
    LOGGER.info("RUN cwd=%s command=%s", cwd or Path.cwd(), display_command(command_list))
    if capture:
        completed = subprocess.run(
            command_list,
            cwd=str(cwd) if cwd else None,
            check=False,
            text=True,
            encoding="utf-8",
            errors="replace",
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
        )
        output = completed.stdout or ""
        for line in output.splitlines():
            LOGGER.info("OUT %s", line)
    else:
        process = subprocess.Popen(
            command_list,
            cwd=str(cwd) if cwd else None,
            text=True,
            encoding="utf-8",
            errors="replace",
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            bufsize=1,
        )
        lines: list[str] = []
        assert process.stdout is not None
        for raw_line in process.stdout:
            line = raw_line.rstrip("\r\n")
            lines.append(line)
            LOGGER.info("OUT %s", line)
        return_code = process.wait()
        output = "\n".join(lines)
        completed = subprocess.CompletedProcess(command_list, return_code, output, None)
    LOGGER.info("EXIT code=%d", completed.returncode)
    if check and completed.returncode != 0:
        raise DeploymentError(
            f"command failed with exit code {completed.returncode}: "
            f"{display_command(command_list)}"
        )
    return completed


def capture_text(command: Sequence[str], *, cwd: Path | None = None) -> str:
    """Run a command and return trimmed output while preserving it in the log."""

    return (run_logged(command, cwd=cwd, capture=True).stdout or "").strip()


def require_command(name: str) -> str:
    """Return a required local executable or fail before network activity."""

    resolved = shutil.which(name)
    if not resolved:
        raise DeploymentError(f"required command is unavailable: {name}")
    LOGGER.info("COMMAND name=%s path=%s", name, resolved)
    return resolved


def resolve_git_bash(git_path: str) -> str:
    """Resolve Git for Windows Bash without relying on Windows PATH lookup."""

    git_executable = Path(git_path).resolve()
    relative_paths = (
        Path("usr") / "bin" / "bash.exe",
        Path("bin") / "bash.exe",
    )
    for git_root in git_executable.parents:
        for relative_path in relative_paths:
            bash_path = git_root / relative_path
            if bash_path.is_file():
                resolved = str(bash_path)
                LOGGER.info("COMMAND name=bash path=%s", resolved)
                return resolved
    raise DeploymentError(f"Git Bash is unavailable beside Git executable: {git_path}")


def sha256_file(path: Path) -> str:
    """Return the SHA-256 digest for a local file."""

    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def is_text_path(path: str) -> bool:
    """Return whether a tracked path must be normalized and CR-verified."""

    pure = PurePosixPath(path)
    return pure.suffix.lower() in TEXT_SUFFIXES or pure.name in TEXT_NAMES


def normalize_lf(data: bytes) -> bytes:
    """Normalize CRLF and bare CR to LF."""

    return data.replace(b"\r\n", b"\n").replace(b"\r", b"\n")


def deployment_paths(version: str, timestamp: str) -> DeploymentPaths:
    """Build deterministic local evidence names and one isolated remote stage."""

    deployment_id = f"cpa-full-{version}-{timestamp}"
    return DeploymentPaths(
        deployment_id=deployment_id,
        version=version,
        master_log=SCRIPT_DIR / f"cpa-full-auto-{timestamp}.log",
        remote_log=SCRIPT_DIR / f"{deployment_id}-remote.log",
        journal_log=SCRIPT_DIR / f"{deployment_id}-journal.log",
        remote_base=f"{REMOTE_STAGE_ROOT}/{deployment_id}",
        remote_archive=f"{REMOTE_STAGE_ROOT}/{deployment_id}/source.tar.gz",
        remote_script=f"{REMOTE_STAGE_ROOT}/{deployment_id}/deploy.sh",
    )


def ssh_base(config: DeploymentConfig) -> list[str]:
    """Return the common SSH command prefix."""

    return [
        "ssh",
        "-i",
        str(config.ssh_key),
        "-p",
        str(config.ssh_port),
        "-o",
        "BatchMode=yes",
        f"{config.user}@{config.host}",
    ]


def scp_base(config: DeploymentConfig) -> list[str]:
    """Return the common SCP command prefix."""

    return [
        "scp",
        "-i",
        str(config.ssh_key),
        "-P",
        str(config.ssh_port),
    ]


def git_output(*arguments: str) -> str:
    """Run Git in the repository and return logged output."""

    return capture_text(["git", *arguments], cwd=REPOSITORY_ROOT)


def discover_latest_stable_tag() -> str:
    """Return the newest stable tag reachable from upstream/main."""

    raw_tags = git_output("tag", "--merged", "upstream/main", "--sort=-v:refname")
    for line in raw_tags.splitlines():
        tag = line.strip()
        if STABLE_TAG_PATTERN.fullmatch(tag):
            return tag
    raise DeploymentError("no stable vX.Y.Z tag is reachable from upstream/main")


VERSION_RESERVATION_CODE = r'''
from __future__ import annotations

import fcntl
import os
import re
import subprocess
import sys
from pathlib import Path

base = sys.argv[1]
stage_root = Path(sys.argv[2])
app = Path(sys.argv[3])
stage_root.mkdir(parents=True, exist_ok=True)
lock_path = stage_root / ".version.lock"
sequence_path = stage_root / f".version-sequence-{base}"
version_pattern = re.compile(rf"(?<!\d){re.escape(base)}\.(\d{{4}})(?!\d)")


def collect_version(text: str, values: list[int]) -> None:
    for match in version_pattern.finditer(text):
        values.append(int(match.group(1)))


with lock_path.open("a+", encoding="utf-8") as lock_handle:
    fcntl.flock(lock_handle.fileno(), fcntl.LOCK_EX)
    suffixes: list[int] = []
    if sequence_path.is_file():
        raw_sequence = sequence_path.read_text(encoding="ascii").strip()
        if raw_sequence:
            suffixes.append(int(raw_sequence))

    binary = app / "cli-proxy-api"
    if binary.is_file():
        completed = subprocess.run(
            [str(binary), "--help"],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=15,
        )
        collect_version(completed.stdout or "", suffixes)

    for root in (app / "backups", app / "deploy-logs", stage_root):
        if not root.is_dir():
            continue
        for path in root.iterdir():
            collect_version(path.name, suffixes)

    maximum = max(suffixes, default=0)
    next_suffix = maximum + 1
    if next_suffix > 9999:
        raise SystemExit(f"build sequence exhausted for {base}: {maximum}")

    temporary = sequence_path.with_name(
        sequence_path.name + f".tmp-{os.getpid()}"
    )
    temporary.write_text(f"{next_suffix}\n", encoding="ascii")
    os.replace(temporary, sequence_path)
    print(f"RESERVED_FROM_MAX={maximum:04d}")
    print(f"VERSION={base}.{next_suffix:04d}")
'''


def reserve_next_version(config: DeploymentConfig, latest_tag: str) -> str:
    """Atomically reserve the next build number on the production server."""

    tag_match = STABLE_TAG_PATTERN.fullmatch(latest_tag)
    if not tag_match:
        raise DeploymentError(f"invalid stable tag: {latest_tag}")
    base = tag_match.group(1)
    remote_command = shlex.join(
        (
            "python3",
            "-c",
            VERSION_RESERVATION_CODE,
            base,
            REMOTE_STAGE_ROOT,
            REMOTE_APP,
        )
    )
    output = capture_text([*ssh_base(config), remote_command])
    version = ""
    for line in output.splitlines():
        if line.startswith("VERSION="):
            version = line.partition("=")[2].strip()
    match = VERSION_PATTERN.fullmatch(version)
    if not match or match.group(1) != base:
        raise DeploymentError(
            f"remote version reservation returned invalid value: {version!r}"
        )
    return version


def parse_tracked_entries() -> list[TrackedEntry]:
    """Read stage-0 tracked paths and modes from the Git index."""

    raw = subprocess.run(
        ["git", "ls-files", "--stage", "-z"],
        cwd=REPOSITORY_ROOT,
        check=True,
        stdout=subprocess.PIPE,
    ).stdout
    entries: list[TrackedEntry] = []
    for record in raw.split(b"\0"):
        if not record:
            continue
        metadata, encoded_path = record.split(b"\t", 1)
        mode, _object_id, stage = metadata.decode("ascii").split()
        if stage != "0":
            raise DeploymentError(
                f"unmerged index entry blocks deployment: {encoded_path!r}"
            )
        path = encoded_path.decode("utf-8", errors="surrogateescape")
        normalized = path.replace("\\", "/")
        entries.append(TrackedEntry(path=normalized, mode=mode))
    return sorted(entries, key=lambda item: item.path)


def validate_source_path(path: str) -> None:
    """Refuse tracked operational secrets even if Git was configured incorrectly."""

    normalized = path.lstrip("./")
    lower = normalized.lower()
    if lower == "auths/.gitkeep":
        return
    if lower in FORBIDDEN_SOURCE_PATHS:
        raise DeploymentError(f"forbidden runtime file is tracked: {path}")
    if any(lower.startswith(prefix) for prefix in FORBIDDEN_SOURCE_PREFIXES) and lower not in ALLOWED_SOURCE_PATHS:
        raise DeploymentError(f"forbidden runtime directory is tracked: {path}")


def add_directory_members(archive: tarfile.TarFile, paths: Iterable[str]) -> int:
    """Add deterministic directory entries before file members."""

    directories = {"CLIProxyAPI"}
    for path in paths:
        pure = PurePosixPath("CLIProxyAPI") / PurePosixPath(path)
        directories.update(str(parent) for parent in pure.parents if str(parent) != ".")
    count = 0
    for directory in sorted(directories, key=lambda value: (value.count("/"), value)):
        info = tarfile.TarInfo(directory.rstrip("/") + "/")
        info.type = tarfile.DIRTYPE
        info.mode = 0o755
        info.uid = 0
        info.gid = 0
        info.uname = "root"
        info.gname = "root"
        info.mtime = 0
        archive.addfile(info)
        count += 1
    return count


def build_source_package(temp_dir: Path, head: str, dirty: bool) -> SourcePackage:
    """Package normalized current tracked content without ignored runtime data."""

    entries = parse_tracked_entries()
    if not entries:
        raise DeploymentError("Git index contains no tracked files")

    archive_path = temp_dir / "source.tar.gz"
    source_digest = hashlib.sha256()
    normalized_text_count = 0
    existing_entries: list[TrackedEntry] = []

    for entry in entries:
        validate_source_path(entry.path)
        local_path = REPOSITORY_ROOT / Path(entry.path)
        if local_path.exists() or local_path.is_symlink():
            existing_entries.append(entry)
        else:
            source_digest.update(b"DELETE\0")
            source_digest.update(entry.path.encode("utf-8", errors="surrogateescape"))
            source_digest.update(b"\0")

    member_count = 0
    with tarfile.open(archive_path, "w:gz", format=tarfile.PAX_FORMAT) as archive:
        member_count += add_directory_members(
            archive, (entry.path for entry in existing_entries)
        )
        for entry in existing_entries:
            local_path = REPOSITORY_ROOT / Path(entry.path)
            archive_name = f"CLIProxyAPI/{entry.path}"
            source_digest.update(entry.path.encode("utf-8", errors="surrogateescape"))
            source_digest.update(b"\0")
            source_digest.update(entry.mode.encode("ascii"))
            source_digest.update(b"\0")

            if entry.mode == "120000":
                target = local_path.read_text(encoding="utf-8", errors="surrogateescape")
                info = tarfile.TarInfo(archive_name)
                info.type = tarfile.SYMTYPE
                info.linkname = target
                info.mode = 0o777
                info.uid = 0
                info.gid = 0
                info.uname = "root"
                info.gname = "root"
                info.mtime = 0
                archive.addfile(info)
                source_digest.update(target.encode("utf-8", errors="surrogateescape"))
                member_count += 1
                continue

            data = local_path.read_bytes()
            if is_text_path(entry.path):
                data = normalize_lf(data)
                normalized_text_count += 1
                if b"\r" in data:
                    raise DeploymentError(
                        f"CR byte remains after normalization: {entry.path}"
                    )
            source_digest.update(data)
            source_digest.update(b"\0")

            info = tarfile.TarInfo(archive_name)
            info.size = len(data)
            info.mode = 0o755 if entry.mode == "100755" else 0o644
            info.uid = 0
            info.gid = 0
            info.uname = "root"
            info.gname = "root"
            info.mtime = 0
            archive.addfile(info, io.BytesIO(data))
            member_count += 1

    with tarfile.open(archive_path, "r:gz") as archive:
        actual_members = archive.getmembers()
        if len(actual_members) != member_count:
            raise DeploymentError(
                f"archive member count mismatch: {len(actual_members)} != {member_count}"
            )
        for member in actual_members:
            relative = member.name.removeprefix("CLIProxyAPI/")
            if not member.isfile() or not is_text_path(relative):
                continue
            handle = archive.extractfile(member)
            if handle is None:
                raise DeploymentError(f"cannot read archive member: {member.name}")
            if b"\r" in handle.read():
                raise DeploymentError(f"CR byte found in archive: {member.name}")

    source_sha256 = source_digest.hexdigest()
    revision = head if not dirty else f"{head}-dirty-{source_sha256[:12]}"
    return SourcePackage(
        archive=archive_path,
        archive_sha256=sha256_file(archive_path),
        source_sha256=source_sha256,
        revision=revision,
        tracked_file_count=len(existing_entries),
        archive_member_count=member_count,
        normalized_text_count=normalized_text_count,
    )


def shell_array(values: Sequence[str]) -> str:
    """Render a safe Bash array body."""

    return "\n".join(f"  {shlex.quote(value)}" for value in values)


REMOTE_SCRIPT_TEMPLATE = r'''#!/usr/bin/env bash
# Remote half of deploy_cpa_full.py.
# It is generated for one immutable source package and one deployment ID.
set -Eeuo pipefail

ID=@@ID@@
VERSION=@@VERSION@@
REVISION=@@REVISION@@
SOURCE_SHA256=@@SOURCE_SHA256@@
SERVICE=@@SERVICE@@
APP=@@APP@@
AUTH_DIR=@@AUTH_DIR@@
PORT=@@SERVICE_PORT@@
GO_BIN=@@GO_BIN@@
BASE=@@BASE@@
ARCHIVE="$BASE/source.tar.gz"
SOURCE_ROOT="$BASE/src/CLIProxyAPI"
BUILD_ROOT="$BASE/build"
MAIN_BUILD="$BUILD_ROOT/cli-proxy-api"
PLUGIN_DIR="$APP/plugins/linux/arm64"
LOG="$BASE/deploy.log"
JOURNAL_FILE="$BASE/journal.txt"
PLUGIN_MANIFEST_BEFORE="$BASE/plugins-before.sha256"
MANAGED_MANIFEST_BUILD="$BASE/plugins-build.sha256"
BACKUP_DIR="$APP/backups"
BACKUP="$BACKUP_DIR/$ID.tar.gz"
DEPLOY_LOG_DIR="$APP/deploy-logs"
DEPLOY_STARTED=0
BACKUP_READY=0
STEP_NUMBER=0

MANAGED_PLUGINS=(
@@MANAGED_PLUGINS@@
)
EXPECTED_PLUGIN_IDS=(
@@EXPECTED_PLUGIN_IDS@@
)

mkdir -p "$BASE" "$BUILD_ROOT"
exec > >(tee -a "$LOG") 2>&1

step() {
  STEP_NUMBER=$((STEP_NUMBER + 1))
  printf '\n%s\n' '==============================================================================='
  printf '%s STEP %02d: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$STEP_NUMBER" "$1"
  printf '%s\n' '==============================================================================='
}

log() {
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

first_line() {
  python3 -c 'import sys; lines=sys.stdin.read().splitlines(); print(lines[0] if lines else "")'
}

auth_inventory() {
  python3 - "$AUTH_DIR" <<'PY'
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
files = sorted(root.glob("*.json"))
invalid = 0
for path in files:
    try:
        with path.open("r", encoding="utf-8") as handle:
            json.load(handle)
    except Exception:
        invalid += 1
print(f"AUTH_COUNT={len(files)}")
print(f"AUTH_INVALID={invalid}")
raise SystemExit(1 if invalid else 0)
PY
}

wait_ready() {
  local attempts="$1"
  local active root_http healthz_http management_http management_ok
  for attempt in $(seq 1 "$attempts"); do
    active="$(systemctl is-active "$SERVICE" 2>/dev/null || true)"
    root_http="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:${PORT}/" 2>/dev/null || true)"
    healthz_http="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:${PORT}/healthz" 2>/dev/null || true)"
    management_http="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:${PORT}/v0/management/config" 2>/dev/null || true)"
    management_ok=0
    case "$management_http" in
      200|401|403) management_ok=1 ;;
    esac
    if [ "$active" = active ] && [ "$root_http" = 200 ] && [ "$healthz_http" = 200 ] && [ "$management_ok" -eq 1 ]; then
      log "READY attempt=$attempt active=$active root_http=$root_http healthz_http=$healthz_http management_http=$management_http"
      return 0
    fi
    if [ $((attempt % 10)) -eq 0 ]; then
      log "WAIT attempt=$attempt active=$active root_http=$root_http healthz_http=$healthz_http management_http=$management_http"
    fi
    sleep 1
  done
  return 1
}

rollback_on_error() {
  local rc="$1"
  trap - ERR
  set +e
  step "ROLLBACK AFTER FAILURE"
  log "DEPLOY_ERROR_STATUS=$rc"
  if [ "$DEPLOY_STARTED" -eq 1 ]; then
    sudo systemctl stop "$SERVICE" >/dev/null 2>&1 || true
    if [ "$BACKUP_READY" -eq 1 ] && sudo test -s "$BACKUP"; then
      sudo rm -f "$APP/cli-proxy-api" "$APP/config.yaml"
      sudo rm -rf "$APP/plugins" "$APP/plugin-data" "$AUTH_DIR"
      sudo tar -xzpf "$BACKUP" -C /
      log "ROLLBACK_FILES=restored"
    else
      log "ROLLBACK_FILES=not-installed"
    fi
    sudo systemctl start "$SERVICE" >/dev/null 2>&1 || true
    if wait_ready 90 >/dev/null 2>&1; then
      log "ROLLBACK_READY=1"
    else
      log "ROLLBACK_READY=0"
    fi
  fi
  log "DEPLOY_RESULT=rolled-back"
  sudo mkdir -p "$DEPLOY_LOG_DIR" >/dev/null 2>&1 || true
  sudo cp "$LOG" "$DEPLOY_LOG_DIR/$ID.log" >/dev/null 2>&1 || true
  if [ -s "$JOURNAL_FILE" ]; then
    sudo cp "$JOURNAL_FILE" "$DEPLOY_LOG_DIR/$ID-journal.log" >/dev/null 2>&1 || true
  fi
  exit "$rc"
}
trap 'rollback_on_error $?' ERR

step "IDENTITY AND SOURCE PREFLIGHT"
log "DEPLOY_ID=$ID"
log "VERSION=$VERSION"
log "REVISION=$REVISION"
log "STARTED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
log "HOST=$(hostname)"
log "ARCH=$(uname -m)"
log "SERVICE_BEFORE=$(systemctl is-active "$SERVICE")"
log "PID_BEFORE=$(systemctl show -p MainPID --value "$SERVICE")"
log "START_BEFORE=$(systemctl show -p ExecMainStartTimestamp --value "$SERVICE")"
log "MAIN_BEFORE_VERSION=$("$APP/cli-proxy-api" --help 2>&1 | first_line)"
df -h / /home /opt

if [ "$(systemctl is-active "$SERVICE")" != active ]; then
  log "REFUSE service is not active before deployment"
  false
fi
if [ ! -s "$ARCHIVE" ]; then
  log "REFUSE source archive is missing"
  false
fi
ACTUAL_SOURCE_SHA256="$(sha256sum "$ARCHIVE" | cut -d' ' -f1)"
log "SOURCE_SHA256_EXPECTED=$SOURCE_SHA256"
log "SOURCE_SHA256_ACTUAL=$ACTUAL_SOURCE_SHA256"
if [ "$ACTUAL_SOURCE_SHA256" != "$SOURCE_SHA256" ]; then
  log "REFUSE source checksum mismatch"
  false
fi

step "LIVE STATE SNAPSHOT"
MAIN_BEFORE_SHA256="$(sha256sum "$APP/cli-proxy-api" | cut -d' ' -f1)"
CONFIG_BEFORE_SHA256="$(sha256sum "$APP/config.yaml" | cut -d' ' -f1)"
auth_inventory | tee "$BASE/auth-before.txt"
AUTH_COUNT_BEFORE="$(python3 -c 'from pathlib import Path; import sys; print(sum(1 for _ in Path(sys.argv[1]).glob("*.json")))' "$AUTH_DIR")"
sha256sum "$PLUGIN_DIR"/*.so | sort -k2 > "$PLUGIN_MANIFEST_BEFORE"
PLUGIN_COUNT_BEFORE="$(wc -l < "$PLUGIN_MANIFEST_BEFORE")"
log "MAIN_BEFORE_SHA256=$MAIN_BEFORE_SHA256"
log "CONFIG_BEFORE_SHA256=$CONFIG_BEFORE_SHA256"
log "AUTH_COUNT_BEFORE=$AUTH_COUNT_BEFORE"
log "PLUGIN_COUNT_BEFORE=$PLUGIN_COUNT_BEFORE"
log "PLUGIN_MANIFEST_BEGIN"
cat "$PLUGIN_MANIFEST_BEFORE"
log "PLUGIN_MANIFEST_END"
for plugin in "${MANAGED_PLUGINS[@]}"; do
  test -s "$PLUGIN_DIR/$plugin.so"
done

step "EXTRACT AND VERIFY LF SOURCE"
rm -rf "$BASE/src" "$BUILD_ROOT"
mkdir -p "$BASE/src" "$BUILD_ROOT"
tar -xzf "$ARCHIVE" -C "$BASE/src"
python3 - "$SOURCE_ROOT" <<'PY'
from pathlib import Path
import sys

root = Path(sys.argv[1])
suffixes = {
    ".c", ".cc", ".conf", ".cpp", ".css", ".go", ".h", ".html",
    ".ini", ".java", ".js", ".json", ".jsx", ".md", ".mod", ".proto",
    ".py", ".rs", ".sh", ".sql", ".sum", ".toml", ".ts", ".tsx",
    ".txt", ".xml", ".yaml", ".yml",
}
names = {".dockerignore", ".gitattributes", ".gitignore", "Dockerfile", "LICENSE", "Makefile"}
files = [
    path for path in root.rglob("*")
    if path.is_file() and (path.suffix.lower() in suffixes or path.name in names)
]
bad = [str(path.relative_to(root)) for path in files if b"\r" in path.read_bytes()]
print(f"LF_VERIFIED_FILES={len(files)}")
print(f"LF_BAD_FILES={len(bad)}")
if bad:
    print("LF_BAD_BEGIN")
    print("\n".join(bad[:50]))
    print("LF_BAD_END")
raise SystemExit(1 if bad else 0)
PY

step "VERIFY REQUIRED SOURCE INVARIANTS"
python3 - "$SOURCE_ROOT" <<'PY'
from pathlib import Path
import sys

root = Path(sys.argv[1])
heartbeat = (root / "sdk/api/handlers/handlers_stream_completion.go").read_text(encoding="utf-8")
xai_stream = (root / "internal/runtime/executor/xai_executor_stream.go").read_text(encoding="utf-8")
heartbeat_compact = "\n".join(line.strip() for line in heartbeat.splitlines())
checks = {
    "VIRTUAL_HEARTBEAT_10S": "realtimeGuardVirtualHeartbeatInterval = 10 * time.Second",
    "VIRTUAL_RESPONSE_CREATED": '[]string{"response.created", "response.in_progress"}',
    "XAI_INCOMPLETE_STREAM_GUARD": "xAI stream disconnected before response.completed",
}
for name, marker in checks.items():
    present = marker in heartbeat
    print(f"SOURCE_INVARIANT[{name}]={int(present)}")
    if not present:
        raise SystemExit(f"required source invariant is missing: {name}")
stop_before_flush = "virtualHeartbeat.stopAndWait()\nfor _, payload := range attempt.Payloads {" in heartbeat_compact
print(f"SOURCE_INVARIANT[HEARTBEAT_STOP_BEFORE_FLUSH]={int(stop_before_flush)}")
if not stop_before_flush:
    raise SystemExit("required source invariant is missing: HEARTBEAT_STOP_BEFORE_FLUSH")
done_tracking = 'bytes.Equal(eventData, []byte("[DONE]"))' in xai_stream and "completionState.Completed = true" in xai_stream
print(f"SOURCE_INVARIANT[XAI_DONE_TRACKING]={int(done_tracking)}")
if not done_tracking:
    raise SystemExit("required source invariant is missing: XAI_DONE_TRACKING")
PY

step "BUILD CGO MAIN BINARY"
export PATH="$(dirname "$GO_BIN"):$PATH"
export CGO_ENABLED=1
export GOOS=linux
export GOARCH=arm64
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X main.Version=$VERSION -X main.Commit=$REVISION -X main.BuildDate=$BUILD_DATE"
log "GO_VERSION=$("$GO_BIN" version)"
log "CGO_ENABLED=$("$GO_BIN" env CGO_ENABLED)"
log "CC=$("$GO_BIN" env CC)"
log "BUILD_DATE=$BUILD_DATE"
cd "$SOURCE_ROOT"
"$GO_BIN" build -buildvcs=false -trimpath -ldflags "$LDFLAGS" -o "$MAIN_BUILD" ./cmd/server
MAIN_BUILD_VERSION="$("$MAIN_BUILD" --help 2>&1 | first_line)"
MAIN_BUILD_FILE="$(file "$MAIN_BUILD")"
MAIN_BUILD_META="$("$GO_BIN" version -m "$MAIN_BUILD" 2>&1)"
MAIN_BUILD_SHA256="$(sha256sum "$MAIN_BUILD" | cut -d' ' -f1)"
log "MAIN_BUILD_VERSION=$MAIN_BUILD_VERSION"
log "MAIN_BUILD_FILE=$MAIN_BUILD_FILE"
log "MAIN_BUILD_SHA256=$MAIN_BUILD_SHA256"
printf '%s\n' "$MAIN_BUILD_META"
case "$MAIN_BUILD_VERSION" in
  *"CLIProxyAPI Version: $VERSION, Commit: $REVISION, BuiltAt: $BUILD_DATE"*) ;;
  *) log "main build version verification failed"; false ;;
esac
case "$MAIN_BUILD_FILE" in
  *'ARM aarch64'*'dynamically linked'*) ;;
  *) log "main build architecture/linkage verification failed"; false ;;
esac
case "$MAIN_BUILD_META" in
  *'CGO_ENABLED=1'*) ;;
  *) log "main build metadata lacks CGO_ENABLED=1"; false ;;
esac
for marker in 'response.created' 'response.in_progress'; do
  if ! grep -aFq "$marker" "$MAIN_BUILD"; then
    log "main binary marker missing: $marker"
    false
  fi
  log "MAIN_BINARY_MARKER[$marker]=1"
done

step "BUILD ALL SIX REPOSITORY PLUGINS"
: > "$MANAGED_MANIFEST_BUILD"
for plugin in "${MANAGED_PLUGINS[@]}"; do
  plugin_source="$SOURCE_ROOT/plugins/src/$plugin"
  plugin_output="$BUILD_ROOT/$plugin.so"
  log "PLUGIN_BUILD_START name=$plugin source=$plugin_source output=$plugin_output"
  test -s "$plugin_source/go.mod"
  cd "$plugin_source"
  "$GO_BIN" build -buildvcs=false -buildmode=c-shared -trimpath=true -o "$plugin_output" .
  rm -f "$BUILD_ROOT/$plugin.h"
  plugin_file="$(file "$plugin_output")"
  plugin_meta="$("$GO_BIN" version -m "$plugin_output" 2>&1)"
  plugin_sha="$(sha256sum "$plugin_output" | cut -d' ' -f1)"
  log "PLUGIN_BUILD_FILE[$plugin]=$plugin_file"
  log "PLUGIN_BUILD_SHA256[$plugin]=$plugin_sha"
  printf '%s\n' "$plugin_meta"
  case "$plugin_file" in
    *'shared object'*'ARM aarch64'*) ;;
    *) log "plugin architecture/buildmode verification failed: $plugin"; false ;;
  esac
  case "$plugin_meta" in
    *'-buildmode=c-shared'*'CGO_ENABLED=1'*) ;;
    *) log "plugin metadata verification failed: $plugin"; false ;;
  esac
  printf '%s  %s\n' "$plugin_sha" "$plugin_output" >> "$MANAGED_MANIFEST_BUILD"
done
log "BUILD_RESULT=success"

step "RECHECK LIVE STATE BEFORE STOP"
if [ "$(systemctl is-active "$SERVICE")" != active ]; then
  log "REFUSE service changed state during build"
  false
fi
if [ "$(sha256sum "$APP/cli-proxy-api" | cut -d' ' -f1)" != "$MAIN_BEFORE_SHA256" ]; then
  log "REFUSE live main binary changed during build"
  false
fi
if [ "$(sha256sum "$APP/config.yaml" | cut -d' ' -f1)" != "$CONFIG_BEFORE_SHA256" ]; then
  log "REFUSE live config changed during build"
  false
fi
if [ "$(python3 -c 'from pathlib import Path; import sys; print(sum(1 for _ in Path(sys.argv[1]).glob("*.json")))' "$AUTH_DIR")" != "$AUTH_COUNT_BEFORE" ]; then
  log "REFUSE auth inventory changed during build"
  false
fi
sha256sum "$PLUGIN_DIR"/*.so | sort -k2 > "$BASE/plugins-before-stop.sha256"
if ! cmp -s "$PLUGIN_MANIFEST_BEFORE" "$BASE/plugins-before-stop.sha256"; then
  log "REFUSE live plugin files changed during build"
  false
fi
log "LIVE_RECHECK=unchanged"

step "STOP SERVICE AND CREATE FULL ROLLBACK BACKUP"
DEPLOY_SERVICE_START="$(date -u '+%Y-%m-%d %H:%M:%S')"
DEPLOY_STARTED=1
sudo systemctl stop "$SERVICE"
sudo mkdir -p "$BACKUP_DIR"
cd /
sudo tar -czpf "$BACKUP" \
  opt/cli-proxy-api/cli-proxy-api \
  opt/cli-proxy-api/config.yaml \
  opt/cli-proxy-api/plugins \
  opt/cli-proxy-api/plugin-data \
  home/ubuntu/.cli-proxy-api
sudo test -s "$BACKUP"
sudo chmod 600 "$BACKUP"
sudo gzip -t "$BACKUP"
log "BACKUP=$BACKUP"
log "BACKUP_SIZE=$(sudo stat -c %s "$BACKUP")"
log "BACKUP_SHA256=$(sudo sha256sum "$BACKUP" | cut -d' ' -f1)"

step "VALIDATE ROLLBACK BACKUP CONTENT"
sudo python3 - "$BACKUP" "$AUTH_COUNT_BEFORE" "$PLUGIN_COUNT_BEFORE" "$MAIN_BEFORE_SHA256" "$CONFIG_BEFORE_SHA256" <<'PY'
import hashlib
import json
import sys
import tarfile

backup = sys.argv[1]
expected_auth_count = int(sys.argv[2])
expected_plugin_count = int(sys.argv[3])
expected_main_sha256 = sys.argv[4]
expected_config_sha256 = sys.argv[5]
required = {
    "opt/cli-proxy-api/cli-proxy-api",
    "opt/cli-proxy-api/config.yaml",
}
with tarfile.open(backup, "r:gz") as archive:
    files = [member for member in archive.getmembers() if member.isfile()]
    by_name = {member.name.lstrip("./"): member for member in files}
    names = set(by_name)
    missing = sorted(required - names)
    plugin_members = [
        member for member in files
        if "/plugins/" in f"/{member.name}" and member.name.endswith(".so")
    ]
    auth_members = [
        member for member in files
        if member.name.lstrip("./").startswith("home/ubuntu/.cli-proxy-api/")
        and member.name.endswith(".json")
    ]
    invalid = 0
    for member in auth_members:
        handle = archive.extractfile(member)
        try:
            json.load(handle)
        except Exception:
            invalid += 1
    main_sha256 = ""
    config_sha256 = ""
    if not missing:
        main_sha256 = hashlib.sha256(archive.extractfile(by_name["opt/cli-proxy-api/cli-proxy-api"]).read()).hexdigest()
        config_sha256 = hashlib.sha256(archive.extractfile(by_name["opt/cli-proxy-api/config.yaml"]).read()).hexdigest()
    print(f"BACKUP_FILES={len(files)}")
    print(f"BACKUP_PLUGIN_COUNT={len(plugin_members)}")
    print(f"BACKUP_AUTH_COUNT={len(auth_members)}")
    print(f"BACKUP_AUTH_INVALID={invalid}")
    print(f"BACKUP_MISSING_REQUIRED={len(missing)}")
    print(f"BACKUP_MAIN_SHA256={main_sha256}")
    print(f"BACKUP_CONFIG_SHA256={config_sha256}")
    print(f"BACKUP_MAIN_MATCH={int(main_sha256 == expected_main_sha256)}")
    print(f"BACKUP_CONFIG_MATCH={int(config_sha256 == expected_config_sha256)}")
    if missing:
        print("BACKUP_MISSING_BEGIN")
        print("\n".join(missing))
        print("BACKUP_MISSING_END")
    if (
        missing
        or invalid
        or len(auth_members) != expected_auth_count
        or len(plugin_members) != expected_plugin_count
        or main_sha256 != expected_main_sha256
        or config_sha256 != expected_config_sha256
    ):
        raise SystemExit(1)
PY
BACKUP_READY=1

step "INSTALL MAIN BINARY AND SIX PLUGINS ATOMICALLY"
sudo install -m 0755 "$MAIN_BUILD" "$APP/cli-proxy-api.new.$ID"
sudo mv -f "$APP/cli-proxy-api.new.$ID" "$APP/cli-proxy-api"
for plugin in "${MANAGED_PLUGINS[@]}"; do
  source_file="$BUILD_ROOT/$plugin.so"
  live_file="$PLUGIN_DIR/$plugin.so"
  sudo install -m 0755 "$source_file" "$live_file.new.$ID"
  sudo mv -f "$live_file.new.$ID" "$live_file"
  log "INSTALLED_PLUGIN=$plugin"
done

step "START SERVICE AND POLL HEALTH FOR UP TO 90 SECONDS"
sudo systemctl start "$SERVICE"
if ! wait_ready 90; then
  log "health check timed out"
  false
fi

step "VERIFY INSTALLED ARTIFACTS AND DATA PRESERVATION"
MAIN_AFTER_VERSION="$("$APP/cli-proxy-api" --help 2>&1 | first_line)"
MAIN_AFTER_SHA256="$(sha256sum "$APP/cli-proxy-api" | cut -d' ' -f1)"
MAIN_AFTER_META="$("$GO_BIN" version -m "$APP/cli-proxy-api" 2>&1)"
CONFIG_AFTER_SHA256="$(sha256sum "$APP/config.yaml" | cut -d' ' -f1)"
AUTH_COUNT_AFTER="$(python3 -c 'from pathlib import Path; import sys; print(sum(1 for _ in Path(sys.argv[1]).glob("*.json")))' "$AUTH_DIR")"
log "MAIN_AFTER_VERSION=$MAIN_AFTER_VERSION"
log "MAIN_AFTER_SHA256=$MAIN_AFTER_SHA256"
log "CONFIG_AFTER_SHA256=$CONFIG_AFTER_SHA256"
log "AUTH_COUNT_AFTER=$AUTH_COUNT_AFTER"
case "$MAIN_AFTER_VERSION" in
  *"CLIProxyAPI Version: $VERSION, Commit: $REVISION, BuiltAt: $BUILD_DATE"*) ;;
  *) log "installed main version mismatch"; false ;;
esac
if [ "$MAIN_AFTER_SHA256" != "$MAIN_BUILD_SHA256" ]; then
  log "installed main checksum mismatch"
  false
fi
if [ "$CONFIG_AFTER_SHA256" != "$CONFIG_BEFORE_SHA256" ]; then
  log "config changed during deployment"
  false
fi
if [ "$AUTH_COUNT_AFTER" != "$AUTH_COUNT_BEFORE" ]; then
  log "auth count changed during deployment"
  false
fi
case "$MAIN_AFTER_META" in
  *'CGO_ENABLED=1'*) ;;
  *) log "installed main lacks CGO_ENABLED=1"; false ;;
esac
auth_inventory | tee "$BASE/auth-after.txt"

while read -r expected_sha built_path; do
  plugin_name="$(basename "$built_path")"
  live_sha="$(sha256sum "$PLUGIN_DIR/$plugin_name" | cut -d' ' -f1)"
  if [ "$live_sha" != "$expected_sha" ]; then
    log "installed managed plugin checksum mismatch: $plugin_name"
    false
  fi
  live_meta="$("$GO_BIN" version -m "$PLUGIN_DIR/$plugin_name" 2>&1)"
  case "$live_meta" in
    *'-buildmode=c-shared'*'CGO_ENABLED=1'*) ;;
    *) log "installed managed plugin metadata mismatch: $plugin_name"; false ;;
  esac
  log "MANAGED_PLUGIN_VERIFIED[$plugin_name]=$live_sha"
done < "$MANAGED_MANIFEST_BUILD"

while read -r old_hash old_path; do
  old_name="$(basename "$old_path")"
  managed=0
  for plugin in "${MANAGED_PLUGINS[@]}"; do
    if [ "$old_name" = "$plugin.so" ]; then
      managed=1
      break
    fi
  done
  if [ "$managed" -eq 1 ]; then
    continue
  fi
  current_hash="$(sha256sum "$PLUGIN_DIR/$old_name" | cut -d' ' -f1)"
  if [ "$current_hash" != "$old_hash" ]; then
    log "non-target plugin changed: $old_name"
    false
  fi
  log "NON_TARGET_PLUGIN_UNCHANGED[$old_name]=1"
done < "$PLUGIN_MANIFEST_BEFORE"

PLUGIN_COUNT_AFTER="$(python3 -c 'from pathlib import Path; import sys; print(sum(1 for _ in Path(sys.argv[1]).glob("*.so")))' "$PLUGIN_DIR")"
log "PLUGIN_COUNT_AFTER=$PLUGIN_COUNT_AFTER"
if [ "$PLUGIN_COUNT_AFTER" != "$PLUGIN_COUNT_BEFORE" ]; then
  log "plugin count changed during deployment"
  false
fi

step "VERIFY MANAGEMENT HEADERS"
HEADER_TEXT="$(curl -sS -D - -o /dev/null --max-time 5 "http://127.0.0.1:${PORT}/v0/management/config" | tr -d '\r' | tr '[:upper:]' '[:lower:]')"
case "$HEADER_TEXT" in
  *'x-cpa-support-plugin: 1'*) log "PLUGIN_SUPPORT_HEADER=1" ;;
  *) log "plugin support header missing"; false ;;
esac
case "$HEADER_TEXT" in
  *"x-cpa-version: $VERSION"*) log "API_VERSION_HEADER=$VERSION" ;;
  *) log "API version header mismatch"; false ;;
esac
case "$HEADER_TEXT" in
  *"x-cpa-commit: ${REVISION,,}"*) log "API_COMMIT_HEADER=$REVISION" ;;
  *) log "API commit header mismatch"; false ;;
esac

step "VERIFY SERVICE JOURNAL AND ALL EIGHT PLUGIN REGISTRATIONS"
journalctl -u "$SERVICE" --since "$DEPLOY_SERVICE_START" --no-pager -o short-iso > "$JOURNAL_FILE"
JOURNAL_TEXT="$(<"$JOURNAL_FILE")"
case "$JOURNAL_TEXT" in
  *'failed to load plugin'*) log "journal contains plugin load failure"; false ;;
esac
case "${JOURNAL_TEXT,,}" in
  *'panic'*) log "journal contains panic"; false ;;
esac
for plugin_id in "${EXPECTED_PLUGIN_IDS[@]}"; do
  case "$JOURNAL_TEXT" in
    *"plugin registered plugin_id=$plugin_id"*) log "PLUGIN_REGISTERED[$plugin_id]=1" ;;
    *) log "plugin registration missing: $plugin_id"; false ;;
  esac
done
case "$JOURNAL_TEXT" in
  *"CLIProxyAPI Version: $VERSION, Commit: $REVISION"*) ;;
  *) log "journal version marker missing"; false ;;
esac
case "$JOURNAL_TEXT" in
  *"API server started successfully on: 0.0.0.0:$PORT"*) ;;
  *) log "journal API startup marker missing"; false ;;
esac

step "FINAL RESULT AND EVIDENCE COPY"
trap - ERR
DEPLOY_STARTED=0
log "DEPLOY_RESULT=success"
log "SERVICE_AFTER=$(systemctl is-active "$SERVICE")"
log "PID_AFTER=$(systemctl show -p MainPID --value "$SERVICE")"
log "START_TIMESTAMP=$(systemctl show -p ExecMainStartTimestamp --value "$SERVICE")"
log "FINISHED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
sudo mkdir -p "$DEPLOY_LOG_DIR"
sudo cp "$LOG" "$DEPLOY_LOG_DIR/$ID.log"
sudo cp "$JOURNAL_FILE" "$DEPLOY_LOG_DIR/$ID-journal.log"

step "CLEAN GO BUILD CACHE AND OLD ROLLBACK ARCHIVES"
set +e
export PATH="$(dirname "$GO_BIN"):$PATH"
"$GO_BIN" clean -cache -modcache
rm -rf "$HOME/.cache/go-build" "$HOME/go/pkg"
if sudo test -s "$BACKUP"; then
  sudo find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 ! -path "$BACKUP" -exec rm -rf {} +
  log "ROLLBACK_KEPT=$BACKUP"
  log "ROLLBACK_KEPT_SIZE=$(sudo stat -c %s "$BACKUP")"
  log "BACKUP_DIR_REMAINING_BEGIN"
  sudo find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -print
  log "BACKUP_DIR_REMAINING_END"
else
  log "ROLLBACK_KEEP_SKIPPED backup missing"
fi
log "GOCACHE_EXISTS=$([ -e "$HOME/.cache/go-build" ] && echo 1 || echo 0)"
log "GOMODCACHE_EXISTS=$([ -e "$HOME/go/pkg" ] && echo 1 || echo 0)"
set -e
'''


def render_remote_script(
    config: DeploymentConfig,
    paths: DeploymentPaths,
    source: SourcePackage,
) -> str:
    """Generate the immutable remote deployment script with safely quoted values."""

    values = {
        "@@ID@@": shlex.quote(paths.deployment_id),
        "@@VERSION@@": shlex.quote(paths.version),
        "@@REVISION@@": shlex.quote(source.revision),
        "@@SOURCE_SHA256@@": shlex.quote(source.archive_sha256),
        "@@SERVICE@@": shlex.quote(REMOTE_SERVICE),
        "@@APP@@": shlex.quote(REMOTE_APP),
        "@@AUTH_DIR@@": shlex.quote(REMOTE_AUTH_DIR),
        "@@SERVICE_PORT@@": shlex.quote(str(config.service_port)),
        "@@GO_BIN@@": shlex.quote(REMOTE_GO),
        "@@BASE@@": shlex.quote(paths.remote_base),
        "@@MANAGED_PLUGINS@@": shell_array(MANAGED_PLUGINS),
        "@@EXPECTED_PLUGIN_IDS@@": shell_array(EXPECTED_PLUGIN_IDS),
    }
    rendered = REMOTE_SCRIPT_TEMPLATE
    for placeholder, value in values.items():
        rendered = rendered.replace(placeholder, value)
    unresolved = sorted(set(re.findall(r"@@[A-Z0-9_]+@@", rendered)))
    if unresolved:
        raise DeploymentError(
            "unresolved remote script placeholders: " + ", ".join(unresolved)
        )
    return rendered


def upload_stage(
    config: DeploymentConfig,
    paths: DeploymentPaths,
    source: SourcePackage,
    remote_script_path: Path,
) -> None:
    """Create an isolated remote stage and upload the verified inputs."""

    remote_target = f"{config.user}@{config.host}"
    run_logged(
        [
            *ssh_base(config),
            f"rm -rf {shlex.quote(paths.remote_base)} && "
            f"mkdir -p {shlex.quote(paths.remote_base)}",
        ]
    )
    run_logged(
        [
            *scp_base(config),
            str(source.archive),
            f"{remote_target}:{paths.remote_archive}",
        ]
    )
    run_logged(
        [
            *scp_base(config),
            str(remote_script_path),
            f"{remote_target}:{paths.remote_script}",
        ]
    )
    run_logged(
        [
            *ssh_base(config),
            " && ".join(
                (
                    f"chmod 700 {shlex.quote(paths.remote_script)}",
                    f"bash -n {shlex.quote(paths.remote_script)}",
                    f"sha256sum {shlex.quote(paths.remote_archive)} {shlex.quote(paths.remote_script)}",
                    f"stat -c '%n|%s|%y' {shlex.quote(paths.remote_archive)} {shlex.quote(paths.remote_script)}",
                )
            ),
        ]
    )


def download_remote_evidence(
    config: DeploymentConfig,
    paths: DeploymentPaths,
) -> None:
    """Download remote evidence when present, including after rollback failures."""

    remote_target = f"{config.user}@{config.host}"
    evidence = (
        (f"{paths.remote_base}/deploy.log", paths.remote_log),
        (f"{paths.remote_base}/journal.txt", paths.journal_log),
    )
    for remote_path, local_path in evidence:
        probe = run_logged(
            [*ssh_base(config), f"test -s {shlex.quote(remote_path)}"],
            check=False,
            capture=True,
        )
        if probe.returncode != 0:
            LOGGER.warning("REMOTE_EVIDENCE_MISSING path=%s", remote_path)
            continue
        run_logged(
            [
                *scp_base(config),
                f"{remote_target}:{remote_path}",
                str(local_path),
            ]
        )
        LOGGER.info(
            "REMOTE_EVIDENCE_SAVED path=%s size=%d sha256=%s",
            local_path,
            local_path.stat().st_size,
            sha256_file(local_path),
        )


def cleanup_remote_stage(
    config: DeploymentConfig,
    paths: DeploymentPaths,
) -> None:
    """Remove transient source/build files while retaining remote evidence."""

    command = " && ".join(
        (
            f"rm -rf {shlex.quote(paths.remote_base + '/src')}",
            f"rm -rf {shlex.quote(paths.remote_base + '/build')}",
            f"rm -f {shlex.quote(paths.remote_archive)}",
            f"test ! -e {shlex.quote(paths.remote_base + '/src')}",
            f"test ! -e {shlex.quote(paths.remote_base + '/build')}",
            f"test ! -e {shlex.quote(paths.remote_archive)}",
        )
    )
    run_logged([*ssh_base(config), command], check=False)


def cleanup_go_build_cache(config: DeploymentConfig) -> None:
    """Remove Go build and module caches left by the remote CGO compile."""

    go_bin = shlex.quote(REMOTE_GO)
    go_path = shlex.quote(str(PurePosixPath(REMOTE_GO).parent))
    command = (
        f"export PATH={go_path}:$PATH; "
        f"{go_bin} clean -cache -modcache; "
        "rm -rf /home/ubuntu/.cache/go-build /home/ubuntu/go/pkg; "
        "printf 'GOCACHE_EXISTS=%s\\n' "
        '"$([ -e /home/ubuntu/.cache/go-build ] && echo 1 || echo 0)"; '
        "printf 'GOMODCACHE_EXISTS=%s\\n' "
        '"$([ -e /home/ubuntu/go/pkg ] && echo 1 || echo 0)"'
    )
    run_logged([*ssh_base(config), command], check=False)


def prune_old_rollback_archives(config: DeploymentConfig, keep_archive: str) -> None:
    """Delete every backup except the rollback archive created by this run."""

    backup_dir = f"{REMOTE_APP}/backups"
    quoted_dir = shlex.quote(backup_dir)
    quoted_keep = shlex.quote(keep_archive)
    command = (
        f"if sudo test -s {quoted_keep}; then "
        f"sudo find {quoted_dir} -mindepth 1 -maxdepth 1 "
        f"! -path {quoted_keep} -exec rm -rf {{}} +; "
        f"printf 'ROLLBACK_KEPT=%s\\n' {quoted_keep}; "
        f"sudo stat -c 'ROLLBACK_KEPT_SIZE=%s' {quoted_keep}; "
        f"sudo find {quoted_dir} -mindepth 1 -maxdepth 1 -print; "
        "else "
        "printf 'ROLLBACK_KEEP_SKIPPED=1\\n'; "
        "fi"
    )
    run_logged([*ssh_base(config), command], check=False)


def main() -> int:
    """Run the complete guarded deployment workflow."""

    timestamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%d%H%M%S")
    master_log = SCRIPT_DIR / f"cpa-full-auto-{timestamp}.log"
    configure_logging(master_log)
    LOGGER.info("MASTER_LOG=%s", master_log)
    config = DeploymentConfig()
    paths: DeploymentPaths | None = None

    try:
        step("LOCAL SAFETY PREFLIGHT")
        if len(sys.argv) != 1:
            raise DeploymentError(
                "this script accepts no arguments; run: python deploy/deploy_cpa_full.py"
            )
        git_path = require_command("git")
        for command in ("ssh", "scp"):
            require_command(command)
        bash_path = resolve_git_bash(git_path)
        if not config.ssh_key.is_file():
            raise DeploymentError(f"SSH key does not exist: {config.ssh_key}")
        if not 1 <= config.ssh_port <= 65535:
            raise DeploymentError("SSH port must be between 1 and 65535")
        if not 1 <= config.service_port <= 65535:
            raise DeploymentError("service port must be between 1 and 65535")

        top_level = Path(git_output("rev-parse", "--show-toplevel")).resolve()
        if top_level != REPOSITORY_ROOT.resolve():
            raise DeploymentError(
                f"script repository mismatch: {top_level} != {REPOSITORY_ROOT.resolve()}"
            )
        branch = git_output("branch", "--show-current")
        head = git_output("rev-parse", "HEAD")
        LOGGER.info("REPOSITORY=%s", REPOSITORY_ROOT)
        LOGGER.info("BRANCH=%s", branch)
        LOGGER.info("HEAD=%s", head)

        step("FETCH AND VERIFY LATEST STABLE UPSTREAM RELEASE")
        run_logged(
            ["git", "fetch", "upstream", "--tags", "--prune"],
            cwd=REPOSITORY_ROOT,
        )
        latest_tag = discover_latest_stable_tag()
        ancestor = run_logged(
            ["git", "merge-base", "--is-ancestor", latest_tag, "HEAD"],
            cwd=REPOSITORY_ROOT,
            check=False,
            capture=True,
        )
        if ancestor.returncode not in (0, 1):
            raise DeploymentError(
                "cannot determine whether the latest stable tag is merged: "
                f"git merge-base exited with {ancestor.returncode}"
            )
        tag_merged = ancestor.returncode == 0
        if not tag_merged:
            LOGGER.warning(
                "LATEST_STABLE_TAG_NOT_MERGED tag=%s head=%s; "
                "continuing with current working-tree source",
                latest_tag,
                head,
            )
        upstream_counts = git_output(
            "rev-list", "--left-right", "--count", "HEAD...upstream/main"
        )
        tag_counts = git_output(
            "rev-list", "--left-right", "--count", f"HEAD...{latest_tag}"
        )
        LOGGER.info("LATEST_STABLE_TAG=%s", latest_tag)
        LOGGER.info("LATEST_STABLE_TAG_MERGED=%d", int(tag_merged))
        LOGGER.info("AHEAD_BEHIND_UPSTREAM=%s", upstream_counts)
        LOGGER.info("AHEAD_BEHIND_TAG=%s", tag_counts)

        step("VERIFY WORKING TREE AND SOURCE INVENTORY")
        unmerged = git_output(
            "-c",
            "core.autocrlf=false",
            "diff",
            "--name-only",
            "--diff-filter=U",
        )
        if unmerged:
            raise DeploymentError(f"unmerged paths block deployment:\n{unmerged}")
        untracked = git_output("ls-files", "--others", "--exclude-standard")
        if untracked:
            raise DeploymentError(
                "untracked non-ignored files would be omitted from the package:\n"
                + untracked
            )
        diff_check = run_logged(
            ["git", "diff", "--check"],
            cwd=REPOSITORY_ROOT,
            check=False,
            capture=True,
        )
        if diff_check.returncode != 0:
            raise DeploymentError("git diff --check failed")
        staged_diff_check = run_logged(
            ["git", "diff", "--cached", "--check"],
            cwd=REPOSITORY_ROOT,
            check=False,
            capture=True,
        )
        if staged_diff_check.returncode != 0:
            raise DeploymentError("git diff --cached --check failed")
        tracked_dirty = bool(
            git_output("status", "--porcelain=v1", "--untracked-files=no")
        )
        LOGGER.info("TRACKED_DIRTY=%d", int(tracked_dirty))
        for plugin in MANAGED_PLUGINS:
            module = REPOSITORY_ROOT / "plugins" / "src" / plugin / "go.mod"
            if not module.is_file():
                raise DeploymentError(f"managed plugin module is missing: {module}")
            LOGGER.info("MANAGED_PLUGIN_SOURCE=%s", module.parent)

        step("RESERVE NEXT BUILD VERSION")
        version = reserve_next_version(config, latest_tag)
        paths = deployment_paths(version, timestamp)
        if paths.master_log != master_log:
            raise DeploymentError("internal master log path mismatch")
        LOGGER.info("AUTO_VERSION=%s", paths.version)
        LOGGER.info("DEPLOYMENT_ID=%s", paths.deployment_id)

        with tempfile.TemporaryDirectory(prefix="cpa-full-deploy-") as temporary:
            temp_dir = Path(temporary)

            step("BUILD NORMALIZED TRACKED SOURCE ARCHIVE")
            source = build_source_package(temp_dir, head, tracked_dirty)
            LOGGER.info("SOURCE_ARCHIVE=%s", source.archive)
            LOGGER.info("SOURCE_ARCHIVE_SHA256=%s", source.archive_sha256)
            LOGGER.info("SOURCE_CONTENT_SHA256=%s", source.source_sha256)
            LOGGER.info("SOURCE_REVISION=%s", source.revision)
            LOGGER.info("TRACKED_FILE_COUNT=%d", source.tracked_file_count)
            LOGGER.info("ARCHIVE_MEMBER_COUNT=%d", source.archive_member_count)
            LOGGER.info("NORMALIZED_TEXT_COUNT=%d", source.normalized_text_count)

            step("GENERATE AND VALIDATE REMOTE DEPLOYMENT SCRIPT")
            remote_script = temp_dir / "deploy.sh"
            remote_script.write_text(
                render_remote_script(config, paths, source),
                encoding="utf-8",
                newline="\n",
            )
            remote_bytes = remote_script.read_bytes()
            if b"\r" in remote_bytes:
                raise DeploymentError("generated remote script contains CR bytes")
            run_logged([bash_path, "-n", str(remote_script)])
            LOGGER.info("REMOTE_SCRIPT=%s", remote_script)
            LOGGER.info("REMOTE_SCRIPT_SIZE=%d", remote_script.stat().st_size)
            LOGGER.info("REMOTE_SCRIPT_SHA256=%s", sha256_file(remote_script))

            step("UPLOAD VERIFIED SOURCE AND REMOTE SCRIPT")
            upload_stage(config, paths, source, remote_script)

            step("EXECUTE REMOTE BUILD, BACKUP, DEPLOYMENT, AND VERIFICATION")
            remote_result = run_logged(
                [*ssh_base(config), paths.remote_script],
                check=False,
            )

            step("DOWNLOAD REMOTE EVIDENCE LOGS")
            download_remote_evidence(config, paths)

            step("CLEAN TRANSIENT REMOTE BUILD FILES")
            cleanup_remote_stage(config, paths)

            if remote_result.returncode != 0:
                raise DeploymentError(
                    f"remote deployment failed with exit code {remote_result.returncode}; "
                    "inspect the local master and downloaded remote logs"
                )

            step("CLEAN GO BUILD CACHE AND OLD ROLLBACK ARCHIVES")
            cleanup_go_build_cache(config)
            prune_old_rollback_archives(
                config,
                keep_archive=f"{REMOTE_APP}/backups/{paths.deployment_id}.tar.gz",
            )

        step("FINAL LOCAL RESULT")
        LOGGER.info("DEPLOY_RESULT=success")
        LOGGER.info("VERSION=%s", paths.version)
        LOGGER.info("MASTER_LOG=%s", paths.master_log)
        LOGGER.info("REMOTE_LOG=%s", paths.remote_log)
        LOGGER.info("JOURNAL_LOG=%s", paths.journal_log)
        return 0
    except DeploymentError as exc:
        LOGGER.error("DEPLOY_RESULT=failed error=%s", exc)
        LOGGER.error("MASTER_LOG=%s", master_log)
        return 1
    except KeyboardInterrupt:
        LOGGER.error("DEPLOY_RESULT=interrupted_by_user")
        LOGGER.error("MASTER_LOG=%s", master_log)
        return 130
    except Exception:
        LOGGER.exception("DEPLOY_RESULT=unexpected_failure")
        LOGGER.error("MASTER_LOG=%s", master_log)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
