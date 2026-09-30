# xAI 服务器巡检与条件巡检规则

> 本文记录 `CPA-Manager-Plus` 当前 wXAi 巡检源码的真实行为。
>
> 最后核对日期：2026-07-18
>
> 权威实现目录：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection`

## 1. 文档维护要求

修改以下任一行为时，必须同步更新本文：

- 服务器巡检、条件巡检、手动刷新的候选与冷却规则。
- `/v1/responses`、`/v1/chat/completions`、billing、credits 的请求顺序和分类。
- HTTP 状态、额度状态与 priority 的映射。
- FREE/SUPER 类型识别与持久化。
- timeout、重试、CPA `proxy-url`、结果落库和 priority 恢复。

若本文与源码冲突，以源码为准，并立即修正文档。

## 2. priority

| priority | 含义 | 巡检处理 |
|---:|---|---|
| `-1` | `free-usage-exhausted` | 冷却结束前所有巡检跳过 |
| `-2` | 普通账号异常或请求异常 | 服务器巡检继续检查；条件巡检可检查 |
| `-3` | 旧版托管异常值 | 服务器巡检继续检查；条件巡检可检查 |
| `-4` | HTTP 401 | 服务器巡检继续检查；条件巡检可检查 |
| `-5` | 停用 | 所有巡检跳过 xAI 网络请求 |
| 其他值或空值 | 正常账号 | 按候选规则检查 |

托管 priority 为 `-1/-2/-3/-4`。托管账号探测恢复健康后，priority 恢复为 `1`。

## 3. 账号类型持久化

账号类型只允许：

- `FREE`
- `SUPER`

类型保存于独立表 `wxai_account_profiles`，以 `account_key` 为主键。迁移时，从每个账号最近的 `wxai_account_status_details.account_type` 回填已有 `FREE/SUPER` 标识。

billing 调用规则：

1. 已知 `FREE`：不调用任何 billing endpoint。
2. 已知 `SUPER`：只调用 `GET /v1/billing?format=credits`。
3. 类型未知：调用 `GET /v1/billing`。
4. 普通 billing 的 `monthlyLimit > 0` 判为 `SUPER`，否则判为 `FREE`，随后落盘。
5. 新判为 `SUPER` 时，再调用 `GET /v1/billing?format=credits`。
6. 对话探测返回 `free-usage-exhausted` 且类型未知时，直接落盘为 `FREE`。

billing 和 credits 只更新账号类型与额度展示，不参与账号健康判定。billing/credits 失败不会降低 priority。

## 4. 对话探测主链路

所有实际参与巡检的账号使用同一套对话探测，不再执行 access token JWT 解码或 `bot_flag_source` 判断。

### 4.1 主探测

- Method：`POST`
- URL：`https://cli-chat-proxy.grok.com/v1/responses`
- Body：`{"model":"grok-4.5","input":"ping","stream":false}`
- `X-XAI-Token-Auth`：`xai-grok-cli`
- `x-grok-client-version`：`0.2.93`
- `User-Agent`：`xai-grok-workspace/0.2.93`

### 4.2 明确结果

以下主探测结果直接使用，不调用 fallback：

- HTTP 2xx：健康。
- HTTP 401：账号异常，priority `-4`。
- 错误 code/message 包含 `free-usage-exhausted`、`used all the included free usage` 或 `included free usage has been exhausted`：额度耗尽，priority `-1`。
- 其他 HTTP 429：账号异常，priority `-2`。
- HTTP 402/403：账号异常，priority `-2`。
- 其他非 404、非 5xx 的 HTTP 错误：账号异常，priority `-2`。

仅 `free-usage-exhausted` 类错误可判定额度耗尽。billing 百分比、credits 百分比和普通 HTTP 429 均不能判定额度耗尽。

### 4.3 含糊结果与 fallback

以下主探测结果视为含糊：

- HTTP 404。
- HTTP 5xx。
- timeout 或其他请求错误。

含糊时调用：

- Method：`POST`
- URL：`https://cli-chat-proxy.grok.com/v1/chat/completions`
- Body：`{"model":"grok-4.5","messages":[{"role":"user","content":"ping"}],"stream":false}`

fallback 的明确结果按同一分类规则处理。fallback 仍为 404/5xx 时，最终按账号异常 `-2` 处理；fallback 请求失败时按请求异常 `-2` 处理。

## 5. timeout 重试

每个 xAI HTTP 请求使用巡检配置中的单次 timeout。发生 timeout 时：

1. 等待 400ms。
2. 原请求重试一次。
3. 非 timeout 网络错误不重试。
4. 上下文已取消时不重试。

该规则同时应用于 responses、chat completions、billing 和 credits。

## 6. 额度冷却

额度耗尽的恢复时间保存于 `wxai_priority_adjustments.recover_at_ms`。

恢复时间计算顺序：

1. `Retry-After` header。
2. `X-RateLimit-Reset` header。
3. JSON 中的 `retry_after`、`retryAfter`、`retry_after_ms`、`reset_at`、`resetAt`、`resets_at`、`recovery_time`、`recover_at` 等字段。
4. 有有效恢复时间：冷却至恢复时间加 1 分钟。
5. 没有有效恢复时间：冷却至当前时间加 24 小时再加 1 分钟。

冷却期间：

- 存在尚未到期、目标 priority 为 `-1` 的额度 adjustment。priority patch 失败时回滚本次 adjustment，不形成假冷却。
- 服务器巡检跳过。
- 条件巡检在原候选算法完成后过滤。
- 手动刷新跳过。
- 不下载 auth JSON，不调用 responses、chat completions、billing 或 credits。
- priority 保持当前值。
- 服务器新 run 复用上一轮状态和额度数据，并标记 `skipped`。

冷却到期后，服务器巡检和手动刷新可重新探测。若再次返回额度耗尽，更新新的 `recover_at_ms`。

## 7. 服务器巡检

服务器巡检创建新 `wxai_inspection_runs`。

候选规则：

1. `priority=-5`：停用集合，不执行 xAI 请求。
2. 额度冷却未结束：冷却集合，不执行 xAI 请求。
3. 其余所有 xAI 账号：全部参与巡检，不再按普通或托管 priority 区分。

服务器巡检执行顺序：

1. 从 CPA 下载 auth JSON。
2. 读取 `access_token`。
3. 执行 responses 主探测，必要时执行 chat completions fallback。
4. 探测健康后，按持久化账号类型刷新 billing 元数据。
5. 根据探测结果调整或恢复 priority。
6. 保存结果、状态详情、原始 HTTP 响应和窗口花费。

## 8. 条件巡检

条件候选算法保持不变：

- 查询最近 10 分钟 `usage_events`。
- `calls > 0`。
- provider 归一化为 `xai`。
- 匹配顺序：`accountKey`、`fileName + authIndex`、`provider + accountID`、唯一 `fileName`、唯一 `authIndex`。
- 排除 `priority=-5` 和 `priority=-1`。
- 其他 priority 可进入。
- 同一账号去重，候选原因仍为 `active_recent`。

候选生成后，再统一过滤仍处于额度冷却期的账号。条件巡检复用最近一次服务器 run，不创建新 run。

## 9. 手动刷新

手动刷新只针对指定账号：

- 停用账号保持停用，不执行 xAI 请求。
- 冷却账号保持额度耗尽状态，不执行 xAI 请求。
- 其他账号执行与服务器巡检相同的 responses、fallback、billing 元数据刷新和 priority 处理。

## 10. CPA proxy-url

每轮有实际 xAI 请求的巡检通过 CPA Management API 读取 `/v0/management/config` 中的 `proxy-url`。

- `proxy-url` 非空：为 xAI HTTP client 设置该代理。
- `socks5://` 和 `socks5h://` 使用 SOCKS5 Dialer，不通过 `http.Transport.Proxy`。
- SOCKS5 同时兼容标准格式 `socks5://user:password@host:port` 和现网旧格式 `socks5://user:password:host:port`。
- `http://` 和 `https://` 使用 `http.Transport.Proxy`。
- `proxy-url` 为空：xAI HTTP client 直接连接，不读取进程环境代理。
- Management config 读取失败：巡检 fail-fast。
- `proxy-url` 不是包含 scheme 和 host 的合法 URL：巡检 fail-fast。
- CPA auth-files 下载和 priority patch 仍直接访问 CPA Management API，不经过该 xAI 代理 client。

## 11. priority 恢复与原始响应

- 健康结果会恢复 `-1/-2/-3/-4` 到 `1`。
- priority adjustment 保存原 priority，避免异常类型变化时丢失初始值。
- responses、chat completions、billing、credits 的 HTTP 响应均追加保存到 `wxai_inspection_http_responses`。
- timeout 没有 HTTP 响应，不写原始响应记录；重试成功后保存成功尝试的响应。
