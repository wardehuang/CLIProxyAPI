package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestInjectMissingReasoningContentPadsAndUsesCache(t *testing.T) {
	resetReasoningCache(128, 0)
	configurePlugin([]byte("enabled: true\npad_placeholder: \" \"\nmodel_substrings:\n  - deepseek\n"))

	body := []byte(`{
		"model":"deepseek-v4-flash-free",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"terminal","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		]
	}`)

	updated, stats := injectMissingReasoningContent(body, " ")
	if !stats.Changed {
		t.Fatal("expected inject change")
	}
	if stats.InjectedPad != 1 {
		t.Fatalf("InjectedPad=%d want 1", stats.InjectedPad)
	}
	if got := gjson.GetBytes(updated, "messages.1.reasoning_content").String(); got != " " {
		t.Fatalf("pad reasoning_content=%q", got)
	}

	cacheReasoningContent(toolCallsCacheKey([]string{"call_1"}), "prior plan")
	updated, stats = injectMissingReasoningContent(body, " ")
	if stats.InjectedCache != 1 {
		t.Fatalf("InjectedCache=%d want 1; stats=%+v", stats.InjectedCache, stats)
	}
	if got := gjson.GetBytes(updated, "messages.1.reasoning_content").String(); got != "prior plan" {
		t.Fatalf("cache reasoning_content=%q", got)
	}
}

func TestCacheFromOpenAIChatResponse(t *testing.T) {
	resetReasoningCache(128, 0)
	configurePlugin(nil)

	body := []byte(`{
		"id":"chatcmpl-1",
		"choices":[{
			"message":{
				"role":"assistant",
				"content":"ok",
				"reasoning_content":"think hard",
				"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"Read","arguments":"{}"}}]
			}
		}]
	}`)
	_ = cacheFromResponseBody(pluginapi.ResponseInterceptRequest{
		Model:          "deepseek-v4-flash-free",
		RequestedModel: "deepseek-v4-flash-free",
		StatusCode:     200,
		Body:           body,
	}, "")

	got, ok := lookupReasoningContent(toolCallsCacheKey([]string{"call_abc"}))
	if !ok || got != "think hard" {
		t.Fatalf("cache lookup = %q ok=%v", got, ok)
	}
}

func TestCacheFromResponsesBody(t *testing.T) {
	resetReasoningCache(128, 0)
	configurePlugin(nil)

	body := []byte(`{
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"server still building"}],"encrypted_content":""},
			{"type":"function_call","call_id":"call_xyz","name":"terminal","arguments":"{}"}
		]
	}`)
	extracted, ok := extractReasoningFromResponses(body)
	if !ok {
		t.Fatal("expected extract ok")
	}
	if extracted.Content != "server still building" {
		t.Fatalf("content=%q", extracted.Content)
	}
	if len(extracted.ToolCallIDs) != 1 || extracted.ToolCallIDs[0] != "call_xyz" {
		t.Fatalf("tool ids=%v", extracted.ToolCallIDs)
	}
}

func TestFinalizeDeepseekRequestSkipsNonDeepseek(t *testing.T) {
	resetReasoningCache(128, 0)
	configurePlugin(nil)
	resp := finalizeDeepseekRequest(pluginapi.RequestFinalizeRequest{
		Model: "gpt-5",
		Body:  []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"c1"}]}]}`),
	}, "")
	if len(resp.Body) != 0 {
		t.Fatalf("expected no rewrite, got %s", resp.Body)
	}
}

func TestStreamAccumulatorCachesOnDone(t *testing.T) {
	resetReasoningCache(128, 0)
	configurePlugin(nil)

	reqID := "req-stream-1"
	_ = observeStreamChunk(pluginapi.StreamChunkInterceptRequest{
		RequestID:      reqID,
		Model:          "deepseek-v4-flash-free",
		RequestedModel: "deepseek-v4-flash-free",
		Body:           []byte(`data: {"choices":[{"delta":{"reasoning_content":"step-"}}]}` + "\n"),
	}, "")
	_ = observeStreamChunk(pluginapi.StreamChunkInterceptRequest{
		RequestID:      reqID,
		Model:          "deepseek-v4-flash-free",
		RequestedModel: "deepseek-v4-flash-free",
		Body:           []byte(`data: {"choices":[{"delta":{"reasoning_content":"one","tool_calls":[{"id":"call_s1","type":"function","function":{"name":"x","arguments":"{}"}}]}}]}` + "\n"),
	}, "")
	_ = observeStreamChunk(pluginapi.StreamChunkInterceptRequest{
		RequestID:      reqID,
		Model:          "deepseek-v4-flash-free",
		RequestedModel: "deepseek-v4-flash-free",
		Body:           []byte("data: [DONE]\n"),
	}, "")

	got, ok := lookupReasoningContent(toolCallsCacheKey([]string{"call_s1"}))
	if !ok {
		t.Fatal("expected stream cache hit")
	}
	if !strings.Contains(got, "step-") || !strings.Contains(got, "one") {
		t.Fatalf("cached content=%q", got)
	}
}
