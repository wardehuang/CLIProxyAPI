package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func handleExternalNodeIngest(request managementRequest) ([]byte, error) {
	if !strings.EqualFold(strings.TrimSpace(request.Method), http.MethodPost) {
		return managementJSON(http.StatusMethodNotAllowed, errorMessage("methodNotAllowed", "method not allowed"))
	}
	text, err := ingestTextFromBody(request.Body)
	if err != nil {
		return managementJSON(http.StatusBadRequest, errorMessage("invalidBody", err.Error()))
	}
	body, err := json.Marshal(map[string]any{"text": text})
	if err != nil {
		return managementJSON(http.StatusBadRequest, errorMessage("invalidBody", "invalid body"))
	}
	if err := pluginRuntime.ensure(); err != nil {
		return nil, err
	}
	return pluginRuntime.withStore(func(store *ipStore) ([]byte, error) {
		return addNodes(store, body)
	})
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
