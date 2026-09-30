package main

import (
	"encoding/json"
	"fmt"
	"net/url"

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

func syncAuthBindings(store *guardianStore, entries []pluginapi.HostAuthFileEntry) error {
	bindings := make([]authBinding, 0, len(entries))
	for _, entry := range entries {
		name := authEntryName(entry)
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
		binding := authBinding{AuthIndex: strings.TrimSpace(entry.AuthIndex), AuthName: name, Status: status, Priority: entry.Priority, Success: entry.Success, Failed: entry.Failed, UpdatedAt: time.Now().UnixMilli(), LastChecked: lastChecked}
		if entry.Disabled || entry.Unavailable {
			binding.Status = "unavailable"
		}
		bindings = append(bindings, binding)
	}
	return store.upsertAuthBindings(bindings)
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
