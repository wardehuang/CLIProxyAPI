package handlers

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	claudeCodeCompactCodexAlt = "responses/compact"
)

// ClaudeCodeCompactRequest describes a compact override selected for a Claude Code request.
type ClaudeCodeCompactRequest struct {
	Applied        bool
	Provider       string
	ModelName      string
	RequestedModel string
	RawJSON        []byte
	Alt            string
	ForceNonStream bool
}

// PrepareClaudeCodeCompactRequest detects Claude Code compact requests and applies upstream-specific overrides.
func (h *BaseAPIHandler) PrepareClaudeCodeCompactRequest(rawJSON []byte, headers http.Header) ClaudeCodeCompactRequest {
	if !IsClaudeCodeCompactRequest(rawJSON, headers) {
		return ClaudeCodeCompactRequest{}
	}

	originalModel := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	providers, _, errMsg := h.getRequestDetails(originalModel)
	if errMsg != nil {
		return ClaudeCodeCompactRequest{}
	}

	for _, provider := range providers {
		switch strings.ToLower(strings.TrimSpace(provider)) {
		case "codex":
			model := ""
			if h != nil && h.Cfg != nil {
				model = strings.TrimSpace(h.Cfg.CodexCompactModel)
			}
			if model == "" {
				return ClaudeCodeCompactRequest{}
			}
			model = withThinkingSuffix(model, "low")
			raw := applyClaudeCompactModel(rawJSON, model)
			return ClaudeCodeCompactRequest{Applied: true, Provider: "codex", ModelName: model, RequestedModel: originalModel, RawJSON: raw, Alt: claudeCodeCompactCodexAlt, ForceNonStream: true}
		case "antigravity":
			model := ""
			if h != nil && h.Cfg != nil {
				model = strings.TrimSpace(h.Cfg.AntigravityCompactModel)
			}
			if model == "" {
				return ClaudeCodeCompactRequest{}
			}
			model = withThinkingSuffix(model, "none")
			raw := applyClaudeCompactModel(rawJSON, model)
			return ClaudeCodeCompactRequest{Applied: true, Provider: "antigravity", ModelName: model, RequestedModel: originalModel, RawJSON: raw}
		}
	}
	return ClaudeCodeCompactRequest{}
}

// IsClaudeCodeCompactRequest returns true for Claude Code's internal conversation compact request.
func IsClaudeCodeCompactRequest(rawJSON []byte, headers http.Header) bool {
	if !isClaudeCodeInboundRequest(rawJSON, headers) {
		return false
	}
	messages := gjson.GetBytes(rawJSON, "messages").Array()
	if len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	if !last.Exists() || !strings.EqualFold(strings.TrimSpace(last.Get("role").String()), "user") {
		return false
	}
	texts := claudeMessageTextBlocks(last.Get("content"))
	if len(texts) == 0 {
		return false
	}

	joined := strings.Join(texts, "\n")
	if hasClaudeCompactInstruction(joined) {
		return true
	}
	if len(texts) >= 2 && len(texts[len(texts)-1]) >= 2000 && hasClaudeCompactInstruction(texts[len(texts)-1]) {
		return true
	}
	return false
}

func isClaudeCodeInboundRequest(rawJSON []byte, headers http.Header) bool {
	if headers != nil {
		ua := strings.ToLower(strings.TrimSpace(headers.Get("User-Agent")))
		if strings.Contains(ua, "claude-cli") || strings.Contains(ua, "claude-code") || strings.Contains(ua, "claude_code") {
			return true
		}
	}

	if containsClaudeCodeSystemPrompt(gjson.GetBytes(rawJSON, "system")) {
		return true
	}
	for _, msg := range gjson.GetBytes(rawJSON, "messages").Array() {
		if strings.EqualFold(strings.TrimSpace(msg.Get("role").String()), "system") && containsClaudeCodeSystemPrompt(msg.Get("content")) {
			return true
		}
	}
	return false
}

func containsClaudeCodeSystemPrompt(node gjson.Result) bool {
	for _, text := range claudeMessageTextBlocks(node) {
		if strings.Contains(text, "You are Claude Code") {
			return true
		}
	}
	return false
}

func claudeMessageTextBlocks(node gjson.Result) []string {
	if !node.Exists() || node.Type == gjson.Null {
		return nil
	}
	if node.Type == gjson.String {
		return []string{node.String()}
	}
	if !node.IsArray() {
		return nil
	}
	texts := make([]string, 0, len(node.Array()))
	for _, block := range node.Array() {
		if block.Type == gjson.String {
			texts = append(texts, block.String())
			continue
		}
		if block.Get("text").Exists() {
			blockType := strings.TrimSpace(block.Get("type").String())
			if blockType == "" || strings.EqualFold(blockType, "text") {
				texts = append(texts, block.Get("text").String())
			}
		}
	}
	return texts
}

func hasClaudeCompactInstruction(text string) bool {
	if len(text) < 500 {
		return false
	}
	markers := []string{
		"CRITICAL: Respond with TEXT ONLY",
		"Do NOT call any tools",
		"Your task is to create a detailed summary",
		"<analysis>",
		"<summary>",
		"Primary Request and Intent",
		"Pending Tasks",
		"Current Work",
		"Optional Next Step",
	}
	score := 0
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			score++
		}
	}
	return score >= 3 && (strings.Contains(text, "compact") || strings.Contains(text, "summary") || strings.Contains(text, "summar"))
}

func applyClaudeCompactModel(rawJSON []byte, model string) []byte {
	out, err := sjson.SetBytes(rawJSON, "model", model)
	if err != nil {
		return rawJSON
	}
	out, _ = sjson.DeleteBytes(out, "thinking")
	return out
}

func withThinkingSuffix(model, suffix string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	base := strings.TrimSpace(parsed.ModelName)
	if base == "" {
		base = model
	}
	if len(util.GetProviderName(base)) == 0 && parsed.HasSuffix {
		return model
	}
	return base + "(" + suffix + ")"
}
