#!/usr/bin/env python3
import json
import re
from pathlib import Path

IDS = ["753a0916", "cc290ea0", "71edf8f7", "467ac32e"]
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
                try:
                    return json.loads(text[start : index + 1])
                except Exception:
                    return None
    return None


def tool_summary(tool: dict):
    name = tool.get("name")
    schema = tool.get("input_schema") or tool.get("parameters")
    if isinstance(tool.get("function"), dict):
        name = name or tool["function"].get("name")
        schema = tool["function"].get("parameters") or schema
    props = {}
    if isinstance(schema, dict):
        raw = schema.get("properties")
        if isinstance(raw, dict):
            props = raw
    return name, sorted(props.keys()), len(props)


def walk_calls(value, out):
    if isinstance(value, dict):
        value_type = value.get("type")
        name = value.get("name")
        if value_type in ("function_call", "tool_use", "custom_tool_call", "tool_call") and isinstance(name, str):
            args = value.get("arguments") if "arguments" in value else value.get("input")
            if isinstance(args, str):
                try:
                    args = json.loads(args)
                except Exception:
                    pass
            out.append((value_type, name, args, value.get("call_id") or value.get("id")))
        if isinstance(value.get("function"), dict):
            function_name = value["function"].get("name")
            if isinstance(function_name, str):
                args = value["function"].get("arguments")
                if isinstance(args, str):
                    try:
                        args = json.loads(args)
                    except Exception:
                        pass
                out.append(("function", function_name, args, None))
        for child in value.values():
            walk_calls(child, out)
    elif isinstance(value, list):
        for child in value:
            walk_calls(child, out)


def parse_sse(body: str):
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


def walk_history(value):
    if isinstance(value, dict):
        value_type = value.get("type")
        if value_type == "tool_use":
            yield ("tool_use", value.get("name"), value.get("input"), value.get("id"))
        if value_type == "tool_result":
            content = value.get("content")
            if isinstance(content, list):
                texts = []
                for block in content:
                    if isinstance(block, dict) and block.get("type") == "text":
                        texts.append(block.get("text") or "")
                    elif isinstance(block, str):
                        texts.append(block)
                content = "\n".join(texts)
            yield ("tool_result", value.get("tool_use_id"), content, value.get("is_error"))
        for child in value.values():
            yield from walk_history(child)
    elif isinstance(value, list):
        for child in value:
            yield from walk_history(child)


for request_id in IDS:
    print("=" * 80)
    print("REQUEST", request_id)
    files = sorted(logdir.glob(f"*{request_id}*"))
    print("files:", [path.name for path in files] or "NONE")
    request_logs = [
        path
        for path in files
        if path.name.startswith("v1-messages-")
        and not path.name.endswith("-strip.log")
        and "mcp-schema" not in path.name
    ]
    if not request_logs:
        print("  no request log")
        continue
    path = request_logs[0]
    text = path.read_text(encoding="utf-8", errors="replace")
    print("log", path.name, "bytes", len(text))
    for line in text.splitlines()[:12]:
        if line.startswith(("URL:", "Timestamp:", "Version:", "Method:")):
            print(" ", line)

    detail = [path.name for path in files if "mcp-schema" in path.name]
    print("mcp-schema detail:", detail or "NONE")

    for title, body in sections(text):
        if title == "REQUEST BODY":
            obj = parse_json(body)
            if not obj:
                print("  REQUEST BODY parse fail")
                continue
            tools = obj.get("tools") or []
            names = []
            mcp = []
            for tool in tools:
                if not isinstance(tool, dict):
                    continue
                name, props, nprops = tool_summary(tool)
                names.append(name)
                if isinstance(name, str) and name.startswith("user-"):
                    mcp.append((name, nprops, props[:12]))
            print(f"  CLIENT tools={len(tools)} mcp={mcp}")
            # history tool uses/results
            events = list(walk_history(obj))
            for event in events:
                if event[0] == "tool_use" and isinstance(event[1], str) and (
                    event[1].startswith("user-") or "mem" in event[1] or event[1] in ("Read", "Glob", "Shell", "Grep")
                ):
                    print(
                        "  HIST tool_use",
                        event[1],
                        "id=",
                        event[3],
                        "input=",
                        json.dumps(event[2], ensure_ascii=False, default=str)[:220],
                    )
                if event[0] == "tool_result":
                    content = event[2]
                    if not isinstance(content, str):
                        content = str(content)
                    interesting = any(
                        marker in content
                        for marker in [
                            "Error",
                            "error",
                            "INVALID",
                            "failed",
                            "Failed",
                            "Unknown",
                            "MCP",
                            "not found",
                            "Worker",
                        ]
                    )
                    if interesting or (isinstance(event[1], str) and "search" in str(event[1])):
                        print(
                            "  HIST tool_result id=",
                            event[1],
                            "is_error=",
                            event[3],
                            "content=",
                            content[:350].replace("\n", " | "),
                        )

        if title.startswith("API REQUEST"):
            api_body = body.split("\nBody:\n", 1)[-1]
            print(f"  {title} upstream_has_search={('user-claude-mem-search' in api_body)} chars={len(api_body.strip())}")
            obj = parse_json(api_body)
            if not obj:
                print("    parse fail; head=", api_body[:200].replace("\n", " "))
                continue
            tools = obj.get("tools")
            if isinstance(tools, list):
                mcp = []
                for tool in tools:
                    if not isinstance(tool, dict):
                        continue
                    name, props, nprops = tool_summary(tool)
                    if isinstance(name, str) and name.startswith("user-"):
                        mcp.append((name, nprops, props[:10], list(tool.keys())))
                print(f"    parsed tools={len(tools)} mcp_count={len(mcp)}")
                for item in mcp:
                    print(f"      {item[0]} nprops={item[1]} props={item[2]} keys={item[3]}")
            else:
                # nested search
                raw = json.dumps(obj, ensure_ascii=False)
                print("    tools field type", type(tools).__name__, "raw has search", "user-claude-mem-search" in raw)
                # show tool shape samples around search
                idx = raw.find("user-claude-mem-search")
                if idx >= 0:
                    print("    raw context:", raw[max(0, idx - 120) : idx + 250])

        if title.startswith("API RESPONSE") or title == "RESPONSE":
            calls = []
            if "event:" in body or "data:" in body:
                for payload in parse_sse(body):
                    walk_calls(payload, calls)
            else:
                obj = parse_json(body)
                if obj:
                    walk_calls(obj, calls)
            seen = set()
            for call in calls:
                if call[2] is None and call[0] != "tool_use":
                    # keep null only for naming
                    pass
                key = (call[0], call[1], json.dumps(call[2], sort_keys=True, default=str))
                if key in seen:
                    continue
                seen.add(key)
                name = call[1]
                if not isinstance(name, str):
                    continue
                if name.startswith("user-") or name in ("Read", "Glob", "Shell", "Grep", "Task"):
                    args = json.dumps(call[2], ensure_ascii=False, default=str)
                    if len(args) > 240:
                        args = args[:240] + "..."
                    print(f"  {title} {call[0]} {name} id={call[3]} args={args}")

    # raw error hunt excluding long handoff if possible
    for pattern in [
        "Error calling Worker",
        "INVALID_SEARCH",
        "Either query or filters",
        "Unknown tool",
        "Tool not found",
        "MCP tool failed",
        "is_error",
        "tool_result",
    ]:
        if pattern not in text:
            continue
        # print first non-handoff occurrence
        start = 0
        while True:
            index = text.find(pattern, start)
            if index < 0:
                break
            context = text[max(0, index - 120) : index + 200]
            if "Handoff" in context or "任务目标" in context or "不要改 claude-mem" in context:
                start = index + 1
                continue
            print("  RAW", pattern, "=>", context.replace("\n", " | ")[:320])
            break
