#!/usr/bin/env python3
"""Inspect or rewrite xAI auth JSON headers after CPA is stopped.

CPA must be stopped before ``apply``. Token refresh and plugin keepalive will
otherwise persist stale in-memory headers back to disk.

Usage from the repository root (SSH to Oracle 01 by default):

    python deploy/update_xai_auth_headers.py inspect
    python deploy/update_xai_auth_headers.py apply --version 1.0.34
    python deploy/update_xai_auth_headers.py apply --version 1.0.34 --replace-headers

On the server after stopping CPA:

    python3 deploy/update_xai_auth_headers.py --local inspect
    python3 deploy/update_xai_auth_headers.py --local apply --version 1.0.34
"""

from __future__ import annotations

import argparse
import collections
import json
import os
import shlex
import stat
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any, Mapping, Sequence

SCRIPT_PATH = Path(__file__).resolve()

DEFAULT_SSH_KEY = Path("E:/Files/SSH Key/oracle-ssh-key-2026-05-16.key")
DEFAULT_HOST = "163.192.9.157"
DEFAULT_USER = "ubuntu"
DEFAULT_SSH_PORT = 27312
DEFAULT_AUTH_DIR = "/home/ubuntu/.cli-proxy-api"
DEFAULT_SERVICE = "cli-proxy-api.service"

XAI_MARKERS = ("xai", "grok")
TOKEN_AUTH_HEADER = "X-XAI-Token-Auth"
TOKEN_AUTH_VALUE = "xai-grok-cli"
CLIENT_VERSION_HEADER = "x-grok-client-version"
CLIENT_IDENTIFIER_HEADER = "x-grok-client-identifier"
CLIENT_IDENTIFIER_VALUE = "grok-shell"
USER_AGENT_HEADER = "User-Agent"
REDACT_HEADER_NAMES = {"authorization"}


class HeaderToolError(RuntimeError):
    """Raised when inspect or apply cannot proceed safely."""


def identity_values(path: Path, obj: Mapping[str, Any]) -> list[str]:
    values = [path.name]
    for key in ("type", "provider", "auth_type", "kind", "service", "name"):
        value = obj.get(key)
        if isinstance(value, str):
            values.append(value)
    return values


def is_xai_auth(path: Path, obj: Mapping[str, Any]) -> bool:
    return any(
        any(marker in value.lower() for marker in XAI_MARKERS)
        for value in identity_values(path, obj)
    )


def load_json_object(path: Path) -> tuple[bytes, dict[str, Any]]:
    raw = path.read_bytes()
    obj = json.loads(raw)
    if not isinstance(obj, dict):
        raise HeaderToolError(f"{path.name}: JSON root is not an object")
    return raw, obj


def header_lookup(headers: Mapping[str, Any]) -> dict[str, tuple[str, Any]]:
    out: dict[str, tuple[str, Any]] = {}
    for key, value in headers.items():
        if isinstance(key, str):
            out[key.lower()] = (key, value)
    return out


def display_header_value(name: str, value: Any) -> str:
    if not isinstance(value, str):
        return f"<{type(value).__name__}>"
    if name.lower() in REDACT_HEADER_NAMES or value.lower().startswith("bearer "):
        return f"<redacted len={len(value)}>"
    if len(value) > 120:
        return f"<redacted len={len(value)}>"
    return value


def user_agent_for_version(version: str) -> str:
    return f"grok-shell/{version} (linux; x86_64)"


def canonical_headers(version: str) -> dict[str, str]:
    return {
        TOKEN_AUTH_HEADER: TOKEN_AUTH_VALUE,
        CLIENT_VERSION_HEADER: version,
        CLIENT_IDENTIFIER_HEADER: CLIENT_IDENTIFIER_VALUE,
        USER_AGENT_HEADER: user_agent_for_version(version),
    }


def iter_xai_files(auth_dir: Path) -> tuple[list[Path], int, int, list[tuple[Path, bytes, dict[str, Any]]]]:
    files = sorted(auth_dir.glob("*.json"))
    parse_errors = 0
    non_xai = 0
    records: list[tuple[Path, bytes, dict[str, Any]]] = []
    for path in files:
        try:
            raw, obj = load_json_object(path)
        except Exception:
            parse_errors += 1
            continue
        if not is_xai_auth(path, obj):
            non_xai += 1
            continue
        records.append((path, raw, obj))
    return files, parse_errors, non_xai, records


def inspect_headers(auth_dir: Path) -> dict[str, Any]:
    files, parse_errors, non_xai, records = iter_xai_files(auth_dir)
    keysets: collections.Counter[tuple[str, ...]] = collections.Counter()
    field_values: dict[str, collections.Counter[str]] = collections.defaultdict(collections.Counter)
    spellings: dict[str, collections.Counter[str]] = collections.defaultdict(collections.Counter)
    missing_headers = 0
    non_string_headers = 0

    for _path, _raw, obj in records:
        headers = obj.get("headers")
        if not isinstance(headers, dict):
            missing_headers += 1
            keysets[("<absent>",)] += 1
            continue
        lowered = tuple(sorted(str(key).lower() for key in headers if isinstance(key, str)))
        keysets[lowered or ("<empty>",)] += 1
        for key, value in headers.items():
            if not isinstance(key, str):
                non_string_headers += 1
                continue
            lowered_key = key.lower()
            spellings[lowered_key][key] += 1
            field_values[lowered_key][display_header_value(key, value)] += 1
            if not isinstance(value, str):
                non_string_headers += 1

    return {
        "auth_dir": str(auth_dir),
        "json_files": len(files),
        "parse_errors": parse_errors,
        "non_xai_count": non_xai,
        "xai_count": len(records),
        "missing_headers": missing_headers,
        "non_string_header_entries": non_string_headers,
        "header_keysets": [
            {"count": count, "keys": list(keys)}
            for keys, count in keysets.most_common()
        ],
        "header_value_distribution": {
            name: dict(values.most_common())
            for name, values in sorted(field_values.items())
        },
        "header_key_spellings": {
            name: dict(items.most_common())
            for name, items in sorted(spellings.items())
        },
    }


def desired_headers(
    current: Mapping[str, Any] | None,
    version: str | None,
    replace_headers: bool,
    extra_sets: Mapping[str, str],
) -> dict[str, str]:
    if replace_headers:
        if not version:
            raise HeaderToolError("--replace-headers requires --version")
        next_headers = canonical_headers(version)
    else:
        next_headers = {}
        if isinstance(current, dict):
            for key, value in current.items():
                if isinstance(key, str) and isinstance(value, str):
                    next_headers[key] = value
        if version:
            lookup = header_lookup(next_headers)
            if not lookup:
                next_headers = canonical_headers(version)
            else:
                version_key = lookup.get(CLIENT_VERSION_HEADER.lower(), (CLIENT_VERSION_HEADER, None))[0]
                ua_key = lookup.get(USER_AGENT_HEADER.lower(), (USER_AGENT_HEADER, None))[0]
                next_headers[version_key] = version
                next_headers[ua_key] = user_agent_for_version(version)
    for name, value in extra_sets.items():
        lookup = header_lookup(next_headers)
        existing = lookup.get(name.lower())
        key = existing[0] if existing else name
        next_headers[key] = value
    if not next_headers:
        raise HeaderToolError("apply requires --version and/or --set")
    return next_headers


def headers_match(obj: Mapping[str, Any], expected: Mapping[str, str]) -> bool:
    headers = obj.get("headers")
    if not isinstance(headers, dict):
        return False
    lookup = header_lookup(headers)
    if len(lookup) != len(expected):
        return False
    for key, value in expected.items():
        item = lookup.get(key.lower())
        if item is None or item[1] != value:
            return False
    return True


def serialize_auth(obj: Mapping[str, Any]) -> bytes:
    return (json.dumps(obj, ensure_ascii=False, indent=2, separators=(",", ": ")) + "\n").encode("utf-8")


def atomic_write(path: Path, data: bytes, mode: int) -> None:
    fd, temp_name = tempfile.mkstemp(
        prefix="." + path.name + ".",
        suffix=".tmp",
        dir=str(path.parent),
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temp_name, stat.S_IMODE(mode))
        os.replace(temp_name, path)
    finally:
        if os.path.exists(temp_name):
            os.unlink(temp_name)


def service_state(service: str) -> str:
    try:
        completed = subprocess.run(
            ["systemctl", "is-active", service],
            check=False,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
        )
    except FileNotFoundError:
        return "unknown"
    return (completed.stdout or completed.stderr or "").strip() or "unknown"


def apply_headers(
    auth_dir: Path,
    service: str,
    version: str | None,
    replace_headers: bool,
    extra_sets: Mapping[str, str],
    dry_run: bool,
    skip_service_check: bool,
) -> dict[str, Any]:
    if not skip_service_check:
        state = service_state(service)
        if state == "active":
            raise HeaderToolError(
                f"{service} is {state}; stop CPA before apply so refresh/keepalive cannot rewrite headers"
            )
        if state not in {"inactive", "failed", "dead"}:
            raise HeaderToolError(
                f"{service} state is {state!r}; stop CPA or pass --skip-service-check only for a test directory"
            )

    files, parse_errors, _skipped, records = iter_xai_files(auth_dir)
    if parse_errors:
        raise HeaderToolError(f"parse_errors={parse_errors}; refuse to apply")
    if not records:
        raise HeaderToolError(f"no xAI auth JSON under {auth_dir}")

    planned = 0
    already_exact = 0
    originals: dict[Path, bytes] = {}
    target_headers: dict[str, str] = {}
    if version and replace_headers:
        target_headers = canonical_headers(version)
    elif version:
        target_headers = {
            CLIENT_VERSION_HEADER: version,
            USER_AGENT_HEADER: user_agent_for_version(version),
        }
    target_headers.update(extra_sets)
    try:
        for path, _raw, initial in records:
            expected = desired_headers(
                initial.get("headers") if isinstance(initial.get("headers"), dict) else None,
                version,
                replace_headers,
                extra_sets,
            )
            if headers_match(initial, expected):
                already_exact += 1
                continue
            planned += 1
            if dry_run:
                continue
            current_raw = path.read_bytes()
            current_obj = json.loads(current_raw)
            if not isinstance(current_obj, dict) or not is_xai_auth(path, current_obj):
                raise HeaderToolError(f"{path.name}: xAI identity changed during apply")
            expected = desired_headers(
                current_obj.get("headers") if isinstance(current_obj.get("headers"), dict) else None,
                version,
                replace_headers,
                extra_sets,
            )
            updated = dict(current_obj)
            updated["headers"] = expected
            originals[path] = current_raw
            atomic_write(path, serialize_auth(updated), path.stat().st_mode)
            verify_obj = json.loads(path.read_bytes())
            if not isinstance(verify_obj, dict) or not headers_match(verify_obj, expected):
                raise HeaderToolError(f"{path.name}: post-write header verification failed")
    except Exception as exc:
        rolled_back = 0
        for path, original in originals.items():
            atomic_write(path, original, path.stat().st_mode)
            rolled_back += 1
        raise HeaderToolError(f"apply failed; rolled_back={rolled_back}; error={exc}") from exc

    after = inspect_headers(auth_dir) if not dry_run else None
    exact_after = 0
    if after is not None:
        exact_after = sum(
            1
            for _path, _raw, obj in iter_xai_files(auth_dir)[3]
            if headers_match(obj, desired_headers(
                obj.get("headers") if isinstance(obj.get("headers"), dict) else None,
                version,
                replace_headers,
                extra_sets,
            ))
        )
        if exact_after != len(records):
            rolled_back = 0
            for path, original in originals.items():
                atomic_write(path, original, path.stat().st_mode)
                rolled_back += 1
            raise HeaderToolError(
                f"verify failed: exact={exact_after} xai_count={len(records)} rolled_back={rolled_back}"
            )

    return {
        "mode": "dry_run" if dry_run else "applied",
        "auth_dir": str(auth_dir),
        "service": service,
        "json_files": len(files),
        "xai_count": len(records),
        "planned": planned,
        "already_exact": already_exact,
        "changed": 0 if dry_run else planned,
        "version": version,
        "replace_headers": replace_headers,
        "extra_sets": dict(extra_sets),
        "target_headers": target_headers,
        "after": after,
    }


def print_json(payload: Mapping[str, Any]) -> None:
    json.dump(payload, sys.stdout, ensure_ascii=False, sort_keys=True, indent=2)
    sys.stdout.write("\n")


def add_shared_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "--local",
        action="store_true",
        help="Operate on --auth-dir on this machine instead of SSH to Oracle 01",
    )
    parser.add_argument("--auth-dir", default=DEFAULT_AUTH_DIR, help="Auth JSON directory")
    parser.add_argument("--service", default=DEFAULT_SERVICE, help="systemd unit to require stopped for apply")
    parser.add_argument("--ssh-key", type=Path, default=DEFAULT_SSH_KEY)
    parser.add_argument("--host", default=DEFAULT_HOST)
    parser.add_argument("--user", default=DEFAULT_USER)
    parser.add_argument("--ssh-port", type=int, default=DEFAULT_SSH_PORT)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Inspect or rewrite xAI auth JSON headers. Stop CPA before apply.",
    )
    add_shared_arguments(parser)
    subparsers = parser.add_subparsers(dest="command", required=True)

    subparsers.add_parser("inspect", help="Show xAI header keysets and every header value distribution")

    apply_parser = subparsers.add_parser(
        "apply",
        help="Rewrite xAI headers. Refuses to run while CPA is active.",
    )
    apply_parser.add_argument(
        "--version",
        help="Set x-grok-client-version and User-Agent grok-shell/<version> (linux; x86_64)",
    )
    apply_parser.add_argument(
        "--replace-headers",
        action="store_true",
        help="Replace headers with the canonical four-key grok-shell set for --version",
    )
    apply_parser.add_argument(
        "--set",
        nargs=2,
        action="append",
        metavar=("NAME", "VALUE"),
        dest="set_headers",
        default=[],
        help="Set or override one header. Repeatable.",
    )
    apply_parser.add_argument("--dry-run", action="store_true", help="Count changes without writing")
    apply_parser.add_argument(
        "--skip-service-check",
        action="store_true",
        help="Allow apply while CPA is running. Unsafe on production.",
    )
    return parser


def run_local(args: argparse.Namespace) -> int:
    auth_dir = Path(args.auth_dir)
    if not auth_dir.is_dir():
        raise HeaderToolError(f"auth dir not found: {auth_dir}")
    if args.command == "inspect":
        print_json(inspect_headers(auth_dir))
        return 0
    extra_sets = {name: value for name, value in args.set_headers}
    if not args.version and not extra_sets:
        raise HeaderToolError("apply requires --version and/or --set")
    print_json(
        apply_headers(
            auth_dir=auth_dir,
            service=args.service,
            version=args.version,
            replace_headers=args.replace_headers,
            extra_sets=extra_sets,
            dry_run=args.dry_run,
            skip_service_check=args.skip_service_check,
        )
    )
    return 0


def ssh_base(args: argparse.Namespace) -> list[str]:
    return [
        "ssh",
        "-i",
        str(args.ssh_key),
        "-p",
        str(args.ssh_port),
        "-o",
        "BatchMode=yes",
        "-o",
        "ConnectTimeout=15",
        f"{args.user}@{args.host}",
    ]


def scp_base(args: argparse.Namespace) -> list[str]:
    return [
        "scp",
        "-q",
        "-i",
        str(args.ssh_key),
        "-P",
        str(args.ssh_port),
        "-o",
        "BatchMode=yes",
    ]


def run_remote(args: argparse.Namespace) -> int:
    if not args.ssh_key.is_file():
        raise HeaderToolError(f"SSH key does not exist: {args.ssh_key}")
    remote_path = f"/tmp/{SCRIPT_PATH.name}"
    upload = subprocess.run(
        [*scp_base(args), SCRIPT_PATH.as_posix(), f"{args.user}@{args.host}:{remote_path}"],
        check=False,
    )
    if upload.returncode != 0:
        raise HeaderToolError(f"scp failed with exit {upload.returncode}")
    remote_args = [
        "python3",
        remote_path,
        "--local",
        "--auth-dir",
        args.auth_dir,
        "--service",
        args.service,
        args.command,
    ]
    if args.command == "apply":
        if args.version:
            remote_args.extend(["--version", args.version])
        if args.replace_headers:
            remote_args.append("--replace-headers")
        for name, value in args.set_headers:
            remote_args.extend(["--set", name, value])
        if args.dry_run:
            remote_args.append("--dry-run")
        if args.skip_service_check:
            remote_args.append("--skip-service-check")
    try:
        completed = subprocess.run(
            [*ssh_base(args), shlex.join(remote_args)],
            check=False,
        )
        return completed.returncode
    finally:
        subprocess.run(
            [*ssh_base(args), f"rm -f {shlex.quote(remote_path)}"],
            check=False,
            capture_output=True,
        )


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.local:
            return run_local(args)
        return run_remote(args)
    except HeaderToolError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
