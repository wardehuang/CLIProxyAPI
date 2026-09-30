# xAI降智守护 / xAI Guardian

## 当前阶段

本目录当前只保存迁移说明，**未实现插件业务**。

后续插件使用自己的 SQLite 保存配置、节点、账号绑定、巡检记录、日志和降智状态。SQLite 不保存 xAI token。xAI token 仍由 CPA auth 文件及现有认证链管理。

旧 `cpa-xai-ip-switcher` 中的 `grok2api`、健康保底、Manager 通信和 Manager 配置不迁移。

## CPA 核心改动

所有核心改动使用 `BEGIN/END xAI Guardian core extension` 注释包裹，便于审查和移除。

### 1. 插件能力与 RPC

- `sdk/pluginapi/types.go`
  - 增加 `XAIStreamGuard` 能力。
  - 增加 xAI 流准备请求/响应。
  - 增加 xAI 流完成请求/响应。
  - 支持 `flush`、`retry`、`fail`。
  - 支持 `reload_selected_auth`、`reload_and_exclude_selected_auth`、`exclude_selected_auth`。
- `sdk/pluginabi/types.go`
  - RPC schema 升至 `7`。
  - 增加 `xai.stream.prepare` 和 `xai.stream.complete`。
- `internal/pluginhost/rpc_schema.go`
  - 增加 xAI 守护请求的 RPC 包装结构。
- `internal/pluginhost/rpc_client.go`
  - 增加 xAI 守护能力映射和 RPC 调用。
- `internal/pluginhost/adapters_interceptors.go`
  - 增加 xAI 守护能力的 Host 分发、插件隔离和异常熔断。

### 2. Handler 到 Executor 的最小桥接

- `sdk/api/handlers/handlers_interceptors.go`
  - 将 xAI 守护能力适配到核心 executor 接口。
  - 暴露已有生命周期 `TraceID`。
- `sdk/api/handlers/handlers_stream.go`
  - 仅向流请求传递 `RequestID`、`TraceID` 和 `XAIStreamGuard`。
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
  - `flush` 回放已缓存流；`retry` 将当前认证作为 credential-scoped 错误交回认证选择链；`fail` 作为 request-scoped 错误停止换号。
  - `reload_selected_auth` 保留当前认证再次选择的可能性；另外两种重试模式把当前认证加入本次请求后续轮次的 xAI 排除集合。
- `sdk/cliproxy/auth/conductor_execution.go`
  - 仅对包含 `xai` provider 的流请求消费 xAI 排除集合。
  - Home 和普通认证选择路径均保留该排除状态。

## Provider 范围

守护能力只由 `XAIExecutor.ExecuteStream` 读取，并由认证流在 provider 为 `xai` 时消费。

非 xAI provider 不调用 xAI 守护回调、不安装 xAI 超时、不读取 xAI 排除集合。

## 未实现和未承诺

- 本目录没有插件入口、SQLite schema、WebUI、账号状态页、服务端巡检页或降智判定业务。
- 尚未把旧插件完整业务迁移到 v8 API。
- 核心只提供流生命周期和认证重试边界；账号状态、巡检规则、SQLite 读写和降智分类由后续 `xai-guardian` 插件实现。
- 没有恢复旧 v7 的 Host Storage、`RequestMetadataEnricher`、`RequestFinalizer` 或 `StreamCompletionInterceptor`。

## 核验记录

- 已执行 `gofmt`。
- 已执行 `go build ./internal/... ./sdk/...`，成功。
- 未运行测试，因本任务未要求运行测试。
- 未执行 Git commit、部署或远端写入。
