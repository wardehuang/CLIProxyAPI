# xAI 服务器巡检与条件巡检规则

> 本文记录 `CPA-Manager-Plus` 当前 wXAi 巡检源码的真实行为。
>
> 最后核对日期：2026-08-12
>
> 权威实现目录：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection`。

## 1. 文档维护要求

修改服务器巡检、条件巡检、手动刷新、额度事件快速路径的候选、请求、代理、状态、priority、冷却、并发、超时、落库或日志行为时，必须同步更新本文。

## 2. priority

| priority | 含义 | 巡检处理 |
| --- | --- | --- |
| `-1` | `free-usage-exhausted` | 服务器/条件巡检在冷却结束前跳过；手动刷新仍探测 |
| `-2` | 普通账号异常或请求异常 | 可继续探测 |
| `-3` | 旧版托管异常 | 可继续探测 |
| `-4` | HTTP 401 | 可继续探测 |
| `-5` | 停用 | 服务器/条件巡检跳过；手动刷新仍探测，成功不恢复 |
| `-6` | JWT 或 SSO bot 标记 | 服务器/条件巡检跳过；手动刷新检查后保持 `-6` |
| `-7` | SSO 检查失败 | 需要重新登录更新 SSO；服务器、条件、手动刷新均继续检查，成功时自动恢复 |
| `-8` | 实时守护判定位置降智的账号异常 | 仅服务器巡检和手动刷新探测；条件巡检跳过；服务器巡检健康后恢复为记录的原始 priority |
| 其他值或空值 | 正常账号 | 按候选规则探测 |

托管 priority 为 `-1/-2/-3/-4/-7/-8`。`-7` 不保存恢复 adjustment，重新登录更新 SSO 且后续探测健康时按账号类型恢复。`-8` 的原 priority 保存在 `wxai_priority_adjustments`；服务器巡检确认健康后恢复该值，未记录原值则保持 `-8` 并让恢复失败显式暴露。

## 3. FREE/SUPER 与 billing

账号类型仅为 `FREE` 或 `SUPER`，保存于 `wxai_account_profiles`，以 `account_key` 为主键。

1. 已知 `FREE`：标准探测只调用 credits。
2. 已知 `SUPER`：调用月度 billing 与 credits；可并发。
3. 类型未知：先调用月度 billing；`monthlyLimit > 0` 判为 `SUPER`，否则判为 `FREE`，随后落盘。
4. 新判为 `SUPER` 后调用 credits。
5. responses 返回 `free-usage-exhausted` 且类型未知时，直接落盘为 `FREE`。
6. billing 或 credits 失败：按探测失败处理；手动刷新不再调用 responses。

billing、credits 与 responses 使用同一轮 xAI HTTP client。billing/credits 使用下载 auth JSON 的 `access_token`，带 `X-XAI-Token-Auth`、billing 固定 client version、`User-Agent`；有 user ID 时带 `x-userid`。

## 4. JWT 与 SSO bot 标记、responses

下载 auth JSON 后，服务器巡检、条件巡检、手动刷新均按以下顺序检查 bot 标记：

1. 解码 `access_token` JWT。
2. `bot_flag_source` 或 `bfs` 存在且不是 `null` 或空白时，立即命中；不执行 SSO 检查。
3. JWT 未命中且 auth JSON 存在非空 `sso` 字段时，使用 auth JSON 的 `proxy_url`、`proxyUrl` 或 `proxy-url` 发起 SSO 检查；不读取或使用 CPA 全局 `proxy-url`。auth 未配置代理时，SSO 检查使用显式直连 transport，不使用进程环境代理。
4. SSO 检查对 `GET https://grok.com/` 设置 `sso` 与 `sso-rw` Cookie，允许重定向，timeout 固定 20 秒，`User-Agent` 为 Chrome 138。
5. SSO 页面 HTTP 200 后将 RSC HTML 中的转义字段还原，解析 `botFlagSource` 和 `botFlagDetails`。`botFlagSource=1/2`，或 `botFlagDetails` 的 `policy=deny` 且 `event=$registration`，均命中；其他值不命中。
6. SSO 字段缺失或空白时跳过 SSO 请求。只要存在 `sso`，SSO 网络错误、非 200、响应过大或页面没有 botFlag 字段均判定 `sso_expired`：设置 priority `-7`、写入诊断详情并结束当前账号，不调用后续 billing、credits、responses。更新 auth JSON 的 SSO 后，后续服务器巡检、条件巡检或手动刷新检查成功，会按正常 priority 恢复规则自动恢复。SSO 请求不写入 `wxai_inspection_http_responses`，仅命中结果或诊断日志落库。
7. JWT 或 SSO 命中都设置 priority `-6`，写入命中来源和完整详情到巡检结果/账号状态详情/日志，结束当前账号，不再调用 billing、credits、responses 或 chat completions。

- `POST https://cli-chat-proxy.grok.com/v1/responses` body 为 `{"model":"grok-4.5","input":"ping","stream":false}`。
- 手动刷新在 billing/credits 成功后调用 responses，并以其结果判定健康。
- 服务器/条件巡检仅在 FREE 账号额度恢复探测时调用 responses。
- 当前巡检不调用 `/v1/chat/completions`，没有 responses fallback。

responses 分类：HTTP 2xx 健康；401 映射 `-4`；`free-usage-exhausted`、`used all the included free usage`、`included free usage has been exhausted` 映射 `-1`；其他 429、402、403 和其他异常映射 `-2`。

## 5. 超时与重试

每个 xAI HTTP 请求使用巡检配置的单次 timeout。timeout 时等待 400ms 后原请求重试一次；非 timeout 网络错误和已取消上下文不重试。规则适用于 responses、billing、credits。

## 6. 额度冷却与 usage event

额度恢复时间保存于 `wxai_priority_adjustments.recover_at_ms`。恢复时间依次从 `Retry-After`、`X-RateLimit-Reset`、响应 JSON 恢复字段解析；无有效值时为当前时间加 24 小时再加 1 分钟。

- 明确额度耗尽 usage event：直接进入快速路径，不创建 run，不下载 auth JSON，不读取 client version，不发送 responses、billing 或 credits；匹配账号后设置 `-1`，写 adjustment、账号类型和可复用 run 的结果/状态/日志。
- SUPER 且 usage event 没有有效冷却时间时，快速路径下载 auth JSON，并通过 CPA `proxy-url` 创建 client 请求 credits 以补充恢复时间。
- 普通 HTTP 429 不直接修改 priority，触发条件巡检。
- 冷却期间服务器巡检跳过；条件巡检在候选生成后过滤；手动刷新不跳过。
- priority patch 失败时回滚本次 adjustment，不形成假冷却。

所有巡检 HTTP 响应的 header 脱敏后保存到 `wxai_inspection_http_responses`；`Authorization`、`Proxy-Authorization`、`Set-Cookie`、`Set-Cookie2` 替换为 `[REDACTED]`。

## 7. 服务器巡检

服务器巡检创建新的 `wxai_inspection_runs`。

触发规则：worker 每 30 秒检查一次配置。`time_points` 模式按时区和最近到期时间点生成 `triggerKey`；同一 `scheduled + triggerKey` 不重复执行；跨越多个时间点只补最近一个。`interval` 模式按最近定时 run 开始时间和 `intervalMinutes` 判断。

候选规则：排除 `priority=-5`、`priority=-6` 和未结束的额度冷却账号；`priority=-7` 仍参与 SSO 状态复检；其余 xAI 账号均参与。

并发规则：并发上限为 `settings.Workers`，最小为 1，不超过候选数。worker 启动按 `workerStartStaggerMs` 错峰，取账号按 `accountTakeStaggerMs` 全局错峰；两个字段缺失或非法时均为 10000ms，`0` 表示不限制。条件巡检复用相同逻辑。

调用顺序：读取 CPA `xai-client-version` 与 `proxy-url`，创建本轮共享代理 client；下载 auth JSON；先检查 JWT bot 标记，JWT 未命中且存在 `sso` 时执行 auth 代理 SSO bot 标记检查；执行 billing/credits；仅 FREE 额度恢复时执行 responses；调整或恢复 priority；保存结果、状态详情、原始响应和窗口花费。

## 8. 条件巡检

条件巡检 worker 启动后立即检查一次，之后每 30 秒检查一次。新 usage event 为 xAI 且 `failed=true`、`fail_status_code=429`、但不能明确判定额度耗尽时，也立即触发一次。

候选查询最近 10 分钟 `usage_events`，要求 `calls > 0` 且 provider 为 xAI。匹配顺序：`accountKey`、`fileName + authIndex`、`provider + accountID`、唯一 `fileName`、唯一 `authIndex`。排除 `priority=-5/-6/-1/-8`，`priority=-7` 保留以复检新 SSO，按账号去重，候选原因为 `active_recent`，再统一过滤冷却账号。

条件巡检复用最近一次非 running 的 run，不创建新 run。普通 429 即时触发与定时 worker 共用本地运行锁和全局巡检锁；已有服务器或条件巡检运行时跳过，不排队、不重试。单账号手动刷新不占用该锁。

## 9. 手动刷新

入口：`POST /v0/management/wxai-inspection/manual-refresh`。

手动刷新不占用全局巡检锁，必须复用最近一次服务器巡检 run；没有 run 时返回 HTTP 400 `至少有过一次服务器巡检`，不探测、不落库。只处理指定单账号，不跳过 `-5` 或 `-1`。

调用顺序：读取 CPA `xai-client-version` 和 `proxy-url`，创建代理 client；下载 auth JSON；先检查 JWT bot 标记，JWT 未命中且存在 `sso` 时执行 auth 代理 SSO bot 标记检查；按 FREE/SUPER 调用 billing/credits；成功后调用 responses；根据结果调整或恢复 priority；写入结果、状态详情、窗口花费和 `【wXAi 手动刷新】` 日志。

成功恢复 `-1/-2/-3/-4/-7`；`-5` 保持停用。失败映射为额度耗尽 `-1`、401 `-4`、其他 `-2`。

## 10. xAI HTTP 代理

服务器巡检、条件巡检、手动刷新和 SUPER 额度恢复 credits 请求，统一使用 CPA `/v0/management/config` 返回的 `proxy-url`。

1. 读取 `proxy-url` 和 CPA `GET /v0/management/xai-client-version`。
2. 使用共享代理构造器创建 HTTP client，支持 `http`、`https`、`socks5`、`socks5h` 和既有 legacy SOCKS5 格式。
3. transport 显式禁用进程 `HTTP_PROXY` / `HTTPS_PROXY`；只使用 CPA `proxy-url`。
4. `proxy-url` 缺失、为空、为 `direct`/`none` 或无法解析时 fail-fast，不直连，不发送 responses、billing 或 credits。
5. 不经 CPA `/v0/management/api-call` 代发 xAI 请求。
6. auth-files 下载、priority patch、`proxy-url` 读取和 client version 读取仍直接访问 CPA Management API。
7. 服务器、条件和手动刷新在创建 client 后写 `wXAi HTTP 客户端已创建（代理）`，记录 `proxyConfigured=true`、`proxyMode=cpa_proxy_url`、账号数。
8. billing/credits 写 `wXAi billing 代理请求诊断`、`wXAi billing 代理响应诊断`、`wXAi billing 代理请求失败`，记录 `transport=proxy`、`viaCPAApiCall=false`、`proxyConfigured=true`、endpoint 和阶段。

## 11. priority 恢复与落库

健康结果会恢复被巡检托管的 priority。`-1/-2/-3/-4/-7` 按账号类型恢复；`-8` 恢复 `wxai_priority_adjustments.original_priority`。priority adjustment 保存原 priority。responses、billing、credits 响应追加保存到 `wxai_inspection_http_responses`；timeout 没有 HTTP 响应时不写记录。服务器巡检写新 run；条件巡检和手动刷新复用 run；明确额度快速路径仅在可复用 run 存在时写结果和巡检日志。

## 12. 独立降智检测

`POST /v0/management/wxai-inspection/tool-call-check` 不属于服务器巡检、条件巡检或手动刷新；不修改 priority，不写巡检 run、结果、状态详情或巡检 HTTP 原始响应。

其代理优先级独立：auth JSON 的 `proxy_url` 优先；为空时使用 CPA 全局 `proxy-url`；都为空时显式直连。该检测请求 `POST /v1/responses`，使用流式 canary，不经 `/v0/management/api-call`。

## 13. CPA xAI 出口节点插件

`cpa-xai-ip-switcher` 是 CPA 动态插件，不属于 Manager 巡检。其维护 xAI auth JSON 的节点槽位和 `proxy_url`，并在流式 responses 成功完成后校验出站 IP、记录节点状态和日志。插件不创建或复用 Manager 巡检 run；配置 `manager_base_url` 和 `manager_management_key` 后，通过 Manager 鉴权 API 向最近一个完成的服务器巡检 run 追加实时守护结果和日志，并同步 `wxai_priority_adjustments`。

### 13.1 智商检测与实时守护判定

- 定时智商检测和实时守护共用插件配置界面的 `qualitySoftTPS`、`qualityHardTPS`。默认值仅用于 SQLite 初始化：`500`、`1000`。
- 定时智商检测请求和实时守护检查的完整 Responses SSE，都识别 `response.reasoning_summary_text.delta`、`response.reasoning_text.delta`、根 `delta_type=thinking_delta` 与对象 `delta.type=thinking_delta`。
- 完整 SSE 按 `output_tokens / generation duration` 计算 TPS。TPS 达到 hard 阈值时，不论是否出现 thinking delta，均判为 `suspected_degradation`，原因 `hard_tps`，等级 `hard`。TPS 达到 soft 阈值且未出现 thinking delta 时，判为 `suspected_degradation`，原因 `soft_tps_missing_thinking_delta`，等级 `soft`。TPS 低于 soft 阈值时，即使未出现 thinking delta 也正常；TPS 位于 soft 与 hard 阈值间且出现 thinking delta 也正常。
- 定时检测将 `thinking_delta`、TPS、阈值和原因持久化到质量尝试记录及日志。实时守护只有 `suspected_degradation` 会执行账号和节点降智处理。上游错误、非 `2xx` HTTP 响应、不完整 SSE 或 SSE 解析失败归为 `unknown`，立即失败返回，不换号、不重载 auth、不替换节点，也不写 `priority=-8`；因此 `429` 仅由 CPA 的额度冷却和 Manager usage event 路径处理。
- 实时守护确认降智时，立即将当前 auth JSON `priority` 设为 `-8`。单个下游请求最多处理 5 个被判降智的账号：前 4 个账号完成标记、节点处理后重载 auth 并换号重试；第 5 个账号仍判降智时不再重试，向调用方返回 `502`。每次重试均重载该 auth 的 `priority` 和 `proxy_url`、清除 session-affinity，并重新选 auth；所有自动调度也排除 `-8`。
- 插件 SQLite 的 `realtime_degradation_failures` 以 `proxy_url` 唯一持久化节点连续降智次数，最多保留最近更新的 1000 行；同节点任一完整正常流将次数清零。第 1 次降智仅记录、标记账号并重试换号；第 2 次连续降智才执行原健康节点替换、保底节点探测和 retry / `502` fail-closed。
- 插件在运行配置保存 `manager_base_url` 和 `manager_management_key`，例如 `http://127.0.0.1:18317`；两项从插件 SQLite 设置读取，并在保存后立即更新当前进程、重启后继续生效。插件不再读取或写入 Manager SQLite。点击检查调用 `GET /v0/management/wxai-inspection/scheduled/latest-completed`，使用 `Authorization: Bearer <manager_management_key>`，返回最近完成的 `scheduled` run ID。
- 实时守护确认降智后，先将当前 auth JSON `priority` 设为 `-8`，再调用 `POST /v0/management/wxai-inspection/realtime-degradation`。Manager 同一鉴权链路先 upsert `wxai_priority_adjustments`，保留已有的 `original_priority`；随后查找最近完成的 `scheduled` run，存在时将该账号的 `wxai_account_status_details.priority` upsert 为 `-8`、保留既有额度和账号类型，再 upsert `wxai_inspection_results` 并插入巡检日志。没有完成的服务器 run 时，仅写 adjustment。Manager 地址、管理密钥、HTTP 调用或落库失败时，插件写 `realtime_guard.manager_api_unavailable`，不阻止账号标记、节点计数或后续替换。
