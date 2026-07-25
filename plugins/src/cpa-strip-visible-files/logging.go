package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func logPluginDebug(hostCallbackID, message string, fields map[string]any) {
	if !pluginDebugEnabled() {
		return
	}
	logPlugin(hostCallbackID, "debug", message, fields)
}

func logPluginInfo(hostCallbackID, message string, fields map[string]any) {
	logPlugin(hostCallbackID, "info", message, fields)
}

func logPlugin(hostCallbackID, level, message string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["plugin"] = pluginName
	payload := map[string]any{
		"level":   level,
		"message": formatPluginLogMessage(message, fields),
		"fields":  fields,
	}
	if trimmedHostCallbackID := strings.TrimSpace(hostCallbackID); trimmedHostCallbackID != "" {
		payload["host_callback_id"] = trimmedHostCallbackID
	}
	_, _ = callHost(pluginabi.MethodHostLog, payload)
}

func formatPluginLogMessage(message string, fields map[string]any) string {
	raw, errMarshal := json.Marshal(fields)
	if errMarshal != nil || len(raw) == 0 {
		return message
	}
	if message == "" {
		return string(raw)
	}
	return message + " | fields=" + string(raw)
}
