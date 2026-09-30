#!/usr/bin/env python3
import json, re
from pathlib import Path
from collections import Counter

files = [
    "v1-messages-2026-08-19T014145-18a67566.log",  # bad
    "v1-messages-2026-08-19T014150-6e1d7817.log",  # bad
    "v1-messages-2026-08-19T014343-65f32289.log",  # good
    "v1-messages-2026-08-19T014419-9e42b4a2.log",  # good
    "v1-messages-2026-08-19T014513-89e66166.log",  # bad
    "v1-messages-2026-08-19T014522-e7ca676e.log",  # bad
]
logdir = Path("/opt/cli-proxy-api/logs")

def sections(text):
    parts = re.split(r"^=== ([^=]+) ===\s*$", text, flags=re.M)
    return {parts[i].strip(): parts[i+1] for i in range(1, len(parts), 2)}

def event_order(body):
    out=[]
    for line in body.splitlines():
        s=line.strip()
        if s.startswith("event:"):
            out.append(s[6:].strip())
        elif s.startswith("data:"):
            d=s[5:].strip()
            if d and d!="[DONE]":
                try:
                    t=json.loads(d).get("type")
                    if t: out.append("data:"+t)
                except Exception:
                    if "usage" in d: out.append("data:has_usage")
    return out

for name in files:
    p=logdir/name
    text=p.read_text(errors="replace")
    sec=sections(text)
    info=sec.get("REQUEST INFO","")
    api=sec.get("API REQUEST 1","")
    up=sec.get("API RESPONSE 1","")
    down=sec.get("RESPONSE","")
    reqb=sec.get("REQUEST BODY","")
    print("="*80)
    print(name)
    print(info.strip().splitlines()[0] if info.strip() else "")
    print("ts", [ln for ln in info.splitlines() if ln.startswith("Timestamp")][:1])
    # upstream url model
    m=re.search(r"Upstream URL: (.+)", api)
    print("upstream", m.group(1).strip() if m else None)
    try:
        rb=json.loads(reqb.strip())
        print("client model", rb.get("model"), "msgs", len(rb.get("messages") or []), "tools", len(rb.get("tools") or []))
    except Exception as e:
        print("req", e)
    # check usage in upstream completed
    completed = None
    for line in up.splitlines():
        if "response.completed" in line and line.strip().startswith("data:"):
            completed = line.strip()[5:].strip()
    if completed:
        try:
            obj=json.loads(completed)
            resp=obj.get("response") or {}
            print("completed keys", sorted(resp.keys()))
            print("usage", resp.get("usage"))
            print("status", resp.get("status"))
            # output items types
            outs=resp.get("output") or []
            print("output_n", len(outs), "types", [o.get("type") for o in outs[:8]])
        except Exception as e:
            print("completed parse fail", e, completed[:200])
    else:
        print("NO completed data line")
    uo=event_order(up)
    do=event_order(down)
    print("UP", Counter(uo).most_common(15), "last", uo[-8:])
    print("DOWN", Counter(do).most_common(15), "last", do[-10:])
    print("message_stop", any(x=="message_stop" or x=="data:message_stop" for x in do))
    # body snippet of API request for input roles
    bm=re.search(r"Body:\n(\{.*)", api, re.S)
    if bm:
        body=bm.group(1).strip().split("\n\n")[0]
        try:
            # may be truncated
            if body.endswith("..."):
                print("body truncated")
            else:
                bj=json.loads(body)
                print("up model", bj.get("model"), "stream", bj.get("stream"), "reasoning", bj.get("reasoning"))
                print("input types", [(i.get("type"), i.get("role")) for i in (bj.get("input") or [])[:6]])
        except Exception as e:
            print("up body parse", str(e)[:80], "head", body[:120])
