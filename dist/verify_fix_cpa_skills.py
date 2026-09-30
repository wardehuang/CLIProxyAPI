import runpy
from pathlib import Path

ns = runpy.run_path(r"E:/AI/CLIProxy/CLIProxyAPI/dist/fix_cpa_skills.py")
print("TR_R", repr(ns["TR_R"]))
print("TR_RN", repr(ns["TR_RN"]))
print("LESSON tr lines:")
for line in ns["LESSON"].splitlines():
    if "tr -d" in line:
        print(repr(line))

ns["main"]()

p = Path(r"E:/AI/CLIProxy/.cursor/skills/cpa-update/SKILL.md")
data = p.read_bytes()
print("cr count", data.count(b"\r"))
text = data.decode("utf-8")
for i, line in enumerate(text.splitlines(), 1):
    if ("tr -d" in line) or ("版本/变量" in line) or ("版本字符串" in line) or ("VERSION/STAMP" in line):
        print(i, repr(line))
    s = line.strip()
    if s in {"'`", "'`。", "'` / `tr -d '"} or (s.startswith("'`") and len(s) < 12):
        print("BAD", i, repr(line))
