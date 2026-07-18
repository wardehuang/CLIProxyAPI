package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestExtractProjectIDFromBodyTextTag(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.5",
		"input":[{
			"role":"user",
			"content":"<rules><user_rule># Project ID\n<project-id>cpa</project-id>\n</user_rule></rules>"
		}]
	}`)

	if got := extractProjectID(nil, body); got != "cpa" {
		t.Fatalf("project id = %q, want cpa", got)
	}
}

func TestExtractProjectIDFromPromptCacheIDTag(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.5",
		"input":[{
			"role":"user",
			"content":"<prompt-cache-id>cpa</prompt-cache-id>"
		}]
	}`)

	if got := extractProjectID(nil, body); got != "cpa" {
		t.Fatalf("project id = %q, want cpa", got)
	}
}

func TestExtractProjectIDPrefersStructuredField(t *testing.T) {
	body := []byte(`{
		"project_id":"structured",
		"input":[{"content":"<prompt-cache-id>text-fallback</prompt-cache-id>"}]
	}`)

	if got := extractProjectID(nil, body); got != "structured" {
		t.Fatalf("project id = %q, want structured", got)
	}
}

func TestBuildRequestContextPrefersProjectIDOverPromptCacheKey(t *testing.T) {
	body := []byte(`{"project_id":"cpa","prompt_cache_key":"client-a"}`)

	info, ok := buildRequestContext(context.Background(), nil, body, nil, "gpt-5-codex", "")
	if !ok {
		t.Fatal("expected request context")
	}
	if info.PromptCacheKey != "project:gpt-5-codex:cpa" {
		t.Fatalf("prompt cache key = %q, want project:gpt-5-codex:cpa", info.PromptCacheKey)
	}

	body = []byte(`{"project_id":"cpa","prompt_cache_key":"client-b"}`)
	info2, ok := buildRequestContext(context.Background(), nil, body, nil, "gpt-5-codex", "")
	if !ok {
		t.Fatal("expected second request context")
	}
	if info.UpstreamPromptCacheKey != info2.UpstreamPromptCacheKey {
		t.Fatalf("upstream prompt cache keys differ for same project: %q vs %q", info.UpstreamPromptCacheKey, info2.UpstreamPromptCacheKey)
	}

	info3, ok := buildRequestContext(context.Background(), nil, body, nil, "gpt-5.5", "")
	if !ok {
		t.Fatal("expected third request context")
	}
	if info.UpstreamPromptCacheKey == info3.UpstreamPromptCacheKey {
		t.Fatalf("upstream prompt cache keys should differ for different models: %q", info.UpstreamPromptCacheKey)
	}
}

func TestBuildRequestContextFallsBackToNativePromptCacheKey(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"native-key"}`)

	info, ok := buildRequestContext(context.Background(), nil, body, nil, "gpt-5-codex", "")
	if !ok {
		t.Fatal("expected request context")
	}
	if info.ProjectID != "" {
		t.Fatalf("project id = %q, want empty", info.ProjectID)
	}
	if info.PromptCacheKey != "native-key" {
		t.Fatalf("prompt cache key = %q, want native-key", info.PromptCacheKey)
	}
}

func TestFinalizeRequestWithProjectIDRewritesBodyAndSyncsHeaders(t *testing.T) {
	info, ok := buildRequestContext(context.Background(), nil, []byte(`{"project_id":"cpa","prompt_cache_key":"native-key"}`), nil, "gpt-5-codex", "")
	if !ok {
		t.Fatal("expected request context")
	}
	resp := finalizeRequest(context.Background(), pluginapi.RequestFinalizeRequest{
		SourceFormat: "openai-responses",
		ToFormat:     "codex",
		Model:        "gpt-5-codex",
		Headers:      http.Header{"session_id": []string{"native-key"}, "Conversation_id": []string{"native-key"}},
		Body:         []byte(`{"prompt_cache_key":"native-key"}`),
		Metadata:     metadataFromContextInfo(info),
	}, "")

	if topLevelPromptCacheKey(resp.Body) != info.UpstreamPromptCacheKey {
		t.Fatalf("final body prompt_cache_key = %q, want %q", topLevelPromptCacheKey(resp.Body), info.UpstreamPromptCacheKey)
	}
	if got := resp.Headers.Get(promptCacheSessionHeader); got != info.UpstreamPromptCacheKey {
		t.Fatalf("%s = %q, want %q", promptCacheSessionHeader, got, info.UpstreamPromptCacheKey)
	}
}

func TestFinalizeRequestWithoutProjectIDSyncsNativeSessionID(t *testing.T) {
	resp := finalizeRequest(context.Background(), pluginapi.RequestFinalizeRequest{
		SourceFormat: "openai-responses",
		ToFormat:     "codex",
		Model:        "gpt-5-codex",
		Body:         []byte(`{"prompt_cache_key":"native-key"}`),
	}, "")

	if len(resp.Body) != 0 {
		t.Fatalf("expected no body rewrite, got %s", string(resp.Body))
	}
	if got := resp.Headers.Get(promptCacheSessionHeader); got != "native-key" {
		t.Fatalf("%s = %q, want native-key", promptCacheSessionHeader, got)
	}
}

func TestFinalizeXAIRequestRewritesBodyAndSyncsGrokConversationHeader(t *testing.T) {
	info, ok := buildRequestContext(context.Background(), nil, []byte(`{"project_id":"cpa"}`), nil, "grok-4.3", "")
	if !ok {
		t.Fatal("expected request context")
	}
	resp := finalizeRequest(context.Background(), pluginapi.RequestFinalizeRequest{
		SourceFormat: "claude",
		ToFormat:     "codex",
		Model:        "grok-4.3",
		Body:         []byte(`{"model":"grok-4.3","input":"hello"}`),
		Metadata:     metadataFromContextInfo(info),
	}, "")

	if got := topLevelPromptCacheKey(resp.Body); got != info.UpstreamPromptCacheKey {
		t.Fatalf("final body prompt_cache_key = %q, want %q", got, info.UpstreamPromptCacheKey)
	}
	if got := resp.Headers.Get(xaiPromptCacheSessionHeader); got != info.UpstreamPromptCacheKey {
		t.Fatalf("%s = %q, want %q", xaiPromptCacheSessionHeader, got, info.UpstreamPromptCacheKey)
	}
	if got := resp.Headers.Get(promptCacheSessionHeader); got != "" {
		t.Fatalf("%s = %q, want empty", promptCacheSessionHeader, got)
	}
}
