#!/usr/bin/env python3
import re
from pathlib import Path

p = Path("/opt/cli-proxy-api/logs/v1-messages-2026-08-18T100317-c7a408d6.log")
text = p.read_text(encoding="utf-8", errors="replace")
parts = re.split(r"^=== ([^=]+) ===\s*$", text, flags=re.M)
sec = {parts[i].strip(): parts[i+1] for i in range(1,len(parts),2)}
resp = sec["RESPONSE"]
print("message_start", resp.count("message_start"))
print("message_delta", resp.count("message_delta"))
print("message_stop", resp.count("message_stop"))
print("content_block_stop", resp.count("content_block_stop"))
print("ping", resp.count("ping"))
print("--- full RESPONSE section after headers ---")
# print from first event
i = resp.find("event:")
print(resp[i:] if i>=0 else resp)
