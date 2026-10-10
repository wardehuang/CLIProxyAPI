package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
	return strings.EqualFold(strings.TrimSpace(entry.Provider), "xai")
}

func findXAIAuthEntry(authIndex string) (pluginapi.HostAuthFileEntry, error) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return pluginapi.HostAuthFileEntry{}, fmt.Errorf("auth_index is required")
	}
	entries, err := listXAIAuthEntries()
	if err != nil {
		return pluginapi.HostAuthFileEntry{}, err
	}
	for _, entry := range entries {
		if entry.AuthIndex == authIndex {
			return entry, nil
		}
	}
	return pluginapi.HostAuthFileEntry{}, fmt.Errorf("persistent xAI auth %s was not found", authIndex)
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
		Name:        name,
		Path:        path,
		Priority:    priority,
		Disabled:    entry.Disabled || boolField(object, "disabled"),
		Unavailable: entry.Unavailable,
		ProxyURL:    strings.TrimSpace(stringField(object, "proxy_url")),
		Raw:         object,
	}, nil
}

func getXAIAuthPriority(authIndex string, fallback int) (int, error) {
	entry := pluginapi.HostAuthFileEntry{AuthIndex: strings.TrimSpace(authIndex), Priority: fallback}
	file, err := getXAIAuthFile(entry)
	if err != nil {
		return 0, err
	}
	return file.Priority, nil
}

func updateXAIAuthPriority(authIndex string, priority int) error {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return fmt.Errorf("auth_index is required")
	}
	raw, err := callHost(pluginabi.MethodHostAuthPriorityUpdate, pluginapi.HostAuthPriorityUpdateRequest{
		AuthIndex: authIndex,
		Priority:  priority,
	})
	if err != nil {
		return err
	}
	var response pluginapi.HostAuthPriorityUpdateResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode auth priority update response: %w", err)
	}
	if response.AuthIndex != authIndex || response.Priority != priority {
		return fmt.Errorf("auth priority update read-back mismatch")
	}
	return nil
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
		bindings = append(bindings, authBindingFromFile(file, now, inspectionRunID, inspectionAt))
	}
	return store.upsertAuthBindings(bindings)
}

func authBindingFromFile(file xaiAuthFile, now time.Time, inspectionRunID, inspectionAt int64) authBinding {
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
	return authBinding{
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
	}
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

	settings, err := store.settings()
	if err != nil {
		return fmt.Errorf("read auth distribution settings: %w", err)
	}
	if settings.HealthySlotCount < 1 {
		return fmt.Errorf("healthy slot count must be positive")
	}
	entries, err := listXAIAuthEntries()
	if err != nil {
		return err
	}
	files, err := loadXAIAuthFiles(entries)
	if err != nil {
		return err
	}
	primarySlots, err := listPrimaryHealthySlotNodes(store)
	if err != nil {
		return err
	}
	slotsByID := make(map[int64]healthySlotNode, len(primarySlots))
	for _, slot := range primarySlots {
		slotsByID[slot.SlotID] = slot
	}
	assignments := make(map[string]authAssignment, len(files))
	for fileIndex, file := range files {
		slotID := int64(fileIndex%settings.HealthySlotCount + 1)
		slot, found := slotsByID[slotID]
		if !found {
			slot = healthySlotNode{SlotID: slotID}
		} else if slot.NodeID != 0 && slot.Status != statusHealthy {
			return fmt.Errorf("healthy auth distribution slot %d node %d is not healthy: %s", slotID, slot.NodeID, slot.Status)
		}
		assignments[file.Index] = authAssignment{SlotID: slotID, NodeID: slot.NodeID, ProxyURL: slot.Address}
	}
	if err := syncAuthBindingsFromFiles(store, files, 0, 0); err != nil {
		return fmt.Errorf("sync auth metadata: %w", err)
	}

	applied := make([]authDistributionUpdate, 0, len(files))
	for _, file := range files {
		assignment := assignments[file.Index]
		if file.ProxyURL == assignment.ProxyURL {
			store.recordAuthProxyWriteSuccess(file.Index)
			continue
		}
		attempts, err := saveAndVerifyAuthProxyURL(file, assignment.ProxyURL)
		if err != nil {
			store.recordAuthProxyWriteFailure(file.Index, err, attempts)
			applied = append(applied, authDistributionUpdate{File: file, OriginalProxyURL: file.ProxyURL})
			if rollbackErr := rollbackAuthDistributionFiles(applied); rollbackErr != nil {
				return fmt.Errorf("save auth distribution %s: %w; rollback failed: %v", file.Index, err, rollbackErr)
			}
			return fmt.Errorf("save auth distribution %s: %w", file.Index, err)
		}
		applied = append(applied, authDistributionUpdate{File: file, OriginalProxyURL: file.ProxyURL})
		store.recordAuthProxyWriteSuccess(file.Index)
	}
	if err := store.replaceAuthDistribution(assignments); err != nil {
		if rollbackErr := rollbackAuthDistributionFiles(applied); rollbackErr != nil {
			return fmt.Errorf("replace auth distribution: %w; rollback failed: %v", err, rollbackErr)
		}
		return fmt.Errorf("replace auth distribution: %w", err)
	}
	return nil
}

// refreshHealthyAuthSlotDistribution re-points the auth files of one healthy slot after the
// realtime guard replaced that slot's node. refreshHealthyAuthDistribution reads every auth
// entry back through the host (one host call per account, thousands of accounts), so it must
// not run on the response path: it stalled degraded responses for minutes.
func refreshHealthyAuthSlotDistribution(store *guardianStore, slotID int64) error {
	store.authDistributionMutex.Lock()
	defer store.authDistributionMutex.Unlock()

	if slotID <= 0 {
		return fmt.Errorf("healthy slot id must be positive")
	}
	settings, err := store.settings()
	if err != nil {
		return fmt.Errorf("read auth distribution settings: %w", err)
	}
	if settings.HealthySlotCount < 1 {
		return fmt.Errorf("healthy slot count must be positive")
	}
	slot, found, err := store.primaryHealthySlotNode(slotID)
	if err != nil {
		return err
	}
	if !found {
		slot = healthySlotNode{SlotID: slotID}
	} else if slot.NodeID != 0 && slot.Status != statusHealthy {
		return fmt.Errorf("healthy auth distribution slot %d node %d is not healthy: %s", slotID, slot.NodeID, slot.Status)
	}
	assignment := authAssignment{SlotID: slot.SlotID, NodeID: slot.NodeID, ProxyURL: slot.Address}

	entries, err := listXAIAuthEntries()
	if err != nil {
		return err
	}
	now := time.Now()
	applied := make([]authDistributionUpdate, 0, 4)
	bindings := make([]authBinding, 0, 4)
	for position, entry := range entries {
		if int64(position%settings.HealthySlotCount+1) != slotID {
			continue
		}
		file, err := getXAIAuthFile(entry)
		if err != nil {
			if rollbackErr := rollbackAuthDistributionFiles(applied); rollbackErr != nil {
				return fmt.Errorf("load auth %s: %w; rollback failed: %v", entry.AuthIndex, err, rollbackErr)
			}
			return fmt.Errorf("load auth %s: %w", entry.AuthIndex, err)
		}
		if file.ProxyURL != assignment.ProxyURL {
			attempts, errSave := saveAndVerifyAuthProxyURL(file, assignment.ProxyURL)
			if errSave != nil {
				store.recordAuthProxyWriteFailure(file.Index, errSave, attempts)
				if rollbackErr := rollbackAuthDistributionFiles(applied); rollbackErr != nil {
					return fmt.Errorf("save auth distribution %s: %w; rollback failed: %v", file.Index, errSave, rollbackErr)
				}
				return fmt.Errorf("save auth distribution %s: %w", file.Index, errSave)
			}
			applied = append(applied, authDistributionUpdate{File: file, OriginalProxyURL: file.ProxyURL})
		}
		store.recordAuthProxyWriteSuccess(file.Index)
		binding := authBindingFromFile(file, now, 0, 0)
		binding.SlotID = assignment.SlotID
		binding.NodeID = assignment.NodeID
		binding.ProxyURL = assignment.ProxyURL
		bindings = append(bindings, binding)
	}
	if err := store.upsertAuthBindingsSubset(bindings); err != nil {
		if rollbackErr := rollbackAuthDistributionFiles(applied); rollbackErr != nil {
			return fmt.Errorf("sync auth metadata: %w; rollback failed: %v", err, rollbackErr)
		}
		return fmt.Errorf("sync auth metadata: %w", err)
	}
	return nil
}

type authDistributionUpdate struct {
	File             xaiAuthFile
	OriginalProxyURL string
}

func rollbackAuthDistributionFiles(updates []authDistributionUpdate) error {
	for index := len(updates) - 1; index >= 0; index-- {
		update := updates[index]
		if _, err := saveAndVerifyAuthProxyURL(update.File, update.OriginalProxyURL); err != nil {
			return fmt.Errorf("restore auth %s: %w", update.File.Index, err)
		}
	}
	return nil
}

type healthySlotNode struct {
	SlotID  int64
	NodeID  int64
	Address string
	Status  string
}

func listPrimaryHealthySlotNodes(store *guardianStore) ([]healthySlotNode, error) {
	rows, err := store.database.Query(`SELECT healthy_slots.slot_id, healthy_slots.node_id, COALESCE(nodes.address, ''), COALESCE(nodes.status, '')
FROM healthy_slots LEFT JOIN nodes ON nodes.id = healthy_slots.node_id AND nodes.scope = 'guard'
WHERE healthy_slots.slot_kind = 'primary'
ORDER BY healthy_slots.slot_id`)
	if err != nil {
		return nil, fmt.Errorf("list primary healthy slots: %w", err)
	}
	defer rows.Close()
	items := make([]healthySlotNode, 0)
	for rows.Next() {
		var item healthySlotNode
		if err := rows.Scan(&item.SlotID, &item.NodeID, &item.Address, &item.Status); err != nil {
			return nil, fmt.Errorf("scan primary healthy slot: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate primary healthy slots: %w", err)
	}
	return items, nil
}

func (store *guardianStore) primaryHealthySlotNode(slotID int64) (healthySlotNode, bool, error) {
	rows, err := store.database.Query(`SELECT healthy_slots.slot_id, healthy_slots.node_id, COALESCE(nodes.address, ''), COALESCE(nodes.status, '')
FROM healthy_slots LEFT JOIN nodes ON nodes.id = healthy_slots.node_id AND nodes.scope = 'guard'
WHERE healthy_slots.slot_kind = 'primary' AND healthy_slots.slot_id = ?`, slotID)
	if err != nil {
		return healthySlotNode{}, false, fmt.Errorf("read primary healthy slot %d: %w", slotID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return healthySlotNode{}, false, fmt.Errorf("read primary healthy slot %d: %w", slotID, err)
		}
		return healthySlotNode{SlotID: slotID}, false, nil
	}
	var item healthySlotNode
	if err := rows.Scan(&item.SlotID, &item.NodeID, &item.Address, &item.Status); err != nil {
		return healthySlotNode{}, false, fmt.Errorf("scan primary healthy slot %d: %w", slotID, err)
	}
	return item, true, nil
}

func saveAndVerifyAuthProxyURL(file xaiAuthFile, proxyURL string) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := updateAndVerifyAuthMetadataString(file.Index, "proxy_url", proxyURL); err != nil {
			lastErr = err
			continue
		}
		return attempt, nil
	}
	return 3, lastErr
}

func updateAndVerifyAuthMetadataString(authIndex, field, value string) error {
	raw, err := callHost(pluginabi.MethodHostAuthMetadataStringUpdate, pluginapi.HostAuthMetadataStringUpdateRequest{
		AuthIndex: authIndex,
		Field:     field,
		Value:     value,
	})
	if err != nil {
		return err
	}
	var response pluginapi.HostAuthMetadataStringUpdateResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode auth metadata update response: %w", err)
	}
	if response.AuthIndex != authIndex || response.Field != field || response.Value != value {
		return fmt.Errorf("auth metadata update read-back mismatch for %s", field)
	}
	return nil
}

func parseRefreshProxyURLLines(text string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	proxyURLs := make([]string, 0, len(lines))
	for index, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		node, err := parseProxyAddress(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", index+1, err)
		}
		proxyURLs = append(proxyURLs, node.Address)
	}
	if len(proxyURLs) == 0 {
		return nil, fmt.Errorf("at least one proxy URL is required")
	}
	return proxyURLs, nil
}

func saveAndVerifyAuthRefreshProxyURL(entry pluginapi.HostAuthFileEntry, proxyURL string) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		file, err := getXAIAuthFile(entry)
		if err != nil {
			lastErr = fmt.Errorf("read auth %s: %w", authEntryName(entry), err)
			continue
		}
		if err := updateAndVerifyAuthMetadataString(file.Index, "refresh_proxy_url", proxyURL); err != nil {
			lastErr = fmt.Errorf("update auth %s: %w", file.Index, err)
			continue
		}
		return nil
	}
	return lastErr
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
