# xAI 服务器巡检与条件巡检规则

> 本文记录 `CPA-Manager-Plus` 当前 wXAi 巡检源码的真实行为。
>
> 最后核对日期：2026-07-28
>
> 权威实现目录：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection`

## 1. 文档维护要求

修改以下任一行为时，必须同步更新本文：

- 服务器巡检、条件巡检、手动刷新的候选与冷却规则。
- `/v1/responses`、`/v1/chat/completions`、billing、credits 的请求顺序和分类。
- HTTP 状态、额度状态与 priority 的映射。
- FREE/SUPER 类型识别与持久化。
- timeout、重试、CPA `proxy-url`、结果落库和 priority 恢复。
- xAI 业务请求 HTTP 429 的条件巡检即时触发、并发和 run 复用规则。

若本文与源码冲突，以源码为准，并立即修正文档。

## 2. priority

| priority | 含义 | 巡检处理 |
|---:|---|---|
| `-1` | `free-usage-exhausted` | 冷却结束前服务器/条件巡检跳过；手动刷新仍探测 |
| `-2` | 普通账号异常或请求异常 | 服务器巡检继续检查；条件巡检可检查；手动刷新可检查 |
| `-3` | 旧版托管异常值 | 服务器巡检继续检查；条件巡检可检查；手动刷新可检查 |
| `-4` | HTTP 401 | 服务器巡检继续检查；条件巡检可检查；手动刷新可检查 |
| `-5` | 停用 | 服务器/条件巡检跳过 xAI 网络请求；手动刷新仍探测（成功不恢复 `-5`） |
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
- `x-grok-client-version`：每轮从 CPA `GET /v0/management/xai-client-version` 读取核心 `xaiClientVersionValue`；当前值为 `0.2.93`。
- `User-Agent`：`xai-grok-workspace/<xaiClientVersionValue>`；当前值为 `xai-grok-workspace/0.2.93`。
- Manager 不保留巡检专用的版本常量，也不在读取失败时回退到硬编码版本。

### 4.2 明确结果

以下主探测结果直接使用，不调用 fallback：

- HTTP 2xx：健康。
- HTTP 401：账号异常，priority `-4`。
- 错误 code/message 包含 `free-usage-exhausted`、`used all the included free usage` 或 `included free usage has been exhausted`：额度耗尽，priority `-1`。
- 其他 HTTP 429：账号异常，priority `-2`。
- HTTP 402/403：账号异常，priority `-2`。
- 其他非 404、非 5xx 的 HTTP 错误：账号异常，priority `-2`。

仅 `free-usage-exhausted` 类错误可判定额度耗尽。billing 百分比、credits 百分比和普通 HTTP 429 均不能判定额度耗尽。

CPA 的实际 xAI 业务请求失败并产生 HTTP 429 usage event 后分流处理：

1. 当 `fail_body`、`raw_json` 或 `fail_summary` 中可明确识别 `free-usage-exhausted`、`used all the included free usage` 或 `included free usage has been exhausted` 时，直接进入请求额度落盘快速路径。
2. 快速路径不查询最近 10 分钟条件候选，不下载 auth JSON，不读取 xAI client version，不创建 xAI HTTP client，也不发送 responses、chat completions、billing 或 credits 请求。
3. wXAi 巡检配置启用时，快速路径通过 CPA Management API 读取 xAI auth file 列表，按 `fileName + authIndex`、唯一 `fileName`、唯一 `authIndex` 匹配当次请求账号；匹配后直接将 priority 调整为 `-1`，写入 `wxai_priority_adjustments`，账号类型未知时写为 `FREE`；存在可复用 run 时再写巡检结果、状态详情和日志。
4. 明确额度事件不触发条件巡检。账号无法安全匹配或 priority patch 失败时记录错误，也不退回到再次发送 xAI 探测请求。
5. 无法明确判定额度耗尽的普通 HTTP 429 才立即触发条件巡检，并由条件巡检重新探测后决定 priority `-2` 或 `-1`。

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

请求额度落盘快速路径复用同一恢复规则：优先使用 usage event 从响应 header 派生的 `header_quota_recover_at_ms`，其次解析当次失败响应 body；均无有效恢复时间时使用默认 24 小时加 1 分钟。

若自动化设置中的 `quotaCooldownEnabled` 同时开启，同一明确额度 usage event 还会独立进入既有 `quota-auto-disable` 流程：该流程按 auth file 保存 CPAMP 所有权并临时设置 `disabled=true`，24 小时后仅恢复由 CPAMP 本次自动停用的文件。该流程与 wXAi priority `-1` adjustment 独立，不替代请求额度落盘快速路径。

额度耗尽响应留痕：

- 所有 xAI HTTP 响应将脱敏后的 response headers 保存到 `wxai_inspection_http_responses.response_headers_json`。
- `Authorization`、`Proxy-Authorization`、`Set-Cookie`、`Set-Cookie2` 的值替换为 `[REDACTED]`，其他 header 原样保存。
- 额度耗尽时写入 `wXAi 额度耗尽响应已记录` 日志，包含 `responseHeaders`、`recoverySource`、`upstreamRecoverAtMs` 和最终 `recoverAtMs`。
- `recoverySource` 为 `header:Retry-After`、`header:X-RateLimit-Reset`、`header:X-Rate-Limit-Reset`、`response_body` 或 `default_24h`。
- 请求额度落盘快速路径复用业务 usage event，不把业务响应伪装成巡检 HTTP 响应，因此不新增 `wxai_inspection_http_responses` 记录。

冷却期间：

- 存在尚未到期、目标 priority 为 `-1` 的额度 adjustment。priority patch 失败时回滚本次 adjustment，不形成假冷却。
- 服务器巡检跳过。
- 条件巡检在原候选算法完成后过滤。
- 手动刷新**不跳过**：仍执行 billing + responses 探测；成功可恢复 priority，失败可更新冷却。
- 服务器/条件巡检冷却跳过时：不下载 auth JSON，不调用 responses、chat completions、billing 或 credits；priority 保持当前值；服务器新 run 复用上一轮状态和额度数据，并标记 `skipped`。
- `/v0/management/wxai-inspection/latest` 为存在 adjustment 的账号返回 `recoverAtMs`；账号状态页面在额度耗尽账号名下显示该冷却截止时间。

冷却到期后，服务器巡检可重新探测。手动刷新在冷却期内也可强制探测。若再次返回额度耗尽，更新新的 `recover_at_ms`。

## 7. 服务器巡检

服务器巡检创建新 `wxai_inspection_runs`。

xAI 业务请求 HTTP 429 不启动服务器巡检，也不创建新的服务器巡检 run。明确额度耗尽时直接落盘；普通 429 才触发条件巡检。

定时触发规则：

1. Worker 每 30 秒检查一次配置。
2. `time_points` 模式按配置时区计算不晚于当前时刻的最近一个时间点，使用该时间点生成 `triggerKey`，格式为 `YYYY-MM-DD HH:mm`。
3. 若到点时手动巡检或条件巡检占用全局巡检锁，本次 tick 跳过；锁释放后的后续 tick 仍会检查最近应执行时间点并补跑，不要求精确命中配置分钟。
4. 若存在同一 `scheduled + triggerKey` 的 run，无论该 run 成功或失败，都不重复执行。
5. 若跨过多个时间点，只补跑最近一个应执行时间点，不追溯创建完整积压队列。
6. `interval` 模式保持原行为：按最近一次定时 run 的开始时间和 `intervalMinutes` 判断是否到期。

候选规则：

1. `priority=-5`：停用集合，不执行 xAI 请求。
2. 额度冷却未结束：冷却集合，不执行 xAI 请求。
3. 其余所有 xAI 账号：全部参与巡检，不再按普通或托管 priority 区分。

探测并发（`workers`）：

1. 同时运行的探测 worker 数上限为 `settings.Workers`（≤0 时按 1；不超过本轮候选账号数）。
2. worker **错峰启动**：第 1 个立即启动，之后每间隔 **10 秒**（`wxaiProbeWorkerStartStagger`）再启动 1 个，直到达到上限。
3. **取账号**也全局错峰：任意 worker 开始探测一个账号前，与上一账号开始时刻至少间隔 **10 秒**（`wxaiProbeAccountTakeStagger`）。第 1 个账号立即开始；第 5、6… 个同样受此间隔约束，不是 worker 空闲就立刻开探。
4. 账号经 channel 投递；已启动的 worker 取到账号后先过取账号门闩，再执行 `inspectSingleAccount`。不是一次性为全部账号开线程。
5. 因此同时 in-flight 探测数 ≤ `workers`，但新账号启动节奏约为每 10 秒一个（探测耗时 > 10 秒时才会叠满并发）。
6. 上下文取消时停止再启动后续 worker，并中断取账号等待；已启动 worker 在 jobs channel 关闭后退出。
7. 条件巡检复用同一 `inspectAccounts` 错峰逻辑。
8. `deleteWorkers`（处置并发）对 wXAi 无效：不自动 disable/delete，priority 调整在探测 worker 内同步完成。

服务器巡检执行顺序：

1. 读取 CPA `proxy-url` 和核心 `xaiClientVersionValue`，创建本轮共享的 xAI HTTP client。
2. 从 CPA 下载 auth JSON。
3. 读取 `access_token`。
4. 执行 responses 主探测，必要时执行 chat completions fallback。
5. 探测健康后，按持久化账号类型刷新 billing 元数据。
6. 根据探测结果调整或恢复 priority。
7. 保存结果、状态详情、原始 HTTP 响应和窗口花费。

## 8. 条件巡检

触发来源：

1. 条件巡检 worker 启动后立即检查一次，之后每 30 秒检查一次。
2. collector 新收到的 usage event 中，存在 `failed=true`、`fail_status_code=429`，且 `provider`（为空时使用 `auth_provider_snapshot`）归一化为 `xai`，但当次响应不能明确判定额度耗尽时，立即触发一次。

同一批 usage events 即使包含多个普通 429，也只触发一次。即时触发和 30 秒 worker 共用同一个条件巡检 worker 本地运行锁，以及 wXAi 巡检服务的全局运行锁；已有条件或服务器巡检运行时，本次普通 429 触发跳过，不排队、不重试。单账号手动刷新不占用全局运行锁，不阻塞也不被阻塞。

条件候选算法保持不变：

- 查询最近 10 分钟 `usage_events`。
- `calls > 0`。
- provider 归一化为 `xai`。
- 匹配顺序：`accountKey`、`fileName + authIndex`、`provider + accountID`、唯一 `fileName`、唯一 `authIndex`。
- 排除 `priority=-5` 和 `priority=-1`。
- 其他 priority 可进入。
- 同一账号去重，候选原因仍为 `active_recent`。

候选生成后，再统一过滤仍处于额度冷却期的账号。条件巡检复用最近一次巡检 run，不创建新 run；最近 run 不存在、ID 无效或状态为 `running` 时跳过。普通 429 即时触发不按报错账号单独巡检，仍按上述规则重新查询最近 10 分钟 usage events 并筛选全部候选账号。

明确额度耗尽事件不进入上述条件候选算法。快速路径收到事件后立即排队处理；同一 worker 内的快速路径串行执行，并复用 wXAi 巡检服务全局运行锁。若已有服务器或条件巡检运行，快速路径每 250ms 等待锁释放后执行，不等待 30 秒定时 tick。单账号手动刷新不占用该锁。一个批次同时存在明确额度事件和普通 429 时，先完成明确额度落盘，再触发一次普通条件巡检，使已进入 `-1` 冷却的账号被候选过滤。

快速路径不创建 run。最近 run 存在且非 `running` 时，结果、状态详情和 `【wXAi 请求额度落盘】` 日志复用该 run；无可复用 run 时仍修改 CPA priority，并持久化 `wxai_priority_adjustments` 和 `wxai_account_profiles`，但不伪造巡检结果或日志 run。

## 9. 手动刷新

实现文件：`manual_refresh.go`。入口：`POST /v0/management/wxai-inspection/manual-refresh`。

独立流程规则：

1. **不占用** wXAi 巡检服务全局运行锁（`acquireRun`）。服务器巡检、条件巡检、请求额度落盘进行中时，手动刷新仍可执行。
2. 不创建新的全量巡检 run：必须复用最近一次 run 写结果/状态/日志。若尚无任何 run，直接返回错误 `至少有过一次服务器巡检`（HTTP 400），不探测、不落库。
3. 只针对请求指定的单个账号；不跳过停用（`-5`）或额度冷却（`-1`）。
4. 健康判定以 responses 为准；billing/credits 只提供元数据，须先成功才进入 responses。

单账号执行顺序：

1. 读取 CPA `proxy-url` 与 `xai-client-version`，创建 xAI HTTP client（`proxy-url` 为 socks5/socks5h 时走 SOCKS5 Dialer）。
2. 下载 auth JSON，读取 `access_token`。
3. 解码 JWT：`bot_flag_source` 非空则设 `priority=-6` 并结束。
4. 账号类型未知：调用 `GET /v1/billing` 判定 `FREE/SUPER` 并落盘；失败则按探测失败处理，不调用 responses。
5. 按类型刷新 billing 元数据（须成功）：
   - `SUPER`：`GET /v1/billing?format=credits`（若本轮已做过月度 billing 则只调 credits；否则月度与 credits 并发）。
   - `FREE` 或其他：`GET /v1/billing?format=credits`。
   - billing/credits 失败：按探测失败调整 priority，**不**调用 responses。
6. billing 成功后，**无论 FREE/SUPER**，一律经上述 xAI HTTP client（CPA 代理，含 socks5）调用：
   - `POST https://cli-chat-proxy.grok.com/v1/responses`
   - Body：`{"model":"grok-4.5","input":"ping","stream":false}`
7. 以 responses 结果判定健康：
   - 成功：恢复托管异常 priority（`-1/-2/-3/-4` → 正常值）；`-5` 不在托管集合，成功后保持停用。
   - 失败：按既有规则映射 `quota_exhausted` → `-1`，401 → `-4`，其他 → `-2`。
8. 写入巡检结果、账号状态详情、窗口花费；日志前缀 `【wXAi 手动刷新】`。

普通 429 即时触发和明确额度快速路径均不是手动刷新：不接受页面传入的指定账号参数。明确额度快速路径使用 usage event 的 auth file 快照定位账号，不执行手动刷新探测。

## 10. CPA proxy-url

每轮有实际 xAI 请求的巡检通过 CPA Management API 读取 `/v0/management/config` 中的 `proxy-url`。

Management API 调用顺序：

1. `GET /v0/management/config` 读取 `proxy-url`。
2. `GET /v0/management/xai-client-version` 读取 CPA 核心 xAI chat executor 使用的 `xaiClientVersionValue`。
3. 两项均读取成功后，创建 xAI HTTP client 并开始账号探测。

`xai-client-version` endpoint 不属于可配置项，只暴露 CPA 核心当前编译值。endpoint 请求失败、返回非 2xx、响应无法解析或版本为空时，本轮实际请求巡检 fail-fast，不发送 responses、chat completions、billing 或 credits 请求。

- `proxy-url` 非空：为 xAI HTTP client 设置该代理。
- `socks5://` 和 `socks5h://` 使用 SOCKS5 Dialer，不通过 `http.Transport.Proxy`。
- SOCKS5 同时兼容标准格式 `socks5://user:password@host:port` 和现网旧格式 `socks5://user:password:host:port`。
- `http://` 和 `https://` 使用 `http.Transport.Proxy`。
- `proxy-url` 为空：xAI HTTP client 直接连接，不读取进程环境代理。
- Management config 读取失败：巡检 fail-fast。
- `proxy-url` 不是包含 scheme 和 host 的合法 URL：巡检 fail-fast。
- CPA auth-files 下载和 priority patch 仍直接访问 CPA Management API，不经过该 xAI 代理 client。
- 服务器巡检、手动刷新、条件巡检在实际发送 xAI 请求前写入 `wXAi 请求代理已配置` 日志，记录 `proxyConfigured`、`proxyMode`、脱敏后的 `proxyHost` 和本次账号数，不记录用户名或密码。
- 手动刷新的 `POST /v1/responses` 必须走该 xAI HTTP client（即 CPA `proxy-url`，含 socks5）；billing/credits 仍经 CPA Management `api-call`，不经该代理 client。
- 请求额度落盘快速路径不创建 xAI HTTP client，不读取 `proxy-url` 或 `xai-client-version`，只通过 CPA Management API 读取 auth file 列表并 patch priority。

## 11. priority 恢复与原始响应

- 健康结果会恢复 `-1/-2/-3/-4` 到 `1`。
- priority adjustment 保存原 priority，避免异常类型变化时丢失初始值。
- responses、chat completions、billing、credits 的 HTTP 响应均追加保存到 `wxai_inspection_http_responses`。
- timeout 没有 HTTP 响应，不写原始响应记录；重试成功后保存成功尝试的响应。
- 明确额度耗尽的业务请求 HTTP 429 直接生成 `isQuota=true`、`errorKind=quota_exhausted`、HTTP 429 的结果，priority 调整为 `-1`；账号类型未知时落盘为 `FREE`。
- 请求额度快速路径复用 `lowerWxaiPriority`：先保存或更新 adjustment，再 patch CPA priority；patch 失败时回滚本次 adjustment，避免形成假冷却。
- 普通业务请求 HTTP 429 只作为条件巡检即时触发信号，不能直接修改 priority。
