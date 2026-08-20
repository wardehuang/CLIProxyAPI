package main

import (
	"bytes"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type streamAccumulator struct {
	reasoning      strings.Builder
	toolCallIDs    map[string]struct{}
	model          string
	requestedModel string
	sourceFormat   string
	startedAt      time.Time
	lastChunkAt    time.Time
	chunkCount     int
	bodyBytesTotal int
	eventCounts    map[string]int
	sawMessageStop bool
	sawMessageDelta bool
	sawContentStop bool
	sawDone        bool
	sawUsage       bool
	sawFinish      bool
	lastEventTypes []string
	lastBodyPreview string
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
	started := time.Now()
	// Hard guarantee: this interceptor never rewrites or drops chunks.
	passThrough := pluginapi.StreamChunkInterceptResponse{}

	if !pluginEnabled() {
		logPluginDebug(hostCallbackID, "stream chunk skipped", map[string]any{
			"reason":      "plugin_disabled",
			"request_id":  strings.TrimSpace(req.RequestID),
			"chunk_index": req.ChunkIndex,
			"elapsed_ms":  elapsedMS(started),
		})
		return passThrough
	}
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		logPluginDebug(hostCallbackID, "stream header init observed", map[string]any{
			"request_id":      strings.TrimSpace(req.RequestID),
			"model":           req.Model,
			"requested_model": req.RequestedModel,
			"source_format":   req.SourceFormat,
			"history_chunks":  len(req.HistoryChunks),
			"req_body_bytes":  len(req.RequestBody),
			"orig_body_bytes": len(req.OriginalRequest),
			"elapsed_ms":      elapsedMS(started),
			"drop_chunk":      false,
			"body_rewrite":    false,
		})
		return passThrough
	}
	if !shouldHandleModel(req.Model, req.RequestedModel) {
		logPluginDebug(hostCallbackID, "stream chunk skipped", map[string]any{
			"reason":          "model_mismatch",
			"request_id":      strings.TrimSpace(req.RequestID),
			"chunk_index":     req.ChunkIndex,
			"model":           req.Model,
			"requested_model": req.RequestedModel,
			"elapsed_ms":      elapsedMS(started),
		})
		return passThrough
	}

	requestID := strings.TrimSpace(req.RequestID)
	body := req.Body
	bodyPreview := previewBytes(body, 320)
	eventTypes, flags := classifyStreamPayload(body)

	if requestID == "" || len(bytes.TrimSpace(body)) == 0 {
		logPluginDebug(hostCallbackID, "stream chunk skipped", map[string]any{
			"reason":       "missing_request_id_or_body",
			"request_id":   requestID,
			"chunk_index":  req.ChunkIndex,
			"body_bytes":   len(body),
			"event_types":  eventTypes,
			"body_preview": bodyPreview,
			"elapsed_ms":   elapsedMS(started),
			"drop_chunk":   false,
			"body_rewrite": false,
		})
		return passThrough
	}

	acc := getOrCreateStreamAccumulator(requestID, req.Model, req.RequestedModel, req.SourceFormat)
	observeStreamPayload(acc, body)
	acc.chunkCount++
	acc.bodyBytesTotal += len(body)
	acc.lastChunkAt = time.Now()
	acc.lastEventTypes = eventTypes
	acc.lastBodyPreview = bodyPreview
	for _, eventType := range eventTypes {
		acc.eventCounts[eventType]++
	}
	if flags.hasMessageStop {
		acc.sawMessageStop = true
	}
	if flags.hasMessageDelta {
		acc.sawMessageDelta = true
	}
	if flags.hasContentBlockStop {
		acc.sawContentStop = true
	}
	if flags.hasDone {
		acc.sawDone = true
	}
	if flags.hasUsage {
		acc.sawUsage = true
	}
	if flags.hasFinishReason {
		acc.sawFinish = true
	}

	terminal := shouldFinalizeStreamPayload(body)
	shouldLogChunk := pluginDebugEnabled() && (acc.chunkCount <= 3 || terminal || flags.hasMessageStop || flags.hasMessageDelta || flags.hasContentBlockStop || flags.hasDone || flags.hasUsage || flags.hasFinishReason)
	if shouldLogChunk {
		logPluginDebug(hostCallbackID, "stream chunk observed", map[string]any{
			"request_id":             requestID,
			"chunk_index":            req.ChunkIndex,
			"model":                  req.Model,
			"requested_model":        req.RequestedModel,
			"source_format":          req.SourceFormat,
			"body_bytes":             len(body),
			"history_chunks":         len(req.HistoryChunks),
			"event_types":            eventTypes,
			"has_message_stop":       flags.hasMessageStop,
			"has_message_delta":      flags.hasMessageDelta,
			"has_content_block_stop": flags.hasContentBlockStop,
			"has_done":               flags.hasDone,
			"has_usage":              flags.hasUsage,
			"has_finish_reason":      flags.hasFinishReason,
			"terminal_payload":       terminal,
			"acc_chunks":             acc.chunkCount,
			"acc_reasoning_chars":    acc.reasoning.Len(),
			"acc_tool_call_ids":      len(acc.toolCallIDs),
			"acc_saw_message_stop":   acc.sawMessageStop,
			"body_preview":           bodyPreview,
			"elapsed_ms":             elapsedMS(started),
			"drop_chunk":             false,
			"body_rewrite":           false,
			"pass_through":           true,
		})
	}

	if terminal {
		logPluginDebug(hostCallbackID, "stream terminal payload seen", map[string]any{
			"request_id":             requestID,
			"chunk_index":            req.ChunkIndex,
			"event_types":            eventTypes,
			"has_message_stop":       flags.hasMessageStop,
			"has_message_delta":      flags.hasMessageDelta,
			"has_content_block_stop": flags.hasContentBlockStop,
			"has_done":               flags.hasDone,
			"has_usage":              flags.hasUsage,
			"acc_event_counts":       cloneEventCounts(acc.eventCounts),
			"acc_saw_message_stop":   acc.sawMessageStop,
			"body_preview":           bodyPreview,
			"elapsed_ms":             elapsedMS(started),
		})
		finalizeStreamAccumulator(requestID, hostCallbackID, "terminal_payload")
	}
	return passThrough
}

func completeRequestLifecycle(completion pluginapi.RequestCompletion, hostCallbackID string) {
	requestID := strings.TrimSpace(completion.RequestID)
	if requestID == "" {
		logPluginDebug(hostCallbackID, "request lifecycle skipped", map[string]any{
			"reason":  "missing_request_id",
			"outcome": string(completion.Outcome),
		})
		return
	}

	summary := snapshotStreamAccumulator(requestID)
	logPluginDebug(hostCallbackID, "request lifecycle complete", map[string]any{
		"request_id":             requestID,
		"outcome":                string(completion.Outcome),
		"status_code":            completion.StatusCode,
		"error":                  strings.TrimSpace(completion.Error),
		"model":                  completion.Model,
		"requested_model":        completion.RequestedModel,
		"source_format":          completion.SourceFormat,
		"stream":                 completion.Stream,
		"acc_present":            summary != nil,
		"acc_chunks":             valueOrZero(summary, func(a *streamAccumulator) int { return a.chunkCount }),
		"acc_body_bytes_total":   valueOrZero(summary, func(a *streamAccumulator) int { return a.bodyBytesTotal }),
		"acc_event_counts":       valueOrNil(summary, func(a *streamAccumulator) any { return cloneEventCounts(a.eventCounts) }),
		"acc_saw_message_stop":   valueOrBool(summary, func(a *streamAccumulator) bool { return a.sawMessageStop }),
		"acc_saw_message_delta":  valueOrBool(summary, func(a *streamAccumulator) bool { return a.sawMessageDelta }),
		"acc_saw_content_stop":   valueOrBool(summary, func(a *streamAccumulator) bool { return a.sawContentStop }),
		"acc_saw_done":           valueOrBool(summary, func(a *streamAccumulator) bool { return a.sawDone }),
		"acc_saw_usage":          valueOrBool(summary, func(a *streamAccumulator) bool { return a.sawUsage }),
		"acc_saw_finish":         valueOrBool(summary, func(a *streamAccumulator) bool { return a.sawFinish }),
		"acc_reasoning_chars":    valueOrZero(summary, func(a *streamAccumulator) int { return a.reasoning.Len() }),
		"acc_tool_call_ids":      valueOrZero(summary, func(a *streamAccumulator) int { return len(a.toolCallIDs) }),
		"acc_last_event_types":   valueOrNil(summary, func(a *streamAccumulator) any { return a.lastEventTypes }),
		"acc_last_body_preview":  valueOrString(summary, func(a *streamAccumulator) string { return a.lastBodyPreview }),
		"acc_duration_ms":        valueOrZero64(summary, func(a *streamAccumulator) int64 { return elapsedMS(a.startedAt) }),
		"missing_message_stop":   summary != nil && summary.chunkCount > 0 && !summary.sawMessageStop,
	})

	if summary != nil && summary.chunkCount > 0 && !summary.sawMessageStop {
		logPluginWarn(hostCallbackID, "deepseek stream ended without message_stop in observed chunks", map[string]any{
			"request_id":           requestID,
			"outcome":              string(completion.Outcome),
			"acc_chunks":           summary.chunkCount,
			"acc_event_counts":     cloneEventCounts(summary.eventCounts),
			"acc_saw_message_delta": summary.sawMessageDelta,
			"acc_saw_content_stop": summary.sawContentStop,
			"acc_saw_done":         summary.sawDone,
			"acc_saw_usage":        summary.sawUsage,
			"acc_saw_finish":       summary.sawFinish,
			"acc_last_event_types": summary.lastEventTypes,
			"acc_last_body_preview": summary.lastBodyPreview,
			"note":                 "plugin is pass-through; missing message_stop means host/upstream path never delivered it to interceptor",
		})
	}

	if completion.Outcome == pluginapi.RequestCompletionSucceeded {
		finalizeStreamAccumulator(requestID, hostCallbackID, "lifecycle_succeeded")
		return
	}
	dropStreamAccumulator(requestID)
	logPluginDebug(hostCallbackID, "stream accumulator dropped", map[string]any{
		"request_id": requestID,
		"reason":     "lifecycle_" + string(completion.Outcome),
	})
}

func getOrCreateStreamAccumulator(requestID, model, requestedModel, sourceFormat string) *streamAccumulator {
	streamAccumulatorState.Lock()
	defer streamAccumulatorState.Unlock()
	if acc, ok := streamAccumulatorState.byRequestID[requestID]; ok {
		if acc.model == "" {
			acc.model = model
		}
		if acc.requestedModel == "" {
			acc.requestedModel = requestedModel
		}
		if acc.sourceFormat == "" {
			acc.sourceFormat = sourceFormat
		}
		return acc
	}
	acc := &streamAccumulator{
		toolCallIDs:    make(map[string]struct{}),
		eventCounts:    make(map[string]int),
		model:          model,
		requestedModel: requestedModel,
		sourceFormat:   sourceFormat,
		startedAt:      time.Now(),
	}
	streamAccumulatorState.byRequestID[requestID] = acc
	return acc
}

func snapshotStreamAccumulator(requestID string) *streamAccumulator {
	streamAccumulatorState.Lock()
	defer streamAccumulatorState.Unlock()
	acc, ok := streamAccumulatorState.byRequestID[requestID]
	if !ok || acc == nil {
		return nil
	}
	// Return a shallow copy so later deletes do not race log readers.
	clone := *acc
	clone.eventCounts = cloneEventCounts(acc.eventCounts)
	clone.toolCallIDs = make(map[string]struct{}, len(acc.toolCallIDs))
	for id := range acc.toolCallIDs {
		clone.toolCallIDs[id] = struct{}{}
	}
	if len(acc.lastEventTypes) > 0 {
		clone.lastEventTypes = append([]string(nil), acc.lastEventTypes...)
	}
	return &clone
}

func dropStreamAccumulator(requestID string) {
	streamAccumulatorState.Lock()
	delete(streamAccumulatorState.byRequestID, requestID)
	streamAccumulatorState.Unlock()
}

func finalizeStreamAccumulator(requestID, hostCallbackID, trigger string) {
	streamAccumulatorState.Lock()
	acc, ok := streamAccumulatorState.byRequestID[requestID]
	if ok {
		delete(streamAccumulatorState.byRequestID, requestID)
	}
	streamAccumulatorState.Unlock()
	if !ok || acc == nil {
		logPluginDebug(hostCallbackID, "stream finalize skipped", map[string]any{
			"reason":     "accumulator_missing",
			"request_id": requestID,
			"trigger":    trigger,
		})
		return
	}
	content := strings.TrimSpace(acc.reasoning.String())
	if content == "" || len(acc.toolCallIDs) == 0 {
		logPluginDebug(hostCallbackID, "deepseek stream reasoning cache skipped", map[string]any{
			"reason":               "incomplete_stream_state",
			"request_id":           requestID,
			"trigger":              trigger,
			"content_chars":        len(content),
			"tool_call_ids":        len(acc.toolCallIDs),
			"model":                acc.model,
			"requested_model":      acc.requestedModel,
			"source_format":        acc.sourceFormat,
			"acc_chunks":           acc.chunkCount,
			"acc_event_counts":     cloneEventCounts(acc.eventCounts),
			"acc_saw_message_stop": acc.sawMessageStop,
			"acc_saw_usage":        acc.sawUsage,
			"acc_saw_done":         acc.sawDone,
			"duration_ms":          elapsedMS(acc.startedAt),
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
		"request_id":           requestID,
		"trigger":              trigger,
		"model":                acc.model,
		"requested_model":      acc.requestedModel,
		"source_format":        acc.sourceFormat,
		"cache_key":            cacheKey,
		"tool_call_ids":        ids,
		"content_chars":        len(content),
		"acc_chunks":           acc.chunkCount,
		"acc_event_counts":     cloneEventCounts(acc.eventCounts),
		"acc_saw_message_stop": acc.sawMessageStop,
		"duration_ms":          elapsedMS(acc.startedAt),
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
	if strings.TrimSpace(root.Get("delta.type").String()) == "thinking_delta" {
		appendReasoningText(acc, root.Get("delta.thinking").String())
	}
	if strings.EqualFold(strings.TrimSpace(root.Get("type").String()), "content_block_start") &&
		strings.TrimSpace(root.Get("content_block.type").String()) == "tool_use" {
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

type streamPayloadFlags struct {
	hasMessageStop      bool
	hasMessageDelta     bool
	hasContentBlockStop bool
	hasDone             bool
	hasUsage            bool
	hasFinishReason     bool
}

func classifyStreamPayload(payload []byte) ([]string, streamPayloadFlags) {
	var flags streamPayloadFlags
	eventTypes := make([]string, 0, 4)
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, flags
	}
	if bytes.Contains(trimmed, []byte("[DONE]")) {
		flags.hasDone = true
		eventTypes = append(eventTypes, "[DONE]")
	}

	scanJSON := func(raw []byte) {
		root := gjson.ParseBytes(raw)
		if !root.Exists() {
			return
		}
		eventType := strings.TrimSpace(root.Get("type").String())
		if eventType != "" {
			eventTypes = append(eventTypes, eventType)
			switch eventType {
			case "message_stop":
				flags.hasMessageStop = true
			case "message_delta":
				flags.hasMessageDelta = true
			case "content_block_stop":
				flags.hasContentBlockStop = true
			}
		}
		if root.Get("usage").Exists() || root.Get("response.usage").Exists() || root.Get("message.usage").Exists() {
			flags.hasUsage = true
			eventTypes = append(eventTypes, "usage")
		}
		choices := root.Get("choices")
		if choices.IsArray() {
			for _, choice := range choices.Array() {
				if strings.TrimSpace(choice.Get("finish_reason").String()) != "" {
					flags.hasFinishReason = true
					eventTypes = append(eventTypes, "finish_reason="+strings.TrimSpace(choice.Get("finish_reason").String()))
				}
				if choice.Get("usage").Exists() {
					flags.hasUsage = true
				}
			}
		}
		if root.Get("delta.stop_reason").Exists() && root.Get("delta.stop_reason").Type != gjson.Null {
			eventTypes = append(eventTypes, "stop_reason="+root.Get("delta.stop_reason").String())
		}
	}

	if root := gjson.ParseBytes(trimmed); root.Exists() && (root.IsObject() || root.IsArray()) {
		scanJSON(trimmed)
	} else {
		for _, line := range bytes.Split(payload, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if bytes.HasPrefix(line, []byte("event:")) {
				eventTypes = append(eventTypes, "event:"+strings.TrimSpace(string(line[len("event:"):])))
				continue
			}
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(data, []byte("[DONE]")) {
				flags.hasDone = true
				continue
			}
			scanJSON(data)
		}
	}
	return uniquePreserveOrder(eventTypes), flags
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

func cloneEventCounts(in map[string]int) map[string]int {
	if len(in) == 0 {
		return map[string]int{}
	}
	out := make(map[string]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func uniquePreserveOrder(values []string) []string {
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
	return out
}

func valueOrZero(acc *streamAccumulator, fn func(*streamAccumulator) int) int {
	if acc == nil {
		return 0
	}
	return fn(acc)
}

func valueOrZero64(acc *streamAccumulator, fn func(*streamAccumulator) int64) int64 {
	if acc == nil {
		return 0
	}
	return fn(acc)
}

func valueOrBool(acc *streamAccumulator, fn func(*streamAccumulator) bool) bool {
	if acc == nil {
		return false
	}
	return fn(acc)
}

func valueOrString(acc *streamAccumulator, fn func(*streamAccumulator) string) string {
	if acc == nil {
		return ""
	}
	return fn(acc)
}

func valueOrNil(acc *streamAccumulator, fn func(*streamAccumulator) any) any {
	if acc == nil {
		return nil
	}
	return fn(acc)
}
