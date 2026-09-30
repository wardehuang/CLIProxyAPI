# CPA 无思考假完成漏拦修复实施方案

> **For Hermes:** 实施时加载 subagent-driven-development 技能，按任务执行并审查；本文件仅为方案，不构成修改实现、运行测试、提交或部署授权。

**目标：** 堵住 FreeAPI 请求 `c36af88b` 的漏拦，同时保留真实工具行动和已成功写入后的收尾回答。

**架构：** 在既有 CPA `cpa-xai-ip-switcher` 完成拦截器中，用单一“思考/行动证据”规则替换 soft TPS 门控。借鉴 fork 的当前响应无思考判定，不搬第二套 sidecar、流缓冲或处罚系统；继续使用 CPA 现有完成后扣流、重试、Manager 降智计数与清零链路。

**技术栈：** Go、CPA pluginapi、Responses 原始 SSE、插件 SQLite 日志、CPA Manager API。

## rev.1 修订摘要

相对前文“取消 terminal 豁免 / 执行器提供文件修改证据”的两个方向，本方案收敛为：

1. 本期直接修 CPA，不依赖 Hermes 新协议，也不引入文件全盘快照。
2. 引入 fork 的无思考判据，使该判据不再依赖 soft TPS；不照搬其工具响应误拦风险和处罚参数。
3. 保留当前响应有效 function_call，以及本轮明确写入工具成功结果的收尾豁免。
4. 取消所有 terminal 退出码的修改证据资格；不写 shell 命令白名单或黑名单。
5. 真实 terminal 写入的低误判支持、跨客户端可信修改凭证、语义“任务完成”审计延期，不宣称本期可解决。

## 1. 已核实的现状

### CPA

- 仓库：`E:/AI/CLIProxy/CLIProxyAPI`，`hook-minimal`；规划前工作区干净，HEAD `7a0a2ec3`。
- `plugins/src/cpa-xai-ip-switcher/realtime_guard.go:187`：分类器已有上游失败、完整性、时序校验、hard TPS、有效工具和 mutation 证据。
- `realtime_guard.go:308-324`：当前只在 soft/hard TPS 之间且无真实思考时检查行动证据。
- `completed_mutation_evidence.go:49-65,84-109`：将 terminal 纳入 mutation，匹配成功结果就立即 true；不区分 SELECT、go env 与真正写入。
- `realtime_guard.go:53-63`：normal 直接返回并异步上报 Manager，未落 normal 判定日志。
- `sdk/api/handlers/handlers_stream_completion.go:246-315`：宿主已暂存响应、判定后回放/丢弃；当前降智尝试预算为总计 5 个账号，耗尽失败，不回放最后一条坏响应。
- `realtime_guard.go:129-135`：通常 ReloadSelectedAuth；只有 stream-progress-timeout 走 ReloadAndExcludeSelectedAuth。新规则若要明确换号，必须补入同一现有重试模式，而非仅修改 reason。

### 本次真实案例

- `c36af88b`，项目 free-api，2026-09-16 09:33:52 +08:00。
- 当前响应：107 output tokens、0 reasoning tokens、无 reasoning item/delta、无当前 function_call、completed message。
- generation_ms=5674，TPS约18.86；线上 soft=0.1、hard=500。
- 当前 user 后三次 terminal：两组 SQLite SELECT、一条 `go env GOMODCACHE GOPATH`，均 exit_code=0/error=null；没有 patch/write_file/skill_manage。
- 最早的成功 SELECT 就足以触发 mutation 豁免，并非历史窗口越界。
- normal reason 未直接落库；已有诊断是基于 SSE/usage/代码重建，不应伪称查到 normal 判定日志。

### 参考 fork

`lij768423-svg/grok2api` main `62ae4347cee557847f63835cc7b892219d848b8b`：

- `backend/internal/application/gateway/quality_retry.go:260`：无真实思考、输出达到下限即 withhold，无需 soft TPS。
- 当前响应信号参与判断，历史 terminal 成功不参与。
- fork 有短答放行、模型/provider/streaming 门控，绝非通用文件修改真实性审计。
- 不复制 fork 的 6 次尝试、12h/15m 处罚、30s hold、256-byte 密文阈值；这些不属于本次 CPA 修复。

## 2. 推荐的唯一生效规则

### 2.1 判定顺序

维持单一分类器，按顺序处理：

1. 现有协议、上游错误、完整性、时间顺序和超时规则不变。
2. hard TPS 规则不变，仍优先于其他豁免；TTFB 整块 `/* */` 禁用状态不变。
3. 使用现有 `evaluateRealtimeGuardThinking`。当前短输出和 burst_dump_disabled 策略、思考摘要/密文阈值不改。日志区分真实证据与策略豁免，不把 below_minimum_output_tokens 或 burst_dump_disabled 描述成真实思考。
4. `IsRealThinking=true`：沿用现有放行，不扫描 input。
5. 当前响应 `CompletedToolCallEvidence=true`：沿用 `completed_tool_call_evidence` 放行，不扫描 input。正文前言与完整工具调用并存仍放行；这只证明模型生成了工具调用，不证明执行成功。
6. 当前响应有 completed message、无 refusal，且同一任务窗口存在明确写入工具的配对成功结果：`completed_mutation_evidence` 放行。
7. 其余无思考响应：分类 `suspected_degradation`、quality `soft`、reason **`missing_thinking_without_action`**，丢弃当前响应并重试。

第 7 步不再要求 TPS 大于 QualitySoftTPS。最低输出门槛继续通过现有 thinking evaluator 生效，禁止另加相互冲突的第二套长度阈值。

不保留旧 `soft_tps_missing_real_thinking` 分支与新分支并跑。旧 reason 仅作为历史日志值保留；历史日志不迁移。QualitySoftTPS 若仍供节点质量探测使用则保留其配置和消费，不删除其他功能所需字段；实施时追踪全部消费者，仅修正实时守护相关说明。

### 2.2 什么算写入证据

沿用已有匹配窗口与 call_id 配对：

- 默认最后 user 之后。
- 仅最后 user 的直接前一项为 function_call_output 时，使用现有回退到上一 user 的规则。
- `patch`、`skill_manage` 按已有明确成功字段；`write_file` 按 verified=true。
- **terminal 一律不作为 mutation 证据。** 包括 SELECT、go env、git status，也包括 shell 中真实写入的命令。
- read_file/search_files/todo/skill_view 不作为 mutation。
- 失败、无结果、配对缺失、其他任务历史写入不认可。
- 不增加“JSON 能解析 / result 非空 / output 没报错就算成功”等宽松兜底。

实施时以真实客户端工具结果契约校验上述字段；若已用工具的实际结果结构与当前实现不符，先报告并确认适配范围，不能猜字段。CPA 接收客户端提供的工具历史，不能把它视为对恶意客户端的可信审计。

### 2.3 terminal 真正写入的处理

本期明确接受：真实 terminal 写入后的无思考收尾也可能被扣住。它的成功工具结果仍留在下一次重试的上下文中，但不构成“必须放行”的证明；不能保证重试一定避免重复操作。

正常有思考的收尾继续放行；使用明确写入工具的成功结果也可放行。不要为了兼容 shell 写入重新加入 exit_code=0 豁免。

后续如需降低此类误拦，另案设计客户端执行器提供“本次工具、目标路径、执行前后内容变化”的可信凭证。必须明确协议来源、真实性、跨客户端适配及任务边界，不能让模型自报文件已变，也不能使用工作区原有 dirty diff 充当本次修改证据。

## 3. 重试与处罚

- 新 reason 使用已有 `StreamCompletionRetryModeReloadAndExcludeSelectedAuth`，确保本次重试排除已命中账号；其他 reason 的重试模式不改。
- 总计 5 个降智账号预算不变，不新增独立重试计数，不照搬 fork 的 6。
- 用现有宿主扣流与虚拟心跳机制，不引入部分正文提前回放、不增补第二种心跳。
- 预算耗尽或无可用账号时沿用 CPA 的现有失败终态；不为了匹配 fork 改成特定 HTTP 503。流式已发头场景须看错误事件，不能要求重写已发出的 HTTP 状态。
- 通过已有 `syncManagerRealtimeDegradation` 计数与冷却，不改 priority 阶梯、冷却时长、恢复/清零条件。
- 正常结果仍必须调用 `notifyManagerRealtimeHealthyAsync`，不能恢复本地 degraded 标记门控。
- 这意味着扩大无思考拦截范围也会扩大账号/节点处罚范围。上线前必须接受这一权衡，不能把“改 classifier”当成没有生产状态副作用。

## 4. 最小日志补齐

新规则需能直接回答“为何放行”，不再只靠反推：

- 新拒绝 reason 继续走现有 degradation 日志。
- 对无思考、通过本轮写入证据放行的收尾，新增一条受现有 retention 管理的正常豁免日志；不为每个 SSE chunk 写日志，不遍历/记录全量历史正文。
- 用已有 `appendProbeLog` 接口，不新建表/日志服务。
- 字段：request_id、trace_id、auth_index、reason、IsRealThinking/RealThinkingReason、当前有效工具数、completed message 数、扫描起点、命中工具名/call_id、mutation 是否命中、TPS/output tokens。
- 给 decision 添加承载命中来源所需的最小字段；不使用多份输入数据源、不重复扫描原请求。真实思考/当前有效工具命中时仍不扫描 input。
- 不记录完整 terminal 命令、工具输出、请求体、代理凭证、headers。日志文本不得新增硬编码中文。
- normal 日志只表示质量规则放行，不宣称业务修改已经完整完成。

## 5. 明确不做

- 不根据“已改好”“硬刷新看”等中英文关键词判降智。
- 不让另一个 LLM 审稿；无模型调用成本或语义分类依赖。
- 不做 shell 命令白名单/黑名单。
- 不部署 fork sidecar，不移植整个 gateway。
- 不改 FreeAPI 业务代码，也不改 grok2api 当前生产服务。
- 不扩展到所有 provider、非流式/未走该 hook 的链路；不承诺显式禁用思考请求可获得新增兼容豁免。若需支持无思考模型，应单独核实路由能力与最终请求格式后定范围，而非凭模型字符串猜测。
- 不声称能发现“有真实思考但撒谎”或“改了一个无关文件却宣称全部完成”。本轮某个成功写入仅为收尾豁免依据，不等于验收全部任务。

## 6. 实施任务与文件

### 任务 1：收紧 mutation 证据

- 修改 `plugins/src/cpa-xai-ip-switcher/completed_mutation_evidence.go`。
- 将 terminal 从 isMutationToolName 删除，删除仅服务 terminal 的退出码解析字段/helper和失去用途的 imports。
- 保留现有 user 窗口和 call_id 配对逻辑。
- 为日志返回最小证据来源；优先用一个结果结构替代重复解析，不添加 bool 与第二套扫描器并存。
- 新建 `plugins/src/cpa-xai-ip-switcher/completed_mutation_evidence_test.go`（计划新增，当前尚不存在）。

### 任务 2：替换 soft TPS 条件

- 修改 `plugins/src/cpa-xai-ip-switcher/realtime_guard.go:308-328`，按第 2 节唯一顺序判定。
- hard TPS、TTFB 注释、原思考 evaluator、超时/异常处理不改。
- 为新 reason 接入 ReloadAndExcludeSelectedAuth。
- 修改 `plugins/src/cpa-xai-ip-switcher/realtime_guard_types.go`，仅增加审计必需字段。
- 扩展现有 `plugins/src/cpa-xai-ip-switcher/realtime_guard_test.go`。

### 任务 3：补齐豁免审计

- 在 realtime_guard.go 的 normal 分支中，针对 completed_mutation_evidence 写最小正常判定日志。
- 保留 normal Manager 上报行为。
- 复用 log_store.go 的接口和保留策略；该文件原则上不需改。
- 全仓搜索旧 soft reason、QualitySoftTPS 的消费者；若 UI 帮助明确把 soft TPS 描述为实时无思考前提，修正相应文案，不能保留误导性说明。只改必要说明，不新增表单或配置开关。

### 任务 4：验收后再申请上线

- 先格式化、检查 diff 和构建，在具备 CGO 且与主程序兼容的 Linux 插件构建环境检查插件产物。
- 是否运行下面的测试和是否部署分别等待用户明确授权。
- 部署仅替换目标插件；备份原产物，核对主程序/插件兼容性与新产物身份，遵循现有部署技能。
- 不自动 commit/push。回滚只恢复旧插件并重启必要进程，不清 DB、不批量重置账号。回滚二进制不会撤销已发生的处罚，账号状态恢复必须另行授权。

## 7. 验收矩阵（写入测试计划，不在规划阶段运行）

1. `c36af88b` 脱敏最小 fixture：无思考+107输出、三次只读 terminal 成功，预期 retry / missing_thinking_without_action，mutation=false。
2. 同一 fixture 降低 TPS 到 soft 阈值以下：仍 retry；不得出现降速即可绕过。
3. 当前完整 function_call，无 refusal，带或不带正文：在不命中 hard TPS 等更高优先级规则时放行，不扫描历史。
4. 当前 completed message，无思考，本轮 write_file verified=true：放行并记录命中 call_id。
5. 本轮 patch/skill_manage 明确成功：按各自结果契约放行；失败/缺字段不放行。
6. 成功写入来自上一个独立 user 任务：不作为本轮证据。
7. user 紧接 function_call_output 的既有追问边界：保持现有回退语义。
8. terminal 实际写文件但仅提供 exit_code=0：仍不构成 mutation 证据，覆盖已明确接受的误拦边界。
9. below_minimum_output_tokens、burst_dump_disabled：原行为不变，日志不得伪称真实思考。
10. 真实 summary/encrypted 思考且满足原阈值：原行为不变；reasoning token 数本身不能替代证据。
11. hard TPS 与有效工具共存：hard TPS 仍优先；TTFB 禁用仍不产生 ttfb_downgrade。
12. 新规则重试：旧账号被排除，坏正文不进入客户端，预算耗尽以失败终态结束，不回放最后坏响应。
13. Manager 异常/正常链路：降智上报与健康上报原语义不变，不重建清零本地门控。
14. normal mutation 日志与拒绝日志：可关联 request_id/trace_id，无凭证、全量请求或命令泄漏。

经用户授权后的定向命令：

- 在插件模块、具备 CGO 的兼容环境：`go test . -run 'TestClassifyRealtimeGuard|TestCompletedMutation' -count=1`。
- 宿主模块：`go test ./sdk/api/handlers -run 'Test.*(Stream|Completion|RealtimeGuard)' -count=1`；先确认所需集成测试存在，再选择具体目标，缺失时新增而非宣称已覆盖。
- 插件编译：插件目录执行 `go build -buildmode=plugin -o cpa-xai-ip-switcher.review.so .`，不得直接覆盖生产文件。
- 主仓构建检查：Windows `go build -o NUL ./cmd/server` 只能验证宿主，不能替代插件编译。
- diff 检查：`git diff --check`；gofmt 仅格式化本次修改的 Go 文件。
- 实际请求 fixture 必须脱敏、最小化；原始含凭证的请求体不得复制到本机或提交仓库。

以上是预期结果，不是已运行的测试结论。

## 8. 需要用户确认的政策变更

方案可直接按上述定义实施，但执行前需明确同意：

1. 取消此前 terminal 成功整体放行；接受真实 shell 写入后的无思考收尾也可能被扣住。
2. 无思考/无行动判据取消 soft TPS 前提，因此低 TPS 的同类响应也会触发现有处罚链。
3. 保留既有短答/burst/有效工具/明确写入豁免及 5 账号失败关闭预算，不移植 fork 的其他阈值或处罚。

本方案解决的是质量启发式漏拦，不是文件修改或任务完成的强真实性证明。
