#!/usr/bin/env python3
import json
import re
from collections import Counter
from pathlib import Path

files = [
    "v1-messages-2026-08-19T014145-18a67566.log",
    "v1-messages-2026-08-19T014150-6e1d7817.log",
]
logdir = Path("/opt/cli-proxy-api/logs")

def split_sections(text: str) -> dict:
    parts = re.split(r"^=== ([^=]+) ===\s*$", text, flags=re.M)
    return {parts[i].strip(): parts[i + 1] for i in range(1, len(parts), 2)}

def event_types(body: str):
    types = []
    for line in body.splitlines():
        s = line.strip()
        if s.startswith("event:"):
            types.append(s[6:].strip())
        elif s.startswith("data:"):
            data = s[5:].strip()
            if data and data != "[DONE]":
                try:
                    obj = json.loads(data)
                    t = obj.get("type")
                    if t:
                        types.append(f"data:{t}")
                except Exception:
                    pass
    return types

def summarize(name: str):
    p = logdir / name
    print("=" * 80)
    print(name, "exists" if p.exists() else "MISSING", p.stat().st_size if p.exists() else 0)
    if not p.exists():
        # fuzzy
        hits = sorted(logdir.glob(f"*{name.split('-')[-1].replace('.log','')}*"))
        print("fuzzy", [h.name for h in hits[:10]])
        return
    text = p.read_text(encoding="utf-8", errors="replace")
    sec = split_sections(text)
    print("SECTIONS", list(sec.keys()))
    print(sec.get("REQUEST INFO", "").strip()[:500])
    api = sec.get("API REQUEST 1", "")
    print("--- API REQUEST head ---")
    for ln in api.strip().splitlines()[:35]:
        print(ln[:240])
    api_resp = sec.get("API RESPONSE 1", "")
    resp = sec.get("RESPONSE", "")
    up = event_types(api_resp)
    down = event_types(resp)
    print("UP events", Counter(up).most_common(20))
    print("UP order unique", up[:30], "... total", len(up))
    print("DOWN events", Counter(down).most_common(20))
    print("DOWN has message_start", any("message_start" in e for e in down))
    print("DOWN has message_delta", any("message_delta" in e for e in down))
    print("DOWN has message_stop", any("message_stop" in e for e in down))
    print("DOWN has content_block_stop", any("content_block_stop" in e for e in down))
    # tail of RESPONSE
    i = resp.find("event:")
    body = resp[i:] if i >= 0 else resp
    lines = [ln for ln in body.splitlines() if ln.strip()]
    print("DOWN tail:")
    for ln in lines[-20:]:
        print(" ", ln[:240])
    print("UP tail:")
    ui = api_resp.find("event:")
    ub = api_resp[ui:] if ui >= 0 else api_resp
    ulines = [ln for ln in ub.splitlines() if ln.strip()]
    for ln in ulines[-15:]:
        print(" ", ln[:240])
    for kw in ["cpa-deepseek", "reasoning", "plugin", "error", "400", "invalid_request", "pad", "inject"]:
        c = len(re.findall(re.escape(kw), text, flags=re.I))
        if c:
            print(f"kw {kw}={c}")
    # request body roles
    try:
        req = json.loads(sec.get("REQUEST BODY", "").strip())
        print("model", req.get("model"), "stream", req.get("stream"), "msgs", len(req.get("messages") or []), "tools", len(req.get("tools") or []))
    except Exception as e:
        print("req parse", e)

# nearby deepseek logs same minute
print("NEARBY")
for p in sorted(logdir.glob("v1-messages-2026-08-19T0141*.log")):
    print(p.name, p.stat().st_size)

for f in files:
    summarize(f)

# journal plugin lines around that time
print("=" * 80)
print("Also list plugin so mtime and enabled config snippet")
