package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	metadataProjectID              = "project_id"
	metadataPromptCacheKey         = "prompt_cache_key"
	metadataUpstreamPromptCacheKey = "upstream_prompt_cache_key"
	metadataPromptCachedID         = "prompt_cached_id"

	metadataCPAProjectID              = "cpa.project_id"
	metadataCPAPromptCacheKey         = "cpa.prompt_cache_key"
	metadataCPAUpstreamPromptCacheKey = "cpa.upstream_prompt_cache_key"
	metadataCPAPromptCachedID         = "cpa.prompt_cached_id"
)

type requestContextInfo struct {
	ProjectID              string `json:"project_id,omitempty"`
	PromptCacheKey         string `json:"prompt_cache_key,omitempty"`
	UpstreamPromptCacheKey string `json:"upstream_prompt_cache_key,omitempty"`
	PromptCachedID         string `json:"prompt_cached_id,omitempty"`
}

func enrichRequestMetadata(ctx context.Context, req pluginapi.RequestMetadataEnrichRequest, hostCallbackID string) pluginapi.RequestMetadataEnrichResponse {
	if !shouldProcessPromptCacheContext(req.SourceFormat, "", req.Model, req.Headers, req.Body, req.Metadata) {
		logPluginDebug(hostCallbackID, "request metadata skipped", map[string]any{
			"reason":        "not_prompt_cache_context_request",
			"source_format": req.SourceFormat,
			"model":         req.Model,
		})
		return pluginapi.RequestMetadataEnrichResponse{}
	}
	info, ok := buildRequestContext(ctx, req.Headers, req.Body, req.Metadata, req.Model, hostCallbackID)
	if !ok {
		logPluginDebug(hostCallbackID, "request metadata skipped", map[string]any{
			"reason":        "missing_prompt_cache_context",
			"source_format": req.SourceFormat,
			"model":         req.Model,
		})
		return pluginapi.RequestMetadataEnrichResponse{}
	}
	logPluginDebug(hostCallbackID, "request metadata enriched", map[string]any{
		"project_id":                info.ProjectID,
		"prompt_cache_key":          info.PromptCacheKey,
		"upstream_prompt_cache_key": info.UpstreamPromptCacheKey,
		"prompt_cached_id":          info.PromptCachedID,
	})
	return pluginapi.RequestMetadataEnrichResponse{Metadata: metadataFromContextInfo(info)}
}

func interceptRequestAfterAuth(ctx context.Context, req pluginapi.RequestInterceptRequest, hostCallbackID string) pluginapi.RequestInterceptResponse {
	if !isCodexOpenAIRequest(req.SourceFormat, req.ToFormat, req.Model) {
		logPluginDebug(hostCallbackID, "prompt cache rewrite skipped", map[string]any{
			"reason":        "not_codex_openai_request",
			"source_format": req.SourceFormat,
			"to_format":     req.ToFormat,
			"model":         req.Model,
		})
		return pluginapi.RequestInterceptResponse{}
	}
	info, ok := contextInfoFromMetadata(req.Metadata)
	if !ok || info.UpstreamPromptCacheKey == "" {
		var built bool
		info, built = buildRequestContext(ctx, req.Headers, req.Body, req.Metadata, req.Model, hostCallbackID)
		if !built || info.UpstreamPromptCacheKey == "" {
			logPluginDebug(hostCallbackID, "prompt cache rewrite skipped", map[string]any{
				"reason": "missing_upstream_prompt_cache_key",
				"model":  req.Model,
			})
			return pluginapi.RequestInterceptResponse{}
		}
	}
	body, changed := setPromptCacheKey(req.Body, info.UpstreamPromptCacheKey)
	if !changed {
		logPluginDebug(hostCallbackID, "prompt cache rewrite skipped", map[string]any{
			"reason":                    "request_body_unchanged",
			"prompt_cache_key":          info.PromptCacheKey,
			"upstream_prompt_cache_key": info.UpstreamPromptCacheKey,
		})
		return pluginapi.RequestInterceptResponse{}
	}
	logPluginDebug(hostCallbackID, "prompt cache key rewritten", map[string]any{
		"project_id":                info.ProjectID,
		"prompt_cache_key":          info.PromptCacheKey,
		"upstream_prompt_cache_key": info.UpstreamPromptCacheKey,
		"prompt_cached_id":          info.PromptCachedID,
	})
	return pluginapi.RequestInterceptResponse{Body: body}
}

func finalizeRequest(ctx context.Context, req pluginapi.RequestFinalizeRequest, hostCallbackID string) pluginapi.RequestFinalizeResponse {
	if !isCodexOpenAIRequest(req.SourceFormat, req.ToFormat, req.Model) {
		logPluginDebug(hostCallbackID, "prompt cache finalizer skipped", map[string]any{
			"reason":        "not_codex_openai_request",
			"source_format": req.SourceFormat,
			"to_format":     req.ToFormat,
			"model":         req.Model,
		})
		return pluginapi.RequestFinalizeResponse{}
	}
	info, ok := buildRequestContext(ctx, req.Headers, req.Body, req.Metadata, req.Model, hostCallbackID)
	if !ok || info.ProjectID == "" || info.UpstreamPromptCacheKey == "" {
		logPluginDebug(hostCallbackID, "prompt cache finalizer skipped", map[string]any{
			"reason":     "missing_project_prompt_cache_context",
			"model":      req.Model,
			"project_id": info.ProjectID,
		})
		return pluginapi.RequestFinalizeResponse{}
	}

	body, changed := setPromptCacheKey(req.Body, info.UpstreamPromptCacheKey)
	if !changed && topLevelPromptCacheKey(req.Body) != info.UpstreamPromptCacheKey {
		logPluginDebug(hostCallbackID, "prompt cache finalizer skipped", map[string]any{
			"reason":                    "request_body_unsettable",
			"project_id":                info.ProjectID,
			"prompt_cache_key":          info.PromptCacheKey,
			"upstream_prompt_cache_key": info.UpstreamPromptCacheKey,
		})
		return pluginapi.RequestFinalizeResponse{}
	}
	resp := pluginapi.RequestFinalizeResponse{}
	if changed {
		resp.Body = body
	}
	logPluginDebug(hostCallbackID, "prompt cache key finalized", map[string]any{
		"project_id":                info.ProjectID,
		"prompt_cache_key":          info.PromptCacheKey,
		"upstream_prompt_cache_key": info.UpstreamPromptCacheKey,
		"prompt_cached_id":          info.PromptCachedID,
		"body_changed":              changed,
		"headers_synced":            false,
	})
	return resp
}

func buildRequestContext(ctx context.Context, headers http.Header, body []byte, metadata map[string]any, model string, hostCallbackID string) (requestContextInfo, bool) {
	projectID := firstNonEmpty(
		stringFromMetadata(metadata, metadataProjectID),
		stringFromMetadata(metadata, metadataCPAProjectID),
		extractProjectID(headers, body),
	)
	promptCacheKey := firstNonEmpty(
		stringFromMetadata(metadata, metadataPromptCacheKey),
		stringFromMetadata(metadata, metadataCPAPromptCacheKey),
		extractPromptCacheKey(headers, body),
	)
	logicalKey := promptCacheKey
	if projectID != "" {
		logicalKey = "project:" + strings.TrimSpace(model) + ":" + projectID
	}
	if logicalKey == "" {
		return requestContextInfo{}, false
	}

	entry, errEntry := loadOrCreatePromptCacheEntry(ctx, hostCallbackID, projectID, logicalKey)
	if errEntry != nil {
		logPluginDebug(hostCallbackID, "prompt cache storage fallback", map[string]any{
			"project_id":       projectID,
			"prompt_cache_key": logicalKey,
			"error":            errEntry.Error(),
		})
		entry = promptCacheEntryFor(projectID, logicalKey)
	}
	return requestContextInfo{
		ProjectID:              projectID,
		PromptCacheKey:         logicalKey,
		UpstreamPromptCacheKey: entry.UpstreamPromptCacheKey,
		PromptCachedID:         entry.PromptCachedID,
	}, true
}

func metadataFromContextInfo(info requestContextInfo) map[string]any {
	metadata := make(map[string]any)
	setStringMetadata(metadata, metadataProjectID, info.ProjectID)
	setStringMetadata(metadata, metadataPromptCacheKey, info.PromptCacheKey)
	setStringMetadata(metadata, metadataUpstreamPromptCacheKey, info.UpstreamPromptCacheKey)
	setStringMetadata(metadata, metadataPromptCachedID, info.PromptCachedID)
	setStringMetadata(metadata, metadataCPAProjectID, info.ProjectID)
	setStringMetadata(metadata, metadataCPAPromptCacheKey, info.PromptCacheKey)
	setStringMetadata(metadata, metadataCPAUpstreamPromptCacheKey, info.UpstreamPromptCacheKey)
	setStringMetadata(metadata, metadataCPAPromptCachedID, info.PromptCachedID)
	return metadata
}

func contextInfoFromMetadata(metadata map[string]any) (requestContextInfo, bool) {
	info := requestContextInfo{
		ProjectID:              firstNonEmpty(stringFromMetadata(metadata, metadataCPAProjectID), stringFromMetadata(metadata, metadataProjectID)),
		PromptCacheKey:         firstNonEmpty(stringFromMetadata(metadata, metadataCPAPromptCacheKey), stringFromMetadata(metadata, metadataPromptCacheKey)),
		UpstreamPromptCacheKey: firstNonEmpty(stringFromMetadata(metadata, metadataCPAUpstreamPromptCacheKey), stringFromMetadata(metadata, metadataUpstreamPromptCacheKey)),
		PromptCachedID:         firstNonEmpty(stringFromMetadata(metadata, metadataCPAPromptCachedID), stringFromMetadata(metadata, metadataPromptCachedID)),
	}
	return info, info.PromptCacheKey != "" || info.UpstreamPromptCacheKey != ""
}

func isCodexOpenAIRequest(sourceFormat, toFormat, model string) bool {
	joined := strings.ToLower(strings.TrimSpace(sourceFormat) + " " + strings.TrimSpace(toFormat) + " " + strings.TrimSpace(model))
	if strings.Contains(joined, "antigravity") {
		return false
	}
	return strings.Contains(joined, "codex") || strings.Contains(joined, "openai") || strings.Contains(joined, "responses")
}

func shouldProcessPromptCacheContext(sourceFormat, toFormat, model string, headers http.Header, body []byte, metadata map[string]any) bool {
	if isCodexOpenAIRequest(sourceFormat, toFormat, model) {
		return true
	}
	joined := strings.ToLower(strings.TrimSpace(sourceFormat) + " " + strings.TrimSpace(toFormat) + " " + strings.TrimSpace(model))
	if strings.Contains(joined, "antigravity") {
		return false
	}
	return firstNonEmpty(
		stringFromMetadata(metadata, metadataProjectID),
		stringFromMetadata(metadata, metadataCPAProjectID),
		stringFromMetadata(metadata, metadataPromptCacheKey),
		stringFromMetadata(metadata, metadataCPAPromptCacheKey),
		extractProjectID(headers, body),
		extractPromptCacheKey(headers, body),
	) != ""
}

func extractProjectID(headers http.Header, body []byte) string {
	if headers != nil {
		for _, name := range []string{"X-Project-Id", "X-Project-ID", "Project-Id", "Project-ID"} {
			if value := strings.TrimSpace(headers.Get(name)); value != "" {
				return value
			}
		}
	}
	root := parseJSONObject(body)
	return firstNonEmpty(
		jsonStringAt(root, "project_id"),
		jsonStringAt(root, "project-id"),
		jsonStringAt(root, "metadata", "project_id"),
		jsonStringAt(root, "metadata", "project-id"),
		jsonStringAt(root, "client_metadata", "project_id"),
		jsonStringAt(root, "client_metadata", "project-id"),
		extractProjectIDFromBodyText(root),
	)
}

func extractProjectIDFromBodyText(value any) string {
	switch typed := value.(type) {
	case string:
		return projectIDFromTaggedText(typed)
	case []any:
		for _, item := range typed {
			if projectID := extractProjectIDFromBodyText(item); projectID != "" {
				return projectID
			}
		}
	case map[string]any:
		for _, key := range []string{"input", "messages", "content", "text", "instructions"} {
			if projectID := extractProjectIDFromBodyText(typed[key]); projectID != "" {
				return projectID
			}
		}
		for _, item := range typed {
			if projectID := extractProjectIDFromBodyText(item); projectID != "" {
				return projectID
			}
		}
	}
	return ""
}

func projectIDFromTaggedText(text string) string {
	lowerText := strings.ToLower(text)
	start := strings.Index(lowerText, "<project-id>")
	if start < 0 {
		return ""
	}
	start += len("<project-id>")
	end := strings.Index(lowerText[start:], "</project-id>")
	if end < 0 {
		return ""
	}
	projectID := strings.TrimSpace(text[start : start+end])
	if projectID == "" || len(projectID) > 256 || strings.Contains(projectID, "<") {
		return ""
	}
	return projectID
}

func extractPromptCacheKey(headers http.Header, body []byte) string {
	root := parseJSONObject(body)
	if key := strings.TrimSpace(jsonStringAt(root, "prompt_cache_key")); key != "" {
		return key
	}
	if key := promptCacheKeyFromTurnMetadata(jsonStringAt(root, "client_metadata", "x-codex-turn-metadata")); key != "" {
		return key
	}
	if windowID := strings.TrimSpace(jsonStringAt(root, "client_metadata", "x-codex-window-id")); windowID != "" {
		return strings.SplitN(windowID, ":", 2)[0]
	}
	if headers != nil {
		if key := promptCacheKeyFromTurnMetadata(headers.Get("X-Codex-Turn-Metadata")); key != "" {
			return key
		}
		if windowID := strings.TrimSpace(headers.Get("X-Codex-Window-Id")); windowID != "" {
			return strings.SplitN(windowID, ":", 2)[0]
		}
	}
	return ""
}

func setPromptCacheKey(body []byte, key string) ([]byte, bool) {
	key = strings.TrimSpace(key)
	if key == "" || len(body) == 0 {
		return nil, false
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil || root == nil {
		return nil, false
	}
	if strings.TrimSpace(jsonStringAt(root, "prompt_cache_key")) == key {
		return nil, false
	}
	root["prompt_cache_key"] = key
	out, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return nil, false
	}
	return out, true
}

func topLevelPromptCacheKey(body []byte) string {
	return strings.TrimSpace(jsonStringAt(parseJSONObject(body), "prompt_cache_key"))
}

func promptCacheKeyFromTurnMetadata(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal([]byte(raw), &metadata); errUnmarshal != nil {
		return ""
	}
	return strings.TrimSpace(jsonStringAt(metadata, "prompt_cache_key"))
}

func parseJSONObject(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return nil
	}
	return root
}

func jsonStringAt(root map[string]any, path ...string) string {
	var current any = root
	for _, part := range path {
		object, ok := current.(map[string]any)
		if !ok || object == nil {
			return ""
		}
		current = object[part]
	}
	switch value := current.(type) {
	case string:
		return strings.TrimSpace(value)
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	default:
		return ""
	}
}

func stringFromMetadata(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	switch value := metadata[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func setStringMetadata(metadata map[string]any, key, value string) {
	if strings.TrimSpace(value) != "" {
		metadata[key] = strings.TrimSpace(value)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func sha1Hex(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

func stableUUID(name string) string {
	sum := sha1.Sum([]byte("cpa-codex-openai-context:" + name))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}
