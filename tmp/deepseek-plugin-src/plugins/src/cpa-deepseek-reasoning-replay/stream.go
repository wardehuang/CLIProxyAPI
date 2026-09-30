package main

import (
	"bytes"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type streamAccumulator struct {
	reasoning   strings.Builder
	toolCallIDs map[string]struct{}
	model       string
}

var streamAccumulatorState = struct {
	sync.Mutex
	byRequestID map[string]*streamAccumulator
}{
	byRequestID: make(map[string]*streamAccumulator),
}

func resetStreamAccumulators() {
	streamAccumulatorState.Lock()
	streamAccumulatorState.byRequestID = make(map[string]*streamAccumulator)
	streamAccumulatorState.Unlock()
}

func observeStreamChunk(req pluginapi.StreamChunkInterceptRequest, hostCallbackID string) pluginapi.StreamChunkInterceptResponse {
	if !pluginEnabled() {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if !shouldHandleModel(req.Model, req.RequestedModel) {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" || len(bytes.TrimSpace(req.Body)) == 0 {
		return pluginapi.StreamChunkInterceptResponse{}
	}

	acc := getOrCreateStreamAccumulator(requestID, req.Model)
	observeStreamPayload(acc, req.Body)
	if shouldFinalizeStreamPayload(req.Body) {
		finalizeStreamAccumulator(requestID, hostCallbackID)
	}
	return pluginapi.StreamChunkInterceptResponse{}
}

func completeRequestLifecycle(completion pluginapi.RequestCompletion, hostCallbackID string) {
	requestID := strings.TrimSpace(completion.RequestID)
	if requestID == "" {
		return
	}
	if completion.Outcome == pluginapi.RequestCompletionSucceeded {
		finalizeStreamAccumulator(requestID, hostCallbackID)
		return
	}
	dropStreamAccumulator(requestID)
}

func getOrCreateStreamAccumulator(requestID, model string) *streamAccumulator {
	streamAccumulatorState.Lock()
	defer streamAccumulatorState.Unlock()
	if acc, ok := streamAccumulatorState.byRequestID[requestID]; ok {
		return acc
	}
	acc := &streamAccumulator{
		toolCallIDs: make(map[string]struct{}),
		model:       model,
	}
	streamAccumulatorState.byRequestID[requestID] = acc
	return acc
}

func dropStreamAccumulator(requestID string) {
	streamAccumulatorState.Lock()
	delete(streamAccumulatorState.byRequestID, requestID)
	streamAccumulatorState.Unlock()
}

func finalizeStreamAccumulator(requestID, hostCallbackID string) {
	streamAccumulatorState.Lock()
	acc, ok := streamAccumulatorState.byRequestID[requestID]
	if ok {
		delete(streamAccumulatorState.byRequestID, requestID)
	}
	streamAccumulatorState.Unlock()
	if !ok || acc == nil {
		return
	}
	content := strings.TrimSpace(acc.reasoning.String())
	if content == "" || len(acc.toolCallIDs) == 0 {
		logPluginDebug(hostCallbackID, "deepseek stream reasoning cache skipped", map[string]any{
			"reason":        "incomplete_stream_state",
			"request_id":    requestID,
			"content_chars": len(content),
			"tool_call_ids": len(acc.toolCallIDs),
			"model":         acc.model,
		})
		return
	}
	ids := make([]string, 0, len(acc.toolCallIDs))
	for id := range acc.toolCallIDs {
		ids = append(ids, id)
	}
	ids = uniqueSortedStrings(ids)
	cacheKey := toolCallsCacheKey(ids)
	cacheReasoningContent(cacheKey, content)
	for _, id := range ids {
		cacheReasoningContent(cacheKeyPrefixToolCalls+id, content)
	}
	logPluginInfo(hostCallbackID, "deepseek stream reasoning content cached", map[string]any{
		"request_id":    requestID,
		"model":         acc.model,
		"cache_key":     cacheKey,
		"tool_call_ids": ids,
		"content_chars": len(content),
	})
}

func observeStreamPayload(acc *streamAccumulator, payload []byte) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return
	}
	// SSE may contain multiple lines/events in one chunk.
	for _, line := range bytes.Split(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(data, []byte("[DONE]")) {
				continue
			}
			observeStreamJSON(acc, data)
			continue
		}
		if line[0] == '{' {
			observeStreamJSON(acc, line)
		}
	}
}

func observeStreamJSON(acc *streamAccumulator, raw []byte) {
	root := gjson.ParseBytes(raw)
	if !root.Exists() {
		return
	}

	// OpenAI chat stream deltas.
	choices := root.Get("choices")
	if choices.IsArray() {
		for _, choice := range choices.Array() {
			delta := choice.Get("delta")
			if !delta.Exists() {
				delta = choice.Get("message")
			}
			appendReasoningText(acc, delta.Get("reasoning_content").String())
			appendReasoningText(acc, delta.Get("reasoning").String())
			collectToolCallIDs(acc, delta.Get("tool_calls"))
			collectToolCallIDs(acc, choice.Get("message.tool_calls"))
		}
	}

	// Responses stream events.
	eventType := strings.TrimSpace(root.Get("type").String())
	switch eventType {
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		appendReasoningText(acc, root.Get("delta").String())
	case "response.output_item.added", "response.output_item.done":
		item := root.Get("item")
		switch strings.TrimSpace(item.Get("type").String()) {
		case "reasoning":
			appendReasoningText(acc, extractResponsesReasoningText(item))
		case "function_call", "custom_tool_call":
			id := firstNonEmpty(item.Get("call_id").String(), item.Get("id").String())
			if id != "" {
				acc.toolCallIDs[strings.TrimSpace(id)] = struct{}{}
			}
		}
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		id := firstNonEmpty(root.Get("call_id").String(), root.Get("item_id").String())
		if id != "" {
			acc.toolCallIDs[strings.TrimSpace(id)] = struct{}{}
		}
	}

	// Claude-like stream blocks.
	if strings.TrimSpace(root.Get("content_block.type").String()) == "thinking" {
		appendReasoningText(acc, root.Get("delta.thinking").String())
		appendReasoningText(acc, root.Get("delta.text").String())
	}
	if strings.TrimSpace(root.Get("content_block.type").String()) == "tool_use" {
		id := strings.TrimSpace(root.Get("content_block.id").String())
		if id != "" {
			acc.toolCallIDs[id] = struct{}{}
		}
	}
}

func appendReasoningText(acc *streamAccumulator, text string) {
	if text == "" {
		return
	}
	// Keep token deltas byte-exact; only final cache value is trimmed.
	acc.reasoning.WriteString(text)
}

func collectToolCallIDs(acc *streamAccumulator, toolCalls gjson.Result) {
	if !toolCalls.IsArray() {
		return
	}
	for _, toolCall := range toolCalls.Array() {
		id := firstNonEmpty(toolCall.Get("id").String(), toolCall.Get("call_id").String())
		if id == "" {
			continue
		}
		acc.toolCallIDs[strings.TrimSpace(id)] = struct{}{}
	}
}

func shouldFinalizeStreamPayload(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return false
	}
	if bytes.Contains(trimmed, []byte("[DONE]")) {
		return true
	}
	root := gjson.ParseBytes(trimmed)
	if !root.Exists() {
		// Multi-line SSE: scan data lines.
		for _, line := range bytes.Split(payload, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(data, []byte("[DONE]")) {
				return true
			}
			item := gjson.ParseBytes(data)
			if streamEventFinished(item) {
				return true
			}
		}
		return false
	}
	return streamEventFinished(root)
}

func streamEventFinished(root gjson.Result) bool {
	if !root.Exists() {
		return false
	}
	eventType := strings.TrimSpace(root.Get("type").String())
	switch eventType {
	case "response.completed", "response.incomplete", "message_stop":
		return true
	}
	choices := root.Get("choices")
	if choices.IsArray() {
		for _, choice := range choices.Array() {
			finish := strings.TrimSpace(choice.Get("finish_reason").String())
			if finish != "" {
				return true
			}
		}
	}
	return false
}
