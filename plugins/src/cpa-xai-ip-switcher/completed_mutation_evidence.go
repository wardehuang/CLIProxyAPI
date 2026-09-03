package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

type completedMutationRequest struct {
	Input json.RawMessage `json:"input"`
}

type completedMutationInputItem struct {
	Type   string `json:"type"`
	Role   string `json:"role"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Output string `json:"output"`
}

type completedMutationToolResult struct {
	Success  bool            `json:"success"`
	Verified bool            `json:"verified"`
	ExitCode *int            `json:"exit_code"`
	Error    json.RawMessage `json:"error"`
}

func hasCompletedMutationEvidence(requestBody []byte) bool {
	var request completedMutationRequest
	if err := json.Unmarshal(requestBody, &request); err != nil {
		return false
	}

	var items []completedMutationInputItem
	if err := json.Unmarshal(request.Input, &items); err != nil {
		return false
	}

	lastUserIndex := -1
	for index := range items {
		if strings.EqualFold(strings.TrimSpace(items[index].Role), "user") {
			lastUserIndex = index
		}
	}
	if lastUserIndex < 0 {
		return false
	}

	scanStartIndex := completedMutationScanStart(items, lastUserIndex)
	mutationCalls := make(map[string]string)
	for index := scanStartIndex; index < len(items); index++ {
		item := items[index]
		switch strings.ToLower(strings.TrimSpace(item.Type)) {
		case "function_call":
			callID := strings.TrimSpace(item.CallID)
			toolName := strings.ToLower(strings.TrimSpace(item.Name))
			if callID == "" || !isMutationToolName(toolName) {
				continue
			}
			mutationCalls[callID] = toolName
		case "function_call_output":
			toolName, exists := mutationCalls[strings.TrimSpace(item.CallID)]
			if exists && completedMutationOutputSucceeded(toolName, item.Output) {
				return true
			}
		}
	}
	return false
}

func completedMutationScanStart(items []completedMutationInputItem, lastUserIndex int) int {
	scanStartIndex := lastUserIndex + 1
	if lastUserIndex == 0 || !strings.EqualFold(strings.TrimSpace(items[lastUserIndex-1].Type), "function_call_output") {
		return scanStartIndex
	}
	for index := lastUserIndex - 1; index >= 0; index-- {
		if strings.EqualFold(strings.TrimSpace(items[index].Role), "user") {
			return index + 1
		}
	}
	return scanStartIndex
}

func isMutationToolName(toolName string) bool {
	switch toolName {
	case "patch", "write_file", "skill_manage", "terminal":
		return true
	default:
		return false
	}
}

func completedMutationOutputSucceeded(toolName, output string) bool {
	var result completedMutationToolResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return false
	}
	if toolName == "write_file" {
		return result.Verified
	}
	if toolName == "terminal" {
		return result.ExitCode != nil && *result.ExitCode == 0 && terminalMutationErrorEmpty(result.Error)
	}
	return result.Success
}

func terminalMutationErrorEmpty(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte(`""`))
}
