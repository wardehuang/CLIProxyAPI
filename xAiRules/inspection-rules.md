# xAI 服务器巡检与条件巡检规则

> 本文记录 `CPA-Manager-Plus` 当前 wXAi 巡检源码的真实行为。
>
> 最后核对日期：2026-08-08
>
> 权威实现目录：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection`

## 1. 文档维护要求

修改以下任一行为时，必须同步更新本文：

- 服务器巡检、条件巡检、手动刷新的候选与冷却规则。
- `/v1/responses`、`/v1/chat/completions`、billing、credits 的请求顺序和分类。
- HTTP 状态、额度状态与 priority 的映射。
- FREE/SUPER 类型识别与持久化。
- timeout、重试、xAI HTTP 直连（不走 CPA `proxy-url`，服务器巡检不再使用探测代理池）、结果落库和 priority 恢复。
- 独立「降智检测」的流式请求、auth/global `ProxyURL` 优先级、指标、分类、响应展示和不落库规则。
- xAI 业务请求 HTTP 429 的条件巡检即时触发、并发和 run 复用规则。

若本文与源码冲突，以源码为准，并立即修正文档。

## 2. priority


| priority | 含义                     | 巡检处理                                    |
| -------- | ---------------------- | --------------------------------------- |
| `-1`     | `free-usage-exhausted` | 冷却结束前服务器/条件巡检跳过；手动刷新仍探测                 |
| `-2`     | 普通账号异常或请求异常            | 服务器巡检继续检查；条件巡检可检查；手动刷新可检查               |
| `-3`     | 旧版托管异常值                | 服务器巡检继续检查；条件巡检可检查；手动刷新可检查               |
| `-4`     | HTTP 401               | 服务器巡检继续检查；条件巡检可检查；手动刷新可检查               |
| `-5`     | 停用                     | 服务器/条件巡检跳过 xAI 网络请求；手动刷新仍探测（成功不恢复 `-5`） |
| `-6`     | JWT `bot_flag_source` 或 `bfs` 命中 | 服务器/条件巡检跳过 xAI 网络请求；手动刷新仍检查，命中后保持 `-6` |
| 其他值或空值   | 正常账号                   | 按候选规则检查                                 |


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

billing / credits 出站：

1. 由 Manager **直连** `https://cli-chat-proxy.grok.com/v1/billing` 与 `...?format=credits`。
2. **不**经 CPA `/v0/management/api-call`。
3. 使用与 responses 同一套直连 HTTP client（`Transport.Proxy = nil`），不读 CPA `proxy-url`，不用环境代理。
4. `Authorization: Bearer <access_token>` 使用已下载 auth JSON 中的 token；Header 含 `X-XAI-Token-Auth`、`x-grok-client-version`（billing 固定 `0.2.101`）、`User-Agent`（`grok-pager/0.2.101 ...`），有 userID 时带 `x-userid`。
5. 日志：`wXAi billing 直连请求诊断` / `wXAi billing 直连响应诊断` / `wXAi billing 直连请求失败`，字段含 `transport=direct`、`viaCPAApiCall=false`、`endpoint`、`requestStage`。

手动刷新中的 billing 和 credits 只更新账号类型与额度展示；服务器巡检、条件巡检的标准账号探测使用 billing/credits 请求结果判断探测成功与否。任一路径的 billing/credits 失败均按既有探测失败规则处理。

服务器巡检、条件巡检和手动刷新统一使用同一个直连 HTTP client：`Transport.Proxy = nil`，不读取 CPA `proxy-url`，不使用探测代理池，也不按 FREE/SUPER 选择代理。

## 4. JWT bot 标记与探测请求

下载 auth JSON 后，服务器巡检、条件巡检、手动刷新在实际发送 xAI 请求前都先解码 `access_token` JWT，检查 `bot_flag_source` 与 `bfs`。任一字段存在且值不是 `null` 或空白字符串时，账号命中 JWT bot 标记：设置 priority `-6`、写入命中的 claim 与值、记录 `wXAi 账号命中 JWT bot 标记` 日志，并结束本轮账号处理，不再调用 billing、credits、responses 或 chat completions。

### 4.1 responses 探测

`POST /v1/responses` 只由以下路径调用：

- 手动刷新：billing/credits 成功后调用，结果作为健康判定。
- 服务器巡检、条件巡检：仅 FREE 账号处于额度恢复探测时调用；结果用于判断是否恢复。

请求参数：

- URL：`https://cli-chat-proxy.grok.com/v1/responses`
- Body：`{"model":"grok-4.5","input":"ping","stream":false}`
- `X-XAI-Token-Auth`：`xai-grok-cli`
- `x-grok-client-version`：每轮从 CPA `GET /v0/management/xai-client-version` 读取核心 `xaiClientVersionValue`；当前值为 `0.2.93`。
- `User-Agent`：`xai-grok-workspace/<xaiClientVersionValue>`；当前值为 `xai-grok-workspace/0.2.93`。
- Manager 不保留巡检专用的版本常量，也不在读取失败时回退到硬编码版本。

以下 responses 结果直接使用，不调用 fallback：

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

### 4.2 chat completions

当前源码不调用 `https://cli-chat-proxy.grok.com/v1/chat/completions`，没有 responses fallback。`wxaiChatCompletionsURL` 仅保留为常量，未接入巡检调用链。

## 5. timeout 重试

每个 xAI HTTP 请求使用巡检配置中的单次 timeout。发生 timeout 时：

1. 等待 400ms。
2. 原请求重试一次。
3. 非 timeout 网络错误不重试。
4. 上下文已取消时不重试。

该规则同时应用于 responses、billing 和 credits。

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
2. `priority=-6`：JWT bot 标记集合，不执行 xAI 请求。
3. 额度冷却未结束：冷却集合，不执行 xAI 请求。
4. 其余所有 xAI 账号：全部参与巡检，不再按普通或托管 priority 区分。

探测并发（`workers`）与错峰（可配置）：

1. 同时运行的探测 worker 数上限为 `settings.Workers`（≤0 时按 1；不超过本轮候选账号数）。
2. worker **错峰启动**：第 1 个立即启动，之后每间隔 `settings.workerStartStaggerMs` 毫秒再启动 1 个，直到达到上限。默认 **10000**；`0` 表示不交错（可同时启动至并发上限）。字段缺失/非法时回落默认 10000。
3. **取账号**也全局错峰：任意 worker 开始探测一个账号前，与上一账号开始时刻至少间隔 `settings.accountTakeStaggerMs` 毫秒。默认 **10000**；`0` 表示不限流。第 1 个账号立即开始；第 5、6… 个同样受此间隔约束（当间隔 > 0 时），不是 worker 空闲就立刻开探。
4. 账号经 channel 投递；已启动的 worker 取到账号后先过取账号门闩，再执行 `inspectSingleAccount`。不是一次性为全部账号开线程。
5. 因此同时 in-flight 探测数 ≤ `workers`；当取账号间隔 > 0 且探测耗时大于该间隔时，才会叠满并发。
6. 上下文取消时停止再启动后续 worker，并中断取账号等待；已启动 worker 在 jobs channel 关闭后退出。
7. 条件巡检复用同一 `inspectAccounts` 错峰逻辑与上述配置。
8. `deleteWorkers`（处置并发）对 wXAi 无效：不自动 disable/delete，priority 调整在探测 worker 内同步完成。
9. 配置入口：服务器巡检设置「探测错峰」；落库字段 `workerStartStaggerMs` / `accountTakeStaggerMs`（指针毫秒，Normalize 后始终有值）。旧字段 `*Seconds` 不再读取。

服务器巡检执行顺序：

1. 读取 CPA 核心 `xaiClientVersionValue`，创建本轮共享的 xAI HTTP client（始终直连，不读 CPA `proxy-url`，也不用进程环境代理）。
2. 从 CPA 下载 auth JSON。
3. 读取 `access_token`，解码 JWT 并检查 `bot_flag_source`、`bfs`；命中任一 bot 标记时设置 `-6`，写结果和日志后结束该账号。
4. 账号类型未知时调用 `GET /v1/billing` 判定 `FREE/SUPER` 并落盘。
5. 未知类型解析后的 `SUPER` 调用 credits；已知 `SUPER` 并发调用月度 billing 与 credits；`FREE` 调用 credits。billing/credits 结果作为服务器/条件巡检标准探测结果。
6. 仅 FREE 额度恢复探测调用 `POST /v1/responses`；命中 bot 标记的账号不会调用任何 xAI endpoint。
7. 根据探测结果调整或恢复 priority。
8. 保存结果、状态详情、原始 HTTP 响应和窗口花费。



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
- 排除 `priority=-5`、`priority=-6` 和 `priority=-1`。
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

1. 读取 CPA `xai-client-version`，创建直连 xAI HTTP client（不读 CPA `proxy-url`；billing/credits/responses 共用）。
2. 下载 auth JSON，读取 `access_token`。
3. 解码 JWT：`bot_flag_source` 或 `bfs` 任一存在且值不是 `null` 或空白字符串时，设 `priority=-6` 并结束。
4. 账号类型未知：Manager 直连 `GET /v1/billing` 判定 `FREE/SUPER` 并落盘；失败则按探测失败处理，不调用 responses。
5. 按类型刷新 billing 元数据（须成功，均 Manager 直连）：
  - `SUPER`：`GET /v1/billing?format=credits`（若本轮已做过月度 billing 则只调 credits；否则月度与 credits 并发）。
  - `FREE` 或其他：`GET /v1/billing?format=credits`。
  - billing/credits 失败：按探测失败调整 priority，**不**调用 responses。
6. billing 成功后，**无论 FREE/SUPER**，一律经上述直连 xAI HTTP client 调用：
  - `POST https://cli-chat-proxy.grok.com/v1/responses`
  - Body：`{"model":"grok-4.5","input":"ping","stream":false}`
7. 以 responses 结果判定健康：
  - 成功：恢复托管异常 priority（`-1/-2/-3/-4` → 正常值）；`-5` 不在托管集合，成功后保持停用。
  - 失败：按既有规则映射 `quota_exhausted` → `-1`，401 → `-4`，其他 → `-2`。
8. 写入巡检结果、账号状态详情、窗口花费；日志前缀 `【wXAi 手动刷新】`。

普通 429 即时触发和明确额度快速路径均不是手动刷新：不接受页面传入的指定账号参数。明确额度快速路径使用 usage event 的 auth file 快照定位账号，不执行手动刷新探测。

## 10. xAI HTTP 直连（不走 CPA proxy-url）

服务器巡检、条件巡检、手动刷新在有实际 xAI 网络请求时，统一创建**直连** xAI HTTP client，覆盖：

- `POST /v1/responses`
- `GET /v1/billing`
- `GET /v1/billing?format=credits`

规则：

1. **不**读取 CPA `/v0/management/config` 的 `proxy-url`。
2. **不**使用进程环境 `HTTP_PROXY` / `HTTPS_PROXY` 等代理（`Transport.Proxy = nil`）。
3. **不**经 CPA `/v0/management/api-call` 代发 xAI 请求。
4. 仅 `GET /v0/management/xai-client-version` 读取 CPA 核心 `xaiClientVersionValue`（供 responses 的 client version header）。
5. version 读取成功后创建直连 client，再开始账号探测。
6. billing/credits 使用 auth JSON 中的 `access_token` 直连；token 来自 CPA auth-files 下载，不经 api-call 的 `$TOKEN$` 替换。

`xai-client-version` endpoint 不属于可配置项，只暴露 CPA 核心当前编译值。endpoint 请求失败、返回非 2xx、响应无法解析或版本为空时，本轮实际请求巡检 fail-fast，不发送 responses、chat completions、billing 或 credits 请求。

- 服务器巡检、手动刷新、条件巡检在实际发送 xAI 请求前写入 `wXAi HTTP 客户端已创建（直连）` 日志，记录 `proxyConfigured=false`、`proxyMode=direct` 和本次账号数。
- billing/credits 每次请求写 `wXAi billing 直连请求诊断` / `wXAi billing 直连响应诊断`（或失败时的 `wXAi billing 直连请求失败`），含 `transport=direct`、`viaCPAApiCall=false`。
- 手动刷新的 billing/credits 与 `POST /v1/responses` 均走上述直连 client。
- CPA auth-files 下载和 priority patch 仍直接访问 CPA Management API。
- 请求额度落盘快速路径默认不创建 xAI HTTP client；仅 SUPER 且 usage 无有效冷却、需补 credits 恢复时间时，临时创建直连 client 调 credits（同样不经 api-call）。
- CPA 后台配置的 `proxy-url` 仅影响 CLIProxyAPI 业务流量，**不影响** Manager 侧 wXAi 巡检探测（含 billing/credits）；独立降智检测按第 12 节的 auth/global 优先级处理。



## 11. priority 恢复与原始响应

- 健康结果会恢复 `-1/-2/-3/-4` 到 `1`。
- priority adjustment 保存原 priority，避免异常类型变化时丢失初始值。
- responses、chat completions、billing、credits 的 HTTP 响应均追加保存到 `wxai_inspection_http_responses`。
- timeout 没有 HTTP 响应，不写原始响应记录；重试成功后保存成功尝试的响应。
- 明确额度耗尽的业务请求 HTTP 429 直接生成 `isQuota=true`、`errorKind=quota_exhausted`、HTTP 429 的结果，priority 调整为 `-1`；账号类型未知时落盘为 `FREE`。
- 请求额度快速路径复用 `lowerWxaiPriority`：先保存或更新 adjustment，再 patch CPA priority；patch 失败时回滚本次 adjustment，避免形成假冷却。
- 普通业务请求 HTTP 429 只作为条件巡检即时触发信号，不能直接修改 priority。



## 12. 独立降智检测

wXAi 账号详情的「降智检测」不是服务器巡检、条件巡检或手动刷新，不修改 priority，不写入 `wxai_inspection_runs`、结果、状态详情、HTTP 原始响应或巡检数据库日志，也不复用巡检 run。入口：`POST /v0/management/wxai-inspection/tool-call-check`。

账号选择与调用顺序：

1. 读取 CPA auth file 列表，按 `accountKey`、`fileName + authIndex` 选择一个 xAI 账号。
2. 下载该 auth JSON，读取 `access_token`。
3. 读取 auth 根字段 `proxy_url`。非空时使用 auth 级代理；为空时读取 CPA `GET /v0/management/config` 的 `proxy-url` 作为全局代理；两者都为空时使用显式直连 transport。auth 级值优先，不因值非法而回退全局代理。
4. 读取 CPA `GET /v0/management/xai-client-version`，使用返回版本组装 xAI CLI headers（`Accept: text/event-stream`）；不使用 Manager 内部固定 client version。
5. 用该 token 对 `https://cli-chat-proxy.grok.com/v1/responses` 发起一次实际 `POST`，不经过 CPA `/v0/management/api-call`。
6. 请求使用 `stream=true`、`reasoning.effort=high`、`reasoning.summary=detailed`、`max_output_tokens=96`、`temperature=0`；不携带 `tools` 字段。读取至 `response.completed`、`[DONE]` 或失败终止事件后返回检测结果；不执行模型返回的工具调用，不发送第二次请求。

短 canary 只用于检测流式形态与答案回显。分类仍使用原有 TPS 规则，不把 `thinking_delta` 或答案字段直接作为降智分类条件。

短 canary 固定要求：

- Prompt：`用中文回答：17 × 23 等于多少？只输出计算过程和答案。`
- 期望答案：`391`。
- `reasoning.effort`：`high`。
- `reasoning.summary`：`detailed`。
- `max_output_tokens`：`96`。
- `temperature`：`0`。
- 不携带 `tools`。

本检测只读取流式响应，不执行任何模型工具调用。

```json
{
  "model": "grok-4.5",
  "input": "用中文回答：17 × 23 等于多少？只输出计算过程和答案。",
  "stream": true,
  "reasoning": {
    "effort": "high",
    "summary": "detailed"
  },
  "max_output_tokens": 96,
  "temperature": 0
}
```

请求体不包含 `tools` 字段。答案期望包含 `391`。

ProxyURL：

- 支持 CPA 相同的 `http`、`https`、`socks5`、`socks5h`、`direct`、`none` 语义。
- SOCKS5 标准格式为 `socks5://username:password@host:port`；为兼容现有 xAI auth JSON，也接受 `username:password:host:port` 与 `socks5://username:password:host:port`，Manager 在建立 SOCKS5 dialer 前统一转换为标准格式。该兼容格式要求恰好四段，不能表达未转义的冒号密码或 IPv6 主机；此类值必须改为标准 URI 并按 URL 规则转义。
- 代理地址返回前脱敏，不返回代理认证信息；请求头中的 `Authorization`、`Proxy-Authorization` 和响应中的 cookie 类 header 脱敏。
- auth 级、全局级和直连来源在结果中分别标记为 `auth`、`global`、`direct`。

流式指标和主动判断：

- `TTFB`：从请求开始到响应 body 首字节被读取，仅作为诊断指标，不参与质量判定。
- 流读取：按 `grok2api` 相同的 `32 KiB` 原始响应 chunk 读取；跨 chunk 仅解析完整 `data:` SSE 行。每个 chunk 的顺序固定为：解析首生成候选 → 写入本次检测的原始 SSE 结果缓冲区 → 提交首生成时间。
- `firstTokenMs`：从请求开始到首个已提交的 Responses 生成 delta 的毫秒数。候选必须为非空 `response.output_text.delta`、`response.reasoning_summary_text.delta`、`response.reasoning_text.delta`、`response.refusal.delta`、`response.function_call_arguments.delta` 或 `response.custom_tool_call_input.delta`；创建事件、output item 增删事件、空 delta、注释、usage 和 `[DONE]` 均不计入。
- 本检测不向浏览器逐段转发 SSE；因此原始 SSE 结果缓冲区的成功写入是 `grok2api` 下游 `writer.Write` + `Flush` 的等价提交点。事件矩阵、chunk 边界和“先解析、后提交、再计时”顺序与 `grok2api` 一致。
- 流终止：`response.completed` 或 `[DONE]` 为成功终止；`response.incomplete`、`response.failed`、`error` 为本次检测错误。EOF 前的未完成残留 SSE 行仍会解析；无成功终止的 2xx 流为 `unknown`。
- `generationMs`：`totalMs - firstTokenMs`；存在首生成事件时最小按 1ms 计算。
- `total`：从请求开始到完整 Responses SSE 流读取结束或终止事件的毫秒数；API 同时返回 `durationMs` 和 `totalMs`。
- `outputTokens`：从 `response.completed.response.usage.output_tokens` 读取。xAI Responses 的 `total_tokens = input_tokens + output_tokens`，故此字段**已包含** `output_tokens_details.reasoning_tokens`；TPS 分子直接使用 `outputTokens`，绝不再加 reasoning tokens。
- `reasoningTokens`：从 `response.completed.response.usage.output_tokens_details.reasoning_tokens` 读取，仅用于展示和计算可见输出。
- `visibleTokens`：`outputTokens - reasoningTokens`；结果不为正但存在可见文本时，按 `(可见字符数 + 3) / 4` 估算。
- `outputTokensPerSecond`：`outputTokens * 1000 / generationMs`，其中 `generationMs = totalMs - firstTokenMs`；`outputTokens` 已含 reasoning tokens。
- `thinkingDelta`：Responses 的 `response.reasoning_summary_text.delta` 或 `response.reasoning_text.delta` 事件出现时为 `true`；同时兼容流中出现 `thinking_delta` 类型的 delta。该字段只展示，不参与分类。
- `answerMatched`：`modelAnswer` 可见文本包含期望答案 `391` 时为 `true`；该字段只展示，不参与分类。
- `outputTokensPerSecond >= 1000`：硬异常，原因 `hard_tps`。
- `outputTokensPerSecond >= 500` 且低于 `1000`：软异常，原因 `soft_tps`。
- 未命中上述 TPS 阈值：正常，原因 `within_threshold`。
- 软/硬异常均返回分类 `suspected_degradation`，另以 `qualityLevel=soft/hard` 区分等级。
- `free-usage-exhausted` 类错误仍单独返回 `quota_exhausted`，不参与 soft/hard TPS 判断。
- 其他 HTTP 错误、网络错误或流读取错误：`unknown`；错误码和原始错误仍展示。
- `modelAnswer` 只拼接可见 `response.output_text.delta`，不拼接 reasoning、refusal 或工具参数文本；不执行工具、不发送第二次请求。

结果、timeout 和日志：

- 单次 timeout 复用 wXAi 巡检设置 `settings.Timeout`，不重试；Web 端「降智检测」请求等待上限为 10 分钟，其他巡检接口继续使用各自的常规等待上限。
- 结果弹窗展示分类、质量等级、判定原因、HTTP 状态码、首字节 TTFB、首生成 token、generation、total、TPS、output tokens、reasoning tokens、visible tokens、是否有 `thinking_delta`、答案是否包含 `391`、答案文本、代理来源、请求体、脱敏请求头、响应头和完整 SSE body（最大 4 MiB）。
- 页面内只保留最后一次检测返回的降智检测结果：新检测完成后替换旧结果；关闭弹窗只隐藏结果，不删除结果；「上次检测结果」按钮重新打开该结果；首次检测前按钮禁用。结果保存在前端运行期共享内存，切换到其他界面再返回时仍可打开；刷新页面、关闭前端应用或组件热重载后清空，不写入后端、数据库或巡检 run。
- 不创建、不删除临时文件；本流程只做流式回答检测。
- 每次检测写入 Manager Server 运行日志 `wXAi 降智检测操作日志`，按 `started`、运行时解析、账号列表、auth 下载、全局 proxy 查询、proxy 解析、client version、上游请求开始/结束记录阶段和耗时；结束日志记录 `statusCode`、`ttfbMs`、`firstTokenMs`、`generationMs`、`totalMs`、`outputTokensPerSecond`、`outputTokens`、`reasoningTokens`、`visibleTokens`、`thinkingDelta`、`expectedAnswer`、`answerMatched`、`qualityLevel`、`classificationReason`、`errorCode`、`classification` 和错误。
- 操作日志不记录 access token、Authorization header 或代理认证信息。
- HTTP 非 2xx、网络错误、响应读取错误都只作为本次检测结果返回，不触发 priority、额度冷却、billing、chat fallback 或巡检流程。



## 13. CPA xAI 出口节点插件（`cpa-xai-ip-switcher`）

本节记录 CLIProxyAPI 插件当前源码的真实行为。插件独立于 Manager wXAi 账号巡检：不执行 FREE/SUPER 判断，不写入账号 `priority`，不调用 `/v1/chat/completions`，不创建或复用 Manager 巡检 run。

### 13.1 触发、槽位与候选

- 初次探测：节点录入后进入 `unprobed`，由初次探测 worker 领取；成功先进入 `connected`，尚未进入健康槽位。
- 保活调度器：插件启动后立即执行首轮，之后按 `keepalive_interval_seconds` 调度。第一阶段在轮次开始时快照 `initial_connected=1` 且状态为 `healthy`、`healthy_candidate`、`connected` 或 `cooldown` 的节点；同一批次仍有 `unprobed`、`probing` 或 `probe_kind=initial` 节点时，该批次候选全部跳过。
- 保活第一阶段结束后，才启动同一轮第二阶段智商探测；两阶段都结束后才完成保活轮次。取消时分别恢复连通探测和智商探测领取前状态。
- 复活调度器：插件启动后立即执行首轮，之后按 `revive_interval_seconds` 调度。只快照 `error`、`probe_kind=''`、`revive_failure_count < 3` 且 `exit_country` 为空或为 `US` 的节点；已确认非 US 的异常节点不再进入复活候选。
- 健康槽位默认 `50` 个，健康备选槽位默认 `20` 个；槽位总数最多 `1000`。槽号 `1..healthy_slot_count` 为 `healthy`，其余为 `healthy_candidate`。健康备选槽位不参与 auth `proxy_url` 分配。
- 空槽候选只从 `connected` 节点领取，按 `latency_ms ASC, id ASC` 排序；领取时原子改为 `quality_probing`，写入槽位处理租约和 `claim_node_id`，`ip_slots.node_id` 在检测通过前保持为空。`claim_node_id` 只用于处理中槽位展示和中断恢复，完成、失败或取消时清零。
- 正式补槽只发生在质量结果为 `normal`：节点按槽位类型改为 `healthy` 或 `healthy_candidate`，再原子写入 `ip_slots.node_id`。`connected` 不是直接补槽条件。

### 13.2 endpoint 与调用顺序

初次、保活和复活的连通探测：

1. 使用节点自身代理访问 `GET https://grok.com/`。
2. 初次探测在 xAI endpoint 成功后，使用同一代理访问 `GET http://163.192.9.157:2261/trace`，持久化出口 IP 和国家代码。
3. 复活探测仅在 `exit_country` 为空时调用出口 trace；已知 `US` 时只调用 xAI endpoint。
4. 初次或复活发现非 US 时，结果为 `非us出口`。初次批次 `delete_non_us=1` 时删除节点，否则保存异常；复活路径保留节点并清零复活失败计数，使其不再进入后续复活候选。

保活第二阶段和空槽补槽的智商探测：

1. 通过 Host ABI `host.auth.list`、`host.auth.get` 读取 xAI auth JSON；质量探测只选择 `priority > 0`、未禁用且存在 `access_token` 的 auth。
2. 选择顺序为当前节点已绑定 auth 文件名排序后的首个、该节点历史成功 auth、全局最近 10 次随机选择排除后的随机 auth。最近 10 次口径只统计 `selection_source='random' AND was_success=0` 的随机领取记录，不把绑定命中、历史命中或成功结果重复计入随机窗口。额度耗尽时把当前 auth 加入本轮已用集合，立即从随机池切换；候选 auth 全部耗尽时结果为 `unknown/auth_pool_exhausted`，不误伤节点。
3. 使用节点自身代理调用 `POST https://cli-chat-proxy.grok.com/v1/responses`，请求体固定为 `grok-4.5`、中文 `17 × 23` 探针、`stream=true`、high/detailed reasoning、`max_output_tokens=96`、`temperature=0`；期望答案 `391` 只作展示信号。
4. 读取完整 SSE，成功条件为 `response.completed` 或 `[DONE]`；`response.incomplete`、`response.failed`、`error`、网络错误、超时和不完整流均为非 `normal` 结果。按 `output_tokens × 1000 / max(1, total_ms-first_token_ms)` 计算 TPS；`TPS >= hard` 为 `suspected_degradation/hard`，`TPS >= soft` 为 `suspected_degradation/soft`，否则为 `normal/healthy`。
5. 智商探测不调用 Manager API，不调用 `/v1/chat/completions`，不执行 FREE/SUPER 判断，不修改账号 priority。

### 13.3 状态、恢复、保底与 auth 分配

- 连通阶段成功：初次节点为 `connected`；保活节点恢复其原状态。
- 保活连通失败：`healthy`、`healthy_candidate`、`connected` 来源改为 `error`，复活目标为 `connected`；`cooldown` 来源改为 `error`，复活目标为 `cooldown`。保活遇到“连接数不足”时保持原状态并记为不可判定。
- 槽位已有节点时，本轮先对该节点做智商探测。质量结果为 `normal`，槽位保持占用；`soft`、`hard` 或普通请求/SSE 失败时节点改为 `cooldown`，清空槽位并继续领取下一个候选。
- 空槽新候选质量结果为 `normal` 才正式占槽；`soft`、`hard` 或普通请求/SSE 失败时节点改为 `cooldown`，清空处理租约并继续当前槽位；auth 池耗尽或不可判定时节点恢复 `connected`，槽位保持空并阻塞该槽位到本轮结束。
- 保底节点：质量阶段优先领取 `healthy_fallback` 节点，先做连通探测，再做智商探测。任一步失败永久删除并继续领取；通过后按槽位类型占槽并保留 `fallback_origin`。下一轮创建后、生成连通候选快照前，先清槽并永久删除上一轮已入槽的保底来源，同时立即重算健康槽位 auth 分配；该保底节点不会再进入下一轮连通探测。当前轮质量阶段结束后，未占槽的溢出 `connected` 节点转为 `healthy_fallback`。
- 槽位数量变化时，普通占槽节点释放为 `connected`，已入槽保底来源永久删除，绑定表清空，随后按新布局重新执行候选领取和智商探测。
- 健康槽位 auth 分配：以 Host Auth List 返回的全部 xAI auth 为全集，不过滤 `priority`；按文件名排序，文件序号从 1 开始，使用 `slot=((fileIndex-1)%healthySlotCount)+1`。健康槽位为空时对应 auth 删除根字段 `proxy_url`。每个文件独立读取；读取失败也按其列表文件名和槽号写入 `ip_slot_auth_bindings.failed`，不会静默跳过。读取成功后执行 Host Auth Save 并重新读取验证，逐文件写入 `verified` 或 `failed` 状态；失败不伪造跨文件事务。
- 质量探测配置默认 `quality_worker_count=8`、单次超时 `25` 秒、soft TPS `500`、hard TPS `1000`；hard 必须大于 soft。配置变化先停止当前 worker并等待取消恢复完成，再重建槽位、保存设置、同步 auth 分配并启动新 worker；槽位重建或设置保存失败时按旧配置恢复 worker。
- 初次探测 worker、保活连通 worker、智商探测 worker、复活 worker 分别受配置并发数限制；连通探测默认 HTTP 总超时 `25` 秒、拨号/TLS 超时 `15` 秒、空闲轮询 `750` 毫秒；质量探测超时可配置，最大 `600` 秒；单节点重试次数使用 `probe_retry_count`。

### 13.4 落库、API、UI 与日志

- SQLite 保存 `ip_nodes`、`ip_batches`、`keepalive_rounds`、`keepalive_round_nodes`、`revive_rounds`、`revive_round_nodes`、`ip_slots`、`ip_slot_auth_bindings`、`quality_probe_attempts`、`auth_selection_history`、`plugin_settings` 和 `plugin_logs`。
- `ip_nodes` 持久化状态、复活目标、质量延迟标记、出口 IP/国家、失败原因和探测时间；`ip_slots.node_id` 保存正式占槽节点，`claim_node_id` 保存处理中节点，`fallback_origin` 保存保底来源。服务启动时会把中断中的节点探测恢复到 `probe_return_status` 或复活目标状态，并清理遗留槽位租约和 `claim_node_id`。
- 保活轮次保存连通阶段完成时间、质量阶段开始/完成时间、两阶段计数；轮次只有两阶段结束才记为完成。质量每次 auth 尝试保存 auth、选择来源、代理、HTTP、TTFB、首生成、生成/总耗时、tokens、TPS、分类、阈值结果和错误详情；不保存 token 或完整 auth JSON。
- 批次日志使用 `batch_probe`，保活连通/智商/保底日志使用 `keepalive_probe`，复活日志使用 `revive_probe`；日志记录轮次、阶段、槽位、节点、auth 选择来源、代理、HTTP/SSE、指标、分类、状态转换、换号、保底删除和 auth 同步结果。
- IP 列表提供 `healthy`、`healthy_candidate`、`healthy_fallback`、`quality_probing` 等状态页签及槽位/保底来源展示；处理中节点通过 `claim_node_id` 显示目标槽位。保活日志批次摘要展示连通阶段与智商阶段的候选、成功和失败计数。`healthy` 节点每行显示“查看绑定 auth”按钮，通过 `GET /nodes/{nodeID}/auth-bindings` 查询当前绑定记录、已验证数和失败数；非健康节点不显示。接口不返回 token 或完整 auth JSON，无绑定时返回“当前无绑定 auth 文件”。
- 批次固定保存初次探测完成数和初次已连通数；实时已连通统计按当前 `healthy`、`healthy_candidate`、`connected`、`cooldown` 状态计算。插件不创建或复用 Manager 巡检 run。
