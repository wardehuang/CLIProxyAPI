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
	mutex            sync.Mutex
	busyByGroup      map[int]string
	groupByAuth      map[string]int
	requestIDByGroup map[int]string
}

func newScheduleGroupState() *scheduleGroupState {
	return &scheduleGroupState{
		busyByGroup:      make(map[int]string),
		groupByAuth:      make(map[string]int),
		requestIDByGroup: make(map[int]string),
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
	clear(state.requestIDByGroup)
}

func (state *scheduleGroupState) release(store *guardianStore, completion pluginapi.RequestCompletion) {
	authID := selectedAuthID(completion.Metadata)
	state.mutex.Lock()
	authGroupID, authTracked := state.groupByAuth[authID]
	groupID := authGroupID
	releaseMethod := "selected_auth_id"
	mismatchReason := ""

	if completion.RequestID != "" {
		groupID = 0
		for busyGroupID, ownerRequestID := range state.requestIDByGroup {
			if ownerRequestID == completion.RequestID {
				groupID = busyGroupID
				break
			}
		}
		if groupID == 0 {
			state.mutex.Unlock()
			detail := fmt.Sprintf("request_id=%q group_id=0 outcome=%q status_code=%d reason=request_id_not_tracked", completion.RequestID, completion.Outcome, completion.StatusCode)
			_ = store.appendLog(logLevelWarn, "schedule.group_release_mismatch", "xAI scheduler could not match completion to its schedule group", detail)
			return
		}
		releaseMethod = "request_id"
		switch {
		case authID == "":
			mismatchReason = "selected_auth_id_missing"
		case !authTracked:
			mismatchReason = "selected_auth_id_not_tracked"
		case authTracked && authGroupID != groupID:
			mismatchReason = "selected_auth_id_group_mismatch"
		}
	} else if authID == "" || !authTracked {
		state.mutex.Unlock()
		return
	}

	ownerRequestID := state.requestIDByGroup[groupID]
	for candidateID, candidateGroupID := range state.groupByAuth {
		if candidateGroupID == groupID {
			delete(state.groupByAuth, candidateID)
		}
	}
	delete(state.busyByGroup, groupID)
	delete(state.requestIDByGroup, groupID)
	state.mutex.Unlock()

	if mismatchReason != "" {
		detail := fmt.Sprintf("request_id=%q group_id=%d outcome=%q status_code=%d reason=%s released=true", completion.RequestID, groupID, completion.Outcome, completion.StatusCode, mismatchReason)
		_ = store.appendLog(logLevelWarn, "schedule.group_release_mismatch", "xAI scheduler released a schedule group by request ID after selected auth mismatch", detail)
	}
	detail := fmt.Sprintf("request_id=%q owner_request_id=%q group_id=%d outcome=%q status_code=%d request_id_match=%t release_method=%s", completion.RequestID, ownerRequestID, groupID, completion.Outcome, completion.StatusCode, completion.RequestID != "" && completion.RequestID == ownerRequestID, releaseMethod)
	_ = store.appendLog(logLevelInfo, "schedule.group_released", "xAI scheduler released a schedule group", detail)
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
	candidatesByGroup, candidateSummary := scheduleCandidatesByGroup(request.Candidates, settings.ScheduleGroupCount)

	state.mutex.Lock()
	var logLevel, logEvent, logMessage, logDetail string
	defer func() {
		state.mutex.Unlock()
		if logEvent != "" {
			_ = store.appendLog(logLevel, logEvent, logMessage, logDetail)
		}
	}()

	previousAuthID := selectedAuthID(request.Options.Metadata)
	if previousAuthID != "" {
		if groupID, ok := state.groupByAuth[previousAuthID]; ok {
			candidates := candidatesByGroup[groupID]
			if len(candidates) == 0 {
				logLevel = logLevelWarn
				logEvent = "schedule.group_rejected"
				logMessage = "xAI scheduler rejected a retry without a group candidate"
				logDetail = schedulePickLogDetail(request, settings, candidateSummary, state.busyByGroup, state.requestIDByGroup, "retry_group_has_no_candidate", 0)
				return scheduleGroupRejection("xai_schedule_group_exhausted", fmt.Sprintf("xAI schedule group %d has no remaining auth", groupID)), nil
			}
			selectedID := strings.TrimSpace(candidates[0].ID)
			state.groupByAuth[selectedID] = groupID
			state.busyByGroup[groupID] = selectedID
			logLevel = logLevelInfo
			logEvent = "schedule.group_selected"
			logMessage = "xAI scheduler reused a schedule group for retry"
			logDetail = schedulePickLogDetail(request, settings, candidateSummary, state.busyByGroup, state.requestIDByGroup, "retry_same_group", groupID)
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
		decision := "no_candidate_in_any_configured_group"
		if len(candidatesByGroup) > 0 {
			decision = "all_candidate_groups_busy"
		}
		logLevel = logLevelWarn
		logEvent = "schedule.group_rejected"
		logMessage = "xAI scheduler found no selectable schedule group"
		logDetail = schedulePickLogDetail(request, settings, candidateSummary, state.busyByGroup, state.requestIDByGroup, decision, 0)
		return scheduleGroupRejection("xai_schedule_groups_busy", "no idle xAI schedule group is available"), nil
	}
	selectedID := strings.TrimSpace(candidatesByGroup[selectedGroup][0].ID)
	if err := store.incrementScheduleGroupCounter(selectedGroup); err != nil {
		return pluginapi.SchedulerPickResponse{}, err
	}
	logDetail = schedulePickLogDetail(request, settings, candidateSummary, state.busyByGroup, state.requestIDByGroup, "selected", selectedGroup)
	state.busyByGroup[selectedGroup] = selectedID
	state.groupByAuth[selectedID] = selectedGroup
	state.requestIDByGroup[selectedGroup] = request.RequestID
	logLevel = logLevelInfo
	logEvent = "schedule.group_selected"
	logMessage = "xAI scheduler selected and occupied a schedule group"
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

type scheduleCandidateSummary struct {
	total                   int
	xai                     int
	otherProvider           int
	missingScheduleGroup    int
	invalidScheduleGroup    int
	outOfRangeScheduleGroup int
	countByGroup            map[int]int
}

func scheduleCandidatesByGroup(candidates []pluginapi.SchedulerAuthCandidate, groupCount int) (map[int][]pluginapi.SchedulerAuthCandidate, scheduleCandidateSummary) {
	grouped := make(map[int][]pluginapi.SchedulerAuthCandidate, groupCount)
	summary := scheduleCandidateSummary{
		total:        len(candidates),
		countByGroup: make(map[int]int, groupCount),
	}
	for _, candidate := range candidates {
		if !strings.EqualFold(strings.TrimSpace(candidate.Provider), "xai") {
			summary.otherProvider++
			continue
		}
		summary.xai++
		rawGroupID, hasGroup := candidate.Attributes[scheduleGroupAttribute]
		if !hasGroup || strings.TrimSpace(rawGroupID) == "" {
			summary.missingScheduleGroup++
			continue
		}
		groupID, err := strconv.Atoi(strings.TrimSpace(rawGroupID))
		if err != nil {
			summary.invalidScheduleGroup++
			continue
		}
		if groupID < 1 || groupID > groupCount {
			summary.outOfRangeScheduleGroup++
			continue
		}
		grouped[groupID] = append(grouped[groupID], candidate)
		summary.countByGroup[groupID]++
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
	return grouped, summary
}

func scheduleBusyGroupIDs(busyByGroup map[int]string) []int {
	groupIDs := make([]int, 0, len(busyByGroup))
	for groupID := range busyByGroup {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Ints(groupIDs)
	return groupIDs
}

func schedulePickLogDetail(request pluginapi.SchedulerPickRequest, settings pluginSettings, summary scheduleCandidateSummary, busyByGroup, busyRequestIDs map[int]string, decision string, selectedGroup int) string {
	groupIDs := make([]int, 0, len(summary.countByGroup))
	for groupID := range summary.countByGroup {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Ints(groupIDs)
	groupCounts := make([]string, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		groupCounts = append(groupCounts, fmt.Sprintf("%d:%d", groupID, summary.countByGroup[groupID]))
	}
	busyGroupIDs := scheduleBusyGroupIDs(busyByGroup)
	busyGroupValues := make([]string, 0, len(busyGroupIDs))
	for _, groupID := range busyGroupIDs {
		busyGroupValues = append(busyGroupValues, fmt.Sprintf("%d:%q", groupID, busyRequestIDs[groupID]))
	}
	return fmt.Sprintf("request_id=%q model=%q provider=%q providers=%q stream=%t configured_group_count=%d candidates_total=%d candidates_xai=%d candidates_other_provider=%d candidates_missing_schedule_group=%d candidates_invalid_schedule_group=%d candidates_out_of_range_schedule_group=%d candidates_by_group=%s busy_groups=%s decision=%s selected_group=%d", request.RequestID, request.Model, request.Provider, strings.Join(request.Providers, ","), request.Stream, settings.ScheduleGroupCount, summary.total, summary.xai, summary.otherProvider, summary.missingScheduleGroup, summary.invalidScheduleGroup, summary.outOfRangeScheduleGroup, strings.Join(groupCounts, ","), strings.Join(busyGroupValues, ","), decision, selectedGroup)
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
