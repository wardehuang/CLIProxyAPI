package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestResolveOpenAICompatWire(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		alt      string
		wantPath string
		wantFmt  sdktranslator.Format
		wantResp bool
	}{
		{name: "default chat", protocol: "", alt: "", wantPath: "/chat/completions", wantFmt: sdktranslator.FormatOpenAI, wantResp: false},
		{name: "explicit chat", protocol: "chat-completions", alt: "", wantPath: "/chat/completions", wantFmt: sdktranslator.FormatOpenAI, wantResp: false},
		{name: "responses", protocol: "responses", alt: "", wantPath: "/responses", wantFmt: sdktranslator.FormatOpenAIResponse, wantResp: true},
		{name: "compact wins", protocol: "chat-completions", alt: "responses/compact", wantPath: "/responses/compact", wantFmt: sdktranslator.FormatOpenAIResponse, wantResp: true},
		{name: "responses compact", protocol: "responses", alt: "responses/compact", wantPath: "/responses/compact", wantFmt: sdktranslator.FormatOpenAIResponse, wantResp: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveOpenAICompatWire(tt.protocol, tt.alt)
			if got.Endpoint != tt.wantPath {
				t.Fatalf("Endpoint = %q, want %q", got.Endpoint, tt.wantPath)
			}
			if got.Format != tt.wantFmt {
				t.Fatalf("Format = %q, want %q", got.Format, tt.wantFmt)
			}
			if got.UsesResp != tt.wantResp {
				t.Fatalf("UsesResp = %v, want %v", got.UsesResp, tt.wantResp)
			}
		})
	}
}

func TestConvertOpenAIChatCompletionsRequestToResponses(t *testing.T) {
	t.Parallel()

	chat := []byte(`{
		"model":"gpt-test",
		"messages":[{"role":"user","content":"hi"}],
		"temperature":0.2,
		"max_tokens":128,
		"stream":false
	}`)
	out := ConvertOpenAIChatCompletionsRequestToResponses("gpt-test", chat, false)
	if gjson.GetBytes(out, "model").String() != "gpt-test" {
		t.Fatalf("model = %s", gjson.GetBytes(out, "model").Raw)
	}
	if !gjson.GetBytes(out, "input").IsArray() {
		t.Fatalf("expected input array, got %s", string(out))
	}
	if gjson.GetBytes(out, "messages").Exists() {
		t.Fatalf("unexpected messages field: %s", string(out))
	}
	if gjson.GetBytes(out, "include").Exists() {
		t.Fatalf("unexpected codex include field: %s", string(out))
	}
	if gjson.GetBytes(out, "temperature").Float() != 0.2 {
		t.Fatalf("temperature = %v", gjson.GetBytes(out, "temperature").Value())
	}
	if gjson.GetBytes(out, "max_output_tokens").Int() != 128 {
		t.Fatalf("max_output_tokens = %v", gjson.GetBytes(out, "max_output_tokens").Value())
	}
	if gjson.GetBytes(out, "stream").Bool() {
		t.Fatalf("stream should be false")
	}
}

func TestConvertOpenAIResponsesObjectToChatCompletionsNonStream(t *testing.T) {
	t.Parallel()

	body := []byte(`{
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
	out := ConvertOpenAIResponsesObjectToChatCompletionsNonStream(t.Context(), "gpt-test", nil, nil, body)
	if gjson.GetBytes(out, "object").String() != "chat.completion" {
		t.Fatalf("object = %s, body=%s", gjson.GetBytes(out, "object").String(), string(out))
	}
	if gjson.GetBytes(out, "choices.0.message.content").String() != "hello" {
		t.Fatalf("content = %q, body=%s", gjson.GetBytes(out, "choices.0.message.content").String(), string(out))
	}
	if gjson.GetBytes(out, "usage.prompt_tokens").Int() != 3 {
		t.Fatalf("prompt_tokens = %v", gjson.GetBytes(out, "usage.prompt_tokens").Value())
	}
}

func TestNormalizeOpenAICompatibilityProtocol(t *testing.T) {
	t.Parallel()
	if got := config.NormalizeOpenAICompatibilityProtocol(""); got != config.OpenAICompatibilityProtocolChatCompletions {
		t.Fatalf("empty = %q", got)
	}
	if got := config.NormalizeOpenAICompatibilityProtocol("Responses"); got != config.OpenAICompatibilityProtocolResponses {
		t.Fatalf("Responses = %q", got)
	}
	if !config.OpenAICompatibilityUsesResponses("openai-response") {
		t.Fatal("expected openai-response alias to enable responses")
	}
}
