package main

import (
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var errHostAuthIndexUnavailable = errors.New("host auth index is unavailable")

type hostAuthList struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type xaiAuthFile struct {
	Entry       pluginapi.HostAuthFileEntry
	Index       string
	Identity    string
	Name        string
	Path        string
	Priority    int
	Disabled    bool
	Unavailable bool
	ProxyURL    string
	Raw         map[string]any
}

type authAssignment struct {
	SlotID   int64
	NodeID   int64
	ProxyURL string
}

func listXAIAuthEntries() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var response hostAuthList
	if err := json.Unmarshal(raw, &response); err != nil {
		var files []pluginapi.HostAuthFileEntry
		if arrayErr := json.Unmarshal(raw, &files); arrayErr != nil {
			return nil, fmt.Errorf("decode xAI auth list: %w", err)
		}
		response.Files = files
	}
	if response.Files == nil {
		return nil, fmt.Errorf("decode xAI auth list: files is missing")
	}
	entries := make([]pluginapi.HostAuthFileEntry, 0, len(response.Files))
	for _, entry := range response.Files {
		if entry.RuntimeOnly || strings.TrimSpace(entry.Path) == "" || !isXAIAuthEntry(entry) {
			continue
		}
		if strings.TrimSpace(entry.AuthIndex) == "" {
			return nil, fmt.Errorf("%w: %s", errHostAuthIndexUnavailable, authEntryName(entry))
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(left, right int) bool {
		return authEntryName(entries[left]) < authEntryName(entries[right])
	})
	return entries, nil
}

func isXAIAuthEntry(entry pluginapi.HostAuthFileEntry) bool {
	value := strings.ToLower(strings.TrimSpace(strings.Join([]string{entry.Provider, entry.Type, entry.Name, entry.Label}, " ")))
	return strings.Contains(value, "xai") || strings.Contains(value, "grok")
}

func authEntryIdentity(entry pluginapi.HostAuthFileEntry) string {
	if value := strings.TrimSpace(entry.AuthIndex); value != "" {
		return value
	}
	if value := strings.TrimSpace(entry.ID); value != "" {
		return value
	}
	return strings.TrimSpace(entry.Name)
}

func authEntryName(entry pluginapi.HostAuthFileEntry) string {
	if value := strings.TrimSpace(entry.Name); value != "" {
		return value
	}
	if value := strings.TrimSpace(entry.Label); value != "" {
		return value
	}
	return authEntryIdentity(entry)
}

func loadXAIAuthFiles(entries []pluginapi.HostAuthFileEntry) ([]xaiAuthFile, error) {
	files := make([]xaiAuthFile, 0, len(entries))
	for _, entry := range entries {
		file, err := getXAIAuthFile(entry)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

func getXAIAuthFile(entry pluginapi.HostAuthFileEntry) (xaiAuthFile, error) {
	index := strings.TrimSpace(entry.AuthIndex)
	if index == "" {
		return xaiAuthFile{}, fmt.Errorf("%w: %s", errHostAuthIndexUnavailable, authEntryName(entry))
	}
	raw, err := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: index})
	if err != nil {
		return xaiAuthFile{}, fmt.Errorf("read xAI auth %s: %w", index, err)
	}
	var response pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return xaiAuthFile{}, fmt.Errorf("decode xAI auth file %s: %w", index, err)
	}
	object := make(map[string]any)
	if len(response.JSON) > 0 {
		if err := json.Unmarshal(response.JSON, &object); err != nil {
			return xaiAuthFile{}, fmt.Errorf("decode xAI auth JSON %s: %w", index, err)
		}
	}
	name := strings.TrimSpace(response.Name)
	if name == "" {
		name = filepath.Base(strings.TrimSpace(response.Path))
	}
	if name == "" {
		name = authEntryName(entry)
	}
	path := strings.TrimSpace(response.Path)
	if path == "" {
		path = strings.TrimSpace(entry.Path)
	}
	resolvedIndex := strings.TrimSpace(response.AuthIndex)
	if resolvedIndex == "" {
		resolvedIndex = index
	}
	priority := entry.Priority
	if _, exists := object["priority"]; exists {
		priority = integerField(object, "priority")
	}
	return xaiAuthFile{
		Entry:       entry,
		Index:       resolvedIndex,
		Identity:    authEntryIdentity(entry),
		Name:        name,
		Path:        path,
		Priority:    priority,
		Disabled:    entry.Disabled || boolField(object, "disabled"),
		Unavailable: entry.Unavailable,
		ProxyURL:    strings.TrimSpace(stringField(object, "proxy_url")),
		Raw:         object,
	}, nil
}

func syncAuthBindings(store *guardianStore, entries []pluginapi.HostAuthFileEntry, inspectionRunID, inspectionAt int64) error {
	files, err := loadXAIAuthFiles(entries)
	if err != nil {
		return err
	}
	return syncAuthBindingsFromFiles(store, files, inspectionRunID, inspectionAt)
}

func syncAuthBindingsFromFiles(store *guardianStore, files []xaiAuthFile, inspectionRunID, inspectionAt int64) error {
	bindings := make([]authBinding, 0, len(files))
	now := time.Now()
	for _, file := range files {
		lastChecked := int64(0)
		checkedAt := file.Entry.UpdatedAt
		if checkedAt.IsZero() {
			checkedAt = file.Entry.LastRefresh
		}
		if !checkedAt.IsZero() {
			lastChecked = checkedAt.UnixMilli()
		}
		status := strings.ToLower(strings.TrimSpace(file.Entry.Status))
		if status == "active" || status == "enabled" || status == "ready" || status == "healthy" {
			status = "available"
		}
		if status == "" {
			status = "unknown"
		}
		if !file.Entry.NextRetryAfter.IsZero() && file.Entry.NextRetryAfter.After(now) && !file.Disabled && !file.Unavailable {
			status = "cooling"
		}
		if file.Disabled || file.Unavailable {
			status = "unavailable"
		}
		bindings = append(bindings, authBinding{
			AuthIndex:       file.Index,
			AuthName:        file.Name,
			Status:          status,
			Priority:        file.Priority,
			Success:         file.Entry.Success,
			Failed:          file.Entry.Failed,
			UpdatedAt:       time.Now().UnixMilli(),
			LastChecked:     lastChecked,
			InspectionRunID: inspectionRunID,
			AccountType:     normalizedAccountType(file.Entry.AccountType),
			ScheduleGroup:   readNestedScheduleGroup(file.Raw),
			LastInspection:  inspectionAt,
		})
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

func readNestedScheduleGroup(values map[string]any) *int {
	maps := []map[string]any{values}
	for _, key := range []string{"attributes", "metadata"} {
		if nested, ok := values[key].(map[string]any); ok {
			maps = append(maps, nested)
		}
	}
	for _, current := range maps {
		value, exists := current["schedule_group"]
		if !exists {
			continue
		}
		if parsed, ok := readScheduleGroupValue(value); ok {
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

func refreshHealthyAuthDistribution(store *guardianStore) error {
	store.authDistributionMutex.Lock()
	defer store.authDistributionMutex.Unlock()

	entries, err := listXAIAuthEntries()
	if err != nil {
		return err
	}
	files, err := loadXAIAuthFiles(entries)
	if err != nil {
		return err
	}
	if err := syncAuthBindingsFromFiles(store, files, 0, 0); err != nil {
		return fmt.Errorf("sync auth metadata: %w", err)
	}
	primarySlots, err := listPrimaryHealthySlotNodes(store)
	if err != nil {
		return err
	}
	used := make(map[string]struct{}, len(primarySlots))
	assignments := make(map[string]authAssignment, len(primarySlots))
	selections := make([]authSelection, 0, len(primarySlots))
	var firstErr error
	for _, slot := range primarySlots {
		selected, source, err := selectAuthForHealthySlot(store, slot, used, files)
		if err != nil {
			store.recordAuthDistributionFailure(slot.NodeID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		assignments[selected.Index] = authAssignment{SlotID: slot.SlotID, NodeID: slot.NodeID, ProxyURL: slot.Address}
		selections = append(selections, authSelection{AuthIndex: selected.Index, AuthIdentity: selected.Identity, NodeID: slot.NodeID, SlotID: slot.SlotID, Source: source})
	}
	for _, file := range files {
		desired := ""
		if firstErr == nil {
			if assignment, assigned := assignments[file.Index]; assigned {
				desired = assignment.ProxyURL
			}
		}
		if file.ProxyURL == desired {
			store.recordAuthProxyWriteSuccess(file.Index)
			continue
		}
		if strings.TrimSpace(file.Path) == "" || file.Raw == nil {
			store.recordAuthProxyWriteFailure(file.Index, fmt.Errorf("auth file is not writable"), 1)
			if firstErr == nil {
				firstErr = fmt.Errorf("auth %s has no writable file", file.Index)
			}
			continue
		}
		attempts, err := saveAndVerifyAuthProxyURL(file, desired)
		if err != nil {
			store.recordAuthProxyWriteFailure(file.Index, err, attempts)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		store.recordAuthProxyWriteSuccess(file.Index)
	}
	if firstErr != nil {
		if err := clearAuthDistributionFiles(store, files); err != nil {
			firstErr = fmt.Errorf("clear auth proxy URLs after failure: %w; previous error: %v", err, firstErr)
		}
		if err := store.replaceAuthDistribution(nil); err != nil {
			return fmt.Errorf("clear auth distribution after write failure: %w; previous error: %v", err, firstErr)
		}
		return firstErr
	}
	if err := store.replaceAuthDistribution(assignments); err != nil {
		clearErr := clearAuthDistributionFiles(store, files)
		distributionErr := store.replaceAuthDistribution(nil)
		if clearErr != nil || distributionErr != nil {
			return fmt.Errorf("replace auth distribution: %w; clear files: %v; clear database: %v", err, clearErr, distributionErr)
		}
		return err
	}
	for _, selection := range selections {
		if err := store.recordAuthSelection(selection); err != nil {
			return err
		}
	}
	return nil
}

func clearAuthDistributionFiles(store *guardianStore, files []xaiAuthFile) error {
	var firstErr error
	for _, file := range files {
		if strings.TrimSpace(file.ProxyURL) == "" && strings.TrimSpace(stringField(file.Raw, "proxy_url")) == "" {
			continue
		}
		attempts, err := saveAndVerifyAuthProxyURL(file, "")
		if err != nil {
			store.recordAuthProxyWriteFailure(file.Index, err, attempts)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		store.recordAuthProxyWriteSuccess(file.Index)
	}
	return firstErr
}

type healthySlotNode struct {
	SlotID  int64
	NodeID  int64
	Address string
}

func listPrimaryHealthySlotNodes(store *guardianStore) ([]healthySlotNode, error) {
	rows, err := store.database.Query(`SELECT healthy_slots.slot_id, healthy_slots.node_id, nodes.address
FROM healthy_slots INNER JOIN nodes ON nodes.id = healthy_slots.node_id
WHERE healthy_slots.slot_kind = 'primary' AND nodes.scope = 'guard' AND nodes.status = ?
ORDER BY healthy_slots.slot_id`, statusHealthy)
	if err != nil {
		return nil, fmt.Errorf("list primary healthy slots: %w", err)
	}
	defer rows.Close()
	items := make([]healthySlotNode, 0)
	for rows.Next() {
		var item healthySlotNode
		if err := rows.Scan(&item.SlotID, &item.NodeID, &item.Address); err != nil {
			return nil, fmt.Errorf("scan primary healthy slot: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate primary healthy slots: %w", err)
	}
	return items, nil
}

func selectAuthForHealthySlot(store *guardianStore, slot healthySlotNode, used map[string]struct{}, files []xaiAuthFile) (xaiAuthFile, string, error) {
	available := make([]xaiAuthFile, 0, len(files))
	for _, file := range files {
		if file.Priority > 0 && !file.Disabled && strings.TrimSpace(stringField(file.Raw, "access_token")) != "" {
			available = append(available, file)
		}
	}
	if len(available) == 0 {
		return xaiAuthFile{}, "", fmt.Errorf("没有 priority>0 且包含 access_token 的 xAI auth")
	}
	for _, file := range available {
		if file.ProxyURL == slot.Address {
			if _, exists := used[file.Identity]; !exists {
				used[file.Identity] = struct{}{}
				return file, "node_binding", nil
			}
		}
	}
	var historicalIdentity string
	err := store.database.QueryRow(`SELECT auth_identity FROM auth_selection_history
WHERE node_id = ? AND was_success = 1 ORDER BY selected_at DESC, id DESC LIMIT 1`, slot.NodeID).Scan(&historicalIdentity)
	if err != nil && err != sql.ErrNoRows {
		return xaiAuthFile{}, "", fmt.Errorf("read auth selection history: %w", err)
	}
	if err == nil {
		for _, file := range available {
			if file.Identity == historicalIdentity {
				if _, exists := used[file.Identity]; !exists {
					used[file.Identity] = struct{}{}
					return file, "node_history", nil
				}
				break
			}
		}
	}
	candidates := make([]xaiAuthFile, 0, len(available))
	for _, file := range available {
		if _, exists := used[file.Identity]; !exists {
			candidates = append(candidates, file)
		}
	}
	if len(candidates) == 0 {
		return xaiAuthFile{}, "", fmt.Errorf("当前健康槽位分配已耗尽可用 auth")
	}
	index, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(len(candidates))))
	if err != nil {
		return xaiAuthFile{}, "", fmt.Errorf("select random auth: %w", err)
	}
	selected := candidates[index.Int64()]
	used[selected.Identity] = struct{}{}
	return selected, "random", nil
}

type authSelection struct {
	AuthIndex    string
	AuthIdentity string
	NodeID       int64
	SlotID       int64
	Source       string
}

func saveAndVerifyAuthProxyURL(file xaiAuthFile, proxyURL string) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		file.Raw["proxy_url"] = proxyURL
		payload, err := json.Marshal(file.Raw)
		if err != nil {
			return attempt, fmt.Errorf("encode auth %s: %w", file.Index, err)
		}
		if _, err := callHost(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: file.Name, JSON: payload}); err != nil {
			lastErr = fmt.Errorf("save auth %s: %w", file.Index, err)
			continue
		}
		verifiedRaw, err := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: file.Index})
		if err != nil {
			lastErr = fmt.Errorf("verify auth %s: %w", file.Index, err)
			continue
		}
		var verified pluginapi.HostAuthGetResponse
		if err := json.Unmarshal(verifiedRaw, &verified); err != nil {
			lastErr = fmt.Errorf("decode verified auth %s: %w", file.Index, err)
			continue
		}
		var object map[string]any
		if err := json.Unmarshal(verified.JSON, &object); err != nil {
			lastErr = fmt.Errorf("decode verified auth JSON %s: %w", file.Index, err)
			continue
		}
		if strings.TrimSpace(stringField(object, "proxy_url")) == proxyURL {
			return attempt, nil
		}
		lastErr = fmt.Errorf("proxy_url verification mismatch for auth %s", file.Index)
	}
	return 3, lastErr
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
	flag, flagOK := value.(bool)
	return ok && flagOK && flag
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
