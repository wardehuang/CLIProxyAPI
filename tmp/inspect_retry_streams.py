#!/usr/bin/env python3
import json
import re
from pathlib import Path

files = [
    "v1-messages-2026-08-18T100317-c7a408d6.log",
    "v1-messages-2026-08-18T100321-dbaf893a.log",
    "v1-messages-2026-08-18T100330-5c217a39.log",
    "v1-messages-2026-08-18T100347-879485a6.log",
    "v1-messages-2026-08-18T100349-101c8085.log",
    "v1-messages-2026-08-18T100352-9fa30c8e.log",
]
logdir = Path("/opt/cli-proxy-api/logs")

def split_sections(text: str) -> dict:
    parts = re.split(r"^=== ([^=]+) ===\s*$", text, flags=re.M)
    out = {}
    for i in range(1, len(parts), 2):
        out[parts[i].strip()] = parts[i + 1]
    return out

def event_types_from_sse(body: str):
    types = []
    for line in body.splitlines():
        line = line.strip()
        if line.startswith("event:"):
            types.append(line[6:].strip())
        elif line.startswith("data:"):
            data = line[5:].strip()
            if data and data != "[DONE]":
                try:
                    obj = json.loads(data)
                    t = obj.get("type")
                    if t:
                        types.append(f"data.type={t}")
                except Exception:
                    pass
    return types

def summarize_stream(label, body: str):
    body = body or ""
    # strip timestamp/header preamble if present
    idx = body.find("event:")
    idx2 = body.find("data:")
    start = -1
    if idx >= 0 and idx2 >= 0:
        start = min(idx, idx2)
    elif idx >= 0:
        start = idx
    elif idx2 >= 0:
        start = idx2
    sse = body[start:] if start >= 0 else body
    types = event_types_from_sse(sse)
    # counts
    from collections import Counter
    c = Counter(types)
    print(f"  {label}: bytes={len(body)} sse_bytes={len(sse)} unique_events={len(c)}")
    for k, v in c.most_common(30):
        print(f"    {v}x {k}")
    # completion markers
    markers = [
        "message_stop",
        "message_delta",
        "content_block_stop",
        "content_block_start",
        "message_start",
        "response.completed",
        "response.output_text.delta",
        "[DONE]",
        "error",
    ]
    for m in markers:
        if m in sse:
            print(f"    has {m}")
    # extract text deltas if claude
    texts = []
    for line in sse.splitlines():
        if not line.startswith("data:"):
            continue
        data = line[5:].strip()
        if not data or data == "[DONE]":
            continue
        try:
            obj = json.loads(data)
        except Exception:
            continue
        t = obj.get("type")
        if t == "content_block_delta":
            delta = obj.get("delta") or {}
            if delta.get("type") == "text_delta":
                texts.append(delta.get("text") or "")
            elif "thinking" in delta:
                texts.append(f"[thinking]{delta.get('thinking')}")
        elif t == "response.output_text.delta":
            texts.append(obj.get("delta") or "")
        elif t == "error" or obj.get("error"):
            print("    ERROR_OBJ", json.dumps(obj, ensure_ascii=False)[:500])
    joined = "".join(texts)
    print(f"    text_join={joined[:200]!r} len={len(joined)}")
    # last 15 lines
    lines = [ln for ln in sse.splitlines() if ln.strip()]
    print("    tail:")
    for ln in lines[-12:]:
        print("     ", ln[:220])

for name in files:
    p = logdir / name
    print("=" * 80)
    print(name, "exists" if p.exists() else "MISSING", p.stat().st_size if p.exists() else 0)
    if not p.exists():
        continue
    text = p.read_text(encoding="utf-8", errors="replace")
    sec = split_sections(text)
    info = sec.get("REQUEST INFO", "")
    print(info.strip()[:400])
    # user query snippet
    try:
        req = json.loads(sec.get("REQUEST BODY", "").strip())
        msgs = req.get("messages") or []
        for m in msgs:
            if m.get("role") == "user":
                c = m.get("content")
                if isinstance(c, str) and "user_query" in c:
                    print("user_query snippet:", c[c.find("user_query"):c.find("user_query")+120].replace("\n"," "))
                elif isinstance(c, list):
                    for part in c:
                        if isinstance(part, dict) and part.get("type") == "text":
                            t = part.get("text") or ""
                            if t.strip():
                                print("user text:", t[:120].replace("\n"," "))
    except Exception as e:
        print("req parse", e)

    api = sec.get("API RESPONSE 1", "")
    resp = sec.get("RESPONSE", "")
    summarize_stream("UPSTREAM/API RESPONSE 1", api)
    summarize_stream("DOWNSTREAM RESPONSE", resp)

    # status lines
    for m in re.finditer(r"Status:\s*(\d+)", text):
        pass
    statuses = re.findall(r"Status:\s*(\d+)", text)
    print("statuses", statuses)

    # network/error notes near end
    if "cancel" in text.lower() or "broken pipe" in text.lower() or "client disconnected" in text.lower():
        for ln in text.splitlines():
            l = ln.lower()
            if any(k in l for k in ("cancel", "broken pipe", "disconnected", "i/o timeout", "context canceled", "reset by peer")):
                print("NOTE", ln[:240])
