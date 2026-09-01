# 实时守护纯工具调用误报修复 Implementation Plan

> **For Hermes:** 实现时逐项执行；未经用户明确批准，不运行测试、不部署、不提交 Git。

**Goal:** 纯工具调用响应不再因 Summary/Encrypted 较短触发 `soft_tps_missing_real_thinking`，同时保留 `hard_tps`、`ttfb_downgrade`、异常流和普通正文 Thinking 判定。

**Architecture:** 不降低全局 Summary/Encrypted 阈值，也不把工具调用伪装成 `isRealThinking=true`。新增明确的 `toolCallOnly` 行动证据：只有“至少一个已完成且结构有效的 `function_call`，并且没有可见正文”才跳过 soft Thinking 检查。插件【实时守护】与 Manager【降智检测】使用同一语义和同一分类顺序。

**Tech Stack:** Go、OpenAI Responses SSE、CPA Plugin API、CPA Manager Plus。

---

## 1. 已确认问题

线上请求 `0f7fb8c9`、`82cd78ed` 都是纯工具调用：

- 工具：`terminal` / `search_files`
- 动作：启动或重启 Vite、查询文件
- `response.output_text.delta`：0 个
- 有效 `function_call`：1 个
- Summary：`Start vite.`、`Restart vite.` 等，11～17 字符
- Encrypted：52～60 字节
- 当前阈值：Summary 32、Encrypted 64
- 五个账号对同一请求产生几乎相同结果，因此连续触发 `soft_tps_missing_real_thinking`

问题不是 SSE 解析，也不是账号关联，而是当前分类没有区分：

1. 普通正文回答；
2. 纯工具调用；
3. 正文与工具调用混合响应。

---

## 2. 选定方案

### 2.1 定义

新增：

```text
completedFunctionCallCount
hasVisibleText
toolCallOnly
```

严格定义：

```text
toolCallOnly =
  completedFunctionCallCount > 0
  且 VisibleTextChars == 0
```

“已完成且结构有效的 function_call”必须同时满足：

```text
item.type == "function_call"
item.status == "completed"
call_id 非空
name 非空
arguments 非空
```

不使用以下弱信号：

- `response.output_item.added`
- `response.function_call_arguments.delta`
- 仅出现工具名
- 未完成的 function call
- `visibleFlushMs == -1` 单独作为依据
- 模型名、提示词、工具名白名单

### 2.2 分类顺序

分类顺序固定为：

```text
1. quota / request error
2. hard_tps
3. soft_tps_missing_real_thinking（仅 toolCallOnly=false 时适用）
4. ttfb_downgrade
5. toolCallOnly=true => normal / healthy / completed_tool_call
6. 其他 => normal / healthy / within_threshold
```

因此：

- 纯工具调用只绕过 soft Thinking 检查。
- `hard_tps` 仍可判降智。
- `ttfb_downgrade` 仍可判降智。
- SSE `failed` / `incomplete` / `error` 仍失败。
- 混合响应仍执行原 Thinking 检查。

### 2.3 为什么不选择降低阈值

不调整：

```text
minSummaryChars = 32
minEncryptedBytes = 64
encryptedBytesPerReasoningToken = 4
```

原因：

- 降到 Summary 11 或 Encrypted 52 会全局放宽所有正文回答。
- 本次证据表明是请求类型差异，不是统一阈值过高。
- 工具调用是否完成，比它产生多少 Thinking 更可靠。

---

## 3. 插件【实时守护】改动

### Task 1：完善已完成 function_call 采集

**Files:**

- Modify: `plugins/src/cpa-xai-ip-switcher/realtime_guard.go:307-450`

**Changes:**

1. 将现有 `FunctionCallCount` 改名为 `CompletedFunctionCallCount`，避免把普通事件计数误解为已完成调用。
2. `collectRealtimeGuardOutputItem` 的 `function_call` 分支增加 `arguments` 非空要求。
3. 保留只在 terminal `response.output_item.done` 或 `status=completed` 时计数。
4. 在 SSE 完成后计算：

```go
evidence.ToolCallOnly = evidence.CompletedFunctionCallCount > 0 && evidence.VisibleTextChars == 0
```

不增加备用解析路径，不从字符串扫描工具名。

### Task 2：让 soft Thinking 分类跳过纯工具调用

**Files:**

- Modify: `plugins/src/cpa-xai-ip-switcher/realtime_guard.go:250-287`
- Modify: `plugins/src/cpa-xai-ip-switcher/realtime_guard_types.go:55-77`

**Changes:**

1. `realtimeGuardDecision` 增加：

```go
CompletedFunctionCallCount int
ToolCallOnly               bool
```

2. 从 evidence 复制到 decision。
3. soft 条件改为：

```go
if TPS > soft && TPS < hard && !IsRealThinking && !ToolCallOnly
```

4. `ttfb_downgrade` 判定之后：

```text
ToolCallOnly=true
=> classification=normal
=> qualityLevel=healthy
=> reason=completed_tool_call
```

5. 不把 `IsRealThinking` 改成 true；保持该字段只表达 Summary/Encrypted Thinking 证据。

### Task 3：增加实时守护审计字段

**Files:**

- Modify: `plugins/src/cpa-xai-ip-switcher/realtime_guard.go` 中 realtime guard 日志格式化位置

**Log fields:**

```text
completedFunctionCalls=N
toolCallOnly=true|false
```

目标日志示例：

```text
isRealThinking=false；thinking原因=reasoning_tokens_without_evidence；completedFunctionCalls=1；toolCallOnly=true；分类=normal；原因=completed_tool_call
```

避免以后再次把“无完整 Thinking”和“无有效行动”混为一谈。

---

## 4. Manager【降智检测】一致性改动

虽然当前固定数学探测不发送 tools，也必须同步判定语义，避免两套逻辑再次漂移。

### Task 4：流式采集已完成 function_call

**Files:**

- Modify: `apps/manager-server/internal/service/toolcallcheck/stream.go:31-52`
- Modify: `apps/manager-server/internal/service/toolcallcheck/stream.go:513-547`

**Changes:**

1. `streamingMetrics` 增加：

```go
completedFunctionCallCount int
toolCallNames              []string
```

2. `collectStreamingOutputItem` 增加 `function_call` 分支，使用与插件完全相同的有效性条件。
3. 去重保存工具名。
4. 流式结果设置现有字段：

```go
result.ToolCallDetected
result.ToolCallNames
```

### Task 5：Manager 分类同步 toolCallOnly

**Files:**

- Modify: `apps/manager-server/internal/service/toolcallcheck/quality_policy.go:25-121`
- Modify: `apps/manager-server/internal/service/toolcallcheck/check.go:53-101`

**Changes:**

1. `streamingThinkingEvidence` 增加：

```go
CompletedFunctionCallCount int
ToolCallOnly               bool
```

2. `Result` 增加：

```go
CompletedFunctionCallCount int  `json:"completedFunctionCallCount"`
ToolCallOnly               bool `json:"toolCallOnly"`
```

3. 使用与插件相同分类顺序。
4. tool-call-only 正常结果：

```text
classification=normal
qualityLevel=healthy
classificationReason=completed_tool_call
isRealThinking 保持原值
```

### Task 6：Manager 审计日志显示行动证据

**Files:**

- Modify: `apps/manager-server/internal/service/wxaiinspection/tool_call_check.go:287-307`

在现有本地未提交的审计字段基础上继续增加：

```text
completedFunctionCallCount
toolCallOnly
toolCallDetected
toolCallNames
```

必须保留当前本地已有改动：

```text
summaryChars
encryptedBytes
encryptedFloor
```

---

## 5. 前端显示

### Task 7：显示响应类型

**Files:**

- Modify: `apps/web/src/services/api/wxaiInspectionService.ts`
- Modify: `apps/web/src/features/monitoring/WxaiInspectionPage.tsx`

新增展示：

```text
响应类型：纯工具调用 / 正文 / 混合
已完成工具调用：N
工具：terminal, search_files
```

当 `toolCallOnly=true`：

```text
真实 Thinking：否
响应行动证据：有效纯工具调用
判定原因：completed_tool_call
```

不把页面文案写成“真实 Thinking：是”。

如加入新固定文案，需同步现有全部 i18n 文件；不新增兼容 fallback 文案。

---

## 6. 验证案例

未经用户明确批准，不运行测试。实现时至少准备以下案例。

### 插件案例

**File:** `plugins/src/cpa-xai-ip-switcher/realtime_guard_test.go`

1. **纯 completed function_call、无 output_text、Summary/Encrypted 不足**
   - 预期：`normal / completed_tool_call`
2. **function_call 只有 added，没有 done**
   - 预期：仍可命中 `soft_tps_missing_real_thinking`
3. **function_call 缺少 call_id / name / arguments**
   - 预期：不算有效行动证据
4. **function_call + 非空 output_text**
   - 预期：`toolCallOnly=false`，沿用 Thinking 判定
5. **纯 function_call 但 TPS >= hard**
   - 预期：`hard_tps`
6. **纯 function_call 命中 ttfb_downgrade**
   - 预期：`ttfb_downgrade`
7. **普通正文 Summary/Encrypted 不足**
   - 预期：行为不变

建议命令，仅在用户明确授权后运行：

```bash
go test ./plugins/src/cpa-xai-ip-switcher -count=1
```

### Manager 案例

**Create:** `apps/manager-server/internal/service/toolcallcheck/quality_policy_test.go`

覆盖与插件相同的七个分类案例。

建议命令，仅在用户明确授权后运行：

```bash
go test ./apps/manager-server/internal/service/toolcallcheck -count=1
```

### 构建验证

构建不属于测试，但也只在用户批准实施后执行：

```bash
go build ./...
```

---

## 7. 线上只读回放验收

不直接重发生产请求。使用已保存的脱敏 SSE fixture 回放：

- `0f7fb8c9`：`terminal` / `search_files` 纯工具调用
- `82cd78ed`：`terminal` 纯工具调用
- `cd7cade7`：纯正文、无 reasoning，仍应降智
- `5b3421e4`：普通正文，按原 Summary/Encrypted 规则判定

预期：

```text
0f7fb8c9 => completed_tool_call
82cd78ed => completed_tool_call
cd7cade7 => soft_tps_missing_real_thinking
5b3421e4 => 保持原分类口径
```

fixture 必须删除请求正文、token、代理认证、Authorization 和账号敏感字段。

---

## 8. 风险与边界

1. **工具调用完成不代表工具执行成功。**
   - 本规则只判断模型能力正常，不判断外部工具运行结果。
2. **混合响应不豁免。**
   - 避免模型输出少量正文再附工具调用时绕过 Thinking 检查。
3. **硬 TPS、TTFB 不豁免。**
   - 防止真正的瞬时倾倒或异常延迟被工具调用掩盖。
4. **不使用提示词/工具名白名单。**
   - 避免新增脆弱规则和重复配置。
5. **不调整全局阈值。**
   - 避免影响普通正文请求。

---

## 9. 实施边界

本计划当前不执行以下动作：

- 不修改源码
- 不运行测试
- 不构建
- 不部署
- 不修改线上配置
- 不提交或推送 Git

用户审查通过后，再明确授权实施范围和是否允许运行测试。
