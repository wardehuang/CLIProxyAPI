package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type toolsShape string

const (
	toolsShapeAnthropic    toolsShape = "anthropic"
	toolsShapeOpenAIFlat   toolsShape = "openai_flat"
	toolsShapeOpenAINested toolsShape = "openai_nested"
)

type patchResult struct {
	Changed       bool
	BeforeBody    []byte
	AfterBody     []byte
	PatchedTools  []string
	InjectedTools []string
	SkippedTools  []string
	MissingTools  []string
}

func interceptRequestBeforeAuth(ctx context.Context, req pluginapi.RequestInterceptRequest, hostCallbackID string) pluginapi.RequestInterceptResponse {
	_ = ctx
	return applySchemaPatchToIntercept(schemaPatchRequestContext{
		Body:           req.Body,
		Model:          req.Model,
		RequestedModel: req.RequestedModel,
		SourceFormat:   req.SourceFormat,
		ToFormat:       req.ToFormat,
		Stream:         req.Stream,
		Stage:          "intercept_before",
		HostCallbackID: hostCallbackID,
		Metadata:       req.Metadata,
	})
}

func interceptRequestAfterAuth(ctx context.Context, req pluginapi.RequestInterceptRequest, hostCallbackID string) pluginapi.RequestInterceptResponse {
	_ = ctx
	return applySchemaPatchToIntercept(schemaPatchRequestContext{
		Body:           req.Body,
		Model:          req.Model,
		RequestedModel: req.RequestedModel,
		SourceFormat:   req.SourceFormat,
		ToFormat:       req.ToFormat,
		Stream:         req.Stream,
		Stage:          "intercept_after",
		HostCallbackID: hostCallbackID,
		Metadata:       req.Metadata,
	})
}

func finalizeRequest(ctx context.Context, req pluginapi.RequestFinalizeRequest, hostCallbackID string) pluginapi.RequestFinalizeResponse {
	_ = ctx
	interceptResponse := applySchemaPatchToIntercept(schemaPatchRequestContext{
		Body:           req.Body,
		Model:          req.Model,
		RequestedModel: req.RequestedModel,
		SourceFormat:   req.SourceFormat,
		ToFormat:       req.ToFormat,
		Stream:         req.Stream,
		Stage:          "finalize",
		HostCallbackID: hostCallbackID,
		Metadata:       req.Metadata,
	})
	return pluginapi.RequestFinalizeResponse{Body: interceptResponse.Body}
}

type schemaPatchRequestContext struct {
	Body           []byte
	Model          string
	RequestedModel string
	SourceFormat   string
	ToFormat       string
	Stream         bool
	Stage          string
	HostCallbackID string
	Metadata       map[string]any
}

func applySchemaPatchToIntercept(req schemaPatchRequestContext) pluginapi.RequestInterceptResponse {
	if len(bytes.TrimSpace(req.Body)) == 0 {
		return pluginapi.RequestInterceptResponse{}
	}
	result, errPatch := patchMcpToolSchemasInRequestBody(req.Body, onlyEmptyEnabled(), injectMissingEnabled())
	if errPatch != nil {
		logPluginWarn(req.HostCallbackID, "mcp schema patch skipped", map[string]any{
			"reason":        "patch_error",
			"error":         errPatch.Error(),
			"stage":         req.Stage,
			"model":         req.Model,
			"source_format": req.SourceFormat,
			"to_format":     req.ToFormat,
		})
		return pluginapi.RequestInterceptResponse{}
	}
	if !result.Changed {
		logPluginDebug(req.HostCallbackID, "mcp schema patch skipped", map[string]any{
			"reason":         "no_change",
			"stage":          req.Stage,
			"model":          req.Model,
			"source_format":  req.SourceFormat,
			"to_format":      req.ToFormat,
			"skipped_tools":  result.SkippedTools,
			"missing_tools":  result.MissingTools,
			"request_id":     metadataString(req.Metadata, "request_id"),
			"registry_count": len(currentSchemaRegistry().toolNames),
		})
		return pluginapi.RequestInterceptResponse{}
	}

	requestID := metadataString(req.Metadata, "request_id")
	requestPath := metadataString(req.Metadata, "request_path")
	if requestPath == "" {
		requestPath = req.SourceFormat
	}

	logPluginInfo(req.HostCallbackID, "mcp tool schemas patched", map[string]any{
		"stage":          req.Stage,
		"model":          req.Model,
		"source_format":  req.SourceFormat,
		"to_format":      req.ToFormat,
		"patched_tools":  result.PatchedTools,
		"injected_tools": result.InjectedTools,
		"skipped_tools":  result.SkippedTools,
		"missing_tools":  result.MissingTools,
		"request_id":     requestID,
		"request_path":   requestPath,
		"before_bytes":   len(result.BeforeBody),
		"after_bytes":    len(result.AfterBody),
	})

	if detailLogEnabled() {
		logPath, errWrite := writePatchDetailLog(patchDetailLogInput{
			RequestID:      requestID,
			RequestPath:    requestPath,
			SourceFormat:   req.SourceFormat,
			ToFormat:       req.ToFormat,
			Model:          req.Model,
			RequestedModel: req.RequestedModel,
			Stream:         req.Stream,
			Stage:          req.Stage,
			Result:         result,
		})
		if errWrite != nil {
			logPluginInfo(req.HostCallbackID, "mcp schema patch detail log failed", map[string]any{
				"error":      errWrite.Error(),
				"request_id": requestID,
				"stage":      req.Stage,
			})
		} else {
			logPluginDebug(req.HostCallbackID, "mcp schema patch detail log written", map[string]any{
				"path":       logPath,
				"request_id": requestID,
				"stage":      req.Stage,
			})
		}
	}

	return pluginapi.RequestInterceptResponse{Body: result.AfterBody}
}

func patchMcpToolSchemasInRequestBody(body []byte, onlyEmpty bool, injectMissing bool) (patchResult, error) {
	result := patchResult{
		BeforeBody: bytes.Clone(body),
	}
	var root any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return result, fmt.Errorf("unmarshal request body: %w", errUnmarshal)
	}
	rootMap, okRootMap := root.(map[string]any)
	if !okRootMap {
		return result, nil
	}

	toolsValue, hasTools := rootMap["tools"]
	if !hasTools {
		if !injectMissing {
			result.AfterBody = bytes.Clone(body)
			return result, nil
		}
		// Client body has no tools array: still inject registry tools in Anthropic shape
		// only when the request looks tool-capable is hard to know; require existing tools[].
		result.AfterBody = bytes.Clone(body)
		return result, nil
	}

	toolsList, okToolsList := toolsValue.([]any)
	if !okToolsList {
		result.AfterBody = bytes.Clone(body)
		return result, nil
	}

	shape := detectToolsShape(toolsList)
	toolsChanged, patched, skipped, missing := patchToolsList(toolsList, onlyEmpty)
	injected := []string{}
	if injectMissing {
		var injectChanged bool
		toolsList, injectChanged, injected = injectMissingTools(toolsList, shape)
		if injectChanged {
			toolsChanged = true
		}
	}

	result.PatchedTools = patched
	result.InjectedTools = injected
	result.SkippedTools = skipped
	result.MissingTools = missing

	if !toolsChanged {
		result.AfterBody = bytes.Clone(body)
		return result, nil
	}

	rootMap["tools"] = toolsList
	afterBody, errMarshal := json.Marshal(rootMap)
	if errMarshal != nil {
		return result, fmt.Errorf("marshal patched request body: %w", errMarshal)
	}
	result.AfterBody = afterBody
	result.Changed = !bytes.Equal(result.BeforeBody, result.AfterBody)
	if !result.Changed {
		result.AfterBody = bytes.Clone(body)
	}
	return result, nil
}

func detectToolsShape(toolsList []any) toolsShape {
	for _, item := range toolsList {
		toolMap, okToolMap := item.(map[string]any)
		if !okToolMap {
			continue
		}
		if functionValue, hasFunction := toolMap["function"]; hasFunction {
			if _, okFunctionMap := functionValue.(map[string]any); okFunctionMap {
				return toolsShapeOpenAINested
			}
		}
		typeValue, _ := toolMap["type"].(string)
		if strings.EqualFold(strings.TrimSpace(typeValue), "function") {
			if _, hasParameters := toolMap["parameters"]; hasParameters {
				return toolsShapeOpenAIFlat
			}
			if _, hasName := toolMap["name"]; hasName {
				return toolsShapeOpenAIFlat
			}
		}
		if _, hasParameters := toolMap["parameters"]; hasParameters {
			if _, hasName := toolMap["name"]; hasName {
				return toolsShapeOpenAIFlat
			}
		}
		if _, hasInputSchema := toolMap["input_schema"]; hasInputSchema {
			return toolsShapeAnthropic
		}
	}
	return toolsShapeAnthropic
}

func patchToolsList(toolsList []any, onlyEmpty bool) (bool, []string, []string, []string) {
	changed := false
	var patched []string
	var skipped []string
	var missing []string
	for index := range toolsList {
		toolMap, okToolMap := toolsList[index].(map[string]any)
		if !okToolMap {
			continue
		}
		toolChanged, toolName, reason := patchOneTool(toolMap, onlyEmpty)
		if toolChanged {
			toolsList[index] = toolMap
			changed = true
			patched = appendUnique(patched, toolName)
			continue
		}
		switch reason {
		case "not_empty", "no_schema_field":
			skipped = appendUnique(skipped, toolName)
		case "not_in_registry":
			if toolName != "" && isEmptyToolSchema(toolMap) {
				missing = appendUnique(missing, toolName)
			}
		}
	}
	return changed, patched, skipped, missing
}

func injectMissingTools(toolsList []any, shape toolsShape) ([]any, bool, []string) {
	presentNames := collectPresentToolNames(toolsList)
	injectedNames := make([]string, 0)
	changed := false
	for _, entry := range allRegisteredTools() {
		if entry.Name == "" || entry.Schema == nil {
			continue
		}
		if _, exists := presentNames[entry.Name]; exists {
			continue
		}
		toolsList = append(toolsList, buildToolObject(entry, shape))
		presentNames[entry.Name] = struct{}{}
		injectedNames = append(injectedNames, entry.Name)
		changed = true
	}
	return toolsList, changed, injectedNames
}

func collectPresentToolNames(toolsList []any) map[string]struct{} {
	present := make(map[string]struct{}, len(toolsList))
	for _, item := range toolsList {
		toolMap, okToolMap := item.(map[string]any)
		if !okToolMap {
			continue
		}
		if functionValue, hasFunction := toolMap["function"]; hasFunction {
			if functionMap, okFunctionMap := functionValue.(map[string]any); okFunctionMap {
				if name := toolNameFromMap(functionMap); name != "" {
					present[name] = struct{}{}
				}
			}
		}
		if name := toolNameFromMap(toolMap); name != "" {
			present[name] = struct{}{}
		}
	}
	return present
}

func buildToolObject(entry registeredTool, shape toolsShape) map[string]any {
	schema := cloneMap(entry.Schema)
	description := strings.TrimSpace(entry.Description)
	switch shape {
	case toolsShapeOpenAINested:
		functionMap := map[string]any{
			"name":       entry.Name,
			"parameters": schema,
		}
		if description != "" {
			functionMap["description"] = description
		}
		return map[string]any{
			"type":     "function",
			"function": functionMap,
		}
	case toolsShapeOpenAIFlat:
		tool := map[string]any{
			"type":       "function",
			"name":       entry.Name,
			"parameters": schema,
		}
		if description != "" {
			tool["description"] = description
		}
		return tool
	default:
		tool := map[string]any{
			"name":         entry.Name,
			"input_schema": schema,
		}
		if description != "" {
			tool["description"] = description
		}
		return tool
	}
}

// patchOneTool mutates toolMap in place when a replacement schema is applied.
// reason: "", "not_empty", "not_in_registry", "no_schema_field", "unnamed"
func patchOneTool(toolMap map[string]any, onlyEmpty bool) (bool, string, string) {
	if toolMap == nil {
		return false, "", "unnamed"
	}

	if functionValue, hasFunction := toolMap["function"]; hasFunction {
		if functionMap, okFunctionMap := functionValue.(map[string]any); okFunctionMap {
			changed, toolName, reason := patchOneTool(functionMap, onlyEmpty)
			if changed {
				toolMap["function"] = functionMap
			}
			return changed, toolName, reason
		}
	}

	toolName := toolNameFromMap(toolMap)
	if toolName == "" {
		return false, "", "unnamed"
	}

	schemaKey, rawSchema, hasSchemaField := locateSchemaField(toolMap)
	if !hasSchemaField {
		registrySchema, exists := lookupToolSchema(toolName)
		if !exists {
			return false, toolName, "not_in_registry"
		}
		// Prefer existing shape field name; default Anthropic input_schema.
		if _, hasParameters := toolMap["type"]; hasParameters {
			if typeString, _ := toolMap["type"].(string); strings.EqualFold(strings.TrimSpace(typeString), "function") {
				toolMap["parameters"] = registrySchema
				return true, toolName, ""
			}
		}
		toolMap["input_schema"] = registrySchema
		return true, toolName, ""
	}

	if onlyEmpty && !isEmptySchemaValue(rawSchema) {
		return false, toolName, "not_empty"
	}

	registrySchema, exists := lookupToolSchema(toolName)
	if !exists {
		return false, toolName, "not_in_registry"
	}

	toolMap[schemaKey] = registrySchema
	return true, toolName, ""
}

func locateSchemaField(toolMap map[string]any) (string, any, bool) {
	for _, key := range []string{"input_schema", "parameters", "arguments", "inputSchema"} {
		if raw, exists := toolMap[key]; exists {
			return key, raw, true
		}
	}
	return "", nil, false
}

func toolNameFromMap(toolMap map[string]any) string {
	if nameValue, hasName := toolMap["name"]; hasName {
		if nameString, okNameString := nameValue.(string); okNameString {
			return strings.TrimSpace(nameString)
		}
	}
	return ""
}

func isEmptyToolSchema(toolMap map[string]any) bool {
	_, rawSchema, hasSchemaField := locateSchemaField(toolMap)
	if !hasSchemaField {
		return true
	}
	return isEmptySchemaValue(rawSchema)
}

func isEmptySchemaValue(raw any) bool {
	if raw == nil {
		return true
	}
	switch typed := raw.(type) {
	case map[string]any:
		return isEmptySchemaObject(typed)
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" || trimmed == "{}" || trimmed == "null" {
			return true
		}
		var decoded any
		if errUnmarshal := json.Unmarshal([]byte(trimmed), &decoded); errUnmarshal != nil {
			return false
		}
		return isEmptySchemaValue(decoded)
	default:
		return false
	}
}

func isEmptySchemaObject(schemaObject map[string]any) bool {
	if schemaObject == nil {
		return true
	}
	if len(schemaObject) == 0 {
		return true
	}

	propertiesValue, hasProperties := schemaObject["properties"]
	if hasProperties {
		switch typedProperties := propertiesValue.(type) {
		case map[string]any:
			if len(typedProperties) > 0 {
				return false
			}
		case nil:
		default:
			return false
		}
	}

	for key, value := range schemaObject {
		switch key {
		case "type":
			typeString, okTypeString := value.(string)
			if okTypeString && strings.EqualFold(strings.TrimSpace(typeString), "object") {
				continue
			}
			if value != nil {
				return false
			}
		case "properties", "required", "additionalProperties", "description", "$schema", "title":
			continue
		default:
			return false
		}
	}
	return true
}

func appendUnique(values []string, addition string) []string {
	addition = strings.TrimSpace(addition)
	if addition == "" {
		return values
	}
	for _, existing := range values {
		if existing == addition {
			return values
		}
	}
	return append(values, addition)
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}
