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
            yield from walk(child)
    elif isinstance(value, list):
        for child in value:
            yield from walk(child)


def extract_request_body(text: str) -> str:
    match = re.search(r"^=== REQUEST BODY ===\s*$", text, re.M)
    if not match:
        return ""
    rest = text[match.end() :]
    next_section = re.search(r"^=== ", rest, re.M)
    if next_section:
        return rest[: next_section.start()]
    return rest


logdir = Path("/opt/cli-proxy-api/logs")
found_any = False
for path in sorted(logdir.glob("v1-messages-2026-07-27T03*.log"), key=lambda item: item.stat().st_mtime, reverse=True):
    if path.name.endswith("-strip.log"):
        continue
    text = path.read_text(encoding="utf-8", errors="replace")
    body = extract_request_body(text)
    obj = parse_json(body)
    if not obj:
        continue
    events = list(walk(obj))
    search_uses = [
        event
        for event in events
        if event[0] == "tool_use"
        and isinstance(event[1], str)
        and (
            event[1].startswith("user-claude")
            or event[1].startswith("user-context")
            or "mem-search" in event[1]
            or "claude-mem" in event[1]
        )
    ]
    interesting_results = []
    for event in events:
        if event[0] != "tool_result":
            continue
        content = event[2]
        if not isinstance(content, str):
            content = str(content)
        lowered = content.lower()
        markers = [
            "invalid_search",
            "error calling worker",
            "either query or filters",
            "mcp",
            "unknown tool",
            "not found",
            "tool not",
            "failed",
        ]
        if not any(marker in lowered for marker in markers):
            continue
        # Skip handoff document noise
        if "## 0. 任务目标" in content or "Handoff：请求侧强制补全" in content or "不要改 claude-mem Worker" in content:
            continue
        if len(content) > 1500 and "INVALID_SEARCH" not in content[:400]:
            continue
        interesting_results.append((event[1], content[:500], event[3]))

    if not search_uses and not interesting_results:
        continue
    found_any = True
    print("=" * 72)
    print(path.name)
    for event in search_uses:
        print(
            "USE",
            event[1],
            "id=",
            event[3],
            "input=",
            json.dumps(event[2], ensure_ascii=False, default=str)[:300],
        )
    for result in interesting_results[:8]:
        print("RESULT id=", result[0], "is_error=", result[2])
        print(" ", result[1].replace("\n", " | ")[:400])

if not found_any:
    print("NO live user-claude tool_use / non-handoff error tool_result found in 03xx logs")

# Also scan API responses for function_call name user-claude
print("\n=== API RESPONSE function_call user-* ===")
for path in sorted(logdir.glob("v1-messages-2026-07-27T03*.log"), key=lambda item: item.stat().st_mtime, reverse=True)[:40]:
    if path.name.endswith("-strip.log"):
        continue
    text = path.read_text(encoding="utf-8", errors="replace")
    response_parts = re.split(r"^=== (?=API RESPONSE|RESPONSE )", text, flags=re.M)
    for part in response_parts:
        if not (part.startswith("API RESPONSE") or part.startswith("RESPONSE ")):
            continue
        for match in re.finditer(r'"name"\s*:\s*"(user-[^"]+)"', part):
            name = match.group(1)
            snip = part[match.start() : match.start() + 250].replace("\n", " | ")
            # only if looks like function call nearby
            window = part[max(0, match.start() - 80) : match.start() + 20]
            if "function_call" in window or "tool_use" in window or '"type":"function_call"' in part[max(0, match.start() - 200) : match.start()]:
                print(path.name, name, snip[:220])
