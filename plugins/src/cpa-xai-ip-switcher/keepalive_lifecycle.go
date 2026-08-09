package main

import (
	"database/sql"
	"fmt"
	"time"
)

func (store *ipStore) completeKeepaliveFailure(node proxyNode, result probeResult) error {
	targetStatus := statusConnected
	if node.ProbeReturnStatus == statusCooldown {
		targetStatus = statusCooldown
	}
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite keepalive failure: %w", err)
	}
	defer transaction.Rollback()

	fallbackOrigin := false
	if node.ProbeReturnStatus == statusHealthy || node.ProbeReturnStatus == statusHealthyCandidate {
		var fallbackOriginValue int64
		if err := transaction.QueryRow(`SELECT fallback_origin FROM ip_slots WHERE node_id = ?`, node.ID).Scan(&fallbackOriginValue); err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("read sqlite failed slot origin for node %d: %w", node.ID, err)
		}
		fallbackOrigin = fallbackOriginValue == 1
	}
	if fallbackOrigin {
		if _, err := transaction.Exec(`
DELETE FROM ip_nodes
WHERE id = ? AND status = ? AND probe_kind = ? AND keepalive_round_id = ?`, node.ID, statusKeepaliveProbing, probeKindKeepalive, node.KeepaliveRoundID); err != nil {
			return fmt.Errorf("delete failed fallback-origin node %d: %w", node.ID, err)
		}
	} else {
		resultUpdate, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, latency_ms = ?, probe_time = ?, probe_started_at = 0,
    probe_kind = '', probe_return_status = '', exit_ip = CASE WHEN ? <> '' THEN ? ELSE exit_ip END,
    error_reason = ?, error_detail = ?, revive_target_status = ?
WHERE id = ? AND status = ? AND probe_kind = ? AND keepalive_round_id = ?`,
			statusError, result.LatencyMs, time.Now().UnixMilli(), result.ExitIP, result.ExitIP,
			result.Reason, result.Detail, targetStatus, node.ID, statusKeepaliveProbing, probeKindKeepalive, node.KeepaliveRoundID)
		if err != nil {
			return fmt.Errorf("save sqlite keepalive failure: %w", err)
		}
		rowsAffected, err := resultUpdate.RowsAffected()
		if err != nil {
			return fmt.Errorf("read sqlite keepalive failure result: %w", err)
		}
		if rowsAffected != 1 {
			return fmt.Errorf("sqlite keepalive failure node %d was not updated", node.ID)
		}
	}
	if node.ProbeReturnStatus == statusHealthy || node.ProbeReturnStatus == statusHealthyCandidate {
		if _, err := transaction.Exec(`
UPDATE ip_slots
SET node_id = 0, claim_node_id = 0, fallback_origin = 0, fallback_entered_round_id = 0, claim_token = '', claim_stage = '', claim_started_at = 0
WHERE node_id = ?`, node.ID); err != nil {
			return fmt.Errorf("clear sqlite failed healthy slot node %d: %w", node.ID, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite keepalive failure: %w", err)
	}
	if fallbackOrigin {
		_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(node.KeepaliveRoundID), logStatusError, logLevelWarn, "fallback.keepalive_failed", node.ID, node.Name, "保底来源节点保活失败，已永久删除", formatProbeResultDetail(result))
		return nil
	}
	_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(node.KeepaliveRoundID), logStatusError, logLevelWarn, "keepalive.failed", node.ID, node.Name, fmt.Sprintf("保活探测失败：%s，节点归类为异常，复活目标 %s", result.Reason, targetStatus), formatProbeResultDetail(result))
	return nil
}
