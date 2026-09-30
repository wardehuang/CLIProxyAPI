from pathlib import Path

r = Path("/opt/cli-proxy-api/logs/v1-messages-2026-07-25T001206-a9f6b10d.log").read_text(
    encoding="utf-8", errors="replace"
)
parts = r.split("=== API REQUEST")
print("sections", len(parts))
for i, part in enumerate(parts[:3]):
    print("--- section", i, "len", len(part))
    print(" visible_files", part.count("<visible_files"))
    print(" placeholder", part.count("cpa-strip-visible-files: removed visible_files"))
