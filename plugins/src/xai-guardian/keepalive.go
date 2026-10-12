package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type dailyProbeSchedule struct {
	times     []time.Time
	location  *time.Location
	timesText string
	timezone  string
}

func parseDailyProbeSchedule(timesText, timezone string) (dailyProbeSchedule, error) {
	parts := strings.Split(strings.TrimSpace(timesText), ",")
	if len(parts) == 0 || len(parts) > 24 {
		return dailyProbeSchedule{}, fmt.Errorf("daily probe requires 1 to 24 daily times")
	}
	location, err := time.LoadLocation(strings.TrimSpace(timezone))
	if err != nil {
		return dailyProbeSchedule{}, fmt.Errorf("daily probe timezone must be a valid IANA timezone")
	}
	times := make([]time.Time, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	canonical := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		parsed, err := time.Parse("15:04", value)
		if err != nil || parsed.Format("15:04") != value {
			return dailyProbeSchedule{}, fmt.Errorf("daily probe times must be comma-separated HH:MM values")
		}
		minute := parsed.Hour()*60 + parsed.Minute()
		if _, exists := seen[minute]; exists {
			return dailyProbeSchedule{}, fmt.Errorf("daily probe times cannot contain duplicates")
		}
		seen[minute] = struct{}{}
		times = append(times, parsed)
		canonical = append(canonical, parsed.Format("15:04"))
	}
	sort.Slice(times, func(i, j int) bool {
		return times[i].Hour()*60+times[i].Minute() < times[j].Hour()*60+times[j].Minute()
	})
	return dailyProbeSchedule{times: times, location: location, timesText: strings.Join(canonical, ","), timezone: strings.TrimSpace(timezone)}, nil
}

func validateDailyProbeSchedule(timesText, timezone string) error {
	_, err := parseDailyProbeSchedule(timesText, timezone)
	return err
}

func nextDailyProbeAt(now time.Time, schedule dailyProbeSchedule) time.Time {
	localNow := now.In(schedule.location)
	for dayOffset := 0; dayOffset <= 2; dayOffset++ {
		date := localNow.AddDate(0, 0, dayOffset)
		for _, scheduledTime := range schedule.times {
			candidate := time.Date(date.Year(), date.Month(), date.Day(), scheduledTime.Hour(), scheduledTime.Minute(), 0, 0, schedule.location)
			if candidate.Hour() != scheduledTime.Hour() || candidate.Minute() != scheduledTime.Minute() {
				continue
			}
			if candidate.After(localNow) {
				return candidate
			}
		}
	}
	panic("validated keepalive schedule has no next time")
}

type keepaliveScheduleState struct {
	mutex           sync.Mutex
	running         bool
	pending         bool
	nextAtUnixMs    int64
	lastStartedMs   int64
	lastCompletedMs int64
	trigger         chan struct{}
}

func newKeepaliveScheduleState() *keepaliveScheduleState {
	return &keepaliveScheduleState{trigger: make(chan struct{}, 1)}
}

func (state *keepaliveScheduleState) markStarted(startedAt time.Time) {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	state.running = true
	state.pending = false
	state.nextAtUnixMs = 0
	state.lastStartedMs = startedAt.UnixMilli()
}

func (state *keepaliveScheduleState) markCompleted(completedAt time.Time) {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	state.running = false
	state.lastCompletedMs = completedAt.UnixMilli()
}

func (state *keepaliveScheduleState) requestNow() (accepted bool, running bool) {
	state.mutex.Lock()
	if state.running {
		state.mutex.Unlock()
		return false, true
	}
	state.pending = true
	state.nextAtUnixMs = 0
	state.mutex.Unlock()
	state.notify()
	return true, false
}

func (state *keepaliveScheduleState) queueNow() {
	state.mutex.Lock()
	state.pending = true
	state.nextAtUnixMs = 0
	state.mutex.Unlock()
	state.notify()
}

func (state *keepaliveScheduleState) notify() {
	select {
	case state.trigger <- struct{}{}:
	default:
	}
}

func (state *keepaliveScheduleState) waitForNext(ctxDone <-chan struct{}, schedule dailyProbeSchedule) (triggered bool, cancelled bool) {
	state.mutex.Lock()
	pending := state.pending
	state.mutex.Unlock()
	if pending {
		state.consumeTrigger()
		return true, false
	}

	nextAt := nextDailyProbeAt(time.Now(), schedule)
	state.mutex.Lock()
	if state.pending {
		state.mutex.Unlock()
		state.consumeTrigger()
		return true, false
	}
	state.nextAtUnixMs = nextAt.UnixMilli()
	state.mutex.Unlock()

	timer := time.NewTimer(time.Until(nextAt))
	defer timer.Stop()
	select {
	case <-ctxDone:
		return false, true
	case <-state.trigger:
		return true, false
	case <-timer.C:
		return false, false
	}
}

func (state *keepaliveScheduleState) consumeTrigger() {
	select {
	case <-state.trigger:
	default:
	}
}

func (state *keepaliveScheduleState) snapshot(schedule dailyProbeSchedule) map[string]any {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	return map[string]any{
		"running":         state.running,
		"pending":         state.pending,
		"nextAt":          state.nextAtUnixMs,
		"lastStartedAt":   state.lastStartedMs,
		"lastCompletedAt": state.lastCompletedMs,
		"times":           schedule.timesText,
		"timezone":        schedule.timezone,
	}
}

func runKeepaliveScheduler(ctx context.Context, store *guardianStore, state *keepaliveScheduleState, settings pluginSettings) {
	schedule, err := parseDailyProbeSchedule(settings.KeepaliveTimes, settings.ProbeTimezone)
	if err != nil {
		_ = store.appendLog(logLevelError, "keepalive.scheduler_failed", "保活调度配置无效", sanitizeLogText(err.Error()))
		return
	}
	_ = store.appendLog(logLevelInfo, "keepalive.scheduler_started", "保活探测 worker 已启动", fmt.Sprintf("线程数 %d，时刻 %s，时区 %s", settings.KeepaliveWorkerCount, schedule.timesText, schedule.timezone))
	for {
		if ctx.Err() != nil {
			return
		}
		triggered, cancelled := state.waitForNext(ctx.Done(), schedule)
		if cancelled {
			return
		}
		if triggered {
			_ = store.appendLog(logLevelInfo, "keepalive.manual_triggered", "立即保活探测已接入调度", "")
		}
		state.markStarted(time.Now())
		if err := runKeepaliveRound(ctx, store, settings); err != nil && ctx.Err() == nil {
			_ = store.appendLog(logLevelError, "keepalive.round_failed", "保活探测轮次失败", err.Error())
		}
		state.markCompleted(time.Now())
	}
}

func runKeepaliveRound(ctx context.Context, store *guardianStore, settings pluginSettings) error {
	roundID, err := store.startKeepaliveRound()
	if err != nil {
		return err
	}
	deletedBatches, deletedNodes, err := store.deleteExpiredIPBatches(time.Now(), settings.IPBatchRetentionDays)
	if err != nil {
		_ = store.finishKeepaliveRound(roundID, "failed", 0, 0, 0, 0)
		return err
	}
	if deletedNodes > 0 {
		if err := refreshHealthyAuthDistribution(store); err != nil {
			_ = store.appendRoundLog(logCategoryKeepalive, roundID, logStatusError, logLevelError, "auth.distribution_failed", "过期节点清理后刷新 auth 分配失败", sanitizeLogText(err.Error()))
		}
	}
	candidateCount, err := store.snapshotKeepaliveRound(roundID)
	if err != nil {
		_ = store.finishKeepaliveRound(roundID, "failed", 0, 0, deletedBatches, deletedNodes)
		return err
	}
	_ = store.appendRoundLog(logCategoryKeepalive, roundID, logStatusProbing, logLevelInfo, "keepalive.round_started", "保活探测轮次开始", fmt.Sprintf("轮次 %d，保活线程数 %d，候选节点 %d", roundID, settings.KeepaliveWorkerCount, candidateCount))

	var workerGroup sync.WaitGroup
	var successCount atomic.Int64
	var failureCount atomic.Int64
	for workerIndex := 0; workerIndex < settings.KeepaliveWorkerCount; workerIndex++ {
		workerGroup.Add(1)
		go func() {
			defer workerGroup.Done()
			runKeepaliveWorker(ctx, store, roundID, settings.ProbeRetryCount, &successCount, &failureCount)
		}()
	}
	workerGroup.Wait()
	if err := store.resetKeepaliveRound(roundID); err != nil {
		_ = store.finishKeepaliveRound(roundID, "failed", successCount.Load(), failureCount.Load(), deletedBatches, deletedNodes)
		return err
	}
	if ctx.Err() != nil {
		_ = store.finishKeepaliveRound(roundID, "interrupted", successCount.Load(), failureCount.Load(), deletedBatches, deletedNodes)
		return ctx.Err()
	}
	if err := store.finishKeepaliveRound(roundID, "completed", successCount.Load(), failureCount.Load(), deletedBatches, deletedNodes); err != nil {
		return err
	}
	if err := store.reconcileHealthySlots(settings); err != nil {
		return err
	}
	if err := refreshHealthyAuthDistribution(store); err != nil {
		_ = store.appendRoundLog(logCategoryKeepalive, roundID, logStatusError, logLevelError, "auth.distribution_failed", "保活探测后刷新 auth 分配失败", sanitizeLogText(err.Error()))
	}
	return store.appendRoundLog(logCategoryKeepalive, roundID, logStatusConnected, logLevelInfo, "keepalive.round_completed", "保活探测轮次完成", fmt.Sprintf("候选 %d，健康 %d，异常 %d，删除批次 %d，删除节点 %d", candidateCount, successCount.Load(), failureCount.Load(), deletedBatches, deletedNodes))
}

func runKeepaliveWorker(ctx context.Context, store *guardianStore, roundID int64, retryCount int, successCount, failureCount *atomic.Int64) {
	for {
		if ctx.Err() != nil {
			return
		}
		claim, found, err := store.claimNextKeepalive(roundID)
		if err != nil {
			_ = store.appendRoundLog(logCategoryKeepalive, roundID, logStatusError, logLevelError, "keepalive.claim_failed", "领取保活节点失败", err.Error())
			return
		}
		if !found {
			return
		}
		result := probeKeepaliveNodeWithRetries(ctx, claim.Node, retryCount)
		if ctx.Err() != nil {
			return
		}
		if result.Status == statusHealthy {
			successCount.Add(1)
		} else {
			failureCount.Add(1)
		}
		if err := store.updateKeepaliveNodeResult(roundID, claim.Node.ID, result); err != nil {
			_ = store.appendRoundLog(logCategoryKeepalive, roundID, logStatusError, logLevelError, "keepalive.result_save_failed", "保存保活节点结果失败", err.Error())
			return
		}
	}
}

func probeKeepaliveNodeWithRetries(ctx context.Context, node proxyNode, retryCount int) nodeProbeResult {
	result := nodeProbeResult{Status: statusUnhealthy, CheckedAt: time.Now().UnixMilli()}
	client, err := newProxyHTTPClient(node.Address)
	if err != nil {
		result.Error = sanitizeLogText(err.Error())
		return result
	}
	for attempt := 0; attempt < retryCount; attempt++ {
		result = probeKeepaliveNode(ctx, client)
		if result.Status == statusHealthy || ctx.Err() != nil || attempt+1 == retryCount {
			return result
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result
		case <-timer.C:
		}
	}
	return result
}

func probeKeepaliveNode(ctx context.Context, client *http.Client) nodeProbeResult {
	started := time.Now()
	result := nodeProbeResult{Status: statusUnhealthy, CheckedAt: started.UnixMilli()}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, xAIProbeURL, nil)
	if err != nil {
		result.Error = "create_probe_request"
		return result
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "xAI-Guardian/0.1.0")
	response, err := client.Do(request)
	if err != nil {
		result.LatencyMS = time.Since(started).Milliseconds()
		result.Error = sanitizeLogText(err.Error())
		return result
	}
	result.LatencyMS = time.Since(started).Milliseconds()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		result.Error = fmt.Sprintf("xai_http_%d", response.StatusCode)
		return result
	}
	result.ExitIP, result.Country, err = probeExitTrace(ctx, client)
	if err != nil {
		result.Error = sanitizeLogText(err.Error())
		return result
	}
	result.Status = statusHealthy
	return result
}
