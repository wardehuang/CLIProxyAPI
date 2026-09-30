from pathlib import Path
import re

paths = [
    Path(r"E:/AI/CLIProxy/.claude/skills/cpa-update/SKILL.md"),
    Path(r"E:/AI/CLIProxy/.cursor/skills/cpa-update/SKILL.md"),
    Path(r"E:/AI/CLIProxy/.claude/skills/cpa-plugin-deploy/SKILL.md"),
    Path(r"E:/AI/CLIProxy/.cursor/skills/cpa-plugin-deploy/SKILL.md"),
    Path(r"E:/AI/CLIProxy/.claude/skills/cpa-full-deploy/SKILL.md"),
    Path(r"E:/AI/CLIProxy/.cursor/skills/cpa-full-deploy/SKILL.md"),
]

# Literal shell snippets. Keep as plain text; do not put real CR bytes here.
TR_R = "tr -d " + "'" + "\\" + "r" + "'"
TR_RN = "tr -d " + "'" + "\\" + "r" + "\\" + "n" + "'"

HARD_EXTRA_LINES = [
    "- **SSH 认证用 key，不是 root 密码**。登录用户 `ubuntu`，端口以 `server-info`/用户当前信息为准（现为 `27312`）。`ubuntu` 有 passwordless sudo。Hermes 本地审批弹窗不等于 root 密码；不要向用户索要 root 密码。",
    "- **源码包与部署脚本必须 LF**。Windows 打包后抽查 archive 成员；远端脚本本地写好、强制 LF、再 scp 执行。禁止嵌套 heredoc 拼脚本。",
    "- **版本/变量匹配前去 CR**。使用 shell 命令 " + TR_R + " / " + TR_RN + "（先去掉回车再做精确匹配）。",
    "- **健康检查最多等 90 秒**。启动初期连接拒绝不算最终失败。",
    "",
]
HARD_EXTRA = "\n".join(HARD_EXTRA_LINES)

LESSON_LINES = [
    "## 踩坑与认证说明（2026-08-17 验证）",
    "",
    "### 不是 root 密码",
    "- 服务器登录用户是 `ubuntu`，用 SSH 私钥登录：`E:/Files/SSH Key/oracle-ssh-key-2026-05-16.key`，端口 `27312`。",
    "- `ubuntu` 已配置 `NOPASSWD: ALL`，远端脚本里的 `sudo systemctl ...` **不需要**交互输入 root/ubuntu 密码。",
    "- 部署过程中 Hermes/桌面端可能弹出 **本地命令审批**（例如 stop/restart systemd、写 `/opt`、删 root 路径临时文件）。这是工具策略审批，**不是** Linux root 密码框，也**不要**因此向用户索要 root 密码。",
    "- 若审批被拒绝/未点，命令会失败；说明原因并等用户批准后重试即可。",
    "",
    "### CRLF / 脚本 / 版本校验",
    "1. Windows 上直接 `git archive` 或工作区 tar，Go/YAML 可能带 CRLF。打包后必须抽查 `cmd/server/main.go`、`go.mod`、`config.example.yaml`，确认无 CR；否则先导出并统一转 LF 再上传。",
    "2. 禁止用“shell 嵌套 Python heredoc”现场拼远端部署脚本。正确做法：本地 `write_file` 写完整 bash 脚本 → 强制 LF → `scp` → 远端 `bash /tmp/....sh <args>`。",
    "3. 版本字符串精确匹配前必须先去掉回车，命令示例：`" + TR_R + "`；否则会出现“help 已打印正确版本，grep 仍失败”。",
    "4. 从 Windows 读出的 VERSION/STAMP 等变量可能带尾部 CR；写入远端命令前先执行：`" + TR_RN + "`。",
    "5. 健康检查：启动后前几秒 `curl: (7) Failed to connect` 正常；必须在最多 90 秒窗口内轮询 `systemctl is-active` + `http://127.0.0.1:18457/v0/management/config`，不要首败即停。",
    "6. 主程序部署不重建插件；日志里插件 loaded 只说明旧插件仍在。",
    "",
    "### 推荐打包流程（主程序/插件通用）",
    "```text",
    "git -c core.autocrlf=false -c core.eol=lf archive --format=tar HEAD",
    "→ 解到临时目录",
    "→ 文本文件统一 LF",
    "→ 抽查 archive 成员 crlf=False",
    "→ scp 上传",
    "→ 服务器 CGO 构建",
    "→ 本地 LF 部署脚本 scp 后执行",
    "```",
    "",
    "### 最近一次主程序成功部署",
    "```text",
    "Version: 7.2.135.0001",
    "Commit: f0fc04e4",
    "BuiltAt: 2026-08-17T06:16:21Z",
    "Service: active",
    "Config merge: merged-with-old-values",
    "Data backup: /opt/cli-proxy-api/backups/pre-custom-7.2.135.0001-20260817062220.tar.gz",
    "Binary backup: /opt/cli-proxy-api/cli-proxy-api.bak.custom.7.2.135.0001.20260817062220",
    "Deploy log: /opt/cli-proxy-api/deploy-logs/cpa-main-7.2.135.0001-20260817141620.log",
    "Plugins: preserved (not rebuilt this run)",
    "```",
    "",
]
LESSON = "\n".join(LESSON_LINES)


def clean_junk_lines(text: str) -> str:
    keep = []
    for line in text.splitlines():
        s = line.strip()
        if s in {"'`", "'`。", "'` / `tr -d '", "`' / `", "`。"}:
            continue
        if s.startswith("'`") and len(s) <= 12:
            continue
        if s == "'":
            continue
        keep.append(line)
    return "\n".join(keep) + "\n"


def strip_old_injected_blocks(text: str) -> str:
    text = re.sub(
        r"- \*\*SSH 认证用 key，不是 root 密码\*\*。.*?(?=\n- \*\*(?:线上数据不能丢失|部署必须可自恢复|不要覆盖线上运行数据)\*\*|\n## )",
        "",
        text,
        count=1,
        flags=re.S,
    )
    text = re.sub(
        r"## 踩坑与认证说明.*?(?=\n## |\Z)",
        "",
        text,
        count=1,
        flags=re.S,
    )
    return text


def ensure_hard_rules(text: str) -> str:
    if "SSH 认证用 key，不是 root 密码" in text and TR_R in text:
        return text
    anchor = "- **不要泄露服务器 secret/token**"
    idx = text.find(anchor)
    if idx < 0:
        # plugin skills may use slightly different bullet
        anchor = "- **不要泄露服务器 secret/token**。读取 config、systemd 或日志时，最终回复不能复述 token、API key、OAuth token 等敏感内容。"
        idx = text.find(anchor)
    if idx < 0:
        raise RuntimeError("hard-rule anchor not found")
    end = text.find("\n", idx)
    if end < 0:
        end = len(text)
    return text[: end + 1] + HARD_EXTRA + text[end + 1 :]


def ensure_lesson(text: str) -> str:
    # Use function replacement so backslashes are not interpreted by re.sub.
    if "## 踩坑与认证说明" in text:
        return re.sub(
            r"## 踩坑与认证说明.*?(?=\n## |\Z)",
            lambda _m: LESSON,
            text,
            count=1,
            flags=re.S,
        )
    marker = "## 14. 最近一次已验证部署记录"
    if marker in text:
        return text.replace(marker, LESSON + marker, 1)
    return text.rstrip() + "\n\n" + LESSON


def fix_version_check(text: str) -> str:
    pattern = r'if ! "\$TMP/cli-proxy-api" --help 2>&1 \|.*?then'

    def repl(_m: re.Match) -> str:
        return (
            'if ! "$TMP/cli-proxy-api" --help 2>&1 | '
            + TR_R
            + ' | grep -Fq "CLIProxyAPI Version: $VERSION"; then'
        )

    return re.sub(pattern, repl, text, count=1, flags=re.S)


def update_latest_record(text: str) -> str:
    def repl(_m: re.Match) -> str:
        return "\n".join(
            [
                "```text",
                "版本：7.2.135.0001",
                "Commit：f0fc04e4",
                "BuiltAt：2026-08-17T06:16:21Z",
                "服务：cli-proxy-api.service active",
                "备份：/opt/cli-proxy-api/backups/pre-custom-7.2.135.0001-20260817062220.tar.gz",
                "旧二进制：/opt/cli-proxy-api/cli-proxy-api.bak.custom.7.2.135.0001.20260817062220",
                "```",
            ]
        )

    return re.sub(
        r"```text\n版本：7\.1\.54\.0001.*?```",
        repl,
        text,
        count=1,
        flags=re.S,
    )


def main() -> None:
    assert "\\" in TR_R and "r" in TR_R and "\r" not in TR_R and "\n" not in TR_R
    assert "\r" not in LESSON
    for p in paths:
        text = p.read_bytes().replace(b"\r\n", b"\n").replace(b"\r", b"\n").decode("utf-8")
        text = text.replace("-p 28922", "-p 27312").replace("-P 28922", "-P 27312")
        text = clean_junk_lines(text)
        text = strip_old_injected_blocks(text)
        text = ensure_hard_rules(text)
        text = ensure_lesson(text)
        text = fix_version_check(text)
        text = update_latest_record(text)
        text = text.replace(
            "推荐使用 Bash heredoc 执行，避免 PowerShell 引号把远端变量展开坏：",
            "优先本地写完整 bash 部署脚本（LF），scp 后远端执行。不要用嵌套 shell/Python heredoc 拼脚本。若必须用 heredoc，仅作简单命令且注意 CRLF：",
        )
        text = re.sub(r"\n{4,}", "\n\n\n", text)
        if not text.endswith("\n"):
            text += "\n"
        p.write_bytes(text.encode("utf-8"))

        data = p.read_bytes()
        decoded = data.decode("utf-8")
        bad = []
        for i, line in enumerate(decoded.splitlines(), 1):
            s = line.strip()
            if s in {"'`", "'`。", "'` / `tr -d '"} or (s.startswith("'`") and len(s) < 12):
                bad.append((i, s))
            if "tr -d '" in line and ("\n" in line or line.count("'") < 2):
                # incomplete command on one line
                if not (TR_R in line or TR_RN in line):
                    bad.append((i, s[:100]))
        print(
            p.as_posix(),
            "cr",
            b"\r" in data,
            "tr_ok",
            TR_R.encode() in data,
            "lesson",
            "不是 root 密码".encode("utf-8") in data,
            "bad",
            bad[:5],
        )


if __name__ == "__main__":
    main()
