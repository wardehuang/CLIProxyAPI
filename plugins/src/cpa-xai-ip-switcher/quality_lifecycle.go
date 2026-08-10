package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

func runQualityRound(ctx context.Context, store *ipStore, roundID int64, workerCount int, settings pluginSettings) (int64, int64, int64) {
	if err := store.updateKeepaliveQualityPhase(roundID, "quality_started", 0, 0, 0); err != nil {
		_ = store.appendProbeLog(logCategoryQualityProbe, keepaliveGroupID(roundID), logStatusError, logLevelError, "quality.round_start_failed", 0, "", "启动智商探测阶段失败", err.Error())
		return 0, 0, 0
	}
	authFiles, authSnapshotErr := listAuthFiles()
	if authSnapshotErr != nil {
		_ = store.appendProbeLog(
			logCategoryQualityProbe,
			keepaliveGroupID(roundID),
			logStatusProbing,
			logLevelWarn,
			"quality.auth_snapshot_unavailable",
			0,
			"",
			"智商探测 auth 快照不可用，本轮不再重复读取 Host Auth",
			authSnapshotErr.Error(),
		)
	}
	qualityWorkerCount := settings.QualityWorkerCount
	if qualityWorkerCount < 1 {
		qualityWorkerCount = workerCount
	}
	var workerGroup sync.WaitGroup
	var candidateCount atomic.Int64
	var successCount atomic.Int64
	var failureCount atomic.Int64
	for workerIndex := 0; workerIndex < qualityWorkerCount; workerIndex++ {
		workerGroup.Add(1)
		go func() {
			defer workerGroup.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				work, err := store.claimNextQualityWork(roundID)
				if err != nil {
					_ = store.appendProbeLog(logCategoryQualityProbe, keepaliveGroupID(roundID), logStatusError, logLevelError, "quality.claim_failed", 0, "", "领取智商探测任务失败", err.Error())
					return
				}
				if work == nil {
					return
				}
				if work.Skip {
					continue
				}
				candidateCount.Add(1)
				result, _, _ := runQualityProbeForWork(ctx, store, *work, settings, authFiles)
				if ctx.Err() != nil {
					if resetErr := store.resetQualityWork(*work); resetErr != nil {
						_ = store.appendProbeLog(logCategoryQualityProbe, keepaliveGroupID(roundID), logStatusError, logLevelError, "quality.reset_failed", work.Node.ID, work.Node.Name, "取消智商探测后重置失败", resetErr.Error())
					}
					return
				}
				if result.Classification == qualityClassificationNormal {
					successCount.Add(1)
				} else {
					failureCount.Add(1)
				}
				if err := store.completeQualityWork(*work, result); err != nil {
					_ = store.appendProbeLog(logCategoryQualityProbe, keepaliveGroupID(roundID), logStatusError, logLevelError, "quality.complete_failed", work.Node.ID, work.Node.Name, "保存智商探测结果失败", err.Error())
					if resetErr := store.resetQualityWork(*work); resetErr != nil {
						_ = store.appendProbeLog(logCategoryQualityProbe, keepaliveGroupID(roundID), logStatusError, logLevelError, "quality.reset_failed", work.Node.ID, work.Node.Name, "保存智商探测结果失败后重置失败", resetErr.Error())
					}
					continue
				}
				_ = store.appendProbeLog(logCategoryQualityProbe, keepaliveGroupID(roundID), qualityLogStatus(result), qualityLogLevel(result), "quality.completed", work.Node.ID, work.Node.Name, qualityLogMessage(result, work.Slot.ID), qualityLogDetail(result))
			}
		}()
	}
	workerGroup.Wait()
	return candidateCount.Load(), successCount.Load(), failureCount.Load()
}

func qualityLogStatus(result qualityProbeResult) string {
	if result.Classification == qualityClassificationNormal {
		return logStatusConnected
	}
	return logStatusError
}

func qualityLogLevel(result qualityProbeResult) string {
	if result.Classification == qualityClassificationNormal {
		return logLevelInfo
	}
	return logLevelWarn
}

func qualityLogMessage(result qualityProbeResult, slotID int64) string {
	if result.Classification == qualityClassificationNormal {
		return fmt.Sprintf("槽位 %d 智商检测通过，正式占槽", slotID)
	}
	return fmt.Sprintf("槽位 %d 智商检测未通过，结果 %s", slotID, result.Classification)
}

func qualityLogDetail(result qualityProbeResult) string {
	return fmt.Sprintf("分类=%s；等级=%s；原因=%s；HTTP=%d；TTFB=%dms；首生成=%dms；生成=%dms；总耗时=%dms；tokens=%d；reasoning_tokens=%d；TPS=%.2f；soft_tps=%.2f；hard_tps=%.2f；答案命中=%t；thinking_delta=%t；详情=%s", result.Classification, result.QualityLevel, result.ClassificationReason, result.StatusCode, result.TTFBMs, result.FirstTokenMs, result.GenerationMs, result.TotalMs, result.OutputTokens, result.ReasoningTokens, result.OutputTokensPerSecond, result.QualitySoftTPS, result.QualityHardTPS, result.AnswerMatched, result.ThinkingDelta, result.Detail)
}
