# CPA Core / Realtime Guard 计时修订计划 rev.2

## 修订摘要 rev.2

- rev.1 仅完成 Manager 的 TPS 窗口展示。
- 本次补上遗漏的 CPA Core / Realtime Guard 修改。
- `ttft_ms` 保持 HTTP `RoundTrip` 开始到首个响应 Body 字节。
- Guard 不再以首个翻译 payload 作为 TTFB。
- AuthManager 内部换号后，Guard 使用最终成功账号自身的 attempt 时间点，不包含前序失败账号耗时。

## 根因

- xAI `UsageReporter` 已在每个 executor invocation 内独立采集 HTTP TTFT，因此 usage `ttft_ms` 口径正确。
- `AuthManager.ExecuteStream` 会在单次调用内部跨账号重试。
- buffered Guard 当前以 request lifecycle 起点作为 `StartedAt`，并以 `FirstPayloadAt` 计算 `TTFB`。
- 因此前序失败账号耗时及 SSE 翻译/拼帧耗时会污染 `ttfb_downgrade`。

## 实施

1. `UsageReporter` 保留 HTTP TTFT 的开始与首字节绝对时间；同一 reporter 发生上游重连时显式重置窗口。
2. `StreamCompletion` 增加上游 HTTP 开始与首字节时间点。
3. xAI executor 在 completion 中传出这两个时间点。
4. buffered stream attempt 使用 source completion 的最终账号时间点；`StartedAt` 保留 handler attempt 起点，`UpstreamStartedAt` 独立表示最终账号的 HTTP 起点。
5. plugin API 把上游 HTTP 起点与首字节时间传给 Realtime Guard。
6. Guard 使用 `FirstResponseByteAt - UpstreamStartedAt` 计算 `TTFBMs`。
7. `generation_ms` 继续使用 `FinishedAt - FirstPayloadAt`。
8. 缺失成功流的 HTTP 首字节时间时 fail-fast 为 unknown，避免静默退回翻译后 payload。

## 验证范围

- 更新 UsageReporter 时间窗口测试。
- 更新 buffered stream completion attempt 隔离测试。
- 更新 Realtime Guard TTFB 判定测试。
- 按用户规则不运行测试、lint 或 build。
- 只执行 `gofmt` 和 `git diff --check` 静态检查。
- 不 commit，不部署。
