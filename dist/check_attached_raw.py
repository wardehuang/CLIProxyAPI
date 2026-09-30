from pathlib import Path
import re

LOGS = Path("/opt/cli-proxy-api/logs")
files = sorted(
    [p for p in LOGS.glob("v1-messages-*.log") if not p.name.endswith("-strip.log")],
    key=lambda p: p.stat().st_mtime,
    reverse=True,
)[:30]
pat = re.compile(r"(?is)<attached_files\b[^>]{0,300}")
pat2 = re.compile(r"(?is)<open_and_recently_viewed_files\b[^>]{0,300}")

print("=== attached_files opens ===")
shown = 0
for path in files:
    text = path.read_text(encoding="utf-8", errors="replace")
    idx = text.find("=== REQUEST BODY ===")
    body = text[idx : idx + 2_000_000] if idx >= 0 else text[:2_000_000]
    match = pat.search(body)
    if not match:
        continue
    start = max(0, match.start() - 100)
    end = min(len(body), match.end() + 500)
    snip = body[start:end]
    print("FILE", path.name)
    print(snip[:600].replace("\n", "\\n"))
    print("---")
    shown += 1
    if shown >= 6:
        break
print("shown", shown)

print("\n=== open_and_recently_viewed_files opens ===")
shown = 0
for path in files:
    text = path.read_text(encoding="utf-8", errors="replace")
    idx = text.find("=== REQUEST BODY ===")
    body = text[idx : idx + 2_000_000] if idx >= 0 else text[:2_000_000]
    match = pat2.search(body)
    if not match:
        continue
    start = max(0, match.start() - 100)
    end = min(len(body), match.end() + 500)
    snip = body[start:end]
    print("FILE", path.name)
    print(snip[:600].replace("\n", "\\n"))
    print("---")
    shown += 1
    if shown >= 4:
        break
print("shown", shown)
