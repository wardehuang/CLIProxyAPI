package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

func runQualityRound(ctx context.Context, store *ipStore, roundID int64, workerCount int, settings pluginSettings) (int64, int64, int64) {
	if err := store.updateKeepaliveQualityPhase(roundID, "quality_started", 0, 0, 0); err != nil {
		_ = store.appendLog(logLevelError, "quality.round_start_failed", 0, "", "启动智商探测阶段失败", err.Error())
		return 0, 0, 0
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
				work, err := store.claimNextFallbackWork(roundID)
				if err != nil {
					_ = store.appendLog(logLevelError, "fallback.claim_failed", 0, "", "领取健康保底节点失败", err.Error())
					return
				}
				if work == nil {
					work, err = store.claimNextQualityWork(roundID)
					if err != nil {
						_ = store.appendLog(logLevelError, "quality.claim_failed", 0, "", "领取智商探测任务失败", err.Error())
						return
					}
				}
				if work == nil {
					return
				}
				if work.Skip {
					continue
				}
				if work.Node.ProbeKind == probeKindFallback {
					connectivityResult := probeNodeWithRetries(ctx, work.Node, settings.ProbeRetryCount)
					if ctx.Err() != nil {
						if resetErr := store.resetFallbackWork(*work); resetErr != nil {
							_ = store.appendLog(logLevelError, "fallback.reset_failed", work.Node.ID, work.Node.Name, "取消保底连通探测后重置失败", resetErr.Error())
						}
						return
					}
					if !connectivityResult.Success || connectivityResult.PreserveStatus {
						failureCount.Add(1)
						if cleanupErr := store.deleteFailedFallback(*work, connectivityResult); cleanupErr != nil {
							_ = store.appendLog(logLevelError, "fallback.cleanup_failed", work.Node.ID, work.Node.Name, "删除失败保底节点失败", cleanupErr.Error())
						}
						continue
					}
					promotedWork, promoteErr := store.promoteFallbackToQuality(*work, connectivityResult)
					if promoteErr != nil {
						_ = store.appendLog(logLevelError, "fallback.promote_failed", work.Node.ID, work.Node.Name, "保底节点转入智商探测失败", promoteErr.Error())
						if resetErr := store.resetFallbackWork(*work); resetErr != nil {
							_ = store.appendLog(logLevelError, "fallback.reset_failed", work.Node.ID, work.Node.Name, "保底节点转入智商探测失败后重置失败", resetErr.Error())
						}
						continue
					}
					work = &promotedWork
				}
				candidateCount.Add(1)
				result, _, _ := runQualityProbeForWork(ctx, store, *work, settings)
				if ctx.Err() != nil {
					if resetErr := store.resetQualityWork(*work); resetErr != nil {
						_ = store.appendLog(logLevelError, "quality.reset_failed", work.Node.ID, work.Node.Name, "取消智商探测后重置失败", resetErr.Error())
					}
					return
				}
				if result.Classification == qualityClassificationNormal {
					successCount.Add(1)
				} else {
					failureCount.Add(1)
				}
				if err := store.completeQualityWork(*work, result); err != nil {
					_ = store.appendLog(logLevelError, "quality.complete_failed", work.Node.ID, work.Node.Name, "保存智商探测结果失败", err.Error())
					if resetErr := store.resetQualityWork(*work); resetErr != nil {
						_ = store.appendLog(logLevelError, "quality.reset_failed", work.Node.ID, work.Node.Name, "保存智商探测结果失败后重置失败", resetErr.Error())
					}
					continue
				}
				_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(roundID), qualityLogStatus(result), qualityLogLevel(result), "quality.completed", work.Node.ID, work.Node.Name, qualityLogMessage(result, work.Slot.ID), qualityLogDetail(result))
			}
		}()
	}
	workerGroup.Wait()
	if ctx.Err() == nil {
		promotedCount, promoteErr := store.promoteConnectedFallbackCandidates(roundID)
		if promoteErr != nil {
			_ = store.appendLog(logLevelError, "fallback.pool_promote_failed", 0, "", "整理健康保底节点失败", promoteErr.Error())
		} else if promotedCount > 0 {
			_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(roundID), logStatusConnected, logLevelInfo, "fallback.pool_promoted", 0, "", "已连通溢出节点转为健康保底", fmt.Sprintf("本轮新增健康保底节点 %d 个", promotedCount))
		}
	}
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
