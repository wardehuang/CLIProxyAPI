# xAI 服务器巡检与条件巡检规则

> 本文记录 `CPA-Manager-Plus` 当前 wXAi 巡检源码的真实行为。
>
> 最后核对日期：2026-07-17
>
> 权威实现目录：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection`

## 1. 文档维护要求

修改以下任一行为时，必须在同一任务中同步更新本文：

- 服务器巡检触发、候选筛选、冷却规则。
- 条件巡检触发、候选窗口、账号匹配规则。
- access token JWT 前置判定，以及 billing、credits、chat completions 的 URL、请求顺序、调用条件或请求头。
- HTTP 状态、额度状态、Cloudflare 状态到 priority 的映射。
- priority 降级、恢复、停用、异常区间规则。
- 并发、超时、重试、落库、日志和 run 复用规则。
- 手动单账号刷新与服务器巡检、条件巡检的差异。

若本文与源码冲突，以源码为准，并立即修正文档。禁止只改源码不更新本文。

## 2. 术语和 priority

| priority | 含义 | 当前处理 |
|---:|---|---|
| `-1` | 额度耗尽 | 服务器巡检对 FREE 账号执行 24 小时冷却；条件巡检排除 |
| `-2` | 请求或账号异常 | 服务器巡检会重新检查；条件巡检可检查 |
| `-3` | 旧版托管异常值 | 当前不会主动写入；服务器巡检和条件巡检均可检查 |
| `-5` | 停用 | 所有自动巡检跳过网络请求 |
| 其他值或空值 | 正常账号 | 服务器巡检只检查 billing；条件巡检检查 billing 和 chat |

托管 priority 指 `-1/-2/-3`。

## 3. 服务器巡检

### 3.1 触发方式

服务器巡检会创建新的 `wxai_inspection_runs`：

- 手动入口：`POST /v0/management/wxai-inspection/run`。
- 定时 Worker：`WxaiInspectionWorker`。
- Worker 启动时立即检查一次，之后每 30 秒检查一次。

定时触发必须满足：

1. Manager Server 已配置 CPA 地址和 management key。
2. wXAi 巡检配置 `enabled=true`。
3. 当前没有服务器巡检或条件巡检占用 `Service` 全局运行锁。
4. schedule 已到期。
5. 相同 `triggerType + triggerKey` 尚未生成 run。

手动 `/run` 不检查 `enabled`，只要运行配置完整就可执行。

### 3.2 默认配置

- schedule：`interval`
- interval：60 分钟
- workers：4
- 单次 HTTP 请求 timeout：25000 ms
- HTTP client 总 timeout：60 秒
- retries：0，当前实现没有重试循环
- used percent threshold：100
- auto action mode：仅允许 `none`
- sample size：当前不参与候选筛选

### 3.3 账号来源

服务器从 CPA auth-files 管理接口读取认证文件，只保留 provider 归一化为以下值的账号：

- `xai`
- `x-ai`
- `grok`

每个账号的内部 `accountKey` 由以下字段组成：

`fileName|displayAccount|authIndex|accountID`

重复 key 会追加 `|duplicate-N`。

### 3.4 候选筛选顺序

1. `priority=-5`：加入停用集合，不执行网络探测。
2. priority 不是 `-1`：加入实际巡检集合。
3. `priority=-1`：进入 FREE 24 小时冷却判断。

因此普通 priority、`-2`、`-3` 都会进入服务器巡检。

### 3.5 FREE `-1` 的 24 小时冷却

只有同时满足以下条件才跳过本轮网络请求：

1. 当前 priority 为 `-1`。
2. 最近一次 run 存在相同 `accountKey`。
3. 最近状态的 `accountType=FREE`。
4. 存在 `wxai_priority_adjustments` 记录。
5. adjustment 的目标 priority 为 `-1`。
6. adjustment 有有效更新时间。
7. 当前时间早于 adjustment 更新时间加 24 小时。

冷却期间：

- 不调用 billing、credits、chat completions。
- priority 保持 `-1`。
- 新 run 仍保存该账号结果。
- 额度、重置时间和检查时间复用上一轮数据。

SUPER `-1`、冷却已结束、没有有效 adjustment 或无法确认上一轮 FREE 状态时，重新执行实际巡检。

### 3.6 停用账号

`priority=-5` 时：

- 不下载 auth JSON。
- 不读取 access token。
- 不调用任何 xAI endpoint。
- 不自动恢复 priority。
- 在新 run 中保存 `keep + skipped` 状态。
- 有历史数据时复用上一轮额度和检查时间。

### 3.7 单账号请求顺序

非停用、非冷却账号按以下顺序执行：

1. 从 CPA 下载目标 auth JSON。
2. 读取现有 `access_token`。
3. 本地解码 JWT header 和 payload，不校验签名。
4. JWT 解码失败时，判定 `account_abnormal`，priority 降为 `-2`，不调用任何 xAI endpoint。
5. JWT payload 的 `bot_flag_source=1` 时，本地判定 HTTP `403` 和 `account_abnormal`，priority 降为 `-2`，不调用任何 xAI endpoint。
6. 其他 JWT 继续调用 `GET https://cli-chat-proxy.grok.com/v1/billing`。
7. 根据 monthly billing 判断 FREE/SUPER。
8. 仅 SUPER 调用 `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits`。
9. 判断 billing 是否额度耗尽。
10. 额度未耗尽时，按服务器巡检 chat 规则决定是否调用 `POST https://cli-chat-proxy.grok.com/v1/chat/completions`。
11. 根据结果调整或恢复 priority。
12. 保存结果、状态详情、原始 HTTP 响应和窗口花费。

服务器巡检不会刷新 OAuth token，也不会回写 `access_token`。JWT header 当前只负责完成结构解码，不校验 `typ`、`alg` 或 `kid` 的具体值。

### 3.8 FREE/SUPER 判断

当前不读取显式套餐字段：

- `monthlyLimit > 0`：`SUPER`
- 其他情况：`FREE`

只有 SUPER 调用 `billing?format=credits`。

### 3.9 credits 失败规则

- 网络失败：保留 monthly billing 结果，不直接降级，继续后续判断。
- HTTP 401/403：账号异常，priority 降为 `-2`。
- 其他非 2xx：忽略 credits 失败，保留 monthly billing 结果。
- 2xx 但响应解析失败：`request_error`，priority 降为 `-2`。

最终 `statusCode` 通常来自 monthly billing 或 chat，不使用普通 credits 非认证错误的状态码覆盖。

### 3.10 服务器巡检的 chat 调用条件

请求固定为：

- URL：`https://cli-chat-proxy.grok.com/v1/chat/completions`
- model：`grok-4.5`
- prompt：`1+1=?`
- stream：`false`

服务器巡检仅在以下条件全部满足时调用 chat：

1. billing 成功。
2. billing 未判定额度耗尽。
3. 当前 priority 是 `-1/-2/-3`。

普通 priority 账号在服务器巡检中只调用 billing，不调用 chat。

## 4. 条件巡检

### 4.1 触发方式

条件巡检由 `WxaiConditionalInspectionWorker` 执行：

- Worker 启动时立即检查一次。
- 之后每 30 秒检查一次。
- 不创建新 run。
- 使用最近一次非 running 的 wXAi run ID。

触发必须满足：

1. Manager Server 已配置 CPA。
2. wXAi 巡检 `enabled=true`。
3. `Service` 当前没有服务器巡检或其他条件巡检。
4. 条件 Worker 自身没有正在运行的任务。
5. 至少存在一个历史 wXAi run。
6. 最近 run 不是 `running`。

### 4.2 候选时间窗口

条件巡检查询最近 10 分钟的 `usage_events`：

`[now - 10分钟, now)`

事件必须满足：

- `calls > 0`
- provider 归一化为 `xai`

不要求请求成功。失败请求只要形成 usage event，也可使账号进入候选。

### 4.3 账号匹配顺序

usage event 与当前 CPA auth 文件按以下顺序匹配：

1. `accountKey`
2. `fileName + authIndex`
3. provider `xai` + `accountID`
4. 唯一 `fileName`
5. 唯一 `authIndex`

匹配失败时跳过该事件。

### 4.4 候选排除

以下账号不会进入条件巡检：

- `priority=-5`
- `priority=-1`

以下账号可以进入：

- 普通 priority
- `priority=-2`
- `priority=-3`

同一账号去重。当前候选原因只有 `active_recent`。

### 4.5 条件巡检请求规则

条件巡检复用服务器巡检的 auth 下载、billing、SUPER credits、额度判断和 priority 映射，但不执行服务器巡检专用的 JWT `bot_flag_source` 前置判定。

关键差异：只要候选账号 billing 成功且额度未耗尽，条件巡检总会调用 chat completions，不限制 priority 是否为 `-1/-2/-3`。

### 4.6 空候选

没有 xAI 账号或没有有效候选时：

- 直接返回最近 run。
- 不创建新 run。
- 不更新 run 统计。
- 当前实现不写“开始”“无候选”“结束”数据库日志。

### 4.7 run 复用和覆盖

条件巡检把结果写回最近一次服务器巡检的 run：

- `wxai_inspection_results` 唯一键为 `(run_id, account_key)`。
- 相同账号结果会覆盖该 run 中原有结果。
- `created_at_ms` 会更新。
- 原始 HTTP 响应表使用追加记录，不覆盖旧响应。
- 条件巡检不重新计算 run 的总数、异常数、额度耗尽数或完成时间。

## 5. 结果到 priority 的映射

| 结果 | ErrorKind | priority 行为 |
|---|---|---|
| auth JSON 下载失败 | `request_error` | 降为 `-2` |
| access token 缺失 | `account_abnormal` | 降为 `-2` |
| 服务器巡检 JWT 解码失败 | `account_abnormal` | 不发 xAI 请求，降为 `-2` |
| 服务器巡检 JWT `bot_flag_source=1` | `account_abnormal`，本地 HTTP `403` | 不发 xAI 请求，降为 `-2` |
| monthly billing 网络或解析失败 | `request_error` | 降为 `-2` |
| monthly billing 401/403 | `account_abnormal` | 降为 `-2` |
| monthly billing 其他非 2xx | `account_abnormal` | 降为 `-2` |
| SUPER credits 401/403 | `account_abnormal` | 降为 `-2` |
| billing 达到额度阈值 | `quota_exhausted` | 降为 `-1` |
| monthly used >= monthly limit | `quota_exhausted` | 降为 `-1` |
| chat 429 | `quota_exhausted` | 降为 `-1` |
| chat 403 且 body 包含 `cloudflare` | `cloudflare_blocked` | 保持当前 priority |
| chat 其他失败 | `account_abnormal` 或 `request_error` | 降为 `-2` |
| billing 成功且 chat 成功或按规则跳过 | 空 | 执行恢复逻辑 |

额度恢复时间优先取未来 weekly/monthly reset；没有有效 reset 时使用当前时间加 30 秒。

## 6. priority 恢复

成功探测后的恢复目标固定为 `1`，不是原始 priority。

- 当前 priority 与 adjustment 的 adjusted priority 一致：patch 为 `1`，关闭异常区间，删除 adjustment。
- 没有 adjustment，但当前 priority 为 `-1/-2/-3`：仍 patch 为 `1`。
- adjustment 存在，但当前 priority 已不等于 adjusted priority：视为 stale adjustment，只删除 adjustment，不改当前 priority。
- Cloudflare 分支不降级，也不执行恢复。
- FREE 冷却账号不执行恢复。
- `priority=-5` 不执行恢复。

priority adjustment 会先写数据库，再 patch CPA auth 文件。patch 失败时可能留下 adjustment，并在后续运行中由 stale 逻辑处理。

## 7. 并发、超时和重试

- 服务器巡检和条件巡检共享 `Service` 全局运行锁，二者互斥。
- 条件 Worker 另有自身运行锁，防止两个 tick 重叠。
- 每次巡检使用 `settings.workers` 并发检查不同账号，默认 4。
- 同一账号内部请求串行：auth 下载、monthly billing、credits、chat、priority patch。
- 每个 xAI HTTP 请求使用独立 timeout，默认 25 秒。
- HTTP client 总 timeout 为 60 秒。
- 当前没有 run 总 timeout，也没有单账号总 timeout。
- `retries` 配置当前没有实际重试实现。

## 8. 落库

### 8.1 服务器巡检

创建并更新：

- `wxai_inspection_runs`
- `wxai_inspection_results`
- `wxai_account_status_details`
- `wxai_priority_adjustments`
- `wxai_account_priority_intervals`
- `wxai_inspection_http_responses`
- `wxai_account_window_costs`
- `wxai_inspection_logs`

### 8.2 条件巡检

- 不创建新 run。
- upsert 最近 run 的账号结果和状态详情。
- 追加原始 HTTP 响应。
- 更新窗口花费。
- 不更新 run 汇总统计。

原始 HTTP response body 最多保存 1 MiB，超出时截断。JWT 解码失败或 `bot_flag_source=1` 属于本地前置结果，没有实际 HTTP 调用，因此不会写入 `wxai_inspection_http_responses`。

## 9. 日志

### 9.1 服务器巡检

run 日志通常包括：

- 巡检开始。
- 候选筛选统计。
- 账号加载或筛选失败。
- priority 调整和恢复。
- 请求异常。
- 结果写入失败。
- 巡检完成。

### 9.2 条件巡检

当前只在以下情况写 run 数据库日志：

- 候选账号刷新完成。
- auth 文件加载失败。
- 候选解析失败。
- 条件巡检取消。

当前不会写：

- 条件巡检 tick 开始。
- 条件巡检实际触发。
- 无候选。
- 候选数量汇总。
- 正常结束汇总。
- billing/chat 内部过程日志。

条件巡检实际探测使用 `quietLogger`，只在探测完成后逐账号写 `【wXAi 条件巡检】账号刷新完成`。

Worker 自身错误通过 `log.Printf` 写进程日志，不属于 `wxai_inspection_logs`。

## 10. 手动单账号刷新差异

入口：`POST /v0/management/wxai-inspection/manual-refresh`

- 不创建新 run，写回最近 run。
- 不检查 `enabled`。
- 不使用 `Service` 全局运行锁，可能与其他巡检并发。
- 普通 priority 账号也调用 chat。
- `priority=-1` 不受服务器巡检 24 小时 FREE 冷却限制。
- `priority=-5` 不执行网络探测，只保存停用状态。
- 不执行服务器巡检专用的 JWT `bot_flag_source` 前置判定。
- 不重新计算 run 汇总统计。

## 11. wXAi 账号状态页面

页面：`CPA-Manager-Plus/apps/web/src/features/monitoring/WxaiInspectionPage.tsx`。

- 状态列只显示状态标签：`已启用`、`已停用`、`额度耗尽` 或 `账号异常`；不显示 HTTP 状态码、错误类别或错误详情。
- 每行最多一项额度窗口且最多一项窗口花费时，使用与 Codex 账号状态页相同的单行紧凑样式；多额度窗口或多窗口花费时保留多行高度。
- 错误详情仍保留在巡检结果数据中，页面状态列不渲染该信息。

## 12. 关键源码索引

- 运行主流程：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/service.go`
- 单账号探测与结果映射：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/wxai_model_probe.go`
- access token JWT 前置判定：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/access_token_precheck.go`
- billing：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/wxai_billing.go`
- 服务器候选和 FREE 冷却：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/server_candidates.go`
- 条件巡检：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/conditional_run.go`
- 条件候选：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/conditional_candidates.go`
- priority 调整和恢复：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/priority_adjustment.go`
- HTTP response 保存：`CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/http_response_capture.go`
- 服务器巡检 Worker：`CPA-Manager-Plus/apps/manager-server/internal/worker/wxai_inspection_worker.go`
- 条件巡检 Worker：`CPA-Manager-Plus/apps/manager-server/internal/worker/wxai_conditional_inspection_worker.go`
- 配置模型：`CPA-Manager-Plus/apps/manager-server/internal/model/wxai_inspection.go`
- repository：`CPA-Manager-Plus/apps/manager-server/internal/repository/wxaiinspection/repository.go`
