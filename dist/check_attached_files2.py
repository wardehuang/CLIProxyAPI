#!/usr/bin/env python3
from pathlib import Path
import re

LOGS = Path("/opt/cli-proxy-api/logs")
files = sorted(
    [p for p in LOGS.glob("v1-messages-*.log") if not p.name.endswith("-strip.log")],
    key=lambda p: p.stat().st_mtime,
    reverse=True,
)[:50]

def request_body(text: str) -> str:
    marker = "=== REQUEST BODY ==="
    idx = text.find(marker)
    if idx < 0:
        return ""
    rest = text[idx + len(marker):]
    for stop in ("\n=== API REQUEST", "\n=== RESPONSE", "\n=== API RESPONSE"):
        s = rest.find(stop)
        if s > 0:
            rest = rest[:s]
            break
    return rest

tag_patterns = {
    "attached_files": re.compile(r"(?is)<attached_files\b[^>]*>.*?</attached_files\s*>"),
    "open_and_recently_viewed_files": re.compile(r"(?is)<open_and_recently_viewed_files\b[^>]*>.*?</open_and_recently_viewed_files\s*>"),
    "visible_files": re.compile(r"(?is)<visible_files\b[^>]*>.*?</visible_files\s*>"),
    "user_query": re.compile(r"(?is)<user_query\b[^>]*>.*?</user_query\s*>"),
}

open_only = {
    "attached_files_open": re.compile(r"(?is)<attached_files\b[^>]{0,200}>"),
    "open_recent_open": re.compile(r"(?is)<open_and_recently_viewed_files\b[^>]{0,200}>"),
    "file_tag": re.compile(r'(?is)<file\b[^>]*path\s*=\s*"[^"]+"[^>]{0,120}>'),
}

print("=== closed tag counts ===")
for path in files:
    body = request_body(path.read_text(encoding="utf-8", errors="replace"))
    if not body:
        continue
    counts = {k: len(p.findall(body)) for k, p in tag_patterns.items()}
    opens = {k: len(p.findall(body)) for k, p in open_only.items()}
    if any(counts.values()) or any(opens.values()):
        print(path.name, "closed=", counts, "opens=", opens)

print("\n=== first real attached_files open tags ===")
shown = 0
for path in files:
    body = request_body(path.read_text(encoding="utf-8", errors="replace"))
    for m in open_only["attached_files_open"].finditer(body):
        start = max(0, m.start() - 100)
        end = min(len(body), m.end() + 500)
        snip = body[start:end]
        print("=" * 60)
        print(path.name)
        print(snip[:700].replace("\n", "\\n"))
        shown += 1
        break
    if shown >= 5:
        break
if shown == 0:
    print("NO real <attached_files ...> open tags found in recent 50 request logs")

print("\n=== first real open_and_recently_viewed_files open tags ===")
shown = 0
for path in files:
    body = request_body(path.read_text(encoding="utf-8", errors="replace"))
    for m in open_only["open_recent_open"].finditer(body):
        start = max(0, m.start() - 100)
        end = min(len(body), m.end() + 500)
        snip = body[start:end]
        print("=" * 60)
        print(path.name)
        print(snip[:700].replace("\n", "\\n"))
        shown += 1
        break
    if shown >= 5:
        break
if shown == 0:
    print("NO real <open_and_recently_viewed_files ...> open tags found")
