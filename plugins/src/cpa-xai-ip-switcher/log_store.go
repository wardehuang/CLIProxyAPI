package main

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func (store *ipStore) ensureBatchMetadata() error {
	rows, err := store.database.Query(`PRAGMA table_info(ip_batches)`)
	if err != nil {
		return fmt.Errorf("inspect sqlite batch metadata: %w", err)
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var (
			columnID       int
			columnName     string
			columnType     string
			notNull        int
			defaultValue   any
			primaryKeyFlag int
		)
		if err := rows.Scan(&columnID, &columnName, &columnType, &notNull, &defaultValue, &primaryKeyFlag); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan sqlite batch metadata: %w", err)
		}
		columns[columnName] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate sqlite batch metadata: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close sqlite batch metadata: %w", err)
	}
	if !columns["sequence_number"] {
		if _, err := store.database.Exec("ALTER TABLE ip_batches ADD COLUMN sequence_number INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("add sqlite batch sequence: %w", err)
		}
	}
	if !columns["delete_non_us"] {
		if _, err := store.database.Exec("ALTER TABLE ip_batches ADD COLUMN delete_non_us INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("add sqlite batch delete option: %w", err)
		}
	}
	initialProbeCountersAdded := false
	if !columns["initial_probe_completed_count"] {
		if _, err := store.database.Exec("ALTER TABLE ip_batches ADD COLUMN initial_probe_completed_count INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("add sqlite initial probe completed counter: %w", err)
		}
		initialProbeCountersAdded = true
	}
	if !columns["initial_connected_count"] {
		if _, err := store.database.Exec("ALTER TABLE ip_batches ADD COLUMN initial_connected_count INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("add sqlite initial connected counter: %w", err)
		}
		initialProbeCountersAdded = true
	}
	if initialProbeCountersAdded {
		if _, err := store.database.Exec(`
UPDATE ip_batches
SET initial_probe_completed_count = COALESCE((
        SELECT COUNT(DISTINCT logs.node_id)
        FROM plugin_logs AS logs
        WHERE logs.category = 'batch_probe'
          AND logs.group_id = ip_batches.batch_id
          AND logs.node_id > 0
          AND logs.event IN ('probe.completed', 'probe.failed', 'probe.deleted_non_us')
    ), 0),
    initial_connected_count = COALESCE((
        SELECT COUNT(DISTINCT logs.node_id)
        FROM plugin_logs AS logs
        WHERE logs.category = 'batch_probe'
          AND logs.group_id = ip_batches.batch_id
          AND logs.node_id > 0
          AND logs.event = 'probe.completed'
          AND logs.log_status = 'connected'
    ), 0)`); err != nil {
			return fmt.Errorf("backfill sqlite initial batch counters: %w", err)
		}
	}

	rows, err = store.database.Query(`
SELECT batch_id
FROM ip_batches
WHERE sequence_number = 0
ORDER BY created_at ASC, batch_id ASC`)
	if err != nil {
		return fmt.Errorf("list unnumbered sqlite batches: %w", err)
	}
	batchIDs := make([]string, 0)
	for rows.Next() {
		var batchID string
		if err := rows.Scan(&batchID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan unnumbered sqlite batch: %w", err)
		}
		batchIDs = append(batchIDs, batchID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate unnumbered sqlite batches: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close unnumbered sqlite batches: %w", err)
	}
	if len(batchIDs) == 0 {
		return nil
	}

	var nextSequenceNumber int64
	if err := store.database.QueryRow(`SELECT COALESCE(MAX(sequence_number), 0) + 1 FROM ip_batches`).Scan(&nextSequenceNumber); err != nil {
		return fmt.Errorf("read next sqlite batch sequence: %w", err)
	}
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite batch sequence migration: %w", err)
	}
	for _, batchID := range batchIDs {
		if _, err := transaction.Exec(`UPDATE ip_batches SET sequence_number = ? WHERE batch_id = ?`, nextSequenceNumber, batchID); err != nil {
			_ = transaction.Rollback()
			return fmt.Errorf("number sqlite batch %s: %w", batchID, err)
		}
		nextSequenceNumber++
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite batch sequence migration: %w", err)
	}
	return nil
}

func (store *ipStore) ensureLogColumns() error {
	rows, err := store.database.Query(`PRAGMA table_info(plugin_logs)`)
	if err != nil {
		return fmt.Errorf("inspect sqlite log columns: %w", err)
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var (
			columnID       int
			columnName     string
			columnType     string
			notNull        int
			defaultValue   any
			primaryKeyFlag int
		)
		if err := rows.Scan(&columnID, &columnName, &columnType, &notNull, &defaultValue, &primaryKeyFlag); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan sqlite log columns: %w", err)
		}
		columns[columnName] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate sqlite log columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close sqlite log columns: %w", err)
	}
	for columnName, columnDefinition := range map[string]string{
		"category":   "TEXT NOT NULL DEFAULT 'general'",
		"group_id":   "TEXT NOT NULL DEFAULT ''",
		"log_status": "TEXT NOT NULL DEFAULT ''",
	} {
		if columns[columnName] {
			continue
		}
		if _, err := store.database.Exec("ALTER TABLE plugin_logs ADD COLUMN " + columnName + " " + columnDefinition); err != nil {
			return fmt.Errorf("add sqlite log column %s: %w", columnName, err)
		}
	}
	if _, err := store.database.Exec(`CREATE INDEX IF NOT EXISTS idx_plugin_logs_category_group ON plugin_logs(category, group_id, id DESC)`); err != nil {
		return fmt.Errorf("create sqlite grouped log index: %w", err)
	}
	return nil
}

func (store *ipStore) appendLog(level, event string, nodeID int64, nodeName, message, detail string) error {
	return store.appendLogWithMetadata(logCategoryGeneral, "", "", level, event, nodeID, nodeName, message, detail)
}

func (store *ipStore) appendProbeLog(category, groupID, logStatus, level, event string, nodeID int64, nodeName, message, detail string) error {
	return store.appendLogWithMetadata(category, groupID, logStatus, level, event, nodeID, nodeName, message, detail)
}

func (store *ipStore) appendLogWithMetadata(category, groupID, logStatus, level, event string, nodeID int64, nodeName, message, detail string) (err error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite log insert: %w", err)
	}
	defer func() {
		if err != nil {
			_ = transaction.Rollback()
		}
	}()

	_, err = transaction.Exec(`
INSERT INTO plugin_logs(created_at, level, event, category, group_id, log_status, node_id, node_name, message, detail)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, time.Now().UnixMilli(), level, event, category, groupID, logStatus, nodeID, nodeName, message, detail)
	if err != nil {
		return fmt.Errorf("insert sqlite log: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite log: %w", err)
	}
	return nil
}

func (store *ipStore) pruneLogs(transaction *sql.Tx, category string) error {
	switch category {
	case logCategoryGeneral:
		if _, err := transaction.Exec(`
DELETE FROM plugin_logs
WHERE category = ?
  AND id NOT IN (SELECT id FROM plugin_logs WHERE category = ? ORDER BY id DESC LIMIT ?)`, category, category, maxPluginLogs); err != nil {
			return fmt.Errorf("prune general sqlite logs: %w", err)
		}
	case logCategoryBatchProbe:
		if _, err := transaction.Exec(`
DELETE FROM plugin_logs
WHERE category = ?
  AND group_id NOT IN (
      SELECT batch_id FROM ip_batches ORDER BY sequence_number DESC, created_at DESC LIMIT ?
  )`, category, maxGroupedLogSets); err != nil {
			return fmt.Errorf("prune batch sqlite logs: %w", err)
		}
	case logCategoryKeepaliveProbe, logCategoryQualityProbe:
		if _, err := transaction.Exec(`
DELETE FROM plugin_logs
WHERE category = ?
  AND group_id NOT IN (
      SELECT CAST(round_id AS TEXT) FROM keepalive_rounds ORDER BY sequence_number DESC, started_at DESC LIMIT ?
  )`, category, maxGroupedLogSets); err != nil {
			return fmt.Errorf("prune keepalive sqlite logs: %w", err)
		}
	case logCategoryReviveProbe:
		if _, err := transaction.Exec(`
DELETE FROM plugin_logs
WHERE category = ?
  AND group_id NOT IN (
      SELECT CAST(round_id AS TEXT) FROM revive_rounds ORDER BY sequence_number DESC, started_at DESC LIMIT ?
  )`, category, maxGroupedLogSets); err != nil {
			return fmt.Errorf("prune revive sqlite logs: %w", err)
		}
	}
	return nil
}

func (store *ipStore) pruneStoredLogs() error {
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite log pruning: %w", err)
	}
	defer transaction.Rollback()

	for _, category := range []string{logCategoryGeneral, logCategoryBatchProbe, logCategoryKeepaliveProbe, logCategoryQualityProbe, logCategoryReviveProbe} {
		if err := store.pruneLogs(transaction, category); err != nil {
			return err
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit sqlite log pruning: %w", err)
	}
	return nil
}

func (store *ipStore) listLogs(search string) ([]pluginLog, error) {
	return store.listLogsByFilter(logCategoryGeneral, "", "", search)
}

func (store *ipStore) listGroupedLogs(category, groupID, logStatus, search string) ([]pluginLog, error) {
	return store.listLogsByFilter(category, strings.TrimSpace(groupID), strings.TrimSpace(logStatus), search)
}

func (store *ipStore) listLogsByFilter(category, groupID, logStatus, search string) ([]pluginLog, error) {
	query := `
SELECT id, created_at, level, event, category, group_id, log_status, node_id, node_name, message, detail
FROM plugin_logs
WHERE category = ?`
	arguments := []any{category}
	if category != logCategoryGeneral {
		query += `
 AND event NOT IN ('probe.started', 'keepalive.started', 'revive.started')`
	}
	if groupID != "" {
		query += ` AND group_id = ?`
		arguments = append(arguments, groupID)
	}
	if logStatus != "" {
		query += ` AND log_status = ?`
		arguments = append(arguments, logStatus)
	}
	search = strings.TrimSpace(search)
	if search != "" {
		escapedSearch := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search)
		searchPattern := "%" + escapedSearch + "%"
		query += `
 AND (
       level LIKE ? ESCAPE '\'
    OR event LIKE ? ESCAPE '\'
    OR CAST(id AS TEXT) LIKE ? ESCAPE '\'
    OR CAST(node_id AS TEXT) LIKE ? ESCAPE '\'
    OR node_name LIKE ? ESCAPE '\'
    OR message LIKE ? ESCAPE '\'
    OR detail LIKE ? ESCAPE '\'
 )`
		for range 7 {
			arguments = append(arguments, searchPattern)
		}
	}
	query += ` ORDER BY id DESC`
	if category == logCategoryGeneral {
		query += ` LIMIT ?`
		arguments = append(arguments, maxPluginLogs)
	}

	rows, err := store.database.Query(query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list sqlite logs: %w", err)
	}
	defer rows.Close()

	items := make([]pluginLog, 0)
	for rows.Next() {
		var item pluginLog
		if err := rows.Scan(
			&item.ID,
			&item.CreatedAt,
			&item.Level,
			&item.Event,
			&item.Category,
			&item.GroupID,
			&item.LogStatus,
			&item.NodeID,
			&item.NodeName,
			&item.Message,
			&item.Detail,
		); err != nil {
			return nil, fmt.Errorf("scan sqlite log: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite logs: %w", err)
	}
	return items, nil
}

func (store *ipStore) listLogGroups(category string) ([]logGroup, error) {
	category = strings.TrimSpace(category)
	switch category {
	case logCategoryBatchProbe:
		return store.listBatchLogGroups()
	case logCategoryKeepaliveProbe:
		return store.listKeepaliveLogGroups(logCategoryKeepaliveProbe)
	case logCategoryQualityProbe:
		return store.listKeepaliveLogGroups(logCategoryQualityProbe)
	case logCategoryReviveProbe:
		return store.listReviveLogGroups()
	default:
		return nil, fmt.Errorf("unsupported log category %s", category)
	}
}

func (store *ipStore) listBatchLogGroups() ([]logGroup, error) {
	rows, err := store.database.Query(`
WITH batch_state AS (
    SELECT
        batches.batch_id,
        batches.sequence_number,
        batches.created_at,
        EXISTS (
            SELECT 1
            FROM ip_nodes AS pending
            WHERE pending.batch_id = batches.batch_id
              AND (
                    pending.status IN (?, ?)
                 OR pending.probe_kind = ?
              )
        ) AS has_pending
    FROM ip_batches AS batches
)
SELECT
    batch_id,
    sequence_number,
    created_at,
    CASE
        WHEN has_pending = 1 THEN 0
        ELSE COALESCE((
            SELECT MAX(NULLIF(completed.probe_time, 0))
            FROM ip_nodes AS completed
            WHERE completed.batch_id = batch_state.batch_id
        ), created_at)
    END,
    CASE WHEN has_pending = 1 THEN ? ELSE ? END,
    (
        SELECT COUNT(*)
        FROM plugin_logs
        WHERE plugin_logs.category = ?
          AND plugin_logs.group_id = batch_state.batch_id
          AND plugin_logs.event NOT IN ('probe.started', 'keepalive.started', 'revive.started')
    )
FROM batch_state
ORDER BY sequence_number DESC, created_at DESC
LIMIT ?`, statusUnprobed, statusProbing, probeKindInitial, groupStatusRunning, groupStatusCompleted, logCategoryBatchProbe, maxGroupedLogSets)
	if err != nil {
		return nil, fmt.Errorf("list sqlite batch log groups: %w", err)
	}
	defer rows.Close()

	items := make([]logGroup, 0)
	for rows.Next() {
		var item logGroup
		if err := rows.Scan(&item.ID, &item.SequenceNumber, &item.StartedAt, &item.CompletedAt, &item.Status, &item.LogCount); err != nil {
			return nil, fmt.Errorf("scan sqlite batch log group: %w", err)
		}
		item.Category = logCategoryBatchProbe
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite batch log groups: %w", err)
	}
	return items, nil
}

func (store *ipStore) listKeepaliveLogGroups(category string) ([]logGroup, error) {
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
    ),
    candidate_count,
    success_count,
    failure_count,
    connectivity_completed_at,
    quality_started_at,
    quality_completed_at,
    quality_candidate_count,
    quality_success_count,
    quality_failure_count
FROM keepalive_rounds AS rounds
ORDER BY sequence_number DESC, started_at DESC
LIMIT ?`, category, maxGroupedLogSets)
	if err != nil {
		return nil, fmt.Errorf("list sqlite keepalive log groups: %w", err)
	}
	defer rows.Close()

	items := make([]logGroup, 0)
	for rows.Next() {
		var item logGroup
		if err := rows.Scan(
			&item.ID,
			&item.SequenceNumber,
			&item.StartedAt,
			&item.CompletedAt,
			&item.Status,
			&item.LogCount,
			&item.CandidateCount,
			&item.SuccessCount,
			&item.FailureCount,
			&item.ConnectivityCompletedAt,
			&item.QualityStartedAt,
			&item.QualityCompletedAt,
			&item.QualityCandidateCount,
			&item.QualitySuccessCount,
			&item.QualityFailureCount,
		); err != nil {
			return nil, fmt.Errorf("scan sqlite keepalive log group: %w", err)
		}
		item.Category = category
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite keepalive log groups: %w", err)
	}
	return items, nil
}

func (store *ipStore) startKeepaliveRound(roundID int64) (int64, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin sqlite keepalive round: %w", err)
	}
	defer transaction.Rollback()

	var sequenceNumber int64
	if err := transaction.QueryRow(`SELECT COALESCE(MAX(sequence_number), 0) + 1 FROM keepalive_rounds`).Scan(&sequenceNumber); err != nil {
		return 0, fmt.Errorf("read sqlite keepalive round sequence: %w", err)
	}
	if _, err := transaction.Exec(`
INSERT INTO keepalive_rounds(round_id, sequence_number, started_at, status)
VALUES (?, ?, ?, ?)`, roundID, sequenceNumber, time.Now().UnixMilli(), groupStatusRunning); err != nil {
		return 0, fmt.Errorf("insert sqlite keepalive round: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return 0, fmt.Errorf("commit sqlite keepalive round: %w", err)
	}
	return sequenceNumber, nil
}

func (store *ipStore) finishKeepaliveRound(roundID int64, status string, candidateCount, successCount, failureCount int64) error {
	_, err := store.database.Exec(`
UPDATE keepalive_rounds
SET completed_at = ?, status = ?, candidate_count = ?, success_count = ?, failure_count = ?
WHERE round_id = ?`, time.Now().UnixMilli(), status, candidateCount, successCount, failureCount, roundID)
	if err != nil {
		return fmt.Errorf("finish sqlite keepalive round: %w", err)
	}
	return nil
}

func keepaliveGroupID(roundID int64) string {
	return strconv.FormatInt(roundID, 10)
}
