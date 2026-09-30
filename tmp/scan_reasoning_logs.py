#!/usr/bin/env python3
from pathlib import Path

logdir = Path("/opt/cli-proxy-api/logs")
files = sorted(logdir.glob("v1-responses-2026-08-18T03*.log"))
print("files", len(files))
for f in files:
    t = f.read_text(encoding="utf-8", errors="replace")
    err = ("must be passed back" in t) and ("reasoning_content" in t)
    deepseek = "deepseek" in t.lower()
    http400 = "HTTP Status: 400" in t or "\nStatus: 400\n" in t
    maybe_ok = "Status: 200" in t or '"status":"completed"' in t
    print(
        f"{f.name} size={f.stat().st_size} err_reason={err} deepseek={deepseek} "
        f"http400={http400} maybe_ok={maybe_ok}"
    )
