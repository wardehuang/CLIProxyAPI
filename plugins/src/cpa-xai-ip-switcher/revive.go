package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type reviveRound struct {
	ID             int64
	SequenceNumber int64
	StartedAt      int64
	CompletedAt    int64
	Status         string
	CandidateCount int64
	SuccessCount   int64
	FailureCount   int64
	DeletedCount   int64
}

func (store *ipStore) ensureReviveMetadata() error {
	_, err := store.database.Exec(`
CREATE TABLE IF NOT EXISTS revive_rounds (
    round_id INTEGER PRIMARY KEY,
    sequence_number INTEGER NOT NULL,
    started_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'running',
    candidate_count INTEGER NOT NULL DEFAULT 0,
    success_count INTEGER NOT NULL DEFAULT 0,
    failure_count INTEGER NOT NULL DEFAULT 0,
    deleted_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_revive_rounds_sequence ON revive_rounds(sequence_number DESC);
CREATE TABLE IF NOT EXISTS revive_round_nodes (
    round_id INTEGER NOT NULL,
    node_id INTEGER NOT NULL,
    PRIMARY KEY(round_id, node_id)
);
CREATE INDEX IF NOT EXISTS idx_revive_round_nodes_node_id ON revive_round_nodes(node_id);`)
	if err != nil {
		return fmt.Errorf("initialize sqlite revive metadata: %w", err)
	}
	return nil
}

func (store *ipStore) startReviveRound(roundID int64) (int64, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin sqlite revive round: %w", err)
	}
	defer transaction.Rollback()

	var sequenceNumber int64
	if err := transaction.QueryRow(`SELECT COALESCE(MAX(sequence_number), 0) + 1 FROM revive_rounds`).Scan(&sequenceNumber); err != nil {
		return 0, fmt.Errorf("read sqlite revive round sequence: %w", err)
	}
	if _, err := transaction.Exec(`
INSERT INTO revive_rounds(round_id, sequence_number, started_at, status)
VALUES (?, ?, ?, ?)`, roundID, sequenceNumber, time.Now().UnixMilli(), groupStatusRunning); err != nil {
		return 0, fmt.Errorf("insert sqlite revive round: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return 0, fmt.Errorf("commit sqlite revive round: %w", err)
	}
	return sequenceNumber, nil
}

func (store *ipStore) snapshotReviveRound(roundID int64) (int64, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin sqlite revive snapshot: %w", err)
	}
	defer transaction.Rollback()

	if _, err := transaction.Exec(`DELETE FROM revive_round_nodes WHERE round_id = ?`, roundID); err != nil {
		return 0, fmt.Errorf("clear sqlite revive snapshot: %w", err)
	}
	result, err := transaction.Exec(`
INSERT INTO revive_round_nodes(round_id, node_id)
SELECT ?, nodes.id
FROM ip_nodes AS nodes
WHERE nodes.status = ?
  AND nodes.probe_kind = ''
  AND nodes.revive_failure_count < ?
  AND (nodes.exit_country = '' OR UPPER(nodes.exit_country) = 'US')`, roundID, statusError, maxReviveFailureCount)
	if err != nil {
		return 0, fmt.Errorf("snapshot sqlite revive candidates: %w", err)
	}
	candidateCount, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read sqlite revive snapshot result: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return 0, fmt.Errorf("commit sqlite revive snapshot: %w", err)
	}
	return candidateCount, nil
}

func (store *ipStore) claimNextRevive(roundID int64) (*proxyNode, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin sqlite revive claim: %w", err)
	}
	defer transaction.Rollback()

	var node proxyNode
	err = transaction.QueryRow(`
SELECT candidates.node_id, node_name, proxy_url, host, input_ip, port, protocol, domain, batch_id, status, revive_failure_count, exit_country, revive_target_status
FROM revive_round_nodes AS candidates
JOIN ip_nodes ON ip_nodes.id = candidates.node_id
WHERE candidates.round_id = ?
  AND status = ?
  AND probe_kind = ''
  AND revive_round_id <> ?
  AND (ip_nodes.exit_country = '' OR UPPER(ip_nodes.exit_country) = 'US')
ORDER BY node_id ASC LIMIT 1`, roundID, statusError, roundID).Scan(
		&node.ID,
		&node.Name,
		&node.ProxyURL,
		&node.Host,
		&node.InputIP,
		&node.Port,
		&node.Protocol,
		&node.Domain,
		&node.BatchID,
		&node.Status,
		&node.ReviveFailureCount,
		&node.ExitCountry,
		&node.ReviveTargetStatus,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select sqlite revive claim: %w", err)
	}
	node.Name = displayProxyNodeName(node.ProxyURL, node.Name)

	startedAt := time.Now().UnixMilli()
	result, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = ?, probe_kind = ?, probe_return_status = '', revive_round_id = ?, error_reason = '', error_detail = ''
WHERE id = ? AND status = ? AND probe_kind = '' AND revive_round_id <> ?`, statusReviveProbing, startedAt, probeKindRevive, roundID, node.ID, statusError, roundID)
	if err != nil {
		return nil, fmt.Errorf("update sqlite revive claim: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read sqlite revive claim result: %w", err)
	}
	if rowsAffected != 1 {
		return nil, fmt.Errorf("sqlite revive claim lost node %d", node.ID)
	}
	if err := transaction.Commit(); err != nil {
		return nil, fmt.Errorf("commit sqlite revive claim: %w", err)
	}
	node.Status = statusReviveProbing
	node.ProbeKind = probeKindRevive
	node.ReviveRoundID = roundID
	node.ProbeStartedAt = startedAt
	return &node, nil
}

func (store *ipStore) completeReviveProbe(node proxyNode, result probeResult) (bool, error) {
	probeTime := time.Now().UnixMilli()
	targetStatus := node.ReviveTargetStatus
	if targetStatus != statusCooldown && targetStatus != statusConnected {
		targetStatus = statusConnected
	}
	if result.Reason == "非us出口" {
		_, err := store.database.Exec(`
UPDATE ip_nodes
SET status = ?, latency_ms = ?, probe_time = ?, probe_started_at = 0,
    probe_kind = '', probe_return_status = '',
    exit_ip = CASE WHEN ? <> '' THEN ? ELSE exit_ip END,
    exit_country = CASE WHEN ? <> '' THEN ? ELSE exit_country END,
    revive_failure_count = 0,
    error_reason = ?, error_detail = ?
WHERE id = ? AND status = ? AND probe_kind = ? AND revive_round_id = ?`, statusError, result.LatencyMs, probeTime, result.ExitIP, result.ExitIP, result.CountryCode, result.CountryCode, result.Reason, result.Detail, node.ID, statusReviveProbing, probeKindRevive, node.ReviveRoundID)
		if err != nil {
			return false, fmt.Errorf("save non-US revive result: %w", err)
		}
		_ = store.appendProbeLog(logCategoryReviveProbe, reviveGroupID(node.ReviveRoundID), logStatusError, logLevelWarn, "revive.non_us", node.ID, node.Name, "复活探测确认非 US 出口，节点保留为异常", formatProbeResultDetail(result))
		return false, nil
	}
	if result.Success {
		_, err := store.database.Exec(`
UPDATE ip_nodes
SET status = ?, initial_connected = 1, latency_ms = ?, probe_time = ?, probe_started_at = 0,
    probe_kind = '', probe_return_status = '',
    exit_ip = CASE WHEN ? <> '' THEN ? ELSE exit_ip END,
    exit_country = CASE WHEN ? <> '' THEN ? ELSE exit_country END,
    revive_failure_count = 0,
    error_reason = '', error_detail = ?
WHERE id = ? AND status = ? AND probe_kind = ? AND revive_round_id = ?`, targetStatus, result.LatencyMs, probeTime, result.ExitIP, result.ExitIP, result.CountryCode, result.CountryCode, result.Detail, node.ID, statusReviveProbing, probeKindRevive, node.ReviveRoundID)
		if err != nil {
			return false, fmt.Errorf("save revive success: %w", err)
		}
		_ = store.appendProbeLog(logCategoryReviveProbe, reviveGroupID(node.ReviveRoundID), logStatusConnected, logLevelInfo, "revive.completed", node.ID, node.Name, fmt.Sprintf("异常节点已恢复为 %s", targetStatus), formatProbeResultDetail(result))
		return false, nil
	}

	nextFailureCount := node.ReviveFailureCount + 1
	transaction, err := store.database.Begin()
	if err != nil {
		return false, fmt.Errorf("begin sqlite revive failure: %w", err)
	}
	defer transaction.Rollback()
	resultUpdate, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, latency_ms = ?, probe_time = ?, probe_started_at = 0,
    probe_kind = '', probe_return_status = '',
    exit_ip = CASE WHEN ? <> '' THEN ? ELSE exit_ip END,
    exit_country = CASE WHEN ? <> '' THEN ? ELSE exit_country END,
    revive_failure_count = ?, error_reason = ?, error_detail = ?
WHERE id = ? AND status = ? AND probe_kind = ? AND revive_round_id = ?`, statusError, result.LatencyMs, probeTime, result.ExitIP, result.ExitIP, result.CountryCode, result.CountryCode, nextFailureCount, result.Reason, result.Detail, node.ID, statusReviveProbing, probeKindRevive, node.ReviveRoundID)
	if err != nil {
		return false, fmt.Errorf("save revive failure: %w", err)
	}
	rowsAffected, err := resultUpdate.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read sqlite revive failure result: %w", err)
	}
	if rowsAffected != 1 {
		return false, fmt.Errorf("sqlite revive result lost node %d", node.ID)
	}

	deleted := false
	if nextFailureCount >= maxReviveFailureCount {
		deleteResult, deleteErr := transaction.Exec(`
DELETE FROM ip_nodes
WHERE id = ? AND status = ? AND probe_kind = '' AND revive_round_id = ?`, node.ID, statusError, node.ReviveRoundID)
		if deleteErr != nil {
			return false, fmt.Errorf("delete exhausted sqlite revive node: %w", deleteErr)
		}
		deletedRows, deleteErr := deleteResult.RowsAffected()
		if deleteErr != nil {
			return false, fmt.Errorf("read sqlite revive delete result: %w", deleteErr)
		}
		deleted = deletedRows == 1
	}
	if err := transaction.Commit(); err != nil {
		return false, fmt.Errorf("commit sqlite revive failure: %w", err)
	}

	if deleted {
		_ = store.appendProbeLog(logCategoryReviveProbe, reviveGroupID(node.ReviveRoundID), logStatusError, logLevelWarn, "revive.deleted", node.ID, node.Name, "节点连续复活探测失败，已永久删除", fmt.Sprintf("第 %d 次失败；%s", nextFailureCount, formatProbeResultDetail(result)))
		return true, nil
	}
	_ = store.appendProbeLog(logCategoryReviveProbe, reviveGroupID(node.ReviveRoundID), logStatusError, logLevelWarn, "revive.failed", node.ID, node.Name, fmt.Sprintf("复活探测失败，第 %d/%d 次", nextFailureCount, maxReviveFailureCount), formatProbeResultDetail(result))
	return false, nil
}

func (store *ipStore) resetReviveProbe(node proxyNode) error {
	_, err := store.database.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = 0, probe_kind = '', probe_return_status = ''
WHERE id = ? AND status = ? AND probe_kind = ? AND revive_round_id = ?`, statusError, node.ID, statusReviveProbing, probeKindRevive, node.ReviveRoundID)
	if err != nil {
		return fmt.Errorf("reset interrupted revive probe: %w", err)
	}
	return nil
}

func (store *ipStore) finishReviveRound(roundID int64, status string, candidateCount, successCount, failureCount, deletedCount int64) error {
	_, err := store.database.Exec(`
UPDATE revive_rounds
SET completed_at = ?, status = ?, candidate_count = ?, success_count = ?, failure_count = ?, deleted_count = ?
WHERE round_id = ?`, time.Now().UnixMilli(), status, candidateCount, successCount, failureCount, deletedCount, roundID)
	if err != nil {
		return fmt.Errorf("finish sqlite revive round: %w", err)
	}
	return nil
}

func reviveGroupID(roundID int64) string {
	return fmt.Sprintf("%d", roundID)
}

func (store *ipStore) listReviveLogGroups() ([]logGroup, error) {
	rows, err := store.database.Query(`
SELECT
    CAST(round_id AS TEXT),
    sequence_number,
    started_at,
    completed_at,
    status,
    (
        SELECT COUNT(*)
        FROM plugin_logs
        WHERE plugin_logs.category = ?
          AND plugin_logs.group_id = CAST(rounds.round_id AS TEXT)
          AND plugin_logs.event NOT IN ('probe.started', 'keepalive.started', 'revive.started')
    )
FROM revive_rounds AS rounds
ORDER BY sequence_number DESC, started_at DESC
LIMIT ?`, logCategoryReviveProbe, maxGroupedLogSets)
	if err != nil {
		return nil, fmt.Errorf("list sqlite revive log groups: %w", err)
	}
	defer rows.Close()

	items := make([]logGroup, 0)
	for rows.Next() {
		var item logGroup
		if err := rows.Scan(&item.ID, &item.SequenceNumber, &item.StartedAt, &item.CompletedAt, &item.Status, &item.LogCount); err != nil {
			return nil, fmt.Errorf("scan sqlite revive log group: %w", err)
		}
		item.Category = logCategoryReviveProbe
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite revive log groups: %w", err)
	}
	return items, nil
}

func runReviveScheduler(ctx context.Context, store *ipStore, settings pluginSettings) {
	_ = store.appendLog(logLevelInfo, "revive.scheduler_started", 0, "", "复活探测调度器已启动", fmt.Sprintf("首轮立即执行，后续间隔 %d 秒，线程上限 %d", settings.ReviveIntervalSeconds, settings.KeepaliveWorkerCount))
	for {
		runReviveRound(ctx, store, settings.KeepaliveWorkerCount, settings.ProbeRetryCount)
		if ctx.Err() != nil {
			return
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

func runReviveRound(ctx context.Context, store *ipStore, workerCount, probeRetryCount int) {
	defer func() {
		_ = store.pruneStoredLogs()
	}()
	roundID := time.Now().UnixNano()
	if _, err := store.startReviveRound(roundID); err != nil {
		_ = store.appendLog(logLevelError, "revive.round_create_failed", 0, "", "创建 复活探测 轮次失败", err.Error())
		return
	}
	candidateCount, err := store.snapshotReviveRound(roundID)
	if err != nil {
		_ = store.finishReviveRound(roundID, groupStatusCompleted, 0, 0, 0, 0)
		_ = store.appendProbeLog(logCategoryReviveProbe, reviveGroupID(roundID), logStatusError, logLevelError, "revive.snapshot_failed", 0, "", "生成复活探测候选快照失败", err.Error())
		return
	}
	_ = store.appendProbeLog(logCategoryReviveProbe, reviveGroupID(roundID), logStatusProbing, logLevelInfo, "revive.round_started", 0, "", "开始复活探测轮次", fmt.Sprintf("轮次 %d，候选异常节点 %d，线程上限 %d", roundID, candidateCount, workerCount))

	var workerGroup sync.WaitGroup
	var successCount atomic.Int64
	var failureCount atomic.Int64
	var deletedCount atomic.Int64
	for workerIndex := 0; workerIndex < workerCount; workerIndex++ {
		workerGroup.Add(1)
		go func() {
			defer workerGroup.Done()
			runReviveWorker(ctx, store, roundID, probeRetryCount, &successCount, &failureCount, &deletedCount)
		}()
	}
	workerGroup.Wait()

	successTotal := successCount.Load()
	failureTotal := failureCount.Load()
	deletedTotal := deletedCount.Load()
	_ = store.finishReviveRound(roundID, groupStatusCompleted, candidateCount, successTotal, failureTotal, deletedTotal)
	if ctx.Err() != nil {
		return
	}
	completionStatus := logStatusConnected
	completionLevel := logLevelInfo
	if failureTotal > 0 {
		completionStatus = logStatusError
		completionLevel = logLevelWarn
	}
	_ = store.appendProbeLog(logCategoryReviveProbe, reviveGroupID(roundID), completionStatus, completionLevel, "revive.round_completed", 0, "", "复活探测轮次完成", fmt.Sprintf("成功 %d，失败 %d，删除 %d", successTotal, failureTotal, deletedTotal))
}

func runReviveWorker(ctx context.Context, store *ipStore, roundID int64, probeRetryCount int, successCount, failureCount, deletedCount *atomic.Int64) {
	for {
		if ctx.Err() != nil {
			return
		}
		node, err := store.claimNextRevive(roundID)
		if err != nil {
			_ = store.appendLog(logLevelError, "revive.claim_failed", 0, "", "领取 复活探测 节点失败", err.Error())
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
			if resetErr := store.resetReviveProbe(*node); resetErr != nil {
				_ = store.appendProbeLog(logCategoryReviveProbe, reviveGroupID(node.ReviveRoundID), logStatusError, logLevelError, "revive.reset_failed", node.ID, node.Name, "取消 复活探测 后重置节点失败", resetErr.Error())
			}
			_ = store.appendProbeLog(logCategoryReviveProbe, reviveGroupID(node.ReviveRoundID), logStatusProbing, logLevelWarn, "revive.cancelled", node.ID, node.Name, "节点 复活探测 已取消", "插件停止中断探测")
			return
		}
		if result.Success {
			successCount.Add(1)
		} else {
			failureCount.Add(1)
		}
		deleted, completeErr := store.completeReviveProbe(*node, result)
		if completeErr != nil {
			if resetErr := store.resetReviveProbe(*node); resetErr != nil {
				_ = store.appendProbeLog(logCategoryReviveProbe, reviveGroupID(node.ReviveRoundID), logStatusError, logLevelError, "revive.reset_failed", node.ID, node.Name, "保存 复活探测 结果后重置节点失败", resetErr.Error())
			}
			_ = store.appendProbeLog(logCategoryReviveProbe, reviveGroupID(node.ReviveRoundID), logStatusError, logLevelError, "revive.save_failed", node.ID, node.Name, "保存 复活探测 结果失败", completeErr.Error())
			continue
		}
		if deleted {
			deletedCount.Add(1)
		}
	}
}
