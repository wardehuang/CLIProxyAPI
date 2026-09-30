#!/usr/bin/env python3
import re, json
from pathlib import Path

files = [
    "v1-messages-2026-08-18T100317-c7a408d6.log",
    "v1-messages-2026-08-18T100321-dbaf893a.log",
    "v1-messages-2026-08-18T100330-5c217a39.log",
    "v1-messages-2026-08-18T100347-879485a6.log",
]
logdir = Path("/opt/cli-proxy-api/logs")

def split_sections(text: str) -> dict:
    parts = re.split(r"^=== ([^=]+) ===\s*$", text, flags=re.M)
    return {parts[i].strip(): parts[i+1] for i in range(1,len(parts),2)}

for name in files:
    p = logdir / name
    text = p.read_text(encoding="utf-8", errors="replace")
    sec = split_sections(text)
    print("="*80, name)
    api_req = sec.get("API REQUEST 1", "")
    # print first 80 lines of API REQUEST
    lines = api_req.strip().splitlines()
    print("--- API REQUEST head ---")
    for ln in lines[:60]:
        print(ln[:240])
    # extract url/model/provider hints
    for key in ("URL", "url", "Host", "Authorization", "x-", "X-", "model", "provider"):
        pass
    # headers block
    print("--- RESPONSE headers/meta ---")
    resp = sec.get("RESPONSE", "")
    for ln in resp.strip().splitlines()[:40]:
        print(ln[:240])
    # check if response.created present upstream
    api = sec.get("API RESPONSE 1", "")
    for ev in ["response.created", "response.output_text.delta", "response.completed", "response.content_part", "response.output_item", "[DONE]"]:
        print(f"up has {ev}:", ev in api)
    # full list of upstream event names
    events = re.findall(r"^event:\s*(.+)$", api, flags=re.M)
    print("upstream events order:", events)
    # any trailer after content_block_stop in RESPONSE
    ridx = resp.rfind("content_block_stop")
    print("response after last content_block_stop:")
    print(repr(resp[ridx:ridx+500]))
