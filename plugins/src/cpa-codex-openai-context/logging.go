package main

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func logPluginDebug(hostCallbackID, message string, fields map[string]any) {
	if !pluginDebugEnabled() {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["plugin"] = pluginName
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
		"host_callback_id": hostCallbackID,
		"level":            "debug",
		"message":          formatPluginLogMessage(message, fields),
		"fields":           fields,
	})
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
