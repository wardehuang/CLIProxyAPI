package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

func runInitialProbeScheduler(ctx context.Context, store *guardianStore, settings pluginSettings, trigger <-chan struct{}) {
	_ = store.appendLog(logLevelInfo, "probe.scheduler_started", "初次探测 worker 已启动", fmt.Sprintf("线程数 %d", settings.WorkerCount))
	firstRound := true
	for {
		if ctx.Err() != nil {
			return
		}
		if firstRound {
			firstRound = false
		} else {
			select {
			case <-ctx.Done():
				return
			case <-trigger:
			}
		}
		if err := runInitialProbeRound(ctx, store, settings); err != nil && ctx.Err() == nil {
			_ = store.appendLog(logLevelError, "probe.round_failed", "初次探测轮次失败", err.Error())
		}
	}
}

func runInitialProbeRound(ctx context.Context, store *guardianStore, settings pluginSettings) error {
	roundID, err := store.startProbeRound()
	if err != nil {
		return err
	}
	_ = store.appendRoundLog(logCategoryBatchProbe, roundID, logStatusProbing, logLevelInfo, "probe.round_started", "初次探测轮次开始", fmt.Sprintf("轮次 %d，探测线程数 %d", roundID, settings.WorkerCount))
	candidateCount, err := store.snapshotProbeRound(roundID)
	if err != nil {
		_ = store.finishProbeRound(roundID, "failed", 0, 0)
		return err
	}
	var workers sync.WaitGroup
	var successCount atomic.Int64
	var failureCount atomic.Int64
	for index := 0; index < settings.WorkerCount; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			runInitialProbeWorker(ctx, store, roundID, settings.ProbeRetryCount, &successCount, &failureCount)
		}()
	}
	workers.Wait()
	if err := store.resetProbeRound(roundID); err != nil {
		_ = store.finishProbeRound(roundID, "failed", successCount.Load(), failureCount.Load())
		return err
	}
	if ctx.Err() != nil {
		_ = store.finishProbeRound(roundID, "interrupted", successCount.Load(), failureCount.Load())
		return ctx.Err()
	}
	if err := store.reconcileHealthySlots(settings); err != nil {
		_ = store.finishProbeRound(roundID, "failed", successCount.Load(), failureCount.Load())
		return err
	}
	if err := refreshHealthyAuthDistribution(store); err != nil {
		_ = store.appendRoundLog(logCategoryBatchProbe, roundID, logStatusError, logLevelError, "auth.distribution_failed", "初次探测后刷新 auth 分配失败", sanitizeLogText(err.Error()))
	}
	if err := store.finishProbeRound(roundID, "completed", successCount.Load(), failureCount.Load()); err != nil {
		return err
	}
	return store.appendRoundLog(logCategoryBatchProbe, roundID, logStatusConnected, logLevelInfo, "probe.round_completed", "初次探测轮次完成", fmt.Sprintf("候选 %d，健康 %d，异常 %d", candidateCount, successCount.Load(), failureCount.Load()))
}

func runInitialProbeWorker(ctx context.Context, store *guardianStore, roundID int64, retryCount int, successCount, failureCount *atomic.Int64) {
	for {
		if ctx.Err() != nil {
			return
		}
		claim, found, err := store.claimNextProbe(roundID)
		if err != nil {
			_ = store.appendRoundLog(logCategoryBatchProbe, roundID, logStatusError, logLevelError, "probe.claim_failed", "领取初次探测节点失败", err.Error())
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
		if err := store.updateProbeNodeResult(roundID, claim.Node.ID, result); err != nil {
			_ = store.appendRoundLog(logCategoryBatchProbe, roundID, logStatusError, logLevelError, "probe.result_save_failed", "保存初次探测结果失败", err.Error())
			return
		}
	}
}

func runReviveScheduler(ctx context.Context, store *guardianStore, settings pluginSettings) {
	_ = store.appendLog(logLevelInfo, "revive.scheduler_started", "复活 worker 已启动", fmt.Sprintf("线程数 %d，间隔 %d 秒", settings.WorkerCount, settings.ReviveIntervalSeconds))
	for {
		if ctx.Err() != nil {
			return
		}
		if err := runReviveRound(ctx, store, settings); err != nil && ctx.Err() == nil {
			_ = store.appendLog(logLevelError, "revive.round_failed", "复活探测轮次失败", err.Error())
		}
		timer := time.NewTimer(time.Duration(settings.ReviveIntervalSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func runReviveRound(ctx context.Context, store *guardianStore, settings pluginSettings) error {
	roundID, err := store.startReviveRound()
	if err != nil {
		return err
	}
	_ = store.appendRoundLog(logCategoryRevive, roundID, logStatusProbing, logLevelInfo, "revive.round_started", "复活探测轮次开始", fmt.Sprintf("轮次 %d，探测线程数 %d", roundID, settings.WorkerCount))
	candidateCount, err := store.snapshotReviveRound(roundID)
	if err != nil {
		_ = store.finishReviveRound(roundID, "failed", 0, 0)
		return err
	}
	var workers sync.WaitGroup
	var successCount atomic.Int64
	var failureCount atomic.Int64
	for index := 0; index < settings.WorkerCount; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			runReviveWorker(ctx, store, roundID, settings.ProbeRetryCount, settings.MaxReviveFailureCount, &successCount, &failureCount)
		}()
	}
	workers.Wait()
	if err := store.resetReviveRound(roundID); err != nil {
		_ = store.finishReviveRound(roundID, "failed", successCount.Load(), failureCount.Load())
		return err
	}
	if ctx.Err() != nil {
		_ = store.finishReviveRound(roundID, "interrupted", successCount.Load(), failureCount.Load())
		return ctx.Err()
	}
	if err := store.reconcileHealthySlots(settings); err != nil {
		_ = store.finishReviveRound(roundID, "failed", successCount.Load(), failureCount.Load())
		return err
	}
	if err := refreshHealthyAuthDistribution(store); err != nil {
		_ = store.appendRoundLog(logCategoryRevive, roundID, logStatusError, logLevelError, "auth.distribution_failed", "复活探测后刷新 auth 分配失败", sanitizeLogText(err.Error()))
	}
	if err := store.finishReviveRound(roundID, "completed", successCount.Load(), failureCount.Load()); err != nil {
		return err
	}
	return store.appendRoundLog(logCategoryRevive, roundID, logStatusConnected, logLevelInfo, "revive.round_completed", "复活探测轮次完成", fmt.Sprintf("候选 %d，复活 %d，仍异常 %d", candidateCount, successCount.Load(), failureCount.Load()))
}

func runReviveWorker(ctx context.Context, store *guardianStore, roundID int64, retryCount, maxFailureCount int, successCount, failureCount *atomic.Int64) {
	for {
		if ctx.Err() != nil {
			return
		}
		claim, found, err := store.claimNextRevive(roundID)
		if err != nil {
			_ = store.appendRoundLog(logCategoryRevive, roundID, logStatusError, logLevelError, "revive.claim_failed", "领取复活节点失败", err.Error())
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
		if err := store.updateReviveNodeResult(roundID, claim.Node.ID, result, maxFailureCount); err != nil {
			_ = store.appendRoundLog(logCategoryRevive, roundID, logStatusError, logLevelError, "revive.result_save_failed", "保存复活结果失败", err.Error())
			return
		}
	}
}
