#!/usr/bin/env python3
"""Scan recent request logs for Cursor/IDE XML-ish context blocks worth stripping."""

from __future__ import annotations

import os
import re
from collections import Counter, defaultdict
from pathlib import Path

LOGS_DIR = Path("/opt/cli-proxy-api/logs")
MAX_FILES = 40
# Prefer large recent v1-messages request logs, not strip detail logs.
FILE_GLOBS = (
    "v1-messages-*.log",
)

# Known / suspected Cursor / agent wrapper tags
KNOWN_TAGS = [
    "visible_files",
    "open_and_recently_viewed_files",
    "attached_files",
    "agent_transcripts",
    "user_info",
    "ide_state",
    "open_and_recently_viewed_files",
    "terminal_files_information",
    "agent_transcripts",
    "rules",
    "user_rules",
    "system_reminder",
    "user_query",
    "open_and_recently_viewed_files",
    "cursor_rules_context",
    "always_applied_workspace_rules",
    "mcp_file_system_servers",
    "mcp_file_system",
    "available_skills",
    "agent_skills",
    "task_notification",
    "task_management",
    "making_code_changes",
    "citing_code",
    "inline_line_numbers",
    "terminal_files_information",
    "git_status",
    "project_layout",
    "repository_structure",
    "environment_info",
    "os_info",
]

# Generic XML-ish open tags in bodies
GENERIC_TAG_PATTERN = re.compile(r"<(?P<name>[a-zA-Z_][\w.-]{1,80})(?:\s|>|/)", re.M)
BLOCK_PATTERN_TEMPLATE = r"(?is)<{name}\b[^>]*>.*?</{name}\s*>"


def list_candidate_files() -> list[Path]:
    files: list[Path] = []
    for path in LOGS_DIR.glob("v1-messages-*.log"):
        name = path.name
        if name.endswith("-strip.log"):
            continue
        files.append(path)
    files.sort(key=lambda p: p.stat().st_mtime, reverse=True)
    return files[:MAX_FILES]


def extract_request_body(text: str) -> str:
    marker = "=== REQUEST BODY ==="
    idx = text.find(marker)
    if idx < 0:
        return text[:2_000_000]
    rest = text[idx + len(marker) :]
    # stop at next major section if present
    for stop in (
        "\n=== API REQUEST",
        "\n=== RESPONSE",
        "\n=== API RESPONSE",
        "\n=== HEADERS ===",
    ):
        stop_idx = rest.find(stop)
        if stop_idx > 0:
            rest = rest[:stop_idx]
            break
    return rest[:5_000_000]


def estimate_block_sizes(body: str, tag_name: str) -> list[int]:
    pattern = re.compile(BLOCK_PATTERN_TEMPLATE.format(name=re.escape(tag_name)))
    return [len(match.group(0)) for match in pattern.finditer(body)]


def main() -> None:
    files = list_candidate_files()
    print(f"scanned_files={len(files)}")
    if not files:
        print("no candidate logs")
        return

    tag_hits = Counter()
    tag_files = defaultdict(set)
    tag_bytes = Counter()
    tag_block_counts = Counter()
    tag_max_block = Counter()
    sample_paths: dict[str, str] = {}
    generic_open_tags = Counter()

    for path in files:
        try:
            raw = path.read_text(encoding="utf-8", errors="replace")
        except Exception as exc:
            print(f"read_fail {path.name}: {exc}")
            continue
        body = extract_request_body(raw)
        lower_body = body.lower()

        # known tags
        for tag in KNOWN_TAGS:
            needle = f"<{tag}"
            if needle.lower() not in lower_body:
                continue
            tag_hits[tag] += 1
            tag_files[tag].add(path.name)
            sizes = estimate_block_sizes(body, tag)
            if sizes:
                total = sum(sizes)
                tag_bytes[tag] += total
                tag_block_counts[tag] += len(sizes)
                tag_max_block[tag] = max(tag_max_block[tag], max(sizes))
            else:
                # tag open present but no closed block match
                tag_bytes[tag] += 0
            if tag not in sample_paths:
                sample_paths[tag] = path.name

        # generic tags
        for match in GENERIC_TAG_PATTERN.finditer(body[:1_500_000]):
            name = match.group("name")
            # skip common HTML/code noise partially
            if name.lower() in {"div", "span", "br", "p", "a", "b", "i", "ul", "li", "pre", "code", "table", "tr", "td", "th", "html", "body", "head", "script", "style"}:
                continue
            if name.startswith("http") or name.startswith("www"):
                continue
            generic_open_tags[name] += 1

    print("\n=== KNOWN TAG HITS (file-level) ===")
    for tag, count in tag_hits.most_common():
        files_n = len(tag_files[tag])
        avg_blocks = (tag_block_counts[tag] / count) if count else 0
        print(
            f"{tag:40s} files={files_n:2d} hits={count:2d} "
            f"sum_block_bytes={tag_bytes[tag]:10d} max_block={tag_max_block[tag]:8d} "
            f"blocks={tag_block_counts[tag]:3d} sample={sample_paths.get(tag,'-')}"
        )

    print("\n=== STRIP CANDIDATES (heuristic) ===")
    # Recommend tags that are auto-context, not the actual user query
    auto_context_like = {
        "visible_files",
        "open_and_recently_viewed_files",
        "attached_files",
        "agent_transcripts",
        "ide_state",
        "terminal_files_information",
        "always_applied_workspace_rules",
        "mcp_file_system_servers",
        "mcp_file_system",
        "available_skills",
        "agent_skills",
        "task_notification",
        "git_status",
        "project_layout",
        "repository_structure",
        "environment_info",
        "os_info",
    }
    keep_like = {
        "user_query",
        "user_info",  # small, often useful
        "rules",  # system rules may be intentional
        "user_rules",
        "system_reminder",
    }
    for tag in sorted(tag_hits.keys(), key=lambda t: (-tag_bytes[t], t)):
        role = "KEEP?" if tag in keep_like else ("STRIP?" if tag in auto_context_like or tag_bytes[tag] > 50_000 else "REVIEW")
        print(f"[{role}] {tag} max_block={tag_max_block[tag]} sum_bytes={tag_bytes[tag]} files={len(tag_files[tag])}")

    print("\n=== TOP GENERIC OPEN TAGS (raw names, noisy) ===")
    for name, count in generic_open_tags.most_common(40):
        print(f"{name:40s} {count}")

    # Deep-dive: sample one large recent request for structure
    print("\n=== SAMPLE STRUCTURE FROM LARGEST RECENT LOG ===")
    largest = max(files, key=lambda p: p.stat().st_size)
    print("file", largest.name, "size", largest.stat().st_size)
    body = extract_request_body(largest.read_text(encoding="utf-8", errors="replace"))
    # find unique closed tags and their total sizes
    closed = re.findall(r"</([a-zA-Z_][\w.-]{1,80})\s*>", body)
    closed_counter = Counter(closed)
    print("closed_tags_top:")
    for name, count in closed_counter.most_common(25):
        sizes = estimate_block_sizes(body, name)
        print(f"  {name:35s} closes={count:3d} blocks={len(sizes):3d} total_bytes={sum(sizes):8d} max={max(sizes) if sizes else 0}")


if __name__ == "__main__":
    main()
