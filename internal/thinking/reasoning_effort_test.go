package thinking

import "testing"

func TestExtractReasoningEffortUsesSuffixOverBody(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"reasoning_effort":"low"}`), "openai", "gpt-5.4(high)")
	if got != "high" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "high")
	}
}

func TestExtractReasoningEffortConvertsBudgetToLevel(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"thinking":{"type":"enabled","budget_tokens":8192}}`), "claude", "claude-sonnet-4-5")
	if got != "medium" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "medium")
	}
}

func TestExtractReasoningEffortSupportsOpenAIResponses(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"reasoning":{"effort":"medium"}}`), "openai-response", "gpt-5.4")
	if got != "medium" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "medium")
	}
}

func TestExtractReasoningEffortMissingConfigIsEmpty(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "openai", "gpt-5.4")
	if got != "" {
		t.Fatalf("ExtractReasoningEffort() = %q, want empty", got)
	}
}

func TestExtractThinkingEnabledUsesSuffixOverBody(t *testing.T) {
	got := ExtractThinkingEnabled([]byte(`{"reasoning_effort":"high"}`), "openai", "gpt-5.4(none)")
	if got {
		t.Fatal("ExtractThinkingEnabled() = true, want false")
	}
}

func TestExtractThinkingEnabledCodexAlwaysTrue(t *testing.T) {
	got := ExtractThinkingEnabled([]byte(`{"reasoning":{"effort":"none"}}`), "codex", "gpt-5.4(none)")
	if !got {
		t.Fatal("ExtractThinkingEnabled() = false, want true for codex")
	}
}

func TestExtractTranslatedThinkingEnabledCodexAlwaysTrue(t *testing.T) {
	got := ExtractTranslatedThinkingEnabled([]byte(`{"input":[{"role":"user","content":"hi"}]}`), "codex")
	if !got {
		t.Fatal("ExtractTranslatedThinkingEnabled() = false, want true for codex")
	}
}

func TestExtractThinkingEnabledReportsEnabledConfig(t *testing.T) {
	got := ExtractThinkingEnabled([]byte(`{"thinking":{"type":"enabled","budget_tokens":8192}}`), "claude", "claude-sonnet-4-5")
	if !got {
		t.Fatal("ExtractThinkingEnabled() = false, want true")
	}
}

func TestExtractThinkingEnabledReportsMissingConfigFalse(t *testing.T) {
	got := ExtractThinkingEnabled([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "openai", "gpt-5.4")
	if got {
		t.Fatal("ExtractThinkingEnabled() = true, want false")
	}
}
