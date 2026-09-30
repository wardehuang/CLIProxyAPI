#!/usr/bin/env python3
import json
import re
from pathlib import Path


def extract_section(text: str, prefix: str) -> str:
    parts = re.split(r"^=== ", text, flags=re.M)
    for part in parts:
        if part.startswith(prefix):
            return "\n".join(part.splitlines()[1:])
    return ""


def parse_sse_events(body: str):
    events = []
    current_event = None
    data_lines = []
    for line in body.splitlines():
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
    return events


def summarize_stream(label: str, body: str):
    print(f"  -- {label}")
    events = parse_sse_events(body)
    print(f"     events={len(events)}")
    arg_pieces = []
    for event_name, payload in events:
        if not isinstance(payload, dict):
            continue
        payload_type = payload.get("type") or event_name or ""
        text = json.dumps(payload, ensure_ascii=False)

        # Upstream OpenAI responses style
        if payload_type == "response.output_item.added":
            item = payload.get("item") or {}
            if item.get("type") == "function_call" or item.get("name"):
                print(
                    "     upstream item.added",
                    "name=",
                    item.get("name"),
                    "args=",
                    repr(item.get("arguments"))[:120],
                )
        if payload_type == "response.function_call_arguments.delta":
            delta = payload.get("delta")
            if isinstance(delta, str):
                arg_pieces.append(delta)
                print("     upstream args.delta", repr(delta)[:120])
            elif isinstance(delta, dict) and "arguments" in delta:
                arg_pieces.append(str(delta.get("arguments")))
                print("     upstream args.delta.obj", delta)
            else:
                print("     upstream args.delta raw", text[:180])
        if payload_type == "response.function_call_arguments.done":
            print(
                "     upstream args.done",
                "name=",
                payload.get("name"),
                "arguments=",
                repr(payload.get("arguments"))[:200],
            )
        if payload_type == "response.output_item.done":
            item = payload.get("item") or {}
            if item.get("name") or item.get("type") == "function_call":
                print(
                    "     upstream item.done",
                    "name=",
                    item.get("name"),
                    "arguments=",
                    repr(item.get("arguments"))[:200],
                )

        # Downstream Anthropic style
        if payload_type == "content_block_start":
            block = payload.get("content_block") or {}
            if block.get("type") == "tool_use" or block.get("name"):
                print(
                    "     down block_start",
                    "name=",
                    block.get("name"),
                    "input=",
                    repr(block.get("input"))[:120],
                )
        if payload_type == "content_block_delta":
            delta = payload.get("delta") or {}
            if delta.get("type") == "input_json_delta" or "partial_json" in delta:
                piece = delta.get("partial_json")
                arg_pieces.append(piece or "")
                print("     down input_json_delta", repr(piece)[:160])
            elif "text" in delta:
                pass
            else:
                if "user-claude" in text or "partial" in text:
                    print("     down delta other", text[:180])
        if payload_type == "content_block_stop":
            if arg_pieces:
                joined = "".join(str(piece) for piece in arg_pieces if piece is not None)
                print("     assembled_from_deltas=", joined[:300])
    if arg_pieces:
        joined = "".join(str(piece) for piece in arg_pieces if piece is not None)
        print(f"     FINAL_ASSEMBLED_ARGS={joined!r}")


for request_id in ["753a0916", "cc290ea0", "71edf8f7", "467ac32e"]:
    paths = [
        path
        for path in Path("/opt/cli-proxy-api/logs").glob(f"*{request_id}*")
        if path.name.startswith("v1-messages-")
        and "mcp-schema" not in path.name
        and not path.name.endswith("-strip.log")
    ]
    if not paths:
        print("MISSING", request_id)
        continue
    path = paths[0]
    text = path.read_text(encoding="utf-8", errors="replace")
    print("=" * 80)
    print(request_id, path.name)
    summarize_stream("UPSTREAM API RESPONSE", extract_section(text, "API RESPONSE"))
    summarize_stream("DOWNSTREAM RESPONSE", extract_section(text, "RESPONSE"))
