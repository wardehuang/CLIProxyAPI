package main

import (
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
	Success  bool `json:"success"`
	Verified bool `json:"verified"`
}

type completedMutationEvidence struct {
	Matched        bool
	ScanStartIndex int
	ToolName       string
	CallID         string
}

func scanCompletedMutationEvidence(requestBody []byte) completedMutationEvidence {
	evidence := completedMutationEvidence{ScanStartIndex: -1}
	var request completedMutationRequest
	if err := json.Unmarshal(requestBody, &request); err != nil {
		return evidence
	}

	var items []completedMutationInputItem
	if err := json.Unmarshal(request.Input, &items); err != nil {
		return evidence
	}

	lastUserIndex := -1
	for index := range items {
		if strings.EqualFold(strings.TrimSpace(items[index].Role), "user") {
			lastUserIndex = index
		}
	}
	if lastUserIndex < 0 {
		return evidence
	}

	scanStartIndex := completedMutationScanStart(items, lastUserIndex)
	evidence.ScanStartIndex = scanStartIndex
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
				evidence.Matched = true
				evidence.ToolName = toolName
				evidence.CallID = strings.TrimSpace(item.CallID)
				return evidence
			}
		}
	}
	return evidence
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
	case "patch", "write_file", "skill_manage":
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
	return result.Success
}
