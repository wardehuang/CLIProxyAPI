package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func (store *guardianStore) ensureNodeScopeUniqueIndex() error {
	rows, err := store.database.Query(`PRAGMA index_list(nodes)`)
	if err != nil {
		return fmt.Errorf("inspect node indexes: %w", err)
	}
	defer rows.Close()

	type nodeIndex struct {
		name   string
		unique bool
	}
	indexes := make([]nodeIndex, 0)
	for rows.Next() {
		var sequence, isUnique, partial int
		var origin, indexName string
		if err := rows.Scan(&sequence, &indexName, &isUnique, &origin, &partial); err != nil {
			return fmt.Errorf("scan node index: %w", err)
		}
		indexes = append(indexes, nodeIndex{name: indexName, unique: isUnique != 0})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate node indexes: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close node indexes: %w", err)
	}

	legacyUniqueAddress := false
	hasScopedUniqueIndex := false
	for _, index := range indexes {
		if !index.unique {
			continue
		}
		columns, err := nodeIndexColumns(store, index.name)
		if err != nil {
			return err
		}
		if len(columns) == 1 && columns[0] == "address" {
			legacyUniqueAddress = true
		}
		if len(columns) == 2 && ((columns[0] == "scope" && columns[1] == "address") || (columns[0] == "address" && columns[1] == "scope")) {
			hasScopedUniqueIndex = true
		}
	}

	if legacyUniqueAddress {
		return store.rebuildNodesWithScopedAddressIndex()
	}
	if hasScopedUniqueIndex {
		return nil
	}
	if _, err := store.database.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_scope_address ON nodes(scope, address)`); err != nil {
		return fmt.Errorf("create scoped node index: %w", err)
	}
	return nil
}

func (store *guardianStore) ensureKeepaliveRoundNodeColumns() error {
	rows, err := store.database.Query(`PRAGMA table_info(keepalive_round_nodes)`)
	if err != nil {
		return fmt.Errorf("inspect keepalive round node schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var columnID, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&columnID, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan keepalive round node schema: %w", err)
		}
		if columnName == "completed_at" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate keepalive round node schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close keepalive round node schema: %w", err)
	}
	if _, err := store.database.Exec(`ALTER TABLE keepalive_round_nodes ADD COLUMN completed_at INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add keepalive round node column: %w", err)
	}
	return nil
}

func nodeIndexColumns(store *guardianStore, indexName string) ([]string, error) {
	quotedName := strings.ReplaceAll(indexName, `"`, `""`)
	rows, err := store.database.Query(`PRAGMA index_info("` + quotedName + `")`)
	if err != nil {
		return nil, fmt.Errorf("inspect node index columns: %w", err)
	}
	defer rows.Close()
	columns := make([]string, 0, 2)
	for rows.Next() {
		var sequence, columnID int
		var columnName sql.NullString
		if err := rows.Scan(&sequence, &columnID, &columnName); err != nil {
			return nil, fmt.Errorf("scan node index columns: %w", err)
		}
		columns = append(columns, columnName.String)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate node index columns: %w", err)
	}
	return columns, nil
}

func (store *guardianStore) rebuildNodesWithScopedAddressIndex() error {
	if _, err := store.database.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for node migration: %w", err)
	}
	defer store.database.Exec(`PRAGMA foreign_keys = ON`)

	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin node scope migration: %w", err)
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE nodes_scope_migration (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			address TEXT NOT NULL,
			scope TEXT NOT NULL DEFAULT 'inspection',
			protocol TEXT NOT NULL,
			host TEXT NOT NULL,
			port INTEGER NOT NULL,
			status TEXT NOT NULL,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			exit_ip TEXT NOT NULL DEFAULT '',
			country TEXT NOT NULL DEFAULT '',
			last_checked INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		)`,
		`INSERT INTO nodes_scope_migration(id, address, scope, protocol, host, port, status, latency_ms, exit_ip, country, last_checked, last_error, created_at)
		 SELECT id, address, COALESCE(NULLIF(scope, ''), 'inspection'), protocol, host, port, status, latency_ms, exit_ip, country, last_checked, last_error, created_at FROM nodes`,
		`DROP TABLE nodes`,
		`ALTER TABLE nodes_scope_migration RENAME TO nodes`,
		`CREATE INDEX idx_nodes_status ON nodes(status, id)`,
		`CREATE UNIQUE INDEX idx_nodes_scope_address ON nodes(scope, address)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate node scope schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit node scope migration: %w", err)
	}
	return nil
}

func (store *guardianStore) recoverKeepaliveState() error {
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin keepalive recovery: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE nodes
SET status = COALESCE(NULLIF((SELECT previous_status FROM keepalive_round_nodes WHERE node_id = nodes.id ORDER BY round_id DESC LIMIT 1), ''), ?)
WHERE scope = 'guard' AND status = ?`, statusUninspected, statusKeepaliveProbing); err != nil {
		return fmt.Errorf("restore interrupted keepalive nodes: %w", err)
	}
	if _, err := tx.Exec(`UPDATE keepalive_rounds SET completed_at = ?, status = 'interrupted' WHERE status = 'running'`, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("mark interrupted keepalive rounds: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit keepalive recovery: %w", err)
	}
	return nil
}

func (store *guardianStore) startKeepaliveRound() (int64, error) {
	result, err := store.database.Exec(`INSERT INTO keepalive_rounds(started_at, status) VALUES (?, 'running')`, time.Now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("start keepalive round: %w", err)
	}
	return result.LastInsertId()
}

func (store *guardianStore) snapshotKeepaliveRound(roundID int64) (int64, error) {
	result, err := store.database.Exec(`INSERT INTO keepalive_round_nodes(round_id, node_id, previous_status)
SELECT ?, id, status FROM nodes
WHERE scope = 'guard' AND status IN (?, ?, ?, ?, ?)`, roundID, statusHealthy, statusHealthyCandidate, statusHealthyFallback, statusConnected, statusCooldown)
	if err != nil {
		return 0, fmt.Errorf("snapshot keepalive nodes: %w", err)
	}
	candidateCount, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read keepalive snapshot count: %w", err)
	}
	if _, err := store.database.Exec(`UPDATE keepalive_rounds SET candidate_count = ? WHERE id = ?`, candidateCount, roundID); err != nil {
		return 0, fmt.Errorf("save keepalive snapshot count: %w", err)
	}
	return candidateCount, nil
}

func (store *guardianStore) claimNextKeepalive(roundID int64) (keepaliveNodeClaim, bool, error) {
	tx, err := store.database.Begin()
	if err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("begin keepalive claim: %w", err)
	}
	defer tx.Rollback()
	var claim keepaliveNodeClaim
	err = tx.QueryRow(`SELECT nodes.id, nodes.address, nodes.protocol, nodes.host, nodes.port, nodes.status, nodes.latency_ms, nodes.exit_ip, nodes.country, nodes.last_checked, nodes.last_error, nodes.created_at, keepalive_round_nodes.previous_status
FROM keepalive_round_nodes
INNER JOIN nodes ON nodes.id = keepalive_round_nodes.node_id
WHERE keepalive_round_nodes.round_id = ? AND keepalive_round_nodes.completed_at = 0 AND nodes.scope = 'guard' AND nodes.status IN (?, ?, ?, ?, ?, ?, ?)
ORDER BY nodes.id
LIMIT 1`, roundID, statusUninspected, statusHealthy, statusUnhealthy, statusHealthyCandidate, statusHealthyFallback, statusConnected, statusCooldown).Scan(
		&claim.Node.ID,
		&claim.Node.Address,
		&claim.Node.Protocol,
		&claim.Node.Host,
		&claim.Node.Port,
		&claim.Node.Status,
		&claim.Node.LatencyMS,
		&claim.Node.ExitIP,
		&claim.Node.Country,
		&claim.Node.LastChecked,
		&claim.Node.LastError,
		&claim.Node.CreatedAt,
		&claim.PreviousStatus,
	)
	if err == sql.ErrNoRows {
		return keepaliveNodeClaim{}, false, nil
	}
	if err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("read keepalive claim: %w", err)
	}
	if _, err := tx.Exec(`UPDATE nodes SET status = ? WHERE id = ? AND scope = 'guard' AND status IN (?, ?, ?, ?, ?, ?, ?)`, statusKeepaliveProbing, claim.Node.ID, statusUninspected, statusHealthy, statusUnhealthy, statusHealthyCandidate, statusHealthyFallback, statusConnected, statusCooldown); err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("mark keepalive node: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return keepaliveNodeClaim{}, false, fmt.Errorf("commit keepalive claim: %w", err)
	}
	return claim, true, nil
}

func (store *guardianStore) updateKeepaliveNodeResult(roundID int64, nodeID int64, result nodeProbeResult) error {
	result.Error = sanitizeLogText(result.Error)
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin keepalive result update: %w", err)
	}
	defer tx.Rollback()
	var nodeName string
	if err := tx.QueryRow(`SELECT address FROM nodes WHERE id = ? AND scope = 'guard'`, nodeID).Scan(&nodeName); err != nil {
		return fmt.Errorf("read keepalive node name: %w", err)
	}
	updated, err := tx.Exec(`UPDATE nodes SET status = ?, latency_ms = ?, exit_ip = ?, country = ?, last_checked = ?, last_error = ?
WHERE id = ? AND scope = 'guard' AND status = ? AND EXISTS (SELECT 1 FROM keepalive_round_nodes WHERE round_id = ? AND node_id = ?)`, result.Status, result.LatencyMS, result.ExitIP, result.Country, result.CheckedAt, result.Error, nodeID, statusKeepaliveProbing, roundID, nodeID)
	if err != nil {
		return fmt.Errorf("update keepalive node result: %w", err)
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return fmt.Errorf("read keepalive node update result: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("keepalive node %d was not claimed by round %d", nodeID, roundID)
	}
	storedStatus := result.Status
	if result.Status == statusHealthy {
		var slotKind string
		if err := tx.QueryRow(`SELECT COALESCE((SELECT slot_kind FROM healthy_slots WHERE node_id = ? LIMIT 1), '')`, nodeID).Scan(&slotKind); err != nil {
			return fmt.Errorf("read keepalive node slot kind: %w", err)
		}
		if slotKind == "candidate" {
			storedStatus = statusHealthyCandidate
		}
	}
	if storedStatus != result.Status {
		if _, err := tx.Exec(`UPDATE nodes SET status = ? WHERE id = ? AND scope = 'guard' AND status = ?`, storedStatus, nodeID, result.Status); err != nil {
			return fmt.Errorf("preserve keepalive node slot status: %w", err)
		}
	}
	if result.Status != statusHealthy {
		if _, err := tx.Exec(`DELETE FROM healthy_slots WHERE node_id = ?`, nodeID); err != nil {
			return fmt.Errorf("clear failed keepalive slots: %w", err)
		}
		if _, err := tx.Exec(`UPDATE auth_bindings SET slot_id = 0, node_id = 0, proxy_url = '', updated_at = ? WHERE node_id = ?`, time.Now().UnixMilli(), nodeID); err != nil {
			return fmt.Errorf("clear failed keepalive auth bindings: %w", err)
		}
	}
	marked, err := tx.Exec(`UPDATE keepalive_round_nodes SET completed_at = ? WHERE round_id = ? AND node_id = ? AND completed_at = 0`, result.CheckedAt, roundID, nodeID)
	if err != nil {
		return fmt.Errorf("mark keepalive round node completed: %w", err)
	}
	markedCount, err := marked.RowsAffected()
	if err != nil {
		return fmt.Errorf("read keepalive round node completion: %w", err)
	}
	if markedCount != 1 {
		return fmt.Errorf("keepalive round node %d was not completed in round %d", nodeID, roundID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit keepalive node result: %w", err)
	}
	return store.appendRoundNodeLog(logCategoryKeepalive, roundID, logStatusForNodeResult(result.Status), logLevelInfo, "keepalive.node_result", nodeID, nodeName, "保活探测节点结果", fmt.Sprintf("状态=%s；延迟=%dms；出口IP=%s；国家=%s；错误=%s", result.Status, result.LatencyMS, result.ExitIP, result.Country, result.Error))
}

func (store *guardianStore) resetKeepaliveRound(roundID int64) error {
	_, err := store.database.Exec(`UPDATE nodes SET status = COALESCE(NULLIF((SELECT previous_status FROM keepalive_round_nodes WHERE round_id = ? AND node_id = nodes.id), ''), ?)
WHERE scope = 'guard' AND status = ? AND id IN (SELECT node_id FROM keepalive_round_nodes WHERE round_id = ?)`, roundID, statusUninspected, statusKeepaliveProbing, roundID)
	if err != nil {
		return fmt.Errorf("reset keepalive round nodes: %w", err)
	}
	return nil
}

func (store *guardianStore) finishKeepaliveRound(roundID int64, status string, successCount, failureCount, deletedBatches, deletedNodes int64) error {
	_, err := store.database.Exec(`UPDATE keepalive_rounds SET completed_at = ?, status = ?, success_count = ?, failure_count = ?, deleted_batches = ?, deleted_nodes = ? WHERE id = ?`, time.Now().UnixMilli(), status, successCount, failureCount, deletedBatches, deletedNodes, roundID)
	if err != nil {
		return fmt.Errorf("finish keepalive round: %w", err)
	}
	return nil
}

func (store *guardianStore) latestKeepaliveRound() (keepaliveRound, bool, error) {
	var round keepaliveRound
	err := store.database.QueryRow(`SELECT id, started_at, completed_at, status, candidate_count, success_count, failure_count, deleted_batches, deleted_nodes
FROM keepalive_rounds ORDER BY id DESC LIMIT 1`).Scan(&round.ID, &round.StartedAt, &round.CompletedAt, &round.Status, &round.CandidateCount, &round.SuccessCount, &round.FailureCount, &round.DeletedBatches, &round.DeletedNodes)
	if err == sql.ErrNoRows {
		return keepaliveRound{}, false, nil
	}
	if err != nil {
		return keepaliveRound{}, false, fmt.Errorf("read latest keepalive round: %w", err)
	}
	return round, true, nil
}
