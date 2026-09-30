#!/usr/bin/env python3
import json
import re
from pathlib import Path

ids = ["b6a43c69", "14fe34e6", "69416ffe", "b8b0a726", "e361edba", "b8740bad", "bea65019", "9b9c6b6d", "5ed33b59"]
logdir = Path("/opt/cli-proxy-api/logs")


def sections(text: str):
    matches = list(re.finditer(r"^=== (.+?) ===\s*$", text, re.M))
    out = []
    for index, match in enumerate(matches):
        start = match.end()
        end = matches[index + 1].start() if index + 1 < len(matches) else len(text)
        out.append((match.group(1), text[start:end]))
    return out


def parse_json(text: str):
    start = text.find("{")
    if start < 0:
        return None
    depth = 0
    in_string = False
    escape = False
    for index in range(start, len(text)):
        char = text[index]
        if in_string:
            if escape:
                escape = False
            elif char == "\\":
                escape = True
            elif char == '"':
                in_string = False
            continue
        if char == '"':
            in_string = True
            continue
        if char == "{":
            depth += 1
        elif char == "}":
            depth -= 1
            if depth == 0:
                return json.loads(text[start : index + 1])
    return None


def walk_calls(value, output):
    if isinstance(value, dict):
        value_type = value.get("type")
        name = value.get("name")
        if value_type in ("function_call", "tool_use", "custom_tool_call", "tool_call") and isinstance(name, str):
            arguments = value.get("arguments") or value.get("input")
            if isinstance(arguments, str):
                try:
                    arguments = json.loads(arguments)
                except Exception:
                    pass
            output.append((value_type, name, arguments))
        for child in value.values():
            walk_calls(child, output)
    elif isinstance(value, list):
        for child in value:
            walk_calls(child, output)


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


def summarize_messages(obj):
    messages = obj.get("messages") or []
    print(f"  messages={len(messages)}")
    for index, message in enumerate(messages[-6:]):
        role = message.get("role")
        content = message.get("content")
        preview = ""
        tool_uses = []
        tool_results = []
        if isinstance(content, str):
            preview = content[:160].replace("\n", " ")
        elif isinstance(content, list):
            texts = []
            for block in content:
                if not isinstance(block, dict):
                    continue
                block_type = block.get("type")
                if block_type == "text":
                    texts.append((block.get("text") or "")[:120])
                elif block_type == "tool_use":
                    tool_uses.append((block.get("name"), block.get("input")))
                elif block_type == "tool_result":
                    result_content = block.get("content")
                    if isinstance(result_content, str):
                        tool_results.append(result_content[:120].replace("\n", " "))
                    else:
                        tool_results.append(str(result_content)[:120])
            preview = " | ".join(texts)[:200]
        print(f"  msg[-{len(messages)-index}] role={role} preview={preview!r}")
        for name, inp in tool_uses[:5]:
            args = json.dumps(inp, ensure_ascii=False, default=str)
            if len(args) > 180:
                args = args[:180] + "..."
            print(f"    hist tool_use {name} {args}")
        for result in tool_results[:3]:
            print(f"    hist tool_result {result!r}")


for request_id in ids:
    print("=" * 72)
    print(request_id)
    log_path = [
        path
        for path in logdir.glob(f"v1-messages-*-{request_id}.log")
        if not path.name.endswith("-strip.log")
    ][0]
    text = log_path.read_text(encoding="utf-8", errors="replace")

    # where user-claude appears
    positions = [match.start() for match in re.finditer("user-claude", text)]
    print("user-claude count", len(positions))
    for pos in positions[:3]:
        print("  context:", text[max(0, pos - 80) : pos + 120].replace("\n", " | "))

    for title, body in sections(text):
        if title == "REQUEST BODY":
            obj = parse_json(body)
            summarize_messages(obj)
            tool_names = []
            for tool in obj.get("tools") or []:
                tool_names.append(tool.get("name"))
            print("  tools_list:", tool_names)
            mcp_like = [name for name in tool_names if isinstance(name, str) and (name.startswith("user-") or "mcp" in name.lower())]
            print("  mcp_like_tools:", mcp_like or "NONE")

        if title.startswith("API RESPONSE") or title == "RESPONSE":
            calls = []
            if "event:" in body or "data:" in body:
                for _, payload in parse_sse(body):
                    walk_calls(payload, calls)
            interesting = []
            seen = set()
            for call in calls:
                key = (call[0], call[1], json.dumps(call[2], sort_keys=True, default=str))
                if key in seen:
                    continue
                seen.add(key)
                interesting.append(call)
            print(f"  {title} all function/tool calls ({len(interesting)}):")
            for call in interesting[:20]:
                args = json.dumps(call[2], ensure_ascii=False, default=str)
                if len(args) > 200:
                    args = args[:200] + "..."
                print(f"    {call[0]} {call[1]} args={args}")
