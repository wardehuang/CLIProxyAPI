package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"golang.org/x/net/proxy"
)

const (
	accountInspectionWorkerCount  = 4
	accountInspectionBodyLimit    = 1024 * 1024
	accountInspectionDetailLimit  = 400
	accountInspectionProbeURL     = "https://cli-chat-proxy.grok.com/v1/billing"
	accountInspectionCreditsURL   = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	accountInspectionClientVer    = "0.2.101"
	accountInspectionUserAgent    = "grok-pager/0.2.101 grok-shell/0.2.101 (macos; aarch64)"
	accountInspectionQuotaGrace   = time.Minute
	accountInspectionQuotaDefault = 24 * time.Hour
	accountInspectionRetryWindow  = 30 * time.Second
)

const (
	accountInspectionPriorityQuota        = -1
	accountInspectionPriorityAbnormal     = -2
	accountInspectionPriorityLegacy       = -3
	accountInspectionPriorityUnauthorized = -4
	accountInspectionPriorityDisabled     = -5
	accountInspectionPriorityPermanent    = -6
	accountInspectionPrioritySSOExpired   = -7
	accountInspectionPriorityDegraded     = -8
	accountInspectionPriorityHealthy      = 1
)

type accountInspectionWorkResult struct {
	result accountInspectionResult
	probed bool
	err    error
}

type xaiInspectionResponse struct {
	statusCode int
	header     http.Header
	body       []byte
}

type xaiInspectionOutcome struct {
	alive        bool
	quota        bool
	statusCode   int
	errorKind    string
	detail       string
	recoveryAtMS int64
}

type xaiBillingSnapshot struct {
	quotaWindows      []accountInspectionQuotaWindow
	monthlyLimitCents *float64
	monthlyUsedCents  *float64
	recoveryAtMS      int64
}

func runAccountInspection(ctx context.Context, store *guardianStore, triggerType string) error {
	inspectionMutex.Lock()
	defer inspectionMutex.Unlock()

	runID, entries, previousResults, err := prepareAccountInspectionRun(store, triggerType)
	if err != nil {
		return err
	}
	return runPreparedAccountInspection(ctx, store, runID, entries, previousResults)
}

func prepareAccountInspectionRun(store *guardianStore, triggerType string) (int64, []pluginapi.HostAuthFileEntry, map[string]accountInspectionResult, error) {
	entries, err := listXAIAuthEntries()
	if err != nil {
		runID, startErr := store.startAccountInspectionRun(0, triggerType)
		if startErr != nil {
			return 0, nil, nil, startErr
		}
		return runID, nil, nil, finishAccountInspectionPreparation(store, runID, err)
	}
	previousResults, err := store.latestAccountInspectionResultByAuth()
	if err != nil {
		return 0, nil, nil, err
	}
	runID, err := store.startAccountInspectionRun(len(entries), triggerType)
	if err != nil {
		return 0, nil, nil, err
	}
	return runID, entries, previousResults, nil
}

func finishAccountInspectionPreparation(store *guardianStore, runID int64, cause error) error {
	if finishErr := store.finishAccountInspectionRun(runID, "failed", 0, 0, 0, 0, 0, 0, sanitizeLogText(cause.Error())); finishErr != nil {
		return fmt.Errorf("%w; finish failed inspection run: %v", cause, finishErr)
	}
	return cause
}

func runPreparedAccountInspection(ctx context.Context, store *guardianStore, runID int64, entries []pluginapi.HostAuthFileEntry, previousResults map[string]accountInspectionResult) error {
	jobs := make(chan pluginapi.HostAuthFileEntry, len(entries))
	results := make(chan accountInspectionWorkResult, len(entries))
	workerCount := min(accountInspectionWorkerCount, len(entries))
	var workers sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for entry := range jobs {
				previous, hasPrevious := previousResults[entry.AuthIndex]
				item, probed, inspectErr := inspectXAIAccount(ctx, store, runID, entry, previous, hasPrevious)
				results <- accountInspectionWorkResult{result: item, probed: probed, err: inspectErr}
			}
		}()
	}
	for _, entry := range entries {
		jobs <- entry
	}
	close(jobs)
	go func() {
		workers.Wait()
		close(results)
	}()

	processed, probed, healthy, quotaExhausted, abnormal, skipped := 0, 0, 0, 0, 0, 0
	var runErr error
	for outcome := range results {
		item := outcome.result
		item.Probed = outcome.probed
		if item.RunID == 0 {
			item.RunID = runID
		}
		if outcome.err != nil {
			if runErr == nil {
				runErr = outcome.err
			}
			item.Status = "skipped"
			item.ActionStatus = "skipped"
			item.ActionReason = "巡检内部处理失败，未完成账号探测"
			item.ActionError = sanitizeLogText(outcome.err.Error())
		}
		if err := store.insertAccountInspectionResult(item); err != nil {
			if runErr == nil {
				runErr = err
			}
			continue
		}
		processed++
		if outcome.probed {
			probed++
		}
		switch item.Status {
		case "healthy":
			healthy++
		case "quota_exhausted":
			quotaExhausted++
		case "abnormal":
			abnormal++
		case "skipped":
			skipped++
		}
		if err := store.updateAccountInspectionRunProgress(runID, processed, probed, healthy, quotaExhausted, abnormal, skipped); err != nil && runErr == nil {
			runErr = err
		}
	}
	if ctx.Err() != nil && runErr == nil {
		runErr = ctx.Err()
	}
	status := "completed"
	runErrorText := ""
	if runErr != nil {
		status = "failed"
		runErrorText = sanitizeLogText(runErr.Error())
	}
	if err := store.finishAccountInspectionRun(runID, status, processed, probed, healthy, quotaExhausted, abnormal, skipped, runErrorText); err != nil {
		if runErr == nil {
			runErr = err
		}
	}
	_ = store.appendLog(logLevelInfo, "inspection.completed", "xAI 账号巡检完成", fmt.Sprintf("run_id=%d total=%d processed=%d probed=%d healthy=%d quota=%d abnormal=%d skipped=%d", runID, len(entries), processed, probed, healthy, quotaExhausted, abnormal, skipped))
	return runErr
}

func settlePermanentRealtimeDegradation(store *guardianStore, authIndex string, currentPriority int, coolingUntil int64, degradationCount int) (int, bool, bool, error) {
	if currentPriority != accountInspectionPriorityPermanent && coolingUntil != degradationPermanentCoolingUntil && degradationCount < 3 {
		return currentPriority, false, false, nil
	}

	store.realtimeDegradationLock.Lock()
	defer store.realtimeDegradationLock.Unlock()
	entry, err := findXAIAuthEntry(authIndex)
	if err != nil {
		return currentPriority, true, false, err
	}
	state, hasState, err := store.degradationStateForAuth(authIndex)
	if err != nil {
		return entry.Priority, true, false, err
	}
	permanent := entry.Priority == accountInspectionPriorityPermanent || (hasState && (state.Count >= 3 || state.CoolingUntil == degradationPermanentCoolingUntil))
	if !permanent {
		return entry.Priority, false, false, nil
	}
	priorityUpdated := entry.Priority != accountInspectionPriorityPermanent
	if priorityUpdated {
		if err := updateXAIAuthPriority(authIndex, accountInspectionPriorityPermanent); err != nil {
			return entry.Priority, true, false, err
		}
	}
	if err := store.finalizePermanentDegradation(authIndex); err != nil {
		return accountInspectionPriorityPermanent, true, priorityUpdated, err
	}
	return accountInspectionPriorityPermanent, true, priorityUpdated, nil
}

func inspectXAIAccount(ctx context.Context, store *guardianStore, runID int64, entry pluginapi.HostAuthFileEntry, previous accountInspectionResult, hasPrevious bool) (accountInspectionResult, bool, error) {
	result := newAccountInspectionResult(runID, entry, previous, hasPrevious)
	if ctx.Err() != nil {
		return preserveAccountInspectionResult(result, previous, hasPrevious, "巡检已取消，未发送请求"), false, nil
	}
	priority := accountPriorityPointer(entry.Priority)
	if entry.Priority != accountInspectionPriorityPermanent && (entry.Disabled || (priority != nil && *priority == accountInspectionPriorityDisabled)) {
		result.Disabled = true
		return preserveAccountInspectionResult(result, previous, hasPrevious, "账号已停用，未调用测活请求，保持停用状态"), false, nil
	}
	now := time.Now()
	coolingUntil, degradationCount, realtimeCooling, err := store.accountInspectionRealtimeCooldown(entry.AuthIndex, now.UnixMilli())
	if err != nil {
		return result, false, err
	}
	permanentPriority, permanent, priorityUpdated, err := settlePermanentRealtimeDegradation(store, entry.AuthIndex, entry.Priority, coolingUntil, degradationCount)
	if err != nil {
		result.Priority = accountInspectionIntPointer(permanentPriority)
		result.ActionReason = "实时降智永久终态处理失败，自动巡检不会探测该账号"
		setAccountInspectionPriorityError(&result, err)
		return result, false, nil
	}
	if permanent {
		result = preserveAccountInspectionResult(result, previous, hasPrevious, "账号 priority 为 -6，永久退出自动巡检；需人工恢复 priority")
		result.Priority = accountInspectionIntPointer(permanentPriority)
		if priorityUpdated {
			result.ActionStatus = "success"
			result.ExecutedAction = "priority_adjustment"
		} else {
			result.ActionStatus = "skipped"
		}
		return result, false, nil
	}
	if realtimeCooling {
		result = preserveAccountInspectionResult(result, previous, hasPrevious, fmt.Sprintf("实时降智冷却中，跳过巡检至 %s", time.UnixMilli(coolingUntil).UTC().Format(time.RFC3339)))
		if priority != nil && *priority == accountInspectionPrioritySSOExpired {
			result.ErrorKind = "sso_expired"
			result.ErrorDetail = previous.ErrorDetail
			result.ActionReason = "SSO 已失效，priority 为 -7，需重新登录获取新的 SSO，跳过巡检"
		}
		return result, false, nil
	}
	adjustment, hasAdjustment, err := store.accountInspectionPriorityAdjustment(entry.AuthIndex)
	if err != nil {
		return result, false, err
	}
	if priority != nil && *priority == accountInspectionPriorityQuota && hasAdjustment && adjustment.AdjustedPriority == accountInspectionPriorityQuota && adjustment.RecoverAtMS > now.UnixMilli() {
		result = preserveAccountInspectionResult(result, previous, hasPrevious, fmt.Sprintf("额度耗尽冷却中，跳过巡检至 %s", time.UnixMilli(adjustment.RecoverAtMS).UTC().Format(time.RFC3339)))
		result.Status = "quota_exhausted"
		result.IsQuota = true
		result.ErrorKind = "quota_exhausted"
		result.RecoverAtMS = adjustment.RecoverAtMS
		result.ActionStatus = "skipped"
		return result, false, nil
	}

	file, err := getXAIAuthFile(entry)
	if err != nil {
		result = applyAccountInspectionFailure(ctx, store, result, priority, xaiInspectionOutcome{errorKind: "request_error", detail: sanitizeLogText(err.Error())}, now)
		return result, true, nil
	}
	result.FileName = file.Name
	result.AuthIndex = file.Index
	result.AccountID = firstNonEmpty(stringField(file.Raw, "account_id"), stringField(file.Raw, "accountId"), stringField(file.Raw, "user_id"), stringField(file.Raw, "userId"), entry.Account)
	result.DisplayAccount = firstNonEmpty(stringField(file.Raw, "email"), entry.Email, entry.Account, file.Name)
	priority = accountPriorityPointer(file.Priority)
	result.Priority = priority
	result.Disabled = file.Disabled || (priority != nil && *priority == accountInspectionPriorityDisabled)
	result.ScheduleGroup = readNestedScheduleGroup(file.Raw)
	if result.Disabled {
		return preserveAccountInspectionResult(result, previous, hasPrevious, "账号已停用，跳过巡检"), false, nil
	}

	accountType, err := store.accountInspectionProfile(file.Index)
	if err != nil {
		return result, false, err
	}
	if accountType == "unknown" {
		accountType = normalizeInspectionAccountType(entry.AccountType)
	}
	if accountType == "unknown" && hasPrevious {
		accountType = normalizeInspectionAccountType(previous.PlanType)
	}
	result.PlanType = accountType
	result.AccountType = accountType
	result.Status = "checking"

	accessToken := strings.TrimSpace(stringField(file.Raw, "access_token"))
	if accessToken == "" {
		result = applyAccountInspectionFailure(ctx, store, result, priority, xaiInspectionOutcome{errorKind: "account_abnormal", detail: "access-token-missing"}, now)
		return result, true, nil
	}
	client, err := newAccountInspectionHTTPClient(file.ProxyURL)
	if err != nil {
		result = applyAccountInspectionFailure(ctx, store, result, priority, xaiInspectionOutcome{errorKind: "request_error", detail: sanitizeLogText(err.Error())}, now)
		return result, true, nil
	}
	defer client.CloseIdleConnections()

	billingUserID := firstNonEmpty(stringField(file.Raw, "sub"), stringField(file.Raw, "subject"), stringField(file.Raw, "user_id"), stringField(file.Raw, "userId"), result.AccountID)
	snapshot := xaiBillingSnapshot{quotaWindows: []accountInspectionQuotaWindow{}}
	monthlyProbed := false
	if accountType == "unknown" {
		monthly, outcome := probeXAIAccountBilling(ctx, client, accountInspectionProbeURL, accessToken, billingUserID)
		monthlyProbed = true
		if !outcome.alive {
			result = applyAccountInspectionFailure(ctx, store, result, priority, outcome, now)
			return result, true, nil
		}
		mergeXAIAccountBilling(&snapshot, monthly)
		accountType = resolveInspectionAccountType(monthly.monthlyLimitCents)
		result.PlanType = accountType
		result.AccountType = accountType
		if err := store.saveAccountInspectionProfile(file.Index, accountType); err != nil {
			_ = store.appendLog(logLevelWarn, "inspection.profile_save_failed", "保存 xAI 账号类型失败", sanitizeLogText(err.Error()))
		}
	}

	var outcome xaiInspectionOutcome
	if accountType == "super" && monthlyProbed {
		credits, creditsOutcome := probeXAIAccountBilling(ctx, client, accountInspectionCreditsURL, accessToken, billingUserID)
		mergeXAIAccountBilling(&snapshot, credits)
		outcome = creditsOutcome
	} else if accountType == "super" {
		monthlyResult := make(chan struct {
			snapshot xaiBillingSnapshot
			outcome  xaiInspectionOutcome
		}, 2)
		creditsResult := make(chan struct {
			snapshot xaiBillingSnapshot
			outcome  xaiInspectionOutcome
		}, 2)
		go func() {
			value, response := probeXAIAccountBilling(ctx, client, accountInspectionProbeURL, accessToken, billingUserID)
			monthlyResult <- struct {
				snapshot xaiBillingSnapshot
				outcome  xaiInspectionOutcome
			}{value, response}
		}()
		go func() {
			value, response := probeXAIAccountBilling(ctx, client, accountInspectionCreditsURL, accessToken, billingUserID)
			creditsResult <- struct {
				snapshot xaiBillingSnapshot
				outcome  xaiInspectionOutcome
			}{value, response}
		}()
		monthly := <-monthlyResult
		credits := <-creditsResult
		mergeXAIAccountBilling(&snapshot, monthly.snapshot)
		mergeXAIAccountBilling(&snapshot, credits.snapshot)
		if !monthly.outcome.alive {
			outcome = monthly.outcome
		} else {
			outcome = credits.outcome
		}
	} else {
		credits, creditsOutcome := probeXAIAccountBilling(ctx, client, accountInspectionCreditsURL, accessToken, billingUserID)
		mergeXAIAccountBilling(&snapshot, credits)
		outcome = creditsOutcome
	}
	applyXAIAccountBillingSnapshot(&result, snapshot)
	if !outcome.alive {
		result = applyAccountInspectionFailure(ctx, store, result, priority, outcome, now)
		return result, true, nil
	}

	result.Status = "healthy"
	result.State = "healthy"
	result.StatusCode = accountInspectionIntPointer(outcome.statusCode)
	result.Error = ""
	result.ErrorKind = ""
	result.ErrorDetail = ""
	result.IsQuota = false
	result.ActionReason = "xAI " + accountType + " billing 探测成功"
	result.Priority, result.OriginalPriority, result.RecoverAtMS = restoreAccountInspectionPriority(ctx, store, &result, priority)
	if result.ErrorKind == "priority_restore_failed" {
		result.ActionReason += "；priority 恢复失败"
	}
	result.CreatedAtMS = nowMillis()
	return result, true, nil
}

func newAccountInspectionResult(runID int64, entry pluginapi.HostAuthFileEntry, previous accountInspectionResult, hasPrevious bool) accountInspectionResult {
	priority := accountPriorityPointer(entry.Priority)
	result := accountInspectionResult{
		RunID:          runID,
		AccountKey:     entry.AuthIndex,
		FileName:       authEntryName(entry),
		DisplayAccount: firstNonEmpty(entry.Email, entry.Account, authEntryName(entry)),
		AuthIndex:      entry.AuthIndex,
		AccountID:      entry.Account,
		Provider:       "xai",
		Disabled:       entry.Disabled || (priority != nil && *priority == accountInspectionPriorityDisabled),
		Status:         "pending",
		State:          entry.Status,
		Action:         "keep",
		ActionReason:   "等待巡检",
		ActionStatus:   "none",
		PlanType:       normalizeInspectionAccountType(entry.AccountType),
		AccountType:    normalizeInspectionAccountType(entry.AccountType),
		Priority:       priority,
		QuotaWindows:   []accountInspectionQuotaWindow{},
		CreatedAtMS:    nowMillis(),
	}
	if hasPrevious {
		result.QuotaWindows = append(result.QuotaWindows[:0], previous.QuotaWindows...)
		result.UsedPercent = previous.UsedPercent
		result.MonthlyLimitCents = previous.MonthlyLimitCents
		result.MonthlyUsedCents = previous.MonthlyUsedCents
		if result.PlanType == "unknown" {
			result.PlanType = normalizeInspectionAccountType(previous.PlanType)
			result.AccountType = result.PlanType
		}
	}
	return result
}

func preserveAccountInspectionResult(result, previous accountInspectionResult, hasPrevious bool, reason string) accountInspectionResult {
	if hasPrevious {
		result.QuotaWindows = append([]accountInspectionQuotaWindow{}, previous.QuotaWindows...)
		result.UsedPercent = previous.UsedPercent
		result.MonthlyLimitCents = previous.MonthlyLimitCents
		result.MonthlyUsedCents = previous.MonthlyUsedCents
	}
	result.Status = "skipped"
	result.ActionStatus = "skipped"
	result.ActionReason = reason
	result.StatusCode = nil
	result.Error = ""
	result.IsQuota = false
	result.ErrorKind = ""
	result.ErrorDetail = ""
	result.CreatedAtMS = nowMillis()
	return result
}

func applyAccountInspectionFailure(ctx context.Context, store *guardianStore, result accountInspectionResult, currentPriority *int, outcome xaiInspectionOutcome, inspectionTime time.Time) accountInspectionResult {
	store.realtimeDegradationLock.Lock()
	defer store.realtimeDegradationLock.Unlock()
	if outcome.statusCode > 0 {
		result.StatusCode = accountInspectionIntPointer(outcome.statusCode)
	}
	result.ErrorDetail = truncateInspectionText(sanitizeLogText(outcome.detail), accountInspectionDetailLimit)
	result.ErrorKind = outcome.errorKind
	result.Status = "abnormal"
	result.State = "failed"
	result.IsQuota = outcome.quota
	fallbackPriority := 0
	if currentPriority != nil {
		fallbackPriority = *currentPriority
	}
	latestPriority, err := getXAIAuthPriority(result.AuthIndex, fallbackPriority)
	if err != nil {
		result.Priority = currentPriority
		result.ActionReason = "读取 Auth priority 失败，未执行自动调整"
		setAccountInspectionPriorityError(&result, err)
		result.CreatedAtMS = nowMillis()
		return result
	}
	currentPriority = accountInspectionIntPointer(latestPriority)
	result.Priority = currentPriority
	coolingUntil := int64(0)
	degradationCount := 0
	realtimeCooling := false
	if currentPriority == nil || *currentPriority != accountInspectionPriorityPermanent {
		coolingUntil, degradationCount, realtimeCooling, err = store.accountInspectionRealtimeCooldown(result.AuthIndex, inspectionTime.UnixMilli())
		if err != nil {
			result.ActionReason = "读取实时降智恢复状态失败，未执行自动调整"
			setAccountInspectionPriorityError(&result, err)
			result.CreatedAtMS = nowMillis()
			return result
		}
	}
	if (currentPriority != nil && *currentPriority == accountInspectionPriorityPermanent) || coolingUntil == degradationPermanentCoolingUntil || degradationCount >= 3 {
		if currentPriority == nil || *currentPriority != accountInspectionPriorityPermanent {
			if err := updateXAIAuthPriority(result.AuthIndex, accountInspectionPriorityPermanent); err != nil {
				result.ActionReason = "第 3 次实时降智永久终态写入 priority -6 失败"
				setAccountInspectionPriorityError(&result, err)
				result.CreatedAtMS = nowMillis()
				return result
			}
			result.ActionStatus = "success"
			result.ExecutedAction = "priority_adjustment"
		} else {
			result.ActionStatus = "none"
		}
		result.Priority = accountInspectionIntPointer(accountInspectionPriorityPermanent)
		result.ActionReason = "第 3 次实时降智进入永久终态，priority 为 -6"
		result.CreatedAtMS = nowMillis()
		return result
	}
	if currentPriority != nil && *currentPriority == accountInspectionPriorityDegraded {
		adjustment, hasAdjustment, err := store.realtimeDegradationPriorityAdjustment(result.AuthIndex)
		if err != nil {
			result.Priority = currentPriority
			result.ActionReason = "读取实时降智 priority adjustment 失败，保留 priority -8"
			setAccountInspectionPriorityError(&result, err)
			result.CreatedAtMS = nowMillis()
			return result
		}
		if hasAdjustment && adjustment.AdjustedPriority == accountInspectionPriorityDegraded {
			result.OriginalPriority = adjustment.OriginalPriority
			result.RecoverAtMS = adjustment.RecoverAtMS
		}
		if outcome.quota {
			result.Status = "quota_exhausted"
			result.ErrorKind = "quota_exhausted"
		}
		result.Priority = currentPriority
		result.ActionStatus = "none"
		switch {
		case coolingUntil == degradationPermanentCoolingUntil:
			result.ActionReason = "第 3 次实时降智终态待清理，巡检未恢复，保留当前 priority"
		case realtimeCooling:
			result.ActionReason = "实时降智冷却中，账号巡检探测未成功，保留 priority -8"
		case coolingUntil > 0:
			result.ActionReason = "实时降智冷却已结束，账号巡检探测未成功，保留 priority -8 等待后续巡检"
		default:
			result.ActionReason = "实时降智恢复状态缺失，巡检未成功，不自动调整 priority -8"
		}
		if !hasAdjustment || adjustment.AdjustedPriority != accountInspectionPriorityDegraded {
			result.ActionReason += "；priority adjustment 不匹配"
		}
		result.CreatedAtMS = nowMillis()
		return result
	}
	if outcome.quota {
		result.Status = "quota_exhausted"
		result.ErrorKind = "quota_exhausted"
		result.ActionReason = "xAI 额度耗尽，priority 调整为 -1"
		result.RecoverAtMS = resolveInspectionQuotaRecovery(outcome.recoveryAtMS, inspectionTime)
		result.Priority, result.OriginalPriority, result.RecoverAtMS = lowerAccountInspectionPriority(ctx, store, &result, currentPriority, accountInspectionPriorityQuota, result.RecoverAtMS)
	} else {
		targetPriority := accountInspectionPriorityAbnormal
		if outcome.statusCode == http.StatusUnauthorized {
			targetPriority = accountInspectionPriorityUnauthorized
		}
		if outcome.errorKind == "request_error" {
			result.Error = result.ErrorDetail
		} else {
			result.ErrorKind = "account_abnormal"
		}
		if outcome.statusCode > 0 {
			result.ActionReason = fmt.Sprintf("xAI 请求返回 HTTP %d，priority 调整为 %d", outcome.statusCode, targetPriority)
		} else {
			result.ActionReason = fmt.Sprintf("xAI 请求失败，priority 调整为 %d", targetPriority)
		}
		result.Priority, result.OriginalPriority, result.RecoverAtMS = lowerAccountInspectionPriority(ctx, store, &result, currentPriority, targetPriority, inspectionTime.Add(accountInspectionRetryWindow).UnixMilli())
	}
	if result.ActionStatus == "failed" {
		result.ActionReason += "；priority 调整失败"
	}
	result.CreatedAtMS = nowMillis()
	return result
}

func lowerAccountInspectionPriority(ctx context.Context, store *guardianStore, result *accountInspectionResult, currentPriority *int, targetPriority int, recoverAtMS int64) (*int, *int, int64) {
	previousAdjustment, hadPreviousAdjustment, err := store.accountInspectionPriorityAdjustment(result.AuthIndex)
	if err != nil {
		setAccountInspectionPriorityError(result, err)
		return currentPriority, nil, recoverAtMS
	}
	originalPriority := currentPriority
	if hadPreviousAdjustment && currentPriority != nil && *currentPriority == previousAdjustment.AdjustedPriority {
		originalPriority = previousAdjustment.OriginalPriority
	}
	if currentPriority != nil && *currentPriority < 0 && originalPriority != nil {
		originalPriority = accountInspectionIntPointer(accountInspectionPriorityHealthy)
	}
	adjustment := accountInspectionPriorityAdjustment{
		AuthIndex:        result.AuthIndex,
		FileName:         result.FileName,
		OriginalPriority: originalPriority,
		AdjustedPriority: targetPriority,
		RecoverAtMS:      recoverAtMS,
	}
	if err := store.saveAccountInspectionPriorityAdjustment(adjustment); err != nil {
		setAccountInspectionPriorityError(result, err)
		return currentPriority, originalPriority, recoverAtMS
	}
	if currentPriority != nil && *currentPriority == targetPriority {
		result.ActionStatus = "none"
		result.OriginalPriority = originalPriority
		return accountInspectionIntPointer(targetPriority), originalPriority, recoverAtMS
	}
	if err := updateXAIAuthPriority(result.AuthIndex, targetPriority); err != nil {
		var rollbackErr error
		if hadPreviousAdjustment {
			rollbackErr = store.saveAccountInspectionPriorityAdjustment(previousAdjustment)
		} else {
			rollbackErr = store.deleteAccountInspectionPriorityAdjustment(result.AuthIndex)
		}
		if rollbackErr != nil {
			err = fmt.Errorf("%w; rollback priority adjustment: %v", err, rollbackErr)
		}
		setAccountInspectionPriorityError(result, err)
		return currentPriority, originalPriority, recoverAtMS
	}
	result.ActionStatus = "success"
	result.ExecutedAction = "priority_adjustment"
	result.OriginalPriority = originalPriority
	return accountInspectionIntPointer(targetPriority), originalPriority, recoverAtMS
}

func restoreAccountInspectionPriority(ctx context.Context, store *guardianStore, result *accountInspectionResult, currentPriority *int) (*int, *int, int64) {
	store.realtimeDegradationLock.Lock()
	defer store.realtimeDegradationLock.Unlock()
	coolingUntil, degradationCount, realtimeCooling, err := store.accountInspectionRealtimeCooldown(result.AuthIndex, nowMillis())
	if err != nil {
		setAccountInspectionPriorityError(result, err)
		return currentPriority, nil, 0
	}
	refreshPriority := func() error {
		fallbackPriority := 0
		if currentPriority != nil {
			fallbackPriority = *currentPriority
		}
		latestPriority, err := getXAIAuthPriority(result.AuthIndex, fallbackPriority)
		if err != nil {
			return err
		}
		currentPriority = accountInspectionIntPointer(latestPriority)
		result.Priority = currentPriority
		return nil
	}
	if coolingUntil == degradationPermanentCoolingUntil || degradationCount >= 3 {
		if err := refreshPriority(); err != nil {
			setAccountInspectionPriorityError(result, err)
			return currentPriority, nil, coolingUntil
		}
		if currentPriority == nil || *currentPriority != accountInspectionPriorityPermanent {
			if err := updateXAIAuthPriority(result.AuthIndex, accountInspectionPriorityPermanent); err != nil {
				setAccountInspectionPriorityError(result, err)
				return currentPriority, nil, coolingUntil
			}
			result.ActionStatus = "success"
			result.ExecutedAction = "priority_adjustment"
		}
		if err := store.finalizePermanentDegradation(result.AuthIndex); err != nil {
			setAccountInspectionPriorityError(result, err)
			return accountInspectionIntPointer(accountInspectionPriorityPermanent), nil, coolingUntil
		}
		result.ActionReason = "第 3 次实时降智已永久退出自动巡检；priority 保持 -6，需人工恢复"
		return accountInspectionIntPointer(accountInspectionPriorityPermanent), nil, 0
	}
	if realtimeCooling {
		if currentPriority == nil || *currentPriority != accountInspectionPriorityDegraded {
			if err := updateXAIAuthPriority(result.AuthIndex, accountInspectionPriorityDegraded); err != nil {
				setAccountInspectionPriorityError(result, err)
				return currentPriority, nil, coolingUntil
			}
			result.ActionStatus = "success"
			result.ExecutedAction = "priority_adjustment"
		}
		result.ActionReason = "实时降智冷却中，跳过 priority 恢复"
		return accountInspectionIntPointer(accountInspectionPriorityDegraded), nil, coolingUntil
	}
	realtimeAdjustment, hasRealtimeAdjustment, err := store.realtimeDegradationPriorityAdjustment(result.AuthIndex)
	if err != nil {
		setAccountInspectionPriorityError(result, err)
		return currentPriority, nil, coolingUntil
	}
	adjustment, hasAdjustment, err := store.accountInspectionPriorityAdjustment(result.AuthIndex)
	if err != nil {
		setAccountInspectionPriorityError(result, err)
		return currentPriority, nil, coolingUntil
	}
	if coolingUntil > 0 || hasRealtimeAdjustment || hasAdjustment || currentPriority == nil || *currentPriority != accountInspectionPriorityHealthy {
		if err := refreshPriority(); err != nil {
			setAccountInspectionPriorityError(result, err)
			return currentPriority, nil, coolingUntil
		}
	}
	if coolingUntil > 0 {
		isRealtimePriority := currentPriority != nil && *currentPriority == accountInspectionPriorityDegraded
		isRealtimeAdjustment := hasRealtimeAdjustment && realtimeAdjustment.AdjustedPriority == accountInspectionPriorityDegraded
		if !isRealtimePriority || !isRealtimeAdjustment {
			result.ActionReason = "实时降智冷却已结束，但 priority 与 adjustment 不匹配，不自动恢复"
			result.ActionStatus = "none"
			return currentPriority, realtimeAdjustment.OriginalPriority, realtimeAdjustment.RecoverAtMS
		}
		if err := updateXAIAuthPriority(result.AuthIndex, accountInspectionPriorityHealthy); err != nil {
			setAccountInspectionPriorityError(result, err)
			return currentPriority, realtimeAdjustment.OriginalPriority, realtimeAdjustment.RecoverAtMS
		}
		result.ActionStatus = "success"
		result.ExecutedAction = "priority_restore"
		if err := store.deleteRealtimeDegradationPriorityAdjustment(result.AuthIndex); err != nil {
			setAccountInspectionPriorityError(result, err)
			return accountInspectionIntPointer(accountInspectionPriorityHealthy), realtimeAdjustment.OriginalPriority, realtimeAdjustment.RecoverAtMS
		}
		result.ActionReason = "实时降智冷却结束且账号巡检探测成功，priority 恢复为 1；连续次数保留"
		return accountInspectionIntPointer(accountInspectionPriorityHealthy), nil, 0
	}
	if hasRealtimeAdjustment {
		if currentPriority != nil && *currentPriority == realtimeAdjustment.AdjustedPriority {
			result.ActionReason = "实时降智恢复状态缺失，不自动恢复 priority -8"
			result.ActionStatus = "none"
			return currentPriority, realtimeAdjustment.OriginalPriority, realtimeAdjustment.RecoverAtMS
		}
		if err := store.deleteRealtimeDegradationPriorityAdjustment(result.AuthIndex); err != nil {
			setAccountInspectionPriorityError(result, err)
			return currentPriority, nil, 0
		}
	}
	if hasAdjustment && (currentPriority == nil || *currentPriority != adjustment.AdjustedPriority) {
		if err := store.deleteAccountInspectionPriorityAdjustment(result.AuthIndex); err != nil {
			setAccountInspectionPriorityError(result, err)
			return currentPriority, nil, 0
		}
		return currentPriority, nil, 0
	}
	if !hasAdjustment && !isInspectionManagedPriority(currentPriority) {
		return currentPriority, nil, 0
	}
	if currentPriority == nil || *currentPriority != accountInspectionPriorityHealthy {
		if err := updateXAIAuthPriority(result.AuthIndex, accountInspectionPriorityHealthy); err != nil {
			setAccountInspectionPriorityError(result, err)
			return currentPriority, adjustment.OriginalPriority, adjustment.RecoverAtMS
		}
		result.ActionStatus = "success"
		result.ExecutedAction = "priority_restore"
	}
	if hasAdjustment {
		if err := store.deleteAccountInspectionPriorityAdjustment(result.AuthIndex); err != nil {
			setAccountInspectionPriorityError(result, err)
			return accountInspectionIntPointer(accountInspectionPriorityHealthy), adjustment.OriginalPriority, adjustment.RecoverAtMS
		}
	}
	return accountInspectionIntPointer(accountInspectionPriorityHealthy), nil, 0
}

func probeXAIAccountBilling(ctx context.Context, client *http.Client, endpoint, accessToken, userID string) (xaiBillingSnapshot, xaiInspectionOutcome) {
	response, err := performXAIAccountBillingRequest(ctx, client, endpoint, accessToken, userID)
	if err != nil {
		return xaiBillingSnapshot{}, xaiInspectionOutcome{errorKind: "request_error", detail: sanitizeLogText(err.Error())}
	}
	outcome := classifyXAIAccountResponse(response)
	if !outcome.alive {
		return xaiBillingSnapshot{}, outcome
	}
	snapshot := xaiBillingSnapshot{quotaWindows: []accountInspectionQuotaWindow{}}
	var parseErr error
	if endpoint == accountInspectionProbeURL {
		parseErr = parseXAIAccountMonthlyBilling(response.body, &snapshot)
	} else {
		parseErr = parseXAIAccountCreditsBilling(response.body, &snapshot)
	}
	if parseErr != nil {
		return xaiBillingSnapshot{}, xaiInspectionOutcome{errorKind: "request_error", detail: "parse billing response: " + sanitizeLogText(parseErr.Error())}
	}
	return snapshot, outcome
}

func performXAIAccountBillingRequest(ctx context.Context, client *http.Client, endpoint, accessToken, userID string) (xaiInspectionResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return xaiInspectionResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	request.Header.Set("x-grok-client-version", accountInspectionClientVer)
	request.Header.Set("User-Agent", accountInspectionUserAgent)
	if userID = strings.TrimSpace(userID); userID != "" {
		request.Header.Set("x-userid", userID)
	}
	response, err := client.Do(request)
	if err != nil {
		return xaiInspectionResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, accountInspectionBodyLimit+1))
	if err != nil {
		return xaiInspectionResponse{}, err
	}
	if len(body) > accountInspectionBodyLimit {
		body = body[:accountInspectionBodyLimit]
	}
	return xaiInspectionResponse{statusCode: response.StatusCode, header: response.Header.Clone(), body: body}, nil
}

func newAccountInspectionHTTPClient(proxyURL string) (*http.Client, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return nil, fmt.Errorf("auth proxy_url 未配置")
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("auth proxy_url 格式无效")
	}
	transport := &http.Transport{}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
	case "socks5", "socks5h":
		var authentication *proxy.Auth
		if parsed.User != nil {
			username := parsed.User.Username()
			password, _ := parsed.User.Password()
			authentication = &proxy.Auth{User: username, Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", parsed.Host, authentication, &net.Dialer{})
		if err != nil {
			return nil, fmt.Errorf("auth proxy_url 格式无效")
		}
		transport.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
			return dialer.Dial(network, address)
		}
	default:
		return nil, fmt.Errorf("auth proxy_url 协议不受支持")
	}
	return &http.Client{Transport: transport}, nil
}

func classifyXAIAccountResponse(response xaiInspectionResponse) xaiInspectionOutcome {
	if response.statusCode >= http.StatusOK && response.statusCode < http.StatusMultipleChoices {
		return xaiInspectionOutcome{alive: true, statusCode: response.statusCode}
	}
	code, message := extractXAIAccountError(response.body)
	detail := truncateInspectionText(sanitizeLogText(firstNonEmpty(message, string(response.body))), accountInspectionDetailLimit)
	if response.statusCode == http.StatusUnauthorized {
		return xaiInspectionOutcome{statusCode: response.statusCode, errorKind: "account_abnormal", detail: detail}
	}
	if isXAIAccountQuotaExhausted(code, message) {
		return xaiInspectionOutcome{quota: true, statusCode: response.statusCode, errorKind: "quota_exhausted", detail: detail, recoveryAtMS: extractXAIAccountQuotaRecovery(response)}
	}
	switch response.statusCode {
	case http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return xaiInspectionOutcome{statusCode: response.statusCode, errorKind: "account_abnormal", detail: detail}
	default:
		if response.statusCode == http.StatusNotFound {
			return xaiInspectionOutcome{statusCode: response.statusCode, errorKind: "account_abnormal", detail: detail}
		}
		if response.statusCode >= http.StatusInternalServerError {
			return xaiInspectionOutcome{statusCode: response.statusCode, errorKind: "account_abnormal", detail: detail}
		}
		return xaiInspectionOutcome{statusCode: response.statusCode, errorKind: "account_abnormal", detail: detail}
	}
}

func extractXAIAccountError(body []byte) (string, string) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", truncateInspectionText(sanitizeLogText(string(body)), accountInspectionDetailLimit)
	}
	code := xAIInspectionString(payload["code"])
	message := ""
	switch value := payload["error"].(type) {
	case map[string]any:
		if code == "" {
			code = xAIInspectionString(value["code"])
		}
		message = firstNonEmpty(xAIInspectionString(value["message"]), xAIInspectionString(value["error"]))
	case string:
		message = value
	}
	if message == "" {
		message = xAIInspectionString(payload["message"])
	}
	return code, truncateInspectionText(sanitizeLogText(strings.TrimSpace(message)), accountInspectionDetailLimit)
}

func isXAIAccountQuotaExhausted(code, message string) bool {
	combined := strings.ToLower(strings.TrimSpace(code) + " " + strings.TrimSpace(message))
	return strings.Contains(combined, "free-usage-exhausted") ||
		strings.Contains(combined, "used all the included free usage") ||
		strings.Contains(combined, "included free usage has been exhausted")
}

func extractXAIAccountQuotaRecovery(response xaiInspectionResponse) int64 {
	now := time.Now()
	if recovery := parseXAIAccountRetryAfter(response.header.Get("Retry-After"), now); recovery > 0 {
		return recovery
	}
	for _, name := range []string{"X-RateLimit-Reset", "X-Rate-Limit-Reset"} {
		if recovery := parseXAIAccountAbsoluteRecovery(response.header.Get(name)); recovery > 0 {
			return recovery
		}
	}
	var payload any
	if json.Unmarshal(response.body, &payload) == nil {
		return findXAIAccountQuotaRecovery(payload, now)
	}
	return 0
}

func findXAIAccountQuotaRecovery(value any, now time.Time) int64 {
	switch typed := value.(type) {
	case map[string]any:
		for key, field := range typed {
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
			switch normalized {
			case "retryafter", "retryafterseconds":
				if recovery := parseXAIAccountRelativeRecovery(field, now, time.Second); recovery > 0 {
					return recovery
				}
			case "retryafterms", "retryaftermilliseconds":
				if recovery := parseXAIAccountRelativeRecovery(field, now, time.Millisecond); recovery > 0 {
					return recovery
				}
			case "resetat", "resetsat", "recoverytime", "recoverat":
				if recovery := parseXAIAccountAbsoluteRecovery(field); recovery > 0 {
					return recovery
				}
			}
		}
		for _, field := range typed {
			if recovery := findXAIAccountQuotaRecovery(field, now); recovery > 0 {
				return recovery
			}
		}
	case []any:
		for _, field := range typed {
			if recovery := findXAIAccountQuotaRecovery(field, now); recovery > 0 {
				return recovery
			}
		}
	}
	return 0
}

func parseXAIAccountRetryAfter(value string, now time.Time) int64 {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
		return now.Add(time.Duration(seconds * float64(time.Second))).UnixMilli()
	}
	if parsed, err := http.ParseTime(value); err == nil {
		return parsed.UnixMilli()
	}
	return 0
}

func parseXAIAccountRelativeRecovery(value any, now time.Time, unit time.Duration) int64 {
	numeric, ok := xAIInspectionNumber(value)
	if !ok || numeric <= 0 {
		return 0
	}
	return now.Add(time.Duration(numeric * float64(unit))).UnixMilli()
}

func parseXAIAccountAbsoluteRecovery(value any) int64 {
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.UnixMilli()
		}
	}
	numeric, ok := xAIInspectionNumber(value)
	if !ok || numeric <= 0 {
		return 0
	}
	if numeric < 100000000000 {
		return int64(numeric * 1000)
	}
	return int64(numeric)
}

func resolveInspectionQuotaRecovery(upstreamRecovery int64, now time.Time) int64 {
	if upstreamRecovery > now.UnixMilli() {
		return time.UnixMilli(upstreamRecovery).Add(accountInspectionQuotaGrace).UnixMilli()
	}
	return now.Add(accountInspectionQuotaDefault + accountInspectionQuotaGrace).UnixMilli()
}

func parseXAIAccountMonthlyBilling(body []byte, snapshot *xaiBillingSnapshot) error {
	var response struct {
		Config struct {
			MonthlyLimit     json.RawMessage `json:"monthlyLimit"`
			Used             json.RawMessage `json:"used"`
			BillingPeriodEnd string          `json:"billingPeriodEnd"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return err
	}
	limit, hasLimit, err := parseXAIAccountNestedNumber(response.Config.MonthlyLimit)
	if err != nil {
		return fmt.Errorf("monthlyLimit: %w", err)
	}
	used, hasUsed, err := parseXAIAccountNestedNumber(response.Config.Used)
	if err != nil {
		return fmt.Errorf("used: %w", err)
	}
	if hasLimit {
		snapshot.monthlyLimitCents = accountInspectionFloatPointer(limit)
	}
	if hasUsed {
		snapshot.monthlyUsedCents = accountInspectionFloatPointer(used)
	}
	resetAt := parseInspectionBillingTime(response.Config.BillingPeriodEnd)
	if hasLimit && limit > 0 && hasUsed && resetAt > 0 {
		snapshot.quotaWindows = append(snapshot.quotaWindows, accountInspectionQuotaWindow{ID: "monthly", LabelKey: "月限额", UsedPercent: accountInspectionFloatPointer(used / limit * 100), ResetAtMS: resetAt})
	}
	return nil
}

func parseXAIAccountCreditsBilling(body []byte, snapshot *xaiBillingSnapshot) error {
	var response struct {
		Config struct {
			CreditUsagePercent *float64 `json:"creditUsagePercent"`
			BillingPeriodEnd   string   `json:"billingPeriodEnd"`
			CurrentPeriod      struct {
				Type  string `json:"type"`
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"currentPeriod"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return err
	}
	periodType := strings.TrimSpace(response.Config.CurrentPeriod.Type)
	resetAt := parseInspectionBillingTime(firstNonEmpty(response.Config.CurrentPeriod.End, response.Config.BillingPeriodEnd))
	snapshot.recoveryAtMS = resetAt
	if response.Config.CreditUsagePercent == nil || periodType == "" || resetAt <= 0 {
		return nil
	}
	windowID, label := "weekly", "周限额"
	if periodType == "USAGE_PERIOD_TYPE_MONTHLY" {
		windowID, label = "monthly", "月限额"
	}
	upsertXAIAccountQuotaWindow(&snapshot.quotaWindows, accountInspectionQuotaWindow{ID: windowID, LabelKey: label, UsedPercent: response.Config.CreditUsagePercent, ResetAtMS: resetAt})
	return nil
}

func parseXAIAccountNestedNumber(raw json.RawMessage) (float64, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, nil
	}
	var direct float64
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct, true, nil
	}
	var wrapped struct {
		Value *float64 `json:"val"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return 0, false, err
	}
	if wrapped.Value == nil {
		return 0, false, nil
	}
	return *wrapped.Value, true, nil
}

func parseInspectionBillingTime(value string) int64 {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}

func upsertXAIAccountQuotaWindow(windows *[]accountInspectionQuotaWindow, candidate accountInspectionQuotaWindow) {
	for index := range *windows {
		if (*windows)[index].ID == candidate.ID {
			(*windows)[index] = candidate
			return
		}
	}
	*windows = append(*windows, candidate)
}

func mergeXAIAccountBilling(target *xaiBillingSnapshot, source xaiBillingSnapshot) {
	for _, window := range source.quotaWindows {
		upsertXAIAccountQuotaWindow(&target.quotaWindows, window)
	}
	if source.monthlyLimitCents != nil {
		target.monthlyLimitCents = source.monthlyLimitCents
	}
	if source.monthlyUsedCents != nil {
		target.monthlyUsedCents = source.monthlyUsedCents
	}
	if source.recoveryAtMS > 0 {
		target.recoveryAtMS = source.recoveryAtMS
	}
}

func applyXAIAccountBillingSnapshot(result *accountInspectionResult, snapshot xaiBillingSnapshot) {
	result.QuotaWindows = snapshot.quotaWindows
	result.MonthlyLimitCents = snapshot.monthlyLimitCents
	result.MonthlyUsedCents = snapshot.monthlyUsedCents
	result.UsedPercent = nil
	for _, window := range snapshot.quotaWindows {
		if window.ID == "weekly" && window.UsedPercent != nil {
			result.UsedPercent = window.UsedPercent
			return
		}
	}
	for _, window := range snapshot.quotaWindows {
		if window.UsedPercent != nil {
			result.UsedPercent = window.UsedPercent
			return
		}
	}
}

func resolveInspectionAccountType(monthlyLimit *float64) string {
	if monthlyLimit != nil && *monthlyLimit > 0 {
		return "super"
	}
	return "free"
}

func normalizeInspectionAccountType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "super":
		return "super"
	case "free":
		return "free"
	default:
		return "unknown"
	}
}

func accountPriorityPointer(priority int) *int {
	return &priority
}

func accountInspectionIntPointer(value int) *int {
	return &value
}

func accountInspectionFloatPointer(value float64) *float64 {
	return &value
}

func isInspectionManagedPriority(priority *int) bool {
	return priority != nil && (*priority == accountInspectionPriorityQuota || *priority == accountInspectionPriorityAbnormal ||
		*priority == accountInspectionPriorityLegacy || *priority == accountInspectionPriorityUnauthorized ||
		*priority == accountInspectionPrioritySSOExpired)
}

func xAIInspectionNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func xAIInspectionString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func truncateInspectionText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func setAccountInspectionPriorityError(result *accountInspectionResult, err error) {
	result.ErrorKind = "priority_adjustment_failed"
	result.ActionStatus = "failed"
	result.ActionError = truncateInspectionText(sanitizeLogText(err.Error()), accountInspectionDetailLimit)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
