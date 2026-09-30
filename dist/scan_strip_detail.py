#!/usr/bin/env python3
from pathlib import Path
import re
from collections import Counter

LOGS = Path("/opt/cli-proxy-api/logs")

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

# Use one mid-size and one large file
files = sorted(
    [p for p in LOGS.glob("v1-messages-*.log") if not p.name.endswith("-strip.log")],
    key=lambda p: p.stat().st_mtime,
    reverse=True,
)[:20]

print("=== per-file top closed tags by total bytes ===")
for path in files[:8]:
    body = request_body(path.read_text(encoding="utf-8", errors="replace"))
    if not body:
        continue
    names = set(re.findall(r"</([a-zA-Z_][\w.-]{1,60})\s*>", body))
    rows = []
    for name in names:
        pat = re.compile(rf"(?is)<{re.escape(name)}\b[^>]*>.*?</{re.escape(name)}\s*>")
        sizes = [len(m.group(0)) for m in pat.finditer(body)]
        if sizes:
            rows.append((sum(sizes), max(sizes), len(sizes), name))
    rows.sort(reverse=True)
    print(f"\n{path.name} body_chars={len(body)}")
    for total, mx, n, name in rows[:12]:
        print(f"  {name:40s} total={total:8d} max={mx:8d} n={n}")

# Inspect user_info composition from largest user_info
print("\n=== user_info peek (largest) ===")
largest_user_info = ("", 0, "")
for path in files:
    body = request_body(path.read_text(encoding="utf-8", errors="replace"))
    pat = re.compile(r"(?is)<user_info\b[^>]*>.*?</user_info\s*>")
    for m in pat.finditer(body):
        block = m.group(0)
        if len(block) > largest_user_info[1]:
            largest_user_info = (path.name, len(block), block)

print("file", largest_user_info[0], "size", largest_user_info[1])
preview = largest_user_info[2]
print("preview_head:\n", preview[:800].replace("\n", "\\n"))
print("preview_tail:\n", preview[-400:].replace("\n", "\\n"))
# nested tags inside user_info
nested = Counter(re.findall(r"</([a-zA-Z_][\w.-]+)\s*>", preview))
print("nested_closed_in_user_info", nested.most_common(20))

# open_and_recently_viewed_files / attached_files raw context
print("\n=== open_and_recently_viewed_files context samples ===")
for path in files:
    body = request_body(path.read_text(encoding="utf-8", errors="replace"))
    if "open_and_recently_viewed_files" not in body and "attached_files" not in body:
        continue
    for key in ("open_and_recently_viewed_files", "attached_files", "ide_state", "visible_files"):
        i = body.find(key)
        if i >= 0:
            snip = body[max(0, i - 80): i + 200].replace("\n", "\\n")
            print(path.name, key, "=>", snip[:280])
    break

# Check system field / tools size roughly via JSON keys if present
print("\n=== system / tools size hints in sample ===")
path = files[0]
body = request_body(path.read_text(encoding="utf-8", errors="replace"))
for key in ('"system"', '"tools"', '"messages"'):
    print(key, "count", body.count(key))
# approximate by finding "system": and next top-level-ish
for m in re.finditer(r'"(system|tools|messages)"\s*:', body):
    print(" key", m.group(1), "at", m.start())
