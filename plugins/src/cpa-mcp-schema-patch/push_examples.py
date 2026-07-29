#!/usr/bin/env python3
"""Push local MCP tool JSON descriptors to CPA cpa-mcp-schema-patch.

Uploads every *.json under examples/ (or a custom --examples-dir) via:

  POST /v0/management/mcp-schema-patch/upload

Auth (one of):
  - env CPA_MANAGEMENT_KEY
  - env MANAGEMENT_KEY
  - flag --management-key
  - header X-Management-Key / Authorization: Bearer <key>

Examples:

  set CPA_MANAGEMENT_KEY=your-key
  python push_examples.py

  python push_examples.py --base-url http://127.0.0.1:18457 --management-key YOUR_KEY

  python push_examples.py --dry-run
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path


DEFAULT_BASE_URL = "https://wcpa.edmundvps.site:18457"
DEFAULT_EXAMPLES_DIR = Path(__file__).resolve().parent / "examples"


def discover_json_files(examples_dir: Path) -> list[tuple[str, Path]]:
    """Return (relative file_name with forward slashes, absolute path)."""
    if not examples_dir.is_dir():
        raise FileNotFoundError(f"examples dir not found: {examples_dir}")

    items: list[tuple[str, Path]] = []
    for path in sorted(examples_dir.rglob("*.json")):
        if not path.is_file():
            continue
        relative = path.relative_to(examples_dir).as_posix()
        items.append((relative, path))
    return items


def build_request(
    base_url: str,
    file_name: str,
    content: str,
    management_key: str,
    reload_after: bool,
) -> urllib.request.Request:
    endpoint = base_url.rstrip("/") + "/v0/management/mcp-schema-patch/upload"
    payload = {
        "file_name": file_name,
        "content": content,
        "reload_after": reload_after,
    }
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    request = urllib.request.Request(
        endpoint,
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/json; charset=utf-8",
            "Accept": "application/json",
            "X-Management-Key": management_key,
            "Authorization": f"Bearer {management_key}",
        },
    )
    return request


def post_upload(
    base_url: str,
    file_name: str,
    content: str,
    management_key: str,
    reload_after: bool,
    timeout_seconds: float,
) -> tuple[int, dict | str]:
    request = build_request(base_url, file_name, content, management_key, reload_after)
    try:
        with urllib.request.urlopen(request, timeout=timeout_seconds) as response:
            raw = response.read()
            status = response.getcode()
    except urllib.error.HTTPError as http_error:
        raw = http_error.read()
        status = http_error.code
    except urllib.error.URLError as url_error:
        raise RuntimeError(f"request failed for {file_name}: {url_error}") from url_error

    text = raw.decode("utf-8", errors="replace")
    try:
        return status, json.loads(text)
    except json.JSONDecodeError:
        return status, text


def list_schemas(base_url: str, management_key: str, timeout_seconds: float) -> tuple[int, dict | str]:
    endpoint = base_url.rstrip("/") + "/v0/management/mcp-schema-patch/schemas"
    request = urllib.request.Request(
        endpoint,
        method="GET",
        headers={
            "Accept": "application/json",
            "X-Management-Key": management_key,
            "Authorization": f"Bearer {management_key}",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout_seconds) as response:
            raw = response.read()
            status = response.getcode()
    except urllib.error.HTTPError as http_error:
        raw = http_error.read()
        status = http_error.code
    except urllib.error.URLError as url_error:
        raise RuntimeError(f"list schemas failed: {url_error}") from url_error

    text = raw.decode("utf-8", errors="replace")
    try:
        return status, json.loads(text)
    except json.JSONDecodeError:
        return status, text


def resolve_management_key(cli_key: str | None) -> str:
    candidates = [
        (cli_key or "").strip(),
        os.environ.get("CPA_MANAGEMENT_KEY", "").strip(),
        os.environ.get("MANAGEMENT_KEY", "").strip(),
    ]
    for value in candidates:
        if value:
            return value
    return ""


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Upload examples/*.json to CPA cpa-mcp-schema-patch management API",
    )
    parser.add_argument(
        "--base-url",
        default=os.environ.get("CPA_BASE_URL", DEFAULT_BASE_URL),
        help=f"CPA base URL (default: {DEFAULT_BASE_URL} or env CPA_BASE_URL)",
    )
    parser.add_argument(
        "--examples-dir",
        default=str(DEFAULT_EXAMPLES_DIR),
        help="Directory containing MCP tool JSON files (default: ./examples)",
    )
    parser.add_argument(
        "--management-key",
        default="",
        help="Management key (or env CPA_MANAGEMENT_KEY / MANAGEMENT_KEY)",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=30.0,
        help="HTTP timeout seconds (default: 30)",
    )
    parser.add_argument(
        "--no-reload-each",
        action="store_true",
        help="Do not reload after each file; only reload after the last file",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="List files that would be uploaded without calling the API",
    )
    parser.add_argument(
        "--list-only",
        action="store_true",
        help="Only GET /mcp-schema-patch/schemas and exit",
    )
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    examples_dir = Path(args.examples_dir).resolve()
    management_key = resolve_management_key(args.management_key)

    if args.list_only:
        if not management_key:
            print("missing management key", file=sys.stderr)
            return 2
        status, payload = list_schemas(args.base_url, management_key, args.timeout)
        print(f"HTTP {status}")
        print(json.dumps(payload, ensure_ascii=False, indent=2) if isinstance(payload, dict) else payload)
        return 0 if 200 <= status < 300 else 1

    files = discover_json_files(examples_dir)
    if not files:
        print(f"no *.json under {examples_dir}", file=sys.stderr)
        return 1

    print(f"examples_dir={examples_dir}")
    print(f"base_url={args.base_url}")
    print(f"file_count={len(files)}")
    for file_name, path in files:
        print(f"  - {file_name} ({path.stat().st_size} bytes)")

    if args.dry_run:
        print("dry-run: no upload")
        return 0

    if not management_key:
        print(
            "missing management key: set CPA_MANAGEMENT_KEY or pass --management-key",
            file=sys.stderr,
        )
        return 2

    failures = 0
    last_index = len(files) - 1
    for index, (file_name, path) in enumerate(files):
        content = path.read_text(encoding="utf-8")
        if args.no_reload_each:
            reload_after = index == last_index
        else:
            reload_after = True

        try:
            status, payload = post_upload(
                base_url=args.base_url,
                file_name=file_name,
                content=content,
                management_key=management_key,
                reload_after=reload_after,
                timeout_seconds=args.timeout,
            )
        except RuntimeError as runtime_error:
            failures += 1
            print(f"FAIL {file_name}: {runtime_error}", file=sys.stderr)
            continue

        ok = 200 <= status < 300
        if ok:
            tool_count = payload.get("tool_count") if isinstance(payload, dict) else None
            print(f"OK   {file_name}  HTTP {status}  tool_count={tool_count}")
        else:
            failures += 1
            print(f"FAIL {file_name}  HTTP {status}", file=sys.stderr)
            print(payload if isinstance(payload, str) else json.dumps(payload, ensure_ascii=False), file=sys.stderr)

    if failures:
        print(f"done with failures={failures}/{len(files)}", file=sys.stderr)
        return 1

    print(f"done ok={len(files)}")
    status, payload = list_schemas(args.base_url, management_key, args.timeout)
    if 200 <= status < 300 and isinstance(payload, dict):
        tool_names = payload.get("tool_names") or []
        print(f"registry tool_count={payload.get('tool_count')} tools={tool_names}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
