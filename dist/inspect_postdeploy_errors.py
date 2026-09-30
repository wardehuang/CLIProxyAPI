#!/usr/bin/env python3
import re
from pathlib import Path

patterns = [
    "Unknown tool",
    "Tool not found",
    "not available",
    "Error calling",
    "INVALID_SEARCH",
    "Failed to call",
    "MCP error",
    "No such tool",
    '"is_error":true',
    '"isError":true',
]
logdir = Path("/opt/cli-proxy-api/logs")
files = sorted(
    [
        path
        for path in logdir.glob("v1-messages-2026-07-27T03*.log")
        if (not path.name.endswith("-strip.log")) and re.search(r"T03(2[6-9]|[3-5])", path.name)
    ],
    key=lambda path: path.stat().st_mtime,
    reverse=True,
)
print("post-deploy files", len(files))
for path in files[:30]:
    text = path.read_text(encoding="utf-8", errors="replace")
    hits = []
    for pattern in patterns:
        start = 0
        found_indexes = []
        while True:
            index = text.find(pattern, start)
            if index < 0:
                break
            context = text[max(0, index - 100) : index + 80]
            if "Handoff" in context or "任务目标" in context or "不要改 claude-mem" in context:
                start = index + 1
                continue
            found_indexes.append(index)
            start = index + 1
            if len(found_indexes) >= 2:
                break
        if found_indexes:
            snippet = text[max(0, found_indexes[0] - 80) : found_indexes[0] + 140].replace("\n", " | ")
            hits.append((pattern, len(found_indexes), snippet))
    has_query = "/mem-search" in text or "mem-search CPA" in text
    if hits:
        print("FILE", path.name, "has_mem_query", has_query)
        for hit in hits:
            print(" ", hit[0], "=>", hit[2][:250])
