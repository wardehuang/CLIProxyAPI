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


def tool_name_and_props(tool: dict):
    name = tool.get("name")
    schema = None
    if isinstance(tool.get("function"), dict):
        function_value = tool["function"]
        name = name or function_value.get("name")
        schema = function_value.get("parameters") or function_value.get("input_schema")
    if schema is None:
        schema = tool.get("input_schema") or tool.get("parameters") or tool.get("arguments")
    properties = {}
    if isinstance(schema, dict):
        raw = schema.get("properties")
        if isinstance(raw, dict):
            properties = raw
    return name, sorted(properties.keys()), schema


def walk(value, output):
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
        if isinstance(value.get("function"), dict):
            function_value = value["function"]
            function_name = function_value.get("name")
            if isinstance(function_name, str):
                arguments = function_value.get("arguments")
                if isinstance(arguments, str):
                    try:
                        arguments = json.loads(arguments)
                    except Exception:
                        pass
                output.append(("function", function_name, arguments))
        for child in value.values():
            walk(child, output)
    elif isinstance(value, list):
        for child in value:
            walk(child, output)


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


for request_id in ids:
    print("=" * 72)
    print(request_id)
    log_path = next(logdir.glob(f"v1-messages-*-{request_id}.log"))
    # skip strip
    candidates = [
        path
        for path in logdir.glob(f"v1-messages-*-{request_id}.log")
        if not path.name.endswith("-strip.log")
    ]
    log_path = candidates[0]
    text = log_path.read_text(encoding="utf-8", errors="replace")
    print("file", log_path.name)
    print("has_INVALID", ("INVALID_SEARCH" in text) or ("Either query or filters" in text))
    print("has_user_claude_text", "user-claude" in text)
    print("has_mcp_schema_detail", bool(list(logdir.glob(f"*{request_id}*mcp-schema*"))))

    for title, body in sections(text):
        if title == "REQUEST BODY":
            obj = parse_json(body)
            tools = obj.get("tools") or []
            print(f"CLIENT tools={len(tools)} model={obj.get('model')}")
            for tool in tools:
                name, props, schema = tool_name_and_props(tool)
                empty = len(props) == 0
                flag = ""
                if isinstance(name, str) and (
                    name.startswith("user-")
                    or "mem" in name
                    or "context" in name
                    or "claude" in name.lower()
                ):
                    flag = " <MCP?>"
                elif name in ("Read", "Shell", "Grep", "Glob"):
                    flag = " <native>"
                print(f"  {name}: empty={empty} nprops={len(props)} props={props[:10]}{flag}")

        if title.startswith("API REQUEST"):
            api_body = body.split("\nBody:\n", 1)[-1]
            obj = parse_json(api_body)
            tools = obj.get("tools") or []
            print(f"UPSTREAM {title} tools={len(tools)}")
            for tool in tools:
                name, props, schema = tool_name_and_props(tool)
                print(
                    f"  type={tool.get('type')} name={name} empty={len(props)==0} nprops={len(props)} props={props[:10]} keys={list(tool.keys())}"
                )

        if title.startswith("API RESPONSE") or title == "RESPONSE":
            calls = []
            if "event:" in body or "data:" in body:
                for event_name, payload in parse_sse(body):
                    walk(payload, calls)
            else:
                obj = parse_json(body)
                if obj:
                    walk(obj, calls)
            interesting = []
            seen = set()
            for call in calls:
                name = call[1]
                if not isinstance(name, str):
                    continue
                if not (
                    name.startswith("user-")
                    or "mem" in name
                    or "context" in name
                    or name in ("Read", "Shell", "Grep", "Glob", "SemanticSearch")
                ):
                    continue
                key = (call[0], name, json.dumps(call[2], sort_keys=True, default=str))
                if key in seen:
                    continue
                seen.add(key)
                interesting.append(call)
            if interesting:
                print(f"{title} calls ({len(interesting)}):")
                for call in interesting[:40]:
                    args = json.dumps(call[2], ensure_ascii=False, default=str)
                    if len(args) > 240:
                        args = args[:240] + "..."
                    print(f"  {call[0]} {call[1]} args={args}")
