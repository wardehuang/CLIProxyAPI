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

func TestPrepareClaudeCodeCompactRequestCodex(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("compact-codex-test", "codex", []*registry.ModelInfo{{ID: "gpt-original"}, {ID: "gpt-compact"}})
	t.Cleanup(func() { reg.UnregisterClient("compact-codex-test") })

	h := NewBaseAPIHandlers(&config.SDKConfig{CodexCompactModel: "gpt-compact"}, nil)
	route := h.PrepareClaudeCodeCompactRequest(compactClaudeRequest("gpt-original"), http.Header{})
	if !route.Applied {
		t.Fatal("compact route was not applied")
	}
	if route.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", route.Provider)
	}
	if route.Alt != "responses/compact" || !route.ForceNonStream {
		t.Fatalf("alt=%q forceNonStream=%v", route.Alt, route.ForceNonStream)
	}
	if route.ModelName != "gpt-compact(low)" {
		t.Fatalf("model = %q, want gpt-compact(low)", route.ModelName)
	}
	if route.RequestedModel != "gpt-original" {
		t.Fatalf("requested model = %q, want gpt-original", route.RequestedModel)
	}
	if !gjson.GetBytes(route.RawJSON, "stream").Bool() {
		t.Fatalf("stream should be preserved for codex compact payload: %s", string(route.RawJSON))
	}
	if gjson.GetBytes(route.RawJSON, "model").String() != "gpt-compact(low)" {
		t.Fatalf("payload model = %q", gjson.GetBytes(route.RawJSON, "model").String())
	}
}

func TestPrepareClaudeCodeCompactRequestAntigravity(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("compact-antigravity-test", "antigravity", []*registry.ModelInfo{{ID: "gemini-original"}, {ID: "ag-compact"}})
	t.Cleanup(func() { reg.UnregisterClient("compact-antigravity-test") })

	h := NewBaseAPIHandlers(&config.SDKConfig{AntigravityCompactModel: "ag-compact"}, nil)
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
	if route.ModelName != "ag-compact(none)" {
		t.Fatalf("model = %q, want ag-compact(none)", route.ModelName)
	}
	if !gjson.GetBytes(route.RawJSON, "stream").Bool() {
		t.Fatalf("stream should be preserved for antigravity compact payload: %s", string(route.RawJSON))
	}
}

func TestPrepareClaudeCodeCompactRequestSkipsEmptyModel(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("compact-empty-test", "codex", []*registry.ModelInfo{{ID: "gpt-original"}})
	t.Cleanup(func() { reg.UnregisterClient("compact-empty-test") })

	h := NewBaseAPIHandlers(&config.SDKConfig{CodexCompactModel: ""}, nil)
	if route := h.PrepareClaudeCodeCompactRequest(compactClaudeRequest("gpt-original"), http.Header{}); route.Applied {
		t.Fatal("empty compact model should skip override")
	}
}

func compactClaudeRequest(model string) []byte {
	return []byte(`{
		"model":"` + model + `",
		"stream":true,
		"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],
		"messages":[{"role":"user","content":[{"type":"text","text":"你叫什么名字\n"},{"type":"text","text":"CRITICAL: Respond with TEXT ONLY.\nDo NOT call any tools.\nYour task is to create a detailed summary of the conversation so far.\nOutput <analysis> and <summary>.\nPrimary Request and Intent\nPending Tasks\nCurrent Work\nOptional Next Step\nThis compact summary prompt is intentionally long.` + strings.Repeat("x", 2200) + `"}]}]
	}`)
}
