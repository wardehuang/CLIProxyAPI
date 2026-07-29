package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	managementPathSchemas = "/mcp-schema-patch/schemas"
	managementPathReload  = "/mcp-schema-patch/reload"
	managementPathUpload  = "/mcp-schema-patch/upload"
)

type managementRouteSpec struct {
	Method string `json:"Method"`
	Path   string `json:"Path"`
}

type managementRegistrationPayload struct {
	Routes []managementRouteSpec `json:"routes"`
}

type uploadRequest struct {
	// FileName is the relative path under schemas-dir, e.g. user-claude-mem/tools/search.json
	FileName string `json:"file_name"`
	// Content is the raw JSON text of one MCP tool descriptor or registry map.
	Content string `json:"content"`
	// ContentBase64 is an alternative to Content for binary-safe transport.
	ContentBase64 string `json:"content_base64"`
	// ReloadAfter writes then reloads the registry. Default true.
	ReloadAfter *bool `json:"reload_after"`
}

func managementRegistration() managementRegistrationPayload {
	return managementRegistrationPayload{
		Routes: []managementRouteSpec{
			{Method: http.MethodGet, Path: managementPathSchemas},
			{Method: http.MethodPost, Path: managementPathReload},
			{Method: http.MethodPost, Path: managementPathUpload},
		},
	}
}

func handleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	path := normalizeManagementPath(req.Path)

	switch {
	case method == http.MethodGet && path == managementPathSchemas:
		return managementJSON(http.StatusOK, schemaStatusPayload())
	case method == http.MethodPost && path == managementPathReload:
		result := reloadSchemaRegistry(resolveSchemasDir())
		logPluginInfo("", "mcp schema registry reloaded via management", map[string]any{
			"tool_count": result.ToolCount,
			"file_count": result.FileCount,
			"errors":     result.Errors,
		})
		return managementJSON(http.StatusOK, map[string]any{
			"status":      "reloaded",
			"schemas_dir": resolveSchemasDir(),
			"tool_count":  result.ToolCount,
			"file_count":  result.FileCount,
			"tool_names":  result.ToolNames,
			"errors":      result.Errors,
			"reloaded_at": time.Now().UTC().Format(time.RFC3339),
		})
	case method == http.MethodPost && path == managementPathUpload:
		return handleUpload(req.Body)
	default:
		return managementJSON(http.StatusNotFound, map[string]any{
			"error":   "not_found",
			"message": fmt.Sprintf("unknown management route %s %s", method, path),
		})
	}
}

func handleUpload(body []byte) pluginapi.ManagementResponse {
	var request uploadRequest
	if errUnmarshal := json.Unmarshal(body, &request); errUnmarshal != nil {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"error":   "invalid_json",
			"message": errUnmarshal.Error(),
		})
	}

	fileName := strings.TrimSpace(request.FileName)
	if fileName == "" {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"error":   "missing_file_name",
			"message": "file_name is required (relative path under schemas-dir)",
		})
	}
	if errValidate := validateUploadFileName(fileName); errValidate != nil {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"error":   "invalid_file_name",
			"message": errValidate.Error(),
		})
	}

	contentBytes, errContent := resolveUploadContent(request)
	if errContent != nil {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"error":   "invalid_content",
			"message": errContent.Error(),
		})
	}
	if len(bytesTrimSpace(contentBytes)) == 0 {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"error":   "empty_content",
			"message": "content is empty",
		})
	}
	if !json.Valid(contentBytes) {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"error":   "invalid_json_content",
			"message": "content must be valid JSON",
		})
	}

	schemasDir := resolveSchemasDir()
	targetPath := filepath.Join(schemasDir, filepath.FromSlash(fileName))
	if errMkdir := os.MkdirAll(filepath.Dir(targetPath), 0o755); errMkdir != nil {
		return managementJSON(http.StatusInternalServerError, map[string]any{
			"error":   "mkdir_failed",
			"message": errMkdir.Error(),
		})
	}
	if errWrite := os.WriteFile(targetPath, contentBytes, 0o644); errWrite != nil {
		return managementJSON(http.StatusInternalServerError, map[string]any{
			"error":   "write_failed",
			"message": errWrite.Error(),
		})
	}

	reloadAfter := true
	if request.ReloadAfter != nil {
		reloadAfter = *request.ReloadAfter
	}

	response := map[string]any{
		"status":      "uploaded",
		"file_name":   fileName,
		"path":        targetPath,
		"bytes":       len(contentBytes),
		"schemas_dir": schemasDir,
	}
	if reloadAfter {
		result := reloadSchemaRegistry(schemasDir)
		response["reloaded"] = true
		response["tool_count"] = result.ToolCount
		response["file_count"] = result.FileCount
		response["tool_names"] = result.ToolNames
		response["errors"] = result.Errors
		logPluginInfo("", "mcp schema uploaded and reloaded", map[string]any{
			"file_name":  fileName,
			"tool_count": result.ToolCount,
		})
	} else {
		response["reloaded"] = false
		logPluginInfo("", "mcp schema uploaded", map[string]any{
			"file_name": fileName,
			"bytes":     len(contentBytes),
		})
	}
	return managementJSON(http.StatusOK, response)
}

func schemaStatusPayload() map[string]any {
	snapshot := currentSchemaRegistry()
	return map[string]any{
		"plugin":      pluginName,
		"version":     pluginVersion,
		"schemas_dir": resolveSchemasDir(),
		"only_empty":  onlyEmptyEnabled(),
		"tool_count":  len(snapshot.toolNames),
		"file_count":  snapshot.fileCount,
		"tool_names":  snapshot.toolNames,
		"load_errors": snapshot.errors,
	}
}

func validateUploadFileName(fileName string) error {
	normalized := filepath.ToSlash(fileName)
	if strings.Contains(normalized, "\\") {
		return fmt.Errorf("file_name must use forward slashes")
	}
	if strings.HasPrefix(normalized, "/") || strings.HasPrefix(normalized, "../") || strings.Contains(normalized, "/../") || normalized == ".." {
		return fmt.Errorf("file_name must be a relative path without ..")
	}
	if strings.Contains(normalized, "..") {
		return fmt.Errorf("file_name must not contain ..")
	}
	if !strings.HasSuffix(strings.ToLower(normalized), ".json") {
		return fmt.Errorf("file_name must end with .json")
	}
	return nil
}

func resolveUploadContent(request uploadRequest) ([]byte, error) {
	if strings.TrimSpace(request.Content) != "" {
		return []byte(request.Content), nil
	}
	if strings.TrimSpace(request.ContentBase64) != "" {
		decoded, errDecode := decodeBase64(request.ContentBase64)
		if errDecode != nil {
			return nil, errDecode
		}
		return decoded, nil
	}
	return nil, fmt.Errorf("content or content_base64 is required")
}

func decodeBase64(value string) ([]byte, error) {
	// std encoding via encoding/json path is heavy; use encoding/base64.
	return decodeStdBase64(strings.TrimSpace(value))
}

func normalizeManagementPath(path string) string {
	trimmed := strings.TrimSpace(path)
	trimmed = strings.TrimRight(trimmed, "/")
	const managementPrefix = "/v0/management"
	if strings.HasPrefix(trimmed, managementPrefix) {
		trimmed = strings.TrimPrefix(trimmed, managementPrefix)
	}
	if trimmed == "" {
		return "/"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	return trimmed
}

func managementJSON(statusCode int, payload any) pluginapi.ManagementResponse {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		fallback, _ := json.Marshal(map[string]any{
			"error":   "marshal_failed",
			"message": errMarshal.Error(),
		})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusInternalServerError,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       fallback,
		}
	}
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	return pluginapi.ManagementResponse{
		StatusCode: statusCode,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}
}

func bytesTrimSpace(value []byte) []byte {
	return []byte(strings.TrimSpace(string(value)))
}
