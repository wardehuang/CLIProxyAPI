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


def extract_json_blobs(text: str):
    blobs = []
    for match in re.finditer(r'\{"model":', text):
        start = match.start()
        depth = 0
        end = None
        for index, char in enumerate(text[start : start + 5_000_000], start):
            if char == "{":
                depth += 1
            elif char == "}":
                depth -= 1
                if depth == 0:
                    end = index + 1
                    break
        if end is None:
            continue
        raw = text[start:end]
        try:
            blobs.append(json.loads(raw))
        except Exception:
            continue
    return blobs


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
    return name, field, len(property_names) == 0, property_names


def walk_tool_calls(value, output):
    if isinstance(value, dict):
        value_type = value.get("type")
        name = value.get("name")
        if value_type == "tool_use" and isinstance(name, str):
            output.append(("tool_use", name, value.get("input")))
        function_value = value.get("function")
        if isinstance(function_value, dict):
            function_name = function_value.get("name")
            if isinstance(function_name, str) and function_name.startswith("user-"):
                arguments = function_value.get("arguments")
                if isinstance(arguments, str):
                    try:
                        arguments = json.loads(arguments)
                    except Exception:
                        pass
                output.append(("function", function_name, arguments))
        if value_type in ("function_call", "tool_call", "custom_tool_call"):
            call_name = name or (function_value or {}).get("name") if isinstance(function_value, dict) else name
            arguments = value.get("arguments") or value.get("input") or value.get("parameters")
            if isinstance(arguments, str):
                try:
                    arguments = json.loads(arguments)
                except Exception:
                    pass
            if isinstance(call_name, str):
                output.append((value_type, call_name, arguments))
        # OpenAI responses API style
        if value.get("type") == "function_call" or ("call_id" in value and "name" in value and "arguments" in value):
            call_name = value.get("name")
            arguments = value.get("arguments")
            if isinstance(arguments, str):
                try:
                    arguments = json.loads(arguments)
                except Exception:
                    pass
            if isinstance(call_name, str) and call_name.startswith("user-"):
                output.append(("responses_call", call_name, arguments))
        for child in value.values():
            walk_tool_calls(child, output)
    elif isinstance(value, list):
        for child in value:
            walk_tool_calls(child, output)


def is_mcp_name(name: str) -> bool:
    return isinstance(name, str) and name.startswith(mcp_prefixes)


for request_id in ids:
    files = sorted(logdir.glob(f"v1-messages-*-{request_id}.log"))
    request_logs = [path for path in files if not path.name.endswith("-strip.log") and not path.name.endswith("-mcp-schema.log")]
    print("=" * 72)
    print(request_id)
    if not request_logs:
        print("  MISSING request log")
        continue
    log_path = request_logs[0]
    text = log_path.read_text(encoding="utf-8", errors="replace")
    print("  file", log_path.name, "bytes", len(text))
    if "INVALID_SEARCH" in text or "Either query or filters" in text:
        print("  marker: INVALID_SEARCH present")
    if "mcp tool schemas patched" in text:
        print("  marker: patched phrase in request log")
    detail_logs = list(logdir.glob(f"*{request_id}*mcp-schema*"))
    print("  mcp-schema detail logs:", [path.name for path in detail_logs] or "NONE")

    blobs = extract_json_blobs(text)
    print("  json_blobs", len(blobs))
    for blob_index, blob in enumerate(blobs):
        tools = blob.get("tools")
        if isinstance(tools, list) and tools:
            print(f"  blob[{blob_index}] REQUEST tools={len(tools)} model={blob.get('model')}")
            native_checked = 0
            for tool in tools:
                if not isinstance(tool, dict):
                    continue
                name, field, empty, property_names = schema_summary(tool)
                if not isinstance(name, str):
                    continue
                if is_mcp_name(name):
                    print(
                        f"    MCP {name}: field={field} empty={empty} nprops={len(property_names)} props={property_names[:10]}"
                    )
                elif name in ("Read", "Shell", "Grep") and native_checked < 3:
                    print(f"    native {name}: empty={empty} nprops={len(property_names)}")
                    native_checked += 1
        calls = []
        walk_tool_calls(blob, calls)
        interesting = [call for call in calls if is_mcp_name(call[1])]
        # de-dup
        seen = set()
        unique_calls = []
        for call in interesting:
            key = (call[0], call[1], json.dumps(call[2], sort_keys=True, ensure_ascii=False) if not isinstance(call[2], str) else call[2])
            if key in seen:
                continue
            seen.add(key)
            unique_calls.append(call)
        if unique_calls:
            print(f"  blob[{blob_index}] MCP tool_calls ({len(unique_calls)}):")
            for call in unique_calls[:20]:
                args_text = json.dumps(call[2], ensure_ascii=False) if call[2] is not None else "null"
                if len(args_text) > 240:
                    args_text = args_text[:240] + "..."
                print(f"    {call[0]} {call[1]} args={args_text}")
