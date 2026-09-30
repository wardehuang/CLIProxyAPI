#!/usr/bin/env python3
import json
import re
from pathlib import Path

logdir = Path("/opt/cli-proxy-api/logs")

# Prefer newest request logs after deploy
candidates = sorted(
    [
        p
        for p in logdir.glob("v1-messages-2026-07-27T03*.log")
        if not p.name.endswith("-strip.log") and "mcp-schema" not in p.name
    ],
    key=lambda p: p.stat().st_mtime,
    reverse=True,
)[:20]


def sections(text: str):
    matches = list(re.finditer(r"^=== (.+?) ===\s*$", text, re.M))
    out = []
    for i, m in enumerate(matches):
        start = m.end()
        end = matches[i + 1].start() if i + 1 < len(matches) else len(text)
        out.append((m.group(1), text[start:end]))
    return out


def parse_json(text: str):
    start = text.find("{")
    if start < 0:
        return None
    depth = 0
    in_s = False
    esc = False
    for i in range(start, len(text)):
        ch = text[i]
        if in_s:
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == '"':
                in_s = False
            continue
        if ch == '"':
            in_s = True
            continue
        if ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth == 0:
                try:
                    return json.loads(text[start : i + 1])
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


def parse_sse(body: str):
    events = []
    event_name = None
    data_lines = []
    for line in body.splitlines():
        if line.startswith("event:"):
            event_name = line[6:].strip()
        elif line.startswith("data:"):
            data_lines.append(line[5:].lstrip())
        elif line.strip() == "":
            if data_lines:
                raw = "\n".join(data_lines)
                try:
                    events.append((event_name, json.loads(raw)))
                except Exception:
                    events.append((event_name, raw))
            event_name = None
            data_lines = []
    return events


interesting = []
for path in candidates:
    text = path.read_text(encoding="utf-8", errors="replace")
    if "mem-search" not in text and "CPA-Manager-Plus" not in text and "user-claude-mem-search" not in text:
        continue
    interesting.append(path)

print("interesting logs", len(interesting))
for path in interesting[:12]:
    text = path.read_text(encoding="utf-8", errors="replace")
    rid = path.name.split("-")[-1].replace(".log", "")
    print("=" * 72)
    print(path.name)
    print("has_user_claude_mem_search", "user-claude-mem-search" in text)
    print("has_INVALID", "INVALID_SEARCH" in text or "Either query or filters" in text)
    print("has_Error calling", "Error calling" in text or "Worker API" in text)
    # detail log
    details = list(logdir.glob(f"*{rid}*mcp-schema*"))
    print("detail", [p.name for p in details])

    for title, body in sections(text):
        if title == "REQUEST BODY":
            obj = parse_json(body)
            if not obj:
                print("REQUEST BODY parse fail")
                continue
            tools = obj.get("tools") or []
            names = [t.get("name") for t in tools if isinstance(t, dict)]
            mcp = [n for n in names if isinstance(n, str) and n.startswith("user-")]
            print(f"CLIENT tools={len(tools)} mcp={mcp}")
            # if mem-search in messages
            raw = json.dumps(obj, ensure_ascii=False)
            if "mem-search" in raw:
                print("  has mem-search in body")
            if "CPA-Manager-Plus" in raw:
                print("  has CPA-Manager-Plus")
        if title.startswith("API REQUEST"):
            api = body.split("\nBody:\n", 1)[-1]
            obj = parse_json(api)
            if not obj:
                continue
            tools = obj.get("tools") or []
            mcp_tools = []
            for t in tools:
                if not isinstance(t, dict):
                    continue
                name = t.get("name")
                schema = t.get("parameters") or t.get("input_schema") or {}
                props = schema.get("properties") if isinstance(schema, dict) else {}
                if isinstance(name, str) and name.startswith("user-"):
                    mcp_tools.append((name, len(props or {}), sorted((props or {}).keys())[:8]))
            print(f"UPSTREAM tools={len(tools)} mcp_count={len(mcp_tools)}")
            for name, nprops, keys in mcp_tools:
                print(f"  {name}: nprops={nprops} keys={keys}")
        if title.startswith("API RESPONSE") or title == "RESPONSE":
            calls = []
            if "event:" in body or "data:" in body:
                for _, payload in parse_sse(body):
                    walk_calls(payload, calls)
            else:
                obj = parse_json(body)
                if obj:
                    walk_calls(obj, calls)
            seen = set()
            uniq = []
            for c in calls:
                if c[0] == "tool_use" and (c[2] is None or c[2] == {}):
                    continue
                key = (c[1], json.dumps(c[2], sort_keys=True, default=str))
                if key in seen:
                    continue
                seen.add(key)
                if c[2] is None:
                    continue
                uniq.append(c)
            mcp_calls = [c for c in uniq if isinstance(c[1], str) and (c[1].startswith("user-") or "mem" in c[1] or c[1] in ("Read", "Glob", "Shell", "Grep"))]
            if mcp_calls:
                print(f"{title} calls:")
                for c in mcp_calls[:15]:
                    args = json.dumps(c[2], ensure_ascii=False, default=str)
                    if len(args) > 200:
                        args = args[:200] + "..."
                    print(f"  {c[0]} {c[1]} args={args}")

# Also scan journal-ish: print any error strings near mem search in newest logs
print("\n=== error snippets ===")
for path in interesting[:8]:
    text = path.read_text(encoding="utf-8", errors="replace")
    for pat in ["INVALID_SEARCH", "Either query", "Error calling", "Worker API", "tool error", "MCP error", "is not a function", "Unknown tool"]:
        if pat in text:
            idx = text.find(pat)
            print(path.name, pat, "=>", text[max(0, idx - 100) : idx + 200].replace("\n", " | "))
