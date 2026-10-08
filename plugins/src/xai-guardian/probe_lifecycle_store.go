package main

import (
	"database/sql"
	"fmt"
	"time"
)

type probeRound struct {
	ID             int64
	StartedAt      int64
	CompletedAt    int64
	Status         string
	CandidateCount int64
	SuccessCount   int64
	FailureCount   int64
}

type healthySlot struct {
	SlotID      int64
	Kind        string
	NodeID      int64
	RefreshedAt int64
}

func (store *guardianStore) recoverProbeAndReviveState() error {
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin probe recovery: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE nodes
SET status = COALESCE(NULLIF((SELECT previous_status FROM probe_round_nodes WHERE node_id = nodes.id ORDER BY round_id DESC LIMIT 1), ''), ?)
WHERE scope = 'guard' AND status = ?`, statusUninspected, statusProbing); err != nil {
		return fmt.Errorf("restore interrupted probe nodes: %w", err)
	}
	if _, err := tx.Exec(`UPDATE nodes
SET status = COALESCE(NULLIF((SELECT previous_status FROM revive_round_nodes WHERE node_id = nodes.id ORDER BY round_id DESC LIMIT 1), ''), ?)
WHERE scope = 'guard' AND status = ?`, statusUnhealthy, statusReviveProbing); err != nil {
		return fmt.Errorf("restore interrupted revive nodes: %w", err)
	}
	if _, err := tx.Exec(`UPDATE probe_rounds SET completed_at = ?, status = 'interrupted' WHERE status = 'running'`, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("mark interrupted probe rounds: %w", err)
	}
	if _, err := tx.Exec(`UPDATE revive_rounds SET completed_at = ?, status = 'interrupted' WHERE status = 'running'`, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("mark interrupted revive rounds: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit probe recovery: %w", err)
	}
	return nil
}

func (store *guardianStore) startProbeRound() (int64, error) {
	result, err := store.database.Exec(`INSERT INTO probe_rounds(started_at, status) VALUES (?, 'running')`, time.Now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("start probe round: %w", err)
	}
	return result.LastInsertId()
}

func (store *guardianStore) snapshotProbeRound(roundID int64) (int64, error) {
	result, err := store.database.Exec(`INSERT INTO probe_round_nodes(round_id, node_id, previous_status)
SELECT ?, id, status FROM nodes WHERE scope = 'guard' AND status = ?`, roundID, statusUninspected)
	if err != nil {
		return 0, fmt.Errorf("snapshot probe nodes: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read probe snapshot count: %w", err)
	}
	if _, err := store.database.Exec(`UPDATE probe_rounds SET candidate_count = ? WHERE id = ?`, count, roundID); err != nil {
		return 0, fmt.Errorf("save probe snapshot count: %w", err)
	}
	return count, nil
}

func (store *guardianStore) claimNextProbe(roundID int64) (keepaliveNodeClaim, bool, error) {
	tx, err := store.database.Begin()
	if err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("begin probe claim: %w", err)
	}
	defer tx.Rollback()
	var claim keepaliveNodeClaim
	err = tx.QueryRow(`SELECT nodes.id, nodes.address, nodes.protocol, nodes.host, nodes.port, nodes.status, nodes.latency_ms, nodes.exit_ip, nodes.country, nodes.last_checked, nodes.last_error, nodes.created_at, probe_round_nodes.previous_status
FROM probe_round_nodes
INNER JOIN nodes ON nodes.id = probe_round_nodes.node_id
WHERE probe_round_nodes.round_id = ? AND nodes.scope = 'guard' AND nodes.status = ?
ORDER BY nodes.id LIMIT 1`, roundID, statusUninspected).Scan(
		&claim.Node.ID, &claim.Node.Address, &claim.Node.Protocol, &claim.Node.Host, &claim.Node.Port,
		&claim.Node.Status, &claim.Node.LatencyMS, &claim.Node.ExitIP, &claim.Node.Country,
		&claim.Node.LastChecked, &claim.Node.LastError, &claim.Node.CreatedAt, &claim.PreviousStatus,
	)
	if err == sql.ErrNoRows {
		return keepaliveNodeClaim{}, false, nil
	}
	if err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("read probe claim: %w", err)
	}
	if _, err := tx.Exec(`UPDATE nodes SET status = ? WHERE id = ? AND scope = 'guard' AND status = ?`, statusProbing, claim.Node.ID, statusUninspected); err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("mark probe node: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("commit probe claim: %w", err)
	}
	return claim, true, nil
}

func (store *guardianStore) updateProbeNodeResult(roundID, nodeID int64, result nodeProbeResult) error {
	result.Error = sanitizeLogText(result.Error)
	var nodeName string
	if err := store.database.QueryRow(`SELECT address FROM nodes WHERE id = ? AND scope = 'guard'`, nodeID).Scan(&nodeName); err != nil {
		return fmt.Errorf("read probe node name: %w", err)
	}
	updated, err := store.database.Exec(`UPDATE nodes SET status = ?, latency_ms = ?, exit_ip = ?, country = ?, last_checked = ?, last_error = ?
WHERE id = ? AND scope = 'guard' AND status = ? AND EXISTS (SELECT 1 FROM probe_round_nodes WHERE round_id = ? AND node_id = ?)`, result.Status, result.LatencyMS, result.ExitIP, result.Country, result.CheckedAt, result.Error, nodeID, statusProbing, roundID, nodeID)
	if err != nil {
		return fmt.Errorf("update probe node result: %w", err)
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return fmt.Errorf("read probe node update result: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("probe node %d was not claimed by round %d", nodeID, roundID)
	}
	return store.appendRoundNodeLog(logCategoryBatchProbe, roundID, logStatusForNodeResult(result.Status), logLevelInfo, "probe.node_result", nodeID, nodeName, "初次探测节点结果", fmt.Sprintf("状态=%s；延迟=%dms；出口IP=%s；国家=%s；错误=%s", result.Status, result.LatencyMS, result.ExitIP, result.Country, result.Error))
}

func (store *guardianStore) resetProbeRound(roundID int64) error {
	_, err := store.database.Exec(`UPDATE nodes SET status = COALESCE(NULLIF((SELECT previous_status FROM probe_round_nodes WHERE round_id = ? AND node_id = nodes.id), ''), ?)
WHERE scope = 'guard' AND status = ? AND id IN (SELECT node_id FROM probe_round_nodes WHERE round_id = ?)`, roundID, statusUninspected, statusProbing, roundID)
	if err != nil {
		return fmt.Errorf("reset probe round nodes: %w", err)
	}
	return nil
}

func (store *guardianStore) finishProbeRound(roundID int64, status string, successCount, failureCount int64) error {
	_, err := store.database.Exec(`UPDATE probe_rounds SET completed_at = ?, status = ?, success_count = ?, failure_count = ? WHERE id = ?`, time.Now().UnixMilli(), status, successCount, failureCount, roundID)
	if err != nil {
		return fmt.Errorf("finish probe round: %w", err)
	}
	return nil
}

func (store *guardianStore) startReviveRound() (int64, error) {
	result, err := store.database.Exec(`INSERT INTO revive_rounds(started_at, status) VALUES (?, 'running')`, time.Now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("start revive round: %w", err)
	}
	return result.LastInsertId()
}

func (store *guardianStore) snapshotReviveRound(roundID int64) (int64, error) {
	result, err := store.database.Exec(`INSERT INTO revive_round_nodes(round_id, node_id, previous_status)
SELECT ?, id, status FROM nodes WHERE scope = 'guard' AND status = ?`, roundID, statusUnhealthy)
	if err != nil {
		return 0, fmt.Errorf("snapshot revive nodes: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read revive snapshot count: %w", err)
	}
	if _, err := store.database.Exec(`UPDATE revive_rounds SET candidate_count = ? WHERE id = ?`, count, roundID); err != nil {
		return 0, fmt.Errorf("save revive snapshot count: %w", err)
	}
	return count, nil
}

func (store *guardianStore) claimNextRevive(roundID int64) (keepaliveNodeClaim, bool, error) {
	tx, err := store.database.Begin()
	if err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("begin revive claim: %w", err)
	}
	defer tx.Rollback()
	var claim keepaliveNodeClaim
	err = tx.QueryRow(`SELECT nodes.id, nodes.address, nodes.protocol, nodes.host, nodes.port, nodes.status, nodes.latency_ms, nodes.exit_ip, nodes.country, nodes.last_checked, nodes.last_error, nodes.created_at, revive_round_nodes.previous_status
FROM revive_round_nodes
INNER JOIN nodes ON nodes.id = revive_round_nodes.node_id
WHERE revive_round_nodes.round_id = ? AND nodes.scope = 'guard' AND nodes.status = ?
ORDER BY nodes.id LIMIT 1`, roundID, statusUnhealthy).Scan(
		&claim.Node.ID, &claim.Node.Address, &claim.Node.Protocol, &claim.Node.Host, &claim.Node.Port,
		&claim.Node.Status, &claim.Node.LatencyMS, &claim.Node.ExitIP, &claim.Node.Country,
		&claim.Node.LastChecked, &claim.Node.LastError, &claim.Node.CreatedAt, &claim.PreviousStatus,
	)
	if err == sql.ErrNoRows {
		return keepaliveNodeClaim{}, false, nil
	}
	if err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("read revive claim: %w", err)
	}
	if _, err := tx.Exec(`UPDATE nodes SET status = ? WHERE id = ? AND scope = 'guard' AND status = ?`, statusReviveProbing, claim.Node.ID, statusUnhealthy); err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("mark revive node: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("commit revive claim: %w", err)
	}
	return claim, true, nil
}

func (store *guardianStore) updateReviveNodeResult(roundID, nodeID int64, result nodeProbeResult, maxFailureCount int) error {
	result.Error = sanitizeLogText(result.Error)
	var nodeName string
	if err := store.database.QueryRow(`SELECT address FROM nodes WHERE id = ? AND scope = 'guard'`, nodeID).Scan(&nodeName); err != nil {
		return fmt.Errorf("read revive node name: %w", err)
	}
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin revive result update: %w", err)
	}
	defer tx.Rollback()
	var failureCount int
	if err := tx.QueryRow(`SELECT revive_failure_count FROM nodes WHERE id = ? AND scope = 'guard' AND status = ?`, nodeID, statusReviveProbing).Scan(&failureCount); err != nil {
		return fmt.Errorf("read revive failure count: %w", err)
	}
	if result.Status == statusHealthy {
		failureCount = 0
	} else {
		failureCount++
	}
	if _, err := tx.Exec(`UPDATE nodes SET status = ?, latency_ms = ?, exit_ip = ?, country = ?, last_checked = ?, last_error = ?, revive_failure_count = ?
WHERE id = ? AND scope = 'guard' AND status = ? AND EXISTS (SELECT 1 FROM revive_round_nodes WHERE round_id = ? AND node_id = ?)`, result.Status, result.LatencyMS, result.ExitIP, result.Country, result.CheckedAt, result.Error, failureCount, nodeID, statusReviveProbing, roundID, nodeID); err != nil {
		return fmt.Errorf("update revive node result: %w", err)
	}
	if result.Status != statusHealthy {
		if _, err := tx.Exec(`DELETE FROM healthy_slots WHERE node_id = ?`, nodeID); err != nil {
			return fmt.Errorf("clear failed revive slots: %w", err)
		}
		if _, err := tx.Exec(`UPDATE auth_bindings SET slot_id = 0, node_id = 0, proxy_url = '', updated_at = ? WHERE node_id = ?`, time.Now().UnixMilli(), nodeID); err != nil {
			return fmt.Errorf("clear failed revive auth bindings: %w", err)
		}
	}
	deleted := result.Status != statusHealthy && failureCount >= maxFailureCount
	if deleted {
		if _, err := tx.Exec(`DELETE FROM ip_batch_nodes WHERE node_id = ?`, nodeID); err != nil {
			return fmt.Errorf("clear deleted revive batch links: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM auth_selection_history WHERE node_id = ?`, nodeID); err != nil {
			return fmt.Errorf("clear deleted revive auth history: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM nodes WHERE id = ? AND scope = 'guard'`, nodeID); err != nil {
			return fmt.Errorf("delete revive failure node: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit revive result update: %w", err)
	}
	statusText := result.Status
	if deleted {
		statusText = "deleted_after_revive_failures"
	}
	return store.appendRoundNodeLog(logCategoryRevive, roundID, logStatusForNodeResult(result.Status), logLevelInfo, "revive.node_result", nodeID, nodeName, "复活探测节点结果", fmt.Sprintf("状态=%s；复活失败次数=%d；延迟=%dms；出口IP=%s；国家=%s；错误=%s", statusText, failureCount, result.LatencyMS, result.ExitIP, result.Country, result.Error))
}

func (store *guardianStore) resetReviveRound(roundID int64) error {
	_, err := store.database.Exec(`UPDATE nodes SET status = COALESCE(NULLIF((SELECT previous_status FROM revive_round_nodes WHERE round_id = ? AND node_id = nodes.id), ''), ?)
WHERE scope = 'guard' AND status = ? AND id IN (SELECT node_id FROM revive_round_nodes WHERE round_id = ?)`, roundID, statusUnhealthy, statusReviveProbing, roundID)
	if err != nil {
		return fmt.Errorf("reset revive round nodes: %w", err)
	}
	return nil
}

func (store *guardianStore) finishReviveRound(roundID int64, status string, successCount, failureCount int64) error {
	_, err := store.database.Exec(`UPDATE revive_rounds SET completed_at = ?, status = ?, success_count = ?, failure_count = ? WHERE id = ?`, time.Now().UnixMilli(), status, successCount, failureCount, roundID)
	if err != nil {
		return fmt.Errorf("finish revive round: %w", err)
	}
	return nil
}

func (store *guardianStore) reconcileHealthySlots(settings pluginSettings) error {
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin healthy slot reconciliation: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	cutoff := time.Now().Add(-time.Duration(settings.HealthySlotMaxAgeMinutes) * time.Minute).UnixMilli()
	rows, err := tx.Query(`SELECT id FROM nodes WHERE scope = 'guard' AND status IN (?, ?, ?) AND last_checked >= ? ORDER BY CASE WHEN latency_ms <= 0 THEN 1 ELSE 0 END, latency_ms ASC, id ASC`, statusHealthy, statusHealthyCandidate, statusHealthyFallback, cutoff)
	if err != nil {
		return fmt.Errorf("list healthy slot candidates: %w", err)
	}
	candidateIDs := make([]int64, 0)
	for rows.Next() {
		var nodeID int64
		if err := rows.Scan(&nodeID); err != nil {
			rows.Close()
			return fmt.Errorf("scan healthy slot candidate: %w", err)
		}
		candidateIDs = append(candidateIDs, nodeID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate healthy slot candidates: %w", err)
	}
	rows.Close()
	existingRows, err := tx.Query(`SELECT slot_id, slot_kind, node_id, refreshed_at FROM healthy_slots ORDER BY slot_id`)
	if err != nil {
		return fmt.Errorf("list existing healthy slots: %w", err)
	}
	existing := make(map[int64]healthySlot)
	for existingRows.Next() {
		var slot healthySlot
		if err := existingRows.Scan(&slot.SlotID, &slot.Kind, &slot.NodeID, &slot.RefreshedAt); err != nil {
			existingRows.Close()
			return fmt.Errorf("scan existing healthy slot: %w", err)
		}
		existing[slot.SlotID] = slot
	}
	if err := existingRows.Err(); err != nil {
		existingRows.Close()
		return fmt.Errorf("iterate existing healthy slots: %w", err)
	}
	if err := existingRows.Close(); err != nil {
		return fmt.Errorf("close existing healthy slots: %w", err)
	}
	if _, err := tx.Exec(`UPDATE nodes SET status = ? WHERE scope = 'guard' AND status IN (?, ?, ?)`, statusConnected, statusHealthy, statusHealthyCandidate, statusHealthyFallback); err != nil {
		return fmt.Errorf("reset healthy node slot status: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM healthy_slots`); err != nil {
		return fmt.Errorf("clear healthy slots: %w", err)
	}
	if _, err := tx.Exec(`UPDATE auth_bindings SET slot_id = 0, node_id = 0, proxy_url = '', updated_at = ?`, now); err != nil {
		return fmt.Errorf("clear healthy slot auth bindings: %w", err)
	}
	available := make(map[int64]struct{}, len(candidateIDs))
	for _, nodeID := range candidateIDs {
		available[nodeID] = struct{}{}
	}
	totalSlotCount := settings.HealthySlotCount + settings.HealthyCandidateSlotCount
	assignments := make([]healthySlot, totalSlotCount)
	for slotIndex := 1; slotIndex <= totalSlotCount; slotIndex++ {
		slotID := int64(slotIndex)
		kind := "primary"
		if slotIndex > settings.HealthySlotCount {
			kind = "candidate"
		}
		previous, exists := existing[slotID]
		if !exists || previous.NodeID == 0 || previous.Kind != kind || previous.RefreshedAt < cutoff {
			continue
		}
		if _, availableNode := available[previous.NodeID]; !availableNode {
			continue
		}
		previous.SlotID = slotID
		previous.Kind = kind
		assignments[slotIndex-1] = previous
		delete(available, previous.NodeID)
	}
	for slotIndex := 1; slotIndex <= totalSlotCount; slotIndex++ {
		slotID := int64(slotIndex)
		kind := "primary"
		status := statusHealthy
		if slotIndex > settings.HealthySlotCount {
			kind = "candidate"
			status = statusHealthyCandidate
		}
		assignment := assignments[slotIndex-1]
		if assignment.NodeID == 0 {
			for _, nodeID := range candidateIDs {
				if _, availableNode := available[nodeID]; !availableNode {
					continue
				}
				assignment = healthySlot{SlotID: slotID, Kind: kind, NodeID: nodeID, RefreshedAt: now}
				delete(available, nodeID)
				break
			}
		}
		if assignment.NodeID == 0 {
			continue
		}
		assignments[slotIndex-1] = assignment
		if _, err := tx.Exec(`INSERT INTO healthy_slots(slot_id, slot_kind, node_id, refreshed_at) VALUES (?, ?, ?, ?)`, assignment.SlotID, assignment.Kind, assignment.NodeID, assignment.RefreshedAt); err != nil {
			return fmt.Errorf("assign healthy slot %d: %w", slotID, err)
		}
		if _, err := tx.Exec(`UPDATE nodes SET status = ? WHERE id = ? AND scope = 'guard'`, status, assignment.NodeID); err != nil {
			return fmt.Errorf("mark healthy slot node %d: %w", assignment.NodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit healthy slot reconciliation: %w", err)
	}
	return nil
}

func (store *guardianStore) healthySlotSummary() (map[string]int64, error) {
	rows, err := store.database.Query(`SELECT healthy_slots.slot_kind, COUNT(*)
FROM healthy_slots
INNER JOIN nodes ON nodes.id = healthy_slots.node_id AND nodes.scope = 'guard'
WHERE (healthy_slots.slot_kind = 'primary' AND nodes.status = ?)
   OR (healthy_slots.slot_kind = 'candidate' AND nodes.status = ?)
   OR (nodes.status = ? AND EXISTS (
       SELECT 1 FROM keepalive_round_nodes AS round_nodes
       INNER JOIN keepalive_rounds AS rounds ON rounds.id = round_nodes.round_id
       WHERE round_nodes.node_id = nodes.id
         AND round_nodes.completed_at = 0
         AND rounds.status = 'running'
         AND ((healthy_slots.slot_kind = 'primary' AND round_nodes.previous_status = ?)
           OR (healthy_slots.slot_kind = 'candidate' AND round_nodes.previous_status = ?))
   ))
GROUP BY healthy_slots.slot_kind`, statusHealthy, statusHealthyCandidate, statusKeepaliveProbing, statusHealthy, statusHealthyCandidate)
	if err != nil {
		return nil, fmt.Errorf("count healthy slots: %w", err)
	}
	defer rows.Close()
	result := map[string]int64{"primary": 0, "candidate": 0}
	for rows.Next() {
		var kind string
		var count int64
		if err := rows.Scan(&kind, &count); err != nil {
			return nil, fmt.Errorf("scan healthy slot count: %w", err)
		}
		result[kind] = count
	}
	return result, rows.Err()
}
