#!/usr/bin/env python3
import json
import re
from pathlib import Path


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


def walk(value):
    if isinstance(value, dict):
        value_type = value.get("type")
        if value_type == "tool_use":
            yield ("use", value.get("name"), value.get("input"), value.get("id"))
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
            yield ("result", value.get("tool_use_id"), content, value.get("is_error"))
        for child in value.values():
            yield from walk(child)
    elif isinstance(value, list):
        for child in value:
            yield from walk(child)


for request_id in ["cc290ea0", "71edf8f7", "467ac32e"]:
    paths = [
        path
        for path in Path("/opt/cli-proxy-api/logs").glob(f"*{request_id}*")
        if path.name.startswith("v1-messages-")
        and "mcp-schema" not in path.name
        and not path.name.endswith("-strip.log")
    ]
    path = paths[0]
    text = path.read_text(encoding="utf-8", errors="replace")
    match = re.search(r"^=== REQUEST BODY ===\s*$", text, re.M)
    rest = text[match.end() :]
    next_section = re.search(r"^=== ", rest, re.M)
    body = rest[: next_section.start()] if next_section else rest
    obj = parse_json(body)
    print("=" * 72)
    print(request_id, path.name)
    if not obj:
        print("parse fail")
        continue
    for event in walk(obj):
        if event[0] == "use":
            print(
                "USE",
                event[1],
                "id=",
                event[3],
                "input=",
                json.dumps(event[2], ensure_ascii=False, default=str)[:300],
            )
        if event[0] == "result":
            content = event[2] if isinstance(event[2], str) else str(event[2])
            print("RESULT id=", event[1], "is_error=", event[3])
            print(" ", content[:800].replace("\n", " | "))
