package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	qualityProbeEndpoint          = "https://cli-chat-proxy.grok.com/v1/responses"
	qualityProbeModel             = "grok-4.5"
	qualityProbePrompt            = "用中文回答：17 × 23 等于多少？只输出计算过程和答案。"
	qualityClassificationQuota    = "quota_exhausted"
	qualityClassificationUnknown  = "unknown"
	qualityClassificationDegraded = "suspected_degradation"
	qualityClassificationNormal   = "normal"
	qualityLevelQuota             = "quota_exhausted"
	qualityLevelUnknown           = "unknown"
	qualityLevelHard              = "hard"
	qualityLevelSoft              = "soft"
	qualityLevelHealthy           = "healthy"
)

type qualityProbeResult struct {
	ProxyURL              string
	StartedAt             int64
	FinishedAt            int64
	StatusCode            int
	Classification        string
	QualityLevel          string
	ClassificationReason  string
	TTFBMs                int64
	FirstTokenMs          int64
	GenerationMs          int64
	TotalMs               int64
	OutputTokens          int64
	ReasoningTokens       int64
	OutputTokensPerSecond float64
	QualitySoftTPS        float64
	QualityHardTPS        float64
	ThinkingDelta         bool
	AnswerMatched         bool
	ErrorCode             string
	Detail                string
	Unavailable           bool
}

func (result qualityProbeResult) DisplayReason() string {
	if result.ClassificationReason != "" {
		return result.ClassificationReason
	}
	if result.Detail != "" {
		return result.Detail
	}
	return result.Classification
}

func runQualityProbeForWork(ctx context.Context, store *ipStore, work qualityWork, settings pluginSettings) (qualityProbeResult, authFile, string) {
	usedAuth := make(map[string]struct{})
	var lastResult qualityProbeResult
	var lastAuth authFile
	var lastSource string
	for {
		var auth authFile
		var source string
		var err error
		if lastResult.Classification == qualityClassificationQuota {
			auth, source, err = selectRandomAuthAfterQuota(store, work.Node, work.Slot.ID, work.RoundID, usedAuth)
		} else {
			auth, source, err = selectAuthForQuality(store, work.Node, work.Slot.ID, work.RoundID, usedAuth)
		}
		if err != nil {
			now := time.Now().UnixMilli()
			result := qualityProbeResult{
				ProxyURL:             work.Node.ProxyURL,
				StartedAt:            now,
				FinishedAt:           now,
				Classification:       qualityClassificationUnknown,
				QualityLevel:         qualityLevelUnknown,
				ClassificationReason: "auth_pool_exhausted",
				QualitySoftTPS:       settings.QualitySoftTPS,
				QualityHardTPS:       settings.QualityHardTPS,
				Detail:               err.Error(),
				Unavailable:          true,
			}
			_ = store.appendProbeLog(
				logCategoryKeepaliveProbe,
				keepaliveGroupID(work.RoundID),
				logStatusProbing,
				logLevelWarn,
				"quality.auth_unavailable",
				work.Node.ID,
				work.Node.Name,
				fmt.Sprintf("槽位 %d 无可用 auth，本次智商探测不可判定", work.Slot.ID),
				fmt.Sprintf("轮次=%d；槽位=%d；节点代理=%s；已尝试auth=%d；原因=%s", work.RoundID, work.Slot.ID, work.Node.ProxyURL, len(usedAuth), err.Error()),
			)
			return result, lastAuth, lastSource
		}
		lastAuth = auth
		lastSource = source
		result := probeQualityOnce(ctx, work.Node.ProxyURL, auth, settings)
		if err := store.recordQualityAttempt(work.RoundID, work.Slot.ID, work.Node.ID, auth, source, result); err != nil {
			_ = store.appendLog(logLevelError, "quality.attempt_save_failed", work.Node.ID, work.Node.Name, "保存智商探测记录失败", err.Error())
		}
		_ = store.appendProbeLog(
			logCategoryKeepaliveProbe,
			keepaliveGroupID(work.RoundID),
			qualityLogStatus(result),
			qualityLogLevel(result),
			"quality.attempt_completed",
			work.Node.ID,
			work.Node.Name,
			fmt.Sprintf("槽位 %d 智商探测尝试完成，分类 %s", work.Slot.ID, result.Classification),
			qualityAttemptLogDetail(work, auth, source, result),
		)
		if result.Classification == qualityClassificationQuota {
			_ = store.appendProbeLog(
				logCategoryKeepaliveProbe,
				keepaliveGroupID(work.RoundID),
				logStatusProbing,
				logLevelWarn,
				"quality.auth_quota_exhausted",
				work.Node.ID,
				work.Node.Name,
				fmt.Sprintf("槽位 %d auth 额度耗尽，立即换号", work.Slot.ID),
				fmt.Sprintf("轮次=%d；auth=%s；auth_index=%s；选择来源=%s；节点代理=%s", work.RoundID, auth.Name, auth.Index, source, work.Node.ProxyURL),
			)
			lastResult = result
			continue
		}
		if result.Classification == qualityClassificationNormal {
			if err := store.recordAuthSuccess(work.Node.ID, work.Slot.ID, work.RoundID, auth, source); err != nil {
				_ = store.appendLog(logLevelError, "quality.auth_history_save_failed", work.Node.ID, work.Node.Name, "保存智商探测成功 auth 历史失败", err.Error())
			}
		}
		return result, auth, source
	}
}

func qualityAttemptLogDetail(work qualityWork, auth authFile, source string, result qualityProbeResult) string {
	return fmt.Sprintf(
		"轮次=%d；槽位=%d；auth=%s；auth_index=%s；选择来源=%s；节点代理=%s；endpoint=%s；model=%s；HTTP=%d；TTFB=%dms；首生成=%dms；生成=%dms；总耗时=%dms；tokens=%d；reasoning_tokens=%d；TPS=%.2f；soft_tps=%.2f；hard_tps=%.2f；分类=%s；等级=%s；原因=%s；thinking_delta=%t；答案命中=%t；错误码=%s；详情=%s",
		work.RoundID,
		work.Slot.ID,
		auth.Name,
		auth.Index,
		source,
		work.Node.ProxyURL,
		qualityProbeEndpoint,
		qualityProbeModel,
		result.StatusCode,
		result.TTFBMs,
		result.FirstTokenMs,
		result.GenerationMs,
		result.TotalMs,
		result.OutputTokens,
		result.ReasoningTokens,
		result.OutputTokensPerSecond,
		result.QualitySoftTPS,
		result.QualityHardTPS,
		result.Classification,
		result.QualityLevel,
		result.ClassificationReason,
		result.ThinkingDelta,
		result.AnswerMatched,
		result.ErrorCode,
		result.Detail,
	)
}

func selectRandomAuthAfterQuota(store *ipStore, node proxyNode, slotID, roundID int64, used map[string]struct{}) (authFile, string, error) {
	authFiles, err := listAuthFiles()
	if err != nil {
		return authFile{}, "", err
	}
	candidates := make([]authFile, 0, len(authFiles))
	for _, auth := range authFiles {
		if auth.HasPositivePriority() {
			if _, exists := used[auth.Identity()]; !exists {
				candidates = append(candidates, auth)
			}
		}
	}
	if len(candidates) == 0 {
		return authFile{}, "", fmt.Errorf("额度耗尽后没有可切换的 xAI auth")
	}
	selected, err := store.recordRandomAuthSelection(roundID, node.ID, slotID, candidates)
	if err != nil {
		return authFile{}, "", err
	}
	used[selected.Identity()] = struct{}{}
	return selected, "quota_random", nil
}

func probeQualityOnce(ctx context.Context, proxyURL string, auth authFile, settings pluginSettings) qualityProbeResult {
	startedAt := time.Now()
	result := qualityProbeResult{
		ProxyURL:       proxyURL,
		StartedAt:      startedAt.UnixMilli(),
		QualitySoftTPS: settings.QualitySoftTPS,
		QualityHardTPS: settings.QualityHardTPS,
	}
	requestBody, err := json.Marshal(map[string]any{
		"model":  qualityProbeModel,
		"input":  qualityProbePrompt,
		"stream": true,
		"reasoning": map[string]string{
			"effort":  "high",
			"summary": "detailed",
		},
		"max_output_tokens": 96,
		"temperature":       0,
	})
	if err != nil {
		return finishUnknownQualityResult(result, "request_encode", err.Error())
	}
	client, err := newProxyHTTPClientWithTimeout(proxyURL, time.Duration(settings.QualityProbeTimeoutSeconds)*time.Second)
	if err != nil {
		return finishUnknownQualityResult(result, "proxy_client", err.Error())
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, qualityProbeEndpoint, bytes.NewReader(requestBody))
	if err != nil {
		return finishUnknownQualityResult(result, "request_create", err.Error())
	}
	request.Header.Set("Authorization", "Bearer "+auth.AccessToken())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	request.Header.Set("x-grok-client-version", "0.2.93")
	request.Header.Set("x-grok-client-identifier", "grok-shell")
	request.Header.Set("User-Agent", "CPA-xai-ip-switcher/0.1.0")
	applyAuthHeaders(request, auth)
	response, err := client.Do(request)
	if err != nil {
		return finishUnknownQualityResult(result, "http_request", truncateProbeDetail(err.Error()))
	}
	defer response.Body.Close()
	result.TTFBMs = time.Since(startedAt).Milliseconds()
	result.StatusCode = response.StatusCode
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 32*1024))
		detail := strings.TrimSpace(string(body))
		if readErr != nil {
			detail = readErr.Error()
		}
		if isQuotaExhausted(detail) {
			return finishQuotaQualityResult(result, detail)
		}
		return finishUnknownQualityResult(result, fmt.Sprintf("http_%d", response.StatusCode), detail)
	}
	return readQualitySSE(response.Body, result, settings)
}

func applyAuthHeaders(request *http.Request, auth authFile) {
	rawHeaders, ok := auth.Raw["headers"].(map[string]any)
	if !ok {
		return
	}
	for key, value := range rawHeaders {
		text, isString := value.(string)
		if isString && strings.TrimSpace(text) != "" {
			request.Header.Set(key, text)
		}
	}
	request.Header.Set("Authorization", "Bearer "+auth.AccessToken())
	request.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	request.Header.Set("x-grok-client-version", "0.2.93")
}

func readQualitySSE(reader io.Reader, result qualityProbeResult, settings pluginSettings) qualityProbeResult {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var dataLines []string
	var modelAnswer strings.Builder
	firstTokenAt := time.Time{}
	completed := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(dataLines) > 0 {
				if processErr := processQualitySSEEvent(strings.Join(dataLines, "\n"), &result, &modelAnswer, &firstTokenAt, &completed); processErr != nil {
					return finishUnknownQualityResult(result, "sse_event", processErr.Error())
				}
				if result.Classification == qualityClassificationQuota {
					return finishQuotaQualityResult(result, result.Detail)
				}
				dataLines = dataLines[:0]
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		if result.Classification == qualityClassificationQuota {
			return finishQuotaQualityResult(result, result.Detail)
		}
		return finishUnknownQualityResult(result, "sse_read", err.Error())
	}
	if len(dataLines) > 0 {
		if processErr := processQualitySSEEvent(strings.Join(dataLines, "\n"), &result, &modelAnswer, &firstTokenAt, &completed); processErr != nil {
			return finishUnknownQualityResult(result, "sse_event", processErr.Error())
		}
		if result.Classification == qualityClassificationQuota {
			return finishQuotaQualityResult(result, result.Detail)
		}
	}
	if !completed {
		return finishUnknownQualityResult(result, "sse_incomplete", "SSE 流未收到 response.completed 或 [DONE]")
	}
	if result.Classification == qualityClassificationQuota {
		return finishQuotaQualityResult(result, result.Detail)
	}
	finishedAt := time.Now()
	result.FinishedAt = finishedAt.UnixMilli()
	result.TotalMs = finishedAt.Sub(time.UnixMilli(result.StartedAt)).Milliseconds()
	if firstTokenAt.IsZero() {
		result.FirstTokenMs = result.TotalMs
	} else {
		result.FirstTokenMs = firstTokenAt.Sub(time.UnixMilli(result.StartedAt)).Milliseconds()
	}
	result.GenerationMs = result.TotalMs - result.FirstTokenMs
	if result.GenerationMs < 1 {
		result.GenerationMs = 1
	}
	result.OutputTokensPerSecond = float64(result.OutputTokens) * 1000 / float64(result.GenerationMs)
	result.AnswerMatched = strings.Contains(modelAnswer.String(), "391")
	if result.OutputTokensPerSecond >= settings.QualityHardTPS {
		result.Classification = qualityClassificationDegraded
		result.QualityLevel = qualityLevelHard
		result.ClassificationReason = "hard_tps"
		return result
	}
	if result.OutputTokensPerSecond >= settings.QualitySoftTPS {
		result.Classification = qualityClassificationDegraded
		result.QualityLevel = qualityLevelSoft
		result.ClassificationReason = "soft_tps"
		return result
	}
	result.Classification = qualityClassificationNormal
	result.QualityLevel = qualityLevelHealthy
	result.ClassificationReason = "within_threshold"
	return result
}

func processQualitySSEEvent(raw string, result *qualityProbeResult, modelAnswer *strings.Builder, firstTokenAt *time.Time, completed *bool) error {
	if raw == "[DONE]" {
		*completed = true
		return nil
	}
	if raw == "" {
		return nil
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		if isQuotaExhausted(raw) {
			result.Classification = qualityClassificationQuota
			result.QualityLevel = qualityLevelQuota
			result.ClassificationReason = "quota_exhausted"
			result.Detail = raw
			*completed = true
			return nil
		}
		return err
	}
	eventType := strings.ToLower(stringField(event, "type"))
	if isQuotaExhausted(raw) {
		result.Classification = qualityClassificationQuota
		result.QualityLevel = qualityLevelQuota
		result.ClassificationReason = "quota_exhausted"
		result.Detail = truncateProbeDetail(raw)
		*completed = true
		return nil
	}
	if strings.Contains(eventType, "failed") || strings.Contains(eventType, "incomplete") || eventType == "error" {
		result.ErrorCode = eventType
		result.Detail = truncateProbeDetail(raw)
		return fmt.Errorf("SSE 事件 %s", eventType)
	}
	delta := stringField(event, "delta")
	if strings.Contains(eventType, "output_text.delta") && delta != "" {
		modelAnswer.WriteString(delta)
	}
	if isQualityFirstTokenEvent(eventType, event) && delta != "" && firstTokenAt.IsZero() {
		*firstTokenAt = time.Now()
	}
	if eventType == "response.reasoning_summary_text.delta" ||
		eventType == "response.reasoning_text.delta" ||
		strings.EqualFold(stringField(event, "delta_type"), "thinking_delta") ||
		qualityDeltaTypeIsThinking(event) {
		result.ThinkingDelta = true
	}
	if eventType == "response.completed" {
		*completed = true
		updateQualityUsage(event, result)
	}
	return nil
}

func qualityDeltaTypeIsThinking(event map[string]any) bool {
	deltaObject, ok := event["delta"].(map[string]any)
	if !ok {
		return false
	}
	return strings.EqualFold(stringField(deltaObject, "type"), "thinking_delta")
}

func isQualityFirstTokenEvent(eventType string, event map[string]any) bool {
	switch eventType {
	case "response.output_text.delta",
		"response.reasoning_summary_text.delta",
		"response.reasoning_text.delta",
		"response.refusal.delta",
		"response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta":
		return true
	}
	if deltaObject, ok := event["delta"].(map[string]any); ok {
		return strings.EqualFold(stringField(deltaObject, "type"), "thinking_delta")
	}
	return false
}

func updateQualityUsage(event map[string]any, result *qualityProbeResult) {
	usage, ok := event["usage"].(map[string]any)
	if !ok {
		response, responseOK := event["response"].(map[string]any)
		if responseOK {
			usage, _ = response["usage"].(map[string]any)
		}
	}
	if usage == nil {
		return
	}
	result.OutputTokens = integerField(usage, "output_tokens")
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		result.ReasoningTokens = integerField(details, "reasoning_tokens")
	}
}

func finishQuotaQualityResult(result qualityProbeResult, detail string) qualityProbeResult {
	result.FinishedAt = time.Now().UnixMilli()
	result.TotalMs = result.FinishedAt - result.StartedAt
	result.Classification = qualityClassificationQuota
	result.QualityLevel = qualityLevelQuota
	result.ClassificationReason = "quota_exhausted"
	result.Detail = truncateProbeDetail(detail)
	return result
}

func finishUnknownQualityResult(result qualityProbeResult, errorCode, detail string) qualityProbeResult {
	result.FinishedAt = time.Now().UnixMilli()
	result.TotalMs = result.FinishedAt - result.StartedAt
	result.Classification = qualityClassificationUnknown
	result.QualityLevel = qualityLevelUnknown
	result.ClassificationReason = errorCode
	result.ErrorCode = errorCode
	result.Detail = truncateProbeDetail(detail)
	return result
}

func isQuotaExhausted(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "free-usage-exhausted") ||
		strings.Contains(lower, "used all the included free usage") ||
		strings.Contains(lower, "included free usage has been exhausted")
}
