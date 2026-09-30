# 实时守护有效响应证据 rev.1 Implementation Plan

> **For Hermes:** 当前仅供审查。未经用户明确授权，不继续修改源码、不运行测试、不构建、不部署、不提交 Git。

## rev.1 修订摘要

相对上一版“纯工具调用豁免”方案，本版新增：

1. 增加“实质正文证据”，覆盖操作完成后的简短最终汇报。
2. 不再把所有正常响应都强制绑定 Summary/Encrypted Thinking 长度。
3. 建议新增两个独立、可配置阈值：
   - 最少实质正文字符：`128`
   - 最少可见输出持续时间：`1000ms`
4. 纯工具调用方案保留。
5. `hard_tps`、`ttfb_downgrade`、异常流、混合工具响应规则不变。
6. `isRealThinking` 仍只表达真实 Thinking，不伪造为 true。

---

**Goal:** 防止“纯工具调用”和“操作完成后的实质最终汇报”因隐藏 Summary/Encrypted 较短被误判降智，同时保留真正异常响应的拦截能力。

**Architecture:** 将 soft 降智从“必须有 Thinking”改为“必须至少有一种有效响应证据”。有效证据分为真实 Thinking、纯工具调用、实质可见正文三类。三类使用同一分类入口，不修改全局 Summary/Encrypted 阈值。

---

## 1. 当前问题

请求 `6bd958b2` 的 Ebbie 响应：

```text
Summary：Concise answer.（15 字符）
Encrypted：58/64 bytes
Reasoning tokens：4
可见正文：304 字符
可见输出持续：3837ms
正文内容：完整汇报头像框、修改密码接口和真实验证结果
```

当前规则只看：

```text
Summary >= 32
或者 Encrypted >= 64
```

因此把正常最终汇报判为：

```text
soft_tps_missing_real_thinking
```

同一请求第三个账号只是把隐藏 Summary 写得更长：

```text
Summary：60
Encrypted：118
```

便通过。说明当前结果依赖隐藏 Summary 的措辞长度，不等价于可见答案质量。

---

## 2. 选定方案：统一有效响应证据

新增统一判断：

```text
hasValidResponseEvidence =
  isRealThinking
  OR toolCallOnly
  OR substantiveVisibleResponse
```

soft 降智条件改为：

```text
softTPS < TPS < hardTPS
AND hasValidResponseEvidence == false
=> soft_tps_missing_valid_response_evidence
```

为避免现有日志/API 大范围改名，可以保留旧 reason：

```text
soft_tps_missing_real_thinking
```

但建议采用新 reason：

```text
soft_tps_missing_valid_response_evidence
```

**推荐采用新 reason。** 旧名称已经不再准确，不做兼容双写。

---

## 3. 三种有效证据

### 3.1 真实 Thinking

保持当前逻辑：

```text
Summary >= 32
或者 Encrypted >= max(64, reasoningTokens × 4)
```

字段：

```text
isRealThinking
realThinkingReason
```

### 3.2 纯工具调用

保持已审查方案：

```text
completedFunctionCallCount > 0
AND outputTextChars == 0
=> toolCallOnly=true
```

有效 function call 必须：

```text
status=completed
call_id 非空
name 非空
arguments 非空
```

正常原因：

```text
completed_tool_call
```

### 3.3 实质可见正文

新增：

```text
substantiveVisibleResponse =
  completedMessageCount > 0
  AND completedFunctionCallCount == 0
  AND outputTextChars >= minSubstantiveVisibleChars
  AND visibleFlushMs >= minSubstantiveVisibleMs
  AND refusalDetected == false
```

建议默认值：

```text
minSubstantiveVisibleChars = 128
minSubstantiveVisibleMs = 1000
```

正常原因：

```text
substantive_visible_response
```

### 为什么是 128 字符

已知样本：

```text
6bd958b2：193 / 304 字符，正常最终汇报
cd7cade7：154～446 字符，正常结果汇报
0f7fb8c9、82cd78ed：0 字符，走纯工具调用证据
固定数学题：通常低于 128 字符，仍要求 Thinking 证据
```

`128` 能覆盖完整结果汇报，不会把一句简短敷衍回答自动判正常。

### 为什么要求 1000ms

防止以下响应仅凭长度绕过：

```text
大量正文瞬间倾倒
预生成缓存一次性回放
极短生成窗口的异常输出
```

已知正常最终汇报可见输出持续均超过 2 秒。

---

## 4. 分类顺序

固定顺序：

```text
1. quota / request error / SSE error
2. hard_tps
3. ttfb_downgrade
4. hasValidResponseEvidence=false 且 TPS 位于 soft 区间
   => soft_tps_missing_valid_response_evidence
5. toolCallOnly=true
   => normal / completed_tool_call
6. substantiveVisibleResponse=true
   => normal / substantive_visible_response
7. isRealThinking=true
   => normal / within_threshold
8. 其他正常低速响应
   => normal / within_threshold
```

`hard_tps` 和 `ttfb_downgrade` 必须排在所有正常证据之前。

---

## 5. 不豁免的情况

以下仍不能作为实质正文证据：

1. `response.output_item.done` 未完成。
2. 只有 refusal。
3. 正文少于 128 字符。
4. 可见输出持续少于 1000ms。
5. 正文与 function call 混合。
6. SSE incomplete / failed / error。
7. HTTP 非 2xx。
8. 命中 `hard_tps`。
9. 命中 `ttfb_downgrade`。

混合“正文 + 工具调用”保持原 Thinking 检查，避免通过随便附一段文字绕过。

---

## 6. 新增配置

插件设置：

```text
realtime_guard_min_substantive_visible_chars = 128
realtime_guard_min_substantive_visible_ms = 1000
```

Manager policy/API：

```text
minSubstantiveVisibleChars
minSubstantiveVisibleMs
```

WebUI 标签必须带单位：

```text
实质正文最少字符
实质正文最少持续时间（ms）
```

保存后立即生效；页面刷新必须回填保存值。

---

## 7. 新增审计字段

插件和 Manager 均输出：

```text
outputTextChars
completedMessageCount
completedFunctionCallCount
toolCallOnly
refusalDetected
substantiveVisibleResponse
validResponseEvidence
validResponseEvidenceReason
```

正常样例：

```text
isRealThinking=false
summaryChars=15
encrypted=58/64
outputTextChars=304
visibleFlushMs=3837
substantiveVisibleResponse=true
validResponseEvidenceReason=substantive_visible_response
classification=normal
```

这样日志不会再出现“Thinking 不足但为何正常”无法解释的问题。

---

## 8. 文件范围

### CLIProxyAPI 插件

可能修改：

- `plugins/src/cpa-xai-ip-switcher/realtime_guard.go`
- `plugins/src/cpa-xai-ip-switcher/realtime_guard_types.go`
- `plugins/src/cpa-xai-ip-switcher/store.go`
- `plugins/src/cpa-xai-ip-switcher/slots.go`
- `plugins/src/cpa-xai-ip-switcher/main.go`
- `plugins/src/cpa-xai-ip-switcher/runtime.go`
- `plugins/src/cpa-xai-ip-switcher/page.html`

### CPA Manager Plus

可能修改：

- `apps/manager-server/internal/service/toolcallcheck/stream.go`
- `apps/manager-server/internal/service/toolcallcheck/quality_policy.go`
- `apps/manager-server/internal/service/toolcallcheck/check.go`
- `apps/manager-server/internal/service/wxaiinspection/tool_call_check.go`
- `apps/web/src/services/api/wxaiInspectionService.ts`
- `apps/web/src/features/monitoring/WxaiInspectionPage.tsx`
- 插件设置表单对应类型和 UI 文件

---

## 9. 回放验收矩阵

```text
0f7fb8c9
纯工具调用
=> completed_tool_call

82cd78ed
纯工具调用
=> completed_tool_call

6bd958b2
304 字符正文、3837ms、Summary 15、Encrypted 58
=> substantive_visible_response

cd7cade7
154～446 字符正文、持续超过 1 秒
=> substantive_visible_response

短正文、无 Thinking、少于 128 字符
=> soft_tps_missing_valid_response_evidence

长正文但小于 1000ms 瞬间倾倒
=> 不获得实质正文证据

混合正文 + function_call
=> 不获得 toolCallOnly / substantiveVisibleResponse 豁免

hard_tps
=> 始终降智

ttfb_downgrade
=> 始终降智
```

---

## 10. 风险与取舍

### 优点

- 不依赖提示词分类。
- 不依赖账号、模型或工具名白名单。
- 不降低 Summary/Encrypted 全局阈值。
- 能解释纯工具调用和最终汇报两类正常响应。
- 审计字段可复算。

### 风险

- 语义错误但篇幅较长、流式持续超过 1 秒的回答可能被视为有有效正文证据。
- 实时守护只能判断响应结构和时序，不能判断答案事实正确性。

这是可接受取舍：该守护的目标应是发现明显协议/推理异常，不应把“隐藏 Summary 不够长”当作答案质量判据。

---

## 11. 当前边界

本次只新增方案文件。

未执行：

- 源码修改
- 测试
- 构建
- 类型检查
- 配置修改
- 部署
- Git commit

上一轮已经存在的本地未提交代码保持原样，本次没有继续修改。
