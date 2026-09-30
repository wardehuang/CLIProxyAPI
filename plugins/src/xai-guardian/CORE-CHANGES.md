# xAI降智守护 / xAI Guardian

## 插件职责

`plugins/src/xai-guardian` 是独立的 CPA v8 动态插件，提供三个管理页签：

- **账号状态**：读取 CPA Host Auth API 暴露的 xAI 账号元数据，保存账号与巡检节点的绑定关系。
- **服务端巡检**：保存代理节点，使用节点执行出口连通性巡检，保存每轮巡检及结果。
- **降智守护**：接收 xAI stream completion，依据思考或工具行动证据决定 `flush`、`retry`、`fail`，保存降智次数和日志。

插件 SQLite 只保存配置、代理节点、账号绑定元数据、巡检记录、日志和降智状态。xAI token、Authorization header、密码和代理认证信息不写入 SQLite、不写入页面、不写入日志。

插件通过 `host.auth.list` 临时读取 CPA 管理的认证元数据；认证值仍由 CPA 认证链管理，插件不请求、不持久化认证值。

旧插件中的第三方网关、健康保底、外部管理器通信和旧生命周期接口没有迁移。

## CPA 核心改动

核心改动使用成对的 `BEGIN/END xAI Guardian core extension` 注释包裹，且只在 xAI provider 执行路径生效。

### 1. 插件能力与 RPC

- `sdk/pluginapi/types.go`
  - 增加 `XAIStreamGuard` 能力。
  - 增加 xAI 流准备请求/响应。
  - 增加 xAI 流完成请求/响应。
  - 支持 `flush`、`retry`、`fail`。
  - 支持 `reload_selected_auth`、`reload_and_exclude_selected_auth`、`exclude_selected_auth`。
- `sdk/pluginabi/types.go`
  - RPC schema 为 `7`。
  - 增加 `xai.stream.prepare` 和 `xai.stream.complete`。
- `internal/pluginhost/rpc_schema.go`
  - 增加 xAI 守护请求的 RPC 包装结构。
- `internal/pluginhost/rpc_client.go`
  - 增加 xAI 守护能力映射和 RPC 调用。
- `internal/pluginhost/adapters_interceptors.go`
  - 增加 xAI 守护能力的 Host 分发、插件隔离和错误处理。

### 2. Handler 到 Executor 的最小桥接

- `sdk/api/handlers/handlers_interceptors.go`
  - 将 xAI 守护能力适配到核心 executor 接口。
  - 传递已有生命周期 `TraceID`。
- `sdk/api/handlers/handlers_stream.go`
  - 仅向 xAI 流请求传递 `RequestID`、`TraceID` 和 `XAIStreamGuard`。
  - 其他 provider executor 不读取该能力。

### 3. xAI 原生流执行器

- `internal/runtime/executor/xai_stream_guard.go`
  - 增加 xAI 专用首个 payload 超时和无进展超时。
  - 无进展计时只观察 xAI SSE 实际数据，心跳和生命周期事件不重置计时器。
  - 收集响应头、状态码、原始 SSE 正文、首字节、首 payload、首个可见文本和终态错误。
  - 超时通过独立 context cancel cause 终止当前 xAI 上游流。
- `internal/runtime/executor/xai_executor_stream.go`
  - 仅在 xAI `/responses` 流路径安装守护 context。
  - 向 Host 提交一次完整的 xAI 流完成结果。
  - 未配置守护能力时保持原有流路径。

### 4. 认证流重试边界

- `sdk/cliproxy/executor/types.go`
  - 增加 xAI 守护完成状态、重试边界错误和 xAI 专用排除认证元数据。
- `sdk/cliproxy/auth/conductor_stream.go`
  - 在认证流完成后同步调用 xAI 守护。
  - `flush` 回放已缓存流；`retry` 将当前认证交回认证选择链；`fail` 作为请求级错误停止换号。
  - 重试模式按插件响应决定是否排除当前认证。
- `sdk/cliproxy/auth/conductor_execution.go`
  - 仅对 xAI provider 流请求消费 xAI 排除集合。
  - 普通 provider 认证选择路径保持原行为。

## Provider 范围

守护能力只由 xAI executor 读取，并由认证流在 provider 为 `xai` 时消费。非 xAI provider 不调用 xAI 守护回调、不安装 xAI 超时、不读取 xAI 排除集合。

## 构建与核验状态

- 已完成插件入口、SQLite schema、账号状态 API、服务端巡检 API、降智守护 API、三页签 HTML 和 v8 RPC 适配。
- 已执行 `gofmt`。
- 核心定向构建已通过：`go build ./internal/... ./sdk/...`。
- 插件 c-shared 构建尚未通过：当前 Windows 环境缺少 cgo 所需的 `gcc`，启用 cgo 时返回 `cgo: C compiler "gcc" not found`。
- 未运行测试。
- 未执行 Git commit、部署或远端写入。
