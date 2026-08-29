# wXAi 调度组与实时守护修复执行计划

**目标：** 修复调度组计数可见性、Manager 状态写入丢失 `schedule_group`、实时守护写错 run，并在不重跑巡检的情况下修复线上最新 run 数据。

**范围：**
- `E:/AI/CLIProxy/CLIProxyAPI`：xAI 插件计数只读 API 与插件配置 UI。
- `E:/AI/CLIProxy/CPA-Manager-Plus`：完整状态快照写入、实时事件关联最新已完成 run。
- Oracle 01：部署两项服务，事务修复最新 run。

**明确不做：** 不调整条件巡检的 30 秒周期、10 分钟 `active_recent` 窗口、去重或退避策略；不提交 Git。

## 步骤

1. 审查两个仓库现有未提交 diff，确认 schedule_group 既有实现和部署脚本。
2. Manager：`writeAccountStatusDetail()` 显式携带 `currentAccount.ScheduleGroup`。
3. Manager：实时降智与实时健康日志改用最新已完成 overall run；保留 `LatestCompletedScheduledRun` 的 scheduled 专用语义。
4. 插件：新增调度组计数 GET 路由，返回 SQLite 当前计数；配置 UI 增加查看入口和只读展示。
5. 只执行 build/type-check 类验证；按用户全局要求不运行 tests。
6. 审查最终 diff，不 commit。
7. 使用现有构建/部署流程部署 CPA 主程序/插件及 CPA Manager Plus，保留配置和状态，核验进程与健康 API。
8. 停止 Manager 容器，备份 SQLite；参数化事务修复最新 run：
   - 从 auth JSON 回填 `schedule_group=NULL` 的状态详情。
   - 从实时降智状态/priority adjustment 校准活跃降智账号状态详情。
   - 对缺失的最新-run实时降智结果，复制已有真实事件语义写入最新 run，保留原探测历史。
9. 重启 Manager，回读数据库与 `/wxai-inspection/latest`；确认目标账号 `priority=-8`、`schedule_group=5`、降智次数 1、24 小时冷却不变。

## 风险控制

- 两个仓库均可能有用户未提交改动：禁止 reset/checkout/clean。
- 数据修复前必须先部署代码，防止再次覆盖。
- SQLite 修改前停止唯一 Manager 容器并创建时间戳备份。
- 修复脚本严格限定最新 run 和已识别账号；异常行数立即回滚。
- 所有 credential-related fields 均不读取或输出。
