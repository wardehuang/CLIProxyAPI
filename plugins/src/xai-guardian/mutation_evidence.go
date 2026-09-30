package main

import (
	"encoding/json"
	"strings"
)

type completedMutationEvidence struct {
	Found          bool
	ScanStartIndex int
	ToolNames      []string
}

func scanCompletedMutationEvidence(originalRequest []byte) completedMutationEvidence {
	result := completedMutationEvidence{ScanStartIndex: -1}
	if len(originalRequest) == 0 {
		return result
	}
	var request map[string]any
	if err := json.Unmarshal(originalRequest, &request); err != nil {
		return result
	}
	input, ok := request["input"].([]any)
	if !ok || len(input) == 0 {
		return result
	}
	lastUser := -1
	for index, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if ok && strings.EqualFold(stringValue(item["role"]), "user") {
			lastUser = index
		}
	}
	if lastUser < 0 {
		return result
	}
	selectedUser := lastUser
	if lastUser > 0 && itemType(input[lastUser-1]) == "function_call_output" {
		selectedUser = -1
		for index := lastUser - 1; index >= 0; index-- {
			if itemRole(input[index]) == "user" {
				selectedUser = index
				break
			}
		}
		if selectedUser < 0 {
			return result
		}
	}
	result.ScanStartIndex = selectedUser
	calls := make(map[string]mutationCall)
	outputs := make(map[string]any)
	for index := selectedUser + 1; index < len(input); index++ {
		item, ok := input[index].(map[string]any)
		if !ok {
			continue
		}
		switch itemType(item) {
		case "function_call":
			callID := strings.TrimSpace(stringValue(item["call_id"]))
			name := mutationToolName(stringValue(item["name"]))
			if callID == "" || name == "" || !isMutationTool(name) || !hasMutationArguments(item["arguments"]) {
				continue
			}
			calls[callID] = mutationCall{Name: name, Arguments: item["arguments"]}
		case "function_call_output":
			callID := strings.TrimSpace(stringValue(item["call_id"]))
			if callID != "" {
				outputs[callID] = item["output"]
			}
		}
	}
	for callID, call := range calls {
		output, ok := outputs[callID]
		if !ok || !mutationOutputSucceeded(call.Name, output) {
			continue
		}
		result.Found = true
		result.ToolNames = append(result.ToolNames, call.Name)
	}
	return result
}

type mutationCall struct {
	Name      string
	Arguments any
}

func itemRole(raw any) string {
	item, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(stringValue(item["role"])))
}

func itemType(raw any) string {
	item, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
}

func mutationToolName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if index := strings.LastIndexByte(name, '.'); index >= 0 {
		name = name[index+1:]
	}
	return name
}

func isMutationTool(name string) bool {
	switch mutationToolName(name) {
	case "patch", "write_file", "skill_manage":
		return true
	default:
		return false
	}
}

func hasMutationArguments(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return value != nil
	}
}

func mutationOutputSucceeded(name string, value any) bool {
	decoded := decodeJSONValue(value)
	object, ok := decoded.(map[string]any)
	if !ok {
		return false
	}
	switch mutationToolName(name) {
	case "write_file":
		return boolValue(object["verified"])
	case "patch", "skill_manage":
		return boolValue(object["success"])
	default:
		return false
	}
}

func decodeJSONValue(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) == nil {
		return decoded
	}
	return value
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}
