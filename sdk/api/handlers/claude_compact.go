package handlers

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	claudeCodeCompactCodexAlt = "responses/compact"
)

// ClaudeCodeCompactRequest describes a compact override selected for a compact/summarize request.
type ClaudeCodeCompactRequest struct {
	Applied        bool
	Provider       string
	ModelName      string
	RequestedModel string
	RawJSON        []byte
	Alt            string
	ForceNonStream bool
}

// PrepareClaudeCodeCompactRequest detects Claude Messages compact/summarize requests and applies upstream-specific overrides.
func (h *BaseAPIHandler) PrepareClaudeCodeCompactRequest(rawJSON []byte, headers http.Header) ClaudeCodeCompactRequest {
	if !IsClaudeMessagesCompactRequest(rawJSON, headers) {
		return ClaudeCodeCompactRequest{}
	}
	return h.prepareCompactRequest(rawJSON, applyClaudeCompactModel)
}

// PrepareOpenAIResponsesCompactRequest detects OpenAI Responses summarize requests and applies upstream-specific overrides.
func (h *BaseAPIHandler) PrepareOpenAIResponsesCompactRequest(rawJSON []byte, headers http.Header) ClaudeCodeCompactRequest {
	if !IsOpenAIResponsesSummarizationRequest(rawJSON, headers) {
		return ClaudeCodeCompactRequest{}
	}
	return h.prepareCompactRequest(rawJSON, applyOpenAIResponsesCompactModel)
}

func (h *BaseAPIHandler) prepareCompactRequest(rawJSON []byte, rewrite func([]byte, string, string) []byte) ClaudeCodeCompactRequest {
	originalModel := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	providers, _, errMsg := h.getRequestDetails(originalModel)
	if errMsg != nil {
		return ClaudeCodeCompactRequest{}
	}

	for _, provider := range providers {
		switch strings.ToLower(strings.TrimSpace(provider)) {
		case "codex":
			modelName := compactModelOverride(originalModel, h.compactCodexModel())
			raw := rewrite(rawJSON, modelName, "low")
			return ClaudeCodeCompactRequest{Applied: true, Provider: "codex", ModelName: modelName, RequestedModel: originalModel, RawJSON: raw}
		case "antigravity":
			modelName := compactModelOverride(originalModel, h.compactAntigravityModel())
			raw := rewrite(rawJSON, modelName, "low")
			return ClaudeCodeCompactRequest{Applied: true, Provider: "antigravity", ModelName: modelName, RequestedModel: originalModel, RawJSON: raw}
		}
	}
	return ClaudeCodeCompactRequest{}
}

// IsClaudeMessagesCompactRequest returns true for Claude Messages compact/summarize requests.
func IsClaudeMessagesCompactRequest(rawJSON []byte, headers http.Header) bool {
	return IsClaudeCodeCompactRequest(rawJSON, headers) || IsClaudeMessagesSummarizationRequest(rawJSON)
}

// IsClaudeMessagesSummarizationRequest returns true for Cursor-style summarize requests sent to /v1/messages.
func IsClaudeMessagesSummarizationRequest(rawJSON []byte) bool {
	texts := claudeLastUserMessageTextBlocks(rawJSON)
	if len(texts) == 0 {
		return false
	}
	return hasSummarizationRequestTag(strings.Join(texts, "\n"))
}

// IsOpenAIResponsesSummarizationRequest returns true for Cursor-style summarize requests sent to /v1/responses.
func IsOpenAIResponsesSummarizationRequest(rawJSON []byte, _ http.Header) bool {
	texts := openAIResponsesLastUserInputTextBlocks(rawJSON)
	if len(texts) == 0 {
		return false
	}
	return hasSummarizationRequestTag(strings.Join(texts, "\n"))
}

// IsClaudeCodeCompactRequest returns true for Claude Code's internal conversation compact request.
func IsClaudeCodeCompactRequest(rawJSON []byte, headers http.Header) bool {
	if !isClaudeCodeInboundRequest(rawJSON, headers) {
		return false
	}
	texts := claudeLastUserMessageTextBlocks(rawJSON)
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

func claudeLastUserMessageTextBlocks(rawJSON []byte) []string {
	messages := gjson.GetBytes(rawJSON, "messages").Array()
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if strings.EqualFold(strings.TrimSpace(msg.Get("role").String()), "user") {
			return claudeMessageTextBlocks(msg.Get("content"))
		}
	}
	return nil
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

func openAIResponsesLastUserInputTextBlocks(rawJSON []byte) []string {
	input := gjson.GetBytes(rawJSON, "input")
	if input.Type == gjson.String {
		return []string{input.String()}
	}
	if !input.IsArray() {
		return nil
	}
	items := input.Array()
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if !strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "user") {
			continue
		}
		if texts := openAIResponsesContentTextBlocks(item.Get("content")); len(texts) > 0 {
			return texts
		}
	}
	return openAIResponsesContentTextBlocks(input)
}

func openAIResponsesContentTextBlocks(node gjson.Result) []string {
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
			if blockType == "" || strings.EqualFold(blockType, "text") || strings.EqualFold(blockType, "input_text") {
				texts = append(texts, block.Get("text").String())
			}
		}
	}
	return texts
}

func (h *BaseAPIHandler) compactCodexModel() string {
	if h == nil || h.Cfg == nil {
		return ""
	}
	return strings.TrimSpace(h.Cfg.CodexCompactModel)
}

func (h *BaseAPIHandler) compactAntigravityModel() string {
	if h == nil || h.Cfg == nil {
		return ""
	}
	return strings.TrimSpace(h.Cfg.AntigravityCompactModel)
}

func compactModelOverride(originalModel, overrideModel string) string {
	overrideModel = strings.TrimSpace(overrideModel)
	if overrideModel != "" {
		return overrideModel
	}
	return originalModel
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

func hasSummarizationRequestTag(text string) bool {
	return strings.Contains(text, "<summarization_request>") && strings.Contains(text, "</summarization_request>")
}

func applyClaudeCompactModel(rawJSON []byte, model string, effort string) []byte {
	out, err := sjson.SetBytes(rawJSON, "model", model)
	if err != nil {
		return rawJSON
	}
	out, _ = sjson.DeleteBytes(out, "thinking")
	if effort != "" {
		out, _ = sjson.SetBytes(out, "output_config.effort", effort)
	}
	return out
}

func applyOpenAIResponsesCompactModel(rawJSON []byte, model string, effort string) []byte {
	out, err := sjson.SetBytes(rawJSON, "model", model)
	if err != nil {
		return rawJSON
	}
	out, _ = sjson.DeleteBytes(out, "thinking")
	if effort != "" {
		out, _ = sjson.SetBytes(out, "reasoning.effort", effort)
	}
	return out
}
