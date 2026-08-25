package main

import (
	"bytes"
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
	HealthySlot     slotRecord
	OriginalNode    proxyNode
	ReplacementNode proxyNode
	HasReplacement  bool
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
		}
		if applyErr := store.applyRealtimeGuard(probe, &decision); applyErr != nil {
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

	outputTokens, reasoningTokens, thinkingDelta, sseErr := parseRealtimeGuardSSE(probe.Body)
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
	generationDuration = time.Duration(generationDuration.Milliseconds()) * time.Millisecond
	if generationDuration < time.Millisecond {
		generationDuration = time.Millisecond
	}
	ttfbDuration := generationStartedAt.Sub(probe.StartedAt)
	ttfbDuration = time.Duration(ttfbDuration.Milliseconds()) * time.Millisecond
	decision.TTFBMs = ttfbDuration.Milliseconds()
	decision.GenerationMs = generationDuration.Milliseconds()
	decision.TotalTokens = outputTokens + reasoningTokens
	decision.TPS = float64(decision.TotalTokens) / generationDuration.Seconds()
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
		return decision
	}
	if ttfbDuration.Seconds() > settings.RealtimeGuardTTFBSeconds &&
		generationDuration.Seconds() < settings.RealtimeGuardGenerationSeconds &&
		decision.TotalTokens > int64(settings.RealtimeGuardTokenThreshold) {
		decision.Action = realtimeGuardActionRetry
		decision.Classification = realtimeGuardClassificationDegradation
		decision.QualityLevel = realtimeGuardQualitySoft
		decision.Reason = "ttfb_downgrade"
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

func parseRealtimeGuardSSE(body []byte) (int64, int64, bool, error) {
	if len(body) == 0 {
		return 0, 0, false, fmt.Errorf("SSE 响应为空")
	}
	var outputTokens int64
	var reasoningTokens int64
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
				return 0, 0, false, fmt.Errorf("解析 SSE 事件失败: %w", err)
			}
			eventType := strings.ToLower(stringField(message, "type"))
			if strings.Contains(eventType, "failed") || strings.Contains(eventType, "incomplete") || eventType == "error" {
				return 0, 0, false, fmt.Errorf("SSE 事件异常: %s", eventType)
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
				if outputDetails, ok := usage["output_tokens_details"].(map[string]any); ok {
					reasoningTokens = int64(integerField(outputDetails, "reasoning_tokens"))
				}
			}
		}
	}
	if !completed {
		return 0, 0, false, fmt.Errorf("SSE 流未收到 response.completed 或 [DONE]")
	}
	return outputTokens, reasoningTokens, thinkingDelta, nil
}

func (store *ipStore) applyRealtimeGuard(probe realtimeGuardProbe, decision *realtimeGuardDecision) error {
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

	if !replacement.HasReplacement {
		if err := refreshHealthyAuthDistribution(store, settings.HealthySlotCount); err != nil {
			return fmt.Errorf("实时守护清空健康槽位 auth 文件: %w", err)
		}
		decision.Action = realtimeGuardActionFail
		decision.Error = "实时守护没有可用健康备选节点"
		store.logRealtimeGuard(probe, *decision, replacement.OriginalNode, proxyNode{})
		return nil
	}

	decision.ReplacementNodeID = replacement.ReplacementNode.ID
	decision.ReplacementProxyURL = replacement.ReplacementNode.ProxyURL
	if err := refreshHealthyAuthDistribution(store, settings.HealthySlotCount); err != nil {
		return fmt.Errorf("实时守护同步所有 auth 文件: %w", err)
	}
	decision.Action = realtimeGuardActionRetry
	store.logRealtimeGuard(probe, *decision, replacement.OriginalNode, replacement.ReplacementNode)
	return nil
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
       claim_token, claim_stage, claim_started_at, last_processed_round_id, blocked_round_id, refresh_at
FROM ip_slots
WHERE slot_id = ? AND node_id = ? AND slot_kind = ?`, probe.SourceSnapshot.SlotID, probe.SourceSnapshot.NodeID, statusHealthy), &replacement.HealthySlot); err != nil {
			if err == sql.ErrNoRows {
				return realtimeGuardReplacement{}, fmt.Errorf("实时守护源槽位快照已失效")
			}
			return realtimeGuardReplacement{}, err
		}
	} else if err := scanRealtimeHealthySlot(transaction.QueryRow(`
SELECT slots.slot_id, slots.slot_kind, slots.node_id, slots.claim_node_id, slots.fallback_origin, slots.fallback_entered_round_id,
       slots.claim_token, slots.claim_stage, slots.claim_started_at, slots.last_processed_round_id, slots.blocked_round_id, slots.refresh_at
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
SET `+slotNodeIDAssignment()+`, claim_node_id = 0, fallback_origin = 0, fallback_entered_round_id = 0,
    claim_token = '', claim_stage = '', claim_started_at = 0
WHERE slot_id = ? AND node_id = ?`, slotNodeIDArgs(0, replacement.HealthySlot.ID, replacement.OriginalNode.ID)...); err != nil {
		return realtimeGuardReplacement{}, fmt.Errorf("清空实时守护健康槽位: %w", err)
	}

	candidate, candidateSlot, candidateFound, err := findRealtimeCandidate(transaction)
	if err != nil {
		return realtimeGuardReplacement{}, err
	}
	if candidateFound {
		if _, err := transaction.Exec(`UPDATE ip_slots SET `+slotNodeIDAssignment()+` WHERE slot_id = ? AND node_id = ?`, slotNodeIDArgs(0, candidateSlot.ID, candidate.ID)...); err != nil {
			return realtimeGuardReplacement{}, fmt.Errorf("清空实时守护健康备选槽位: %w", err)
		}
		if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, error_reason = '', error_detail = '', revive_target_status = ?
WHERE id = ? AND status = ?`, statusHealthy, statusHealthy, candidate.ID, statusHealthyCandidate); err != nil {
			return realtimeGuardReplacement{}, fmt.Errorf("提升实时守护健康备选节点: %w", err)
		}
		if _, err := transaction.Exec(`UPDATE ip_slots SET `+slotNodeIDAssignment()+` WHERE slot_id = ? AND node_id = 0`, slotNodeIDArgs(candidate.ID, replacement.HealthySlot.ID)...); err != nil {
			return realtimeGuardReplacement{}, fmt.Errorf("写入实时守护健康替换槽位: %w", err)
		}
		replacement.ReplacementNode = candidate
		replacement.ReplacementNode.Status = statusHealthy
		replacement.HasReplacement = true
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
		&slot.ClaimToken, &slot.ClaimStage, &slot.ClaimStartedAt, &slot.LastProcessedRoundID, &slot.BlockedRoundID, &slot.RefreshAt,
	)
}

func findRealtimeCandidate(transaction *sql.Tx) (proxyNode, slotRecord, bool, error) {
	var candidate proxyNode
	var slot slotRecord
	err := transaction.QueryRow(`
SELECT slots.slot_id, slots.slot_kind, slots.node_id, slots.claim_node_id, slots.fallback_origin, slots.fallback_entered_round_id,
       slots.claim_token, slots.claim_stage, slots.claim_started_at, slots.last_processed_round_id, slots.blocked_round_id, slots.refresh_at,
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
		&slot.ClaimToken, &slot.ClaimStage, &slot.ClaimStartedAt, &slot.LastProcessedRoundID, &slot.BlockedRoundID, &slot.RefreshAt,
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
		fmt.Sprintf("request_id=%s；auth_file=%s；auth_index=%s；节点代理=%s；HTTP=%d；等级=%s；TPS=%.2f；TTFB=%dms；首字后耗时=%dms；输出+思考tokens=%d", probe.RequestID, probe.AuthFileName, probe.AuthIndex, probe.ProxyURL, probe.StatusCode, decision.QualityLevel, decision.TPS, decision.TTFBMs, decision.GenerationMs, decision.TotalTokens),
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
		fmt.Sprintf("request_id=%s；auth_id=%s；auth_index=%s；HTTP=%d；分类=%s；等级=%s；TPS=%.2f；TTFB=%dms；首字后耗时=%dms；输出+思考tokens=%d；原节点=%d；原代理=%s；替换节点=%d；替换代理=%s；动作=%s；重试=%d/%d；错误=%s", probe.RequestID, probe.AuthID, probe.AuthIndex, probe.StatusCode, decision.Classification, decision.QualityLevel, decision.TPS, decision.TTFBMs, decision.GenerationMs, decision.TotalTokens, originalNode.ID, originalNode.ProxyURL, replacementNode.ID, replacementNode.ProxyURL, decision.Action, probe.RetryCount, probe.MaxRetries, decision.Error),
	)
}
