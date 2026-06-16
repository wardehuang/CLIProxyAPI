package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

const (
	metadataCompactDetected       = "cpa.compact.detected"
	metadataCompactKind           = "cpa.compact.kind"
	metadataCompactReason         = "cpa.compact.reason"
	metadataCompactOriginalModel  = "cpa.compact.original_model"
	metadataCompactTargetModel    = "cpa.compact.target_model"
	metadataCompactProvider       = "cpa.compact.provider"
	metadataCompactRouteRewritten = "cpa.compact.route_rewritten"

	compactKindClaudeMessages = "claude_messages"
	compactKindOpenAIResponse = "openai_responses"

	providerCodex       = "codex"
	providerAntigravity = "antigravity"

	defaultCodexCompactModel       = "gpt-5.4-mini"
	defaultAntigravityCompactModel = "gemini-3.1-flash-lite"
)

var compactConfigState = struct {
	sync.RWMutex
	config compactPluginConfig
}{config: defaultCompactPluginConfig()}

type compactPluginConfig struct {
	CodexCompactModel       string `yaml:"codex-compact-model" json:"codex-compact-model"`
	AntigravityCompactModel string `yaml:"antigravity-compact-model" json:"antigravity-compact-model"`
}

type compactDetection struct {
	Detected bool
	Kind     string
	Reason   string
}

func defaultCompactPluginConfig() compactPluginConfig {
	return compactPluginConfig{
		CodexCompactModel:       defaultCodexCompactModel,
		AntigravityCompactModel: defaultAntigravityCompactModel,
	}
}

func configurePlugin(configYAML []byte) {
	config := defaultCompactPluginConfig()
	if len(bytes.TrimSpace(configYAML)) > 0 {
		var parsed compactPluginConfig
		if errUnmarshal := yaml.Unmarshal(configYAML, &parsed); errUnmarshal == nil {
			config.CodexCompactModel = firstNonEmpty(parsed.CodexCompactModel, config.CodexCompactModel)
			config.AntigravityCompactModel = firstNonEmpty(parsed.AntigravityCompactModel, config.AntigravityCompactModel)
		}
	}
	compactConfigState.Lock()
	compactConfigState.config = config
	compactConfigState.Unlock()
}

func enrichRequestMetadata(ctx context.Context, req pluginapi.RequestMetadataEnrichRequest) pluginapi.RequestMetadataEnrichResponse {
	_ = ctx
	detection := detectCompactRequest(req.SourceFormat, "", req.Body)
	if !detection.Detected {
		return pluginapi.RequestMetadataEnrichResponse{}
	}
	return pluginapi.RequestMetadataEnrichResponse{Metadata: map[string]any{
		metadataCompactDetected:      true,
		metadataCompactKind:          detection.Kind,
		metadataCompactReason:        detection.Reason,
		metadataCompactOriginalModel: firstNonEmpty(req.Model, jsonString(req.Body, "model")),
	}}
}

func rewriteCompactRoute(ctx context.Context, req pluginapi.RouteRewriteRequest) pluginapi.RouteRewriteResponse {
	_ = ctx
	detection := compactDetectionFromMetadata(req.Metadata)
	if !detection.Detected {
		detection = detectCompactRequest(req.SourceFormat, req.Alt, req.Body)
	}
	if !detection.Detected {
		return pluginapi.RouteRewriteResponse{}
	}
	provider, targetModel := compactTargetForProviders(req.Providers)
	metadata := map[string]any{
		metadataCompactDetected:      true,
		metadataCompactKind:          detection.Kind,
		metadataCompactReason:        detection.Reason,
		metadataCompactOriginalModel: firstNonEmpty(req.RequestedModel, req.NormalizedModel, jsonString(req.Body, "model")),
	}
	if provider != "" {
		metadata[metadataCompactProvider] = provider
	}
	if targetModel == "" {
		return pluginapi.RouteRewriteResponse{Metadata: metadata}
	}
	metadata[metadataCompactTargetModel] = targetModel
	metadata[metadataCompactRouteRewritten] = true
	return pluginapi.RouteRewriteResponse{
		RequestedModel:  targetModel,
		NormalizedModel: targetModel,
		Metadata:        metadata,
	}
}

func detectCompactRequest(sourceFormat, alt string, body []byte) compactDetection {
	if strings.EqualFold(strings.TrimSpace(alt), "responses/compact") {
		return compactDetection{Detected: true, Kind: compactKindOpenAIResponse, Reason: "responses_compact_alt"}
	}
	if len(bytes.TrimSpace(body)) == 0 || !json.Valid(body) {
		return compactDetection{}
	}
	if isOpenAIResponseFormat(sourceFormat) && isOpenAIResponsesSummarizationRequest(body) {
		return compactDetection{Detected: true, Kind: compactKindOpenAIResponse, Reason: "openai_responses_summarization"}
	}
	if isClaudeMessagesFormat(sourceFormat) && isClaudeMessagesCompactRequest(body) {
		return compactDetection{Detected: true, Kind: compactKindClaudeMessages, Reason: "claude_messages_compact"}
	}
	return compactDetection{}
}

func isOpenAIResponsesSummarizationRequest(body []byte) bool {
	if strings.EqualFold(gjson.GetBytes(body, "context_management.type").String(), "compaction") {
		return true
	}
	if gjson.GetBytes(body, "context_management.compaction").Exists() {
		return true
	}
	return strings.EqualFold(gjson.GetBytes(body, "metadata.cpa_compact").String(), "true")
}

func isClaudeMessagesCompactRequest(body []byte) bool {
	return jsonContainsAnyString(body,
		"your task is to create a detailed summary of the conversation so far",
		"this summary should be thorough in capturing technical details",
		"provide a detailed summary of our conversation so far",
		"create a concise summary of the conversation so far",
		"compact the conversation",
		"context compaction",
	)
}

func compactTargetForProviders(providers []string) (string, string) {
	compactConfigState.RLock()
	config := compactConfigState.config
	compactConfigState.RUnlock()
	for _, provider := range providers {
		switch normalizeProvider(provider) {
		case providerCodex:
			return providerCodex, strings.TrimSpace(config.CodexCompactModel)
		case providerAntigravity:
			return providerAntigravity, strings.TrimSpace(config.AntigravityCompactModel)
		}
	}
	return "", ""
}

func compactDetectionFromMetadata(metadata map[string]any) compactDetection {
	if len(metadata) == 0 || !boolFromMetadata(metadataCompactDetected, metadata) {
		return compactDetection{}
	}
	return compactDetection{
		Detected: true,
		Kind:     stringFromMetadata(metadataCompactKind, metadata),
		Reason:   stringFromMetadata(metadataCompactReason, metadata),
	}
}

func isOpenAIResponseFormat(format string) bool {
	format = strings.ToLower(strings.TrimSpace(format))
	return strings.Contains(format, "response") || strings.Contains(format, "responses")
}

func isClaudeMessagesFormat(format string) bool {
	format = strings.ToLower(strings.TrimSpace(format))
	return strings.Contains(format, "claude") || strings.Contains(format, "anthropic")
}

func jsonString(body []byte, path string) string {
	return strings.TrimSpace(gjson.GetBytes(body, path).String())
}

func jsonContainsAnyString(body []byte, needles ...string) bool {
	var value any
	if errUnmarshal := json.Unmarshal(body, &value); errUnmarshal != nil {
		return false
	}
	return containsAnyStringValue(value, needles)
}

func containsAnyStringValue(value any, needles []string) bool {
	switch typed := value.(type) {
	case string:
		text := strings.ToLower(typed)
		for _, needle := range needles {
			if strings.Contains(text, strings.ToLower(needle)) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if containsAnyStringValue(item, needles) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if containsAnyStringValue(item, needles) {
				return true
			}
		}
	}
	return false
}

func boolFromMetadata(key string, metadata map[string]any) bool {
	switch value := metadata[key].(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "true")
	default:
		return false
	}
}

func stringFromMetadata(key string, metadata map[string]any) string {
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "codex", "openai-codex":
		return providerCodex
	case "antigravity", "google-antigravity":
		return providerAntigravity
	default:
		return provider
	}
}
