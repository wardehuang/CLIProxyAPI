package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func finalizeWebSearchRequest(request pluginapi.RequestFinalizeRequest) (pluginapi.RequestFinalizeResponse, error) {
	if !strings.EqualFold(metadataString(request.Metadata, selectedProviderKey), xaiProviderName) {
		return pluginapi.RequestFinalizeResponse{}, nil
	}

	body, alias, changed, errRewrite := aliasClientWebSearch(request.Body)
	if errRewrite != nil {
		return pluginapi.RequestFinalizeResponse{}, fmt.Errorf("alias xai web_search request: %w", errRewrite)
	}
	if !changed {
		return pluginapi.RequestFinalizeResponse{}, nil
	}
	return pluginapi.RequestFinalizeResponse{
		Body: body,
		Metadata: map[string]any{
			aliasMetadataKey: alias,
		},
	}, nil
}

func interceptWebSearchResponse(request pluginapi.ResponseInterceptRequest) (pluginapi.ResponseInterceptResponse, error) {
	alias := metadataString(request.Metadata, aliasMetadataKey)
	if alias == "" {
		return pluginapi.ResponseInterceptResponse{}, nil
	}
	body, changed, errRewrite := restoreClientWebSearchJSON(request.Body, alias)
	if errRewrite != nil {
		return pluginapi.ResponseInterceptResponse{}, fmt.Errorf("restore xai web_search response: %w", errRewrite)
	}
	if !changed {
		return pluginapi.ResponseInterceptResponse{}, nil
	}
	return pluginapi.ResponseInterceptResponse{Body: body}, nil
}

func interceptWebSearchStreamChunk(request pluginapi.StreamChunkInterceptRequest) (pluginapi.StreamChunkInterceptResponse, error) {
	alias := metadataString(request.Metadata, aliasMetadataKey)
	if alias == "" || len(request.Body) == 0 {
		return pluginapi.StreamChunkInterceptResponse{}, nil
	}
	body, changed, errRewrite := restoreClientWebSearchStream(request.Body, alias)
	if errRewrite != nil {
		return pluginapi.StreamChunkInterceptResponse{}, fmt.Errorf("restore xai web_search stream chunk: %w", errRewrite)
	}
	if !changed {
		return pluginapi.StreamChunkInterceptResponse{}, nil
	}
	return pluginapi.StreamChunkInterceptResponse{Body: body}, nil
}

func aliasClientWebSearch(body []byte) ([]byte, string, bool, error) {
	root, errDecode := decodeJSON(body)
	if errDecode != nil {
		return nil, "", false, errDecode
	}
	object, ok := root.(map[string]any)
	if !ok {
		return nil, "", false, fmt.Errorf("request body root must be an object")
	}
	tools, ok := object["tools"].([]any)
	if !ok {
		return nil, "", false, nil
	}

	occupied := make(map[string]struct{}, len(tools))
	hasClientWebSearch := false
	for _, rawTool := range tools {
		tool, okTool := rawTool.(map[string]any)
		if !okTool || stringField(tool, "type") != "function" {
			continue
		}
		name := stringField(tool, "name")
		if name == "" {
			continue
		}
		occupied[name] = struct{}{}
		if name == clientWebSearchName {
			hasClientWebSearch = true
		}
	}
	if !hasClientWebSearch {
		return nil, "", false, nil
	}

	alias := availableAlias(occupied)
	for _, rawTool := range tools {
		tool, okTool := rawTool.(map[string]any)
		if okTool && stringField(tool, "type") == "function" && stringField(tool, "name") == clientWebSearchName {
			tool["name"] = alias
		}
	}
	rewriteRequestToolChoice(object["tool_choice"], alias)
	rewriteRequestFunctionCallHistory(object["input"], alias)

	rewritten, errMarshal := json.Marshal(object)
	if errMarshal != nil {
		return nil, "", false, errMarshal
	}
	return rewritten, alias, true, nil
}

func availableAlias(occupied map[string]struct{}) string {
	if _, exists := occupied[aliasBaseName]; !exists {
		return aliasBaseName
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s_%d", aliasBaseName, suffix)
		if _, exists := occupied[candidate]; !exists {
			return candidate
		}
	}
}

func rewriteRequestToolChoice(value any, alias string) {
	choice, ok := value.(map[string]any)
	if !ok {
		return
	}
	if stringField(choice, "type") == "function" && stringField(choice, "name") == clientWebSearchName {
		choice["name"] = alias
	}
	allowed, ok := choice["tools"].([]any)
	if !ok {
		return
	}
	for _, rawTool := range allowed {
		tool, okTool := rawTool.(map[string]any)
		if okTool && stringField(tool, "type") == "function" && stringField(tool, "name") == clientWebSearchName {
			tool["name"] = alias
		}
	}
}

func rewriteRequestFunctionCallHistory(value any, alias string) {
	input, ok := value.([]any)
	if !ok {
		return
	}
	for _, rawItem := range input {
		item, okItem := rawItem.(map[string]any)
		if okItem && stringField(item, "type") == "function_call" && stringField(item, "name") == clientWebSearchName {
			item["name"] = alias
		}
	}
}

func restoreClientWebSearchStream(body []byte, alias string) ([]byte, bool, error) {
	if json.Valid(body) {
		return restoreClientWebSearchJSON(body, alias)
	}

	lines := bytes.SplitAfter(body, []byte("\n"))
	changed := false
	for index, line := range lines {
		contentLength := len(line)
		if contentLength > 0 && line[contentLength-1] == '\n' {
			contentLength--
		}
		if contentLength > 0 && line[contentLength-1] == '\r' {
			contentLength--
		}
		content := line[:contentLength]
		trimmed := bytes.TrimLeft(content, " \t")
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		prefixLength := len(content) - len(trimmed) + len("data:")
		for prefixLength < len(content) && (content[prefixLength] == ' ' || content[prefixLength] == '\t') {
			prefixLength++
		}
		payload := content[prefixLength:]
		if bytes.Equal(payload, []byte("[DONE]")) || !json.Valid(payload) {
			continue
		}
		rewritten, lineChanged, errRewrite := restoreClientWebSearchJSON(payload, alias)
		if errRewrite != nil {
			return nil, false, errRewrite
		}
		if !lineChanged {
			continue
		}
		rebuilt := make([]byte, 0, len(line)-len(payload)+len(rewritten))
		rebuilt = append(rebuilt, content[:prefixLength]...)
		rebuilt = append(rebuilt, rewritten...)
		rebuilt = append(rebuilt, line[contentLength:]...)
		lines[index] = rebuilt
		changed = true
	}
	if !changed {
		return nil, false, nil
	}
	return bytes.Join(lines, nil), true, nil
}

func restoreClientWebSearchJSON(body []byte, alias string) ([]byte, bool, error) {
	root, errDecode := decodeJSON(body)
	if errDecode != nil {
		return nil, false, errDecode
	}
	if !restoreAliasInValue(root, alias) {
		return nil, false, nil
	}
	rewritten, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return nil, false, errMarshal
	}
	return rewritten, true, nil
}

func restoreAliasInValue(value any, alias string) bool {
	changed := false
	switch node := value.(type) {
	case map[string]any:
		typeName := stringField(node, "type")
		switch typeName {
		case "function", "function_call", "tool_call", "tool_use":
			if stringField(node, "name") == alias {
				node["name"] = clientWebSearchName
				changed = true
			}
		}
		for _, field := range []string{"function", "function_call", "functionCall"} {
			function, ok := node[field].(map[string]any)
			if ok && stringField(function, "name") == alias {
				function["name"] = clientWebSearchName
				changed = true
			}
		}
		for _, child := range node {
			changed = restoreAliasInValue(child, alias) || changed
		}
	case []any:
		for _, child := range node {
			changed = restoreAliasInValue(child, alias) || changed
		}
	}
	return changed
}

func decodeJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return nil, errDecode
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		return nil, fmt.Errorf("JSON contains trailing data")
	}
	return value, nil
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}
