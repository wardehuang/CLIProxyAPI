package main

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRewriteOpenAIResponsesCompactRouteToCodexModel(t *testing.T) {
	configurePlugin([]byte("codex-compact-model: gpt-test-compact\n"))

	body := []byte(`{"model":"gpt-5.4","context_management":{"type":"compaction"}}`)
	metaResp := enrichRequestMetadata(context.Background(), pluginapi.RequestMetadataEnrichRequest{
		SourceFormat: "openai-response",
		Model:        "gpt-5.4",
		Body:         body,
	})
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
	})
	if routeResp.RequestedModel != "gpt-test-compact" || routeResp.NormalizedModel != "gpt-test-compact" {
		t.Fatalf("route models = %q/%q, want gpt-test-compact", routeResp.RequestedModel, routeResp.NormalizedModel)
	}
	if routeResp.Metadata[metadataCompactProvider] != providerCodex {
		t.Fatalf("provider metadata = %v, want %s", routeResp.Metadata[metadataCompactProvider], providerCodex)
	}
}

func TestRewriteClaudeCompactRouteToAntigravityModel(t *testing.T) {
	configurePlugin([]byte("antigravity-compact-model: gemini-test-compact\n"))

	body := []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far."}]}`)
	routeResp := rewriteCompactRoute(context.Background(), pluginapi.RouteRewriteRequest{
		SourceFormat:    "anthropic",
		RequestedModel:  "claude-sonnet",
		NormalizedModel: "claude-sonnet",
		Providers:       []string{"antigravity"},
		Body:            body,
	})
	if routeResp.RequestedModel != "gemini-test-compact" || routeResp.NormalizedModel != "gemini-test-compact" {
		t.Fatalf("route models = %q/%q, want gemini-test-compact", routeResp.RequestedModel, routeResp.NormalizedModel)
	}
	if routeResp.Metadata[metadataCompactKind] != compactKindClaudeMessages {
		t.Fatalf("kind metadata = %v, want %s", routeResp.Metadata[metadataCompactKind], compactKindClaudeMessages)
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
	})
	if routeResp.RequestedModel != "" || routeResp.NormalizedModel != "" || len(routeResp.Metadata) != 0 {
		t.Fatalf("unexpected rewrite response: %+v", routeResp)
	}
}
