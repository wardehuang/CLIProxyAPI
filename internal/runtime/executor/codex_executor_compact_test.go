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

func TestCodexExecutorCompactTranslatesClaudeRequest(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_compact","object":"response.compaction","model":"gpt-5.4-mini-2026-03-17","output_text":"compacted summary","usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33,"input_tokens_details":{"cached_tokens":7}}}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	claudePayload := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":[{"type":"text","text":"compact this conversation"}]}],"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"thinking":{"type":"adaptive"},"stream":true}`)
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini(low)",
		Payload: claudePayload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/responses/compact" {
		t.Fatalf("path = %q, want %q", gotPath, "/responses/compact")
	}
	if gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("compact body leaked Claude messages: %s", string(gotBody))
	}
	if !gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("compact body missing Codex input: %s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "gpt-5.4-mini" {
		t.Fatalf("upstream model = %q, want gpt-5.4-mini; body=%s", got, string(gotBody))
	}
	if !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Fatalf("stream should match normal codex requests: %s", string(gotBody))
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "compacted summary" {
		t.Fatalf("translated compact text = %q; payload=%s", got, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "usage.cache_read_input_tokens").Int(); got != 7 {
		t.Fatalf("cache read tokens = %d; payload=%s", got, string(resp.Payload))
	}
}

func TestCodexExecutorCompactAddsDefaultInstructions(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{
			name:    "missing instructions",
			payload: `{"model":"gpt-5.4","input":"hello"}`,
		},
		{
			name:    "null instructions",
			payload: `{"model":"gpt-5.4","instructions":null,"input":"hello"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				body, _ := io.ReadAll(r.Body)
				gotBody = body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
			}))
			defer server.Close()

			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": server.URL,
				"api_key":  "test",
			}}

			resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "gpt-5.4",
				Payload: []byte(tc.payload),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-response"),
				Alt:          "responses/compact",
				Stream:       false,
			})
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if gotPath != "/responses/compact" {
				t.Fatalf("path = %q, want %q", gotPath, "/responses/compact")
			}
			if !gjson.GetBytes(gotBody, "instructions").Exists() {
				t.Fatalf("expected instructions in compact request body, got %s", string(gotBody))
			}
			if gjson.GetBytes(gotBody, "instructions").Type != gjson.String {
				t.Fatalf("instructions type = %v, want string", gjson.GetBytes(gotBody, "instructions").Type)
			}
			if gjson.GetBytes(gotBody, "instructions").String() != "" {
				t.Fatalf("instructions = %q, want empty string", gjson.GetBytes(gotBody, "instructions").String())
			}
			if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
				t.Fatalf("payload = %s", string(resp.Payload))
			}
		})
	}
}
