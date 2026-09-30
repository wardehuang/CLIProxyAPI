#!/usr/bin/env python3
import json
import re
from pathlib import Path

files = [
    "v1-messages-2026-08-18T100317-c7a408d6.log",
    "v1-messages-2026-08-18T100321-dbaf893a.log",
    "v1-messages-2026-08-18T100330-5c217a39.log",
    "v1-messages-2026-08-18T100347-879485a6.log",
]
logdir = Path("/opt/cli-proxy-api/logs")

def split_sections(text: str) -> dict:
    parts = re.split(r"^=== ([^=]+) ===\s*$", text, flags=re.M)
    out = {}
    for i in range(1, len(parts), 2):
        out[parts[i].strip()] = parts[i + 1]
    return out

def summarize(path: Path) -> None:
    text = path.read_text(encoding="utf-8", errors="replace")
    sec = split_sections(text)
    print("=" * 80)
    print("FILE", path.name)
    # header-ish keys
    for key in [
        "REQUEST INFO",
        "REQUEST HEADERS",
        "RESPONSE HEADERS",
        "ERROR",
        "UPSTREAM ERROR",
        "API RESPONSE METADATA",
        "VERSION",
    ]:
        if key in sec:
            body = sec[key].strip()
            if len(body) > 1200:
                body = body[:1200] + "\n...[truncated]..."
            print(f"--- {key} ---")
            print(body)

    # request body summary
    req_raw = sec.get("REQUEST BODY", "").strip()
    if req_raw:
        try:
            req = json.loads(req_raw)
        except Exception as exc:
            print("REQUEST BODY parse fail", exc, "len", len(req_raw))
            req = None
        if isinstance(req, dict):
            model = req.get("model")
            stream = req.get("stream")
            msgs = req.get("messages") or []
            tools = req.get("tools") or []
            thinking = {k: req.get(k) for k in ("thinking", "reasoning", "reasoning_effort", "output_config") if k in req}
            print("REQ model=", model, "stream=", stream, "messages=", len(msgs), "tools=", len(tools), "thinking_fields=", thinking)
            roles = {}
            rc_count = 0
            tool_call_assistants = 0
            missing_rc = 0
            for m in msgs:
                role = (m.get("role") or "?").lower()
                roles[role] = roles.get(role, 0) + 1
                if role == "assistant":
                    has_tc = bool(m.get("tool_calls")) or (
                        isinstance(m.get("content"), list)
                        and any((p.get("type") if isinstance(p, dict) else None) in ("tool_use", "tool_call") for p in m.get("content") or [])
                    )
                    rc = m.get("reasoning_content")
                    # claude content thinking blocks
                    has_thinking_block = False
                    content = m.get("content")
                    if isinstance(content, list):
                        for p in content:
                            if isinstance(p, dict) and p.get("type") in ("thinking", "redacted_thinking"):
                                has_thinking_block = True
                                if str(p.get("thinking") or p.get("text") or "").strip():
                                    rc_count += 1
                    if isinstance(rc, str) and rc.strip():
                        rc_count += 1
                    if has_tc:
                        tool_call_assistants += 1
                        if not ((isinstance(rc, str) and rc.strip()) or has_thinking_block):
                            missing_rc += 1
            print("REQ roles=", roles, "assistant_rcish=", rc_count, "assistant_tool_turns=", tool_call_assistants, "missing_rc_tool_turns=", missing_rc)
            # print last 4 messages sketch
            print("REQ last messages:")
            for m in msgs[-6:]:
                role = m.get("role")
                content = m.get("content")
                if isinstance(content, str):
                    c = content[:100].replace("\n", " ")
                elif isinstance(content, list):
                    types = []
                    for p in content:
                        if isinstance(p, dict):
                            types.append(p.get("type") or "?")
                        else:
                            types.append(type(p).__name__)
                    c = "list:" + ",".join(types[:12])
                else:
                    c = repr(content)[:100]
                extra = []
                if m.get("tool_calls"):
                    extra.append(f"tool_calls={len(m.get('tool_calls') or [])}")
                if "reasoning_content" in m:
                    rc = m.get("reasoning_content")
                    extra.append(f"rc_len={len(rc) if isinstance(rc, str) else type(rc).__name__}")
                if m.get("tool_call_id"):
                    extra.append(f"tool_call_id={m.get('tool_call_id')}")
                print(f"  - {role} {c} {' '.join(extra)}")

    # upstream request
    up_req = sec.get("API REQUEST", "").strip() or sec.get("UPSTREAM REQUEST", "").strip() or sec.get("TRANSLATED REQUEST", "").strip()
    for cand in ["API REQUEST BODY", "UPSTREAM REQUEST BODY", "TRANSLATED REQUEST BODY", "REQUEST TO PROVIDER"]:
        if cand in sec:
            up_req = sec[cand].strip()
            print("UPSTREAM KEY", cand)
            break
    # scan all section names
    print("SECTIONS", list(sec.keys()))

    # response / error body
    for key in sec:
        kl = key.lower()
        if any(x in kl for x in ("error", "response body", "upstream", "api response", "provider")):
            body = sec[key].strip()
            if not body:
                continue
            print(f"--- {key} (len={len(body)}) ---")
            # try json extract message
            try:
                obj = json.loads(body)
                print(json.dumps(obj, ensure_ascii=False)[:2000])
            except Exception:
                print(body[:2000])
                if len(body) > 2000:
                    print("...[truncated]...")

    # search keywords in whole file
    for kw in [
        "reasoning_content",
        "invalid_request_error",
        "must be passed back",
        "deepseek",
        "injected",
        "cpa-deepseek",
        "status",
        "400",
        "429",
        "500",
        "retry",
        "timeout",
        "canceled",
        "cancelled",
    ]:
        cnt = len(re.findall(re.escape(kw), text, flags=re.I))
        if cnt:
            print(f"kw {kw}={cnt}")

for name in files:
    p = logdir / name
    if not p.exists():
        print("MISSING", name)
        continue
    summarize(p)

# also list nearby logs same minute
print("=" * 80)
print("NEARBY")
for p in sorted(logdir.glob("v1-messages-2026-08-18T1003*.log")):
    print(p.name, p.stat().st_size)
