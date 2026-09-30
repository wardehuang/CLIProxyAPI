from pathlib import Path

p = Path(r"E:/AI/CLIProxy/.cursor/skills/cpa-update/SKILL.md")
data = p.read_bytes()
needle = "版本字符串精确匹配前必须".encode("utf-8")
idx = data.find(needle)
print("idx", idx)
chunk = data[idx : idx + 160]
print(chunk)
print("hex", chunk.hex())

# also hard rule line
needle2 = "版本/变量匹配前去 CR".encode("utf-8")
idx2 = data.find(needle2)
print("idx2", idx2)
chunk2 = data[idx2 : idx2 + 120]
print(chunk2)
print("hex2", chunk2.hex())
