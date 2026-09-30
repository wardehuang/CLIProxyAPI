#!/usr/bin/env python3
"""Package CPA main source with optional dirty overlays; force LF in text files."""
from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import tarfile
import tempfile
from pathlib import Path

TEXT_SUFFIXES = {
    ".go",
    ".mod",
    ".sum",
    ".yaml",
    ".yml",
    ".md",
    ".json",
    ".toml",
    ".txt",
    ".sh",
    ".ps1",
    ".html",
    ".css",
    ".js",
    ".ts",
    ".c",
    ".h",
    ".cpp",
    ".cc",
    ".proto",
    ".gitignore",
    ".dockerignore",
    ".example",
}


def run(cmd: list[str], cwd: Path | None = None) -> None:
    print("+", " ".join(cmd), flush=True)
    subprocess.check_call(cmd, cwd=str(cwd) if cwd else None)


def is_text_path(path: Path) -> bool:
    name = path.name.lower()
    if name in {"go.mod", "go.sum", "dockerfile", "makefile", "license"}:
        return True
    return path.suffix.lower() in TEXT_SUFFIXES


def force_lf_tree(root: Path) -> tuple[int, list[str]]:
    changed = 0
    checked: list[str] = []
    for path in root.rglob("*"):
        if not path.is_file():
            continue
        if not is_text_path(path):
            continue
        data = path.read_bytes()
        if b"\r" not in data:
            if path.as_posix().endswith(
                (
                    "cmd/server/main.go",
                    "go.mod",
                    "config.example.yaml",
                    "internal/runtime/executor/openai_compat_executor.go",
                    "internal/runtime/executor/helps/openai_compat_protocol.go",
                )
            ):
                checked.append(f"{path.relative_to(root).as_posix()}: cr=False")
            continue
        path.write_bytes(data.replace(b"\r\n", b"\n").replace(b"\r", b"\n"))
        changed += 1
        if path.as_posix().endswith(
            (
                "cmd/server/main.go",
                "go.mod",
                "config.example.yaml",
                "internal/runtime/executor/openai_compat_executor.go",
                "internal/runtime/executor/helps/openai_compat_protocol.go",
            )
        ):
            checked.append(f"{path.relative_to(root).as_posix()}: cr=False(after-normalize)")
    return changed, checked


def verify_no_cr(root: Path, rels: list[str]) -> None:
    for rel in rels:
        path = root / rel
        if not path.exists():
            raise SystemExit(f"missing required file in package: {rel}")
        data = path.read_bytes()
        if b"\r" in data:
            raise SystemExit(f"CRLF/CR still present: {rel}")
        print(f"spot-check ok: {rel} cr=False bytes={len(data)}", flush=True)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument(
        "--overlay",
        action="append",
        default=[],
        help="repo-relative path to overlay from working tree",
    )
    args = ap.parse_args()

    repo = Path(args.repo).resolve()
    out = Path(args.out).resolve()
    out.parent.mkdir(parents=True, exist_ok=True)

    with tempfile.TemporaryDirectory(prefix="cpa-src-") as tmp:
        tmp_path = Path(tmp)
        export_dir = tmp_path / "src"
        export_dir.mkdir()
        archive_path = tmp_path / "head.tar"
        run(
            [
                "git",
                "-c",
                "core.autocrlf=false",
                "-c",
                "core.eol=lf",
                "archive",
                "--format=tar",
                "-o",
                str(archive_path),
                "HEAD",
            ],
            cwd=repo,
        )
        with tarfile.open(archive_path, "r:") as tf:
            tf.extractall(export_dir)

        for rel in args.overlay:
            rel = rel.replace("\\", "/").lstrip("./")
            src = repo / rel
            dst = export_dir / rel
            if not src.exists():
                raise SystemExit(f"overlay missing: {rel}")
            dst.parent.mkdir(parents=True, exist_ok=True)
            if src.is_dir():
                if dst.exists():
                    shutil.rmtree(dst)
                shutil.copytree(src, dst)
            else:
                shutil.copy2(src, dst)
            print(f"overlay: {rel}", flush=True)

        changed, checked = force_lf_tree(export_dir)
        print(f"normalized_files={changed}", flush=True)
        for line in checked:
            print(line, flush=True)

        verify_no_cr(
            export_dir,
            [
                "cmd/server/main.go",
                "go.mod",
                "config.example.yaml",
                "internal/runtime/executor/openai_compat_executor.go",
                "internal/runtime/executor/helps/openai_compat_protocol.go",
                "internal/config/config_types.go",
                "internal/config/config_normalization.go",
            ],
        )

        if out.exists():
            out.unlink()
        with tarfile.open(out, "w:gz", format=tarfile.GNU_FORMAT) as tf:
            for path in sorted(export_dir.rglob("*")):
                if path.is_file():
                    arcname = path.relative_to(export_dir).as_posix()
                    tf.add(path, arcname=arcname)
        print(f"wrote {out} size={out.stat().st_size}", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
