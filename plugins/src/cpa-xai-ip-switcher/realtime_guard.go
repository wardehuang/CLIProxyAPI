package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

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
		notifyManagerRealtimeHealthyAsync(probe)
		return pluginapi.StreamCompletionInterceptResponse{
			Action:     pluginapi.StreamCompletionAction(decision.Action),
			Reason:     decision.Reason,
			StatusCode: http.StatusBadGateway,
			Error:      decision.Error,
		}, nil
	}
	if decision.Classification == realtimeGuardClassificationTransient {
		return pluginapi.StreamCompletionInterceptResponse{
			Action:     pluginapi.StreamCompletionActionRetry,
			RetryMode:  pluginapi.StreamCompletionRetryModeExcludeSelectedAuth,
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
		RetryMode:  pluginapi.StreamCompletionRetryModeReloadSelectedAuth,
		Reason:     decision.Reason,
		StatusCode: http.StatusBadGateway,
		Error:      decision.Error,
	}, nil
}

func realtimeGuardProbeFromCompletion(completion pluginapi.StreamCompletionInterceptRequest) realtimeGuardProbe {
	return realtimeGuardProbe{
		RequestID:           completion.RequestID,
		Provider:            completion.Provider,
		SourceFormat:        completion.SourceFormat,
		Model:               completion.Model,
		RequestedModel:      completion.RequestedModel,
		AuthID:              completion.AuthID,
		AuthIndex:           completion.AuthIndex,
		AuthFileName:        completion.AuthFileName,
		ProxyURL:            completion.ProxyURL,
		RequestHeaders:      completion.RequestHeaders,
		ResponseHeaders:     completion.ResponseHeaders,
		Body:                bytes.Clone(completion.Body),
		StatusCode:          completion.StatusCode,
		Error:               completion.Error,
		Completed:           completion.Completed,
		StartedAt:           completion.StartedAt,
		UpstreamStartedAt:   completion.UpstreamStartedAt,
		FirstResponseByteAt: completion.FirstResponseByteAt,
		FirstPayloadAt:      completion.FirstPayloadAt,
		FirstVisibleAt:      completion.FirstVisibleAt,
		FinishedAt:          completion.FinishedAt,
		RetryCount:          completion.RetryCount,
		MaxRetries:          completion.MaxRetries,
		Metadata:            completion.Metadata,
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
	if !probe.FinishedAt.IsZero() && !probe.StartedAt.IsZero() {
		requestDuration := probe.FinishedAt.Sub(probe.StartedAt)
		decision.TotalDurationMs = requestDuration.Milliseconds()
		if probe.FirstPayloadAt.IsZero() && requestDuration > time.Duration(settings.RealtimeGuardTimeoutSeconds)*time.Second {
			decision.Action = realtimeGuardActionRetry
			decision.Classification = realtimeGuardClassificationDegradation
			decision.QualityLevel = realtimeGuardQualitySoft
			decision.Reason = "first_payload_timeout"
			decision.Error = fmt.Sprintf("实时守护首字超过 %d 秒未返回", settings.RealtimeGuardTimeoutSeconds)
			return decision
		}
	}
	if strings.TrimSpace(probe.Error) != "" {
		if isRealtimeGuardTransientUpstreamError(probe.Error) {
			return realtimeGuardDecision{
				Action:         realtimeGuardActionRetry,
				Reason:         "upstream_temporarily_unavailable",
				Classification: realtimeGuardClassificationTransient,
				QualityLevel:   realtimeGuardQualityUnknown,
				Error:          probe.Error,
			}
		}
		return realtimeGuardUnknownDecision("upstream_error", probe.Error)
	}
	if probe.StatusCode < http.StatusOK || probe.StatusCode >= http.StatusMultipleChoices {
		return realtimeGuardUnknownDecision(fmt.Sprintf("http_%d", probe.StatusCode), "上游 HTTP 响应异常")
	}
	if !probe.Completed {
		return realtimeGuardUnknownDecision("sse_incomplete", "SSE 流未完整结束")
	}

	evidence, sseErr := parseRealtimeGuardSSE(probe.Body)
	if sseErr != nil {
		return realtimeGuardUnknownDecision("sse_failed", sseErr.Error())
	}
	if probe.UpstreamStartedAt.IsZero() {
		return realtimeGuardUnknownDecision("upstream_started_at_missing", "上游 HTTP 请求起点缺失")
	}
	if probe.FirstResponseByteAt.IsZero() {
		return realtimeGuardUnknownDecision("upstream_first_byte_missing", "上游 HTTP 首字节时间缺失")
	}
	if probe.FirstResponseByteAt.Before(probe.UpstreamStartedAt) {
		return realtimeGuardUnknownDecision("upstream_ttfb_invalid", "上游 HTTP 首字节早于请求起点")
	}
	if probe.FirstPayloadAt.IsZero() {
		return realtimeGuardUnknownDecision("first_payload_missing", "SSE 首个 payload 时间缺失")
	}
	if probe.FirstPayloadAt.Before(probe.FirstResponseByteAt) {
		return realtimeGuardUnknownDecision("first_payload_invalid", "SSE 首个 payload 早于上游 HTTP 首字节")
	}
	if probe.FinishedAt.IsZero() {
		return realtimeGuardUnknownDecision("finished_at_missing", "SSE 流结束时间缺失")
	}
	generationStartedAt := probe.FirstPayloadAt
	generationFinishedAt := probe.FinishedAt
	if generationFinishedAt.Before(generationStartedAt) {
		return realtimeGuardUnknownDecision("generation_window_invalid", "生成窗口终点早于 SSE 首个 payload")
	}
	generationDuration := generationFinishedAt.Sub(generationStartedAt)
	generationDuration = time.Duration(generationDuration.Milliseconds()) * time.Millisecond
	if generationDuration < time.Millisecond {
		generationDuration = time.Millisecond
	}
	ttfbDuration := probe.FirstResponseByteAt.Sub(probe.UpstreamStartedAt)
	ttfbDuration = time.Duration(ttfbDuration.Milliseconds()) * time.Millisecond
	decision.TTFBMs = ttfbDuration.Milliseconds()
	decision.GenerationMs = generationDuration.Milliseconds()
	evidence = evaluateRealtimeGuardThinking(evidence, probe, settings)
	decision.TotalTokens = evidence.OutputTokens + evidence.ReasoningTokens
	decision.IsRealThinking = evidence.IsRealThinking
	decision.RealThinkingReason = evidence.Reason
	decision.SummaryChars = evidence.SummaryChars
	decision.EncryptedBytes = evidence.EncryptedBytes
	decision.EncryptedFloor = evidence.EncryptedFloor
	decision.VisibleTokens = evidence.VisibleTokens
	decision.VisibleFlushMs = evidence.VisibleFlushMs
	decision.TPS = float64(decision.TotalTokens) / generationDuration.Seconds()
	if decision.TPS >= settings.QualityHardTPS {
		decision.Action = realtimeGuardActionRetry
		decision.Classification = realtimeGuardClassificationDegradation
		decision.QualityLevel = realtimeGuardQualityHard
		decision.Reason = "hard_tps"
		return decision
	}
	if decision.TPS > settings.QualitySoftTPS && decision.TPS < settings.QualityHardTPS && !decision.IsRealThinking {
		decision.Action = realtimeGuardActionRetry
		decision.Classification = realtimeGuardClassificationDegradation
		decision.QualityLevel = realtimeGuardQualitySoft
		decision.Reason = "soft_tps_missing_real_thinking"
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

func isRealtimeGuardTransientUpstreamError(message string) bool {
	return strings.EqualFold(
		strings.TrimSpace(message),
		"Service temporarily unavailable. The model's availability is currently degraded.",
	)
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

type realtimeGuardThinkingEvidence struct {
	OutputTokens           int64
	ReasoningTokens        int64
	SummaryChars           int
	SummaryText            string
	EncryptedBytes         int
	ReasoningItemID        string
	ReasoningItemStarted   bool
	ReasoningItemCompleted bool
	ReasoningMetadataError bool
	VisibleTextChars       int
	FunctionCallCount      int
	VisibleTokens          int64
	VisibleFlushMs         int64
	EncryptedFloor         int
	IsRealThinking         bool
	Reason                 string
}

func parseRealtimeGuardSSE(body []byte) (realtimeGuardThinkingEvidence, error) {
	if len(body) == 0 {
		return realtimeGuardThinkingEvidence{}, fmt.Errorf("SSE 响应为空")
	}
	evidence := realtimeGuardThinkingEvidence{VisibleFlushMs: -1}
	var summaryDelta strings.Builder
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
				return realtimeGuardThinkingEvidence{}, fmt.Errorf("解析 SSE 事件失败: %w", err)
			}
			eventType := strings.ToLower(stringField(message, "type"))
			if strings.Contains(eventType, "failed") || strings.Contains(eventType, "incomplete") || eventType == "error" {
				return realtimeGuardThinkingEvidence{}, fmt.Errorf("SSE 事件异常: %s", eventType)
			}
			switch eventType {
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				recordRealtimeGuardReasoningItemID(stringField(message, "item_id"), &evidence)
				summaryDelta.WriteString(stringField(message, "delta"))
			case "response.reasoning_summary_text.done", "response.reasoning_text.done":
				recordRealtimeGuardReasoningItemID(stringField(message, "item_id"), &evidence)
				recordRealtimeGuardSummary(stringField(message, "text"), &evidence)
			case "response.reasoning_summary_part.done":
				recordRealtimeGuardReasoningItemID(stringField(message, "item_id"), &evidence)
				if part, ok := message["part"].(map[string]any); ok && strings.EqualFold(stringField(part, "type"), "summary_text") {
					recordRealtimeGuardSummary(stringField(part, "text"), &evidence)
				}
			case "response.output_text.delta":
				evidence.VisibleTextChars += utf8.RuneCountInString(stringField(message, "delta"))
			case "response.output_item.added":
				if item, ok := message["item"].(map[string]any); ok {
					collectRealtimeGuardOutputItem(item, &evidence, false)
				}
			case "response.output_item.done":
				if item, ok := message["item"].(map[string]any); ok {
					collectRealtimeGuardOutputItem(item, &evidence, true)
				}
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
				evidence.OutputTokens = int64(integerField(usage, "output_tokens"))
				if outputDetails, ok := usage["output_tokens_details"].(map[string]any); ok {
					evidence.ReasoningTokens = int64(integerField(outputDetails, "reasoning_tokens"))
				}
			}
			if response, ok := message["response"].(map[string]any); ok {
				if output, ok := response["output"].([]any); ok {
					for _, rawItem := range output {
						if item, itemOK := rawItem.(map[string]any); itemOK {
							collectRealtimeGuardOutputItem(item, &evidence, true)
						}
					}
				}
			}
		}
	}
	if !completed {
		return realtimeGuardThinkingEvidence{}, fmt.Errorf("SSE 流未收到 response.completed 或 [DONE]")
	}
	recordRealtimeGuardSummary(summaryDelta.String(), &evidence)
	return evidence, nil
}

func collectRealtimeGuardOutputItem(item map[string]any, evidence *realtimeGuardThinkingEvidence, terminal bool) {
	if evidence == nil {
		return
	}
	switch strings.ToLower(stringField(item, "type")) {
	case "reasoning":
		recordRealtimeGuardReasoningItemID(stringField(item, "id"), evidence)
		if terminal || strings.EqualFold(stringField(item, "status"), "completed") {
			evidence.ReasoningItemCompleted = true
		}
		evidence.EncryptedBytes = maxRealtimeGuardInt(evidence.EncryptedBytes, len([]byte(strings.TrimSpace(stringField(item, "encrypted_content")))))
		if summaries, ok := item["summary"].([]any); ok {
			for _, rawSummary := range summaries {
				summary, summaryOK := rawSummary.(map[string]any)
				if !summaryOK || !strings.EqualFold(stringField(summary, "type"), "summary_text") {
					continue
				}
				recordRealtimeGuardSummary(stringField(summary, "text"), evidence)
			}
		}
	case "message":
		if content, ok := item["content"].([]any); ok {
			visibleChars := 0
			for _, rawContent := range content {
				part, partOK := rawContent.(map[string]any)
				if !partOK {
					continue
				}
				text := stringField(part, "text")
				if text == "" {
					text = stringField(part, "refusal")
				}
				visibleChars += utf8.RuneCountInString(text)
			}
			evidence.VisibleTextChars = maxRealtimeGuardInt(evidence.VisibleTextChars, visibleChars)
		}
	case "function_call":
		if strings.EqualFold(stringField(item, "status"), "completed") && strings.TrimSpace(stringField(item, "call_id")) != "" && strings.TrimSpace(stringField(item, "name")) != "" {
			evidence.FunctionCallCount++
		}
	}
}

func recordRealtimeGuardReasoningItemID(itemID string, evidence *realtimeGuardThinkingEvidence) {
	if evidence == nil {
		return
	}
	itemID = strings.TrimSpace(itemID)
	evidence.ReasoningItemStarted = true
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

func recordRealtimeGuardSummary(text string, evidence *realtimeGuardThinkingEvidence) {
	if evidence == nil {
		return
	}
	text = strings.TrimSpace(text)
	chars := utf8.RuneCountInString(text)
	if chars > evidence.SummaryChars {
		evidence.SummaryChars = chars
		evidence.SummaryText = text
	}
}

func isRealtimeGuardPlaceholderSummary(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "thinking", "thinking...", "thinking…":
		return true
	default:
		return false
	}
}

func evaluateRealtimeGuardThinking(evidence realtimeGuardThinkingEvidence, probe realtimeGuardProbe, settings pluginSettings) realtimeGuardThinkingEvidence {
	if evidence.OutputTokens < int64(settings.RealtimeGuardMinOutputTokens) {
		evidence.IsRealThinking = true
		evidence.Reason = "below_minimum_output_tokens"
		return evidence
	}
	evidence.VisibleTokens = evidence.OutputTokens - evidence.ReasoningTokens
	if evidence.VisibleTokens < 0 {
		evidence.VisibleTokens = 0
	}
	evidence.EncryptedFloor = settings.RealtimeGuardMinEncryptedBytes
	dynamicFloor := int(evidence.ReasoningTokens) * settings.RealtimeGuardEncryptedBytesPerReasoningToken
	if dynamicFloor > evidence.EncryptedFloor {
		evidence.EncryptedFloor = dynamicFloor
	}
	if !probe.FirstVisibleAt.IsZero() && !probe.FinishedAt.IsZero() {
		evidence.VisibleFlushMs = probe.FinishedAt.Sub(probe.FirstVisibleAt).Milliseconds()
	}
	hasSummaryEvidence := !evidence.ReasoningMetadataError &&
		evidence.SummaryChars >= settings.RealtimeGuardMinSummaryChars &&
		!isRealtimeGuardPlaceholderSummary(evidence.SummaryText)
	hasEncryptedEvidence := !evidence.ReasoningMetadataError && evidence.ReasoningItemCompleted && evidence.EncryptedBytes >= evidence.EncryptedFloor
	burstDump := evidence.ReasoningTokens >= int64(settings.RealtimeGuardBurstMinReasoningTokens) &&
		evidence.VisibleTokens > 0 && evidence.VisibleTokens < int64(settings.RealtimeGuardBurstMaxVisibleTokens) &&
		evidence.VisibleFlushMs >= 0 && evidence.VisibleFlushMs < int64(settings.RealtimeGuardBurstMaxWindowMS)
	evidence.IsRealThinking = (hasSummaryEvidence || hasEncryptedEvidence) && !burstDump
	switch {
	case burstDump:
		evidence.Reason = "burst_dump"
	case evidence.ReasoningMetadataError:
		evidence.Reason = "reasoning_metadata_invalid"
	case hasSummaryEvidence && hasEncryptedEvidence:
		evidence.Reason = "summary_and_encrypted_evidence"
	case hasSummaryEvidence:
		evidence.Reason = "summary_evidence"
	case hasEncryptedEvidence:
		evidence.Reason = "encrypted_evidence"
	case evidence.ReasoningTokens > 0:
		evidence.Reason = "reasoning_tokens_without_evidence"
	default:
		evidence.Reason = "missing_thinking_evidence"
	}
	return evidence
}

func maxRealtimeGuardInt(left, right int) int {
	if left > right {
		return left
	}
	return right
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
		negativePriority := auth.PrioritySet && auth.Priority < 0
		if auth.Index == degradedAuthIndex || auth.Disabled || auth.AccessToken() == "" || negativePriority {
			continue
		}
		return nil
	}
	return fmt.Errorf("实时守护没有可用的新 xAI auth，拒绝重试")
}

func (store *ipStore) logRealtimeDegradationDetected(probe realtimeGuardProbe, decision realtimeGuardDecision) error {
	authIdentity := realtimeGuardAuthIdentity(probe)
	return store.appendProbeLog(
		logCategoryRealtimeGuard,
		probe.RequestID,
		logStatusProbing,
		logLevelWarn,
		"realtime_guard.degradation_detected",
		probe.SourceSnapshot.NodeID,
		"",
		fmt.Sprintf("【检测到降智】auth:%s，节点:%s，原因:%s", authIdentity, probe.ProxyURL, decision.Reason),
		fmt.Sprintf("request_id=%s；auth_file=%s；auth_index=%s；节点代理=%s；HTTP=%d；等级=%s；总耗时=%dms；TPS=%.2f；TTFB=%dms；首字后耗时=%dms；输出+思考tokens=%d；isRealThinking=%t；thinking原因=%s；summary字符=%d；encrypted=%d/%d字节；可见tokens=%d；可见倾倒=%dms", probe.RequestID, authIdentity, probe.AuthIndex, probe.ProxyURL, probe.StatusCode, decision.QualityLevel, decision.TotalDurationMs, decision.TPS, decision.TTFBMs, decision.GenerationMs, decision.TotalTokens, decision.IsRealThinking, decision.RealThinkingReason, decision.SummaryChars, decision.EncryptedBytes, decision.EncryptedFloor, decision.VisibleTokens, decision.VisibleFlushMs),
	)
}

func realtimeGuardAuthIdentity(probe realtimeGuardProbe) string {
	if fileName := strings.TrimSpace(probe.AuthFileName); fileName != "" && fileName != "." {
		return fileName
	}
	if authIndex := strings.TrimSpace(probe.AuthIndex); authIndex != "" {
		return authIndex
	}
	return strings.TrimSpace(probe.AuthID)
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
		fmt.Sprintf("request_id=%s；auth_id=%s；auth_index=%s；HTTP=%d；分类=%s；等级=%s；总耗时=%dms；TPS=%.2f；TTFB=%dms；首字后耗时=%dms；输出+思考tokens=%d；isRealThinking=%t；thinking原因=%s；summary字符=%d；encrypted=%d/%d字节；可见tokens=%d；可见倾倒=%dms；原节点=%d；原代理=%s；替换节点=%d；替换代理=%s；动作=%s；重试=%d/%d；错误=%s", probe.RequestID, probe.AuthID, probe.AuthIndex, probe.StatusCode, decision.Classification, decision.QualityLevel, decision.TotalDurationMs, decision.TPS, decision.TTFBMs, decision.GenerationMs, decision.TotalTokens, decision.IsRealThinking, decision.RealThinkingReason, decision.SummaryChars, decision.EncryptedBytes, decision.EncryptedFloor, decision.VisibleTokens, decision.VisibleFlushMs, originalNode.ID, originalNode.ProxyURL, replacementNode.ID, replacementNode.ProxyURL, decision.Action, probe.RetryCount, probe.MaxRetries, decision.Error),
	)
}
