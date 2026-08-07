package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

func runKeepaliveScheduler(ctx context.Context, store *ipStore, workerCount, intervalSeconds, probeRetryCount int) {
	_ = store.appendLog(
		logLevelInfo,
		"keepalive.scheduler_started",
		0,
		"",
		"保活调度器已启动",
		fmt.Sprintf("保活线程数 %d，首轮立即执行，后续间隔 %d 秒", workerCount, intervalSeconds),
	)

	for {
		runKeepaliveRound(ctx, store, workerCount, probeRetryCount)
		if ctx.Err() != nil {
			return
		}

		timer := time.NewTimer(time.Duration(intervalSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func runKeepaliveRound(ctx context.Context, store *ipStore, workerCount, probeRetryCount int) {
	roundID := time.Now().UnixNano()
	if _, err := store.startKeepaliveRound(roundID); err != nil {
		_ = store.appendLog(logLevelError, "keepalive.round_create_failed", 0, "", "创建保活探测轮次失败", err.Error())
		return
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
		"开始保活探测轮次",
		fmt.Sprintf("轮次 %d，保活线程数 %d，候选节点 %d，仅使用触发时已完成探测的批次", roundID, workerCount, candidateCount),
	)

	var workerGroup sync.WaitGroup
	var successCount atomic.Int64
	var failureCount atomic.Int64
	for workerIndex := 0; workerIndex < workerCount; workerIndex++ {
		workerGroup.Add(1)
		go func() {
			defer workerGroup.Done()
			runKeepaliveWorker(ctx, store, roundID, probeRetryCount, &successCount, &failureCount)
		}()
	}
	workerGroup.Wait()
	if ctx.Err() != nil {
		_ = store.finishKeepaliveRound(roundID, groupStatusCompleted, candidateCount, successCount.Load(), failureCount.Load())
		return
	}

	successTotal := successCount.Load()
	failureTotal := failureCount.Load()
	_ = store.finishKeepaliveRound(roundID, groupStatusCompleted, candidateCount, successTotal, failureTotal)
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
		"保活探测轮次完成",
		fmt.Sprintf("成功 %d，失败 %d", successTotal, failureTotal),
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
			_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(node.KeepaliveRoundID), logStatusProbing, logLevelWarn, "keepalive.cancelled", node.ID, node.Name, "节点保活探测已取消", "插件配置变更或插件关闭中断保活探测")
			return
		}
		if result.Success {
			successCount.Add(1)
		} else {
			failureCount.Add(1)
		}
		if err := store.completeProbe(*node, result); err != nil {
			if resetErr := store.resetProbe(*node); resetErr != nil {
				_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(node.KeepaliveRoundID), logStatusError, logLevelError, "keepalive.reset_failed", node.ID, node.Name, "保存保活结果后重置节点失败", resetErr.Error())
			}
			_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(node.KeepaliveRoundID), logStatusError, logLevelError, "keepalive.save_failed", node.ID, node.Name, "保存保活结果失败", err.Error())
		}
	}
}
