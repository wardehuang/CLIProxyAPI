from pathlib import Path
import subprocess

strip = Path("/opt/cli-proxy-api/logs/v1-messages-2026-07-25T001204-a9f6b10d-strip.log")
req = Path("/opt/cli-proxy-api/logs/v1-messages-2026-07-25T001206-a9f6b10d.log")
text = strip.read_text(encoding="utf-8", errors="replace")
idx = text.find("=== BEFORE BODY ===")
print(text[:idx] if idx >= 0 else text[:2000])
print("==== strip file size", strip.stat().st_size)
print("==== request log size", req.stat().st_size)

after_part = text[text.find("=== AFTER BODY ===") :] if "=== AFTER BODY ===" in text else ""
before_part = text[idx : text.find("=== AFTER BODY ===")] if idx >= 0 else ""
print("visible_files in BEFORE section:", before_part.count("<visible_files"))
print("visible_files in AFTER section:", after_part.count("<visible_files"))
print("placeholder in AFTER:", after_part.count("cpa-strip-visible-files: removed visible_files"))

r = req.read_text(encoding="utf-8", errors="replace")
print("request log has visible_files:", "<visible_files" in r)
print("request log has placeholder:", "cpa-strip-visible-files: removed visible_files" in r)
for line in r.splitlines()[:50]:
    low = line.lower()
    if (
        low.startswith("content-length")
        or line.startswith("URL:")
        or line.startswith("Timestamp")
        or line.startswith("=== ")
        or line.startswith("Model")
        or '"model"' in line[:80]
    ):
        print(line[:200])

try:
    out = subprocess.check_output(
        ["bash", "-lc", "grep -h a9f6b10d /opt/cli-proxy-api/logs/main.log 2>/dev/null | grep -i strip | head -n 20"],
        text=True,
    )
except Exception as exc:
    out = str(exc)
print("==== main strip lines ====")
print(out if out.strip() else "(none)")
