package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

type authFile struct {
	Index    string
	Name     string
	Path     string
	Email    string
	Priority int
	Disabled bool
	ProxyURL string
	Raw      map[string]any
}

func listXAIAuthEntries() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var response hostAuthListResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		var files []pluginapi.HostAuthFileEntry
		if arrayErr := json.Unmarshal(raw, &files); arrayErr != nil {
			return nil, fmt.Errorf("decode xAI auth list: %w", err)
		}
		response.Files = files
	}
	entries := make([]pluginapi.HostAuthFileEntry, 0, len(response.Files))
	for _, entry := range response.Files {
		if isXAIAuthEntry(entry) && authEntryIdentity(entry) != "" {
			entries = append(entries, entry)
		}
	}
	sort.SliceStable(entries, func(leftIndex, rightIndex int) bool {
		return strings.ToLower(authEntryName(entries[leftIndex])) < strings.ToLower(authEntryName(entries[rightIndex]))
	})
	return entries, nil
}

func authEntryIdentity(entry pluginapi.HostAuthFileEntry) string {
	if identity := strings.TrimSpace(entry.AuthIndex); identity != "" {
		return identity
	}
	if identity := strings.TrimSpace(entry.ID); identity != "" {
		return identity
	}
	return strings.TrimSpace(entry.Name)
}

func authEntryName(entry pluginapi.HostAuthFileEntry) string {
	if name := strings.TrimSpace(entry.Name); name != "" {
		return name
	}
	return authEntryIdentity(entry)
}

func getAuthFileForEntry(entry pluginapi.HostAuthFileEntry) (authFile, error) {
	identity := authEntryIdentity(entry)
	file, err := getAuthFile(identity)
	if err != nil && strings.TrimSpace(entry.Name) != "" && strings.TrimSpace(entry.Name) != identity {
		return getAuthFile(entry.Name)
	}
	return file, err
}

func listAuthFiles() ([]authFile, error) {
	entries, err := listXAIAuthEntries()
	if err != nil {
		return nil, err
	}
	authFiles := make([]authFile, 0, len(entries))
	for _, entry := range entries {
		file, getErr := getAuthFileForEntry(entry)
		if getErr != nil {
			continue
		}
		authFiles = append(authFiles, file)
	}
	return authFiles, nil
}

func isXAIAuthEntry(entry pluginapi.HostAuthFileEntry) bool {
	provider := strings.ToLower(strings.TrimSpace(entry.Provider + " " + entry.Type + " " + entry.Name))
	return strings.Contains(provider, "xai") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry.Name)), "xai-")
}

func getAuthFile(authIndex string) (authFile, error) {
	raw, err := callHost(pluginabi.MethodHostAuthGet, map[string]any{"auth_index": authIndex})
	if err != nil {
		raw, err = callHost(pluginabi.MethodHostAuthGet, map[string]any{"name": authIndex})
		if err != nil {
			return authFile{}, err
		}
	}
	var response hostAuthGetResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return authFile{}, fmt.Errorf("decode xAI auth file %s: %w", authIndex, err)
	}
	object := make(map[string]any)
	if len(response.JSON) > 0 {
		if err := json.Unmarshal(response.JSON, &object); err != nil {
			return authFile{}, fmt.Errorf("decode xAI auth JSON %s: %w", authIndex, err)
		}
	}
	name := strings.TrimSpace(response.Name)
	if name == "" {
		name = filepath.Base(response.Path)
	}
	if name == "" {
		name = strings.TrimSpace(authIndex)
	}
	index := strings.TrimSpace(response.AuthIndex)
	if index == "" {
		index = strings.TrimSpace(authIndex)
	}
	return authFile{
		Index:    index,
		Name:     name,
		Path:     response.Path,
		Email:    stringField(object, "email"),
		Priority: integerField(object, "priority"),
		Disabled: boolField(object, "disabled"),
		ProxyURL: strings.TrimSpace(stringField(object, "proxy_url")),
		Raw:      object,
	}, nil
}

func saveAuthFile(auth authFile) error {
	if strings.TrimSpace(auth.Name) == "" {
		return fmt.Errorf("xAI auth name is required")
	}
	raw, err := json.Marshal(auth.Raw)
	if err != nil {
		return fmt.Errorf("encode xAI auth %s: %w", auth.Name, err)
	}
	_, err = callHost(pluginabi.MethodHostAuthSave, map[string]any{
		"name": auth.Name,
		"json": json.RawMessage(raw),
	})
	if err != nil {
		return fmt.Errorf("save xAI auth %s: %w", auth.Name, err)
	}
	return nil
}

func setAuthProxyURL(auth authFile, proxyURL string) error {
	if proxyURL == "" {
		delete(auth.Raw, "proxy_url")
	} else {
		auth.Raw["proxy_url"] = proxyURL
	}
	return saveAuthFile(auth)
}

func (auth authFile) Identity() string {
	if auth.Index != "" {
		return auth.Name + "#" + auth.Index
	}
	return auth.Name
}

func (auth authFile) AccessToken() string {
	return strings.TrimSpace(stringField(auth.Raw, "access_token"))
}

func (auth authFile) HasPositivePriority() bool {
	return auth.Priority > 0 && !auth.Disabled && auth.AccessToken() != ""
}

func stringField(object map[string]any, key string) string {
	value, ok := object[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

func integerField(object map[string]any, key string) int {
	value, ok := object[key]
	if !ok {
		return 0
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := strconv.Atoi(typed.String())
		if err == nil {
			return parsed
		}
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err == nil {
			return parsed
		}
	}
	return 0
}

func boolField(object map[string]any, key string) bool {
	value, ok := object[key]
	if !ok {
		return false
	}
	flag, ok := value.(bool)
	return ok && flag
}

func selectAuthForQuality(store *ipStore, node proxyNode, slotID, roundID int64, used map[string]struct{}) (authFile, string, error) {
	authFiles, err := listAuthFiles()
	if err != nil {
		return authFile{}, "", err
	}
	available := make([]authFile, 0, len(authFiles))
	for _, auth := range authFiles {
		if auth.HasPositivePriority() {
			available = append(available, auth)
		}
	}
	if len(available) == 0 {
		return authFile{}, "", fmt.Errorf("没有 priority>0 且包含 access_token 的 xAI auth")
	}
	for _, auth := range available {
		if auth.ProxyURL == node.ProxyURL {
			if _, exists := used[auth.Identity()]; !exists {
				used[auth.Identity()] = struct{}{}
				return auth, "node_binding", nil
			}
		}
	}
	var historicalIdentity string
	if err := store.database.QueryRow(`
SELECT auth_identity
FROM auth_selection_history
WHERE node_id = ? AND was_success = 1
ORDER BY selected_at DESC, id DESC LIMIT 1`, node.ID).Scan(&historicalIdentity); err == nil {
		for _, auth := range available {
			if auth.Identity() != historicalIdentity {
				continue
			}
			if _, exists := used[auth.Identity()]; !exists {
				used[auth.Identity()] = struct{}{}
				return auth, "node_history", nil
			}
			break
		}
	}
	candidates := make([]authFile, 0, len(available))
	for _, auth := range available {
		if _, exists := used[auth.Identity()]; !exists {
			candidates = append(candidates, auth)
		}
	}
	if len(candidates) == 0 {
		return authFile{}, "", fmt.Errorf("当前智商探测轮次已耗尽可用 auth")
	}
	selected, err := store.recordRandomAuthSelection(roundID, node.ID, slotID, candidates)
	if err != nil {
		return authFile{}, "", err
	}
	used[selected.Identity()] = struct{}{}
	return selected, "random", nil
}
