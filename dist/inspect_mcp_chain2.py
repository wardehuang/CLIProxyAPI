#!/usr/bin/env python3
import json
import re
from pathlib import Path

ids = [
    "b6a43c69",
    "14fe34e6",
    "69416ffe",
    "b8b0a726",
    "e361edba",
    "b8740bad",
    "bea65019",
    "9b9c6b6d",
    "5ed33b59",
]
logdir = Path("/opt/cli-proxy-api/logs")
mcp_prefixes = ("user-claude-mem", "user-context-mode")
section_re = re.compile(r"^=== (.+?) ===\s*$", re.M)


def split_sections(text: str):
    matches = list(section_re.finditer(text))
    sections = []
    for index, match in enumerate(matches):
        title = match.group(1).strip()
        start = match.end()
        end = matches[index + 1].start() if index + 1 < len(matches) else len(text)
        sections.append((title, text[start:end]))
    return sections


def first_json_object(text: str):
    start = text.find("{")
    if start < 0:
        return None, "no-brace"
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
                raw = text[start : index + 1]
                try:
                    return json.loads(raw), None
                except Exception as error:
                    return None, f"json-error:{error}"
    return None, "unclosed"


def schema_summary(tool: dict):
    name = tool.get("name")
    schema = None
    field = None
    for key in ("input_schema", "parameters", "arguments", "inputSchema"):
        if key in tool:
            schema = tool[key]
            field = key
            break
    function_value = tool.get("function")
    if isinstance(function_value, dict):
        if not name:
            name = function_value.get("name")
        for key in ("parameters", "input_schema", "arguments"):
            if key in function_value:
                schema = function_value[key]
                field = "function." + key
                break
    properties = {}
    if isinstance(schema, dict):
        raw_properties = schema.get("properties")
        if isinstance(raw_properties, dict):
            properties = raw_properties
    property_names = sorted(properties.keys())
    return name, field, len(property_names) == 0, property_names, schema


def walk_calls(value, output):
    if isinstance(value, dict):
        value_type = value.get("type")
        name = value.get("name")
        if value_type == "tool_use" and isinstance(name, str):
            output.append(("tool_use", name, value.get("input")))
        if value_type in ("function_call", "custom_tool_call", "tool_call") and isinstance(name, str):
            arguments = value.get("arguments") or value.get("input")
            if isinstance(arguments, str):
                try:
                    arguments = json.loads(arguments)
                except Exception:
                    pass
            output.append((value_type, name, arguments))
        function_value = value.get("function")
        if isinstance(function_value, dict):
            function_name = function_value.get("name")
            if isinstance(function_name, str):
                arguments = function_value.get("arguments")
                if isinstance(arguments, str):
                    try:
                        arguments = json.loads(arguments)
                    except Exception:
                        pass
                output.append(("function", function_name, arguments))
        # SSE payload may embed json strings
        for child in value.values():
            walk_calls(child, output)
    elif isinstance(value, list):
        for child in value:
            walk_calls(child, output)
    elif isinstance(value, str):
        stripped = value.strip()
        if stripped.startswith("{") or stripped.startswith("["):
            try:
                parsed = json.loads(stripped)
            except Exception:
                return
            walk_calls(parsed, output)


def is_mcp(name: str) -> bool:
    return isinstance(name, str) and name.startswith(mcp_prefixes)


def parse_sse_events(body_text: str):
    events = []
    current_event = None
    data_lines = []
    for line in body_text.splitlines():
        if line.startswith("event:"):
            current_event = line[6:].strip()
        elif line.startswith("data:"):
            data_lines.append(line[5:].lstrip())
        elif line.strip() == "":
            if data_lines:
                raw = "\n".join(data_lines)
                try:
                    events.append((current_event, json.loads(raw)))
                except Exception:
                    events.append((current_event, raw))
            current_event = None
            data_lines = []
    if data_lines:
        raw = "\n".join(data_lines)
        try:
            events.append((current_event, json.loads(raw)))
        except Exception:
            events.append((current_event, raw))
    return events


for request_id in ids:
    print("=" * 72)
    print(request_id)
    files = sorted(logdir.glob(f"v1-messages-*-{request_id}.log"))
    request_logs = [
        path
        for path in files
        if not path.name.endswith("-strip.log") and "mcp-schema" not in path.name
    ]
    if not request_logs:
        print("  MISSING")
        continue
    text = request_logs[0].read_text(encoding="utf-8", errors="replace")
    print("  file", request_logs[0].name)
    detail = list(logdir.glob(f"*{request_id}*mcp-schema*"))
    print("  mcp-schema detail:", [p.name for p in detail] or "NONE")
    if "INVALID_SEARCH" in text or "Either query or filters" in text:
        print("  FAIL marker present in raw log")

    sections = split_sections(text)
    print("  sections:", [title for title, _ in sections])

    for title, body in sections:
        if title == "REQUEST BODY":
            obj, error = first_json_object(body)
            print("  REQUEST BODY parse:", "ok" if obj else error, "chars", len(body.strip()))
            if not obj:
                print("  REQUEST BODY head:", body.strip()[:200].replace("\n", "\\n"))
                continue
            tools = obj.get("tools") or []
            print("  client tools:", len(tools), "model:", obj.get("model"))
            for tool in tools:
                if not isinstance(tool, dict):
                    continue
                name, field, empty, props, schema = schema_summary(tool)
                if is_mcp(name):
                    print(f"    client MCP {name}: empty={empty} field={field} nprops={len(props)} props={props}")
                elif name in ("Read", "Shell"):
                    print(f"    client native {name}: empty={empty} nprops={len(props)}")

        if title.startswith("API REQUEST"):
            # Body: after headers
            body_marker = body.find("\nBody:\n")
            api_body = body[body_marker + 7 :] if body_marker >= 0 else body
            obj, error = first_json_object(api_body)
            print(f"  {title} parse:", "ok" if obj else error)
            if not obj:
                continue
            # tools may be under tools or under something else for responses API
            tools = obj.get("tools")
            if tools is None and isinstance(obj.get("input"), list):
                # responses API often has tools top-level still
                pass
            if isinstance(tools, list):
                print(f"  upstream tools: {len(tools)}")
                for tool in tools:
                    if not isinstance(tool, dict):
                        continue
                    name, field, empty, props, schema = schema_summary(tool)
                    if is_mcp(name):
                        print(
                            f"    upstream MCP {name}: empty={empty} field={field} nprops={len(props)} props={props}"
                        )
                    elif name in ("Read", "Shell"):
                        print(f"    upstream native {name}: empty={empty} nprops={len(props)}")
            else:
                # search raw for tool names in serialized form
                raw = json.dumps(obj, ensure_ascii=False)
                for prefix in mcp_prefixes:
                    if prefix in raw:
                        print(f"  upstream raw contains {prefix}")
                        break
                # try tools in nested places
                def find_tools(node, path=""):
                    found = []
                    if isinstance(node, dict):
                        if isinstance(node.get("tools"), list):
                            found.append((path + ".tools", node["tools"]))
                        for key, value in node.items():
                            found.extend(find_tools(value, path + "." + key))
                    elif isinstance(node, list):
                        for index, value in enumerate(node[:50]):
                            found.extend(find_tools(value, f"{path}[{index}]"))
                    return found

                nested = find_tools(obj)
                print("  upstream nested tools paths:", [p for p, _ in nested][:10])
                for path, tools_list in nested[:3]:
                    for tool in tools_list:
                        if not isinstance(tool, dict):
                            continue
                        name, field, empty, props, schema = schema_summary(tool)
                        if is_mcp(name):
                            print(
                                f"    {path} MCP {name}: empty={empty} field={field} nprops={len(props)} props={props}"
                            )

        if title.startswith("API RESPONSE") or title.startswith("RESPONSE BODY") or title == "RESPONSE":
            # May be SSE
            if "event:" in body or "data:" in body:
                events = parse_sse_events(body)
                calls = []
                for event_name, payload in events:
                    walk_calls(payload, calls)
                interesting = []
                seen = set()
                for call in calls:
                    if not is_mcp(call[1]):
                        continue
                    key = (call[0], call[1], json.dumps(call[2], sort_keys=True, default=str))
                    if key in seen:
                        continue
                    seen.add(key)
                    interesting.append(call)
                if interesting:
                    print(f"  {title} MCP calls ({len(interesting)}):")
                    for call in interesting[:30]:
                        args = json.dumps(call[2], ensure_ascii=False, default=str)
                        if len(args) > 220:
                            args = args[:220] + "..."
                        print(f"    {call[0]} {call[1]} args={args}")
            else:
                obj, error = first_json_object(body)
                if obj:
                    calls = []
                    walk_calls(obj, calls)
                    interesting = [c for c in calls if is_mcp(c[1])]
                    if interesting:
                        print(f"  {title} MCP calls:")
                        for call in interesting[:20]:
                            args = json.dumps(call[2], ensure_ascii=False, default=str)
                            if len(args) > 220:
                                args = args[:220] + "..."
                            print(f"    {call[0]} {call[1]} args={args}")
