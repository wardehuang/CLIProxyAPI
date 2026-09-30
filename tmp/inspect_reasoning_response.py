#!/usr/bin/env python3
import json
import re
from pathlib import Path

files = [
    "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T033314-881e1513.log",
    "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T033323-85fb0cd3.log",
    "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T033329-d29fe6c2.log",
    "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T033251-7e9908da.log",
    "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T033256-891913d1.log",
    "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T033300-2b079479.log",
]


def sections(text: str):
    parts = re.split(r"^=== ([^=]+) ===\s*$", text, flags=re.M)
    return {parts[i].strip(): parts[i + 1] for i in range(1, len(parts), 2)}


for path in files:
    p = Path(path)
    text = p.read_text(encoding="utf-8", errors="replace")
    sec = sections(text)
    resp = sec.get("RESPONSE", "") + "\n" + sec.get("API RESPONSE 1", "")
    # count reasoning-related events/fields
    keys = [
        "reasoning_content",
        "reasoning_summary_text",
        "response.reasoning",
        '"type":"reasoning"',
        "encrypted_content",
        "must be passed back",
        "tool_calls",
        "function_call",
    ]
    print("=" * 72)
    print(p.name, "size", p.stat().st_size)
    for k in keys:
        print(f"  count[{k}]={resp.count(k)} full={text.count(k)}")
    # sample first few SSE lines containing reasoning
    samples = []
    for ln in resp.splitlines():
        if "reasoning" in ln.lower() and ("delta" in ln or "content" in ln or "summary" in ln):
            samples.append(ln[:220])
            if len(samples) >= 5:
                break
    print("  samples:")
    for s in samples:
        print("   ", s)
    # if nonstream body JSON
    body_json_lines = [ln for ln in resp.splitlines() if ln.startswith("{") and "choices" in ln]
    if body_json_lines:
        try:
            j = json.loads(body_json_lines[0])
            msg = j.get("choices", [{}])[0].get("message", {})
            print("  nonstream rc?", "reasoning_content" in msg, "rc_len", len(str(msg.get("reasoning_content") or "")))
        except Exception as e:
            print("  parse body fail", e)
