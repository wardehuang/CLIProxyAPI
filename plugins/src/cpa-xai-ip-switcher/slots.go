package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type slotRecord struct {
	ID                     int64
	Kind                   string
	NodeID                 int64
	ClaimNodeID            int64
	FallbackOrigin         bool
	FallbackEnteredRoundID int64
	ClaimToken             string
	ClaimStage             string
	ClaimStartedAt         int64
	LastProcessedRoundID   int64
	BlockedRoundID         int64
}

type qualityWork struct {
	Slot           slotRecord
	Node           proxyNode
	PreviousStatus string
	NewCandidate   bool
	FallbackOrigin bool
	ClaimToken     string
	RoundID        int64
	Skip           bool
}

type authBinding struct {
	SlotID       int64
	NodeID       int64
	AuthName     string
	AuthIndex    string
	AuthIdentity string
	ProxyURL     string
	SyncStatus   string
	SyncError    string
	VerifiedAt   int64
	UpdatedAt    int64
}

func (store *ipStore) ensureSlotMetadata() error {
	if err := ensureSQLiteColumn(store.database, "ip_nodes", "revive_target_status", "TEXT NOT NULL DEFAULT 'connected'"); err != nil {
		return err
	}
	if err := ensureSQLiteColumn(store.database, "ip_nodes", "quality_deferred_round_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureSQLiteColumn(store.database, "ip_nodes", "manual_fallback", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{{"connectivity_completed_at", "INTEGER NOT NULL DEFAULT 0"}, {"quality_started_at", "INTEGER NOT NULL DEFAULT 0"}, {"quality_completed_at", "INTEGER NOT NULL DEFAULT 0"}, {"quality_candidate_count", "INTEGER NOT NULL DEFAULT 0"}, {"quality_success_count", "INTEGER NOT NULL DEFAULT 0"}, {"quality_failure_count", "INTEGER NOT NULL DEFAULT 0"}} {
		if err := ensureSQLiteColumn(store.database, "keepalive_rounds", column.name, column.definition); err != nil {
			return err
		}
	}
	_, err := store.database.Exec(`
CREATE TABLE IF NOT EXISTS ip_slots (
    slot_id INTEGER PRIMARY KEY,
    slot_kind TEXT NOT NULL,
    node_id INTEGER NOT NULL DEFAULT 0,
    claim_node_id INTEGER NOT NULL DEFAULT 0,
    fallback_origin INTEGER NOT NULL DEFAULT 0,
    fallback_entered_round_id INTEGER NOT NULL DEFAULT 0,
    claim_token TEXT NOT NULL DEFAULT '',
    claim_stage TEXT NOT NULL DEFAULT '',
    claim_started_at INTEGER NOT NULL DEFAULT 0,
    last_processed_round_id INTEGER NOT NULL DEFAULT 0,
    blocked_round_id INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ip_slots_node_id ON ip_slots(node_id);
CREATE INDEX IF NOT EXISTS idx_ip_slots_claim ON ip_slots(claim_token, slot_id);
CREATE TABLE IF NOT EXISTS ip_slot_auth_bindings (
    auth_name TEXT PRIMARY KEY,
    auth_index TEXT NOT NULL DEFAULT '',
    auth_identity TEXT NOT NULL DEFAULT '',
    slot_id INTEGER NOT NULL DEFAULT 0,
    node_id INTEGER NOT NULL DEFAULT 0,
    proxy_url TEXT NOT NULL DEFAULT '',
    sync_status TEXT NOT NULL,
    sync_error TEXT NOT NULL DEFAULT '',
    verified_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ip_slot_auth_bindings_node ON ip_slot_auth_bindings(node_id, slot_id, auth_name);
CREATE TABLE IF NOT EXISTS quality_probe_attempts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    round_id INTEGER NOT NULL,
    slot_id INTEGER NOT NULL,
    node_id INTEGER NOT NULL,
    auth_name TEXT NOT NULL DEFAULT '',
    auth_index TEXT NOT NULL DEFAULT '',
    auth_identity TEXT NOT NULL DEFAULT '',
    selection_source TEXT NOT NULL DEFAULT '',
    proxy_url TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    finished_at INTEGER NOT NULL,
    status_code INTEGER NOT NULL DEFAULT 0,
    classification TEXT NOT NULL DEFAULT '',
    quality_level TEXT NOT NULL DEFAULT '',
    classification_reason TEXT NOT NULL DEFAULT '',
    ttfb_ms INTEGER NOT NULL DEFAULT 0,
    first_token_ms INTEGER NOT NULL DEFAULT 0,
    generation_ms INTEGER NOT NULL DEFAULT 0,
    total_ms INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens_per_second REAL NOT NULL DEFAULT 0,
    error_code TEXT NOT NULL DEFAULT '',
    error_detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_quality_probe_attempts_round ON quality_probe_attempts(round_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_quality_probe_attempts_node ON quality_probe_attempts(node_id, finished_at DESC);
CREATE TABLE IF NOT EXISTS auth_selection_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    selected_at INTEGER NOT NULL,
    auth_name TEXT NOT NULL,
    auth_index TEXT NOT NULL DEFAULT '',
    auth_identity TEXT NOT NULL,
    selection_source TEXT NOT NULL,
    node_id INTEGER NOT NULL DEFAULT 0,
    slot_id INTEGER NOT NULL DEFAULT 0,
    round_id INTEGER NOT NULL DEFAULT 0,
    was_success INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_auth_selection_history_recent ON auth_selection_history(selected_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_auth_selection_history_node ON auth_selection_history(node_id, slot_id, was_success, selected_at DESC);
CREATE INDEX IF NOT EXISTS idx_auth_selection_history_identity ON auth_selection_history(auth_identity, selected_at DESC);
INSERT OR IGNORE INTO ip_node_statuses(status, display_name, sort_order) VALUES ('quality_probing', '智商探测中', 75);`)
	if err != nil {
		return fmt.Errorf("initialize sqlite slot metadata: %w", err)
	}
	if err := ensureSQLiteColumn(store.database, "ip_slots", "claim_node_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := store.database.Exec(`CREATE INDEX IF NOT EXISTS idx_ip_slots_claim_node_id ON ip_slots(claim_node_id)`); err != nil {
		return fmt.Errorf("create sqlite slot claim node index: %w", err)
	}
	if err := ensureSQLiteColumn(store.database, "quality_probe_attempts", "ttfb_ms", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return nil
}

func ensureSQLiteColumn(database *sql.DB, tableName, columnName, columnDefinition string) error {
	rows, err := database.Query("PRAGMA table_info(" + tableName + ")")
	if err != nil {
		return fmt.Errorf("inspect sqlite %s columns: %w", tableName, err)
	}
	defer rows.Close()
	for rows.Next() {
		var columnID int
		var currentName, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&columnID, &currentName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan sqlite %s columns: %w", tableName, err)
		}
		if currentName == columnName {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate sqlite %s columns: %w", tableName, err)
	}
	if _, err := database.Exec("ALTER TABLE " + tableName + " ADD COLUMN " + columnName + " " + columnDefinition); err != nil {
		return fmt.Errorf("add sqlite %s.%s: %w", tableName, columnName, err)
	}
	return nil
}

func defaultPluginSettings() pluginSettings {
	return pluginSettings{
		WorkerCount:                defaultWorkerCount,
		RefreshIntervalSeconds:     defaultRefreshIntervalSeconds,
		KeepaliveWorkerCount:       defaultKeepaliveWorkerCount,
		KeepaliveIntervalSeconds:   defaultKeepaliveIntervalSeconds,
		ReviveIntervalSeconds:      defaultReviveIntervalSeconds,
		ProbeRetryCount:            defaultProbeRetryCount,
		HealthySlotCount:           defaultHealthySlotCount,
		HealthyCandidateSlotCount:  defaultHealthyCandidateCount,
		QualityWorkerCount:         defaultQualityWorkerCount,
		QualityProbeTimeoutSeconds: defaultQualityProbeTimeout,
		QualitySoftTPS:             defaultQualitySoftTPS,
		QualityHardTPS:             defaultQualityHardTPS,
	}
}

func slotCount(settings pluginSettings) int {
	return settings.HealthySlotCount + settings.HealthyCandidateSlotCount
}

func (store *ipStore) clearAutomaticFallbackNodes() error {
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite automatic fallback cleanup: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, revive_target_status = ?
WHERE status = ? AND manual_fallback = 0`, statusConnected, statusConnected, statusHealthyFallback); err != nil {
		return fmt.Errorf("restore sqlite automatic fallback nodes: %w", err)
	}
	if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, revive_target_status = ?
WHERE id IN (
    SELECT node_id FROM ip_slots WHERE fallback_origin = 1 AND node_id > 0
)`, statusConnected, statusConnected); err != nil {
		return fmt.Errorf("restore sqlite automatic fallback slot nodes: %w", err)
	}
	if _, err := transaction.Exec(`
DELETE FROM ip_slot_auth_bindings
WHERE slot_id IN (SELECT slot_id FROM ip_slots WHERE fallback_origin = 1)`); err != nil {
		return fmt.Errorf("delete sqlite automatic fallback bindings: %w", err)
	}
	if _, err := transaction.Exec(`
UPDATE ip_slots
SET node_id = 0, claim_node_id = 0, fallback_origin = 0, fallback_entered_round_id = 0,
    claim_token = '', claim_stage = '', claim_started_at = 0, last_processed_round_id = 0, blocked_round_id = 0
WHERE fallback_origin = 1`); err != nil {
		return fmt.Errorf("clear sqlite automatic fallback slots: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite automatic fallback cleanup: %w", err)
	}
	return nil
}

func (store *ipStore) reconcileSlotLayout(_ pluginSettings, settings pluginSettings) error {
	if err := validateSlotSettings(settings); err != nil {
		return err
	}
	totalSlots := slotCount(settings)
	var existingCount int
	if err := store.database.QueryRow(`SELECT COUNT(*) FROM ip_slots`).Scan(&existingCount); err != nil {
		return fmt.Errorf("count sqlite slots: %w", err)
	}
	var matchingLayoutCount int
	if err := store.database.QueryRow(`
SELECT COUNT(*)
FROM ip_slots
WHERE (slot_id <= ? AND slot_kind = ?)
   OR (slot_id > ? AND slot_kind = ?)`, settings.HealthySlotCount, statusHealthy, settings.HealthySlotCount, statusHealthyCandidate).Scan(&matchingLayoutCount); err != nil {
		return fmt.Errorf("count sqlite matching slots: %w", err)
	}
	if existingCount != totalSlots || matchingLayoutCount != totalSlots {
		if err := store.resetSlotAssignments(); err != nil {
			return err
		}
	}
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite slot layout: %w", err)
	}
	defer transaction.Rollback()
	for slotID := 1; slotID <= totalSlots; slotID++ {
		slotKind := statusHealthyCandidate
		if slotID <= settings.HealthySlotCount {
			slotKind = statusHealthy
		}
		if _, err := transaction.Exec(`
INSERT INTO ip_slots(slot_id, slot_kind)
VALUES (?, ?)
ON CONFLICT(slot_id) DO UPDATE SET slot_kind = excluded.slot_kind`, slotID, slotKind); err != nil {
			return fmt.Errorf("save sqlite slot %d: %w", slotID, err)
		}
	}
	if _, err := transaction.Exec(`DELETE FROM ip_slots WHERE slot_id > ?`, totalSlots); err != nil {
		return fmt.Errorf("remove sqlite excess slots: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite slot layout: %w", err)
	}
	return nil
}

func (store *ipStore) resetSlotAssignments() error {
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite slot reset: %w", err)
	}
	defer transaction.Rollback()
	rows, err := transaction.Query(`SELECT slot_id, node_id, fallback_origin FROM ip_slots WHERE node_id > 0`)
	if err != nil {
		return fmt.Errorf("list sqlite assigned slots: %w", err)
	}
	type assignedSlot struct {
		slotID         int64
		nodeID         int64
		fallbackOrigin int64
	}
	assigned := make([]assignedSlot, 0)
	for rows.Next() {
		var item assignedSlot
		if err := rows.Scan(&item.slotID, &item.nodeID, &item.fallbackOrigin); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan sqlite assigned slot: %w", err)
		}
		assigned = append(assigned, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate sqlite assigned slots: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close sqlite assigned slots: %w", err)
	}
	for _, item := range assigned {
		if item.fallbackOrigin == 1 {
			if _, err := transaction.Exec(`DELETE FROM ip_nodes WHERE id = ?`, item.nodeID); err != nil {
				return fmt.Errorf("delete sqlite fallback node %d: %w", item.nodeID, err)
			}
			continue
		}
		if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = 0, probe_kind = '', probe_return_status = '', keepalive_round_id = 0,
    error_reason = '', error_detail = '', revive_target_status = ?, quality_deferred_round_id = 0
WHERE id = ?`, statusConnected, statusConnected, item.nodeID); err != nil {
			return fmt.Errorf("release sqlite slot node %d: %w", item.nodeID, err)
		}
	}
	if _, err := transaction.Exec(`DELETE FROM ip_slots`); err != nil {
		return fmt.Errorf("clear sqlite slots: %w", err)
	}
	if _, err := transaction.Exec(`DELETE FROM ip_slot_auth_bindings`); err != nil {
		return fmt.Errorf("clear sqlite slot auth bindings: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite slot reset: %w", err)
	}
	return nil
}

func validateSlotSettings(settings pluginSettings) error {
	if settings.HealthySlotCount < 1 || settings.HealthySlotCount > maxSlotCount {
		return fmt.Errorf("healthy slot count must be between 1 and %d", maxSlotCount)
	}
	if settings.HealthyCandidateSlotCount < 0 || settings.HealthyCandidateSlotCount > maxSlotCount {
		return fmt.Errorf("healthy candidate slot count must be between 0 and %d", maxSlotCount)
	}
	if slotCount(settings) > maxSlotCount {
		return fmt.Errorf("total slot count must be between 1 and %d", maxSlotCount)
	}
	if settings.QualityWorkerCount < 1 || settings.QualityWorkerCount > maxProbeWorkers {
		return fmt.Errorf("quality worker count must be between 1 and %d", maxProbeWorkers)
	}
	if settings.QualityProbeTimeoutSeconds < 1 || settings.QualityProbeTimeoutSeconds > maxQualityProbeTimeoutSeconds {
		return fmt.Errorf("quality probe timeout must be between 1 and %d seconds", maxQualityProbeTimeoutSeconds)
	}
	if settings.QualitySoftTPS <= 0 || settings.QualityHardTPS <= settings.QualitySoftTPS {
		return fmt.Errorf("quality hard TPS must be greater than quality soft TPS")
	}
	return nil
}

func newSlotClaimToken(slotID, roundID int64) string {
	return fmt.Sprintf("%d-%d-%d", roundID, slotID, time.Now().UnixNano())
}

func (store *ipStore) claimNextQualityWork(roundID int64) (*qualityWork, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin sqlite quality claim: %w", err)
	}
	defer transaction.Rollback()
	var slot slotRecord
	err = transaction.QueryRow(`
SELECT slot_id, slot_kind, node_id, claim_node_id, fallback_origin, fallback_entered_round_id, claim_token, claim_stage,
       claim_started_at, last_processed_round_id, blocked_round_id
FROM ip_slots AS slots
WHERE slots.claim_token = ''
  AND slots.last_processed_round_id <> ?
  AND (
      slots.node_id > 0
      OR (slots.node_id = 0 AND slots.blocked_round_id <> ? AND EXISTS (
          SELECT 1 FROM ip_nodes AS candidates
          WHERE candidates.status = ? AND candidates.probe_kind = '' AND candidates.quality_deferred_round_id <> ?
      ))
  )
ORDER BY slots.slot_id ASC
LIMIT 1`, roundID, roundID, statusConnected, roundID).Scan(
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
		return nil, fmt.Errorf("select sqlite quality slot: %w", err)
	}
	if slot.FallbackOrigin && slot.FallbackEnteredRoundID == roundID {
		if _, err := transaction.Exec(`UPDATE ip_slots SET last_processed_round_id = ? WHERE slot_id = ?`, roundID, slot.ID); err != nil {
			return nil, fmt.Errorf("mark sqlite fallback slot processed: %w", err)
		}
		if err := transaction.Commit(); err != nil {
			return nil, fmt.Errorf("commit sqlite fallback slot skip: %w", err)
		}
		return &qualityWork{Slot: slot, RoundID: roundID, Skip: true}, nil
	}
	var node proxyNode
	previousStatus := ""
	if slot.NodeID > 0 {
		if err := scanProxyNode(transaction.QueryRow(`
SELECT id, node_name, proxy_url, host, input_ip, port, protocol, domain, batch_id, status,
       initial_connected, probe_kind, probe_return_status, keepalive_round_id, revive_round_id,
       revive_failure_count, latency_ms, entered_at, probe_started_at, probe_time, exit_ip,
       exit_country, error_reason, error_detail, revive_target_status
FROM ip_nodes WHERE id = ?`, slot.NodeID), &node); err != nil {
			return nil, err
		}
		previousStatus = node.Status
	} else {
		if err := scanProxyNode(transaction.QueryRow(`
SELECT id, node_name, proxy_url, host, input_ip, port, protocol, domain, batch_id, status,
       initial_connected, probe_kind, probe_return_status, keepalive_round_id, revive_round_id,
       revive_failure_count, latency_ms, entered_at, probe_started_at, probe_time, exit_ip,
       exit_country, error_reason, error_detail, revive_target_status
FROM ip_nodes
WHERE status = ? AND probe_kind = '' AND quality_deferred_round_id <> ?
ORDER BY latency_ms ASC, id ASC
LIMIT 1`, statusConnected, roundID), &node); err != nil {
			if err == sql.ErrNoRows {
				if _, updateErr := transaction.Exec(`UPDATE ip_slots SET blocked_round_id = ? WHERE slot_id = ?`, roundID, slot.ID); updateErr != nil {
					return nil, fmt.Errorf("mark sqlite empty slot blocked: %w", updateErr)
				}
				if err := transaction.Commit(); err != nil {
					return nil, fmt.Errorf("commit sqlite empty slot block: %w", err)
				}
				return nil, nil
			}
			return nil, err
		}
		previousStatus = statusConnected
	}
	node.Name = displayProxyNodeName(node.ProxyURL, node.Name)
	claimToken := newSlotClaimToken(slot.ID, roundID)
	startedAt := time.Now().UnixMilli()
	nodeUpdate, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = ?, probe_kind = ?, probe_return_status = ?, keepalive_round_id = ?,
    error_reason = '', error_detail = ''
WHERE id = ? AND status = ? AND probe_kind = ''`, statusQualityProbing, startedAt, probeKindQuality, previousStatus, roundID, node.ID, previousStatus)
	if err != nil {
		return nil, fmt.Errorf("claim sqlite quality node %d: %w", node.ID, err)
	}
	nodeRowsAffected, err := nodeUpdate.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read sqlite quality node claim result: %w", err)
	}
	if nodeRowsAffected != 1 {
		return nil, fmt.Errorf("sqlite quality node %d claim was lost", node.ID)
	}
	slotUpdate, err := transaction.Exec(`
UPDATE ip_slots
SET claim_node_id = ?, claim_token = ?, claim_stage = ?, claim_started_at = ?, blocked_round_id = 0
WHERE slot_id = ? AND claim_token = ''`, node.ID, claimToken, probeKindQuality, startedAt, slot.ID)
	if err != nil {
		return nil, fmt.Errorf("claim sqlite quality slot %d: %w", slot.ID, err)
	}
	slotRowsAffected, err := slotUpdate.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read sqlite quality slot claim result: %w", err)
	}
	if slotRowsAffected != 1 {
		return nil, fmt.Errorf("sqlite quality slot %d claim was lost", slot.ID)
	}
	if err := transaction.Commit(); err != nil {
		return nil, fmt.Errorf("commit sqlite quality claim: %w", err)
	}
	node.Status = statusQualityProbing
	node.ProbeKind = probeKindQuality
	node.ProbeReturnStatus = previousStatus
	node.KeepaliveRoundID = roundID
	node.ProbeStartedAt = startedAt
	node.SlotID = slot.ID
	node.FallbackOrigin = slot.FallbackOrigin
	return &qualityWork{Slot: slot, Node: node, PreviousStatus: previousStatus, NewCandidate: slot.NodeID == 0, FallbackOrigin: slot.FallbackOrigin, ClaimToken: claimToken, RoundID: roundID}, nil
}

func scanProxyNode(row *sql.Row, node *proxyNode) error {
	return row.Scan(
		&node.ID, &node.Name, &node.ProxyURL, &node.Host, &node.InputIP, &node.Port, &node.Protocol,
		&node.Domain, &node.BatchID, &node.Status, &node.InitialConnected, &node.ProbeKind,
		&node.ProbeReturnStatus, &node.KeepaliveRoundID, &node.ReviveRoundID, &node.ReviveFailureCount,
		&node.LatencyMs, &node.EnteredAt, &node.ProbeStartedAt, &node.ProbeTime, &node.ExitIP,
		&node.ExitCountry, &node.ErrorReason, &node.ErrorDetail, &node.ReviveTargetStatus,
	)
}

func (store *ipStore) resetQualityWork(work qualityWork) error {
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite quality reset: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = 0, probe_kind = '', probe_return_status = '', keepalive_round_id = 0
WHERE id = ? AND status = ? AND probe_kind = ?`, work.PreviousStatus, work.Node.ID, statusQualityProbing, probeKindQuality); err != nil {
		return fmt.Errorf("reset sqlite quality node %d: %w", work.Node.ID, err)
	}
	if _, err := transaction.Exec(`
UPDATE ip_slots
SET node_id = ?, claim_node_id = 0, fallback_origin = ?, fallback_entered_round_id = ?, claim_token = '', claim_stage = '',
    claim_started_at = ?, last_processed_round_id = ?, blocked_round_id = ?
WHERE slot_id = ? AND claim_token = ?`, work.Slot.NodeID, boolInteger(work.Slot.FallbackOrigin), work.Slot.FallbackEnteredRoundID, work.Slot.ClaimStartedAt, work.Slot.LastProcessedRoundID, work.Slot.BlockedRoundID, work.Slot.ID, work.ClaimToken); err != nil {
		return fmt.Errorf("reset sqlite quality slot %d: %w", work.Slot.ID, err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite quality reset: %w", err)
	}
	return nil
}

func (store *ipStore) completeQualityWork(work qualityWork, result qualityProbeResult) error {
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite quality completion: %w", err)
	}
	defer transaction.Rollback()
	var currentSlot slotRecord
	if err := transaction.QueryRow(`
SELECT slot_id, slot_kind, node_id, claim_node_id, fallback_origin, fallback_entered_round_id, claim_token, claim_stage,
       claim_started_at, last_processed_round_id, blocked_round_id
FROM ip_slots WHERE slot_id = ?`, work.Slot.ID).Scan(
		&currentSlot.ID, &currentSlot.Kind, &currentSlot.NodeID, &currentSlot.ClaimNodeID, &currentSlot.FallbackOrigin,
		&currentSlot.FallbackEnteredRoundID, &currentSlot.ClaimToken, &currentSlot.ClaimStage,
		&currentSlot.ClaimStartedAt, &currentSlot.LastProcessedRoundID, &currentSlot.BlockedRoundID,
	); err != nil {
		return fmt.Errorf("read sqlite quality slot %d: %w", work.Slot.ID, err)
	}
	if currentSlot.ClaimToken != work.ClaimToken {
		return fmt.Errorf("sqlite quality claim %s is no longer active", work.ClaimToken)
	}
	clearClaim := `claim_node_id = 0, claim_token = '', claim_stage = '', claim_started_at = 0`
	markProcessed := clearClaim + `, last_processed_round_id = ?`
	deferredRoundID := int64(0)
	if work.NewCandidate && !work.FallbackOrigin {
		deferredRoundID = work.RoundID
	}
	if result.Unavailable {
		if work.FallbackOrigin {
			if _, err := transaction.Exec(`DELETE FROM ip_nodes WHERE id = ? AND status = ? AND probe_kind = ?`, work.Node.ID, statusQualityProbing, probeKindQuality); err != nil {
				return fmt.Errorf("delete unavailable fallback node %d: %w", work.Node.ID, err)
			}
			if _, err := transaction.Exec(`UPDATE ip_slots SET `+clearClaim+` WHERE slot_id = ? AND claim_token = ?`, work.Slot.ID, work.ClaimToken); err != nil {
				return fmt.Errorf("clear unavailable fallback slot %d: %w", work.Slot.ID, err)
			}
		} else {
			if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = 0, probe_kind = '', probe_return_status = '', keepalive_round_id = 0,
    quality_deferred_round_id = ?
WHERE id = ? AND status = ? AND probe_kind = ?`, work.PreviousStatus, deferredRoundID, work.Node.ID, statusQualityProbing, probeKindQuality); err != nil {
				return fmt.Errorf("restore sqlite unavailable quality node %d: %w", work.Node.ID, err)
			}
			if _, err := transaction.Exec(`UPDATE ip_slots SET `+clearClaim+`, last_processed_round_id = ?, blocked_round_id = ? WHERE slot_id = ? AND claim_token = ?`, work.RoundID, work.RoundID, work.Slot.ID, work.ClaimToken); err != nil {
				return fmt.Errorf("restore sqlite unavailable quality slot %d: %w", work.Slot.ID, err)
			}
		}
	} else if result.Classification == qualityClassificationNormal {
		if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, initial_connected = 1, probe_started_at = 0, probe_kind = '', probe_return_status = '',
    keepalive_round_id = 0, quality_deferred_round_id = 0, probe_time = ?, latency_ms = ?, error_reason = '', error_detail = '',
    revive_target_status = ?
WHERE id = ? AND status = ? AND probe_kind = ?`, currentSlot.Kind, time.Now().UnixMilli(), work.Node.LatencyMs, statusConnected, work.Node.ID, statusQualityProbing, probeKindQuality); err != nil {
			return fmt.Errorf("save sqlite healthy quality node %d: %w", work.Node.ID, err)
		}
		fallbackEnteredRoundID := int64(0)
		if work.FallbackOrigin {
			fallbackEnteredRoundID = work.RoundID
		}
		if _, err := transaction.Exec(`
UPDATE ip_slots
SET node_id = ?, fallback_origin = ?, fallback_entered_round_id = ?, `+markProcessed+`
WHERE slot_id = ? AND claim_token = ?`, work.Node.ID, boolInteger(work.FallbackOrigin), fallbackEnteredRoundID, work.RoundID, work.Slot.ID, work.ClaimToken); err != nil {
			return fmt.Errorf("save sqlite healthy quality slot %d: %w", work.Slot.ID, err)
		}
	} else {
		errorReason := result.DisplayReason()
		if work.FallbackOrigin {
			if _, err := transaction.Exec(`DELETE FROM ip_nodes WHERE id = ? AND status = ? AND probe_kind = ?`, work.Node.ID, statusQualityProbing, probeKindQuality); err != nil {
				return fmt.Errorf("delete sqlite failed fallback node %d: %w", work.Node.ID, err)
			}
		} else if _, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = 0, probe_kind = '', probe_return_status = '', keepalive_round_id = 0,
    quality_deferred_round_id = 0, probe_time = ?, latency_ms = ?, error_reason = ?, error_detail = ?, revive_target_status = ?
WHERE id = ? AND status = ? AND probe_kind = ?`, statusCooldown, time.Now().UnixMilli(), work.Node.LatencyMs, errorReason, result.Detail, statusCooldown, work.Node.ID, statusQualityProbing, probeKindQuality); err != nil {
			return fmt.Errorf("save sqlite cooldown quality node %d: %w", work.Node.ID, err)
		}
		if _, err := transaction.Exec(`
UPDATE ip_slots
SET node_id = 0, fallback_origin = 0, fallback_entered_round_id = 0, `+clearClaim+`
WHERE slot_id = ? AND claim_token = ?`, work.Slot.ID, work.ClaimToken); err != nil {
			return fmt.Errorf("clear sqlite failed quality slot %d: %w", work.Slot.ID, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite quality completion: %w", err)
	}
	return nil
}

func boolInteger(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (store *ipStore) recordQualityAttempt(roundID int64, slotID int64, nodeID int64, auth authFile, selectionSource string, result qualityProbeResult) error {
	_, err := store.database.Exec(`
INSERT INTO quality_probe_attempts(
    round_id, slot_id, node_id, auth_name, auth_index, auth_identity, selection_source, proxy_url,
    started_at, finished_at, status_code, classification, quality_level, classification_reason,
    ttfb_ms, first_token_ms, generation_ms, total_ms, output_tokens, reasoning_tokens, output_tokens_per_second,
    error_code, error_detail
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		roundID, slotID, nodeID, auth.Name, auth.Index, auth.Identity(), selectionSource, result.ProxyURL,
		result.StartedAt, result.FinishedAt, result.StatusCode, result.Classification, result.QualityLevel,
		result.ClassificationReason, result.TTFBMs, result.FirstTokenMs, result.GenerationMs, result.TotalMs,
		result.OutputTokens, result.ReasoningTokens, result.OutputTokensPerSecond, result.ErrorCode, result.Detail)
	if err != nil {
		return fmt.Errorf("insert sqlite quality attempt: %w", err)
	}
	return nil
}

func (store *ipStore) recordAuthSuccess(nodeID, slotID, roundID int64, auth authFile, selectionSource string) error {
	_, err := store.database.Exec(`
INSERT INTO auth_selection_history(selected_at, auth_name, auth_index, auth_identity, selection_source, node_id, slot_id, round_id, was_success)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`, time.Now().UnixMilli(), auth.Name, auth.Index, auth.Identity(), selectionSource, nodeID, slotID, roundID)
	if err != nil {
		return fmt.Errorf("insert sqlite auth success history: %w", err)
	}
	return nil
}

func (store *ipStore) recordRandomAuthSelection(roundID, nodeID, slotID int64, candidates []authFile) (authFile, error) {
	if len(candidates) == 0 {
		return authFile{}, fmt.Errorf("没有可随机选择的 xAI auth")
	}
	transaction, err := store.database.Begin()
	if err != nil {
		return authFile{}, fmt.Errorf("begin sqlite random auth selection: %w", err)
	}
	defer transaction.Rollback()
	rows, err := transaction.Query(`
SELECT auth_identity
FROM auth_selection_history
WHERE selection_source = 'random' AND was_success = 0
ORDER BY selected_at DESC, id DESC
LIMIT 10`)
	if err != nil {
		return authFile{}, fmt.Errorf("read recent sqlite random selection history: %w", err)
	}
	recent := make(map[string]struct{}, 10)
	for rows.Next() {
		var identity string
		if err := rows.Scan(&identity); err != nil {
			_ = rows.Close()
			return authFile{}, fmt.Errorf("scan recent sqlite random auth history: %w", err)
		}
		recent[identity] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return authFile{}, fmt.Errorf("close recent sqlite random auth history: %w", err)
	}
	available := make([]authFile, 0, len(candidates))
	for _, candidate := range candidates {
		if _, exists := recent[candidate.Identity()]; !exists {
			available = append(available, candidate)
		}
	}
	if len(available) == 0 {
		return authFile{}, fmt.Errorf("最近 10 次随机 auth 已覆盖全部可用账号")
	}
	selected := available[int(time.Now().UnixNano()%int64(len(available)))]
	if _, err := transaction.Exec(`
INSERT INTO auth_selection_history(selected_at, auth_name, auth_index, auth_identity, selection_source, node_id, slot_id, round_id, was_success)
VALUES (?, ?, ?, ?, 'random', ?, ?, ?, 0)`, time.Now().UnixMilli(), selected.Name, selected.Index, selected.Identity(), nodeID, slotID, roundID); err != nil {
		return authFile{}, fmt.Errorf("save sqlite random auth history: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return authFile{}, fmt.Errorf("commit sqlite random auth selection: %w", err)
	}
	return selected, nil
}

func (store *ipStore) findSlotForNode(nodeID int64) (slotRecord, bool, error) {
	var slot slotRecord
	err := store.database.QueryRow(`
SELECT slot_id, slot_kind, node_id, claim_node_id, fallback_origin, fallback_entered_round_id, claim_token, claim_stage,
       claim_started_at, last_processed_round_id, blocked_round_id
FROM ip_slots WHERE node_id = ?`, nodeID).Scan(
		&slot.ID, &slot.Kind, &slot.NodeID, &slot.ClaimNodeID, &slot.FallbackOrigin, &slot.FallbackEnteredRoundID,
		&slot.ClaimToken, &slot.ClaimStage, &slot.ClaimStartedAt, &slot.LastProcessedRoundID, &slot.BlockedRoundID,
	)
	if err == sql.ErrNoRows {
		return slotRecord{}, false, nil
	}
	if err != nil {
		return slotRecord{}, false, fmt.Errorf("find sqlite slot for node %d: %w", nodeID, err)
	}
	return slot, true, nil
}

func (store *ipStore) blockSlotForRound(slotID, roundID int64) error {
	_, err := store.database.Exec(`UPDATE ip_slots SET blocked_round_id = ?, last_processed_round_id = ? WHERE slot_id = ? AND node_id = 0`, roundID, roundID, slotID)
	if err != nil {
		return fmt.Errorf("block sqlite slot %d: %w", slotID, err)
	}
	return nil
}

func (store *ipStore) listHealthyAuthBindings(nodeID int64) ([]authBinding, error) {
	rows, err := store.database.Query(`
SELECT bindings.slot_id, bindings.node_id, bindings.auth_name, bindings.auth_index, bindings.auth_identity,
       bindings.proxy_url, bindings.sync_status, bindings.sync_error, bindings.verified_at, bindings.updated_at
FROM ip_slot_auth_bindings AS bindings
JOIN ip_slots AS slots ON slots.slot_id = bindings.slot_id
WHERE bindings.node_id = ? AND slots.slot_kind = ?
ORDER BY bindings.auth_name ASC`, nodeID, statusHealthy)
	if err != nil {
		return nil, fmt.Errorf("list sqlite auth bindings: %w", err)
	}
	defer rows.Close()
	items := make([]authBinding, 0)
	for rows.Next() {
		var item authBinding
		if err := rows.Scan(&item.SlotID, &item.NodeID, &item.AuthName, &item.AuthIndex, &item.AuthIdentity, &item.ProxyURL, &item.SyncStatus, &item.SyncError, &item.VerifiedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan sqlite auth binding: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite auth bindings: %w", err)
	}
	return items, nil
}

func (store *ipStore) replaceAuthBindings(bindings []authBinding) error {
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite auth binding replacement: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec(`DELETE FROM ip_slot_auth_bindings`); err != nil {
		return fmt.Errorf("clear sqlite auth bindings: %w", err)
	}
	for _, binding := range bindings {
		if _, err := transaction.Exec(`
INSERT INTO ip_slot_auth_bindings(auth_name, auth_index, auth_identity, slot_id, node_id, proxy_url, sync_status, sync_error, verified_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, binding.AuthName, binding.AuthIndex, binding.AuthIdentity, binding.SlotID, binding.NodeID, binding.ProxyURL, binding.SyncStatus, binding.SyncError, binding.VerifiedAt, binding.UpdatedAt); err != nil {
			return fmt.Errorf("insert sqlite auth binding %s: %w", binding.AuthName, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite auth binding replacement: %w", err)
	}
	return nil
}

func (store *ipStore) upsertAuthBinding(binding authBinding) error {
	_, err := store.database.Exec(`
INSERT INTO ip_slot_auth_bindings(auth_name, auth_index, auth_identity, slot_id, node_id, proxy_url, sync_status, sync_error, verified_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(auth_name) DO UPDATE SET
    auth_index = excluded.auth_index,
    auth_identity = excluded.auth_identity,
    slot_id = excluded.slot_id,
    node_id = excluded.node_id,
    proxy_url = excluded.proxy_url,
    sync_status = excluded.sync_status,
    sync_error = excluded.sync_error,
    verified_at = excluded.verified_at,
    updated_at = excluded.updated_at`, binding.AuthName, binding.AuthIndex, binding.AuthIdentity, binding.SlotID, binding.NodeID, binding.ProxyURL, binding.SyncStatus, binding.SyncError, binding.VerifiedAt, binding.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert sqlite auth binding %s: %w", binding.AuthName, err)
	}
	return nil
}

func (store *ipStore) updateKeepaliveQualityPhase(roundID int64, stage string, candidateCount, successCount, failureCount int64) error {
	var query string
	switch strings.TrimSpace(stage) {
	case "connectivity_completed":
		query = `UPDATE keepalive_rounds SET connectivity_completed_at = ?, candidate_count = ?, success_count = ?, failure_count = ? WHERE round_id = ?`
	case "quality_started":
		query = `UPDATE keepalive_rounds SET quality_started_at = ? WHERE round_id = ?`
	case "quality_completed":
		query = `UPDATE keepalive_rounds SET quality_completed_at = ?, quality_candidate_count = ?, quality_success_count = ?, quality_failure_count = ? WHERE round_id = ?`
	default:
		return fmt.Errorf("unknown keepalive phase %s", stage)
	}
	var args []any
	switch stage {
	case "connectivity_completed":
		args = []any{time.Now().UnixMilli(), candidateCount, successCount, failureCount, roundID}
	case "quality_started":
		args = []any{time.Now().UnixMilli(), roundID}
	default:
		args = []any{time.Now().UnixMilli(), candidateCount, successCount, failureCount, roundID}
	}
	if _, err := store.database.Exec(query, args...); err != nil {
		return fmt.Errorf("update sqlite keepalive phase %s: %w", stage, err)
	}
	return nil
}

func (store *ipStore) setQualityRoundCompleted(roundID int64, candidateCount, successCount, failureCount int64) error {
	return store.updateKeepaliveQualityPhase(roundID, "quality_completed", candidateCount, successCount, failureCount)
}

func (store *ipStore) findSlotByID(slotID int64) (slotRecord, bool, error) {
	var slot slotRecord
	err := store.database.QueryRow(`
SELECT slot_id, slot_kind, node_id, claim_node_id, fallback_origin, fallback_entered_round_id, claim_token, claim_stage,
       claim_started_at, last_processed_round_id, blocked_round_id
FROM ip_slots WHERE slot_id = ?`, slotID).Scan(
		&slot.ID, &slot.Kind, &slot.NodeID, &slot.ClaimNodeID, &slot.FallbackOrigin, &slot.FallbackEnteredRoundID,
		&slot.ClaimToken, &slot.ClaimStage, &slot.ClaimStartedAt, &slot.LastProcessedRoundID, &slot.BlockedRoundID,
	)
	if err == sql.ErrNoRows {
		return slotRecord{}, false, nil
	}
	if err != nil {
		return slotRecord{}, false, fmt.Errorf("find sqlite slot %d: %w", slotID, err)
	}
	return slot, true, nil
}
