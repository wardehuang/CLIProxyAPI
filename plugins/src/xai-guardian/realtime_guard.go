package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	EncryptedBytes         int
	ReasoningTokens        int
	OutputTokens           int
	ReasoningItemID        string
	ReasoningItemCompleted bool
	ReasoningMetadataError bool
	CompletedFunctionCalls int
	FunctionCallNames      []string
	CompletedMessage       bool
	RefusalDetected        bool
	BurstDump              bool
	StreamError            string
	CompletedEvent         bool
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
	if evidence.StreamError != "" {
		return streamFailureDecision(store, completion)
	}
	mutation := completedMutationEvidence{}
	toolEvidence := evidence.CompletedFunctionCalls > 0 && !evidence.RefusalDetected
	if !toolEvidence && !isRealThinking(evidence, completion, settings) && evidence.CompletedMessage {
		mutation = scanCompletedMutationEvidence(completion.OriginalRequest)
	}

	reason, degraded := classifyStream(evidence, mutation, completion, settings)
	if !degraded {
		if authIndex != "" {
			if err := store.clearDegradationIfRecovered(authIndex); err != nil {
				return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: "state_write_failed", StatusCode: http.StatusInternalServerError}, err
			}
		}
		_ = store.appendLog(logLevelInfo, "guard.normal", "xAI 响应通过降智守护", reason)
		return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFlush}, nil
	}
	if authIndex == "" {
		return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: "missing_auth_index", StatusCode: http.StatusBadGateway}, nil
	}
	state, err := store.recordDegradation(authIndex, authName, completion.RequestID, reason)
	if err != nil {
		return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: "state_write_failed", StatusCode: http.StatusInternalServerError}, err
	}
	detail := fmt.Sprintf("count=%d summary_chars=%d encrypted_bytes=%d output_tokens=%d reasoning_tokens=%d", state.Count, evidence.SummaryChars, evidence.EncryptedBytes, evidence.OutputTokens, evidence.ReasoningTokens)
	_ = store.appendLog(logLevelWarn, "guard.degraded", "xAI 响应命中降智守护", reason+" "+detail)
	return pluginapi.XAIStreamCompletionResponse{
		Action:     pluginapi.XAIStreamActionRetry,
		RetryMode:  pluginapi.XAIStreamRetryModeReloadAndExcludeSelectedAuth,
		Reason:     reason,
		StatusCode: http.StatusBadGateway,
	}, nil
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
	if status == http.StatusTooManyRequests {
		_ = store.appendLog(logLevelError, "guard.rate_limited", "xAI 上游返回限流，拒绝在守护层换号", reason)
		return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionFail, Reason: "upstream_rate_limited", StatusCode: status}, nil
	}
	_ = store.appendLog(logLevelWarn, "guard.stream_failed", "xAI 流请求失败，交由核心重试链处理", reason)
	mode := pluginapi.XAIStreamRetryModeReloadSelectedAuth
	if isGuardTimeout(completion.Error) || status == http.StatusUnauthorized || status == http.StatusForbidden || status >= http.StatusInternalServerError {
		mode = pluginapi.XAIStreamRetryModeReloadAndExcludeSelectedAuth
	}
	return pluginapi.XAIStreamCompletionResponse{Action: pluginapi.XAIStreamActionRetry, RetryMode: mode, Reason: reason, StatusCode: status}, nil
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

func isGuardTimeout(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "first payload timeout") || strings.Contains(lower, "progress timeout") || strings.Contains(lower, "idle timeout") || strings.Contains(lower, "context deadline exceeded")
}

func classifyStream(evidence streamEvidence, mutation completedMutationEvidence, completion pluginapi.XAIStreamCompletionRequest, settings pluginSettings) (string, bool) {
	if evidence.BurstDump {
		return "burst_dump_disabled", false
	}
	if isBurstDump(evidence, completion, settings) {
		return "burst_dump_disabled", false
	}
	if evidence.CompletedFunctionCalls > 0 && !evidence.RefusalDetected {
		return "completed_tool_call_evidence", false
	}
	if isRealThinking(evidence, completion, settings) {
		return "thinking_evidence", false
	}
	if mutation.Found {
		return "completed_mutation_evidence", false
	}
	return "missing_thinking_without_action", true
}

func isRealThinking(evidence streamEvidence, completion pluginapi.XAIStreamCompletionRequest, settings pluginSettings) bool {
	if evidence.RefusalDetected {
		return false
	}
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
	if bytes.Contains(body, []byte("data:")) {
		for _, event := range splitSSEEvents(body) {
			if len(event.payload) == 0 || bytes.Equal(bytes.TrimSpace(event.payload), []byte("[DONE]")) {
				continue
			}
			value := decodePayload(event.payload)
			if value == nil {
				continue
			}
			applyStreamEvent(&evidence, event.name, value)
		}
		return evidence
	}
	value := decodePayload(body)
	if value != nil {
		applyStreamEvent(&evidence, valueType(value), value)
	}
	return evidence
}

type sseEvent struct {
	name    string
	payload []byte
}

func splitSSEEvents(body []byte) []sseEvent {
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	result := make([]sseEvent, 0)
	for _, block := range bytes.Split(normalized, []byte("\n\n")) {
		var name string
		var data []string
		scanner := bufio.NewScanner(bytes.NewReader(block))
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if len(data) > 0 {
			result = append(result, sseEvent{name: name, payload: []byte(strings.Join(data, "\n"))})
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
	if eventName == "" {
		eventName = valueType(value)
	}
	eventName = strings.ToLower(strings.TrimSpace(eventName))
	if strings.Contains(eventName, "failed") || strings.Contains(eventName, "error") {
		evidence.StreamError = eventName
	}
	if eventName == "response.incomplete" {
		evidence.StreamError = "response.incomplete"
	}
	if eventName == "response.completed" {
		evidence.CompletedEvent = true
	}
	if strings.Contains(stringifyLower(value), "burst_dump") {
		evidence.BurstDump = true
	}
	if strings.Contains(eventName, "function_call_arguments.done") {
		recordFunctionCall(evidence, object)
	}
	if strings.Contains(eventName, "reasoning") {
		if itemID, ok := object["item_id"]; ok {
			recordReasoningItemID(evidence, stringValue(itemID))
		}
	}
	if strings.Contains(eventName, "output_item.done") || strings.Contains(eventName, "output_item.added") {
		item := nestedObject(object, "item")
		if item == nil {
			item = nestedObject(object, "output_item")
		}
		if item == nil {
			item = object
		}
		recordOutputItem(evidence, item)
	}
	if strings.Contains(eventName, "reasoning_summary") || strings.Contains(eventName, "summary_text") || strings.Contains(eventName, "reasoning_text") {
		if strings.Contains(eventName, ".delta") {
			appendSummaryDelta(eventTextValue(object), evidence)
		} else {
			recordSummary(eventTextValue(object), evidence)
		}
	}
	if strings.Contains(eventName, "encrypted_content") {
		evidence.EncryptedBytes += eventEncryptedLength(object)
	}
	if eventName == "response.completed" || strings.HasPrefix(eventName, "response.") {
		response := nestedObject(object, "response")
		if response == nil {
			response = object
		}
		if status := strings.ToLower(stringValue(response["status"])); status == "incomplete" || status == "failed" {
			evidence.StreamError = status
		}
		if output, ok := response["output"].([]any); ok {
			for _, rawItem := range output {
				item, ok := rawItem.(map[string]any)
				if ok {
					recordOutputItem(evidence, item)
				}
			}
		}
		readUsage(evidence, response["usage"])
		readUsage(evidence, object["usage"])
	}
	if strings.Contains(eventName, "usage") {
		readUsage(evidence, object["usage"])
	}
	if hasNonEmptyRefusal(value) {
		evidence.RefusalDetected = true
	}
}

func recordOutputItem(evidence *streamEvidence, item map[string]any) {
	typeName := strings.ToLower(stringValue(item["type"]))
	status := strings.ToLower(stringValue(item["status"]))
	if typeName == "reasoning" {
		recordReasoningItemID(evidence, stringValue(item["id"]))
		if status == "completed" {
			evidence.ReasoningItemCompleted = true
		}
	}
	if typeName == "message" && status == "completed" && strings.TrimSpace(stringValue(item["id"])) != "" {
		evidence.CompletedMessage = true
	}
	if typeName == "function_call" && status == "completed" {
		recordFunctionCall(evidence, item)
	}
	if typeName == "reasoning" {
		if summary := item["summary"]; summary != nil {
			recordSummary(summary, evidence)
		}
		if encrypted := item["encrypted_content"]; encrypted != nil {
			evidence.EncryptedBytes = maxInt(evidence.EncryptedBytes, encryptedLength(encrypted))
		}
	}
	if typeName == "message" {
		if content, ok := item["content"].([]any); ok {
			for _, rawContent := range content {
				contentItem, ok := rawContent.(map[string]any)
				if !ok {
					continue
				}
				if strings.Contains(strings.ToLower(stringValue(contentItem["type"])), "refusal") && strings.TrimSpace(stringValue(contentItem["refusal"])) != "" {
					evidence.RefusalDetected = true
				}
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
	arguments := object["arguments"]
	if callID == "" || name == "" || !hasMutationArguments(arguments) {
		return
	}
	for _, existing := range evidence.FunctionCallNames {
		if existing == callID {
			return
		}
	}
	evidence.CompletedFunctionCalls++
	evidence.FunctionCallNames = append(evidence.FunctionCallNames, callID)
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
	evidence.ReasoningTokens = maxInt(evidence.ReasoningTokens, integerValue(usage["reasoning_tokens"]))
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

func eventEncryptedLength(object map[string]any) int {
	if value := object["encrypted_content"]; value != nil {
		return encryptedLength(value)
	}
	return encryptedLength(object["delta"])
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

func encryptedLength(value any) int {
	if value == nil {
		return 0
	}
	if text, ok := value.(string); ok {
		return len([]byte(text))
	}
	return textLength(value)
}

func hasNonEmptyRefusal(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.Contains(strings.ToLower(key), "refusal") && (strings.TrimSpace(stringValue(child)) != "" || strings.TrimSpace(summaryText(child)) != "" || hasNonEmptyRefusal(child)) {
				return true
			}
			if hasNonEmptyRefusal(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if hasNonEmptyRefusal(child) {
				return true
			}
		}
	}
	return false
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
