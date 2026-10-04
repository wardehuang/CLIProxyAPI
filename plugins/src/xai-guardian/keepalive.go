package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

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

func (state *keepaliveScheduleState) waitForNext(ctxDone <-chan struct{}, intervalSeconds int) (triggered bool, cancelled bool) {
	state.mutex.Lock()
	pending := state.pending
	state.mutex.Unlock()
	if pending {
		state.consumeTrigger()
		return true, false
	}

	nextAt := time.Now().Add(time.Duration(intervalSeconds) * time.Second)
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

func (state *keepaliveScheduleState) snapshot(intervalSeconds int) map[string]any {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	return map[string]any{
		"running":         state.running,
		"pending":         state.pending,
		"nextAt":          state.nextAtUnixMs,
		"lastStartedAt":   state.lastStartedMs,
		"lastCompletedAt": state.lastCompletedMs,
		"intervalSeconds": intervalSeconds,
	}
}

func runKeepaliveScheduler(ctx context.Context, store *guardianStore, state *keepaliveScheduleState, settings pluginSettings) {
	_ = store.appendLog(logLevelInfo, "keepalive.scheduler_started", "保活探测 worker 已启动", fmt.Sprintf("线程数 %d，间隔 %d 秒", settings.KeepaliveWorkerCount, settings.KeepaliveIntervalSeconds))
	for {
		if ctx.Err() != nil {
			return
		}
		state.markStarted(time.Now())
		if err := runKeepaliveRound(ctx, store, settings); err != nil && ctx.Err() == nil {
			_ = store.appendLog(logLevelError, "keepalive.round_failed", "保活探测轮次失败", err.Error())
		}
		state.markCompleted(time.Now())
		if ctx.Err() != nil {
			return
		}
		triggered, cancelled := state.waitForNext(ctx.Done(), settings.KeepaliveIntervalSeconds)
		if cancelled {
			return
		}
		if triggered {
			_ = store.appendLog(logLevelInfo, "keepalive.manual_triggered", "立即保活探测已接入调度", "")
		}
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
	candidateCount, err := store.snapshotKeepaliveRound(roundID)
	if err != nil {
		_ = store.finishKeepaliveRound(roundID, "failed", 0, 0, deletedBatches, deletedNodes)
		return err
	}

	var workerGroup sync.WaitGroup
	var successCount atomic.Int64
	var failureCount atomic.Int64
	for workerIndex := 0; workerIndex < settings.KeepaliveWorkerCount; workerIndex++ {
		workerGroup.Add(1)
		go func() {
			defer workerGroup.Done()
			runKeepaliveWorker(ctx, store, roundID, settings.KeepaliveProbeRetryCount, &successCount, &failureCount)
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
	return store.appendLog(logLevelInfo, "keepalive.round_completed", "保活探测轮次完成", fmt.Sprintf("候选 %d，健康 %d，异常 %d，删除批次 %d，删除节点 %d", candidateCount, successCount.Load(), failureCount.Load(), deletedBatches, deletedNodes))
}

func runKeepaliveWorker(ctx context.Context, store *guardianStore, roundID int64, retryCount int, successCount, failureCount *atomic.Int64) {
	for {
		if ctx.Err() != nil {
			return
		}
		claim, found, err := store.claimNextKeepalive(roundID)
		if err != nil {
			_ = store.appendLog(logLevelError, "keepalive.claim_failed", "领取保活节点失败", err.Error())
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
			_ = store.appendLog(logLevelError, "keepalive.result_save_failed", "保存保活节点结果失败", err.Error())
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
