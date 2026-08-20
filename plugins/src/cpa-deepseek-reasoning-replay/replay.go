package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	cacheKeyPrefixToolCalls = "toolcalls:"
	defaultPadReason        = "pad"
	cacheHitReason          = "cache"
)

type injectStats struct {
	Changed       bool
	InjectedCache int
	InjectedPad   int
	Skipped       int
	ToolCallKeys  []string
}

type extractedReasoning struct {
	Content     string
	ToolCallIDs []string
	CacheKey    string
}

func finalizeDeepseekRequest(req pluginapi.RequestFinalizeRequest, hostCallbackID string) pluginapi.RequestFinalizeResponse {
	started := time.Now()
	bodyShape := describeBodyShape(req.Body)
	if !pluginEnabled() {
		logPluginDebug(hostCallbackID, "request finalize skipped", map[string]any{
			"reason":     "plugin_disabled",
			"model":      req.Model,
			"elapsed_ms": elapsedMS(started),
		})
		return pluginapi.RequestFinalizeResponse{}
	}
	if !shouldHandleModel(req.Model, req.RequestedModel) {
		logPluginDebug(hostCallbackID, "request finalize skipped", map[string]any{
			"reason":          "model_mismatch",
			"model":           req.Model,
			"requested_model": req.RequestedModel,
			"source_format":   req.SourceFormat,
			"to_format":       req.ToFormat,
			"body_shape":      bodyShape,
			"body_bytes":      len(req.Body),
			"elapsed_ms":      elapsedMS(started),
		})
		return pluginapi.RequestFinalizeResponse{}
	}
	if !isOpenAIChatBody(req.Body) {
		logPluginDebug(hostCallbackID, "request finalize skipped", map[string]any{
			"reason":          "not_openai_chat_body",
			"model":           req.Model,
			"requested_model": req.RequestedModel,
			"source_format":   req.SourceFormat,
			"to_format":       req.ToFormat,
			"body_shape":      bodyShape,
			"body_bytes":      len(req.Body),
			"body_preview":    previewBytes(req.Body, 240),
			"elapsed_ms":      elapsedMS(started),
			"note":            "responses/claude bodies are intentionally ignored here",
		})
		return pluginapi.RequestFinalizeResponse{}
	}

	updated, stats := injectMissingReasoningContent(req.Body, padPlaceholder())
	if !stats.Changed {
		logPluginDebug(hostCallbackID, "request finalize skipped", map[string]any{
			"reason":          "no_missing_reasoning",
			"model":           req.Model,
			"requested_model": req.RequestedModel,
			"source_format":   req.SourceFormat,
			"to_format":       req.ToFormat,
			"skipped":         stats.Skipped,
			"messages":        gjson.GetBytes(req.Body, "messages.#").Int(),
			"body_shape":      bodyShape,
			"elapsed_ms":      elapsedMS(started),
		})
		return pluginapi.RequestFinalizeResponse{}
	}

	logPluginInfo(hostCallbackID, "deepseek reasoning content injected", map[string]any{
		"model":           req.Model,
		"requested_model": req.RequestedModel,
		"source_format":   req.SourceFormat,
		"to_format":       req.ToFormat,
		"injected_cache":  stats.InjectedCache,
		"injected_pad":    stats.InjectedPad,
		"tool_call_keys":  stats.ToolCallKeys,
		"body_bytes_in":   len(req.Body),
		"body_bytes_out":  len(updated),
		"elapsed_ms":      elapsedMS(started),
	})
	return pluginapi.RequestFinalizeResponse{Body: updated}
}

func cacheFromResponseBody(req pluginapi.ResponseInterceptRequest, hostCallbackID string) pluginapi.ResponseInterceptResponse {
	started := time.Now()
	bodyShape := describeBodyShape(req.Body)
	if !pluginEnabled() {
		logPluginDebug(hostCallbackID, "response intercept skipped", map[string]any{
			"reason":     "plugin_disabled",
			"elapsed_ms": elapsedMS(started),
		})
		return pluginapi.ResponseInterceptResponse{}
	}
	if !shouldHandleModel(req.Model, req.RequestedModel) {
		logPluginDebug(hostCallbackID, "response intercept skipped", map[string]any{
			"reason":          "model_mismatch",
			"model":           req.Model,
			"requested_model": req.RequestedModel,
			"elapsed_ms":      elapsedMS(started),
		})
		return pluginapi.ResponseInterceptResponse{}
	}
	if req.StatusCode < 200 || req.StatusCode >= 300 {
		logPluginDebug(hostCallbackID, "response intercept skipped", map[string]any{
			"reason":      "non_2xx",
			"status_code": req.StatusCode,
			"model":       req.Model,
			"elapsed_ms":  elapsedMS(started),
		})
		return pluginapi.ResponseInterceptResponse{}
	}

	extracted, ok := extractReasoningFromAnyBody(req.Body)
	if !ok {
		logPluginDebug(hostCallbackID, "response intercept cache skipped", map[string]any{
			"reason":         "no_reasoning_or_tool_calls",
			"model":          req.Model,
			"requested_model": req.RequestedModel,
			"source_format":  req.SourceFormat,
			"status_code":    req.StatusCode,
			"body_shape":     bodyShape,
			"response_bytes": len(req.Body),
			"body_preview":   previewBytes(req.Body, 240),
			"elapsed_ms":     elapsedMS(started),
			"body_rewrite":   false,
		})
		return pluginapi.ResponseInterceptResponse{}
	}
	cacheReasoningContent(extracted.CacheKey, extracted.Content)
	for _, toolCallID := range extracted.ToolCallIDs {
		cacheReasoningContent(cacheKeyPrefixToolCalls+toolCallID, extracted.Content)
	}
	logPluginInfo(hostCallbackID, "deepseek reasoning content cached", map[string]any{
		"model":          req.Model,
		"cache_key":      extracted.CacheKey,
		"tool_call_ids":  extracted.ToolCallIDs,
		"content_chars":  len(extracted.Content),
		"source_format":  req.SourceFormat,
		"response_bytes": len(req.Body),
		"body_shape":     bodyShape,
		"elapsed_ms":     elapsedMS(started),
		"body_rewrite":   false,
	})
	return pluginapi.ResponseInterceptResponse{}
}

func describeBodyShape(body []byte) map[string]any {
	shape := map[string]any{
		"bytes": len(body),
	}
	if len(bytes.TrimSpace(body)) == 0 {
		shape["empty"] = true
		return shape
	}
	root := gjson.ParseBytes(body)
	if !root.Exists() {
		shape["json"] = false
		return shape
	}
	shape["json"] = true
	for _, key := range []string{"messages", "input", "output", "choices", "content", "model", "stream", "type"} {
		if root.Get(key).Exists() {
			shape["has_"+key] = true
		}
	}
	if root.Get("messages").IsArray() {
		shape["messages_count"] = root.Get("messages.#").Int()
	}
	if root.Get("input").IsArray() {
		shape["input_count"] = root.Get("input.#").Int()
	}
	if root.Get("tools").IsArray() {
		shape["tools_count"] = root.Get("tools.#").Int()
	}
	return shape
}

func shouldHandleModel(model, requestedModel string) bool {
	return modelMatches(model) || modelMatches(requestedModel)
}

func isOpenAIChatBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	messages := gjson.GetBytes(body, "messages")
	return messages.Exists() && messages.IsArray()
}

func injectMissingReasoningContent(body []byte, placeholder string) ([]byte, injectStats) {
	stats := injectStats{}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, stats
	}
	updated := body
	changed := false
	for index, message := range messages.Array() {
		if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "assistant") {
			continue
		}
		toolCalls := message.Get("tool_calls")
		if !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
			continue
		}
		existing := strings.TrimSpace(message.Get("reasoning_content").String())
		if existing != "" {
			stats.Skipped++
			continue
		}
		toolCallIDs := extractToolCallIDs(toolCalls)
		if len(toolCallIDs) == 0 {
			stats.Skipped++
			continue
		}
		cacheKey := toolCallsCacheKey(toolCallIDs)
		content, reason := resolveInjectContent(cacheKey, toolCallIDs, placeholder)
		path := fmt.Sprintf("messages.%d.reasoning_content", index)
		next, errSet := sjson.SetBytes(updated, path, content)
		if errSet != nil {
			stats.Skipped++
			continue
		}
		updated = next
		changed = true
		stats.ToolCallKeys = append(stats.ToolCallKeys, cacheKey)
		if reason == cacheHitReason {
			stats.InjectedCache++
		} else {
			stats.InjectedPad++
		}
	}
	stats.Changed = changed
	return updated, stats
}

func resolveInjectContent(cacheKey string, toolCallIDs []string, placeholder string) (string, string) {
	if content, ok := lookupReasoningContent(cacheKey); ok {
		return content, cacheHitReason
	}
	for _, toolCallID := range toolCallIDs {
		if content, ok := lookupReasoningContent(cacheKeyPrefixToolCalls + toolCallID); ok {
			return content, cacheHitReason
		}
	}
	if placeholder == "" {
		placeholder = " "
	}
	return placeholder, defaultPadReason
}

func extractToolCallIDs(toolCalls gjson.Result) []string {
	if !toolCalls.IsArray() {
		return nil
	}
	ids := make([]string, 0, len(toolCalls.Array()))
	for _, toolCall := range toolCalls.Array() {
		id := strings.TrimSpace(toolCall.Get("id").String())
		if id == "" {
			id = strings.TrimSpace(toolCall.Get("call_id").String())
		}
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	return uniqueSortedStrings(ids)
}

func toolCallsCacheKey(toolCallIDs []string) string {
	ids := uniqueSortedStrings(toolCallIDs)
	if len(ids) == 0 {
		return ""
	}
	return cacheKeyPrefixToolCalls + strings.Join(ids, ",")
}

func extractReasoningFromAnyBody(body []byte) (extractedReasoning, bool) {
	if extracted, ok := extractReasoningFromOpenAIChat(body); ok {
		return extracted, true
	}
	if extracted, ok := extractReasoningFromResponses(body); ok {
		return extracted, true
	}
	if extracted, ok := extractReasoningFromClaude(body); ok {
		return extracted, true
	}
	return extractedReasoning{}, false
}

func extractReasoningFromOpenAIChat(body []byte) (extractedReasoning, bool) {
	choices := gjson.GetBytes(body, "choices")
	if !choices.IsArray() || len(choices.Array()) == 0 {
		// Some non-stream wrappers put message at top level.
		message := gjson.GetBytes(body, "message")
		if message.Exists() {
			return extractReasoningFromOpenAIMessage(message)
		}
		return extractedReasoning{}, false
	}
	for _, choice := range choices.Array() {
		if extracted, ok := extractReasoningFromOpenAIMessage(choice.Get("message")); ok {
			return extracted, true
		}
	}
	return extractedReasoning{}, false
}

func extractReasoningFromOpenAIMessage(message gjson.Result) (extractedReasoning, bool) {
	if !message.Exists() {
		return extractedReasoning{}, false
	}
	content := firstNonEmpty(
		strings.TrimSpace(message.Get("reasoning_content").String()),
		strings.TrimSpace(message.Get("reasoning").String()),
	)
	toolCallIDs := extractToolCallIDs(message.Get("tool_calls"))
	if content == "" || len(toolCallIDs) == 0 {
		return extractedReasoning{}, false
	}
	return extractedReasoning{
		Content:     content,
		ToolCallIDs: toolCallIDs,
		CacheKey:    toolCallsCacheKey(toolCallIDs),
	}, true
}

func extractReasoningFromResponses(body []byte) (extractedReasoning, bool) {
	output := gjson.GetBytes(body, "output")
	if !output.IsArray() {
		// Some envelopes nest under response.output
		output = gjson.GetBytes(body, "response.output")
	}
	if !output.IsArray() {
		return extractedReasoning{}, false
	}
	var reasoningParts []string
	var toolCallIDs []string
	for _, item := range output.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "reasoning":
			text := extractResponsesReasoningText(item)
			if text != "" {
				reasoningParts = append(reasoningParts, text)
			}
		case "function_call", "custom_tool_call":
			id := firstNonEmpty(
				strings.TrimSpace(item.Get("call_id").String()),
				strings.TrimSpace(item.Get("id").String()),
			)
			if id != "" {
				toolCallIDs = append(toolCallIDs, id)
			}
		}
	}
	content := strings.TrimSpace(strings.Join(reasoningParts, "\n"))
	toolCallIDs = uniqueSortedStrings(toolCallIDs)
	if content == "" || len(toolCallIDs) == 0 {
		return extractedReasoning{}, false
	}
	return extractedReasoning{
		Content:     content,
		ToolCallIDs: toolCallIDs,
		CacheKey:    toolCallsCacheKey(toolCallIDs),
	}, true
}

func extractResponsesReasoningText(item gjson.Result) string {
	if encrypted := strings.TrimSpace(item.Get("encrypted_content").String()); encrypted != "" {
		// Encrypted blobs are not useful for DeepSeek chat reasoning_content echo.
		// Prefer summary text when present.
	}
	summary := item.Get("summary")
	if summary.IsArray() {
		parts := make([]string, 0, len(summary.Array()))
		for _, part := range summary.Array() {
			text := strings.TrimSpace(part.Get("text").String())
			if text == "" {
				continue
			}
			parts = append(parts, text)
		}
		if joined := strings.TrimSpace(strings.Join(parts, "\n")); joined != "" {
			return joined
		}
	}
	return firstNonEmpty(
		strings.TrimSpace(item.Get("content").String()),
		strings.TrimSpace(item.Get("text").String()),
	)
}

func extractReasoningFromClaude(body []byte) (extractedReasoning, bool) {
	content := gjson.GetBytes(body, "content")
	if !content.IsArray() {
		return extractedReasoning{}, false
	}
	var reasoningParts []string
	var toolCallIDs []string
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "thinking", "redacted_thinking":
			text := strings.TrimSpace(part.Get("thinking").String())
			if text == "" {
				text = strings.TrimSpace(part.Get("text").String())
			}
			if text != "" {
				reasoningParts = append(reasoningParts, text)
			}
		case "tool_use":
			id := strings.TrimSpace(part.Get("id").String())
			if id != "" {
				toolCallIDs = append(toolCallIDs, id)
			}
		}
	}
	joined := strings.TrimSpace(strings.Join(reasoningParts, "\n"))
	toolCallIDs = uniqueSortedStrings(toolCallIDs)
	if joined == "" || len(toolCallIDs) == 0 {
		return extractedReasoning{}, false
	}
	return extractedReasoning{
		Content:     joined,
		ToolCallIDs: toolCallIDs,
		CacheKey:    toolCallsCacheKey(toolCallIDs),
	}, true
}

func uniqueSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
