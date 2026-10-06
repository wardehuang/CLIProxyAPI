package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	scheduleGroupAttribute  = "schedule_group"
	selectedAuthMetadataKey = "selected_auth_id"
)

var errScheduleGroupCountBusy = fmt.Errorf("schedule group count cannot change while requests are busy")

type scheduleGroupState struct {
	mutex       sync.Mutex
	busyByGroup map[int]string
	groupByAuth map[string]int
}

func newScheduleGroupState() *scheduleGroupState {
	return &scheduleGroupState{
		busyByGroup: make(map[int]string),
		groupByAuth: make(map[string]int),
	}
}

func (state *scheduleGroupState) hasBusy() bool {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	return len(state.busyByGroup) > 0
}

func (state *scheduleGroupState) resetRuntime() {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	clear(state.busyByGroup)
	clear(state.groupByAuth)
}

func (state *scheduleGroupState) release(completion pluginapi.RequestCompletion) {
	authID := selectedAuthID(completion.Metadata)
	if authID == "" {
		return
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()
	groupID, ok := state.groupByAuth[authID]
	if !ok {
		return
	}
	for candidateID, candidateGroupID := range state.groupByAuth {
		if candidateGroupID == groupID {
			delete(state.groupByAuth, candidateID)
		}
	}
	delete(state.busyByGroup, groupID)
}

func selectedAuthID(metadata map[string]any) string {
	value, ok := metadata[selectedAuthMetadataKey].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func (state *scheduleGroupState) pick(store *guardianStore, settings pluginSettings, request pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	if !schedulerRequestIncludesXAI(request) {
		return pluginapi.SchedulerPickResponse{}, nil
	}
	candidatesByGroup := scheduleCandidatesByGroup(request.Candidates, settings.ScheduleGroupCount)

	state.mutex.Lock()
	defer state.mutex.Unlock()

	previousAuthID := selectedAuthID(request.Options.Metadata)
	if previousAuthID != "" {
		if groupID, ok := state.groupByAuth[previousAuthID]; ok {
			candidates := candidatesByGroup[groupID]
			if len(candidates) == 0 {
				return scheduleGroupRejection("xai_schedule_group_exhausted", fmt.Sprintf("xAI schedule group %d has no remaining auth", groupID)), nil
			}
			selectedID := strings.TrimSpace(candidates[0].ID)
			state.groupByAuth[selectedID] = groupID
			state.busyByGroup[groupID] = selectedID
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: selectedID}, nil
		}
	}

	counters, err := store.scheduleGroupCounters(settings.ScheduleGroupCount)
	if err != nil {
		return pluginapi.SchedulerPickResponse{}, err
	}
	selectedGroup := 0
	selectedCount := int64(0)
	for groupID := 1; groupID <= settings.ScheduleGroupCount; groupID++ {
		if _, busy := state.busyByGroup[groupID]; busy || len(candidatesByGroup[groupID]) == 0 {
			continue
		}
		count := counters[groupID]
		if selectedGroup == 0 || count < selectedCount || count == selectedCount && groupID < selectedGroup {
			selectedGroup = groupID
			selectedCount = count
		}
	}
	if selectedGroup == 0 {
		return scheduleGroupRejection("xai_schedule_groups_busy", "no idle xAI schedule group is available"), nil
	}
	selectedID := strings.TrimSpace(candidatesByGroup[selectedGroup][0].ID)
	if err := store.incrementScheduleGroupCounter(selectedGroup); err != nil {
		return pluginapi.SchedulerPickResponse{}, err
	}
	state.busyByGroup[selectedGroup] = selectedID
	state.groupByAuth[selectedID] = selectedGroup
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: selectedID}, nil
}

func (controller *runtimeController) schedulerPick(request pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	controller.mutex.RLock()
	defer controller.mutex.RUnlock()
	if controller.runtimeScheduleGroupCount < 1 {
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("schedule groups are not configured")
	}
	settings := pluginSettings{ScheduleGroupCount: controller.runtimeScheduleGroupCount}
	return controller.scheduleGroups.pick(controller.store, settings, request)
}

func scheduleGroupRejection(code, message string) pluginapi.SchedulerPickResponse {
	return pluginapi.SchedulerPickResponse{
		Handled:      true,
		Reject:       true,
		RejectCode:   code,
		RejectReason: message,
	}
}

func schedulerRequestIncludesXAI(request pluginapi.SchedulerPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(request.Provider), "xai") {
		return true
	}
	for _, provider := range request.Providers {
		if strings.EqualFold(strings.TrimSpace(provider), "xai") {
			return true
		}
	}
	return false
}

func scheduleCandidatesByGroup(candidates []pluginapi.SchedulerAuthCandidate, groupCount int) map[int][]pluginapi.SchedulerAuthCandidate {
	grouped := make(map[int][]pluginapi.SchedulerAuthCandidate, groupCount)
	for _, candidate := range candidates {
		if !strings.EqualFold(strings.TrimSpace(candidate.Provider), "xai") {
			continue
		}
		groupID, err := strconv.Atoi(strings.TrimSpace(candidate.Attributes[scheduleGroupAttribute]))
		if err != nil || groupID < 1 || groupID > groupCount {
			continue
		}
		grouped[groupID] = append(grouped[groupID], candidate)
	}
	for groupID := range grouped {
		sort.SliceStable(grouped[groupID], func(i, j int) bool {
			left := grouped[groupID][i]
			right := grouped[groupID][j]
			if left.Priority != right.Priority {
				return left.Priority > right.Priority
			}
			return strings.TrimSpace(left.ID) < strings.TrimSpace(right.ID)
		})
	}
	return grouped
}

func (store *guardianStore) ensureScheduleGroupStorage() error {
	_, err := store.database.Exec(`
CREATE TABLE IF NOT EXISTS schedule_group_counters (
    group_id INTEGER PRIMARY KEY,
    call_count INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);`)
	if err != nil {
		return fmt.Errorf("initialize schedule group storage: %w", err)
	}
	return nil
}

func (store *guardianStore) reconcileScheduleGroupCounters(groupCount int) error {
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin schedule group reconciliation: %w", err)
	}
	defer transaction.Rollback()
	now := time.Now().UnixMilli()
	for groupID := 1; groupID <= groupCount; groupID++ {
		if _, err := transaction.Exec(`
INSERT OR IGNORE INTO schedule_group_counters(group_id, call_count, updated_at)
VALUES (?, 0, ?)`, groupID, now); err != nil {
			return fmt.Errorf("ensure schedule group %d: %w", groupID, err)
		}
	}
	if _, err := transaction.Exec(`DELETE FROM schedule_group_counters WHERE group_id > ?`, groupCount); err != nil {
		return fmt.Errorf("remove schedule groups above %d: %w", groupCount, err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit schedule group reconciliation: %w", err)
	}
	return nil
}

func (store *guardianStore) scheduleGroupCounters(groupCount int) (map[int]int64, error) {
	rows, err := store.database.Query(`
SELECT group_id, call_count
FROM schedule_group_counters
WHERE group_id BETWEEN 1 AND ?`, groupCount)
	if err != nil {
		return nil, fmt.Errorf("read schedule group counters: %w", err)
	}
	defer rows.Close()
	counters := make(map[int]int64, groupCount)
	for rows.Next() {
		var groupID int
		var callCount int64
		if err := rows.Scan(&groupID, &callCount); err != nil {
			return nil, fmt.Errorf("scan schedule group counter: %w", err)
		}
		counters[groupID] = callCount
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schedule group counters: %w", err)
	}
	for groupID := 1; groupID <= groupCount; groupID++ {
		if _, ok := counters[groupID]; !ok {
			return nil, fmt.Errorf("schedule group %d counter is missing", groupID)
		}
	}
	return counters, nil
}

func (store *guardianStore) incrementScheduleGroupCounter(groupID int) error {
	result, err := store.database.Exec(`
UPDATE schedule_group_counters
SET call_count = call_count + 1, updated_at = ?
WHERE group_id = ?`, time.Now().UnixMilli(), groupID)
	if err != nil {
		return fmt.Errorf("increment schedule group %d counter: %w", groupID, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read schedule group %d increment result: %w", groupID, err)
	}
	if rowsAffected != 1 {
		return fmt.Errorf("schedule group %d counter is missing", groupID)
	}
	return nil
}

func (controller *runtimeController) scheduleGroupCountersAPI(store *guardianStore) (int, []byte, error) {
	settings, err := store.settings()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	controller.mutex.RLock()
	activeGroupCount := controller.runtimeScheduleGroupCount
	controller.mutex.RUnlock()
	groupCount := settings.ScheduleGroupCount
	if groupCount < 1 {
		return http.StatusInternalServerError, nil, fmt.Errorf("schedule groups are not configured")
	}
	counters, err := store.scheduleGroupCounters(groupCount)
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	items := make([]map[string]any, 0, groupCount)
	var total int64
	for groupID := 1; groupID <= groupCount; groupID++ {
		count := counters[groupID]
		total += count
		items = append(items, map[string]any{"groupId": groupID, "callCount": count})
	}
	return jsonAPIResult(map[string]any{"groupCount": groupCount, "activeGroupCount": activeGroupCount, "totalCalls": total, "items": items}, nil)
}
