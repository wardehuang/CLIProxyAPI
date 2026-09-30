package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var demoteState = struct {
	sync.Mutex
	inFlight map[string]struct{}
}{inFlight: make(map[string]struct{})}

func handleUsage(ctx context.Context, record pluginapi.UsageRecord) error {
	_ = ctx
	if !shouldDemoteXAIAuth(record) {
		logPluginDebug("usage skipped", map[string]any{
			"provider":    record.Provider,
			"failed":      record.Failed,
			"status_code": record.Failure.StatusCode,
			"auth_index":  record.AuthIndex,
			"auth_id":     record.AuthID,
			"model":       record.Model,
		})
		return nil
	}

	authIndex := strings.TrimSpace(record.AuthIndex)
	if authIndex == "" {
		logPluginInfo("xai 403 demote skipped: missing auth_index", map[string]any{
			"auth_id": record.AuthID,
			"model":   record.Model,
		})
		return nil
	}

	if !beginDemote(authIndex) {
		logPluginDebug("xai 403 demote already in flight", map[string]any{
			"auth_index": authIndex,
			"auth_id":    record.AuthID,
		})
		return nil
	}
	defer endDemote(authIndex)

	return demoteAuthPriority(authIndex, record)
}

func shouldDemoteXAIAuth(record pluginapi.UsageRecord) bool {
	if !strings.EqualFold(strings.TrimSpace(record.Provider), providerXAI) {
		return false
	}
	if !record.Failed {
		return false
	}
	return record.Failure.StatusCode == http.StatusForbidden
}

func beginDemote(authIndex string) bool {
	demoteState.Lock()
	defer demoteState.Unlock()
	if _, exists := demoteState.inFlight[authIndex]; exists {
		return false
	}
	demoteState.inFlight[authIndex] = struct{}{}
	return true
}

func endDemote(authIndex string) {
	demoteState.Lock()
	delete(demoteState.inFlight, authIndex)
	demoteState.Unlock()
}

func demoteAuthPriority(authIndex string, record pluginapi.UsageRecord) error {
	authFile, errGet := callHostAuthGet(authIndex)
	if errGet != nil {
		return fmt.Errorf("host.auth.get %s: %w", authIndex, errGet)
	}
	fileName := strings.TrimSpace(authFile.Name)
	if fileName == "" {
		return fmt.Errorf("auth file name empty for auth_index %s", authIndex)
	}
	if len(authFile.JSON) == 0 {
		return fmt.Errorf("auth file json empty for auth_index %s", authIndex)
	}

	updatedJSON, previousPriority, alreadyTarget, errPatch := setAuthPriorityJSON(authFile.JSON, targetPriorityValue)
	if errPatch != nil {
		return fmt.Errorf("patch auth priority %s: %w", fileName, errPatch)
	}
	if alreadyTarget {
		logPluginDebug("xai 403 demote skipped: priority already target", map[string]any{
			"auth_index":        authIndex,
			"auth_id":           record.AuthID,
			"auth_file":         fileName,
			"previous_priority": previousPriority,
			"priority":          targetPriorityValue,
			"model":             record.Model,
		})
		return nil
	}

	saved, errSave := callHostAuthSave(fileName, updatedJSON)
	if errSave != nil {
		return fmt.Errorf("host.auth.save %s: %w", fileName, errSave)
	}

	logPluginInfo("xai 403 demoted auth priority to -1", map[string]any{
		"auth_index":        authIndex,
		"auth_id":           record.AuthID,
		"auth_file":         saved.Name,
		"auth_path":         saved.Path,
		"previous_priority": previousPriority,
		"priority":          targetPriorityValue,
		"model":             record.Model,
		"status_code":       record.Failure.StatusCode,
	})
	return nil
}

func setAuthPriorityJSON(rawJSON json.RawMessage, priority int) (json.RawMessage, string, bool, error) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rawJSON, &payload); errUnmarshal != nil {
		return nil, "", false, fmt.Errorf("decode auth json: %w", errUnmarshal)
	}
	if payload == nil {
		payload = map[string]any{}
	}

	previousPriority := priorityValueString(payload[authPriorityField])
	if previousIsTargetPriority(payload[authPriorityField], priority) {
		return nil, previousPriority, true, nil
	}

	payload[authPriorityField] = priority
	updated, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return nil, previousPriority, false, fmt.Errorf("encode auth json: %w", errMarshal)
	}
	updated = append(updated, '\n')
	return json.RawMessage(updated), previousPriority, false, nil
}

func previousIsTargetPriority(raw any, target int) bool {
	switch value := raw.(type) {
	case float64:
		return int(value) == target
	case int:
		return value == target
	case int64:
		return int(value) == target
	case json.Number:
		parsed, errParse := value.Int64()
		return errParse == nil && int(parsed) == target
	case string:
		parsed, errParse := strconv.Atoi(strings.TrimSpace(value))
		return errParse == nil && parsed == target
	default:
		return false
	}
}

func priorityValueString(raw any) string {
	if raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		return strconv.Itoa(int(value))
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case json.Number:
		return value.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}

func callHostAuthGet(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	result, errCall := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if errCall != nil {
		return pluginapi.HostAuthGetResponse{}, errCall
	}
	var response pluginapi.HostAuthGetResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		return pluginapi.HostAuthGetResponse{}, fmt.Errorf("decode host.auth.get result: %w", errUnmarshal)
	}
	return response, nil
}

func callHostAuthSave(name string, rawJSON json.RawMessage) (pluginapi.HostAuthSaveResponse, error) {
	result, errCall := callHost(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: rawJSON,
	})
	if errCall != nil {
		return pluginapi.HostAuthSaveResponse{}, errCall
	}
	var response pluginapi.HostAuthSaveResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		return pluginapi.HostAuthSaveResponse{}, fmt.Errorf("decode host.auth.save result: %w", errUnmarshal)
	}
	return response, nil
}
