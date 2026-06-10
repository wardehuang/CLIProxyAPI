package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/tidwall/gjson"
)

func TestIsClaudeCodeCompactRequest(t *testing.T) {
	raw := []byte(`{
			"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],
			"messages":[{"role":"user","content":[{"type":"text","text":"你叫什么名字\n"},{"type":"text","text":"CRITICAL: Respond with TEXT ONLY.\nDo NOT call any tools.\nYour task is to create a detailed summary of the conversation so far.\nOutput <analysis> and <summary>.\nPrimary Request and Intent\nPending Tasks\nCurrent Work\nOptional Next Step\nThis compact summary prompt is intentionally long.` + strings.Repeat("x", 2200) + `"}]}]
		}`)

	if !IsClaudeCodeCompactRequest(raw, http.Header{}) {
		t.Fatal("expected Claude Code compact request")
	}
}

func TestIsClaudeCodeCompactRequestRejectsNormalClaudeCodeRequest(t *testing.T) {
	raw := []byte(`{
			"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],
			"messages":[{"role":"user","content":[{"type":"text","text":"你叫什么名字"}]}]
		}`)

	if IsClaudeCodeCompactRequest(raw, http.Header{}) {
		t.Fatal("normal Claude Code request was classified as compact")
	}
}

func TestIsClaudeCodeCompactRequestAllowsTrailingSystemMessage(t *testing.T) {
	raw := []byte(`{
			"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],
			"messages":[
				{"role":"user","content":[{"type":"text","text":"compact没有缓存的吗\n"},{"type":"text","text":"CRITICAL: Respond with TEXT ONLY.\nDo NOT call any tools.\nYour task is to create a detailed summary of the conversation so far.\nOutput <analysis> and <summary>.\nPrimary Request and Intent\nPending Tasks\nCurrent Work\nOptional Next Step\nThis compact summary prompt is intentionally long.` + strings.Repeat("x", 2200) + `"}]},
				{"role":"system","content":[{"type":"text","text":"trailing system context after the compact request"}]}
			]
		}`)

	if !IsClaudeCodeCompactRequest(raw, http.Header{}) {
		t.Fatal("expected compact request even with trailing system message")
	}
}

func TestIsClaudeCodeCompactRequestIgnoresTrailingSystemCompactText(t *testing.T) {
	raw := []byte(`{
			"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],
			"messages":[
				{"role":"user","content":[{"type":"text","text":"你叫什么名字"}]},
				{"role":"system","content":[{"type":"text","text":"CRITICAL: Respond with TEXT ONLY.\nDo NOT call any tools.\nYour task is to create a detailed summary of the conversation so far.\nOutput <analysis> and <summary>.\nPrimary Request and Intent\nPending Tasks\nCurrent Work\nOptional Next Step\nThis compact summary prompt is intentionally long.` + strings.Repeat("x", 2200) + `"}]}
			]
		}`)

	if IsClaudeCodeCompactRequest(raw, http.Header{}) {
		t.Fatal("trailing system compact-like text should not classify a normal user request as compact")
	}
}

func TestPrepareClaudeCodeCompactRequestCodex(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("compact-codex-test", "codex", []*registry.ModelInfo{{ID: "gpt-original"}})
	t.Cleanup(func() { reg.UnregisterClient("compact-codex-test") })

	h := NewBaseAPIHandlers(&config.SDKConfig{CodexCompactModel: "gpt-5.4-mini"}, nil)
	route := h.PrepareClaudeCodeCompactRequest(compactClaudeRequest("gpt-original"), http.Header{})
	if !route.Applied {
		t.Fatal("compact route was not applied")
	}
	if route.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", route.Provider)
	}
	if route.Alt != "" || route.ForceNonStream {
		t.Fatalf("alt=%q forceNonStream=%v", route.Alt, route.ForceNonStream)
	}
	if route.ModelName != "gpt-5.4-mini" {
		t.Fatalf("model = %q, want gpt-5.4-mini", route.ModelName)
	}
	if route.RequestedModel != "gpt-original" {
		t.Fatalf("requested model = %q, want gpt-original", route.RequestedModel)
	}
	if !gjson.GetBytes(route.RawJSON, "stream").Bool() {
		t.Fatalf("stream should be preserved for codex compact payload: %s", string(route.RawJSON))
	}
	if gjson.GetBytes(route.RawJSON, "model").String() != "gpt-5.4-mini" {
		t.Fatalf("payload model = %q", gjson.GetBytes(route.RawJSON, "model").String())
	}
	if gjson.GetBytes(route.RawJSON, "thinking").Exists() {
		t.Fatalf("thinking should be removed for compact payload: %s", string(route.RawJSON))
	}
	if got := gjson.GetBytes(route.RawJSON, "output_config.effort").String(); got != "low" {
		t.Fatalf("output_config.effort = %q, want low", got)
	}
}

func TestPrepareClaudeCodeCompactRequestAntigravity(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("compact-antigravity-test", "antigravity", []*registry.ModelInfo{{ID: "gemini-original"}})
	t.Cleanup(func() { reg.UnregisterClient("compact-antigravity-test") })

	h := NewBaseAPIHandlers(&config.SDKConfig{AntigravityCompactModel: "gemini-3.1-flash-lite"}, nil)
	route := h.PrepareClaudeCodeCompactRequest(compactClaudeRequest("gemini-original"), http.Header{})
	if !route.Applied {
		t.Fatal("compact route was not applied")
	}
	if route.Provider != "antigravity" {
		t.Fatalf("provider = %q, want antigravity", route.Provider)
	}
	if route.Alt != "" || route.ForceNonStream {
		t.Fatalf("alt=%q forceNonStream=%v", route.Alt, route.ForceNonStream)
	}
	if route.ModelName != "gemini-3.1-flash-lite" {
		t.Fatalf("model = %q, want gemini-3.1-flash-lite", route.ModelName)
	}
	if route.RequestedModel != "gemini-original" {
		t.Fatalf("requested model = %q, want gemini-original", route.RequestedModel)
	}
	if !gjson.GetBytes(route.RawJSON, "stream").Bool() {
		t.Fatalf("stream should be preserved for antigravity compact payload: %s", string(route.RawJSON))
	}
	if gjson.GetBytes(route.RawJSON, "model").String() != "gemini-3.1-flash-lite" {
		t.Fatalf("payload model = %q", gjson.GetBytes(route.RawJSON, "model").String())
	}
	if gjson.GetBytes(route.RawJSON, "thinking").Exists() {
		t.Fatalf("thinking should be removed for compact payload: %s", string(route.RawJSON))
	}
	if got := gjson.GetBytes(route.RawJSON, "output_config.effort").String(); got != "low" {
		t.Fatalf("output_config.effort = %q, want low", got)
	}
}

func TestPrepareClaudeCodeCompactRequestWithoutConfiguredOverride(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("compact-no-override-test", "codex", []*registry.ModelInfo{{ID: "gpt-original"}})
	t.Cleanup(func() { reg.UnregisterClient("compact-no-override-test") })

	h := NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	route := h.PrepareClaudeCodeCompactRequest(compactClaudeRequest("gpt-original"), http.Header{})
	if !route.Applied {
		t.Fatal("compact route should apply without a configured replacement model")
	}
	if route.ModelName != "gpt-original" {
		t.Fatalf("model = %q, want gpt-original", route.ModelName)
	}
	if route.RequestedModel != "gpt-original" {
		t.Fatalf("requested model = %q, want gpt-original", route.RequestedModel)
	}
}

func compactClaudeRequest(model string) []byte {
	return []byte(`{
			"model":"` + model + `",
			"stream":true,
			"thinking":{"type":"enabled","budget_tokens":1024},
			"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],
			"messages":[{"role":"user","content":[{"type":"text","text":"你叫什么名字\n"},{"type":"text","text":"CRITICAL: Respond with TEXT ONLY.\nDo NOT call any tools.\nYour task is to create a detailed summary of the conversation so far.\nOutput <analysis> and <summary>.\nPrimary Request and Intent\nPending Tasks\nCurrent Work\nOptional Next Step\nThis compact summary prompt is intentionally long.` + strings.Repeat("x", 2200) + `"}]}]
		}`)
}
