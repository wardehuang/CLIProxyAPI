package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const probeKindRealtimeGuard = "realtime_guard"

type realtimeGuardReplacement struct {
	HealthySlot      slotRecord
	OriginalNode     proxyNode
	ReplacementNode  proxyNode
	CandidateSlot    slotRecord
	FromFallback     bool
	HasReplacement   bool
	FallbackReserved bool
}

func (store *ipStore) ensureRealtimeGuardMetadata() error {
	if _, err := store.database.Exec(`CREATE INDEX IF NOT EXISTS idx_plugin_logs_realtime_guard ON plugin_logs(category, created_at DESC, id DESC)`); err != nil {
		return fmt.Errorf("create realtime guard log index: %w", err)
	}
	return nil
}

func handleStreamCompletionIntercept(completion pluginapi.StreamCompletionInterceptRequest) (pluginapi.StreamCompletionInterceptResponse, error) {
	if !strings.EqualFold(strings.TrimSpace(completion.Provider), "xai") {
		return pluginapi.StreamCompletionInterceptResponse{Action: pluginapi.StreamCompletionActionFlush}, nil
	}
	if err := pluginRuntime.ensure(); err != nil {
		return pluginapi.StreamCompletionInterceptResponse{
			Action:     pluginapi.StreamCompletionActionFail,
			Reason:     "realtime_guard_unavailable",
			StatusCode: http.StatusBadGateway,
			Error:      err.Error(),
		}, nil
	}

	probe := realtimeGuardProbeFromCompletion(completion)
	snapshot := realtimeGuardSnapshotFromMetadata(completion.Metadata)
	if snapshot.SlotID > 0 && snapshot.NodeID > 0 && snapshot.ProxyURL != "" {
		probe.SourceSnapshot = snapshot
		probe.ProxyURL = snapshot.ProxyURL
	}
	decision := classifyRealtimeGuardProbe(probe)
	if decision.Classification == realtimeGuardClassificationNormal {
		_, _ = pluginRuntime.withStore(func(store *ipStore) ([]byte, error) {
			return nil, store.clearRealtimeDegradationFailure(probe.ProxyURL)
		})
		return pluginapi.StreamCompletionInterceptResponse{
			Action:     pluginapi.StreamCompletionAction(decision.Action),
			Reason:     decision.Reason,
			StatusCode: http.StatusBadGateway,
			Error:      decision.Error,
		}, nil
	}
	if decision.Classification == realtimeGuardClassificationUnknown {
		return pluginapi.StreamCompletionInterceptResponse{
			Action:     pluginapi.StreamCompletionActionFail,
			Reason:     decision.Reason,
			StatusCode: http.StatusBadGateway,
			Error:      decision.Error,
		}, nil
	}

	pluginRuntime.realtimeGuardMutex.Lock()
	defer pluginRuntime.realtimeGuardMutex.Unlock()
	managerBaseURL, managerManagementKey := pluginRuntime.managerAPISettings()
	pluginRuntime.topologyMutex.Lock()
	defer pluginRuntime.topologyMutex.Unlock()

	_, err := pluginRuntime.withStore(func(store *ipStore) ([]byte, error) {
		if decision.Classification == realtimeGuardClassificationDegradation {
			if logErr := store.logRealtimeDegradationDetected(probe, decision); logErr != nil {
				return nil, logErr
			}
			failure, failureErr := store.recordRealtimeDegradationFailure(probe, decision)
			if failureErr != nil {
				return nil, failureErr
			}
			auth, originalPriority, authErr := markRealtimeGuardAuthDegraded(probe)
			if authErr != nil {
				return nil, authErr
			}
			originalPriority, originalPriorityErr := store.rememberRealtimeDegradedAuth(auth)
			if originalPriorityErr != nil {
				return nil, originalPriorityErr
			}
			if managerErr := syncManagerRealtimeDegradation(managerBaseURL, managerManagementKey, auth, originalPriority, probe, decision); managerErr != nil {
				store.logRealtimeManagerAPIUnavailable(probe, decision, failure, managerErr)
				return nil, managerErr
			}
			if failure.ConsecutiveFailureCount < realtimeDegradationReplacementThreshold {
				if retryErr := ensureRealtimeGuardReplacementAuth(probe.AuthIndex); retryErr != nil {
					return nil, retryErr
				}
				store.logRealtimeDegradationRecorded(probe, decision, failure)
				return nil, nil
			}
		}
		if applyErr := store.applyRealtimeGuard(context.Background(), probe, &decision); applyErr != nil {
			return nil, applyErr
		}
		if decision.Action == realtimeGuardActionRetry {
			if retryErr := ensureRealtimeGuardReplacementAuth(probe.AuthIndex); retryErr != nil {
				return nil, retryErr
			}
		}
		return nil, nil
	})
	if err != nil {
		return pluginapi.StreamCompletionInterceptResponse{
			Action:     pluginapi.StreamCompletionActionFail,
			Reason:     "realtime_guard_failed",
			StatusCode: http.StatusBadGateway,
			Error:      err.Error(),
		}, nil
	}
	return pluginapi.StreamCompletionInterceptResponse{
		Action:     pluginapi.StreamCompletionAction(decision.Action),
		Reason:     decision.Reason,
		StatusCode: http.StatusBadGateway,
		Error:      decision.Error,
	}, nil
}

func realtimeGuardProbeFromCompletion(completion pluginapi.StreamCompletionInterceptRequest) realtimeGuardProbe {
	return realtimeGuardProbe{
		RequestID:       completion.RequestID,
		Provider:        completion.Provider,
		SourceFormat:    completion.SourceFormat,
		Model:           completion.Model,
		RequestedModel:  completion.RequestedModel,
		AuthID:          completion.AuthID,
		AuthIndex:       completion.AuthIndex,
		AuthFileName:    completion.AuthFileName,
		ProxyURL:        completion.ProxyURL,
		RequestHeaders:  completion.RequestHeaders,
		ResponseHeaders: completion.ResponseHeaders,
		Body:            bytes.Clone(completion.Body),
		StatusCode:      completion.StatusCode,
		Error:           completion.Error,
		Completed:       completion.Completed,
		StartedAt:       completion.StartedAt,
		FirstPayloadAt:  completion.FirstPayloadAt,
		FinishedAt:      completion.FinishedAt,
		RetryCount:      completion.RetryCount,
		MaxRetries:      completion.MaxRetries,
		Metadata:        completion.Metadata,
	}
}

func classifyRealtimeGuardProbe(probe realtimeGuardProbe) realtimeGuardDecision {
	settings, err := pluginRuntime.currentSettings()
	if err != nil {
		return realtimeGuardDecision{
			Action:         realtimeGuardActionFail,
			Reason:         "settings_unavailable",
			Classification: realtimeGuardClassificationUnknown,
			QualityLevel:   realtimeGuardQualityUnknown,
			Error:          err.Error(),
		}
	}
	return classifyRealtimeGuardProbeWithSettings(probe, settings)
}

func classifyRealtimeGuardProbeWithSettings(probe realtimeGuardProbe, settings pluginSettings) realtimeGuardDecision {
	decision := realtimeGuardDecision{
		Action:         realtimeGuardActionFlush,
		Classification: realtimeGuardClassificationNormal,
		QualityLevel:   realtimeGuardQualityHealthy,
		Reason:         "within_threshold",
	}
	if strings.TrimSpace(probe.Error) != "" {
		return realtimeGuardUnknownDecision("upstream_error", probe.Error)
	}
	if probe.StatusCode < http.StatusOK || probe.StatusCode >= http.StatusMultipleChoices {
		return realtimeGuardUnknownDecision(fmt.Sprintf("http_%d", probe.StatusCode), "上游 HTTP 响应异常")
	}
	if !probe.Completed {
		return realtimeGuardUnknownDecision("sse_incomplete", "SSE 流未完整结束")
	}

	outputTokens, thinkingDelta, sseErr := parseRealtimeGuardSSE(probe.Body)
	if sseErr != nil {
		return realtimeGuardUnknownDecision("sse_failed", sseErr.Error())
	}
	generationStartedAt := probe.FirstPayloadAt
	if generationStartedAt.IsZero() {
		generationStartedAt = probe.StartedAt
	}
	generationFinishedAt := probe.FinishedAt
	if generationFinishedAt.IsZero() {
		generationFinishedAt = time.Now()
	}
	generationDuration := generationFinishedAt.Sub(generationStartedAt)
	if generationDuration < time.Millisecond {
		generationDuration = time.Millisecond
	}
	decision.TPS = float64(outputTokens) / generationDuration.Seconds()
	if decision.TPS >= settings.QualityHardTPS {
		decision.Action = realtimeGuardActionRetry
		decision.Classification = realtimeGuardClassificationDegradation
		decision.QualityLevel = realtimeGuardQualityHard
		decision.Reason = "hard_tps"
		return decision
	}
	if decision.TPS >= settings.QualitySoftTPS && !thinkingDelta {
		decision.Action = realtimeGuardActionRetry
		decision.Classification = realtimeGuardClassificationDegradation
		decision.QualityLevel = realtimeGuardQualitySoft
		decision.Reason = "soft_tps_missing_thinking_delta"
	}
	return decision
}

func realtimeGuardUnknownDecision(reason, detail string) realtimeGuardDecision {
	return realtimeGuardDecision{
		Action:         realtimeGuardActionFail,
		Reason:         reason,
		Classification: realtimeGuardClassificationUnknown,
		QualityLevel:   realtimeGuardQualityUnknown,
		Error:          detail,
	}
}

func parseRealtimeGuardSSE(body []byte) (int64, bool, error) {
	if len(body) == 0 {
		return 0, false, fmt.Errorf("SSE 响应为空")
	}
	var outputTokens int64
	thinkingDelta := false
	completed := false
	for _, event := range strings.Split(string(body), "\n\n") {
		for _, line := range strings.Split(event, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				completed = true
				continue
			}
			var message map[string]any
			if err := json.Unmarshal([]byte(payload), &message); err != nil {
				return 0, false, fmt.Errorf("解析 SSE 事件失败: %w", err)
			}
			eventType := strings.ToLower(stringField(message, "type"))
			if strings.Contains(eventType, "failed") || strings.Contains(eventType, "incomplete") || eventType == "error" {
				return 0, false, fmt.Errorf("SSE 事件异常: %s", eventType)
			}
			if eventType == "response.reasoning_summary_text.delta" ||
				eventType == "response.reasoning_text.delta" ||
				strings.EqualFold(stringField(message, "delta_type"), "thinking_delta") ||
				qualityDeltaTypeIsThinking(message) {
				thinkingDelta = true
			}
			if eventType != "response.completed" {
				continue
			}
			completed = true
			usage, ok := message["usage"].(map[string]any)
			if !ok {
				response, responseOK := message["response"].(map[string]any)
				if responseOK {
					usage, _ = response["usage"].(map[string]any)
				}
			}
			if usage != nil {
				outputTokens = int64(integerField(usage, "output_tokens"))
			}
		}
	}
	if !completed {
		return 0, false, fmt.Errorf("SSE 流未收到 response.completed 或 [DONE]")
	}
	return outputTokens, thinkingDelta, nil
}

func (store *ipStore) applyRealtimeGuard(ctx context.Context, probe realtimeGuardProbe, decision *realtimeGuardDecision) error {
	settings, err := store.settings()
	if err != nil {
		return err
	}
	replacement, err := store.startRealtimeGuardReplacement(probe, *decision)
	if err != nil {
		return err
	}
	decision.OriginalNodeID = replacement.OriginalNode.ID
	decision.OriginalProxyURL = replacement.OriginalNode.ProxyURL

	if replacement.HasReplacement && !replacement.FromFallback {
		decision.ReplacementNodeID = replacement.ReplacementNode.ID
		decision.ReplacementProxyURL = replacement.ReplacementNode.ProxyURL
		if err := refreshHealthyAuthDistribution(store, settings.HealthySlotCount); err != nil {
			return fmt.Errorf("实时守护同步候补 auth 文件: %w", err)
		}
		decision.Action = realtimeGuardActionRetry
		store.logRealtimeGuard(probe, *decision, replacement.OriginalNode, replacement.ReplacementNode)
		return nil
	}

	if err := refreshHealthyAuthDistribution(store, settings.HealthySlotCount); err != nil {
		return fmt.Errorf("实时守护清空健康槽位 auth 文件: %w", err)
	}
	if !replacement.FallbackReserved {
		decision.Action = realtimeGuardActionFail
		decision.Error = "实时守护没有可用健康备选或健康保底节点"
		store.logRealtimeGuard(probe, *decision, replacement.OriginalNode, proxyNode{})
		return nil
	}

	for replacement.FallbackReserved {
		connectivityResult := probeNodeWithRetries(ctx, replacement.ReplacementNode, 1)
		if !connectivityResult.Success {
			if err := store.finishRealtimeFallbackFailure(replacement.ReplacementNode, connectivityResult.Reason, connectivityResult.Detail); err != nil {
				return err
			}
			store.logRealtimeFallbackCheck(probe, replacement.ReplacementNode, "connectivity_failed", connectivityResult.Reason+"；"+connectivityResult.Detail)
			replacement, err = store.reserveNextRealtimeFallback(replacement.HealthySlot)
			if err != nil {
				return err
			}
			continue
		}

		qualityResult, qualityErr := store.probeRealtimeFallbackQuality(ctx, replacement.ReplacementNode, replacement.HealthySlot.ID)
		if qualityErr != nil || qualityResult.Classification != qualityClassificationNormal {
			detail := ""
			if qualityErr != nil {
				detail = qualityErr.Error()
			} else {
				detail = qualityResult.DisplayReason()
			}
			if err := store.finishRealtimeFallbackFailure(replacement.ReplacementNode, "quality_failed", detail); err != nil {
				return err
			}
			store.logRealtimeFallbackCheck(probe, replacement.ReplacementNode, "quality_failed", detail)
			replacement, err = store.reserveNextRealtimeFallback(replacement.HealthySlot)
			if err != nil {
				return err
			}
			continue
		}

		if err := store.finishRealtimeFallbackSuccess(replacement.HealthySlot, replacement.ReplacementNode, connectivityResult); err != nil {
			return err
		}
		if err := refreshHealthyAuthDistribution(store, settings.HealthySlotCount); err != nil {
			return fmt.Errorf("实时守护同步保底替换 auth 文件: %w", err)
		}
		decision.Action = realtimeGuardActionRetry
		decision.ReplacementNodeID = replacement.ReplacementNode.ID
		decision.ReplacementProxyURL = replacement.ReplacementNode.ProxyURL
		store.logRealtimeGuard(probe, *decision, replacement.OriginalNode, replacement.ReplacementNode)
		return nil
	}
	return fmt.Errorf("实时守护状态异常")
}

func (store *ipStore) startRealtimeGuardReplacement(probe realtimeGuardProbe, decision realtimeGuardDecision) (realtimeGuardReplacement, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return realtimeGuardReplacement{}, fmt.Errorf("开始实时守护替换事务: %w", err)
	}
	defer transaction.Rollback()

	var replacement realtimeGuardReplacement
	if probe.SourceSnapshot.SlotID > 0 && probe.SourceSnapshot.NodeID > 0 {
		if err := scanRealtimeHealthySlot(transaction.QueryRow(`
SELECT slot_id, slot_kind, node_id, claim_node_id, fallback_origin, fallback_entered_round_id,
       claim_token, claim_stage, claim_started_at, last_processed_round_id, blocked_round_id
FROM ip_slots
WHERE slot_id = ? AND node_id = ? AND slot_kind = ?`, probe.SourceSnapshot.SlotID, probe.SourceSnapshot.NodeID, statusHealthy), &replacement.HealthySlot); err != nil {
			if err == sql.ErrNoRows {
				return realtimeGuardReplacement{}, fmt.Errorf("实时守护源槽位快照已失效")
			}
			return realtimeGuardReplacement{}, err
		}
	} else if err := scanRealtimeHealthySlot(transaction.QueryRow(`
SELECT slots.slot_id, slots.slot_kind, slots.node_id, slots.claim_node_id, slots.fallback_origin, slots.fallback_entered_round_id,
       slots.claim_token, slots.claim_stage, slots.claim_started_at, slots.last_processed_round_id, slots.blocked_round_id
FROM ip_slots AS slots
JOIN ip_nodes AS nodes ON nodes.id = slots.node_id
WHERE slots.slot_kind = ? AND nodes.proxy_url = ?
ORDER BY slots.slot_id ASC
LIMIT 1`, statusHealthy, strings.TrimSpace(probe.ProxyURL)), &replacement.HealthySlot); err != nil {
		if err == sql.ErrNoRows {
			return realtimeGuardReplacement{}, fmt.Errorf("实时守护未找到 proxy_url 对应的健康槽位")
		}
		return realtimeGuardReplacement{}, err
	}
	if err := store.validateRealtimeGuardSnapshot(transaction, probe.SourceSnapshot, replacement.HealthySlot); err != nil {
		return realtimeGuardReplacement{}, err
	}
	if err := scanProxyNode(transaction.QueryRow(`
SELECT id, node_name, proxy_url, host, input_ip, port, protocol, domain, batch_id, status,
       initial_connected, probe_kind, probe_return_status, keepalive_round_id, revive_round_id,
       revive_failure_count, latency_ms, entered_at, probe_started_at, probe_time, exit_ip,
       exit_country, error_reason, error_detail, revive_target_status
FROM ip_nodes WHERE id = ?`, replacement.HealthySlot.NodeID), &replacement.OriginalNode); err != nil {
		return realtimeGuardReplacement{}, err
	}
	if replacement.OriginalNode.Status != statusHealthy {
		return realtimeGuardReplacement{}, fmt.Errorf("实时守护节点 %d 当前不是健康状态", replacement.OriginalNode.ID)
	}

	if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = 0, probe_kind = '', probe_return_status = '',
    error_reason = ?, error_detail = ?, revive_target_status = ?
WHERE id = ? AND status = ?`, statusCooldown, decision.Reason, decision.Error, statusCooldown, replacement.OriginalNode.ID, statusHealthy); err != nil {
		return realtimeGuardReplacement{}, fmt.Errorf("标记实时守护异常节点: %w", err)
	}
	if _, err := transaction.Exec(`
UPDATE ip_slots
SET node_id = 0, claim_node_id = 0, fallback_origin = 0, fallback_entered_round_id = 0,
    claim_token = '', claim_stage = '', claim_started_at = 0
WHERE slot_id = ? AND node_id = ?`, replacement.HealthySlot.ID, replacement.OriginalNode.ID); err != nil {
		return realtimeGuardReplacement{}, fmt.Errorf("清空实时守护健康槽位: %w", err)
	}

	candidate, candidateSlot, candidateFound, err := findRealtimeCandidate(transaction)
	if err != nil {
		return realtimeGuardReplacement{}, err
	}
	if candidateFound {
		if _, err := transaction.Exec(`UPDATE ip_slots SET node_id = 0 WHERE slot_id = ? AND node_id = ?`, candidateSlot.ID, candidate.ID); err != nil {
			return realtimeGuardReplacement{}, fmt.Errorf("清空实时守护健康备选槽位: %w", err)
		}
		if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, error_reason = '', error_detail = '', revive_target_status = ?
WHERE id = ? AND status = ?`, statusHealthy, statusHealthy, candidate.ID, statusHealthyCandidate); err != nil {
			return realtimeGuardReplacement{}, fmt.Errorf("提升实时守护健康备选节点: %w", err)
		}
		if _, err := transaction.Exec(`UPDATE ip_slots SET node_id = ? WHERE slot_id = ? AND node_id = 0`, candidate.ID, replacement.HealthySlot.ID); err != nil {
			return realtimeGuardReplacement{}, fmt.Errorf("写入实时守护健康替换槽位: %w", err)
		}
		replacement.ReplacementNode = candidate
		replacement.ReplacementNode.Status = statusHealthy
		replacement.CandidateSlot = candidateSlot
		replacement.HasReplacement = true
	} else {
		fallback, found, fallbackErr := reserveRealtimeFallback(transaction)
		if fallbackErr != nil {
			return realtimeGuardReplacement{}, fallbackErr
		}
		if found {
			replacement.ReplacementNode = fallback
			replacement.FallbackReserved = true
			replacement.FromFallback = true
		}
	}
	if err := transaction.Commit(); err != nil {
		return realtimeGuardReplacement{}, fmt.Errorf("提交实时守护替换事务: %w", err)
	}
	return replacement, nil
}

func (store *ipStore) validateRealtimeGuardSnapshot(transaction *sql.Tx, snapshot realtimeGuardSourceSnapshot, slot slotRecord) error {
	if snapshot.SlotID == 0 || snapshot.NodeID == 0 {
		return nil
	}
	if snapshot.SlotID != slot.ID || snapshot.NodeID != slot.NodeID {
		return fmt.Errorf("实时守护源槽位快照不匹配")
	}
	var bindingUpdatedAt int64
	err := transaction.QueryRow(`
SELECT updated_at
FROM ip_slot_auth_bindings
WHERE auth_identity = ? AND slot_id = ? AND node_id = ? AND proxy_url = ?`, snapshot.AuthIdentity, snapshot.SlotID, snapshot.NodeID, snapshot.ProxyURL).Scan(&bindingUpdatedAt)
	if err != nil {
		return fmt.Errorf("验证实时守护源 auth 绑定: %w", err)
	}
	if bindingUpdatedAt != snapshot.BindingUpdatedAt {
		return fmt.Errorf("实时守护源 auth 绑定已变更")
	}
	return nil
}

func scanRealtimeHealthySlot(row *sql.Row, slot *slotRecord) error {
	return row.Scan(
		&slot.ID, &slot.Kind, &slot.NodeID, &slot.ClaimNodeID, &slot.FallbackOrigin, &slot.FallbackEnteredRoundID,
		&slot.ClaimToken, &slot.ClaimStage, &slot.ClaimStartedAt, &slot.LastProcessedRoundID, &slot.BlockedRoundID,
	)
}

func findRealtimeCandidate(transaction *sql.Tx) (proxyNode, slotRecord, bool, error) {
	var candidate proxyNode
	var slot slotRecord
	err := transaction.QueryRow(`
SELECT slots.slot_id, slots.slot_kind, slots.node_id, slots.claim_node_id, slots.fallback_origin, slots.fallback_entered_round_id,
       slots.claim_token, slots.claim_stage, slots.claim_started_at, slots.last_processed_round_id, slots.blocked_round_id,
       nodes.id, nodes.node_name, nodes.proxy_url, nodes.host, nodes.input_ip, nodes.port, nodes.protocol, nodes.domain,
       nodes.batch_id, nodes.status, nodes.initial_connected, nodes.probe_kind, nodes.probe_return_status,
       nodes.keepalive_round_id, nodes.revive_round_id, nodes.revive_failure_count, nodes.latency_ms, nodes.entered_at,
       nodes.probe_started_at, nodes.probe_time, nodes.exit_ip, nodes.exit_country, nodes.error_reason, nodes.error_detail,
       nodes.revive_target_status
FROM ip_slots AS slots
JOIN ip_nodes AS nodes ON nodes.id = slots.node_id
WHERE slots.slot_kind = ? AND nodes.status = ? AND nodes.probe_kind = ''
ORDER BY nodes.latency_ms ASC, nodes.id ASC
LIMIT 1`, statusHealthyCandidate, statusHealthyCandidate).Scan(
		&slot.ID, &slot.Kind, &slot.NodeID, &slot.ClaimNodeID, &slot.FallbackOrigin, &slot.FallbackEnteredRoundID,
		&slot.ClaimToken, &slot.ClaimStage, &slot.ClaimStartedAt, &slot.LastProcessedRoundID, &slot.BlockedRoundID,
		&candidate.ID, &candidate.Name, &candidate.ProxyURL, &candidate.Host, &candidate.InputIP, &candidate.Port, &candidate.Protocol,
		&candidate.Domain, &candidate.BatchID, &candidate.Status, &candidate.InitialConnected, &candidate.ProbeKind,
		&candidate.ProbeReturnStatus, &candidate.KeepaliveRoundID, &candidate.ReviveRoundID, &candidate.ReviveFailureCount,
		&candidate.LatencyMs, &candidate.EnteredAt, &candidate.ProbeStartedAt, &candidate.ProbeTime, &candidate.ExitIP,
		&candidate.ExitCountry, &candidate.ErrorReason, &candidate.ErrorDetail, &candidate.ReviveTargetStatus,
	)
	if err == sql.ErrNoRows {
		return proxyNode{}, slotRecord{}, false, nil
	}
	if err != nil {
		return proxyNode{}, slotRecord{}, false, fmt.Errorf("选择实时守护健康备选节点: %w", err)
	}
	return candidate, slot, true, nil
}

func reserveRealtimeFallback(transaction *sql.Tx) (proxyNode, bool, error) {
	var fallback proxyNode
	err := scanProxyNode(transaction.QueryRow(`
SELECT id, node_name, proxy_url, host, input_ip, port, protocol, domain, batch_id, status,
       initial_connected, probe_kind, probe_return_status, keepalive_round_id, revive_round_id,
       revive_failure_count, latency_ms, entered_at, probe_started_at, probe_time, exit_ip,
       exit_country, error_reason, error_detail, revive_target_status
FROM ip_nodes
WHERE status = ? AND manual_fallback = 1 AND probe_kind = ''
ORDER BY entered_at ASC, id ASC
LIMIT 1`, statusHealthyFallback), &fallback)
	if err == sql.ErrNoRows {
		return proxyNode{}, false, nil
	}
	if err != nil {
		return proxyNode{}, false, fmt.Errorf("选择实时守护健康保底节点: %w", err)
	}
	result, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = ?, probe_kind = ?, probe_return_status = ?, error_reason = '', error_detail = ''
WHERE id = ? AND status = ? AND manual_fallback = 1 AND probe_kind = ''`, statusProbing, time.Now().UnixMilli(), probeKindRealtimeGuard, statusHealthyFallback, fallback.ID, statusHealthyFallback)
	if err != nil {
		return proxyNode{}, false, fmt.Errorf("预留实时守护健康保底节点: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return proxyNode{}, false, err
	}
	if affected != 1 {
		return proxyNode{}, false, fmt.Errorf("实时守护健康保底节点预留丢失")
	}
	fallback.Status = statusProbing
	fallback.ProbeKind = probeKindRealtimeGuard
	return fallback, true, nil
}

func (store *ipStore) reserveNextRealtimeFallback(healthySlot slotRecord) (realtimeGuardReplacement, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return realtimeGuardReplacement{}, fmt.Errorf("开始实时守护下一保底事务: %w", err)
	}
	defer transaction.Rollback()
	fallback, found, err := reserveRealtimeFallback(transaction)
	if err != nil {
		return realtimeGuardReplacement{}, err
	}
	if err := transaction.Commit(); err != nil {
		return realtimeGuardReplacement{}, fmt.Errorf("提交实时守护下一保底事务: %w", err)
	}
	return realtimeGuardReplacement{HealthySlot: healthySlot, ReplacementNode: fallback, FromFallback: found, FallbackReserved: found}, nil
}

func (store *ipStore) finishRealtimeFallbackFailure(node proxyNode, reason, detail string) error {
	_, err := store.database.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = 0, probe_kind = '', probe_return_status = '',
    error_reason = ?, error_detail = ?, revive_target_status = ?
WHERE id = ? AND status = ? AND probe_kind = ?`, statusCooldown, reason, detail, statusCooldown, node.ID, statusProbing, probeKindRealtimeGuard)
	if err != nil {
		return fmt.Errorf("保存实时守护保底节点失败: %w", err)
	}
	return nil
}

func (store *ipStore) finishRealtimeFallbackSuccess(slot slotRecord, node proxyNode, connectivity probeResult) error {
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("开始实时守护保底成功事务: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, latency_ms = ?, probe_time = ?, probe_started_at = 0, probe_kind = '', probe_return_status = '',
    error_reason = '', error_detail = '', revive_target_status = ?
WHERE id = ? AND status = ? AND probe_kind = ?`, statusHealthy, connectivity.LatencyMs, time.Now().UnixMilli(), statusHealthy, node.ID, statusProbing, probeKindRealtimeGuard); err != nil {
		return fmt.Errorf("保存实时守护保底健康节点: %w", err)
	}
	if _, err := transaction.Exec(`UPDATE ip_slots SET node_id = ? WHERE slot_id = ? AND slot_kind = ? AND node_id = 0`, node.ID, slot.ID, statusHealthy); err != nil {
		return fmt.Errorf("写入实时守护保底健康槽位: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("提交实时守护保底成功事务: %w", err)
	}
	return nil
}

func (store *ipStore) probeRealtimeFallbackQuality(ctx context.Context, node proxyNode, slotID int64) (qualityProbeResult, error) {
	settings, err := store.settings()
	if err != nil {
		return qualityProbeResult{}, err
	}
	authFiles, err := listAuthFiles()
	if err != nil {
		return qualityProbeResult{}, err
	}
	usedAuth := make(map[string]struct{})
	for {
		auth, source, selectErr := selectAuthForQuality(store, node, slotID, 0, usedAuth, authFiles)
		if selectErr != nil {
			return qualityProbeResult{}, selectErr
		}
		result := probeQualityOnce(ctx, node.ProxyURL, auth, settings)
		if err := store.recordQualityAttempt(0, slotID, node.ID, auth, "realtime_"+source, result); err != nil {
			return qualityProbeResult{}, err
		}
		if result.Classification == qualityClassificationQuota {
			continue
		}
		if result.Classification == qualityClassificationNormal {
			if err := store.recordAuthSuccess(node.ID, slotID, 0, auth, "realtime_"+source); err != nil {
				return qualityProbeResult{}, err
			}
		}
		return result, nil
	}
}

func ensureRealtimeGuardReplacementAuth(degradedAuthIndex string) error {
	authFiles, err := listAuthFiles()
	if err != nil {
		return fmt.Errorf("读取可重试 xAI auth: %w", err)
	}
	for _, auth := range authFiles {
		if auth.Index == degradedAuthIndex || auth.Disabled || auth.AccessToken() == "" || auth.Priority == managerAccountAbnormalPriority {
			continue
		}
		return nil
	}
	return fmt.Errorf("实时守护没有可用的新 xAI auth，拒绝重试")
}

func (store *ipStore) logRealtimeDegradationDetected(probe realtimeGuardProbe, decision realtimeGuardDecision) error {
	return store.appendProbeLog(
		logCategoryRealtimeGuard,
		probe.RequestID,
		logStatusProbing,
		logLevelWarn,
		"realtime_guard.degradation_detected",
		probe.SourceSnapshot.NodeID,
		"",
		fmt.Sprintf("【检测到降智】auth:%s，节点:%s，原因:%s", probe.AuthFileName, probe.ProxyURL, decision.Reason),
		fmt.Sprintf("request_id=%s；auth_file=%s；auth_index=%s；节点代理=%s；HTTP=%d；等级=%s；TPS=%.2f", probe.RequestID, probe.AuthFileName, probe.AuthIndex, probe.ProxyURL, probe.StatusCode, decision.QualityLevel, decision.TPS),
	)
}

func (store *ipStore) logRealtimeDegradationRecorded(probe realtimeGuardProbe, decision realtimeGuardDecision, failure realtimeDegradationFailure) {
	_ = store.appendProbeLog(
		logCategoryRealtimeGuard,
		probe.RequestID,
		logStatusProbing,
		logLevelWarn,
		"realtime_guard.degradation_recorded",
		failure.NodeID,
		failure.NodeName,
		"实时守护记录节点首次降智，账号已标记为异常",
		fmt.Sprintf("request_id=%s；auth_id=%s；auth_index=%s；节点代理=%s；降智次数=%d/%d；原因=%s；等级=%s；TPS=%.2f；动作=仅换号重试", probe.RequestID, probe.AuthID, probe.AuthIndex, failure.ProxyURL, failure.ConsecutiveFailureCount, realtimeDegradationReplacementThreshold, decision.Reason, decision.QualityLevel, decision.TPS),
	)
}

func (store *ipStore) logRealtimeManagerAPIUnavailable(probe realtimeGuardProbe, decision realtimeGuardDecision, failure realtimeDegradationFailure, managerErr error) {
	_ = store.appendProbeLog(
		logCategoryRealtimeGuard,
		probe.RequestID,
		logStatusError,
		logLevelWarn,
		"realtime_guard.manager_api_unavailable",
		failure.NodeID,
		failure.NodeName,
		"实时守护未能调用 CPA-Manager-Plus API",
		fmt.Sprintf("request_id=%s；auth_id=%s；auth_index=%s；节点代理=%s；降智次数=%d；原因=%s；错误=%s", probe.RequestID, probe.AuthID, probe.AuthIndex, failure.ProxyURL, failure.ConsecutiveFailureCount, decision.Reason, managerErr.Error()),
	)
}

func (store *ipStore) logRealtimeFallbackCheck(probe realtimeGuardProbe, node proxyNode, stage, detail string) {
	_ = store.appendProbeLog(
		logCategoryRealtimeGuard,
		probe.RequestID,
		logStatusProbing,
		logLevelWarn,
		"realtime_guard."+stage,
		node.ID,
		node.Name,
		"实时守护健康保底节点检测未通过",
		fmt.Sprintf("request_id=%s；auth_id=%s；slot_proxy=%s；节点=%s；详情=%s", probe.RequestID, probe.AuthID, probe.ProxyURL, node.ProxyURL, detail),
	)
}

func (store *ipStore) logRealtimeGuard(probe realtimeGuardProbe, decision realtimeGuardDecision, originalNode, replacementNode proxyNode) {
	level := logLevelWarn
	status := logStatusError
	if decision.Action == realtimeGuardActionRetry {
		level = logLevelInfo
		status = logStatusConnected
	}
	_ = store.appendProbeLog(
		logCategoryRealtimeGuard,
		probe.RequestID,
		status,
		level,
		"realtime_guard."+decision.Reason,
		originalNode.ID,
		originalNode.Name,
		"实时守护发现异常并已处理",
		fmt.Sprintf("request_id=%s；auth_id=%s；auth_index=%s；HTTP=%d；分类=%s；等级=%s；TPS=%.2f；原节点=%d；原代理=%s；替换节点=%d；替换代理=%s；动作=%s；重试=%d/%d；错误=%s", probe.RequestID, probe.AuthID, probe.AuthIndex, probe.StatusCode, decision.Classification, decision.QualityLevel, decision.TPS, originalNode.ID, originalNode.ProxyURL, replacementNode.ID, replacementNode.ProxyURL, decision.Action, probe.RetryCount, probe.MaxRetries, decision.Error),
	)
}
