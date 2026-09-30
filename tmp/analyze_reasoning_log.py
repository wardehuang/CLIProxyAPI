#!/usr/bin/env python3
import json
import re
import sys
from collections import Counter
from pathlib import Path


def main() -> int:
    path = Path(sys.argv[1] if len(sys.argv) > 1 else "/opt/cli-proxy-api/logs/v1-responses-2026-08-18T033329-d29fe6c2.log")
    text = path.read_text(encoding="utf-8", errors="replace")
    parts = re.split(r"^=== ([^=]+) ===\s*$", text, flags=re.M)
    sec = {parts[i].strip(): parts[i + 1] for i in range(1, len(parts), 2)}
    print("SECTIONS", list(sec))

    req = json.loads(sec["REQUEST BODY"].strip())
    api_sec = sec["API REQUEST 1"]
    body_lines = [ln for ln in api_sec.splitlines() if ln.startswith("{") and '"model"' in ln]
    print("body_lines", len(body_lines))
    api_body = json.loads(body_lines[0])

    print("CLIENT model", req.get("model"))
    print("CLIENT include", req.get("include"))
    print("CLIENT reasoning", req.get("reasoning"))
    print("CLIENT stream", req.get("stream"))
    print("CLIENT input count", len(req.get("input", [])))

    c = Counter()
    for i, it in enumerate(req.get("input", [])):
        t = it.get("type") or ("role:" + str(it.get("role")))
        c[t] += 1
        extra = []
        if "reasoning_content" in it:
            extra.append("has_rc_key=" + str(bool(it.get("reasoning_content"))))
        if it.get("type") == "reasoning":
            extra.append("summary=" + str(bool(it.get("summary"))))
            extra.append("enc=" + str(bool(it.get("encrypted_content"))))
        preview = ""
        if it.get("role") and it.get("type") in (None, "message"):
            content = it.get("content")
            if isinstance(content, str):
                preview = content[:90].replace("\n", " ")
            elif isinstance(content, list):
                preview = str([x.get("type") for x in content])[:90]
        elif it.get("type") == "function_call":
            preview = f"{it.get('name')} id={str(it.get('call_id', ''))[:40]}"
        elif it.get("type") == "function_call_output":
            preview = f"out id={str(it.get('call_id', ''))[:40]} len={len(str(it.get('output', '')))}"
        elif it.get("type") == "reasoning":
            preview = str({k: it.get(k) for k in it if k != "summary"})[:120]
        print(f"IN[{i}] type={it.get('type')} role={it.get('role')} extra={extra} :: {preview}")

    print("INPUT TYPE COUNTS", dict(c))
    print("any reasoning item?", any(it.get("type") == "reasoning" for it in req["input"]))
    print("any reasoning_content in input json?", "reasoning_content" in json.dumps(req["input"], ensure_ascii=False))

    print("\nUPSTREAM model", api_body.get("model"))
    print("UPSTREAM keys", sorted(api_body.keys()))
    print("UPSTREAM reasoning_effort", api_body.get("reasoning_effort"))
    print("UPSTREAM messages", len(api_body.get("messages", [])))

    rc_count = 0
    bad = []
    for i, m in enumerate(api_body["messages"]):
        role = m.get("role")
        has_rc = "reasoning_content" in m
        rc = m.get("reasoning_content")
        if has_rc:
            rc_count += 1
        tc = m.get("tool_calls")
        content = m.get("content")
        cprev = ""
        if isinstance(content, str):
            cprev = content[:70].replace("\n", " ")
        elif content is not None:
            cprev = str(type(content))
        extra = []
        if has_rc:
            extra.append("rc_len=" + str(len(str(rc)) if rc is not None else "None"))
        if tc:
            names = [t.get("function", {}).get("name") for t in tc]
            extra.append(f"tool_calls={len(tc)} names={names}")
        if m.get("tool_call_id"):
            extra.append("tool_call_id=" + str(m.get("tool_call_id"))[:40])
        print(f"UP[{i}] {role} extra={extra} content={cprev!r}")
        if role == "assistant" and tc and not rc:
            bad.append(i)

    print("upstream messages with reasoning_content key", rc_count)
    print("assistant+tool_calls missing reasoning_content idxs", bad)

    print("\nAPI REQUEST HEAD excerpt:")
    for ln in api_sec.splitlines()[:45]:
        if ln.startswith("{") and '"model"' in ln:
            break
        print(ln[:220])

    if "API ERROR RESPONSE" in sec:
        print("\nERROR SECTION:")
        print(sec["API ERROR RESPONSE"][:800])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
