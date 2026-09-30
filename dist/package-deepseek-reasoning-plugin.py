#!/usr/bin/env python3
import hashlib
import io
import subprocess
import tarfile
import time
from pathlib import Path

root = Path(__file__).resolve().parents[1]
plugin = "cpa-deepseek-reasoning-replay"
ts = time.strftime("%Y%m%d%H%M%S")
try:
    commit = subprocess.check_output(
        ["git", "rev-parse", "--short=8", "HEAD"],
        cwd=root,
        text=True,
    ).strip()
except Exception:
    commit = "unknown"
dirty = ""
try:
    st = subprocess.check_output(["git", "status", "--porcelain"], cwd=root, text=True)
    if st.strip():
        dirty = "-dirty"
except Exception:
    dirty = "-dirty"
deploy_id = f"cpa-plugin-{plugin}-{commit}{dirty}-{ts}"
dist = root / "dist"
dist.mkdir(exist_ok=True)
src_pkg = dist / f"{deploy_id}-source.tar.gz"

include_dirs = [
    "sdk/pluginapi",
    "sdk/pluginabi",
    f"plugins/src/{plugin}",
]
include_files = ["go.mod", "go.sum"]

members: list[Path] = []
for rel in include_files:
    path = root / rel
    if path.exists():
        members.append(path)

for directory in include_dirs:
    base = root / directory
    for path in base.rglob("*"):
        if not path.is_file():
            continue
        if path.name in {".DS_Store"}:
            continue
        if path.suffix in {".so", ".exe", ".out"}:
            continue
        rel = path.relative_to(root).as_posix()
        if path.name.endswith("_test.go") and f"plugins/src/{plugin}/" not in rel:
            continue
        members.append(path)


def lf_bytes(data: bytes, path: Path) -> bytes:
    text_names = {"go.mod", "go.sum"}
    text_ext = {".go", ".mod", ".sum", ".yaml", ".yml", ".md", ".json", ".txt", ".sh"}
    if path.name in text_names or path.suffix.lower() in text_ext:
        return data.replace(b"\r\n", b"\n").replace(b"\r", b"\n")
    return data


unique_members = sorted(set(members), key=lambda item: item.as_posix())
with tarfile.open(src_pkg, "w:gz", format=tarfile.GNU_FORMAT) as tar:
    for path in unique_members:
        rel = path.relative_to(root).as_posix()
        data = lf_bytes(path.read_bytes(), path)
        info = tarfile.TarInfo(name=rel)
        info.size = len(data)
        info.mtime = int(path.stat().st_mtime)
        info.mode = 0o644
        tar.addfile(info, io.BytesIO(data))

checked = 0
with tarfile.open(src_pkg, "r:gz") as tar:
    for member in tar.getmembers():
        if not member.isfile():
            continue
        if not member.name.endswith((".go", ".mod", ".sum")):
            continue
        data = tar.extractfile(member).read()
        if b"\r" in data:
            raise SystemExit(f"CRLF in archive member {member.name}")
        checked += 1

(dist / "cpa-deepseek-reasoning-replay-deploy-id.txt").write_text(deploy_id + "\n", encoding="utf-8", newline="\n")
script = dist / "cpa-deepseek-reasoning-replay-remote.sh"
if script.exists():
    script.write_bytes(script.read_bytes().replace(b"\r\n", b"\n").replace(b"\r", b"\n"))

print(deploy_id)
print(src_pkg)
print(src_pkg.stat().st_size)
print(len(unique_members), checked)
print(hashlib.sha256(src_pkg.read_bytes()).hexdigest())
