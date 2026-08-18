package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorProtocolResponsesChatClient(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_1",
			"object":"response",
			"created_at":1700000000,
			"status":"completed",
			"model":"gpt-test",
			"output":[
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}
			],
			"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}
		}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("compat", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:     "compat",
			Protocol: "responses",
		}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "compat",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"api_key":      "test",
			"compat_name":  "compat",
			"provider_key": "compat",
		},
	}
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-test",
		Payload: []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	if !gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("expected responses input, body=%s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("unexpected chat messages on responses upstream: %s", string(gotBody))
	}
	if gjson.GetBytes(resp.Payload, "object").String() != "chat.completion" {
		t.Fatalf("client payload object = %s, body=%s", gjson.GetBytes(resp.Payload, "object").String(), string(resp.Payload))
	}
	if gjson.GetBytes(resp.Payload, "choices.0.message.content").String() != "hello" {
		t.Fatalf("client content = %q, body=%s", gjson.GetBytes(resp.Payload, "choices.0.message.content").String(), string(resp.Payload))
	}
}

func TestOpenAICompatExecutorProtocolResponsesClientPassthrough(t *testing.T) {
	var gotPath string
	var gotBody []byte
	upstream := []byte(`{
		"id":"resp_1",
		"object":"response",
		"created_at":1700000000,
		"status":"completed",
		"model":"gpt-test",
		"output":[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}
		],
		"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}
	}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(upstream)
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("compat", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:     "compat",
			Protocol: "responses",
		}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "compat",
		Attributes: map[string]string{
			"base_url":    server.URL + "/v1",
			"api_key":     "test",
			"compat_name": "compat",
		},
	}
	payload := []byte(`{"model":"gpt-test","input":[{"role":"user","content":"hi"}]}`)
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-test",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	if !gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("expected input passthrough, body=%s", string(gotBody))
	}
	if gjson.GetBytes(resp.Payload, "object").String() != "response" {
		t.Fatalf("client payload object = %s, body=%s", gjson.GetBytes(resp.Payload, "object").String(), string(resp.Payload))
	}
}

func TestOpenAICompatExecutorRequestToFormatUsesProtocol(t *testing.T) {
	executor := NewOpenAICompatExecutor("compat", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:     "compat",
			Protocol: "responses",
		}},
	})
	got := executor.RequestToFormat(cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if got != sdktranslator.FormatOpenAIResponse {
		t.Fatalf("RequestToFormat = %q, want %q", got, sdktranslator.FormatOpenAIResponse)
	}
}
