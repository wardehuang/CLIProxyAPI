//go:build cgo

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func handleExternalNodeIngest(request managementRequest) ([]byte, error) {
	if !strings.EqualFold(strings.TrimSpace(request.Method), http.MethodPost) {
		return managementJSON(http.StatusMethodNotAllowed, externalAPIError("methodNotAllowed", "method not allowed"))
	}
	text, err := ingestTextFromBody(request.Body)
	if err != nil {
		return managementJSON(http.StatusBadRequest, externalAPIError("invalidBody", err.Error()))
	}
	return guardianRuntime.ingestExternalNodes(text)
}

func (controller *runtimeController) ingestExternalNodes(text string) ([]byte, error) {
	if err := controller.ensure(); err != nil {
		return nil, err
	}
	controller.mutex.RLock()
	store := controller.store
	controller.mutex.RUnlock()

	nodes, inputErrors := parseProxyLines(text)
	result := map[string]any{
		"batchId":    "",
		"added":      0,
		"duplicates": 0,
		"errors":     inputErrors,
	}
	if len(nodes) == 0 {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"error": map[string]string{"code": "invalidInput", "message": "没有可录入的有效 IP"},
			"data":  result,
		})
	}
	batchID, added, duplicates, err := store.insertIPBatch(nodes, len(inputErrors))
	if err != nil {
		_ = store.appendLog(logLevelError, "ip_batch.insert_failed", "external node ingest failed", sanitizeLogText(err.Error()))
		return managementJSON(http.StatusInternalServerError, externalAPIError("insertFailed", err.Error()))
	}
	result["batchId"] = batchID
	result["added"] = added
	result["duplicates"] = duplicates
	_ = store.appendLog(logLevelInfo, "ip_batch.created", "降智守护 IP 批次已创建", fmt.Sprintf("批次 %s，新增 %d，重复 %d，格式错误 %d", batchID, added, duplicates, len(inputErrors)))
	if added > 0 {
		controller.queueInitialProbeAfterBatch()
	}
	return managementJSON(http.StatusOK, map[string]any{"data": result})
}

func ingestTextFromBody(body []byte) (string, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", fmt.Errorf("至少录入一行 IP")
	}
	if trimmed[0] != '{' && trimmed[0] != '[' && trimmed[0] != '"' {
		return trimmed, nil
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return "", fmt.Errorf("invalid body")
	}
	text := strings.TrimSpace(strings.Join(ingestLinesFromValue(value), "\n"))
	if text == "" {
		return "", fmt.Errorf("至少录入一行 IP")
	}
	return text, nil
}

func ingestLinesFromValue(value any) []string {
	switch typed := value.(type) {
	case string:
		if text := strings.TrimSpace(typed); text != "" {
			return []string{text}
		}
	case []any:
		lines := make([]string, 0, len(typed))
		for _, item := range typed {
			lines = append(lines, ingestLinesFromValue(item)...)
		}
		return lines
	case map[string]any:
		for _, key := range []string{"text", "ips", "ip", "proxy", "proxyUrl", "proxy_url", "url", "line", "nodes", "proxies", "lines", "items"} {
			item, ok := typed[key]
			if !ok {
				continue
			}
			if lines := ingestLinesFromValue(item); len(lines) > 0 {
				return lines
			}
		}
	}
	return nil
}

func externalAPIError(code, message string) map[string]any {
	return map[string]any{"error": map[string]string{"code": code, "message": message}}
}
