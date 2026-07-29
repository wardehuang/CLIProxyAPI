# cpa-mcp-schema-patch

Cursor 发给 CPA 的 MCP tool `input_schema` 常被掏成 `{"type":"object"}`。Grok 严格遵守空 schema，tool call 只能发出 `{}`。

本插件在请求进入 CPA 后、转发上游前：

1. 读取本地上传的 MCP tool JSON（Cursor descriptor 或 registry map）
2. 对**空 schema** 且 registry 命中的 tool 写回完整 schema
3. （默认）把 registry 里有、请求 `tools[]` 里没有的 MCP tool **注入**进去
4. 保留已有完整 native tool；不覆盖非空 schema

## 配置

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    cpa-mcp-schema-patch:
      enabled: true
      priority: 5
      debug: false
      schemas-dir: mcp-schemas
      only-empty: true
      inject-missing: true
      logs-dir: logs
      detail-log: auto
```

- `schemas-dir`：相对 CPA 工作目录，或绝对路径
- `only-empty`：默认 `true`，只补空 schema，不覆盖 `Read`/`Shell` 等完整工具
- `inject-missing`：默认 `true`，registry 有而请求 `tools[]` 缺失的 MCP tool 一并追加（修 Cursor 不暴露 MCP tools 的情况）
- `detail-log`：`auto|true|false`。`auto` = 主机 `request-log=true` 且 `commercial-mode=false` 时写 before/after 详情文件（同 strip）
- `logs-dir`：详情日志目录，默认 `logs`

## 上传本地 MCP JSON

### 方式 A：直接拷贝 Cursor descriptor

把本机：

```text
~/.cursor/projects/<project>/mcps/user-claude-mem/tools/*.json
~/.cursor/projects/<project>/mcps/user-context-mode/tools/*.json
```

拷到服务器：

```text
/opt/cli-proxy-api/mcp-schemas/user-claude-mem/tools/search.json
/opt/cli-proxy-api/mcp-schemas/user-claude-mem/tools/timeline.json
...
/opt/cli-proxy-api/mcp-schemas/user-context-mode/tools/ctx_execute.json
...
```

命名规则：

| 文件 | 请求中的 tool name |
|---|---|
| `user-claude-mem/tools/search.json` | `user-claude-mem-search` |
| `user-context-mode/tools/ctx_execute.json` | `user-context-mode-ctx_execute` |

descriptor 字段用 `arguments`；插件会转成请求体的 `input_schema` / `parameters`。

### 方式 B：Management API 上传

认证后：

```bash
# 查看已加载 schema
GET /v0/management/mcp-schema-patch/schemas

# 热重载目录
POST /v0/management/mcp-schema-patch/reload

# 上传单个 JSON
POST /v0/management/mcp-schema-patch/upload
Content-Type: application/json

{
  "file_name": "user-claude-mem/tools/search.json",
  "content": "{ ... cursor tool descriptor json ... }",
  "reload_after": true
}
```

### 方式 C：registry map 单文件

```json
{
  "user-claude-mem-search": {
    "type": "object",
    "properties": {
      "query": { "type": "string" },
      "limit": { "type": "number" }
    },
    "additionalProperties": true
  }
}
```

## 挂载点

- `request.intercept_before` / `request.intercept_after`：入站体补全
- `request.finalize`：上游发送前再补一次（防格式转换后 schema 变空）

支持：

- Anthropic：`tools[].input_schema`
- OpenAI function：`tools[].function.parameters`
- 顶层 `parameters` / `arguments`

## 构建

```bash
cd plugins/src/cpa-mcp-schema-patch
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -buildmode=c-shared -o cpa-mcp-schema-patch.so
```

产物放到 CPA `plugins/linux/arm64/cpa-mcp-schema-patch.so`，重启服务。

## 批量上传 examples

脚本：`push_examples.py`（标准库，无第三方依赖）

```powershell
cd E:\AI\CLIProxy\CLIProxyAPI\plugins\src\cpa-mcp-schema-patch

# 预览
python push_examples.py --dry-run

# 上传（需要 management key）
$env:CPA_MANAGEMENT_KEY = "<your-management-key>"
python push_examples.py
# 或
python push_examples.py --base-url http://127.0.0.1:18457 --management-key "<key>"

# 只查 registry
python push_examples.py --list-only
```

默认扫描 `./examples/**/*.json`，按相对路径作为 `file_name` 调：

`POST /v0/management/mcp-schema-patch/upload`

## 验收

1. 上传 `user-claude-mem-search` 完整 schema
2. Grok + mem-search：转发前 `input_schema.properties` 含 `query`/`limit`
3. tool call 不再是 `{}`
4. 原生 `Read` schema 不变
