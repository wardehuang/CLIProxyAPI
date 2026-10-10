package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	burstMinReasoningTokens = 80
	burstMaxVisibleTokens   = 32
	burstMaxWindowMS        = 1000
)

type streamEvidence struct {
	SummaryChars           int
	SummaryText            string
	OutputText             string
	EncryptedBytes         int
	ReasoningTokens        int
	OutputTokens           int
	ReasoningItemID        string
	ReasoningItemCompleted bool
	ReasoningMetadataError bool
	CompletedFunctionCalls int
	FunctionCallNames      []string
	FunctionCallToolNames  []string
	CompletedMessage       bool
	RefusalDetected        bool
	ReasoningDelta         bool
	BurstDump              bool
	StreamError            string
	CompletedEvent         bool
	TerminalEvent          bool
}

func prepareXAIStream(_ context.Context, request pluginapi.XAIStreamPrepareRequest) (pluginapi.XAIStreamPrepareResponse, error) {
	if !isXAIProvider(request.Provider) {
		return pluginapi.XAIStreamPrepareResponse{}, nil
	}
	if err := guardianRuntime.ensure(); err != nil {
		return pluginapi.XAIStreamPrepareResponse{}, err
	}
	store := guardianRuntime.currentStore()
	settings, err := store.settings()
	if err != nil {
		return pluginapi.XAIStreamPrepareResponse{}, err
	}
	if strings.TrimSpace(request.AuthIndex) != "" {
		if err := store.observeAuthAttempt(request.AuthIndex, request.AuthFileName, request.ProxyURL); err != nil {
			return pluginapi.XAIStreamPrepareResponse{}, err
		}
	}
	return pluginapi.XAIStreamPrepareResponse{
		FirstPayloadTimeoutSeconds: settings.RealtimeGuardTimeoutSeconds,
		ProgressTimeoutSeconds:     settings.RealtimeGuardIdleTimeoutSeconds,
		MaxRetries:                 0,
		Metadata: map[string]any{
			"prepared_at": time.Now().UnixMilli(),
		},
	}, nil
}

func completeXAIStream(_ context.Context, completion pluginapi.XAIStreamCompletionRequest) (pluginapi.XAIStreamCompletionResponse, error) {
	if !isXAIProvider(completion.Provider) {
		return pluginapi.XAIStreamCompletionResponse{}, nil
	}
	if err := guardianRuntime.ensure(); err != nil {
		return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: "guardian_unavailable", StatusCode: http.StatusBadGateway}, err
	}
	store := guardianRuntime.currentStore()
	settings, err := store.settings()
	if err != nil {
		return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: "settings_unavailable", StatusCode: http.StatusBadGateway}, err
	}
	authIndex := strings.TrimSpace(completion.AuthIndex)
	authName := strings.TrimSpace(completion.AuthFileName)
	if authIndex != "" {
		if err := store.observeAuthAttempt(authIndex, authName, completion.ProxyURL); err != nil {
			return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: "state_write_failed", StatusCode: http.StatusInternalServerError}, err
		}
	}
	if completion.Error != "" || completion.StatusCode >= http.StatusBadRequest || !completion.Completed {
		return streamFailureDecision(store, completion)
	}

	evidence := parseStreamEvidence(completion.Body)
	classification := classifyRealtimeGuardEvidence(evidence, completion, settings)
	reason, degraded := classification.Reason, classification.Degraded
	if classification.Unknown {
		_ = store.appendLog(logLevelWarn, "guard.classification_unknown", "xAI 响应无法完成实时守护分类", classification.Reason)
		return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: classification.Reason, StatusCode: http.StatusBadGateway, Error: "realtime guard classification unavailable"}, nil
	}
	if !degraded {
		degradationCleared := false
		if authIndex != "" {
			degradationCleared, err = applyRealtimeHealthy(store, authIndex)
			if err != nil {
				return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: "state_write_failed", StatusCode: http.StatusInternalServerError}, err
			}
		}
		detail := reason
		if degradationCleared {
			detail += " consecutive_degradation_cleared=true"
		}
		_ = store.appendLog(logLevelInfo, "guard.normal", "xAI 响应通过降智守护", detail)
		return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFlush}, nil
	}
	if authIndex == "" {
		return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: "missing_auth_index", StatusCode: http.StatusBadGateway}, nil
	}
	transition, err := applyRealtimeDegradation(store, authIndex, authName, completion.RequestID, reason)
	if err != nil {
		return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: "state_write_failed", StatusCode: http.StatusInternalServerError}, err
	}
	if strings.TrimSpace(completion.ProxyURL) != "" {
		if err := replaceRealtimeGuardSlot(store, completion.ProxyURL, reason); err != nil {
			_ = store.appendLog(logLevelWarn, "guard.slot_replacement_failed", "实时守护候选槽位替换失败", sanitizeLogText(err.Error()))
		}
	}
	if err := refreshHealthyAuthDistribution(store); err != nil {
		_ = store.appendLog(logLevelError, "auth.distribution_failed", "实时守护完成后刷新 auth 分配失败", sanitizeLogText(err.Error()))
	}
	detail := fmt.Sprintf("count=%d priority=%d cooling_until=%d count_advanced=%t waiting_for_inspection=%t permanent=%t summary_chars=%d encrypted_bytes=%d output_tokens=%d reasoning_tokens=%d",
		transition.State.Count, transition.Priority, transition.State.CoolingUntil, transition.CountAdvanced, transition.WaitingForInspection,
		transition.Permanent, evidence.SummaryChars, evidence.EncryptedBytes, evidence.OutputTokens, evidence.ReasoningTokens)
	_ = store.appendLog(logLevelWarn, "guard.degraded", "xAI 响应命中降智守护", reason+" "+detail)
	return pluginapi.XAIStreamCompletionResponse{
		Action:     pluginapi.XAIStreamActionRetry,
		RetryMode:  pluginapi.XAIStreamRetryModeExcludeSelectedAuthWithSharedAccountLimit,
		Reason:     reason,
		StatusCode: http.StatusBadGateway,
	}, nil
}

type realtimeGuardClassification struct {
	Unknown                   bool
	Reason                    string
	Degraded                  bool
	IsRealThinking            bool
	CompletedToolCallEvidence bool
	CompletedMutationEvidence bool
	BurstDump                 bool
}

func classifyRealtimeGuardEvidence(evidence streamEvidence, completion pluginapi.XAIStreamCompletionRequest, settings pluginSettings) realtimeGuardClassification {
	classification := realtimeGuardClassification{
		Reason:                    "within_threshold",
		CompletedToolCallEvidence: evidence.CompletedFunctionCalls > 0 && !evidence.RefusalDetected,
		BurstDump:                 evidence.BurstDump || isBurstDump(evidence, completion, settings),
	}
	classification.IsRealThinking = classification.BurstDump || isRealThinking(evidence, completion, settings)

	if strings.TrimSpace(completion.Error) != "" {
		classification.Unknown = true
		classification.Reason = "upstream_error"
		return classification
	}
	if completion.StatusCode < http.StatusOK || completion.StatusCode >= http.StatusMultipleChoices {
		classification.Unknown = true
		classification.Reason = fmt.Sprintf("http_%d", completion.StatusCode)
		return classification
	}
	if !completion.Completed {
		classification.Unknown = true
		classification.Reason = "sse_incomplete"
		return classification
	}
	if evidence.StreamError != "" {
		classification.Unknown = true
		classification.Reason = "sse_failed"
		return classification
	}
	if !evidence.CompletedEvent {
		classification.Unknown = true
		classification.Reason = "sse_incomplete"
		return classification
	}

	generationDuration, timingError := realtimeGuardGenerationDuration(completion)
	if timingError != "" {
		classification.Unknown = true
		classification.Reason = timingError
		return classification
	}
	totalTokens := evidence.OutputTokens + evidence.ReasoningTokens
	tps := float64(totalTokens) / generationDuration.Seconds()
	if tps >= settings.QualityHardTPS {
		classification.Degraded = true
		classification.Reason = "hard_tps"
		return classification
	}

	if !classification.IsRealThinking {
		if classification.CompletedToolCallEvidence {
			classification.Reason = "completed_tool_call_evidence"
			return classification
		}
		if evidence.CompletedMessage && !evidence.RefusalDetected {
			mutation := scanCompletedMutationEvidence(completion.OriginalRequest)
			classification.CompletedMutationEvidence = mutation.Found
		}
		if !classification.CompletedMutationEvidence {
			classification.Degraded = true
			classification.Reason = "missing_thinking_without_action"
			return classification
		}
		classification.Reason = "completed_mutation_evidence"
		return classification
	}
	if classification.CompletedToolCallEvidence {
		classification.Reason = "completed_tool_call_evidence"
	}
	return classification
}

func realtimeGuardGenerationDuration(completion pluginapi.XAIStreamCompletionRequest) (time.Duration, string) {
	if completion.UpstreamStartedAt.IsZero() {
		return 0, "upstream_started_at_missing"
	}
	if completion.FirstResponseByteAt.IsZero() {
		return 0, "upstream_first_byte_missing"
	}
	if completion.FirstResponseByteAt.Before(completion.UpstreamStartedAt) {
		return 0, "upstream_ttfb_invalid"
	}
	if completion.FirstPayloadAt.IsZero() {
		return 0, "first_payload_missing"
	}
	if completion.FirstPayloadAt.Before(completion.FirstResponseByteAt) {
		return 0, "first_payload_invalid"
	}
	if completion.FinishedAt.IsZero() {
		return 0, "finished_at_missing"
	}
	if completion.FinishedAt.Before(completion.FirstPayloadAt) {
		return 0, "generation_window_invalid"
	}
	generationDuration := time.Duration(completion.FinishedAt.Sub(completion.FirstPayloadAt).Milliseconds()) * time.Millisecond
	if generationDuration < time.Millisecond {
		generationDuration = time.Millisecond
	}
	return generationDuration, ""
}

type realtimeDegradationTransition struct {
	State                degradationState
	Priority             int
	CountAdvanced        bool
	WaitingForInspection bool
	Permanent            bool
}

func applyRealtimeDegradation(store *guardianStore, authIndex, authName, requestID, reason string) (realtimeDegradationTransition, error) {
	store.realtimeDegradationLock.Lock()
	defer store.realtimeDegradationLock.Unlock()

	entry, err := findXAIAuthEntry(authIndex)
	if err != nil {
		return realtimeDegradationTransition{}, err
	}
	previousAdjustment, hadPreviousAdjustment, err := store.realtimeDegradationPriorityAdjustment(authIndex)
	if err != nil {
		return realtimeDegradationTransition{}, err
	}
	previousState, hadPreviousState, err := store.degradationStateForAuth(authIndex)
	if err != nil {
		return realtimeDegradationTransition{}, err
	}
	currentPriority := entry.Priority
	if currentPriority == accountInspectionPriorityPermanent {
		if err := store.finalizePermanentDegradation(authIndex); err != nil {
			return realtimeDegradationTransition{State: previousState, Priority: currentPriority, Permanent: true}, err
		}
		return realtimeDegradationTransition{State: previousState, Priority: currentPriority, Permanent: true}, nil
	}
	if hadPreviousState && (previousState.Count >= 3 || previousState.CoolingUntil == degradationPermanentCoolingUntil) {
		if err := updateXAIAuthPriority(authIndex, accountInspectionPriorityPermanent); err != nil {
			return realtimeDegradationTransition{State: previousState, Priority: currentPriority, Permanent: true}, err
		}
		if err := store.finalizePermanentDegradation(authIndex); err != nil {
			return realtimeDegradationTransition{State: previousState, Priority: accountInspectionPriorityPermanent, Permanent: true}, err
		}
		return realtimeDegradationTransition{State: previousState, Priority: accountInspectionPriorityPermanent, Permanent: true}, nil
	}
	if currentPriority == accountInspectionPriorityDegraded && (!hadPreviousAdjustment || previousAdjustment.AdjustedPriority != accountInspectionPriorityDegraded || !hadPreviousState) {
		return realtimeDegradationTransition{State: previousState, Priority: currentPriority, WaitingForInspection: true}, fmt.Errorf("priority -8 lacks a matching persisted realtime adjustment")
	}

	now := nowMillis()
	cooldownExpired := hadPreviousState && previousState.CoolingUntil > 0 && previousState.CoolingUntil <= now
	if cooldownExpired && (currentPriority != accountInspectionPriorityHealthy || hadPreviousAdjustment) {
		return realtimeDegradationTransition{State: previousState, Priority: currentPriority, WaitingForInspection: true}, nil
	}
	advanceAfterCooldown := !cooldownExpired || (currentPriority == accountInspectionPriorityHealthy && !hadPreviousAdjustment)
	state, advanced, err := store.recordDegradation(authIndex, authName, requestID, reason, advanceAfterCooldown)
	if err != nil {
		return realtimeDegradationTransition{}, err
	}
	transition := realtimeDegradationTransition{State: state, Priority: currentPriority, CountAdvanced: advanced}
	if cooldownExpired && !advanced {
		transition.WaitingForInspection = false
		return transition, nil
	}
	if state.Count >= 3 {
		state.Count = 3
		state.CoolingUntil = degradationPermanentCoolingUntil
		if err := updateXAIAuthPriority(authIndex, accountInspectionPriorityPermanent); err != nil {
			return realtimeDegradationTransition{}, err
		}
		if err := store.finalizePermanentDegradation(authIndex); err != nil {
			return realtimeDegradationTransition{State: state, Priority: accountInspectionPriorityPermanent, Permanent: true}, err
		}
		return realtimeDegradationTransition{State: state, Priority: accountInspectionPriorityPermanent, Permanent: true}, nil
	}
	if currentPriority == accountInspectionPriorityDegraded && hadPreviousAdjustment && previousAdjustment.AdjustedPriority == accountInspectionPriorityDegraded {
		if previousAdjustment.RecoverAtMS != state.CoolingUntil {
			previousAdjustment.RecoverAtMS = state.CoolingUntil
			if err := store.saveRealtimeDegradationPriorityAdjustment(previousAdjustment); err != nil {
				return realtimeDegradationTransition{}, rollbackRealtimeDegradation(store, authIndex, previousState, hadPreviousState, previousAdjustment, hadPreviousAdjustment, err)
			}
		}
		transition.Priority = accountInspectionPriorityDegraded
		return transition, nil
	}

	originalPriority := &currentPriority
	if hadPreviousAdjustment && (currentPriority == previousAdjustment.AdjustedPriority || previousAdjustment.AdjustedPriority == accountInspectionPriorityDegraded) {
		originalPriority = previousAdjustment.OriginalPriority
	}
	adjustment := accountInspectionPriorityAdjustment{
		AuthIndex:        authIndex,
		FileName:         entry.Name,
		OriginalPriority: originalPriority,
		AdjustedPriority: accountInspectionPriorityDegraded,
		RecoverAtMS:      state.CoolingUntil,
	}
	if err := store.saveRealtimeDegradationPriorityAdjustment(adjustment); err != nil {
		return realtimeDegradationTransition{}, rollbackRealtimeDegradation(store, authIndex, previousState, hadPreviousState, previousAdjustment, hadPreviousAdjustment, err)
	}
	if currentPriority != accountInspectionPriorityDegraded {
		if err := updateXAIAuthPriority(authIndex, accountInspectionPriorityDegraded); err != nil {
			return realtimeDegradationTransition{}, rollbackRealtimeDegradation(store, authIndex, previousState, hadPreviousState, previousAdjustment, hadPreviousAdjustment, err)
		}
	}
	transition.Priority = accountInspectionPriorityDegraded
	return transition, nil
}

func rollbackRealtimeDegradation(store *guardianStore, authIndex string, previousState degradationState, hadPreviousState bool, previousAdjustment accountInspectionPriorityAdjustment, hadPreviousAdjustment bool, cause error) error {
	stateErr := store.restoreDegradationState(authIndex, previousState, hadPreviousState)
	var adjustmentErr error
	if hadPreviousAdjustment {
		adjustmentErr = store.saveRealtimeDegradationPriorityAdjustment(previousAdjustment)
	} else {
		adjustmentErr = store.deleteRealtimeDegradationPriorityAdjustment(authIndex)
	}
	if stateErr != nil || adjustmentErr != nil {
		return fmt.Errorf("%w; rollback degradation state error=%v adjustment error=%v", cause, stateErr, adjustmentErr)
	}
	return cause
}

func applyRealtimeHealthy(store *guardianStore, authIndex string) (bool, error) {
	store.realtimeDegradationLock.Lock()
	defer store.realtimeDegradationLock.Unlock()
	state, exists, err := store.degradationStateForAuth(authIndex)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if state.Count >= 3 || state.CoolingUntil == degradationPermanentCoolingUntil {
		entry, err := findXAIAuthEntry(authIndex)
		if err != nil {
			return false, err
		}
		if entry.Priority != accountInspectionPriorityPermanent {
			if err := updateXAIAuthPriority(authIndex, accountInspectionPriorityPermanent); err != nil {
				return false, err
			}
		}
		if err := store.finalizePermanentDegradation(authIndex); err != nil {
			return false, err
		}
		return false, nil
	}
	if state.CoolingUntil <= 0 || state.CoolingUntil > nowMillis() {
		return false, nil
	}
	entry, err := findXAIAuthEntry(authIndex)
	if err != nil {
		return false, err
	}
	if entry.Priority != accountInspectionPriorityHealthy {
		return false, nil
	}
	return store.clearDegradationIfRecovered(authIndex, entry.Priority)
}

var errRealtimeGuardCandidateUnavailable = errors.New("realtime guard candidate is unavailable")

func replaceRealtimeGuardSlot(store *guardianStore, proxyURL, reason string) error {
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin realtime guard slot replacement: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	var primarySlotID, sourceNodeID int64
	if err := tx.QueryRow(`SELECT healthy_slots.slot_id, healthy_slots.node_id
FROM healthy_slots
INNER JOIN nodes ON nodes.id = healthy_slots.node_id
WHERE healthy_slots.slot_kind = 'primary' AND nodes.scope = 'guard' AND nodes.status = ? AND nodes.address = ?
ORDER BY healthy_slots.slot_id LIMIT 1`, statusHealthy, proxyURL).Scan(&primarySlotID, &sourceNodeID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("realtime guard source slot is unavailable")
		}
		return fmt.Errorf("read realtime guard source slot: %w", err)
	}
	var candidateSlotID, candidateNodeID int64
	if err := tx.QueryRow(`SELECT healthy_slots.slot_id, healthy_slots.node_id
FROM healthy_slots
INNER JOIN nodes ON nodes.id = healthy_slots.node_id
WHERE healthy_slots.slot_kind = 'candidate' AND nodes.scope = 'guard' AND nodes.status = ?
ORDER BY nodes.last_checked DESC, nodes.id DESC LIMIT 1`, statusHealthyCandidate).Scan(&candidateSlotID, &candidateNodeID); err != nil {
		if err != sql.ErrNoRows {
			return fmt.Errorf("read realtime guard candidate slot: %w", err)
		}
		released, err := tx.Exec(`UPDATE nodes SET status = ?, last_error = ? WHERE id = ? AND scope = 'guard' AND status = ?`, statusCooldown, sanitizeLogText(reason), sourceNodeID, statusHealthy)
		if err != nil {
			return fmt.Errorf("release realtime guard source node: %w", err)
		}
		releasedCount, err := released.RowsAffected()
		if err != nil {
			return fmt.Errorf("read realtime guard source release result: %w", err)
		}
		if releasedCount != 1 {
			return fmt.Errorf("realtime guard source node %d was not healthy", sourceNodeID)
		}
		if _, err := tx.Exec(`UPDATE auth_bindings SET slot_id = 0, node_id = 0, proxy_url = '', updated_at = ? WHERE node_id = ?`, now, sourceNodeID); err != nil {
			return fmt.Errorf("clear realtime guard source auth bindings: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM healthy_slots WHERE slot_id = ? AND node_id = ?`, primarySlotID, sourceNodeID); err != nil {
			return fmt.Errorf("clear realtime guard source slot: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit realtime guard source release: %w", err)
		}
		return errRealtimeGuardCandidateUnavailable
	}
	released, err := tx.Exec(`UPDATE nodes SET status = ?, last_error = ? WHERE id = ? AND scope = 'guard' AND status = ?`, statusCooldown, sanitizeLogText(reason), sourceNodeID, statusHealthy)
	if err != nil {
		return fmt.Errorf("release realtime guard source node: %w", err)
	}
	releasedCount, err := released.RowsAffected()
	if err != nil {
		return fmt.Errorf("read realtime guard source release result: %w", err)
	}
	if releasedCount != 1 {
		return fmt.Errorf("realtime guard source node %d was not healthy", sourceNodeID)
	}
	if _, err := tx.Exec(`UPDATE auth_bindings SET slot_id = 0, node_id = 0, proxy_url = '', updated_at = ? WHERE node_id = ?`, now, sourceNodeID); err != nil {
		return fmt.Errorf("clear realtime guard source auth bindings: %w", err)
	}
	clearedCandidate, err := tx.Exec(`DELETE FROM healthy_slots WHERE slot_id = ? AND node_id = ?`, candidateSlotID, candidateNodeID)
	if err != nil {
		return fmt.Errorf("clear realtime guard candidate slot: %w", err)
	}
	clearedCandidateCount, err := clearedCandidate.RowsAffected()
	if err != nil {
		return fmt.Errorf("read realtime guard candidate slot result: %w", err)
	}
	if clearedCandidateCount != 1 {
		return fmt.Errorf("realtime guard candidate slot %d was not available", candidateSlotID)
	}
	promoted, err := tx.Exec(`UPDATE nodes SET status = ?, last_error = '' WHERE id = ? AND scope = 'guard' AND status = ?`, statusHealthy, candidateNodeID, statusHealthyCandidate)
	if err != nil {
		return fmt.Errorf("promote realtime guard candidate: %w", err)
	}
	promotedCount, err := promoted.RowsAffected()
	if err != nil {
		return fmt.Errorf("read realtime guard candidate promotion result: %w", err)
	}
	if promotedCount != 1 {
		return fmt.Errorf("realtime guard candidate node %d was not healthy_candidate", candidateNodeID)
	}
	replaced, err := tx.Exec(`UPDATE healthy_slots SET node_id = ?, refreshed_at = ? WHERE slot_id = ? AND node_id = ? AND slot_kind = 'primary'`, candidateNodeID, now, primarySlotID, sourceNodeID)
	if err != nil {
		return fmt.Errorf("replace realtime guard primary slot: %w", err)
	}
	replacedCount, err := replaced.RowsAffected()
	if err != nil {
		return fmt.Errorf("read realtime guard primary replacement result: %w", err)
	}
	if replacedCount != 1 {
		return fmt.Errorf("realtime guard primary slot %d was not occupied by source node %d", primarySlotID, sourceNodeID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit realtime guard slot replacement: %w", err)
	}
	return nil
}

func isXAIProvider(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "xai")
}

func streamFailureDecision(store *guardianStore, completion pluginapi.XAIStreamCompletionRequest) (pluginapi.XAIStreamCompletionResponse, error) {
	status := completion.StatusCode
	if status == 0 {
		status = http.StatusBadGateway
	}
	reason := classifyStreamFailure(completion)
	guardReason := realtimeGuardFailureReason(completion)
	if status == http.StatusTooManyRequests {
		code, message := extractXAIAccountError(completion.Body)
		if isXAIAccountQuotaExhausted(code, message) {
			recoveryAt, err := applyRealtimeQuotaCooldown(store, completion)
			if err != nil {
				_ = store.appendLog(logLevelError, "guard.quota_cooldown_failed", "xAI quota cooldown update failed; refusing account switch", sanitizeLogText(err.Error()))
				return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: "quota_cooldown_update_failed", StatusCode: http.StatusInternalServerError, Error: sanitizeLogText(err.Error())}, nil
			}
			detail := fmt.Sprintf("auth_index=%q priority=%d recover_at_ms=%d marker=%q", strings.TrimSpace(completion.AuthIndex), accountInspectionPriorityQuota, recoveryAt, firstNonEmpty(code, message))
			_ = store.appendLog(logLevelWarn, "guard.quota_exhausted", "xAI account quota exhausted; cooldown applied before switching", sanitizeLogText(detail))
			return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionRetry, RetryMode: pluginapi.XAIStreamRetryModeExcludeSelectedAuthWithSharedAccountLimit, Reason: "quota_exhausted", StatusCode: status}, nil
		}
		_ = store.appendLog(logLevelError, "guard.rate_limited", "xAI 上游返回非额度耗尽限流，拒绝在守护层换号", reason)
		return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: guardReason, StatusCode: status}, nil
	}
	_ = store.appendLog(logLevelWarn, "guard.stream_failed", "xAI 流请求失败，交由核心重试链处理", reason)
	mode := pluginapi.XAIStreamRetryModeReloadSelectedAuth
	if isGuardTimeout(completion.Error) || status == http.StatusUnauthorized || status == http.StatusForbidden || status >= http.StatusInternalServerError {
		mode = pluginapi.XAIStreamRetryModeReloadAndExcludeSelectedAuth
	}
	return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionRetry, RetryMode: mode, Reason: guardReason, StatusCode: status}, nil
}

func applyRealtimeQuotaCooldown(store *guardianStore, completion pluginapi.XAIStreamCompletionRequest) (int64, error) {
	authIndex := strings.TrimSpace(completion.AuthIndex)
	if authIndex == "" {
		return 0, fmt.Errorf("auth_index is required for quota cooldown")
	}
	store.realtimeDegradationLock.Lock()
	defer store.realtimeDegradationLock.Unlock()
	currentPriority, err := getXAIAuthPriority(authIndex, 0)
	if err != nil {
		return 0, fmt.Errorf("read auth priority for quota cooldown: %w", err)
	}
	now := time.Now()
	response := xaiInspectionResponse{statusCode: completion.StatusCode, header: completion.ResponseHeaders, body: completion.Body}
	recoveryAt := resolveInspectionQuotaRecovery(extractXAIAccountQuotaRecovery(response), now)
	result := accountInspectionResult{AuthIndex: authIndex, FileName: firstNonEmpty(completion.AuthFileName, authIndex)}
	_, _, recoveryAt = lowerAccountInspectionPriority(context.Background(), store, &result, accountInspectionIntPointer(currentPriority), accountInspectionPriorityQuota, recoveryAt)
	if result.ActionStatus == "failed" {
		return recoveryAt, fmt.Errorf("set auth priority to %d: %s", accountInspectionPriorityQuota, result.ActionError)
	}
	return recoveryAt, nil
}

func classifyStreamFailure(completion pluginapi.XAIStreamCompletionRequest) string {
	if strings.TrimSpace(completion.Error) != "" {
		if isGuardTimeout(completion.Error) {
			return "stream_timeout"
		}
		return "upstream_stream_failed"
	}
	if completion.StatusCode >= http.StatusBadRequest {
		return "upstream_http_" + strconv.Itoa(completion.StatusCode)
	}
	return "upstream_stream_incomplete"
}

func realtimeGuardFailureReason(completion pluginapi.XAIStreamCompletionRequest) string {
	if completion.StatusCode == http.StatusTooManyRequests {
		return "upstream_rate_limited"
	}
	return classifyStreamFailure(completion)
}

func isGuardTimeout(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "first payload timeout") || strings.Contains(lower, "progress timeout") || strings.Contains(lower, "idle timeout") || strings.Contains(lower, "context deadline exceeded")
}

func isRealThinking(evidence streamEvidence, completion pluginapi.XAIStreamCompletionRequest, settings pluginSettings) bool {
	if evidence.OutputTokens < settings.RealtimeGuardMinOutputTokens {
		return true
	}
	encryptedFloor := settings.RealtimeGuardMinEncryptedBytes
	calculatedFloor := evidence.ReasoningTokens * settings.RealtimeGuardEncryptedBytesPerReasoningToken
	if calculatedFloor > encryptedFloor {
		encryptedFloor = calculatedFloor
	}
	visibleTokens := evidence.OutputTokens - evidence.ReasoningTokens
	if visibleTokens < 0 {
		visibleTokens = 0
	}
	if isBurstDump(evidence, completion, settings) {
		return true
	}
	hasSummaryEvidence := !evidence.ReasoningMetadataError && evidence.SummaryChars >= settings.RealtimeGuardMinSummaryChars && !isPlaceholderSummary(evidence.SummaryText)
	hasEncryptedEvidence := !evidence.ReasoningMetadataError && evidence.ReasoningItemCompleted && evidence.EncryptedBytes >= encryptedFloor
	return hasSummaryEvidence || hasEncryptedEvidence
}

func isBurstDump(evidence streamEvidence, completion pluginapi.XAIStreamCompletionRequest, settings pluginSettings) bool {
	visibleTokens := evidence.OutputTokens - evidence.ReasoningTokens
	if visibleTokens <= 0 {
		return false
	}
	visibleFlushMS := int64(-1)
	if !completion.FirstVisibleAt.IsZero() && !completion.FinishedAt.IsZero() {
		visibleFlushMS = completion.FinishedAt.Sub(completion.FirstVisibleAt).Milliseconds()
	}
	return evidence.ReasoningTokens >= settings.RealtimeGuardBurstMinReasoningTokens && visibleTokens < settings.RealtimeGuardBurstMaxVisibleTokens && visibleFlushMS >= 0 && visibleFlushMS < int64(settings.RealtimeGuardBurstMaxWindowMS)
}

func parseStreamEvidence(body []byte) streamEvidence {
	evidence := streamEvidence{}
	if len(body) == 0 {
		return evidence
	}
	if !bytes.Contains(body, []byte("data:")) {
		evidence.StreamError = "sse_failed"
		return evidence
	}
	for _, event := range splitSSEEvents(body) {
		payload := bytes.TrimSpace(event.payload)
		if bytes.Equal(payload, []byte("[DONE]")) {
			evidence.CompletedEvent = true
			evidence.TerminalEvent = true
			continue
		}
		var object map[string]any
		if err := json.Unmarshal(payload, &object); err != nil {
			evidence.StreamError = "sse_failed"
			return evidence
		}
		if object == nil {
			evidence.StreamError = "sse_failed"
			return evidence
		}
		applyStreamEvent(&evidence, valueType(object), object)
		if evidence.StreamError != "" {
			return evidence
		}
	}
	return evidence
}

type sseEvent struct {
	payload []byte
}

func splitSSEEvents(body []byte) []sseEvent {
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	result := make([]sseEvent, 0)
	for _, block := range bytes.Split(normalized, []byte("\n\n")) {
		for _, rawLine := range bytes.Split(block, []byte("\n")) {
			line := bytes.TrimSpace(rawLine)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[len("data:"):])
			result = append(result, sseEvent{payload: append([]byte(nil), payload...)})
		}
	}
	return result
}

func decodePayload(payload []byte) any {
	var value any
	if json.Unmarshal(bytes.TrimSpace(payload), &value) != nil {
		return nil
	}
	return value
}

func valueType(value any) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return stringValue(object["type"])
}

func applyStreamEvent(evidence *streamEvidence, eventName string, value any) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	eventName = strings.ToLower(strings.TrimSpace(eventName))
	if strings.Contains(eventName, "failed") || strings.Contains(eventName, "incomplete") || eventName == "error" || strings.Contains(eventName, "error") {
		evidence.StreamError = eventName
		if eventName == "response.failed" || eventName == "response.incomplete" {
			evidence.TerminalEvent = true
		}
		return
	}
	if strings.Contains(stringifyLower(value), "burst_dump") {
		evidence.BurstDump = true
	}
	if eventName == "response.completed" {
		evidence.CompletedEvent = true
		evidence.TerminalEvent = true
	}
	if eventName == "response.refusal.delta" {
		evidence.RefusalDetected = true
	}
	switch eventName {
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		recordReasoningItemID(evidence, stringValue(object["item_id"]))
		if eventTextLength(object) > 0 {
			evidence.ReasoningDelta = true
		}
		appendSummaryDelta(eventTextValue(object), evidence)
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		recordReasoningItemID(evidence, stringValue(object["item_id"]))
		recordSummary(object["text"], evidence)
	case "response.reasoning_summary_part.done":
		recordReasoningItemID(evidence, stringValue(object["item_id"]))
		if part := nestedObject(object, "part"); part != nil && strings.EqualFold(stringValue(part["type"]), "summary_text") {
			recordSummary(part["text"], evidence)
		}
	case "response.output_item.added", "response.output_item.done":
		item := nestedObject(object, "item")
		if item == nil {
			item = nestedObject(object, "output_item")
		}
		if item == nil {
			item = object
		}
		recordOutputItem(evidence, item, eventName == "response.output_item.done")
	case "response.output_text.delta":
		evidence.OutputText += stringValue(object["delta"])
	case "response.output_text.done":
		if text := stringValue(object["text"]); text != "" {
			evidence.OutputText = text
		}
	case "response.completed":
		response := nestedObject(object, "response")
		if response == nil {
			response = object
		}
		usage := object["usage"]
		if usage == nil {
			usage = response["usage"]
		}
		readUsage(evidence, usage)
		if output, ok := response["output"].([]any); ok {
			for _, rawItem := range output {
				if item, itemOK := rawItem.(map[string]any); itemOK {
					recordOutputItem(evidence, item, true)
				}
			}
		}
	}
}

func recordOutputItem(evidence *streamEvidence, item map[string]any, terminal bool) {
	typeName := strings.ToLower(stringValue(item["type"]))
	status := strings.ToLower(stringValue(item["status"]))
	if typeName == "reasoning" {
		recordReasoningItemID(evidence, stringValue(item["id"]))
		if terminal || status == "completed" {
			evidence.ReasoningItemCompleted = true
		}
		if encrypted := strings.TrimSpace(stringValue(item["encrypted_content"])); len(encrypted) > evidence.EncryptedBytes {
			evidence.EncryptedBytes = len([]byte(encrypted))
		}
		if summaries, ok := item["summary"].([]any); ok {
			for _, rawSummary := range summaries {
				summary, summaryOK := rawSummary.(map[string]any)
				if !summaryOK || !strings.EqualFold(stringValue(summary["type"]), "summary_text") {
					continue
				}
				recordSummary(stringValue(summary["text"]), evidence)
			}
		}
	}
	if typeName == "message" && terminal && status == "completed" && strings.TrimSpace(stringValue(item["id"])) != "" {
		evidence.CompletedMessage = true
	}
	if typeName == "function_call" && terminal && status == "completed" {
		recordFunctionCall(evidence, item)
	}
	if typeName == "message" {
		if content, ok := item["content"].([]any); ok {
			var outputText strings.Builder
			for _, rawContent := range content {
				contentItem, ok := rawContent.(map[string]any)
				if !ok {
					continue
				}
				contentType := strings.ToLower(stringValue(contentItem["type"]))
				if contentType == "refusal" {
					evidence.RefusalDetected = true
				}
				if status == "completed" && contentType == "output_text" {
					outputText.WriteString(stringValue(contentItem["text"]))
				}
			}
			if outputText.Len() > 0 {
				evidence.OutputText = outputText.String()
			}
		}
	}
}

func recordReasoningItemID(evidence *streamEvidence, itemID string) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		evidence.ReasoningMetadataError = true
		return
	}
	if evidence.ReasoningItemID == "" {
		evidence.ReasoningItemID = itemID
		return
	}
	if evidence.ReasoningItemID != itemID {
		evidence.ReasoningMetadataError = true
	}
}

func recordSummary(value any, evidence *streamEvidence) {
	text := strings.TrimSpace(summaryText(value))
	if text == "" {
		return
	}
	chars := len([]rune(text))
	if chars > evidence.SummaryChars {
		evidence.SummaryChars = chars
		evidence.SummaryText = text
	}
}

func appendSummaryDelta(value any, evidence *streamEvidence) {
	text := summaryText(value)
	if text == "" {
		return
	}
	evidence.SummaryText += text
	evidence.SummaryChars = len([]rune(evidence.SummaryText))
}

func summaryText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := summaryText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	case map[string]any:
		for _, key := range []string{"text", "delta", "summary_text", "value"} {
			if child := typed[key]; child != nil {
				return summaryText(child)
			}
		}
	}
	return ""
}

func eventTextValue(object map[string]any) any {
	if value := object["delta"]; value != nil {
		return value
	}
	if value := object["text"]; value != nil {
		return value
	}
	if value := object["summary_text"]; value != nil {
		return value
	}
	if value := object["part"]; value != nil {
		return value
	}
	return object["summary"]
}

func isPlaceholderSummary(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "thinking", "thinking...", "thinking…":
		return true
	default:
		return false
	}
}

func recordFunctionCall(evidence *streamEvidence, object map[string]any) {
	callID := strings.TrimSpace(stringValue(object["call_id"]))
	name := strings.TrimSpace(stringValue(object["name"]))
	arguments := strings.TrimSpace(stringValue(object["arguments"]))
	if callID == "" || name == "" || arguments == "" {
		return
	}
	for _, existing := range evidence.FunctionCallNames {
		if existing == callID {
			return
		}
	}
	evidence.CompletedFunctionCalls++
	evidence.FunctionCallNames = append(evidence.FunctionCallNames, callID)
	evidence.FunctionCallToolNames = append(evidence.FunctionCallToolNames, name)
}

func readUsage(evidence *streamEvidence, value any) {
	usage, ok := value.(map[string]any)
	if !ok {
		return
	}
	evidence.OutputTokens = maxInt(evidence.OutputTokens, integerValue(usage["output_tokens"]))
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		evidence.ReasoningTokens = maxInt(evidence.ReasoningTokens, integerValue(details["reasoning_tokens"]))
	}
}

func nestedObject(object map[string]any, key string) map[string]any {
	value, _ := object[key].(map[string]any)
	return value
}

func eventTextLength(object map[string]any) int {
	if value := object["delta"]; value != nil {
		return textLength(value)
	}
	if value := object["text"]; value != nil {
		return textLength(value)
	}
	return textLength(object["summary"])
}

func textLength(value any) int {
	switch typed := value.(type) {
	case string:
		return len([]rune(typed))
	case []any:
		total := 0
		for _, item := range typed {
			total += textLength(item)
		}
		return total
	case map[string]any:
		if text := typed["text"]; text != nil {
			return textLength(text)
		}
		if value := typed["value"]; value != nil {
			return textLength(value)
		}
	}
	return 0
}

func stringifyLower(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return strings.ToLower(string(raw))
}

func integerValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case json.Number:
		value, _ := typed.Int64()
		return int(value)
	case int:
		return typed
	default:
		return 0
	}
}

func maxInt(left, right int) int {
	if right > left {
		return right
	}
	return left
}
