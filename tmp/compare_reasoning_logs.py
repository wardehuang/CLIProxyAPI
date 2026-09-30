#!/usr/bin/env python3
import json
import re
from pathlib import Path

files = [
    "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T033323-85fb0cd3.log",
    "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T033314-881e1513.log",
    "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T033518-42ae0309.log",
    "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T033329-d29fe6c2.log",
    "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T034933-f15f9952.log",
]


def sections(text: str):
    parts = re.split(r"^=== ([^=]+) ===\s*$", text, flags=re.M)
    return {parts[i].strip(): parts[i + 1] for i in range(1, len(parts), 2)}


def first_json_with_model(sec: str):
    for ln in sec.splitlines():
        if ln.startswith("{") and '"model"' in ln:
            return json.loads(ln)
    return None


for path in files:
    p = Path(path)
    if not p.exists():
        print("MISSING", path)
        continue
    text = p.read_text(encoding="utf-8", errors="replace")
    sec = sections(text)
    req = json.loads(sec["REQUEST BODY"].strip())
    api = first_json_with_model(sec.get("API REQUEST 1", ""))
    err = "must be passed back" in text
    print("=" * 80)
    print(p.name, "size", p.stat().st_size, "err", err)
    print("client model", req.get("model"))
    print("client reasoning", req.get("reasoning"))
    print("client include", req.get("include"))
    print("client stream", req.get("stream"))
    print("input len", len(req.get("input", [])))
    types = {}
    rc_in = 0
    reasoning_items = 0
    for it in req.get("input", []):
        t = it.get("type") or ("role:" + str(it.get("role")))
        types[t] = types.get(t, 0) + 1
        if it.get("type") == "reasoning":
            reasoning_items += 1
        if it.get("reasoning_content"):
            rc_in += 1
    print("input types", types)
    print("reasoning items", reasoning_items, "input rc fields", rc_in)
    if api:
        print("up model", api.get("model"), "effort", api.get("reasoning_effort"))
        msgs = api.get("messages", [])
        print("up msgs", len(msgs))
        asst_tc = 0
        asst_tc_rc = 0
        asst_rc = 0
        for m in msgs:
            if m.get("role") != "assistant":
                continue
            if m.get("reasoning_content"):
                asst_rc += 1
            if m.get("tool_calls"):
                asst_tc += 1
                if m.get("reasoning_content"):
                    asst_tc_rc += 1
        print("asst with rc", asst_rc, "asst+tc", asst_tc, "asst+tc+rc", asst_tc_rc)
        # upstream url from API REQUEST 1
        for ln in sec.get("API REQUEST 1", "").splitlines():
            if ln.startswith("Upstream URL:") or ln.startswith("Auth:"):
                print(ln[:220])
    # response status
    for key in ("API ERROR RESPONSE", "API RESPONSE 1", "RESPONSE"):
        if key in sec:
            head = "\n".join(sec[key].strip().splitlines()[:6])
            print(key, "=>", head[:300].replace("\n", " | "))
