package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	managerAccountAbnormalPriority = -8
	managerRealtimeDegradationKind = "position_degradation"
)

func (controller *runtimeController) managerDatabase() string {
	controller.mutex.RLock()
	defer controller.mutex.RUnlock()
	return controller.managerDatabasePath
}

func markRealtimeGuardAuthDegraded(probe realtimeGuardProbe) (authFile, *int, error) {
	if strings.TrimSpace(probe.AuthIndex) == "" {
		return authFile{}, nil, fmt.Errorf("实时守护缺少 auth_index，不能设置账号 priority=-8")
	}
	auth, err := getAuthFile(probe.AuthIndex)
	if err != nil {
		return authFile{}, nil, err
	}
	var originalPriority *int
	if auth.PrioritySet {
		priority := auth.Priority
		originalPriority = &priority
	}
	if auth.Priority != managerAccountAbnormalPriority {
		auth.Raw["priority"] = managerAccountAbnormalPriority
		if err := saveAuthFileDirect(auth); err != nil {
			return authFile{}, nil, fmt.Errorf("写入 xAI auth priority=-8: %w", err)
		}
	}
	return auth, originalPriority, nil
}

func latestManagerScheduledInspectionRunID(managerDatabasePath string) (int64, error) {
	managerDatabasePath = strings.TrimSpace(managerDatabasePath)
	if managerDatabasePath == "" {
		return 0, fmt.Errorf("请先输入 Manager 数据库路径")
	}
	if _, err := os.Stat(managerDatabasePath); err != nil {
		return 0, fmt.Errorf("Manager 数据库不可访问 %s: %w", managerDatabasePath, err)
	}
	database, err := sql.Open("sqlite3", "file:"+managerDatabasePath+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return 0, fmt.Errorf("打开 Manager 数据库: %w", err)
	}
	defer database.Close()
	var latestScheduledRunID int64
	err = database.QueryRow(`
SELECT id
FROM wxai_inspection_runs
WHERE trigger_type = 'scheduled' AND status = 'completed'
ORDER BY finished_at_ms DESC, started_at_ms DESC, id DESC
LIMIT 1`).Scan(&latestScheduledRunID)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("Manager 数据库没有已完成的 xAi 服务器巡检 run")
	}
	if err != nil {
		return 0, fmt.Errorf("读取最近服务器巡检 run: %w", err)
	}
	return latestScheduledRunID, nil
}

func syncManagerRealtimeDegradation(managerDatabasePath string, auth authFile, originalPriority *int, probe realtimeGuardProbe, decision realtimeGuardDecision) error {
	managerDatabasePath = strings.TrimSpace(managerDatabasePath)
	if managerDatabasePath == "" {
		return fmt.Errorf("未配置 manager_database_path")
	}
	if _, err := os.Stat(managerDatabasePath); err != nil {
		return fmt.Errorf("Manager 数据库不可访问 %s: %w", managerDatabasePath, err)
	}
	database, err := sql.Open("sqlite3", "file:"+managerDatabasePath+"?_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("打开 Manager 数据库: %w", err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		return fmt.Errorf("连接 Manager 数据库: %w", err)
	}

	now := time.Now().UnixMilli()
	accountKey, displayAccount, accountID := managerAccountIdentity(auth)
	transaction, err := database.Begin()
	if err != nil {
		return fmt.Errorf("开始 Manager 同步事务: %w", err)
	}
	defer transaction.Rollback()

	storedOriginalPriority := originalPriority
	var existingAdjustedPriority int
	var existingOriginalPriority sql.NullInt64
	err = transaction.QueryRow(`
SELECT adjusted_priority, original_priority
FROM wxai_priority_adjustments
WHERE account_key = ?`, accountKey).Scan(&existingAdjustedPriority, &existingOriginalPriority)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("读取 Manager priority adjustment: %w", err)
	}
	if err == nil && existingAdjustedPriority == managerAccountAbnormalPriority && existingOriginalPriority.Valid {
		priority := int(existingOriginalPriority.Int64)
		storedOriginalPriority = &priority
	}

	if _, err := transaction.Exec(`
INSERT INTO wxai_priority_adjustments (
    account_key, file_name, display_account, auth_index, account_id,
    original_priority, adjusted_priority, recover_at_ms, created_at_ms, updated_at_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
ON CONFLICT(account_key) DO UPDATE SET
    file_name = excluded.file_name,
    display_account = excluded.display_account,
    auth_index = excluded.auth_index,
    account_id = excluded.account_id,
    original_priority = excluded.original_priority,
    adjusted_priority = excluded.adjusted_priority,
    recover_at_ms = NULL,
    updated_at_ms = excluded.updated_at_ms`,
		accountKey,
		auth.Name,
		displayAccount,
		auth.Index,
		accountID,
		storedOriginalPriority,
		managerAccountAbnormalPriority,
		now,
		now,
	); err != nil {
		return fmt.Errorf("写入 Manager priority adjustment: %w", err)
	}

	var latestScheduledRunID int64
	err = transaction.QueryRow(`
SELECT id
FROM wxai_inspection_runs
WHERE trigger_type = 'scheduled' AND status = 'completed'
ORDER BY finished_at_ms DESC, started_at_ms DESC, id DESC
LIMIT 1`).Scan(&latestScheduledRunID)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("读取最近服务器巡检 run: %w", err)
	}
	if err == nil {
		if err := writeManagerRealtimeDegradationRecord(transaction, latestScheduledRunID, auth, accountKey, displayAccount, accountID, probe, decision, now); err != nil {
			return err
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("提交 Manager 同步事务: %w", err)
	}
	return nil
}

func managerAccountIdentity(auth authFile) (string, string, string) {
	displayAccount := strings.TrimSpace(auth.Email)
	if displayAccount == "" {
		displayAccount = auth.Name
	}
	accountID := firstManagerAuthString(auth.Raw, "account_id", "accountId", "sub", "subject", "user_id", "userId")
	return strings.Join([]string{auth.Name, displayAccount, auth.Index, accountID}, "|"), displayAccount, accountID
}

func firstManagerAuthString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(stringField(values, key)); value != "" {
			return value
		}
	}
	return ""
}

func writeManagerRealtimeDegradationRecord(transaction *sql.Tx, runID int64, auth authFile, accountKey, displayAccount, accountID string, probe realtimeGuardProbe, decision realtimeGuardDecision, now int64) error {
	reason := "位置降智"
	detail := fmt.Sprintf("来源=realtime_guard；原因=%s；等级=%s；TPS=%.2f；请求=%s；代理=%s", decision.Reason, decision.QualityLevel, decision.TPS, probe.RequestID, probe.ProxyURL)
	if _, err := transaction.Exec(`
INSERT INTO wxai_inspection_results (
    run_id, account_key, file_name, display_account, auth_index, account_id, provider,
    disabled, status, state, action, action_reason, action_status, executed_action,
    action_error, status_code, used_percent, is_quota, error, plan_type,
    quota_windows_json, monthly_limit_cents, monthly_used_cents, error_kind,
    error_detail, created_at_ms
) VALUES (?, ?, ?, ?, ?, ?, 'xai', 0, 'abnormal', 'account_abnormal',
    'keep', ?, 'success', 'priority_-8', '', NULL, NULL, 0, '', '', NULL, NULL,
    NULL, ?, ?, ?)
ON CONFLICT(run_id, account_key) DO UPDATE SET
    status = excluded.status,
    state = excluded.state,
    action = excluded.action,
    action_reason = excluded.action_reason,
    action_status = excluded.action_status,
    executed_action = excluded.executed_action,
    error_kind = excluded.error_kind,
    error_detail = excluded.error_detail,
    created_at_ms = excluded.created_at_ms`,
		runID,
		accountKey,
		auth.Name,
		displayAccount,
		auth.Index,
		accountID,
		reason,
		managerRealtimeDegradationKind,
		detail,
		now,
	); err != nil {
		return fmt.Errorf("写入 Manager 巡检结果: %w", err)
	}
	logDetail, err := json.Marshal(map[string]any{
		"accountKey":      accountKey,
		"fileName":        auth.Name,
		"authIndex":       auth.Index,
		"priority":        managerAccountAbnormalPriority,
		"reason":          managerRealtimeDegradationKind,
		"realtimeGuard":   decision.Reason,
		"qualityLevel":    decision.QualityLevel,
		"tokensPerSecond": decision.TPS,
		"requestID":       probe.RequestID,
		"proxyURL":        probe.ProxyURL,
	})
	if err != nil {
		return fmt.Errorf("编码 Manager 巡检日志: %w", err)
	}
	if _, err := transaction.Exec(`
INSERT INTO wxai_inspection_logs (run_id, level, message, detail_json, created_at_ms)
VALUES (?, 'warn', '实时守护发现位置降智，xAI 账号 priority 已设为 -8', ?, ?)`, runID, string(logDetail), now); err != nil {
		return fmt.Errorf("写入 Manager 巡检日志: %w", err)
	}
	return nil
}
