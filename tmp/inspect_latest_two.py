#!/usr/bin/env python3
import json, re
from collections import Counter
from pathlib import Path

files = [
    "v1-messages-2026-08-19T061852-733e2297.log",
    "v1-messages-2026-08-19T061858-b73c344c.log",
    "v1-messages-2026-08-19T061909-9fd44a14.log",
    "v1-messages-2026-08-19T061913-6ae8b6e5.log",
]
logdir = Path("/opt/cli-proxy-api/logs")

def sections(text):
    parts = re.split(r"^=== ([^=]+) ===\s*$", text, flags=re.M)
    return {parts[i].strip(): parts[i+1] for i in range(1, len(parts), 2)}

def event_types(body):
    types=[]
    for line in body.splitlines():
        s=line.strip()
        if s.startswith("event:"):
            types.append(s[6:].strip())
        elif s.startswith("data:"):
            d=s[5:].strip()
            if d and d != "[DONE]":
                try:
                    t=json.loads(d).get("type")
                    if t: types.append("data:"+t)
                except Exception:
                    pass
            elif d == "[DONE]":
                types.append("[DONE]")
    return types

for name in files:
    p = logdir/name
    print("="*80)
    print(name, "exists" if p.exists() else "MISSING", p.stat().st_size if p.exists() else 0)
    if not p.exists():
        continue
    text=p.read_text(encoding="utf-8", errors="replace")
    sec=sections(text)
    print(sec.get("REQUEST INFO","").strip()[:400])
    api=sec.get("API REQUEST 1","")
    up=sec.get("API RESPONSE 1","")
    down=sec.get("RESPONSE","")
    m=re.search(r"Upstream URL: (.+)", api)
    print("upstream", m.group(1).strip() if m else None)
    try:
        rb=json.loads(sec.get("REQUEST BODY","").strip())
        print("model", rb.get("model"), "stream", rb.get("stream"), "msgs", len(rb.get("messages") or []), "tools", len(rb.get("tools") or []))
    except Exception as e:
        print("req parse", e)
    completed=None
    for line in up.splitlines():
        if line.strip().startswith("data:") and "response.completed" in line:
            completed=line.strip()[5:].strip()
    if completed:
        try:
            obj=json.loads(completed)
            resp=obj.get("response") or {}
            print("completed keys", sorted(resp.keys()))
            print("usage", resp.get("usage"))
        except Exception as e:
            print("completed parse", e, completed[:180])
    else:
        print("NO completed")
    uo=event_types(up); do=event_types(down)
    print("UP", Counter(uo).most_common(12), "last", uo[-8:])
    print("DOWN", Counter(do).most_common(12), "last", do[-12:])
    print("message_stop", any("message_stop" in x for x in do), "message_delta", any("message_delta" in x for x in do), "content_block_stop", any("content_block_stop" in x for x in do))
    for kw in ["reasoning_content","invalid_request","400","plugin"]:
        c=len(re.findall(re.escape(kw), text, flags=re.I))
        if c: print(f"kw {kw}={c}")
