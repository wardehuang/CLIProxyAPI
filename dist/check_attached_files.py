#!/usr/bin/env python3
from pathlib import Path
import re

LOGS = Path("/opt/cli-proxy-api/logs")
files = sorted(
    [p for p in LOGS.glob("v1-messages-*.log") if not p.name.endswith("-strip.log")],
    key=lambda p: p.stat().st_mtime,
    reverse=True,
)[:30]

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

count = 0
for path in files:
    body = request_body(path.read_text(encoding="utf-8", errors="replace"))
    if "attached_files" not in body and "open_and_recently_viewed_files" not in body:
        continue
    count += 1
    print("=" * 80)
    print(path.name)
    for key in ("attached_files", "open_and_recently_viewed_files", "visible_files", "user_query"):
        # show first few occurrences with context
        for m in re.finditer(re.escape(key), body):
            start = max(0, m.start() - 120)
            end = min(len(body), m.end() + 350)
            snip = body[start:end].replace("\n", "\\n")
            print(f"\n[{key}]")
            print(snip)
            break
    if count >= 4:
        break
print(f"\nfiles_with_attached_or_open={count}")
