#!/usr/bin/env python3
import json
import re
from pathlib import Path

logdir = Path("/opt/cli-proxy-api/logs")

# Post-deploy requests from journal: bfd07cee, 869034ca, 6c4db507 and any newer with mem-search
ids = []
for p in sorted(logdir.glob("v1-messages-*.log"), key=lambda x: x.stat().st_mtime, reverse=True):
    if p.name.endswith("-strip.log") or "mcp-schema" in p.name:
        continue
    # time after 03:26
    if "2026-07-27T032" in p.name or "2026-07-27T033" in p.name or "2026-07-27T034" in p.name:
        text_head = p.read_text(encoding="utf-8", errors="replace")[:5000]
        full = None
        if "CPA-Manager-Plus" in p.read_text(encoding="utf-8", errors="replace")[:200000] or "mem-search" in p.name:
            full = p.read_text(encoding="utf-8", errors="replace")
            if "mem-search" in full or "CPA-Manager-Plus" in full or "user-claude-mem-search" in full:
                ids.append(p)
    if len(ids) >= 15:
        break

# Also explicitly search by journal ids if files exist
for rid in ["bfd07cee", "869034ca", "6c4db507"]:
    matches = list(logdir.glob(f"*{rid}*"))
    print("rid files", rid, [m.name for m in matches])


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
                except Exception as e:
                    return None
    return None


def tool_entries(tools):
    out = []
    for t in tools or []:
        if not isinstance(t, dict):
            continue
        name = t.get("name")
        schema = t.get("input_schema") or t.get("parameters")
        if isinstance(t.get("function"), dict):
            name = name or t["function"].get("name")
            schema = t["function"].get("parameters") or schema
        props = {}
        if isinstance(schema, dict):
            props = schema.get("properties") or {}
        out.append((name, t.get("type"), len(props) if isinstance(props, dict) else -1, sorted(props)[:10] if isinstance(props, dict) else []))
    return out


# Find logs that have INVALID and extract tool_result around search
print("\n=== INVALID / Error calling contexts ===")
for p in sorted(logdir.glob("v1-messages-2026-07-27T03*.log"), key=lambda x: x.stat().st_mtime, reverse=True)[:40]:
    if p.name.endswith("-strip.log"):
        continue
    text = p.read_text(encoding="utf-8", errors="replace")
    if "INVALID_SEARCH" not in text and "Error calling Worker" not in text and "Either query or filters" not in text:
        continue
    print("=" * 60)
    print(p.name)
    for pat in ["INVALID_SEARCH", "Error calling Worker", "Either query or filters", "user-claude-mem-search"]:
        idx = 0
        count = 0
        while count < 3:
            pos = text.find(pat, idx)
            if pos < 0:
                break
            snip = text[max(0, pos - 250) : pos + 350].replace("\n", " | ")
            print(f"  [{pat}#{count}] {snip[:500]}")
            idx = pos + len(pat)
            count += 1

# Check newest post-deploy with inject evidence via size change: look API REQUEST tool count raw
print("\n=== post-deploy mem-search API REQUEST tool names ===")
for p in sorted(logdir.glob("v1-messages-2026-07-27T033*.log"), key=lambda x: x.stat().st_mtime, reverse=True):
    if p.name.endswith("-strip.log"):
        continue
    text = p.read_text(encoding="utf-8", errors="replace")
    if "mem-search" not in text and "CPA-Manager-Plus" not in text:
        continue
    print("=" * 60)
    print(p.name, "bytes", len(text))
    for title, body in sections(text):
        if title == "REQUEST BODY":
            obj = parse_json(body)
            if obj:
                entries = tool_entries(obj.get("tools"))
                print("CLIENT", len(entries), [e[0] for e in entries if e[0] and str(e[0]).startswith("user-")])
        if title.startswith("API REQUEST"):
            api = body.split("\nBody:\n", 1)[-1]
            # raw string search for user-claude-mem-search in upstream body
            has = "user-claude-mem-search" in api
            print(title, "raw has user-claude-mem-search=", has, "body_chars", len(api))
            obj = parse_json(api)
            if obj:
                tools = obj.get("tools")
                print("  parsed tools type", type(tools).__name__, "len", len(tools) if isinstance(tools, list) else None)
                if isinstance(tools, list):
                    entries = tool_entries(tools)
                    mcp = [e for e in entries if e[0] and str(e[0]).startswith("user-")]
                    print("  mcp", mcp)
                    print("  all names sample", [e[0] for e in entries[:5]], "... total", len(entries))
                # maybe tools under different key for responses
                raw = json.dumps(obj)
                print("  dump has user-claude-mem-search", "user-claude-mem-search" in raw)
                # find tool-like arrays
                def find_tools(node, path=""):
                    found = []
                    if isinstance(node, dict):
                        if isinstance(node.get("tools"), list):
                            found.append((path + ".tools", node["tools"]))
                        for k, v in node.items():
                            found.extend(find_tools(v, path + "." + str(k)))
                    elif isinstance(node, list) and len(node) < 5:
                        for i, v in enumerate(node):
                            found.extend(find_tools(v, f"{path}[{i}]"))
                    return found
                nested = find_tools(obj)
                for path, arr in nested[:5]:
                    ents = tool_entries(arr)
                    mcp = [e for e in ents if e[0] and str(e[0]).startswith("user-")]
                    if mcp or "tool" in path:
                        print("  nested", path, "len", len(arr), "mcp", mcp[:5])
