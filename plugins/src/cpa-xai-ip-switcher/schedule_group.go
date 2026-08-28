package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	scheduleGroupAttribute = "schedule_group"
	scheduleTurnMetadataKey = "cpa_xai_schedule_turn_id"
)

var errScheduleGroupCountBusy = errors.New("schedule group count cannot change while requests are busy")

type scheduleGroupState struct {
	mutex       sync.Mutex
	busyByGroup map[int]string
	groupByTurn map[string]int
}

func newScheduleGroupState() *scheduleGroupState {
	return &scheduleGroupState{
		busyByGroup: make(map[int]string),
		groupByTurn: make(map[string]int),
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
	clear(state.groupByTurn)
}

func (state *scheduleGroupState) release(completion pluginapi.RequestCompletion) {
	turnID := scheduleTurnID(completion.Metadata)
	if turnID == "" {
		return
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()
	groupID, ok := state.groupByTurn[turnID]
	if !ok {
		return
	}
	delete(state.groupByTurn, turnID)
	if state.busyByGroup[groupID] == turnID {
		delete(state.busyByGroup, groupID)
	}
}

func enrichScheduleTurnMetadata(request pluginapi.RequestMetadataEnrichRequest) (pluginapi.RequestMetadataEnrichResponse, error) {
	if scheduleTurnID(request.Metadata) != "" {
		return pluginapi.RequestMetadataEnrichResponse{}, nil
	}
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return pluginapi.RequestMetadataEnrichResponse{}, fmt.Errorf("generate schedule turn id: %w", err)
	}
	return pluginapi.RequestMetadataEnrichResponse{
		Metadata: map[string]any{scheduleTurnMetadataKey: hex.EncodeToString(buffer)},
	}, nil
}

func scheduleTurnID(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	value, _ := metadata[scheduleTurnMetadataKey].(string)
	return strings.TrimSpace(value)
}

func (state *scheduleGroupState) pick(store *ipStore, settings pluginSettings, request pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	if !schedulerRequestIncludesXAI(request) {
		return pluginapi.SchedulerPickResponse{}, nil
	}
	turnID := scheduleTurnID(request.Options.Metadata)
	if turnID == "" {
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("missing %s metadata", scheduleTurnMetadataKey)
	}
	candidatesByGroup := scheduleCandidatesByGroup(request.AllCandidates, settings.ScheduleGroupCount)

	state.mutex.Lock()
	defer state.mutex.Unlock()

	if groupID, ok := state.groupByTurn[turnID]; ok {
		candidates := candidatesByGroup[groupID]
		if len(candidates) == 0 {
			return scheduleGroupRejection(
				"xai_schedule_group_exhausted",
				fmt.Sprintf("xAI schedule group %d has no remaining auth", groupID),
			), nil
		}
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: candidates[0].ID}, nil
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
		return scheduleGroupRejection(
			"xai_schedule_groups_busy",
			"no idle xAI schedule group is available",
		), nil
	}
	if err := store.incrementScheduleGroupCounter(selectedGroup); err != nil {
		return pluginapi.SchedulerPickResponse{}, err
	}
	state.busyByGroup[selectedGroup] = turnID
	state.groupByTurn[turnID] = selectedGroup
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: candidatesByGroup[selectedGroup][0].ID}, nil
}

func (controller *runtimeController) schedulerPick(request pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	controller.mutex.RLock()
	defer controller.mutex.RUnlock()
	if controller.store == nil {
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("plugin store is not initialized")
	}
	settings, err := controller.store.settings()
	if err != nil {
		return pluginapi.SchedulerPickResponse{}, err
	}
	return controller.scheduleGroups.pick(controller.store, settings, request)
}

func scheduleGroupRejection(code, message string) pluginapi.SchedulerPickResponse {
	return pluginapi.SchedulerPickResponse{
		Handled: true,
		Rejection: &pluginapi.SchedulerRejection{
			Code:       code,
			Message:    message,
			HTTPStatus: http.StatusServiceUnavailable,
			Retryable:  true,
		},
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

func (store *ipStore) ensureScheduleGroupStorage() error {
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

func (store *ipStore) reconcileScheduleGroupCounters(groupCount int) error {
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

func (store *ipStore) scheduleGroupCounters(groupCount int) (map[int]int64, error) {
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

func (store *ipStore) incrementScheduleGroupCounter(groupID int) error {
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

func (store *ipStore) resetScheduleGroupCounters() error {
	if _, err := store.database.Exec(`
UPDATE schedule_group_counters
SET call_count = 0, updated_at = ?`, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("reset schedule group counters: %w", err)
	}
	return nil
}
