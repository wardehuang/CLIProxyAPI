package main

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRewriteOpenAIResponsesCompactRouteToCodexModel(t *testing.T) {
	configurePlugin([]byte("codex-compact-model: gpt-test-compact\ncodex-compact-reasoning-effort: low\n"))

	body := []byte(`{"model":"gpt-5.4","context_management":{"type":"compaction"}}`)
	metaResp := enrichRequestMetadata(context.Background(), pluginapi.RequestMetadataEnrichRequest{
		SourceFormat: "openai-response",
		Model:        "gpt-5.4",
		Body:         body,
	}, "")
	if metaResp.Metadata[metadataCompactDetected] != true {
		t.Fatalf("metadata compact detected = %v, want true", metaResp.Metadata[metadataCompactDetected])
	}

	routeResp := rewriteCompactRoute(context.Background(), pluginapi.RouteRewriteRequest{
		SourceFormat:    "openai-response",
		RequestedModel:  "gpt-5.4",
		NormalizedModel: "gpt-5.4",
		Providers:       []string{"codex"},
		Body:            body,
		Metadata:        metaResp.Metadata,
	}, "")
	if routeResp.RequestedModel != "gpt-test-compact" || routeResp.NormalizedModel != "gpt-test-compact" {
		t.Fatalf("route models = %q/%q, want gpt-test-compact", routeResp.RequestedModel, routeResp.NormalizedModel)
	}
	if routeResp.Metadata["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort metadata = %v, want low", routeResp.Metadata["reasoning_effort"])
	}
	if routeResp.Metadata[metadataCompactProvider] != providerCodex {
		t.Fatalf("provider metadata = %v, want %s", routeResp.Metadata[metadataCompactProvider], providerCodex)
	}
}

func TestRewriteClaudeCompactRouteToAntigravityModel(t *testing.T) {
	configurePlugin([]byte("antigravity-compact-model: gemini-test-compact\nantigravity-compact-reasoning-effort: medium\n"))

	body := []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far."}]}`)
	routeResp := rewriteCompactRoute(context.Background(), pluginapi.RouteRewriteRequest{
		SourceFormat:    "anthropic",
		RequestedModel:  "claude-sonnet",
		NormalizedModel: "claude-sonnet",
		Providers:       []string{"antigravity"},
		Body:            body,
	}, "")
	if routeResp.RequestedModel != "gemini-test-compact" || routeResp.NormalizedModel != "gemini-test-compact" {
		t.Fatalf("route models = %q/%q, want gemini-test-compact", routeResp.RequestedModel, routeResp.NormalizedModel)
	}
	if routeResp.Metadata["reasoning_effort"] != "medium" {
		t.Fatalf("reasoning_effort metadata = %v, want medium", routeResp.Metadata["reasoning_effort"])
	}
	if routeResp.Metadata[metadataCompactKind] != compactKindClaudeMessages {
		t.Fatalf("kind metadata = %v, want %s", routeResp.Metadata[metadataCompactKind], compactKindClaudeMessages)
	}
}

func TestRewriteOpenAIResponsesSummarizationTagToCodexModel(t *testing.T) {
	configurePlugin([]byte("codex-compact-model: gpt-test-compact\n"))

	body := []byte(`{"model":"gpt-5.5","instructions":"You are an intelligent assistant, tasked with summarizing the following conversation. You MUST follow the instructions given in the <summarization_request> tags and summarize the conversation. This summary will be provided to another AI assistant.","input":[{"role":"user","content":"normal history"}]}`)
	routeResp := rewriteCompactRoute(context.Background(), pluginapi.RouteRewriteRequest{
		SourceFormat:    "openai-response",
		RequestedModel:  "gpt-5.5",
		NormalizedModel: "gpt-5.5",
		Providers:       []string{"codex"},
		Body:            body,
	}, "")
	if routeResp.RequestedModel != "gpt-test-compact" || routeResp.NormalizedModel != "gpt-test-compact" {
		t.Fatalf("route models = %q/%q, want gpt-test-compact", routeResp.RequestedModel, routeResp.NormalizedModel)
	}
	if routeResp.Metadata[metadataCompactReason] != "cursor_summarization" {
		t.Fatalf("reason metadata = %v, want cursor_summarization", routeResp.Metadata[metadataCompactReason])
	}
	if routeResp.Metadata[metadataCompactKind] != compactKindCursorSummarization {
		t.Fatalf("kind metadata = %v, want %s", routeResp.Metadata[metadataCompactKind], compactKindCursorSummarization)
	}
}

func TestRewriteClaudeMessagesSummarizationTagToCodexModel(t *testing.T) {
	configurePlugin([]byte("codex-compact-model: gpt-test-compact\n"))

	body := []byte(`{"model":"claude-sonnet","system":"You are an intelligent assistant, tasked with summarizing the following conversation. You MUST follow the instructions given in the <summarization_request> tags and summarize the conversation. This summary will be provided to another AI assistant.","messages":[{"role":"user","content":"normal history"}]}`)
	routeResp := rewriteCompactRoute(context.Background(), pluginapi.RouteRewriteRequest{
		SourceFormat:    "anthropic",
		RequestedModel:  "claude-sonnet",
		NormalizedModel: "claude-sonnet",
		Providers:       []string{"codex"},
		Body:            body,
	}, "")
	if routeResp.RequestedModel != "gpt-test-compact" || routeResp.NormalizedModel != "gpt-test-compact" {
		t.Fatalf("route models = %q/%q, want gpt-test-compact", routeResp.RequestedModel, routeResp.NormalizedModel)
	}
	if routeResp.Metadata[metadataCompactKind] != compactKindCursorSummarization {
		t.Fatalf("kind metadata = %v, want %s", routeResp.Metadata[metadataCompactKind], compactKindCursorSummarization)
	}
}

func TestRewriteCompactRouteUsesDefaultTarget(t *testing.T) {
	configurePlugin([]byte("compact-provider: antigravity\ncompact-model: gemini-unified-compact\ncompact-reasoning-effort: low\n"))

	body := []byte(`{"model":"gpt-5.5","context_management":{"type":"compaction"}}`)
	routeResp := rewriteCompactRoute(context.Background(), pluginapi.RouteRewriteRequest{
		SourceFormat:    "openai-response",
		RequestedModel:  "gpt-5.5",
		NormalizedModel: "gpt-5.5",
		Providers:       []string{"codex"},
		Body:            body,
	}, "")
	if routeResp.RequestedModel != "gemini-unified-compact" || routeResp.NormalizedModel != "gemini-unified-compact" {
		t.Fatalf("route models = %q/%q, want gemini-unified-compact", routeResp.RequestedModel, routeResp.NormalizedModel)
	}
	if len(routeResp.Providers) != 1 || routeResp.Providers[0] != providerAntigravity {
		t.Fatalf("providers = %v, want [%s]", routeResp.Providers, providerAntigravity)
	}
	if routeResp.Metadata[metadataCompactProvider] != providerAntigravity {
		t.Fatalf("provider metadata = %v, want %s", routeResp.Metadata[metadataCompactProvider], providerAntigravity)
	}
	if routeResp.Metadata["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort metadata = %v, want low", routeResp.Metadata["reasoning_effort"])
	}
}

func TestRewriteCompactRouteSourceOverrideWins(t *testing.T) {
	configurePlugin([]byte("compact-provider: antigravity\ncompact-model: gemini-unified-compact\ncompact-reasoning-effort: low\ncodex-compact-provider: codex\ncodex-compact-model: gpt-codex-compact\ncodex-compact-reasoning-effort: high\n"))

	body := []byte(`{"model":"gpt-5.5","context_management":{"type":"compaction"}}`)
	routeResp := rewriteCompactRoute(context.Background(), pluginapi.RouteRewriteRequest{
		SourceFormat:    "openai-response",
		RequestedModel:  "gpt-5.5",
		NormalizedModel: "gpt-5.5",
		Providers:       []string{"codex"},
		Body:            body,
	}, "")
	if routeResp.RequestedModel != "gpt-codex-compact" || routeResp.NormalizedModel != "gpt-codex-compact" {
		t.Fatalf("route models = %q/%q, want gpt-codex-compact", routeResp.RequestedModel, routeResp.NormalizedModel)
	}
	if len(routeResp.Providers) != 1 || routeResp.Providers[0] != providerCodex {
		t.Fatalf("providers = %v, want [%s]", routeResp.Providers, providerCodex)
	}
	if routeResp.Metadata["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort metadata = %v, want high", routeResp.Metadata["reasoning_effort"])
	}
}

func TestCursorNormalRequestWithHistoricalSummarizationTagDoesNotRewriteRoute(t *testing.T) {
	configurePlugin(nil)

	routeResp := rewriteCompactRoute(context.Background(), pluginapi.RouteRewriteRequest{
		SourceFormat:    "openai-response",
		RequestedModel:  "gpt-5.4",
		NormalizedModel: "gpt-5.4",
		Providers:       []string{"codex"},
		Body:            []byte(`{"model":"gpt-5.4","instructions":"You are GPT-5.5. You are running as a coding agent in Cursor IDE on a user's computer.","input":[{"role":"user","content":"historical text <summarization_request>please summarize</summarization_request>"}],"tools":[{"type":"function","name":"Shell"}]}`),
	}, "")
	if routeResp.RequestedModel != "" || routeResp.NormalizedModel != "" || len(routeResp.Metadata) != 0 {
		t.Fatalf("unexpected rewrite response: %+v", routeResp)
	}
}

func TestNormalRequestDoesNotRewriteRoute(t *testing.T) {
	configurePlugin(nil)

	routeResp := rewriteCompactRoute(context.Background(), pluginapi.RouteRewriteRequest{
		SourceFormat:    "openai-response",
		RequestedModel:  "gpt-5.4",
		NormalizedModel: "gpt-5.4",
		Providers:       []string{"codex"},
		Body:            []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}, "")
	if routeResp.RequestedModel != "" || routeResp.NormalizedModel != "" || len(routeResp.Metadata) != 0 {
		t.Fatalf("unexpected rewrite response: %+v", routeResp)
	}
}
