#!/usr/bin/env python3
import json
import re
import subprocess
from pathlib import Path

# Get journal lines
cmd = [
    "sudo",
    "journalctl",
    "-u",
    "cli-proxy-api",
    "--since",
    "2026-07-27 03:28:00",
    "--until",
    "2026-07-27 03:40:00",
    "--no-pager",
]
out = subprocess.check_output(cmd, text=True, errors="replace")
print("=== journal inject summary ===")
for line in out.splitlines():
    if "mcp tool schemas patched" not in line:
        continue
    # extract fields json after fields=
    m = re.search(r"fields=(\{.*\})(?: model=|$)", line)
    if not m:
        # try raw
        print(line[:300])
        continue
    try:
        # fields may not be pure json due to single quotes? it looks like json with double quotes
        fields_raw = m.group(1)
        # journal uses Go fmt map which might be invalid json - use regex
    except Exception:
        pass
    rid = re.search(r'"request_id":"([^"]*)"', line)
    stage = re.search(r'"stage":"([^"]*)"', line)
    inj = re.search(r'"injected_tools":(\[[^\]]*\]|null)', line)
    pat = re.search(r'"patched_tools":(\[[^\]]*\]|null)', line)
    before = re.search(r'"before_bytes":(\d+)', line)
    after = re.search(r'"after_bytes":(\d+)', line)
    print(
        f"rid={rid.group(1) if rid else '-'} stage={stage.group(1) if stage else '-'} "
        f"before={before.group(1) if before else '-'} after={after.group(1) if after else '-'} "
        f"injected={inj.group(1) if inj else '-'} patched={pat.group(1) if pat else '-'}"
    )

logdir = Path("/opt/cli-proxy-api/logs")
print("\n=== newest logs listing ===")
logs = sorted(logdir.glob("v1-messages-*.log"), key=lambda p: p.stat().st_mtime, reverse=True)
for p in logs[:30]:
    if p.name.endswith("-strip.log"):
        continue
    print(p.name, p.stat().st_size, p.stat().st_mtime)


def sections(text):
    ms = list(re.finditer(r"^=== (.+?) ===\s*$", text, re.M))
    out = []
    for i, m in enumerate(ms):
        start = m.end()
        end = ms[i + 1].start() if i + 1 < len(ms) else len(text)
        out.append((m.group(1), text[start:end]))
    return out


def parse_json(text):
    s = text.find("{")
    if s < 0:
        return None
    depth = 0
    ins = False
    esc = False
    for i in range(s, len(text)):
        ch = text[i]
        if ins:
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == '"':
                ins = False
            continue
        if ch == '"':
            ins = True
            continue
        if ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth == 0:
                try:
                    return json.loads(text[s : i + 1])
                except Exception:
                    return None
    return None


def walk_calls(value, out):
    if isinstance(value, dict):
        t = value.get("type")
        name = value.get("name")
        if t in ("function_call", "tool_use", "custom_tool_call", "tool_call") and isinstance(name, str):
            args = value.get("arguments") if "arguments" in value else value.get("input")
            if isinstance(args, str):
                try:
                    args = json.loads(args)
                except Exception:
                    pass
            out.append((t, name, args))
        for child in value.values():
            walk_calls(child, out)
    elif isinstance(value, list):
        for child in value:
            walk_calls(child, out)


def parse_sse(body):
    events = []
    data_lines = []
    for line in body.splitlines():
        if line.startswith("data:"):
            data_lines.append(line[5:].lstrip())
        elif line.strip() == "":
            if data_lines:
                raw = "\n".join(data_lines)
                try:
                    events.append(json.loads(raw))
                except Exception:
                    pass
            data_lines = []
    return events


print("\n=== scan newest 25 request logs for mem-search / MCP calls ===")
count = 0
for p in logs:
    if p.name.endswith("-strip.log") or "mcp-schema" in p.name:
        continue
    text = p.read_text(encoding="utf-8", errors="replace")
    # only after deploy roughly - file mtime or name
    if "2026-07-27T0326" <= p.name or True:
        pass
    interesting = (
        "mem-search" in text
        or "CPA-Manager-Plus" in text
        or "user-claude-mem-search" in text
    )
    if not interesting:
        continue
    # Focus on logs after 03:26
    if not re.search(r"2026-07-27T03(2[6-9]|[3-5]\d)", p.name):
        # still include if has actual tool call not just handoff doc
        if "function_call" in text and "user-claude-mem-search" in text:
            pass
        else:
            continue

    count += 1
    if count > 20:
        break
    print("=" * 72)
    print(p.name)

    # Extract REQUEST INFO timestamp
    for line in text.splitlines()[:10]:
        if line.startswith("Timestamp:") or line.startswith("URL:"):
            print(" ", line)

    for title, body in sections(text):
        if title == "REQUEST BODY":
            obj = parse_json(body)
            if obj:
                tools = obj.get("tools") or []
                names = [t.get("name") for t in tools if isinstance(t, dict)]
                mcp = [n for n in names if isinstance(n, str) and n.startswith("user-")]
                print(f"  CLIENT tools={len(tools)} mcp_in_client={mcp}")
        if title.startswith("API REQUEST"):
            api = body.split("\nBody:\n", 1)[-1]
            has_search = "user-claude-mem-search" in api
            print(f"  {title} upstream_raw_has_search={has_search} chars={len(api.strip())}")
            obj = parse_json(api)
            if obj and isinstance(obj.get("tools"), list):
                names = []
                for t in obj["tools"]:
                    if not isinstance(t, dict):
                        continue
                    n = t.get("name")
                    if isinstance(t.get("function"), dict):
                        n = n or t["function"].get("name")
                    names.append(n)
                mcp = [n for n in names if isinstance(n, str) and n.startswith("user-")]
                print(f"    parsed tools={len(names)} mcp={mcp}")
        if title.startswith("API RESPONSE") or title == "RESPONSE":
            calls = []
            if "event:" in body or "data:" in body:
                for payload in parse_sse(body):
                    walk_calls(payload, calls)
            # dedupe with args
            seen = set()
            for c in calls:
                if c[2] is None:
                    continue
                key = (c[0], c[1], json.dumps(c[2], sort_keys=True, default=str))
                if key in seen:
                    continue
                seen.add(key)
                name = c[1]
                if not isinstance(name, str):
                    continue
                if name.startswith("user-") or name in ("Read", "Glob", "Shell", "Grep"):
                    args = json.dumps(c[2], ensure_ascii=False, default=str)
                    if len(args) > 180:
                        args = args[:180] + "..."
                    print(f"  {title} {c[0]} {name} args={args}")

    # Look for tool_result errors in messages history (actual runtime, not handoff doc)
    # Prefer short error lines
    for m in re.finditer(r"Error calling Worker API[^\n]{0,200}", text):
        snip = m.group(0)
        # skip if clearly inside handoff markdown large context - check nearby for Handoff
        start = max(0, m.start() - 80)
        ctx = text[start : m.start()]
        if "Handoff" in ctx or "验收标准" in ctx or "不要改 claude-mem" in ctx:
            continue
        print("  LIVE_ERROR:", snip)

    for m in re.finditer(r"user-claude-mem-search", text):
        snip = text[max(0, m.start() - 120) : m.start() + 200].replace("\n", " | ")
        if "Handoff" in snip or "input_schema" in snip and "验收" in text[max(0, m.start() - 500) : m.start()]:
            continue
        if "function_call" in snip or "tool_use" in snip or "name" in snip:
            print("  search_ctx:", snip[:280])
            break
