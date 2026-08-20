package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func logPluginDebug(hostCallbackID, message string, fields map[string]any) {
	if !pluginDebugEnabled() {
		return
	}
	// Emit as info when debug is on so default journal levels still capture the trail.
	logPlugin(hostCallbackID, "info", "[debug] "+message, fields)
}

func logPluginInfo(hostCallbackID, message string, fields map[string]any) {
	logPlugin(hostCallbackID, "info", message, fields)
}

func logPluginWarn(hostCallbackID, message string, fields map[string]any) {
	logPlugin(hostCallbackID, "warn", message, fields)
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

func previewBytes(raw []byte, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = 240
	}
	text := strings.ToValidUTF8(string(raw), "�")
	text = strings.ReplaceAll(text, "\r", "\\r")
	text = strings.ReplaceAll(text, "\n", "\\n")
	if utf8.RuneCountInString(text) <= maxRunes {
		return text
	}
	runes := []rune(text)
	return string(runes[:maxRunes]) + fmt.Sprintf("…(+%d runes)", len(runes)-maxRunes)
}

func elapsedMS(started time.Time) int64 {
	if started.IsZero() {
		return 0
	}
	return time.Since(started).Milliseconds()
}
