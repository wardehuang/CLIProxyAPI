package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIsEmptySchemaObject(t *testing.T) {
	if !isEmptySchemaObject(map[string]any{"type": "object"}) {
		t.Fatal("type object only should be empty")
	}
	if !isEmptySchemaObject(map[string]any{"type": "object", "properties": map[string]any{}}) {
		t.Fatal("empty properties should be empty")
	}
	if isEmptySchemaObject(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
		},
	}) {
		t.Fatal("non-empty properties should not be empty")
	}
}

func TestLoadCursorDescriptorAndPatchAnthropicBody(t *testing.T) {
	tempDir := t.TempDir()
	toolDir := filepath.Join(tempDir, "user-claude-mem", "tools")
	if errMkdir := os.MkdirAll(toolDir, 0o755); errMkdir != nil {
		t.Fatalf("mkdir: %v", errMkdir)
	}
	descriptor := map[string]any{
		"name":        "search",
		"description": "Step 1: Search memory",
		"arguments": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query"},
				"limit": map[string]any{"type": "number", "description": "Max results"},
			},
			"additionalProperties": true,
		},
	}
	raw, errMarshal := json.Marshal(descriptor)
	if errMarshal != nil {
		t.Fatalf("marshal descriptor: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(toolDir, "search.json"), raw, 0o644); errWrite != nil {
		t.Fatalf("write descriptor: %v", errWrite)
	}

	loadResult := reloadSchemaRegistry(tempDir)
	if loadResult.ToolCount != 1 {
		t.Fatalf("ToolCount=%d errors=%v", loadResult.ToolCount, loadResult.Errors)
	}
	if loadResult.ToolNames[0] != "user-claude-mem-search" {
		t.Fatalf("tool name = %q", loadResult.ToolNames[0])
	}

	bodyMap := map[string]any{
		"model": "grok-4.5",
		"tools": []any{
			map[string]any{
				"name":        "Read",
				"description": "native",
				"input_schema": map[string]any{
					"type": "object",
					"required": []any{"path"},
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
				},
			},
			map[string]any{
				"name":         "user-claude-mem-search",
				"description":  "mem search",
				"input_schema": map[string]any{"type": "object"},
			},
			map[string]any{
				"name":         "user-unknown-tool",
				"description":  "unknown empty",
				"input_schema": map[string]any{"type": "object"},
			},
		},
	}
	body, errBody := json.Marshal(bodyMap)
	if errBody != nil {
		t.Fatalf("marshal body: %v", errBody)
	}

	result, errPatch := patchMcpToolSchemasInRequestBody(body, true, false)
	if errPatch != nil {
		t.Fatalf("patch error: %v", errPatch)
	}
	if !result.Changed {
		t.Fatal("expected change")
	}
	if len(result.PatchedTools) != 1 || result.PatchedTools[0] != "user-claude-mem-search" {
		t.Fatalf("PatchedTools=%v", result.PatchedTools)
	}

	var after map[string]any
	if errUnmarshal := json.Unmarshal(result.AfterBody, &after); errUnmarshal != nil {
		t.Fatalf("unmarshal after: %v", errUnmarshal)
	}
	tools := after["tools"].([]any)
	readTool := tools[0].(map[string]any)
	readSchema := readTool["input_schema"].(map[string]any)
	readProperties := readSchema["properties"].(map[string]any)
	if _, hasPath := readProperties["path"]; !hasPath {
		t.Fatal("Read schema must stay intact")
	}

	searchTool := tools[1].(map[string]any)
	searchSchema := searchTool["input_schema"].(map[string]any)
	searchProperties := searchSchema["properties"].(map[string]any)
	if _, hasQuery := searchProperties["query"]; !hasQuery {
		t.Fatalf("search schema missing query: %#v", searchSchema)
	}
	if _, hasLimit := searchProperties["limit"]; !hasLimit {
		t.Fatalf("search schema missing limit: %#v", searchSchema)
	}

	unknownTool := tools[2].(map[string]any)
	unknownSchema := unknownTool["input_schema"].(map[string]any)
	if _, hasProperties := unknownSchema["properties"]; hasProperties {
		if propertiesMap, ok := unknownSchema["properties"].(map[string]any); ok && len(propertiesMap) > 0 {
			t.Fatal("unknown empty tool must not be invented")
		}
	}
}

func TestPatchOpenAIFunctionParameters(t *testing.T) {
	tempDir := t.TempDir()
	registry := map[string]any{
		"user-claude-mem-search": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
		},
	}
	raw, _ := json.Marshal(registry)
	if errWrite := os.WriteFile(filepath.Join(tempDir, "registry.json"), raw, 0o644); errWrite != nil {
		t.Fatalf("write registry: %v", errWrite)
	}
	if result := reloadSchemaRegistry(tempDir); result.ToolCount != 1 {
		t.Fatalf("load failed: %#v", result)
	}

	bodyMap := map[string]any{
		"model": "grok-4.5",
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "user-claude-mem-search",
					"description": "mem",
					"parameters":   map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
		},
	}
	body, _ := json.Marshal(bodyMap)
	result, errPatch := patchMcpToolSchemasInRequestBody(body, true, false)
	if errPatch != nil {
		t.Fatalf("patch: %v", errPatch)
	}
	if !result.Changed {
		t.Fatal("expected change")
	}
	var after map[string]any
	_ = json.Unmarshal(result.AfterBody, &after)
	tools := after["tools"].([]any)
	functionWrapper := tools[0].(map[string]any)
	functionMap := functionWrapper["function"].(map[string]any)
	parameters := functionMap["parameters"].(map[string]any)
	properties := parameters["properties"].(map[string]any)
	if _, hasQuery := properties["query"]; !hasQuery {
		t.Fatalf("parameters not patched: %#v", parameters)
	}
}

func TestComposeToolNameFromPath(t *testing.T) {
	got := composeToolNameFromPath("user-claude-mem/tools/search.json", "search")
	if got != "user-claude-mem-search" {
		t.Fatalf("got %q", got)
	}
	got = composeToolNameFromPath("user-context-mode/tools/ctx_execute.json", "ctx_execute")
	if got != "user-context-mode-ctx_execute" {
		t.Fatalf("got %q", got)
	}
}

func TestInjectMissingMcpToolsAnthropic(t *testing.T) {
	tempDir := t.TempDir()
	toolDir := filepath.Join(tempDir, "user-claude-mem", "tools")
	if errMkdir := os.MkdirAll(toolDir, 0o755); errMkdir != nil {
		t.Fatalf("mkdir: %v", errMkdir)
	}
	descriptor := map[string]any{
		"name":        "search",
		"description": "Step 1: Search memory",
		"arguments": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "number"},
			},
		},
	}
	raw, _ := json.Marshal(descriptor)
	if errWrite := os.WriteFile(filepath.Join(toolDir, "search.json"), raw, 0o644); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	if result := reloadSchemaRegistry(tempDir); result.ToolCount != 1 {
		t.Fatalf("load: %#v", result)
	}

	bodyMap := map[string]any{
		"model": "grok-4.5",
		"tools": []any{
			map[string]any{
				"name": "Read",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(bodyMap)
	result, errPatch := patchMcpToolSchemasInRequestBody(body, true, true)
	if errPatch != nil {
		t.Fatalf("patch: %v", errPatch)
	}
	if !result.Changed {
		t.Fatal("expected inject change")
	}
	if len(result.InjectedTools) != 1 || result.InjectedTools[0] != "user-claude-mem-search" {
		t.Fatalf("InjectedTools=%v", result.InjectedTools)
	}

	var after map[string]any
	_ = json.Unmarshal(result.AfterBody, &after)
	tools := after["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools len=%d", len(tools))
	}
	injected := tools[1].(map[string]any)
	if injected["name"] != "user-claude-mem-search" {
		t.Fatalf("name=%v", injected["name"])
	}
	if injected["description"] != "Step 1: Search memory" {
		t.Fatalf("description=%v", injected["description"])
	}
	schema := injected["input_schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	if _, hasQuery := properties["query"]; !hasQuery {
		t.Fatalf("missing query: %#v", schema)
	}
}

func TestInjectMissingOpenAIFlat(t *testing.T) {
	tempDir := t.TempDir()
	registry := map[string]any{
		"user-claude-mem-search": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
		},
	}
	raw, _ := json.Marshal(registry)
	_ = os.WriteFile(filepath.Join(tempDir, "registry.json"), raw, 0o644)
	_ = reloadSchemaRegistry(tempDir)

	bodyMap := map[string]any{
		"model": "grok-4.5",
		"tools": []any{
			map[string]any{
				"type": "function",
				"name": "Shell",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(bodyMap)
	result, errPatch := patchMcpToolSchemasInRequestBody(body, true, true)
	if errPatch != nil {
		t.Fatalf("patch: %v", errPatch)
	}
	var after map[string]any
	_ = json.Unmarshal(result.AfterBody, &after)
	tools := after["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools len=%d want 2", len(tools))
	}
	injected := tools[1].(map[string]any)
	if injected["type"] != "function" || injected["name"] != "user-claude-mem-search" {
		t.Fatalf("injected=%#v", injected)
	}
	parameters := injected["parameters"].(map[string]any)
	properties := parameters["properties"].(map[string]any)
	if _, hasQuery := properties["query"]; !hasQuery {
		t.Fatalf("parameters=%#v", parameters)
	}
}
