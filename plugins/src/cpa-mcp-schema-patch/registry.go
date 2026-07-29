package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// registeredTool is one MCP tool loaded from local JSON.
type registeredTool struct {
	Name        string
	Description string
	Schema      map[string]any
}

type schemaRegistrySnapshot struct {
	// byToolName maps request tool name (e.g. user-claude-mem-search) to full tool entry.
	byToolName map[string]registeredTool
	toolNames  []string
	fileCount  int
	errors     []string
}

type schemaLoadResult struct {
	ToolCount int
	FileCount int
	ToolNames []string
	Errors    []string
}

var schemaRegistryState = struct {
	sync.RWMutex
	snapshot schemaRegistrySnapshot
}{
	snapshot: schemaRegistrySnapshot{
		byToolName: map[string]registeredTool{},
	},
}

func reloadSchemaRegistry(schemasDir string) schemaLoadResult {
	snapshot := loadSchemaRegistryFromDir(schemasDir)
	schemaRegistryState.Lock()
	schemaRegistryState.snapshot = snapshot
	schemaRegistryState.Unlock()
	return schemaLoadResult{
		ToolCount: len(snapshot.toolNames),
		FileCount: snapshot.fileCount,
		ToolNames: append([]string(nil), snapshot.toolNames...),
		Errors:    append([]string(nil), snapshot.errors...),
	}
}

func currentSchemaRegistry() schemaRegistrySnapshot {
	schemaRegistryState.RLock()
	defer schemaRegistryState.RUnlock()
	copied := schemaRegistrySnapshot{
		byToolName: make(map[string]registeredTool, len(schemaRegistryState.snapshot.byToolName)),
		toolNames:  append([]string(nil), schemaRegistryState.snapshot.toolNames...),
		fileCount:  schemaRegistryState.snapshot.fileCount,
		errors:     append([]string(nil), schemaRegistryState.snapshot.errors...),
	}
	for toolName, entry := range schemaRegistryState.snapshot.byToolName {
		copied.byToolName[toolName] = cloneRegisteredTool(entry)
	}
	return copied
}

func lookupRegisteredTool(toolName string) (registeredTool, bool) {
	trimmedName := strings.TrimSpace(toolName)
	if trimmedName == "" {
		return registeredTool{}, false
	}
	schemaRegistryState.RLock()
	defer schemaRegistryState.RUnlock()
	entry, exists := schemaRegistryState.snapshot.byToolName[trimmedName]
	if !exists || entry.Schema == nil {
		return registeredTool{}, false
	}
	return cloneRegisteredTool(entry), true
}

func lookupToolSchema(toolName string) (map[string]any, bool) {
	entry, exists := lookupRegisteredTool(toolName)
	if !exists {
		return nil, false
	}
	return entry.Schema, true
}

func allRegisteredTools() []registeredTool {
	snapshot := currentSchemaRegistry()
	out := make([]registeredTool, 0, len(snapshot.toolNames))
	for _, toolName := range snapshot.toolNames {
		entry, exists := snapshot.byToolName[toolName]
		if !exists {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func loadSchemaRegistryFromDir(schemasDir string) schemaRegistrySnapshot {
	snapshot := schemaRegistrySnapshot{
		byToolName: map[string]registeredTool{},
	}
	trimmedDir := strings.TrimSpace(schemasDir)
	if trimmedDir == "" {
		snapshot.errors = append(snapshot.errors, "schemas-dir is empty")
		return snapshot
	}
	dirInfo, errStat := os.Stat(trimmedDir)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			snapshot.errors = append(snapshot.errors, fmt.Sprintf("schemas-dir does not exist: %s", trimmedDir))
			return snapshot
		}
		snapshot.errors = append(snapshot.errors, fmt.Sprintf("stat schemas-dir failed: %v", errStat))
		return snapshot
	}
	if !dirInfo.IsDir() {
		snapshot.errors = append(snapshot.errors, fmt.Sprintf("schemas-dir is not a directory: %s", trimmedDir))
		return snapshot
	}

	errWalk := filepath.WalkDir(trimmedDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			snapshot.errors = append(snapshot.errors, fmt.Sprintf("walk %s: %v", path, walkErr))
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			return nil
		}
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			snapshot.errors = append(snapshot.errors, fmt.Sprintf("read %s: %v", path, errRead))
			return nil
		}
		relativePath, errRel := filepath.Rel(trimmedDir, path)
		if errRel != nil {
			relativePath = path
		}
		loadedCount, loadErrors := ingestSchemaJSON(raw, relativePath, snapshot.byToolName)
		snapshot.fileCount++
		if loadedCount == 0 && len(loadErrors) == 0 {
			snapshot.errors = append(snapshot.errors, fmt.Sprintf("%s: no tool schema extracted", relativePath))
		}
		snapshot.errors = append(snapshot.errors, loadErrors...)
		return nil
	})
	if errWalk != nil {
		snapshot.errors = append(snapshot.errors, fmt.Sprintf("walk schemas-dir failed: %v", errWalk))
	}

	toolNames := make([]string, 0, len(snapshot.byToolName))
	for toolName := range snapshot.byToolName {
		toolNames = append(toolNames, toolName)
	}
	sort.Strings(toolNames)
	snapshot.toolNames = toolNames
	return snapshot
}

// ingestSchemaJSON accepts:
//  1. Cursor MCP tool descriptor: {"name":"search","description":"...","arguments":{...}}
//  2. Anthropic tool: {"name":"...","input_schema":{...}}
//  3. OpenAI function: {"name":"...","parameters":{...}} or {"type":"function","function":{...}}
//  4. Registry map: {"user-claude-mem-search":{schema}, ...}
//  5. Registry wrapper: {"tools":{...}} or {"schemas":{...}}
func ingestSchemaJSON(raw []byte, relativePath string, destination map[string]registeredTool) (int, []string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0, []string{fmt.Sprintf("%s: empty file", relativePath)}
	}

	var root any
	if errUnmarshal := json.Unmarshal(trimmed, &root); errUnmarshal != nil {
		return 0, []string{fmt.Sprintf("%s: invalid json: %v", relativePath, errUnmarshal)}
	}

	rootMap, okRootMap := root.(map[string]any)
	if !okRootMap {
		return 0, []string{fmt.Sprintf("%s: root must be a JSON object", relativePath)}
	}

	loaded := 0
	var errors []string

	if toolsValue, hasTools := rootMap["tools"]; hasTools {
		if toolsMap, okToolsMap := toolsValue.(map[string]any); okToolsMap {
			count, loadErrors := ingestNameToSchemaMap(toolsMap, relativePath, destination)
			return count, loadErrors
		}
		if toolsArray, okToolsArray := toolsValue.([]any); okToolsArray {
			for index, item := range toolsArray {
				itemMap, okItemMap := item.(map[string]any)
				if !okItemMap {
					errors = append(errors, fmt.Sprintf("%s: tools[%d] is not an object", relativePath, index))
					continue
				}
				if registerOneToolDescriptor(itemMap, relativePath, destination) {
					loaded++
				} else {
					errors = append(errors, fmt.Sprintf("%s: tools[%d] missing usable name/schema", relativePath, index))
				}
			}
			return loaded, errors
		}
	}

	if schemasValue, hasSchemas := rootMap["schemas"]; hasSchemas {
		if schemasMap, okSchemasMap := schemasValue.(map[string]any); okSchemasMap {
			return ingestNameToSchemaMap(schemasMap, relativePath, destination)
		}
	}

	if registerOneToolDescriptor(rootMap, relativePath, destination) {
		return 1, nil
	}

	if looksLikeNameToSchemaMap(rootMap) {
		return ingestNameToSchemaMap(rootMap, relativePath, destination)
	}

	return 0, []string{fmt.Sprintf("%s: unrecognized schema file shape", relativePath)}
}

func ingestNameToSchemaMap(nameToSchema map[string]any, relativePath string, destination map[string]registeredTool) (int, []string) {
	loaded := 0
	var errors []string
	for rawName, rawValue := range nameToSchema {
		toolName := strings.TrimSpace(rawName)
		if toolName == "" {
			continue
		}
		if valueMap, okValueMap := rawValue.(map[string]any); okValueMap {
			if schemaFromDescriptor, okDescriptor := extractSchemaObject(valueMap); okDescriptor {
				description := stringField(valueMap, "description")
				canonicalName := toolName
				if !looksLikeRequestToolName(canonicalName) {
					resolved := resolveToolName(valueMap, relativePath)
					if resolved != "" {
						canonicalName = resolved
					}
				}
				destination[canonicalName] = registeredTool{
					Name:        canonicalName,
					Description: description,
					Schema:      schemaFromDescriptor,
				}
				loaded++
				continue
			}
			if isSchemaShaped(valueMap) {
				destination[toolName] = registeredTool{
					Name:   toolName,
					Schema: cloneMap(valueMap),
				}
				loaded++
				continue
			}
			errors = append(errors, fmt.Sprintf("%s: entry %q is not a schema object", relativePath, toolName))
			continue
		}
		schemaObject, okSchema := asSchemaObject(rawValue)
		if !okSchema {
			errors = append(errors, fmt.Sprintf("%s: entry %q is not a schema object", relativePath, toolName))
			continue
		}
		destination[toolName] = registeredTool{
			Name:   toolName,
			Schema: schemaObject,
		}
		loaded++
	}
	return loaded, errors
}

func registerOneToolDescriptor(descriptor map[string]any, relativePath string, destination map[string]registeredTool) bool {
	if descriptor == nil {
		return false
	}

	if functionValue, hasFunction := descriptor["function"]; hasFunction {
		if functionMap, okFunctionMap := functionValue.(map[string]any); okFunctionMap {
			return registerOneToolDescriptor(functionMap, relativePath, destination)
		}
	}

	schemaObject, okSchema := extractSchemaObject(descriptor)
	if !okSchema {
		return false
	}

	toolName := resolveToolName(descriptor, relativePath)
	if toolName == "" {
		return false
	}
	destination[toolName] = registeredTool{
		Name:        toolName,
		Description: stringField(descriptor, "description"),
		Schema:      schemaObject,
	}
	return true
}

func extractSchemaObject(descriptor map[string]any) (map[string]any, bool) {
	for _, key := range []string{"arguments", "input_schema", "parameters", "inputSchema"} {
		if raw, exists := descriptor[key]; exists {
			if schemaObject, okSchema := asSchemaObject(raw); okSchema {
				return schemaObject, true
			}
		}
	}
	if _, hasName := descriptor["name"]; hasName {
		return nil, false
	}
	if _, hasDescription := descriptor["description"]; hasDescription {
		// description alone without schema field is not a bare schema unless type+properties.
	}
	if _, hasType := descriptor["type"]; hasType {
		if _, hasProperties := descriptor["properties"]; hasProperties {
			if _, hasName := descriptor["name"]; hasName {
				return nil, false
			}
			return cloneMap(descriptor), true
		}
	}
	return nil, false
}

func asSchemaObject(raw any) (map[string]any, bool) {
	switch typed := raw.(type) {
	case map[string]any:
		return cloneMap(typed), true
	case json.RawMessage:
		var decoded map[string]any
		if errUnmarshal := json.Unmarshal(typed, &decoded); errUnmarshal != nil || decoded == nil {
			return nil, false
		}
		return decoded, true
	default:
		return nil, false
	}
}

func resolveToolName(descriptor map[string]any, relativePath string) string {
	if nameValue, hasName := descriptor["name"]; hasName {
		if nameString, okNameString := nameValue.(string); okNameString {
			trimmedName := strings.TrimSpace(nameString)
			if trimmedName != "" {
				if looksLikeRequestToolName(trimmedName) {
					return trimmedName
				}
				composed := composeToolNameFromPath(relativePath, trimmedName)
				if composed != "" {
					return composed
				}
				return trimmedName
			}
		}
	}
	baseName := strings.TrimSuffix(filepath.Base(relativePath), filepath.Ext(relativePath))
	composed := composeToolNameFromPath(relativePath, baseName)
	if composed != "" {
		return composed
	}
	if looksLikeRequestToolName(baseName) {
		return baseName
	}
	return ""
}

func composeToolNameFromPath(relativePath, shortToolName string) string {
	shortToolName = strings.TrimSpace(shortToolName)
	if shortToolName == "" {
		return ""
	}
	if looksLikeRequestToolName(shortToolName) {
		return shortToolName
	}

	normalizedPath := filepath.ToSlash(relativePath)
	parts := strings.Split(normalizedPath, "/")
	if len(parts) < 2 {
		return ""
	}

	serverCandidate := ""
	for index := len(parts) - 2; index >= 0; index-- {
		part := strings.TrimSpace(parts[index])
		if part == "" || strings.EqualFold(part, "tools") {
			continue
		}
		serverCandidate = part
		break
	}
	if serverCandidate == "" {
		return ""
	}
	if strings.HasPrefix(serverCandidate, "user-") {
		return serverCandidate + "-" + shortToolName
	}
	return "user-" + serverCandidate + "-" + shortToolName
}

func looksLikeRequestToolName(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "user-") && strings.Count(trimmed, "-") >= 2 {
		return true
	}
	return strings.Contains(trimmed, "-") && !strings.Contains(trimmed, "/")
}

func looksLikeNameToSchemaMap(rootMap map[string]any) bool {
	if len(rootMap) == 0 {
		return false
	}
	if _, hasName := rootMap["name"]; hasName {
		return false
	}
	if _, hasArguments := rootMap["arguments"]; hasArguments {
		return false
	}
	if _, hasInputSchema := rootMap["input_schema"]; hasInputSchema {
		return false
	}
	if _, hasParameters := rootMap["parameters"]; hasParameters {
		return false
	}

	schemaLikeCount := 0
	for key, value := range rootMap {
		if strings.TrimSpace(key) == "" {
			return false
		}
		schemaObject, okSchema := asSchemaObject(value)
		if !okSchema {
			return false
		}
		if isSchemaShaped(schemaObject) {
			schemaLikeCount++
		}
	}
	return schemaLikeCount > 0
}

func isSchemaShaped(schemaObject map[string]any) bool {
	if schemaObject == nil {
		return false
	}
	if _, hasType := schemaObject["type"]; hasType {
		return true
	}
	if _, hasProperties := schemaObject["properties"]; hasProperties {
		return true
	}
	return false
}

func stringField(source map[string]any, key string) string {
	raw, exists := source[key]
	if !exists || raw == nil {
		return ""
	}
	if text, okText := raw.(string); okText {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(fmt.Sprint(raw))
}

func cloneRegisteredTool(entry registeredTool) registeredTool {
	return registeredTool{
		Name:        entry.Name,
		Description: entry.Description,
		Schema:      cloneMap(entry.Schema),
	}
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	raw, errMarshal := json.Marshal(source)
	if errMarshal != nil {
		out := make(map[string]any, len(source))
		for key, value := range source {
			out[key] = value
		}
		return out
	}
	var out map[string]any
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		out = make(map[string]any, len(source))
		for key, value := range source {
			out[key] = value
		}
	}
	return out
}
