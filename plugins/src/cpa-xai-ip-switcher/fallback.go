package main

import (
	"database/sql"
	"fmt"
	"time"
)

func (store *ipStore) removeExpiredFallbackSlots(roundID int64) (int64, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin sqlite expired fallback cleanup: %w", err)
	}
	defer transaction.Rollback()

	if _, err := transaction.Exec(`
DELETE FROM ip_slot_auth_bindings
WHERE slot_id IN (
    SELECT slot_id
    FROM ip_slots
    WHERE fallback_origin = 1 AND fallback_entered_round_id <> ?
)`, roundID); err != nil {
		return 0, fmt.Errorf("delete sqlite expired fallback bindings: %w", err)
	}
	if _, err := transaction.Exec(`
DELETE FROM ip_nodes
WHERE id IN (
    SELECT node_id
    FROM ip_slots
    WHERE fallback_origin = 1 AND fallback_entered_round_id <> ? AND node_id > 0
)`, roundID); err != nil {
		return 0, fmt.Errorf("delete sqlite expired fallback nodes: %w", err)
	}
	cleanupResult, err := transaction.Exec(`
UPDATE ip_slots
SET node_id = 0, claim_node_id = 0, fallback_origin = 0, fallback_entered_round_id = 0,
    claim_token = '', claim_stage = '', claim_started_at = 0,
    last_processed_round_id = 0, blocked_round_id = 0
WHERE fallback_origin = 1 AND fallback_entered_round_id <> ?`, roundID)
	if err != nil {
		return 0, fmt.Errorf("clear sqlite expired fallback slots: %w", err)
	}
	cleanedSlotCount, err := cleanupResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read sqlite expired fallback cleanup result: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return 0, fmt.Errorf("commit sqlite expired fallback cleanup: %w", err)
	}
	return cleanedSlotCount, nil
}

func (store *ipStore) claimNextFallbackWork(roundID int64) (*qualityWork, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin sqlite fallback claim: %w", err)
	}
	defer transaction.Rollback()

	var slot slotRecord
	err = transaction.QueryRow(`
SELECT slot_id, slot_kind, node_id, claim_node_id, fallback_origin, fallback_entered_round_id, claim_token, claim_stage,
       claim_started_at, last_processed_round_id, blocked_round_id
FROM ip_slots AS slots
WHERE slots.node_id = 0
  AND slots.claim_token = ''
  AND slots.last_processed_round_id <> ?
  AND EXISTS (
      SELECT 1
      FROM ip_nodes AS candidates
      WHERE candidates.status = ? AND candidates.probe_kind = ''
  )
ORDER BY slots.slot_id ASC
LIMIT 1`, roundID, statusHealthyFallback).Scan(
		&slot.ID,
		&slot.Kind,
		&slot.NodeID,
		&slot.ClaimNodeID,
		&slot.FallbackOrigin,
		&slot.FallbackEnteredRoundID,
		&slot.ClaimToken,
		&slot.ClaimStage,
		&slot.ClaimStartedAt,
		&slot.LastProcessedRoundID,
		&slot.BlockedRoundID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select sqlite fallback slot: %w", err)
	}

	var node proxyNode
	if err := scanProxyNode(transaction.QueryRow(`
SELECT id, node_name, proxy_url, host, input_ip, port, protocol, domain, batch_id, status,
       initial_connected, probe_kind, probe_return_status, keepalive_round_id, revive_round_id,
       revive_failure_count, latency_ms, entered_at, probe_started_at, probe_time, exit_ip,
       exit_country, error_reason, error_detail, revive_target_status
FROM ip_nodes
WHERE status = ? AND probe_kind = ''
ORDER BY id ASC
LIMIT 1`, statusHealthyFallback), &node); err != nil {
		return nil, fmt.Errorf("select sqlite fallback node: %w", err)
	}
	node.Name = displayProxyNodeName(node.ProxyURL, node.Name)

	claimToken := newSlotClaimToken(slot.ID, roundID)
	startedAt := time.Now().UnixMilli()
	result, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = ?, probe_kind = ?, probe_return_status = ?, keepalive_round_id = ?,
    error_reason = '', error_detail = ''
WHERE id = ? AND status = ? AND probe_kind = ''`, statusKeepaliveProbing, startedAt, probeKindFallback, statusHealthyFallback, roundID, node.ID, statusHealthyFallback)
	if err != nil {
		return nil, fmt.Errorf("claim sqlite fallback node %d: %w", node.ID, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read sqlite fallback node claim result: %w", err)
	}
	if rowsAffected != 1 {
		return nil, fmt.Errorf("sqlite fallback claim lost node %d", node.ID)
	}
	result, err = transaction.Exec(`
UPDATE ip_slots
SET claim_node_id = ?, claim_token = ?, claim_stage = ?, claim_started_at = ?, blocked_round_id = 0
WHERE slot_id = ? AND claim_token = ''`, node.ID, claimToken, probeKindFallback, startedAt, slot.ID)
	if err != nil {
		return nil, fmt.Errorf("claim sqlite fallback slot %d: %w", slot.ID, err)
	}
	rowsAffected, err = result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read sqlite fallback slot claim result: %w", err)
	}
	if rowsAffected != 1 {
		return nil, fmt.Errorf("sqlite fallback slot %d claim was lost", slot.ID)
	}
	if err := transaction.Commit(); err != nil {
		return nil, fmt.Errorf("commit sqlite fallback claim: %w", err)
	}

	node.Status = statusKeepaliveProbing
	node.ProbeKind = probeKindFallback
	node.ProbeReturnStatus = statusHealthyFallback
	node.KeepaliveRoundID = roundID
	node.ProbeStartedAt = startedAt
	node.SlotID = slot.ID
	return &qualityWork{
		Slot:           slot,
		Node:           node,
		PreviousStatus: statusHealthyFallback,
		FallbackOrigin: true,
		ClaimToken:     claimToken,
		RoundID:        roundID,
	}, nil
}

func (store *ipStore) resetFallbackWork(work qualityWork) error {
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite fallback reset: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = 0, probe_kind = '', probe_return_status = '', keepalive_round_id = 0
WHERE id = ? AND status = ? AND probe_kind = ? AND keepalive_round_id = ?`, statusHealthyFallback, work.Node.ID, statusKeepaliveProbing, probeKindFallback, work.RoundID); err != nil {
		return fmt.Errorf("restore sqlite fallback node %d: %w", work.Node.ID, err)
	}
	if _, err := transaction.Exec(`
UPDATE ip_slots
SET node_id = ?, claim_node_id = 0, fallback_origin = ?, fallback_entered_round_id = ?, claim_token = '', claim_stage = '',
    claim_started_at = ?, last_processed_round_id = ?, blocked_round_id = ?
WHERE slot_id = ? AND claim_token = ?`, work.Slot.NodeID, boolInteger(work.Slot.FallbackOrigin), work.Slot.FallbackEnteredRoundID, work.Slot.ClaimStartedAt, work.Slot.LastProcessedRoundID, work.Slot.BlockedRoundID, work.Slot.ID, work.ClaimToken); err != nil {
		return fmt.Errorf("reset sqlite fallback slot %d: %w", work.Slot.ID, err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite fallback reset: %w", err)
	}
	return nil
}

func (store *ipStore) deleteFailedFallback(work qualityWork, result probeResult) error {
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite failed fallback cleanup: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec(`
DELETE FROM ip_nodes
WHERE id = ? AND status = ? AND probe_kind = ? AND keepalive_round_id = ?`, work.Node.ID, statusKeepaliveProbing, probeKindFallback, work.RoundID); err != nil {
		return fmt.Errorf("delete sqlite failed fallback node %d: %w", work.Node.ID, err)
	}
	if _, err := transaction.Exec(`
UPDATE ip_slots
SET claim_node_id = 0, claim_token = '', claim_stage = '', claim_started_at = 0
WHERE slot_id = ? AND claim_token = ?`, work.Slot.ID, work.ClaimToken); err != nil {
		return fmt.Errorf("clear sqlite failed fallback slot %d: %w", work.Slot.ID, err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite failed fallback cleanup: %w", err)
	}
	_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(work.RoundID), logStatusError, logLevelWarn, "fallback.connectivity_failed", work.Node.ID, work.Node.Name, fmt.Sprintf("保底节点连通探测失败，已永久删除并继续领取；槽位 %d", work.Slot.ID), formatProbeResultDetail(result))
	return nil
}

func (store *ipStore) promoteFallbackToQuality(work qualityWork, result probeResult) (qualityWork, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return qualityWork{}, fmt.Errorf("begin sqlite fallback quality promotion: %w", err)
	}
	defer transaction.Rollback()
	qualityStartedAt := time.Now().UnixMilli()
	if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = ?, probe_kind = ?, probe_return_status = ?,
    probe_time = ?, latency_ms = ?, keepalive_round_id = ?
WHERE id = ? AND status = ? AND probe_kind = ? AND keepalive_round_id = ?`, statusQualityProbing, qualityStartedAt, probeKindQuality, statusHealthyFallback, time.Now().UnixMilli(), result.LatencyMs, work.RoundID, work.Node.ID, statusKeepaliveProbing, probeKindFallback, work.RoundID); err != nil {
		return qualityWork{}, fmt.Errorf("promote sqlite fallback node %d to quality: %w", work.Node.ID, err)
	}
	if _, err := transaction.Exec(`
UPDATE ip_slots
SET claim_stage = ?, claim_started_at = ?
WHERE slot_id = ? AND claim_token = ?`, probeKindQuality, qualityStartedAt, work.Slot.ID, work.ClaimToken); err != nil {
		return qualityWork{}, fmt.Errorf("promote sqlite fallback slot %d to quality: %w", work.Slot.ID, err)
	}
	if err := transaction.Commit(); err != nil {
		return qualityWork{}, fmt.Errorf("commit sqlite fallback quality promotion: %w", err)
	}
	work.Node.Status = statusQualityProbing
	work.Node.ProbeKind = probeKindQuality
	work.Node.ProbeReturnStatus = statusHealthyFallback
	work.Node.ProbeStartedAt = qualityStartedAt
	work.Node.LatencyMs = result.LatencyMs
	return work, nil
}

func (store *ipStore) promoteConnectedFallbackCandidates(roundID int64) (int64, error) {
	result, err := store.database.Exec(`
UPDATE ip_nodes
SET status = ?, revive_target_status = ?
WHERE status = ?
  AND initial_connected = 1
  AND probe_kind = ''
  AND quality_deferred_round_id <> ?
  AND NOT EXISTS (
      SELECT 1 FROM ip_slots AS slots WHERE slots.node_id = ip_nodes.id
  )`, statusHealthyFallback, statusConnected, statusConnected, roundID)
	if err != nil {
		return 0, fmt.Errorf("promote sqlite connected fallback candidates: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read promoted sqlite fallback candidate count: %w", err)
	}
	return count, nil
}
