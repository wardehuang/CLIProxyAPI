package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type hostAuthList struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

func listXAIAuthEntries() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var response hostAuthList
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode xAI auth list: %w", err)
	}
	if response.Files == nil {
		return nil, fmt.Errorf("decode xAI auth list: files is missing")
	}
	entries := make([]pluginapi.HostAuthFileEntry, 0, len(response.Files))
	for _, entry := range response.Files {
		if !isXAIAuthEntry(entry) {
			continue
		}
		if strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func isXAIAuthEntry(entry pluginapi.HostAuthFileEntry) bool {
	value := strings.ToLower(strings.TrimSpace(strings.Join([]string{entry.Provider, entry.Type, entry.Name, entry.Label}, " ")))
	return strings.Contains(value, "xai") || strings.Contains(value, "grok")
}

func authEntryName(entry pluginapi.HostAuthFileEntry) string {
	if strings.TrimSpace(entry.Name) != "" {
		return strings.TrimSpace(entry.Name)
	}
	if strings.TrimSpace(entry.Label) != "" {
		return strings.TrimSpace(entry.Label)
	}
	return strings.TrimSpace(entry.AuthIndex)
}

func syncAuthBindings(store *guardianStore, entries []pluginapi.HostAuthFileEntry, inspectionRunID, inspectionAt int64) error {
	bindings := make([]authBinding, 0, len(entries))
	for _, entry := range entries {
		name := authEntryName(entry)
		scheduleGroup, err := authEntryScheduleGroup(entry)
		if err != nil {
			return fmt.Errorf("read schedule group for auth index %s: %w", entry.AuthIndex, err)
		}
		lastChecked := int64(0)
		checkedAt := entry.UpdatedAt
		if checkedAt.IsZero() {
			checkedAt = entry.LastRefresh
		}
		if !checkedAt.IsZero() {
			lastChecked = checkedAt.UnixMilli()
		}
		status := strings.ToLower(strings.TrimSpace(entry.Status))
		if status == "active" || status == "enabled" || status == "ready" || status == "healthy" {
			status = "available"
		}
		if status == "" {
			status = "unknown"
		}
		if !entry.NextRetryAfter.IsZero() && entry.NextRetryAfter.After(time.Now()) && !entry.Disabled && !entry.Unavailable {
			status = "cooling"
		}
		binding := authBinding{
			AuthIndex:       strings.TrimSpace(entry.AuthIndex),
			AuthName:        name,
			Status:          status,
			Priority:        entry.Priority,
			Success:         entry.Success,
			Failed:          entry.Failed,
			UpdatedAt:       time.Now().UnixMilli(),
			LastChecked:     lastChecked,
			InspectionRunID: inspectionRunID,
			AccountType:     normalizedAccountType(entry.AccountType),
			ScheduleGroup:   scheduleGroup,
			LastInspection:  inspectionAt,
		}
		if entry.Disabled || entry.Unavailable {
			binding.Status = "unavailable"
		}
		bindings = append(bindings, binding)
	}
	return store.upsertAuthBindings(bindings)
}

func normalizedAccountType(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "FREE", "SUPER":
		return strings.ToUpper(strings.TrimSpace(value))
	default:
		return ""
	}
}

func authEntryScheduleGroup(entry pluginapi.HostAuthFileEntry) (*int, error) {
	if strings.TrimSpace(entry.Path) == "" {
		return nil, nil
	}
	raw, err := callHost(pluginabi.MethodHostAuthGet, map[string]string{"auth_index": entry.AuthIndex})
	if err != nil {
		return nil, err
	}
	var response pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode auth file response: %w", err)
	}
	if len(response.JSON) == 0 {
		return nil, nil
	}
	var metadata map[string]any
	if err := json.Unmarshal(response.JSON, &metadata); err != nil {
		return nil, fmt.Errorf("decode auth file metadata: %w", err)
	}
	return readNestedScheduleGroup(metadata), nil
}

func readNestedScheduleGroup(values map[string]any) *int {
	maps := []map[string]any{values}
	for _, key := range []string{"attributes", "metadata"} {
		nested, ok := values[key].(map[string]any)
		if ok {
			maps = append(maps, nested)
		}
	}
	for _, current := range maps {
		value, exists := current["schedule_group"]
		if !exists {
			continue
		}
		parsed, ok := readScheduleGroupValue(value)
		if ok {
			return &parsed
		}
	}
	return nil
}

func readScheduleGroupValue(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		return 0, false
	}
}

func redactProxyURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.ForceQuery = false
	return parsed.String()
}
