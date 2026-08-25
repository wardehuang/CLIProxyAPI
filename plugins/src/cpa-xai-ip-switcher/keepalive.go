package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

func runKeepaliveScheduler(ctx context.Context, store *ipStore, settings pluginSettings) {
	state := pluginRuntime.keepalive
	if state == nil {
		state = newKeepaliveScheduleState()
		pluginRuntime.keepalive = state
	}
	_ = store.appendLog(
		logLevelInfo,
		"keepalive.scheduler_started",
		0,
		"",
		"保活调度器已启动",
		fmt.Sprintf("保活线程数 %d，首轮立即执行，后续间隔 %d 秒", settings.KeepaliveWorkerCount, settings.KeepaliveIntervalSeconds),
	)
	state.markWaiting(time.Now())

	for {
		if ctx.Err() != nil {
			return
		}
		startedAt := time.Now()
		state.markStarted(startedAt)
		runKeepaliveRound(ctx, store, settings)
		state.markCompleted(time.Now())
		if ctx.Err() != nil {
			return
		}

		triggered, cancelled := state.waitForNext(ctx.Done(), settings.KeepaliveIntervalSeconds)
		if cancelled {
			return
		}
		if triggered {
			_ = store.appendLog(logLevelInfo, "keepalive.manual_triggered", 0, "", "立即保活请求已接入调度", "将立即执行完整保活轮次")
		}
	}
}

func runKeepaliveRound(ctx context.Context, store *ipStore, settings pluginSettings) {
	pluginRuntime.topologyMutex.Lock()
	defer pluginRuntime.topologyMutex.Unlock()
	defer func() {
		_ = store.pruneStoredLogs()
	}()
	liveSettings, err := store.settings()
	if err != nil {
		_ = store.appendLog(logLevelError, "keepalive.settings_read_failed", 0, "", "读取保活实时配置失败", err.Error())
		return
	}
	settings.HealthySlotCount = liveSettings.HealthySlotCount
	settings.HealthyCandidateSlotCount = liveSettings.HealthyCandidateSlotCount
	if err := store.reconcileSlotLayout(pluginSettings{}, settings); err != nil {
		_ = store.appendLog(logLevelError, "keepalive.slot_layout_failed", 0, "", "保活前同步健康槽位布局失败", err.Error())
		return
	}
	if err := syncGrok2apiDegradedNodesBeforeKeepalive(store, settings); err != nil {
		_ = store.appendLog(logLevelWarn, "grok2api.degraded_sync_failed", 0, "", "保活前读取降智节点失败，继续执行保活探测", err.Error())
	}
	roundID := time.Now().UnixNano()
	if _, err := store.startKeepaliveRound(roundID); err != nil {
		_ = store.appendLog(logLevelError, "keepalive.round_create_failed", 0, "", "创建保活探测轮次失败", err.Error())
		return
	}
	expiredCount, err := store.expireStaleHealthySlots(roundID, settings.HealthySlotMaxAgeMinutes)
	if err != nil {
		_ = store.finishKeepaliveRound(roundID, groupStatusCompleted, 0, 0, 0)
		_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(roundID), logStatusError, logLevelError, "keepalive.slot_expire_failed", 0, "", "健康槽位超时清槽失败", err.Error())
		return
	}
	if expiredCount > 0 {
		if err := refreshHealthyAuthDistribution(store, settings.HealthySlotCount); err != nil {
			_ = store.appendLog(logLevelError, "auth_distribution.expire_failed", 0, "", "健康槽位超时清槽后重分配 auth 失败", err.Error())
		}
	}
	candidateCount, err := store.snapshotKeepaliveRound(roundID)
	if err != nil {
		_ = store.finishKeepaliveRound(roundID, groupStatusCompleted, 0, 0, 0)
		_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(roundID), logStatusError, logLevelError, "keepalive.snapshot_failed", 0, "", "生成保活候选快照失败", err.Error())
		return
	}
	_ = store.appendProbeLog(
		logCategoryKeepaliveProbe,
		keepaliveGroupID(roundID),
		logStatusProbing,
		logLevelInfo,
		"keepalive.round_started",
		0,
		"",
		"开始保活连通阶段",
		fmt.Sprintf("轮次 %d，保活线程数 %d，候选节点 %d，仅使用触发时已完成探测的批次", roundID, settings.KeepaliveWorkerCount, candidateCount),
	)

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
	if err := store.updateKeepaliveQualityPhase(roundID, "connectivity_completed", candidateCount, successCount.Load(), failureCount.Load()); err != nil {
		_ = store.appendLog(logLevelError, "keepalive.connectivity_phase_save_failed", 0, "", "保存保活连通阶段失败", err.Error())
	}
	if ctx.Err() != nil {
		_ = store.finishKeepaliveRound(roundID, groupStatusCompleted, candidateCount, successCount.Load(), failureCount.Load())
		return
	}

	qualityCandidateCount, qualitySuccessCount, qualityFailureCount := runQualityRound(ctx, store, roundID, settings.KeepaliveWorkerCount, settings)
	_ = store.setQualityRoundCompleted(roundID, qualityCandidateCount, qualitySuccessCount, qualityFailureCount)
	if ctx.Err() != nil {
		_ = store.finishKeepaliveRound(roundID, groupStatusCompleted, candidateCount, successCount.Load(), failureCount.Load())
		_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(roundID), logStatusProbing, logLevelWarn, "keepalive.round_cancelled", 0, "", "保活两阶段轮次已取消", fmt.Sprintf("连通成功 %d，连通失败 %d，智商候选 %d，智商通过 %d，智商未通过 %d；所有处理中节点已恢复领取前状态", successCount.Load(), failureCount.Load(), qualityCandidateCount, qualitySuccessCount, qualityFailureCount))
		return
	}
	if err := refreshHealthyAuthDistribution(store, settings.HealthySlotCount); err != nil {
		_ = store.appendLog(logLevelError, "auth_distribution.round_failed", 0, "", "保活轮次后重分配 auth 失败", err.Error())
	}
	if _, err := syncHealthySlotsToGrok2api(store, grok2apiSyncTriggerKeepalive); err != nil {
		_ = store.appendLog(logLevelError, "grok2api.keepalive_sync_failed", 0, "", "保活轮次后同步 grok2api 失败", err.Error())
	}

	failureTotal := failureCount.Load() + qualityFailureCount
	_ = store.finishKeepaliveRound(roundID, groupStatusCompleted, candidateCount, successCount.Load(), failureCount.Load())
	completionStatus := logStatusConnected
	completionLevel := logLevelInfo
	if failureTotal > 0 {
		completionStatus = logStatusError
		completionLevel = logLevelWarn
	}
	_ = store.appendProbeLog(
		logCategoryKeepaliveProbe,
		keepaliveGroupID(roundID),
		completionStatus,
		completionLevel,
		"keepalive.round_completed",
		0,
		"",
		"保活两阶段轮次完成",
		fmt.Sprintf("连通成功 %d，连通失败 %d，智商候选 %d，智商通过 %d，智商未通过 %d", successCount.Load(), failureCount.Load(), qualityCandidateCount, qualitySuccessCount, qualityFailureCount),
	)
}

func runKeepaliveWorker(ctx context.Context, store *ipStore, roundID int64, probeRetryCount int, successCount, failureCount *atomic.Int64) {
	for {
		if ctx.Err() != nil {
			return
		}
		node, err := store.claimNextKeepalive(roundID)
		if err != nil {
			_ = store.appendLog(logLevelError, "keepalive.claim_failed", 0, "", "领取保活节点失败", err.Error())
			if !waitForProbePoll(ctx) {
				return
			}
			continue
		}
		if node == nil {
			return
		}

		result := probeNodeWithRetries(ctx, *node, probeRetryCount)
		if ctx.Err() != nil {
			if resetErr := store.resetProbe(*node); resetErr != nil {
				_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(node.KeepaliveRoundID), logStatusError, logLevelError, "keepalive.reset_failed", node.ID, node.Name, "取消保活后重置节点失败", resetErr.Error())
			}
			_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(node.KeepaliveRoundID), logStatusProbing, logLevelWarn, "keepalive.cancelled", node.ID, node.Name, "节点保活探测已取消", "插件停止中断保活探测")
			return
		}
		if result.Success {
			successCount.Add(1)
		} else {
			failureCount.Add(1)
		}
		var completionErr error
		if result.Success || result.PreserveStatus {
			completionErr = store.completeProbe(*node, result)
		} else {
			completionErr = store.completeKeepaliveFailure(*node, result)
		}
		if completionErr != nil {
			if resetErr := store.resetProbe(*node); resetErr != nil {
				_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(node.KeepaliveRoundID), logStatusError, logLevelError, "keepalive.reset_failed", node.ID, node.Name, "保存保活结果失败后重置节点失败", resetErr.Error())
			}
			_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(node.KeepaliveRoundID), logStatusError, logLevelError, "keepalive.save_failed", node.ID, node.Name, "保存保活结果失败", completionErr.Error())
		}
	}
}
