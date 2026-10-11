package main

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const completedToolResultWrapupAfterCrossAuthReason = "completed_tool_result_wrapup_after_cross_auth"

type completedToolResultEvidence struct {
	Found          bool
	ScanStartIndex int
	ToolNames      []string
}

func hasZeroThinkingWithoutAction(evidence streamEvidence) bool {
	return evidence.ReasoningTokens == 0 && evidence.SummaryChars == 0 && evidence.EncryptedBytes == 0 &&
		!evidence.ReasoningDelta && evidence.ReasoningItemID == "" && !evidence.ReasoningItemCompleted &&
		!evidence.ReasoningMetadataError && !evidence.NonTextOutput && evidence.CompletedMessage &&
		!evidence.RefusalDetected && strings.TrimSpace(evidence.OutputText) != ""
}

func crossAuthToolResultWrapup(evidence streamEvidence, completion pluginapi.XAIStreamCompletionRequest, settings pluginSettings) completedToolResultEvidence {
	result := completedToolResultEvidence{ScanStartIndex: -1}
	previousAuth := strings.TrimSpace(stringValue(completion.Metadata[pluginapi.XAIStreamPreviousZeroThinkingAuthIndexMetadataKey]))
	if previousAuth == "" || strings.TrimSpace(completion.AuthIndex) == "" || previousAuth == strings.TrimSpace(completion.AuthIndex) ||
		!hasZeroThinkingWithoutAction(evidence) || evidence.OutputTokens < 1 || evidence.OutputTokens > settings.RealtimeGuardWrapupMaxOutputTokens {
		return result
	}
	return scanCompletedToolResultEvidence(completion.OriginalRequest)
}

// currentUserEvidenceInput is shared with mutation evidence so both rules use
// exactly the same user-turn boundary, including execution-time followups.
func currentUserEvidenceInput(originalRequest []byte) ([]any, int) {
	var request map[string]any
	if json.Unmarshal(originalRequest, &request) != nil {
		return nil, -1
	}
	input, ok := request["input"].([]any)
	if !ok || len(input) == 0 {
		return nil, -1
	}
	lastUser := -1
	for index, rawItem := range input {
		if itemRole(rawItem) == "user" {
			lastUser = index
		}
	}
	if lastUser < 0 {
		return nil, -1
	}
	if lastUser > 0 && itemType(input[lastUser-1]) == "function_call_output" {
		for index := lastUser - 1; index >= 0; index-- {
			if itemRole(input[index]) == "user" {
				return input, index
			}
		}
		return nil, -1
	}
	return input, lastUser
}

func scanCompletedToolResultEvidence(originalRequest []byte) completedToolResultEvidence {
	input, selectedUser := currentUserEvidenceInput(originalRequest)
	result := completedToolResultEvidence{ScanStartIndex: selectedUser}
	if selectedUser < 0 {
		return result
	}
	pending := make(map[string]string)
	seen := make(map[string]struct{})
	batchHasOutput, batchFailed := false, false
	for _, rawItem := range input[selectedUser+1:] {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		switch itemType(item) {
		case "function_call":
			callID := strings.TrimSpace(stringValue(item["call_id"]))
			name := mutationToolName(stringValue(item["name"]))
			if callID == "" || name == "" || !hasMutationArguments(item["arguments"]) {
				result.Found = false
				return result
			}
			if _, duplicate := seen[callID]; duplicate {
				result.Found = false
				return result
			}
			if batchHasOutput {
				// Only the latest tool batch can justify the current wrapup.
				result.Found, batchHasOutput, batchFailed = false, false, false
				result.ToolNames = nil
			}
			seen[callID] = struct{}{}
			pending[callID] = name
		case "function_call_output":
			callID := strings.TrimSpace(stringValue(item["call_id"]))
			name, matched := pending[callID]
			if !matched {
				result.Found = false
				return result
			}
			delete(pending, callID)
			batchHasOutput = true
			succeeded, hasData := toolResultWrapupOutput(name, item["output"])
			batchFailed = batchFailed || !succeeded
			if succeeded && hasData {
				result.Found = true
				result.ToolNames = append(result.ToolNames, name)
			}
		}
	}
	result.Found = result.Found && batchHasOutput && !batchFailed && len(pending) == 0
	return result
}

// Tool output is untrusted external data. A tool-specific success contract is
// required; bookkeeping tools may complete but cannot supply factual evidence.
func toolResultWrapupOutput(name string, value any) (bool, bool) {
	object, ok := decodeJSONValue(value).(map[string]any)
	if !ok || toolResultHasError(object) {
		return false, false
	}
	if name == "clientfn_web_search" {
		name = "web_search"
	} else if suffix, aliased := strings.CutPrefix(name, "clientfn_web_search_"); aliased {
		index, err := strconv.Atoi(suffix)
		if err == nil && index > 0 {
			name = "web_search"
		}
	}
	switch name {
	case "terminal":
		exitCode, valid := toolResultInteger(object["exit_code"])
		succeeded := valid && exitCode == 0
		return succeeded, succeeded && strings.TrimSpace(stringValue(object["output"])) != ""
	case "read_file":
		totalLines, valid := toolResultInteger(object["total_lines"])
		succeeded := valid && totalLines >= 0 && !boolValue(object["is_binary"]) && !boolValue(object["is_image"])
		return succeeded, succeeded && strings.TrimSpace(stringValue(object["content"])) != ""
	case "search_files":
		totalCount, valid := toolResultInteger(object["total_count"])
		if !valid || totalCount < 0 {
			return false, false
		}
		hasData := totalCount > 0 && (nonemptyToolResultArray(object["matches"]) || nonemptyToolResultArray(object["files"]) ||
			strings.TrimSpace(stringValue(object["matches_text"])) != "" || nonemptyToolResultCounts(object["counts"]))
		return true, hasData
	case "web_search":
		data, valid := object["data"].(map[string]any)
		if !valid || !boolValue(object["success"]) {
			return false, false
		}
		results, valid := data["web"].([]any)
		if !valid {
			return false, false
		}
		for _, rawResult := range results {
			result, valid := rawResult.(map[string]any)
			if !valid || strings.TrimSpace(stringValue(result["url"])) == "" {
				return false, false
			}
		}
		return true, len(results) > 0
	case "web_extract":
		results, valid := object["results"].([]any)
		if !valid {
			return false, false
		}
		for _, rawResult := range results {
			result, valid := rawResult.(map[string]any)
			if !valid || toolResultHasError(result) || strings.TrimSpace(stringValue(result["url"])) == "" ||
				strings.TrimSpace(stringValue(result["content"])) == "" {
				return false, false
			}
		}
		return true, len(results) > 0
	default:
		return false, false
	}
}

func toolResultHasError(object map[string]any) bool {
	if value, exists := object["error"]; exists && value != nil && value != false && value != "" {
		return true
	}
	if value, exists := object["success"]; exists && value != true {
		return true
	}
	if boolValue(object["is_error"]) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(stringValue(object["status"]))) {
	case "error", "failed", "failure", "timeout", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func toolResultInteger(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok {
		return 0, false
	}
	integer := int(number)
	return integer, float64(integer) == number
}

func nonemptyToolResultArray(value any) bool {
	items, ok := value.([]any)
	return ok && len(items) > 0
}

func nonemptyToolResultCounts(value any) bool {
	counts, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, count := range counts {
		if number, valid := toolResultInteger(count); valid && number > 0 {
			return true
		}
	}
	return false
}
