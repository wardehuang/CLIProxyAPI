# xAI Schedule Group Implementation Plan

> **For Hermes:** 逐任务实现；每层先写测试，再写实现。不得自动 commit、push、部署。测试只在用户明确授权后运行。

**Goal:** 为 CPA xAI 请求增加持久化调用计数、内存 busy 锁、同 turn 固定组重试，以及 Manager 巡检后的全量 JSON 分组。

**Architecture:** CPA 核心只扩展通用 scheduler 契约：scheduler 链、跨 priority 候选、结构化拒绝、`schedule_group` 属性暴露。xAI 的组选择、busy 生命周期、计数持久化全部归 `cpa-xai-ip-switcher`。Manager 是 auth JSON 的 `schedule_group` 唯一写入方。

**Tech Stack:** Go、CPA plugin ABI、SQLite、CPA Management API、React/TypeScript。

---

## 1. 已确认语义

- 插件设置名：`schedule_group_count`；默认 `10`；必须为正整数。
- auth JSON 字段名：`schedule_group`。
- Manager 每次巡检：查询插件组数；查询失败则巡检失败，且不修改 JSON。
- 分组范围：全部 xAI JSON，包括 disabled 或当前不可调度文件。
- 分组顺序：仅按 CPA auth ID 升序；ID 相同再按文件名升序保证稳定。
- 分配公式：排序后第 `i` 个账号分到 `((i - 1) % N) + 1`。
- Manager 每次巡检都对全部文件发起字段写入；不按旧值跳过。
- 任一 JSON 写入失败：巡检失败，不调用计数重置；下次巡检全量重试。
- 全部 JSON 写入成功后：调用插件接口清零全部组调用次数；不清 busy。
- 首次调度：只看空闲且至少有一个可用候选的组；选择调用次数最少组；相同则组号最小。
- 账号选择：先选组，再在组内按 `priority` 降序、auth ID 升序选择第一个候选。
- 每次账号选择成功后，该组调用次数 `+1`；首次和同 turn 每次换号都计数。
- 同一 turn 的所有内部重试，包括实时降智、429、5xx 和其他可重试错误，固定原组；不释放 busy。
- 只有 CPA 发出 terminal `request.complete` 后释放 busy。
- 固定组内账号耗尽：HTTP `503`，错误码 `xai_schedule_group_exhausted`。
- 无空闲且可用组：HTTP `503`，错误码 `xai_schedule_groups_busy`。
- busy 仅保存在内存；插件重启后重置。
- 调用次数保存在插件 SQLite；插件重启后保留。
- 组数变更时若存在任何 busy 请求，配置保存返回 `409 Conflict`；不改变旧配置。
- 重置接口只清零调用次数，不修改 busy。
- Manager 查询接口只返回配置的组数 N。

## 2. 评估结论

### 2.1 是否解决同账号并行

能。成立条件：

1. 每个 auth JSON 只有一个合法 `schedule_group`。
2. 同一个组同时最多绑定一个 turn。
3. 同 turn 所有重试保持原组。
4. terminal completion 必须释放原组。

因为账号唯一归组，不同 busy 组不共享账号；同组被串行化，所以同一账号不会被两个并行 turn 同时选中。

明确代价：

- 最大 xAI 并发固定为 `schedule_group_count`，默认 `10`。
- 第 11 个并行请求不排队，直接返回 `503 xai_schedule_groups_busy`。
- 一个组内任意长请求会阻塞整个组，不只是阻塞当前账号。
- 插件重启会清空 busy；重启瞬间仍在执行的旧请求不再受旧 busy 保护。这是已接受的“重启重置状态”语义。
- 首次 Manager 全量写几千文件会产生大量 Management API 调用和 watcher 更新；后续即使 Manager 仍逐文件调用，CPA 对相同值可保持 no-op，实际文件更新量预计显著下降。

### 2.2 能否只改插件和 Manager

不能可靠完成。当前 CPA 有四个核心边界：

1. `internal/pluginhost/scheduler.go` 只调用最高优先级的一个 scheduler；antigravity 与 xAI scheduler 会互斥。
2. scheduler candidate 当前不暴露 auth JSON 的 `schedule_group`。
3. scheduler 当前只收到全局最高 `priority` 桶，无法实现“先选组，再在组内按 priority 排序”。
4. scheduler 插件错误没有结构化拒绝契约，无法稳定返回指定 HTTP 503 和错误码。

因此采用已确认方案：修改 CPA 通用 scheduler 扩展点，不把 xAI 业务调度写入 CPA 核心。

## 3. 核心数据模型

### 3.1 CPA scheduler 请求

在 `sdk/pluginapi/types.go` 扩展：

- 保留 `Candidates`：现有全局最高 priority 候选，兼容 antigravity 插件。
- 新增 `AllCandidates`：所有当前可选择、未被 tried 排除、跨 priority 的候选。
- `schedule_group` 通过 `SchedulerAuthCandidate.Attributes` 暴露。

### 3.2 CPA scheduler 结构化拒绝

新增统一结构：

- `Code`
- `Message`
- `HTTPStatus`
- `Retryable`

`SchedulerPickResponse` 的有效决策三选一：

1. `AuthID`
2. `DelegateBuiltin`
3. `Rejection`

禁止混合返回。

### 3.3 插件运行时状态

内存：

- `groups[groupID].Busy`
- `groups[groupID].TurnID`
- `turnGroups[turnID]groupID`

SQLite：

```text
schedule_group_counters
- group_id INTEGER PRIMARY KEY
- call_count INTEGER NOT NULL
- updated_at INTEGER NOT NULL
```

唯一 turn 标识由插件 `RequestMetadataEnricher` 在首次执行前写入 metadata，例如 `cpa_xai_schedule_turn_id`。scheduler 和 `request.complete` 使用同一 metadata 字段关联。

## 4. 实施任务

### Task 1: CPA scheduler 链

**Objective:** 多个 scheduler 按插件优先级依次获得处理机会，不再只取第一个。

**Files:**

- Modify: `internal/pluginhost/scheduler.go`
- Modify: `internal/pluginhost/scheduler_test.go`

**行为：**

1. 遍历 `activeRecords()` 中所有未 fuse 的 scheduler。
2. `Handled:false`：继续下一个 scheduler。
3. 合法 `AuthID`、`DelegateBuiltin` 或 `Rejection`：立即停止链。
4. scheduler 返回 error：立即停止并向上返回，不静默换下一个插件。
5. panic/fuse 或非法 response：记录并继续下一个 scheduler；全部未处理才回落 CPA built-in。
6. 每次调用正确设置当前插件的 `req.Plugin`。

**测试：**

- 高优先级未处理，低优先级处理。
- 高优先级处理后低优先级不调用。
- 高优先级显式 delegate built-in 后停止链。
- scheduler error 不调用下一个。
- panic/非法 response 后调用下一个。
- 所有插件未处理时 `handled=false`。

### Task 2: 暴露 `schedule_group` 与跨 priority 候选

**Objective:** xAI scheduler 能看到全部可选择账号及其组号，同时不改变现有 antigravity 候选语义。

**Files:**

- Modify: `internal/watcher/synthesizer/file.go`
- Modify: `internal/watcher/synthesizer/file_test.go` 或现有对应 auth file synthesizer 测试
- Modify: `sdk/pluginapi/types.go`
- Modify: `sdk/cliproxy/auth/conductor_selection.go`
- Modify: `sdk/cliproxy/auth/scheduler_test.go` 或新增 `sdk/cliproxy/auth/conductor_plugin_scheduler_test.go`
- Modify: `internal/pluginhost/rpc_schema_test.go`

**行为：**

1. 使用现有 `copyIntegerMetadataAttribute` 将 JSON `schedule_group` 写入 auth attributes。
2. 缺失、非整数、非正数不写入 attribute；插件会跳过该账号。
3. 保持 `Candidates` 为现有最高 priority 集合。
4. 新增 `AllCandidates`，来源为跨 priority、已通过 disabled/cooldown/model/tried/pinned 过滤的候选。
5. dynamic plugin ABI 序列化/反序列化新字段。
6. 不暴露任何 credential 字段；继续经过 `schedulerSafeAttributes`。

**测试：**

- 数字和数字字符串 `schedule_group` 被暴露。
- 缺失、非法、零、负数不暴露。
- `Candidates` 仍只有最高 priority。
- `AllCandidates` 包含多个 priority，并排除 disabled/cooldown/tried。
- `schedule_group` 不被敏感字段过滤器删除。

### Task 3: scheduler 结构化拒绝

**Objective:** 插件能精确返回 503 和机器错误码。

**Files:**

- Modify: `sdk/pluginapi/types.go`
- Modify: `internal/pluginhost/scheduler.go`
- Modify: `internal/pluginhost/scheduler_test.go`
- Modify: `sdk/cliproxy/auth/conductor_selection.go`
- Test: `sdk/cliproxy/auth/conductor_plugin_scheduler_test.go`

**行为：**

1. host 验证 response 只能有一个决策。
2. conductor 将 `Rejection` 转为 `*auth.Error`。
3. `HTTPStatus=503`、`Code=xai_schedule_groups_busy` 等必须原样进入现有 handler 错误映射。
4. Rejection 不进入 built-in scheduler，不尝试下一 scheduler。

**测试：**

- 503/status/code/message/retryable 完整保留。
- rejection 与 auth ID/delegate 混合时判非法。
- rejection 不触发后续 scheduler 或 built-in fallback。

### Task 4: 插件配置与 SQLite 计数

**Objective:** 保存组数和每组累计调用次数。

**Files:**

- Modify: `plugins/src/cpa-xai-ip-switcher/main.go`
- Modify: `plugins/src/cpa-xai-ip-switcher/runtime.go`
- Modify: `plugins/src/cpa-xai-ip-switcher/store.go`
- Create: `plugins/src/cpa-xai-ip-switcher/schedule_group_store.go`
- Create: `plugins/src/cpa-xai-ip-switcher/schedule_group_store_test.go`

**行为：**

1. `pluginSettings` 增加 `ScheduleGroupCount`，默认 `10`。
2. SQLite settings 读写、public settings、payload parsing 全链路增加 `scheduleGroupCount`。
3. 建表 `schedule_group_counters`。
4. 读取计数时缺失组按 `0` 初始化；禁止使用第二数据源兜底。
5. 计数增加必须在单事务中完成。
6. 重置计数使用单事务将全部已配置组设为 `0`。
7. 组数减少时保留超范围历史行或删除超范围行只选一种：计划采用删除超范围行，保证 SQLite 只含当前 `1..N`。
8. 组数变更且存在 busy 时返回 `409`，整个 settings 更新失败。

**测试：**

- 新库默认 10 组、计数 0。
- 增加、持久化、重开数据库后保留。
- reset 只清计数。
- 组数变更迁移 1..N。
- busy 时变更组数返回冲突，原值不变。

### Task 5: 插件 turn 关联与调度状态机

**Objective:** 实现组级互斥、组内账号选择、重试固定组和 terminal 释放。

**Files:**

- Create: `plugins/src/cpa-xai-ip-switcher/schedule_group_scheduler.go`
- Create: `plugins/src/cpa-xai-ip-switcher/schedule_group_scheduler_test.go`
- Modify: `plugins/src/cpa-xai-ip-switcher/main.go`
- Modify: `plugins/src/cpa-xai-ip-switcher/runtime.go`

**Capabilities:**

- `Scheduler`
- `RequestMetadataEnricher`
- `RequestLifecyclePlugin`
- 保留现有 `RequestFinalizer` 和 `StreamCompletionInterceptor`

**状态机：**

1. metadata enricher 写入稳定 turn ID；同一请求后续执行沿用原值。
2. 非 xAI provider：`Handled:false`，交给 scheduler 链下一个插件。
3. xAI 首次 pick：
   - 使用 `AllCandidates`。
   - 跳过无 `schedule_group`、非法组号、组号大于 N 的账号。
   - 建立每组候选；候选按 priority 降序、auth ID 升序。
   - 只考虑非 busy 且候选非空组。
   - 按 SQLite call count 升序、group ID 升序选组。
   - 在同一临界区内持久化计数 `+1`、标 busy、绑定 turn，再返回账号。
4. 同 turn 重试：
   - 从 `turnGroups` 取原组。
   - 只从该组剩余 `AllCandidates` 选第一个。
   - 每次成功选择前持久化计数 `+1`。
   - 不释放 busy。
5. 原组无候选：返回 `503 xai_schedule_group_exhausted`。
6. 首次无空闲候选组：返回 `503 xai_schedule_groups_busy`。
7. `request.complete` 收到任何 terminal status：按 turn ID 释放对应组并删除映射。
8. 重复 completion 必须幂等；找不到 turn 不修改其他组。
9. 插件重新 configure/restart 时清空 busy 和 turn map；SQLite 计数保留。

**并发测试：**

- 相同计数选择最小组号。
- 计数不同选择最少组。
- 10 个组最多允许 10 个并行 turn；第 11 个返回指定 503。
- 两个并发首次 pick 不会拿到同组。
- 同 turn 多次 pick 固定组并逐次加计数。
- 同 turn 当前 auth 被 tried 排除后选择组内下一账号。
- terminal success/failed/rejected/canceled 均释放。
- reset counters 不释放 busy。
- 插件重建只清内存状态。

### Task 6: 插件 Management API 与配置页

**Objective:** Manager 可查询组数、重置计数；插件页面可编辑组数和手动重置。

**Files:**

- Modify: `plugins/src/cpa-xai-ip-switcher/main.go`
- Modify: `plugins/src/cpa-xai-ip-switcher/page.html`

**Management API 子路径：**

- `GET /schedule-groups/config` → `{ scheduleGroupCount: N }`
- `POST /schedule-groups/reset-counters` → 清零调用次数，busy 不变
- 现有 `GET/PUT /settings` 增加 `scheduleGroupCount`

**配置页：**

1. 设置弹窗新增“调度组数量”，默认回填已保存值。
2. 新增“重置调度组调用次数”按钮。
3. 重置前显示明确确认文案：不会释放正在执行的 busy 组。
4. 保存组数遇到 busy 返回 409 时显示服务端错误，不重建运行态。
5. 不增加兼容性空值 fallback；旧库通过数据库默认迁移得到 10。

### Task 7: Manager 插件客户端与全量分组阶段

**Objective:** 每次巡检按最终 xAI 文件集合重写组号，成功后重置计数。

**Files:**

- Create: `E:/AI/CLIProxy/CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/schedule_group.go`
- Create: `E:/AI/CLIProxy/CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/schedule_group_test.go`
- Modify: `E:/AI/CLIProxy/CPA-Manager-Plus/apps/manager-server/internal/service/wxaiinspection/service.go`
- Modify: `E:/AI/CLIProxy/CPA-Manager-Plus/apps/manager-server/internal/service/cpaauthfiles/client.go`
- 必要时 Modify: Manager service 依赖注入/app wiring 文件

**流程：**

1. 巡检开始时通过现有 CPA Management API 鉴权封装调用插件 `GET /schedule-groups/config`。
2. 查询失败或 N 非正数：立即将巡检标记失败，不修改 JSON。
3. 正常执行现有巡检和动作。
4. 动作完成后重新读取 CPA auth files，得到最终全部 xAI JSON；不使用巡检前旧快照。
5. 按 canonical auth ID 升序、文件名升序排序。
6. 对每个文件依次调用 `PATCH /v0/management/auth-files/fields`，写入整数 `schedule_group`。
7. 即使旧值相同也发起 PATCH；由 CPA 单文件字段处理决定是否实际落盘。
8. 任一 PATCH 失败：停止后续流程，巡检失败，不 reset counters；已完成文件不回滚，下次巡检全量覆盖。
9. 全部 PATCH 成功后调用 `POST /schedule-groups/reset-counters`。
10. reset 失败：巡检失败；JSON 分组保留，计数未确认清零。
11. 整个批次接入现有 auth mutation coordinator，避免与禁用、删除、优先级恢复任务交叉写同一文件。

**测试：**

- N=10 时 1..12 分配为 1..10,1,2。
- 输入顺序随机时输出仍按 auth ID/文件名稳定。
- disabled xAI 仍参与分组。
- 非 xAI 文件不写。
- 查询失败零 PATCH。
- 第 k 个 PATCH 失败后不 reset，并返回巡检失败。
- 全部成功后只调用一次 reset。
- reset 失败时巡检失败。

### Task 8: Manager 状态持久化与 API 字段

**Objective:** 最新账号状态返回每个账号本次分配的调度组。

**Files:**

- Modify: `E:/AI/CLIProxy/CPA-Manager-Plus/apps/manager-server/internal/model/wxai_inspection.go`
- Modify: `E:/AI/CLIProxy/CPA-Manager-Plus/apps/manager-server/internal/repository/sqlite/wxai_migrate.go`
- Modify: `E:/AI/CLIProxy/CPA-Manager-Plus/apps/manager-server/internal/repository/wxaiinspection/repository.go`
- Modify: `E:/AI/CLIProxy/CPA-Manager-Plus/apps/manager-server/internal/store/wxai_inspection.go`

**数据：**

- `WxaiAccountStatusDetail.ScheduleGroup *int`
- `WxaiAccountStatusItem.ScheduleGroup *int`
- SQLite `wxai_account_status_details.schedule_group INTEGER`

**行为：**

1. Manager 分组成功后，把每个 account key 对应 group 写入当前 run 的 status detail。
2. status query join 返回 `scheduleGroup`。
3. 旧 run 没有值时返回 `null/omitted`，不伪造 10 或重新计算。
4. 分组阶段失败时，不把未完整提交的映射标为成功状态。

### Task 9: Manager WebUI 调度组列

**Objective:** 在账号状态表中展示调度组，并缩小额度窗口列。

**Files:**

- Modify: `E:/AI/CLIProxy/CPA-Manager-Plus/apps/web/src/services/api/wxaiInspectionService.ts`
- Modify: `E:/AI/CLIProxy/CPA-Manager-Plus/apps/web/src/features/monitoring/WxaiInspectionPage.tsx`
- Modify: `E:/AI/CLIProxy/CPA-Manager-Plus/apps/web/src/features/monitoring/CodexAccountStatusPage.module.scss`

**UI：**

1. API type 增加 `scheduleGroup?: number`。
2. 在“优先级”后增加“调度组”。
3. 空值显示现有空值样式，不猜测组号。
4. 缩小 `.accountStatusColQuota` 宽度，为新列留空间。
5. 表头与每行 grid column 数量严格一致。
6. 小屏响应式规则同步增加该列，不通过横向叠加破坏现有布局。

### Task 10: 端到端验证与上线顺序

**Objective:** 避免启用 scheduler 后因旧 JSON 无组号导致全部 xAI 账号被跳过。

**测试/验证命令：仅在用户明确授权后执行。**

**本地验证范围：**

- CPA core scheduler/pluginhost 定向 Go tests。
- `cpa-xai-ip-switcher` package tests 和插件构建。
- Manager server 定向 Go tests。
- Manager WebUI type-check/build。
- 不自动 commit。

**上线顺序：**

1. 部署包含新字段写入能力和 UI 的 Manager，但先不启用 xAI scheduler capability。
2. 部署 CPA 核心 scheduler 链、跨 priority candidates、结构化拒绝和属性暴露。
3. 部署插件数据库/API/UI能力，保持 scheduler 开关关闭或插件旧版本未注册 Scheduler。
4. 运行一次完整 Manager 巡检；核验全部 xAI JSON 都有合法 `schedule_group`，总数与 auth inventory 一致。
5. 核验插件 `schedule_group_count=N`、SQLite counters 已清零。
6. 启用注册 Scheduler 的插件版本并重启 CPA/插件。
7. 发起并发探针：N 个请求成功占用不同组，第 N+1 个返回 `503 xai_schedule_groups_busy`。
8. 制造组内换号：确认 auth 变化但 group 不变，counter 再加 1，terminal 后 busy 释放。
9. 核验 antigravity 请求仍进入 antigravity scheduler，xAI 请求进入 xAI scheduler，普通 provider 回落 built-in。
10. 核验 xAI Responses 全流缓存、虚拟 `response.created`、10 秒 `response.in_progress` 心跳未被覆盖。

## 5. 风险与处理

- **吞吐硬上限：** N 个组就是 N 个并发；无队列。已确认返回 503。
- **初次写入风暴：** 几千次 PATCH 和 watcher 事件。按 ID 顺序串行写，优先稳定性，不引入未定义 worker count。
- **部分写入：** 无跨文件事务。失败不回滚、不 reset；下次巡检全量修复。
- **terminal 丢失：** 正常 host 契约应发送一次 terminal completion；本方案不添加猜测性 TTL。若插件进程未重启且 completion 永久丢失，该组会保持 busy，需要运维重启插件。此范围需在实现验证中做故障注入确认。
- **插件重启窗口：** busy 清空是明确要求；旧 in-flight 请求可能在重试时重新选组。
- **scheduler 链兼容：** antigravity 插件当前对非目标请求返回空 response/`Handled:false`，适合链式执行，不需复制或修改其业务算法。
- **Manager 工作区：** 当前已存在用户改动 `LOCAL_VERSION`；实施时不得覆盖或提交该文件。

## 6. 完成标准

- 所有 xAI JSON 有 `1..N` 的 `schedule_group`。
- 缺失或非法组号账号不进入 xAI scheduler。
- antigravity 与 xAI scheduler 可同时生效。
- 组选择、账号选择、重试、counter、busy、terminal release 均有并发测试覆盖。
- 指定 503 状态和错误码从插件完整到达客户端。
- Manager 巡检失败语义、全量写入、reset 时序与状态列均有测试覆盖。
- UI 列顺序正确，额度窗口变窄。
- 未自动 commit、push、部署。
- 当前仅完成计划；未写代码，未运行测试。
