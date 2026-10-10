package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type accountInspectionQuotaWindow struct {
	ID          string   `json:"id"`
	LabelKey    string   `json:"labelKey"`
	UsedPercent *float64 `json:"usedPercent,omitempty"`
	ResetAtMS   int64    `json:"resetAtMs,omitempty"`
}

type accountInspectionRun struct {
	ID                  int64  `json:"id"`
	TriggerType         string `json:"triggerType"`
	Status              string `json:"status"`
	StartedAtMS         int64  `json:"startedAtMs"`
	FinishedAtMS        int64  `json:"finishedAtMs,omitempty"`
	TotalFiles          int    `json:"totalFiles"`
	ProcessedCount      int    `json:"processedCount"`
	ProbeSetCount       int    `json:"probeSetCount"`
	HealthyCount        int    `json:"healthyCount"`
	QuotaExhaustedCount int    `json:"quotaExhaustedCount"`
	AbnormalCount       int    `json:"abnormalCount"`
	SkippedCount        int    `json:"skippedCount"`
	Error               string `json:"error,omitempty"`
}

type accountInspectionResult struct {
	ID                int64                          `json:"id"`
	RunID             int64                          `json:"runId"`
	AccountKey        string                         `json:"accountKey"`
	FileName          string                         `json:"fileName"`
	DisplayAccount    string                         `json:"displayAccount"`
	AuthIndex         string                         `json:"authIndex,omitempty"`
	AccountID         string                         `json:"accountId,omitempty"`
	Provider          string                         `json:"provider"`
	Disabled          bool                           `json:"disabled"`
	Probed            bool                           `json:"probed"`
	Status            string                         `json:"status,omitempty"`
	State             string                         `json:"state,omitempty"`
	Action            string                         `json:"action"`
	ActionReason      string                         `json:"actionReason"`
	ActionStatus      string                         `json:"actionStatus,omitempty"`
	ExecutedAction    string                         `json:"executedAction,omitempty"`
	ActionError       string                         `json:"actionError,omitempty"`
	StatusCode        *int                           `json:"statusCode,omitempty"`
	UsedPercent       *float64                       `json:"usedPercent,omitempty"`
	IsQuota           bool                           `json:"isQuota"`
	Error             string                         `json:"error,omitempty"`
	PlanType          string                         `json:"planType,omitempty"`
	QuotaWindows      []accountInspectionQuotaWindow `json:"quotaWindows,omitempty"`
	MonthlyLimitCents *float64                       `json:"monthlyLimitCents,omitempty"`
	MonthlyUsedCents  *float64                       `json:"monthlyUsedCents,omitempty"`
	ErrorKind         string                         `json:"errorKind,omitempty"`
	ErrorDetail       string                         `json:"errorDetail,omitempty"`
	ScheduleGroup     *int                           `json:"scheduleGroup,omitempty"`
	Priority          *int                           `json:"priority,omitempty"`
	OriginalPriority  *int                           `json:"originalPriority,omitempty"`
	RecoverAtMS       int64                          `json:"recoverAtMs,omitempty"`
	AccountType       string                         `json:"accountType,omitempty"`
	CreatedAtMS       int64                          `json:"createdAtMs"`
}

type accountInspectionResponse struct {
	Run   *accountInspectionRun     `json:"run"`
	Items []accountInspectionResult `json:"items"`
}

type accountInspectionRunList struct {
	Items []accountInspectionRun `json:"items"`
	Total int                    `json:"total"`
}

type accountInspectionPriorityAdjustment struct {
	AuthIndex        string
	FileName         string
	OriginalPriority *int
	AdjustedPriority int
	RecoverAtMS      int64
}

type sqlRowScanner interface {
	Scan(dest ...any) error
}

func (store *guardianStore) startAccountInspectionRun(total int, triggerType string) (int64, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	now := nowMillis()
	result, err := store.database.Exec(`INSERT INTO account_inspection_runs(trigger_type, started_at, status, total) VALUES (?, ?, 'running', ?)`, triggerType, now, total)
	if err != nil {
		return 0, fmt.Errorf("start account inspection run: %w", err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read account inspection run ID: %w", err)
	}
	return runID, nil
}

func (store *guardianStore) updateAccountInspectionRunProgress(runID int64, processed, probed, healthy, quotaExhausted, abnormal, skipped int) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	result, err := store.database.Exec(`UPDATE account_inspection_runs SET
		processed = ?, probed = ?, healthy = ?, quota_exhausted = ?, abnormal = ?, skipped = ?
		WHERE id = ? AND status = 'running'`, processed, probed, healthy, quotaExhausted, abnormal, skipped, runID)
	if err != nil {
		return fmt.Errorf("update account inspection run progress: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read account inspection run progress update result: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("account inspection run %d is not running", runID)
	}
	return nil
}

func (store *guardianStore) insertAccountInspectionResult(result accountInspectionResult) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	quotaWindows, err := json.Marshal(result.QuotaWindows)
	if err != nil {
		return fmt.Errorf("encode account inspection quota windows: %w", err)
	}
	_, err = store.database.Exec(`INSERT INTO account_inspection_results(
		run_id, account_key, file_name, display_account, auth_index, account_id, provider,
		disabled, probed, status, state, action, action_reason, action_status, executed_action,
		action_error, status_code, used_percent, is_quota, error, plan_type, quota_windows_json,
		monthly_limit_cents, monthly_used_cents, error_kind, error_detail, schedule_group,
		priority, original_priority, recover_at_ms, created_at_ms
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(run_id, auth_index) DO UPDATE SET
		account_key=excluded.account_key, file_name=excluded.file_name, display_account=excluded.display_account,
		account_id=excluded.account_id, provider=excluded.provider, disabled=excluded.disabled, probed=excluded.probed,
		status=excluded.status, state=excluded.state, action=excluded.action,
		action_reason=excluded.action_reason, action_status=excluded.action_status,
		executed_action=excluded.executed_action, action_error=excluded.action_error,
		status_code=excluded.status_code, used_percent=excluded.used_percent, is_quota=excluded.is_quota,
		error=excluded.error, plan_type=excluded.plan_type, quota_windows_json=excluded.quota_windows_json,
		monthly_limit_cents=excluded.monthly_limit_cents, monthly_used_cents=excluded.monthly_used_cents,
		error_kind=excluded.error_kind, error_detail=excluded.error_detail, schedule_group=excluded.schedule_group,
		priority=excluded.priority, original_priority=excluded.original_priority,
		recover_at_ms=excluded.recover_at_ms, created_at_ms=excluded.created_at_ms`,
		result.RunID, result.AccountKey, result.FileName, result.DisplayAccount, result.AuthIndex, result.AccountID,
		result.Provider, result.Disabled, result.Probed, result.Status, result.State, result.Action, result.ActionReason,
		result.ActionStatus, result.ExecutedAction, result.ActionError, nullableInt(result.StatusCode),
		nullableFloat(result.UsedPercent), result.IsQuota, result.Error, result.PlanType, string(quotaWindows),
		nullableFloat(result.MonthlyLimitCents), nullableFloat(result.MonthlyUsedCents), result.ErrorKind,
		result.ErrorDetail, nullableInt(result.ScheduleGroup), nullableInt(result.Priority),
		nullableInt(result.OriginalPriority), result.RecoverAtMS, result.CreatedAtMS)
	if err != nil {
		return fmt.Errorf("save account inspection result: %w", err)
	}
	return nil
}

func (store *guardianStore) finishAccountInspectionRun(runID int64, status string, processed, probed, healthy, quotaExhausted, abnormal, skipped int, runError string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	_, err := store.database.Exec(`UPDATE account_inspection_runs SET
		completed_at = ?, status = ?, processed = ?, probed = ?, healthy = ?,
		quota_exhausted = ?, abnormal = ?, skipped = ?, error = ?
		WHERE id = ?`, nowMillis(), status, processed, probed, healthy, quotaExhausted, abnormal, skipped, runError, runID)
	if err != nil {
		return fmt.Errorf("finish account inspection run: %w", err)
	}
	return nil
}

func (store *guardianStore) syncLatestRealtimeAccountInspection(authIndex string, priority int, recoverAtMS int64) error {
	status, errorKind, reason, isQuota := "", "", "", 0
	switch priority {
	case accountInspectionPriorityQuota:
		status = "quota_exhausted"
		errorKind = "quota_exhausted"
		reason = "实时守护检测到 xAI 额度耗尽，priority 为 -1"
		isQuota = 1
	case accountInspectionPriorityDegraded:
		status = "abnormal"
		errorKind = "account_abnormal"
		reason = "实时守护检测到账号异常，priority 为 -8"
	default:
		return fmt.Errorf("unsupported realtime inspection priority %d", priority)
	}

	store.mutex.Lock()
	defer store.mutex.Unlock()
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin realtime account inspection update: %w", err)
	}
	defer tx.Rollback()

	var runID int64
	if err := tx.QueryRow(`SELECT id FROM account_inspection_runs ORDER BY id DESC LIMIT 1`).Scan(&runID); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return fmt.Errorf("read latest account inspection run for realtime update: %w", err)
	}
	updated, err := tx.Exec(`UPDATE account_inspection_results SET
		status = ?, state = 'failed', action = 'priority_adjustment', action_reason = ?,
		action_status = 'success', executed_action = 'priority_adjustment', action_error = '',
		is_quota = ?, error_kind = ?, error_detail = ?, error = '', priority = ?, recover_at_ms = ?
		WHERE run_id = ? AND auth_index = ?`,
		status, reason, isQuota, errorKind, reason, priority, recoverAtMS, runID, authIndex)
	if err != nil {
		return fmt.Errorf("update latest realtime account inspection status: %w", err)
	}
	rows, err := updated.RowsAffected()
	if err != nil {
		return fmt.Errorf("read latest realtime account inspection update count: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("latest account inspection run %d contains %d rows for auth %q", runID, rows, authIndex)
	}
	if _, err := tx.Exec(`UPDATE account_inspection_runs SET
		processed = (SELECT COUNT(*) FROM account_inspection_results WHERE run_id = ?),
		probed = (SELECT COALESCE(SUM(probed), 0) FROM account_inspection_results WHERE run_id = ?),
		healthy = (SELECT COUNT(*) FROM account_inspection_results WHERE run_id = ? AND status = 'healthy'),
		quota_exhausted = (SELECT COUNT(*) FROM account_inspection_results WHERE run_id = ? AND status = 'quota_exhausted'),
		abnormal = (SELECT COUNT(*) FROM account_inspection_results WHERE run_id = ? AND status = 'abnormal'),
		skipped = (SELECT COUNT(*) FROM account_inspection_results WHERE run_id = ? AND status = 'skipped')
		WHERE id = ?`, runID, runID, runID, runID, runID, runID, runID); err != nil {
		return fmt.Errorf("recount latest account inspection run after realtime update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit latest realtime account inspection update: %w", err)
	}
	return nil
}

func (store *guardianStore) listAccountInspectionRuns(limit int) (accountInspectionRunList, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	var total int
	if err := store.database.QueryRow(`SELECT COUNT(*) FROM account_inspection_runs`).Scan(&total); err != nil {
		return accountInspectionRunList{}, fmt.Errorf("count account inspection runs: %w", err)
	}
	rows, err := store.database.Query(`SELECT id, trigger_type, started_at, completed_at, status, total, processed, probed, healthy, quota_exhausted, abnormal, skipped, error
		FROM account_inspection_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return accountInspectionRunList{}, fmt.Errorf("list account inspection runs: %w", err)
	}
	defer rows.Close()
	items := make([]accountInspectionRun, 0)
	for rows.Next() {
		run, err := scanAccountInspectionRun(rows)
		if err != nil {
			return accountInspectionRunList{}, err
		}
		items = append(items, run)
	}
	if err := rows.Err(); err != nil {
		return accountInspectionRunList{}, fmt.Errorf("read account inspection runs: %w", err)
	}
	return accountInspectionRunList{Items: items, Total: total}, nil
}

func (store *guardianStore) latestAccountInspection() (accountInspectionResponse, error) {
	var runID int64
	err := store.database.QueryRow(`SELECT id FROM account_inspection_runs ORDER BY id DESC LIMIT 1`).Scan(&runID)
	if err == sql.ErrNoRows {
		return accountInspectionResponse{Items: []accountInspectionResult{}}, nil
	}
	if err != nil {
		return accountInspectionResponse{}, fmt.Errorf("read latest account inspection run: %w", err)
	}
	return store.accountInspection(runID)
}

func (store *guardianStore) accountInspection(runID int64) (accountInspectionResponse, error) {
	row := store.database.QueryRow(`SELECT id, trigger_type, started_at, completed_at, status, total, processed, probed, healthy, quota_exhausted, abnormal, skipped, error
		FROM account_inspection_runs WHERE id = ?`, runID)
	run, err := scanAccountInspectionRun(row)
	if err != nil {
		return accountInspectionResponse{}, err
	}
	items, err := store.accountInspectionResults(runID)
	if err != nil {
		return accountInspectionResponse{}, err
	}
	return accountInspectionResponse{Run: &run, Items: items}, nil
}

func (store *guardianStore) accountInspectionResults(runID int64) ([]accountInspectionResult, error) {
	rows, err := store.database.Query(`SELECT id, run_id, account_key, file_name, display_account, auth_index, account_id,
		provider, disabled, probed, status, state, action, action_reason, action_status, executed_action, action_error,
		status_code, used_percent, is_quota, error, plan_type, quota_windows_json, monthly_limit_cents,
		monthly_used_cents, error_kind, error_detail, schedule_group, priority, original_priority, recover_at_ms, created_at_ms
		FROM account_inspection_results WHERE run_id = ? ORDER BY file_name COLLATE NOCASE, display_account COLLATE NOCASE, auth_index`, runID)
	if err != nil {
		return nil, fmt.Errorf("list account inspection results: %w", err)
	}
	defer rows.Close()
	items := make([]accountInspectionResult, 0)
	for rows.Next() {
		var item accountInspectionResult
		var disabled, probed, isQuota int
		var statusCode, scheduleGroup, priority, originalPriority sql.NullInt64
		var usedPercentFloat, monthlyLimitFloat, monthlyUsedFloat sql.NullFloat64
		var quotaWindowsJSON string
		if err := rows.Scan(&item.ID, &item.RunID, &item.AccountKey, &item.FileName, &item.DisplayAccount,
			&item.AuthIndex, &item.AccountID, &item.Provider, &disabled, &probed, &item.Status, &item.State, &item.Action,
			&item.ActionReason, &item.ActionStatus, &item.ExecutedAction, &item.ActionError, &statusCode,
			&usedPercentFloat, &isQuota, &item.Error, &item.PlanType, &quotaWindowsJSON, &monthlyLimitFloat,
			&monthlyUsedFloat, &item.ErrorKind, &item.ErrorDetail, &scheduleGroup, &priority, &originalPriority,
			&item.RecoverAtMS, &item.CreatedAtMS); err != nil {
			return nil, fmt.Errorf("scan account inspection result: %w", err)
		}
		item.Disabled = disabled != 0
		item.Probed = probed != 0
		item.IsQuota = isQuota != 0
		if statusCode.Valid {
			value := int(statusCode.Int64)
			item.StatusCode = &value
		}
		if usedPercentFloat.Valid {
			value := usedPercentFloat.Float64
			item.UsedPercent = &value
		}
		if monthlyLimitFloat.Valid {
			value := monthlyLimitFloat.Float64
			item.MonthlyLimitCents = &value
		}
		if monthlyUsedFloat.Valid {
			value := monthlyUsedFloat.Float64
			item.MonthlyUsedCents = &value
		}
		if scheduleGroup.Valid {
			value := int(scheduleGroup.Int64)
			item.ScheduleGroup = &value
		}
		if priority.Valid {
			value := int(priority.Int64)
			item.Priority = &value
		}
		if originalPriority.Valid {
			value := int(originalPriority.Int64)
			item.OriginalPriority = &value
		}
		if err := json.Unmarshal([]byte(quotaWindowsJSON), &item.QuotaWindows); err != nil {
			return nil, fmt.Errorf("decode account inspection quota windows: %w", err)
		}
		if item.QuotaWindows == nil {
			item.QuotaWindows = []accountInspectionQuotaWindow{}
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read account inspection results: %w", err)
	}
	return items, nil
}

func (store *guardianStore) latestAccountInspectionResultByAuth() (map[string]accountInspectionResult, error) {
	var runID int64
	err := store.database.QueryRow(`SELECT id FROM account_inspection_runs ORDER BY id DESC LIMIT 1`).Scan(&runID)
	if err == sql.ErrNoRows {
		return map[string]accountInspectionResult{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest account inspection run: %w", err)
	}
	items, err := store.accountInspectionResults(runID)
	if err != nil {
		return nil, err
	}
	result := make(map[string]accountInspectionResult, len(items))
	for _, item := range items {
		result[item.AuthIndex] = item
	}
	return result, nil
}

func (store *guardianStore) accountInspectionProfile(authIndex string) (string, error) {
	var accountType string
	err := store.database.QueryRow(`SELECT account_type FROM account_inspection_profiles WHERE auth_index = ?`, authIndex).Scan(&accountType)
	if err == sql.ErrNoRows {
		return "unknown", nil
	}
	if err != nil {
		return "", fmt.Errorf("read account inspection profile: %w", err)
	}
	return accountType, nil
}

func (store *guardianStore) saveAccountInspectionProfile(authIndex, accountType string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	_, err := store.database.Exec(`INSERT INTO account_inspection_profiles(auth_index, account_type, updated_at_ms) VALUES (?, ?, ?)
		ON CONFLICT(auth_index) DO UPDATE SET account_type = excluded.account_type, updated_at_ms = excluded.updated_at_ms`, authIndex, accountType, nowMillis())
	if err != nil {
		return fmt.Errorf("save account inspection profile: %w", err)
	}
	return nil
}

func (store *guardianStore) accountInspectionPriorityAdjustment(authIndex string) (accountInspectionPriorityAdjustment, bool, error) {
	var adjustment accountInspectionPriorityAdjustment
	var originalPriority sql.NullInt64
	err := store.database.QueryRow(`SELECT auth_index, file_name, original_priority, adjusted_priority, recover_at_ms
		FROM account_inspection_priority_adjustments WHERE auth_index = ?`, authIndex).Scan(
		&adjustment.AuthIndex, &adjustment.FileName, &originalPriority, &adjustment.AdjustedPriority, &adjustment.RecoverAtMS)
	if err == sql.ErrNoRows {
		return accountInspectionPriorityAdjustment{}, false, nil
	}
	if err != nil {
		return accountInspectionPriorityAdjustment{}, false, fmt.Errorf("read account inspection priority adjustment: %w", err)
	}
	if originalPriority.Valid {
		value := int(originalPriority.Int64)
		adjustment.OriginalPriority = &value
	}
	return adjustment, true, nil
}

func (store *guardianStore) saveAccountInspectionPriorityAdjustment(adjustment accountInspectionPriorityAdjustment) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	_, err := store.database.Exec(`INSERT INTO account_inspection_priority_adjustments(auth_index, file_name, original_priority, adjusted_priority, recover_at_ms)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(auth_index) DO UPDATE SET file_name = excluded.file_name,
		original_priority = excluded.original_priority, adjusted_priority = excluded.adjusted_priority, recover_at_ms = excluded.recover_at_ms`,
		adjustment.AuthIndex, adjustment.FileName, nullableInt(adjustment.OriginalPriority), adjustment.AdjustedPriority, adjustment.RecoverAtMS)
	if err != nil {
		return fmt.Errorf("save account inspection priority adjustment: %w", err)
	}
	return nil
}

func (store *guardianStore) deleteAccountInspectionPriorityAdjustment(authIndex string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if _, err := store.database.Exec(`DELETE FROM account_inspection_priority_adjustments WHERE auth_index = ?`, authIndex); err != nil {
		return fmt.Errorf("delete account inspection priority adjustment: %w", err)
	}
	return nil
}

func (store *guardianStore) realtimeDegradationPriorityAdjustment(authIndex string) (accountInspectionPriorityAdjustment, bool, error) {
	var adjustment accountInspectionPriorityAdjustment
	var originalPriority sql.NullInt64
	err := store.database.QueryRow(`SELECT auth_index, file_name, original_priority, adjusted_priority, recover_at_ms
		FROM realtime_degradation_priority_adjustments WHERE auth_index = ?`, authIndex).Scan(
		&adjustment.AuthIndex, &adjustment.FileName, &originalPriority, &adjustment.AdjustedPriority, &adjustment.RecoverAtMS)
	if err == sql.ErrNoRows {
		return accountInspectionPriorityAdjustment{}, false, nil
	}
	if err != nil {
		return accountInspectionPriorityAdjustment{}, false, fmt.Errorf("read realtime degradation priority adjustment: %w", err)
	}
	if originalPriority.Valid {
		value := int(originalPriority.Int64)
		adjustment.OriginalPriority = &value
	}
	return adjustment, true, nil
}

func (store *guardianStore) saveRealtimeDegradationPriorityAdjustment(adjustment accountInspectionPriorityAdjustment) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	_, err := store.database.Exec(`INSERT INTO realtime_degradation_priority_adjustments(auth_index, file_name, original_priority, adjusted_priority, recover_at_ms)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(auth_index) DO UPDATE SET file_name = excluded.file_name,
		original_priority = excluded.original_priority, adjusted_priority = excluded.adjusted_priority, recover_at_ms = excluded.recover_at_ms`,
		adjustment.AuthIndex, adjustment.FileName, nullableInt(adjustment.OriginalPriority), adjustment.AdjustedPriority, adjustment.RecoverAtMS)
	if err != nil {
		return fmt.Errorf("save realtime degradation priority adjustment: %w", err)
	}
	return nil
}

func (store *guardianStore) deleteRealtimeDegradationPriorityAdjustment(authIndex string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if _, err := store.database.Exec(`DELETE FROM realtime_degradation_priority_adjustments WHERE auth_index = ?`, authIndex); err != nil {
		return fmt.Errorf("delete realtime degradation priority adjustment: %w", err)
	}
	return nil
}

func (store *guardianStore) finalizePermanentDegradation(authIndex string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin permanent degradation cleanup: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM realtime_degradation_priority_adjustments WHERE auth_index = ?`, authIndex); err != nil {
		return fmt.Errorf("delete permanent realtime degradation priority adjustment: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM account_inspection_priority_adjustments WHERE auth_index = ?`, authIndex); err != nil {
		return fmt.Errorf("delete permanent account inspection priority adjustment: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM degradation_states WHERE auth_index = ?`, authIndex); err != nil {
		return fmt.Errorf("delete permanent degradation state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit permanent degradation cleanup: %w", err)
	}
	return nil
}

func (store *guardianStore) accountInspectionRealtimeCooldown(authIndex string, nowMS int64) (int64, int, bool, error) {
	var coolingUntil int64
	var count int
	err := store.database.QueryRow(`SELECT cooling_until, count FROM degradation_states WHERE auth_index = ?`, authIndex).Scan(&coolingUntil, &count)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("read realtime degradation cooldown: %w", err)
	}
	return coolingUntil, count, coolingUntil == degradationPermanentCoolingUntil || coolingUntil > nowMS, nil
}

func (store *guardianStore) countAccountInspectionResults(runID int64) (processed, probed, healthy, quotaExhausted, abnormal, skipped int, err error) {
	err = store.database.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN probed = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'healthy' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN is_quota = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'abnormal' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN action_status = 'skipped' OR status = 'skipped' THEN 1 ELSE 0 END), 0)
		FROM account_inspection_results WHERE run_id = ?`, runID).Scan(&processed, &probed, &healthy, &quotaExhausted, &abnormal, &skipped)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("count account inspection results: %w", err)
	}
	return processed, probed, healthy, quotaExhausted, abnormal, skipped, nil
}

func scanAccountInspectionRun(scanner sqlRowScanner) (accountInspectionRun, error) {
	var run accountInspectionRun
	var triggerType string
	var startedAt, finishedAt int64
	var status string
	var errorMessage string
	if err := scanner.Scan(&run.ID, &triggerType, &startedAt, &finishedAt, &status, &run.TotalFiles, &run.ProcessedCount,
		&run.ProbeSetCount, &run.HealthyCount, &run.QuotaExhaustedCount, &run.AbnormalCount, &run.SkippedCount, &errorMessage); err != nil {
		if err == sql.ErrNoRows {
			return accountInspectionRun{}, fmt.Errorf("%w: account inspection run not found", sql.ErrNoRows)
		}
		return accountInspectionRun{}, fmt.Errorf("scan account inspection run: %w", err)
	}
	run.Status = status
	run.TriggerType = triggerType
	run.StartedAtMS = startedAt
	run.FinishedAtMS = finishedAt
	run.Error = errorMessage
	return run, nil
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nowMillis() int64 {
	return time.Now().UnixMilli()
}
