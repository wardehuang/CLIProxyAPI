package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	defaultDatabasePath             = "/opt/cli-proxy-api/plugin-data/cpa-xai-ip-switcher/ip-switcher.sqlite3"
	defaultWorkerCount              = 4
	defaultRefreshIntervalSeconds   = 30
	defaultKeepaliveWorkerCount     = 8
	defaultKeepaliveIntervalSeconds = 1800
	defaultReviveIntervalSeconds    = 1800
	defaultProbeRetryCount          = 3
	defaultHealthySlotCount         = 50
	defaultHealthyCandidateCount    = 20
	defaultQualityWorkerCount       = 8
	defaultQualityProbeTimeout      = 25
	defaultQualitySoftTPS           = 500.0
	defaultQualityHardTPS           = 1000.0
	maxProbeWorkers                 = 64
	maxRefreshIntervalSeconds       = 3600
	maxKeepaliveIntervalSeconds     = 86400
	maxProbeRetryCount              = 10
	maxReviveFailureCount           = 3
	maxSlotCount                    = 1000
	maxQualityProbeTimeoutSeconds   = 600
	settingsDefaultsVersion         = "3"
	maxPluginLogs                   = 1000
	maxGroupedLogSets               = 10

	logLevelInfo  = "info"
	logLevelWarn  = "warn"
	logLevelError = "error"

	logCategoryGeneral        = "general"
	logCategoryBatchProbe     = "batch_probe"
	logCategoryKeepaliveProbe = "keepalive_probe"
	logCategoryQualityProbe   = "quality_probe"
	logCategoryRealtimeGuard  = "realtime_guard"
	logCategoryReviveProbe    = "revive_probe"
	logStatusConnected        = "connected"
	logStatusProbing          = "probing"
	logStatusError            = "error"

	groupStatusRunning   = "running"
	groupStatusCompleted = "completed"

	statusHealthy          = "healthy"
	statusHealthyCandidate = "healthy_candidate"
	statusHealthyFallback  = "healthy_fallback"
	statusConnected        = "connected"
	statusCooldown         = "cooldown"
	statusProbing          = "probing"
	statusKeepaliveProbing = "keepalive_probing"
	statusQualityProbing   = "quality_probing"
	statusReviveProbing    = "revive_probing"
	statusUnprobed         = "unprobed"
	statusError            = "error"
	statusAll              = "all"

	probeKindInitial   = "initial"
	probeKindKeepalive = "keepalive"
	probeKindQuality   = "quality"
	probeKindRevive    = "revive"
)

type pluginSettings struct {
	WorkerCount                int
	RefreshIntervalSeconds     int
	KeepaliveWorkerCount       int
	KeepaliveIntervalSeconds   int
	ReviveIntervalSeconds      int
	ProbeRetryCount            int
	HealthySlotCount           int
	HealthyCandidateSlotCount  int
	QualityWorkerCount         int
	QualityProbeTimeoutSeconds int
	QualitySoftTPS             float64
	QualityHardTPS             float64
	Grok2apiSyncEnabled        bool
	Grok2apiBaseUrl            string
	Grok2apiAdminUsername      string
	Grok2apiAdminPassword      string
}

type proxyNode struct {
	ID                 int64
	Name               string
	ProxyURL           string
	Host               string
	InputIP            string
	Port               int
	Protocol           string
	Domain             string
	BatchID            string
	Status             string
	InitialConnected   int64
	ProbeKind          string
	ProbeReturnStatus  string
	KeepaliveRoundID   int64
	ReviveRoundID      int64
	ReviveFailureCount int64
	LatencyMs          int64
	EnteredAt          int64
	ProbeStartedAt     int64
	ProbeTime          int64
	ExitIP             string
	ExitCountry        string
	ErrorReason        string
	ErrorDetail        string
	ReviveTargetStatus string
	SlotID             int64
	FallbackOrigin     bool
	EmptySlot          bool
}

type ipBatch struct {
	ID                     string
	SequenceNumber         int64
	CreatedAt              int64
	TotalCount             int64
	DuplicateCount         int64
	InputErrorCount        int64
	DeleteNonUS            int64
	CompletedCount         int64
	PendingCount           int64
	CandidateCount         int64
	InitialConnectedCount  int64
	RealtimeConnectedCount int64
}

type keepaliveRound struct {
	ID             int64
	SequenceNumber int64
	StartedAt      int64
	CompletedAt    int64
	Status         string
	CandidateCount int64
	SuccessCount   int64
	FailureCount   int64
}

type logGroup struct {
	ID                      string
	SequenceNumber          int64
	StartedAt               int64
	CompletedAt             int64
	Status                  string
	LogCount                int64
	Category                string
	CandidateCount          int64
	SuccessCount            int64
	FailureCount            int64
	ConnectivityCompletedAt int64
	QualityStartedAt        int64
	QualityCompletedAt      int64
	QualityCandidateCount   int64
	QualitySuccessCount     int64
	QualityFailureCount     int64
}

type inputLineError struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

type pluginLog struct {
	ID        int64
	CreatedAt int64
	Level     string
	Event     string
	Category  string
	GroupID   string
	LogStatus string
	NodeID    int64
	NodeName  string
	Message   string
	Detail    string
}

type ipStore struct {
	database *sql.DB
	path     string
}

func openIPStore(path string) (*ipStore, error) {
	resolvedPath, err := resolveDatabasePath(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	database, err := sql.Open("sqlite3", resolvedPath+"?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	store := &ipStore{database: database, path: resolvedPath}
	if err := store.initialize(); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := os.Chmod(resolvedPath, 0o600); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("protect sqlite database: %w", err)
	}
	return store, nil
}

func resolveDatabasePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultDatabasePath
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	return filepath.Clean(filepath.Join(workingDirectory, path)), nil
}

func (store *ipStore) initialize() error {
	_, err := store.database.Exec(`
CREATE TABLE IF NOT EXISTS ip_nodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    node_name TEXT NOT NULL,
    proxy_url TEXT NOT NULL UNIQUE,
    host TEXT NOT NULL,
    input_ip TEXT NOT NULL,
    port INTEGER NOT NULL,
    protocol TEXT NOT NULL,
    domain TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    initial_connected INTEGER NOT NULL DEFAULT 0,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    entered_at INTEGER NOT NULL,
    probe_started_at INTEGER NOT NULL DEFAULT 0,
    probe_kind TEXT NOT NULL DEFAULT '',
    probe_return_status TEXT NOT NULL DEFAULT '',
    keepalive_round_id INTEGER NOT NULL DEFAULT 0,
    revive_round_id INTEGER NOT NULL DEFAULT 0,
    revive_failure_count INTEGER NOT NULL DEFAULT 0,
    probe_time INTEGER NOT NULL DEFAULT 0,
    exit_ip TEXT NOT NULL DEFAULT '',
    exit_country TEXT NOT NULL DEFAULT '',
    error_reason TEXT NOT NULL DEFAULT '',
    error_detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_ip_nodes_status_id ON ip_nodes(status, id);
CREATE TABLE IF NOT EXISTS ip_node_statuses (
    status TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    sort_order INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ip_node_statuses_sort_order ON ip_node_statuses(sort_order, status);
INSERT OR IGNORE INTO ip_node_statuses(status, display_name, sort_order) VALUES
    ('healthy', '健康', 10),
    ('healthy_candidate', '健康备选', 20),
    ('healthy_fallback', '健康保底', 30),
    ('connected', '已连通', 40),
    ('cooldown', '冷却中', 50),
    ('probing', '探测中', 60),
    ('keepalive_probing', '保活探测中', 70),
    ('quality_probing', '智商探测中', 75),
    ('revive_probing', '复活探测中', 80),
    ('unprobed', '未探测', 90),
    ('error', '异常', 100);
CREATE TABLE IF NOT EXISTS ip_batches (
    batch_id TEXT PRIMARY KEY,
    sequence_number INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    total_count INTEGER NOT NULL DEFAULT 0,
    duplicate_count INTEGER NOT NULL DEFAULT 0,
    input_error_count INTEGER NOT NULL DEFAULT 0,
    delete_non_us INTEGER NOT NULL DEFAULT 0,
    initial_probe_completed_count INTEGER NOT NULL DEFAULT 0,
    initial_connected_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ip_batches_created_at ON ip_batches(created_at DESC);
CREATE TABLE IF NOT EXISTS keepalive_rounds (
    round_id INTEGER PRIMARY KEY,
    sequence_number INTEGER NOT NULL,
    started_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'running',
    candidate_count INTEGER NOT NULL DEFAULT 0,
    success_count INTEGER NOT NULL DEFAULT 0,
    failure_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_keepalive_rounds_sequence ON keepalive_rounds(sequence_number DESC);
CREATE TABLE IF NOT EXISTS keepalive_round_nodes (
    round_id INTEGER NOT NULL,
    node_id INTEGER NOT NULL,
    PRIMARY KEY(round_id, node_id)
);
CREATE INDEX IF NOT EXISTS idx_keepalive_round_nodes_node_id ON keepalive_round_nodes(node_id);
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
CREATE INDEX IF NOT EXISTS idx_revive_round_nodes_node_id ON revive_round_nodes(node_id);
CREATE TABLE IF NOT EXISTS plugin_settings (
    setting_key TEXT PRIMARY KEY,
    setting_value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS plugin_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at INTEGER NOT NULL,
    level TEXT NOT NULL,
    event TEXT NOT NULL,
    category TEXT NOT NULL DEFAULT 'general',
    group_id TEXT NOT NULL DEFAULT '',
    log_status TEXT NOT NULL DEFAULT '',
    node_id INTEGER NOT NULL DEFAULT 0,
    node_name TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_plugin_logs_created_id ON plugin_logs(created_at DESC, id DESC);
INSERT OR IGNORE INTO plugin_settings(setting_key, setting_value) VALUES
    ('worker_count', '4'),
    ('refresh_interval_seconds', '30'),
    ('keepalive_worker_count', '8'),
    ('keepalive_interval_seconds', '1800'),
    ('revive_interval_seconds', '1800'),
    ('probe_retry_count', '3'),
    ('healthy_slot_count', '50'),
    ('healthy_candidate_slot_count', '20'),
    ('quality_worker_count', '8'),
    ('quality_probe_timeout_seconds', '25'),
    ('quality_soft_tps', '500'),
    ('quality_hard_tps', '1000');
`)
	if err != nil {
		return fmt.Errorf("initialize sqlite database: %w", err)
	}
	if err := store.migrateSettingsDefaults(); err != nil {
		return err
	}
	if err := store.ensureProbeColumns(); err != nil {
		return err
	}
	if _, err := store.database.Exec(`CREATE INDEX IF NOT EXISTS idx_ip_nodes_revive_candidates ON ip_nodes(status, exit_country, id)`); err != nil {
		return fmt.Errorf("create sqlite revive candidate index: %w", err)
	}
	if err := store.ensureBatchColumns(); err != nil {
		return err
	}
	if err := store.ensureLogColumns(); err != nil {
		return err
	}
	if err := store.ensureBatchMetadata(); err != nil {
		return err
	}
	if err := store.ensureInitialConnectedColumn(); err != nil {
		return err
	}
	if err := store.ensureReviveMetadata(); err != nil {
		return err
	}
	if err := store.ensureSlotMetadata(); err != nil {
		return err
	}
	if err := store.ensureRealtimeGuardMetadata(); err != nil {
		return err
	}
	if err := store.clearAutomaticFallbackNodes(); err != nil {
		return err
	}
	if err := store.pruneStoredLogs(); err != nil {
		return err
	}
	_, err = store.database.Exec(`
UPDATE ip_nodes
SET status = CASE
        WHEN probe_kind = 'realtime_guard' AND manual_fallback = 1 THEN 'healthy_fallback'
        WHEN probe_kind = 'fallback' THEN 'connected'
        WHEN probe_kind IN ('keepalive', 'quality') AND probe_return_status IN ('healthy', 'healthy_candidate', 'healthy_fallback', 'connected', 'cooldown') THEN probe_return_status
        WHEN probe_kind = 'revive' AND revive_target_status IN ('cooldown', 'connected') THEN revive_target_status
        WHEN probe_kind = 'revive' THEN 'error'
        ELSE 'unprobed'
    END,
    probe_started_at = 0,
    probe_kind = '',
    probe_return_status = '',
    keepalive_round_id = 0,
    revive_round_id = 0
WHERE status IN ('probing', 'keepalive_probing', 'quality_probing', 'revive_probing');`)
	if err != nil {
		return fmt.Errorf("reset interrupted sqlite probes: %w", err)
	}
	if _, err := store.database.Exec(`
UPDATE ip_slots
SET claim_node_id = 0, claim_token = '', claim_stage = '', claim_started_at = 0
WHERE claim_token <> '' OR claim_node_id <> 0`); err != nil {
		return fmt.Errorf("clear interrupted sqlite slot claims: %w", err)
	}
	if _, err := store.database.Exec(`DELETE FROM keepalive_round_nodes`); err != nil {
		return fmt.Errorf("clear interrupted sqlite keepalive round: %w", err)
	}
	if _, err := store.database.Exec(`DELETE FROM revive_round_nodes`); err != nil {
		return fmt.Errorf("clear interrupted sqlite revive round: %w", err)
	}
	if _, err := store.database.Exec(`
UPDATE keepalive_rounds
SET status = ?, completed_at = ?
WHERE status = ?`, groupStatusCompleted, time.Now().UnixMilli(), groupStatusRunning); err != nil {
		return fmt.Errorf("finish interrupted sqlite keepalive rounds: %w", err)
	}
	if _, err := store.database.Exec(`
UPDATE revive_rounds
SET status = ?, completed_at = ?
WHERE status = ?`, groupStatusCompleted, time.Now().UnixMilli(), groupStatusRunning); err != nil {
		return fmt.Errorf("finish interrupted sqlite revive rounds: %w", err)
	}
	return nil
}

func (store *ipStore) migrateSettingsDefaults() error {
	var version string
	err := store.database.QueryRow(`SELECT setting_value FROM plugin_settings WHERE setting_key = 'settings_defaults_version'`).Scan(&version)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("read settings defaults version: %w", err)
	}

	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin settings defaults migration: %w", err)
	}
	defer transaction.Rollback()

	var keepaliveInterval string
	if err := transaction.QueryRow(`SELECT setting_value FROM plugin_settings WHERE setting_key = 'keepalive_interval_seconds'`).Scan(&keepaliveInterval); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read legacy keepalive interval: %w", err)
	}
	if strings.TrimSpace(keepaliveInterval) == "300" {
		if _, err := transaction.Exec(`UPDATE plugin_settings SET setting_value = ? WHERE setting_key = 'keepalive_interval_seconds'`, strconv.Itoa(defaultKeepaliveIntervalSeconds)); err != nil {
			return fmt.Errorf("migrate keepalive interval default: %w", err)
		}
	}
	if _, err := transaction.Exec(`
INSERT INTO plugin_settings(setting_key, setting_value) VALUES (?, ?)
ON CONFLICT(setting_key) DO NOTHING`, "revive_interval_seconds", strconv.Itoa(defaultReviveIntervalSeconds)); err != nil {
		return fmt.Errorf("migrate revive interval default: %w", err)
	}
	if _, err := transaction.Exec(`
INSERT INTO plugin_settings(setting_key, setting_value) VALUES (?, ?)
ON CONFLICT(setting_key) DO UPDATE SET setting_value = excluded.setting_value`, "settings_defaults_version", settingsDefaultsVersion); err != nil {
		return fmt.Errorf("save settings defaults version: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit settings defaults migration: %w", err)
	}
	return nil
}

func (store *ipStore) ensureProbeColumns() error {
	rows, err := store.database.Query(`PRAGMA table_info(ip_nodes)`)
	if err != nil {
		return fmt.Errorf("inspect sqlite node columns: %w", err)
	}
	defer rows.Close()

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
			return fmt.Errorf("scan sqlite node columns: %w", err)
		}
		columns[columnName] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate sqlite node columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close sqlite node columns: %w", err)
	}
	for columnName, columnDefinition := range map[string]string{
		"probe_kind":           "TEXT NOT NULL DEFAULT ''",
		"probe_return_status":  "TEXT NOT NULL DEFAULT ''",
		"keepalive_round_id":   "INTEGER NOT NULL DEFAULT 0",
		"revive_round_id":      "INTEGER NOT NULL DEFAULT 0",
		"revive_failure_count": "INTEGER NOT NULL DEFAULT 0",
		"exit_country":         "TEXT NOT NULL DEFAULT ''",
	} {
		if columns[columnName] {
			continue
		}
		if _, err := store.database.Exec("ALTER TABLE ip_nodes ADD COLUMN " + columnName + " " + columnDefinition); err != nil {
			return fmt.Errorf("add sqlite node column %s: %w", columnName, err)
		}
	}
	return nil
}

func (store *ipStore) ensureInitialConnectedColumn() error {
	rows, err := store.database.Query(`PRAGMA table_info(ip_nodes)`)
	if err != nil {
		return fmt.Errorf("inspect sqlite initial connection column: %w", err)
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
			return fmt.Errorf("scan sqlite initial connection column: %w", err)
		}
		columns[columnName] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate sqlite initial connection column: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close sqlite initial connection column: %w", err)
	}
	if !columns["initial_connected"] {
		if _, err := store.database.Exec("ALTER TABLE ip_nodes ADD COLUMN initial_connected INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("add sqlite initial connection column: %w", err)
		}
	}
	if _, err := store.database.Exec(`
UPDATE ip_nodes
SET initial_connected = 1
WHERE initial_connected = 0
  AND id IN (
      SELECT node_id
      FROM plugin_logs
      WHERE category = ?
        AND event = 'probe.completed'
        AND log_status = ?
        AND node_id > 0
  )`, logCategoryBatchProbe, logStatusConnected); err != nil {
		return fmt.Errorf("backfill sqlite initial connected nodes: %w", err)
	}
	if _, err := store.database.Exec(`
UPDATE ip_nodes
SET initial_connected = 1
WHERE initial_connected = 0
  AND status IN (?, ?, ?, ?, ?)`, statusHealthy, statusHealthyCandidate, statusHealthyFallback, statusConnected, statusCooldown); err != nil {
		return fmt.Errorf("infer sqlite initial connected nodes: %w", err)
	}
	return nil
}

func (store *ipStore) ensureBatchColumns() error {
	rows, err := store.database.Query(`PRAGMA table_info(ip_nodes)`)
	if err != nil {
		return fmt.Errorf("inspect sqlite batch columns: %w", err)
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
			return fmt.Errorf("scan sqlite batch columns: %w", err)
		}
		columns[columnName] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate sqlite batch columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close sqlite batch columns: %w", err)
	}
	if !columns["batch_id"] {
		if _, err := store.database.Exec("ALTER TABLE ip_nodes ADD COLUMN batch_id TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("add sqlite batch column: %w", err)
		}
	}
	if _, err := store.database.Exec(`CREATE INDEX IF NOT EXISTS idx_ip_nodes_batch_id ON ip_nodes(batch_id)`); err != nil {
		return fmt.Errorf("create sqlite batch index: %w", err)
	}

	var legacyCount int64
	if err := store.database.QueryRow(`SELECT COUNT(*) FROM ip_nodes WHERE batch_id = ''`).Scan(&legacyCount); err != nil {
		return fmt.Errorf("count legacy sqlite nodes: %w", err)
	}
	if legacyCount == 0 {
		return nil
	}
	legacyCreatedAt := time.Now().UnixMilli()
	if err := store.database.QueryRow(`SELECT COALESCE(MIN(entered_at), ?) FROM ip_nodes WHERE batch_id = ''`, legacyCreatedAt).Scan(&legacyCreatedAt); err != nil {
		return fmt.Errorf("read legacy sqlite batch time: %w", err)
	}
	if _, err := store.database.Exec(`
INSERT OR IGNORE INTO ip_batches(batch_id, created_at, total_count)
VALUES ('legacy', ?, ?)`, legacyCreatedAt, legacyCount); err != nil {
		return fmt.Errorf("create legacy sqlite batch: %w", err)
	}
	if _, err := store.database.Exec(`UPDATE ip_nodes SET batch_id = 'legacy' WHERE batch_id = ''`); err != nil {
		return fmt.Errorf("assign legacy sqlite batch: %w", err)
	}
	if _, err := store.database.Exec(`
UPDATE ip_batches
SET total_count = (SELECT COUNT(*) FROM ip_nodes WHERE batch_id = 'legacy')
WHERE batch_id = 'legacy'`); err != nil {
		return fmt.Errorf("update legacy sqlite batch: %w", err)
	}
	return nil
}

func (store *ipStore) close() error {
	if store == nil || store.database == nil {
		return nil
	}
	return store.database.Close()
}

func newBatchID() string {
	return fmt.Sprintf("B%d", time.Now().UnixNano())
}

func (store *ipStore) insertNodes(nodes []proxyNode, inputErrorCount int, deleteNonUS, manualFallback bool) (batchID string, added, duplicates int, err error) {
	if len(nodes) == 0 {
		return "", 0, 0, nil
	}
	batchID = newBatchID()
	transaction, err := store.database.Begin()
	if err != nil {
		return batchID, 0, 0, fmt.Errorf("begin sqlite batch insert: %w", err)
	}
	defer func() {
		if err != nil {
			_ = transaction.Rollback()
		}
	}()

	createdAt := time.Now().UnixMilli()
	var sequenceNumber int64
	if err := transaction.QueryRow(`SELECT COALESCE(MAX(sequence_number), 0) + 1 FROM ip_batches`).Scan(&sequenceNumber); err != nil {
		return batchID, 0, 0, fmt.Errorf("read sqlite batch sequence: %w", err)
	}
	if _, err := transaction.Exec(`
INSERT INTO ip_batches(batch_id, sequence_number, created_at, input_error_count, delete_non_us)
VALUES (?, ?, ?, ?, ?)`, batchID, sequenceNumber, createdAt, inputErrorCount, deleteNonUS); err != nil {
		return batchID, 0, 0, fmt.Errorf("create sqlite batch: %w", err)
	}

	initialStatus := statusUnprobed
	initialConnected := 0
	manualFallbackValue := 0
	if manualFallback {
		initialStatus = statusHealthyFallback
		initialConnected = 1
		manualFallbackValue = 1
	}
	statement, err := transaction.Prepare(`
INSERT OR IGNORE INTO ip_nodes(
    node_name, proxy_url, host, input_ip, port, protocol, domain, batch_id, status, initial_connected, manual_fallback, entered_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return batchID, 0, 0, fmt.Errorf("prepare sqlite insert: %w", err)
	}
	defer statement.Close()

	for _, node := range nodes {
		result, execErr := statement.Exec(
			node.Name,
			node.ProxyURL,
			node.Host,
			node.InputIP,
			node.Port,
			node.Protocol,
			node.Domain,
			batchID,
			initialStatus,
			initialConnected,
			manualFallbackValue,
			createdAt,
		)
		if execErr != nil {
			return batchID, 0, 0, fmt.Errorf("insert sqlite node: %w", execErr)
		}
		rowsAffected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return batchID, 0, 0, fmt.Errorf("read sqlite insert result: %w", rowsErr)
		}
		if rowsAffected == 0 {
			duplicates++
		} else {
			added++
		}
	}
	initialProbeCompletedCount := 0
	initialConnectedCount := 0
	if manualFallback {
		initialProbeCompletedCount = added
		initialConnectedCount = added
	}
	if _, err := transaction.Exec(`
UPDATE ip_batches
SET total_count = ?, duplicate_count = ?, initial_probe_completed_count = ?, initial_connected_count = ?
WHERE batch_id = ?`, added, duplicates, initialProbeCompletedCount, initialConnectedCount, batchID); err != nil {
		return batchID, 0, 0, fmt.Errorf("update sqlite batch: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return batchID, 0, 0, fmt.Errorf("commit sqlite batch insert: %w", err)
	}
	return batchID, added, duplicates, nil
}

func (store *ipStore) listNodes(status string) ([]proxyNode, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if status == statusAll {
		rows, err = store.database.Query(`
SELECT nodes.id, nodes.node_name, nodes.proxy_url, nodes.batch_id, nodes.protocol, nodes.status, nodes.initial_connected,
       nodes.latency_ms, nodes.entered_at, nodes.probe_started_at, nodes.probe_time, nodes.exit_ip, nodes.exit_country,
       nodes.revive_failure_count, nodes.error_reason, nodes.error_detail,
       COALESCE(slots.slot_id, 0), COALESCE(slots.fallback_origin, 0)
FROM ip_nodes AS nodes
LEFT JOIN ip_slots AS slots ON slots.node_id = nodes.id OR slots.claim_node_id = nodes.id
ORDER BY nodes.id DESC`)
	} else {
		rows, err = store.database.Query(`
SELECT nodes.id, nodes.node_name, nodes.proxy_url, nodes.batch_id, nodes.protocol, nodes.status, nodes.initial_connected,
       nodes.latency_ms, nodes.entered_at, nodes.probe_started_at, nodes.probe_time, nodes.exit_ip, nodes.exit_country,
       nodes.revive_failure_count, nodes.error_reason, nodes.error_detail,
       COALESCE(slots.slot_id, 0), COALESCE(slots.fallback_origin, 0)
FROM ip_nodes AS nodes
LEFT JOIN ip_slots AS slots ON slots.node_id = nodes.id OR slots.claim_node_id = nodes.id
WHERE nodes.status = ? ORDER BY nodes.id DESC`, status)
	}
	if err != nil {
		return nil, fmt.Errorf("list sqlite nodes: %w", err)
	}
	defer rows.Close()

	items := make([]proxyNode, 0)
	for rows.Next() {
		var node proxyNode
		if err := rows.Scan(
			&node.ID,
			&node.Name,
			&node.ProxyURL,
			&node.BatchID,
			&node.Protocol,
			&node.Status,
			&node.InitialConnected,
			&node.LatencyMs,
			&node.EnteredAt,
			&node.ProbeStartedAt,
			&node.ProbeTime,
			&node.ExitIP,
			&node.ExitCountry,
			&node.ReviveFailureCount,
			&node.ErrorReason,
			&node.ErrorDetail,
			&node.SlotID,
			&node.FallbackOrigin,
		); err != nil {
			return nil, fmt.Errorf("scan sqlite node: %w", err)
		}
		node.Name = displayProxyNodeName(node.ProxyURL, node.Name)
		items = append(items, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite nodes: %w", err)
	}
	if status == statusAll || status == statusHealthy {
		emptyHealthySlots, err := store.listEmptyHealthySlotNodes()
		if err != nil {
			return nil, err
		}
		items = append(items, emptyHealthySlots...)
	}
	return items, nil
}

func (store *ipStore) listEmptyHealthySlotNodes() ([]proxyNode, error) {
	rows, err := store.database.Query(`
SELECT slot_id
FROM ip_slots
WHERE slot_kind = ? AND node_id = 0
ORDER BY slot_id ASC`, statusHealthy)
	if err != nil {
		return nil, fmt.Errorf("list sqlite empty healthy slots: %w", err)
	}
	defer rows.Close()

	items := make([]proxyNode, 0)
	for rows.Next() {
		var slotID int64
		if err := rows.Scan(&slotID); err != nil {
			return nil, fmt.Errorf("scan sqlite empty healthy slot: %w", err)
		}
		items = append(items, proxyNode{
			ID:        -slotID,
			Status:    statusHealthy,
			SlotID:    slotID,
			EmptySlot: true,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite empty healthy slots: %w", err)
	}
	return items, nil
}

func (store *ipStore) listBatches() ([]ipBatch, error) {
	rows, err := store.database.Query(`
SELECT
    batches.batch_id,
    batches.sequence_number,
    batches.created_at,
    batches.duplicate_count,
    batches.input_error_count,
    batches.delete_non_us,
    batches.total_count,
    batches.initial_probe_completed_count,
    CASE
        WHEN batches.total_count > batches.initial_probe_completed_count THEN batches.total_count - batches.initial_probe_completed_count
        ELSE 0
    END,
    COALESCE(SUM(CASE
        WHEN nodes.status IN (?, ?, ?, ?)
         AND NOT EXISTS (
             SELECT 1
             FROM ip_nodes AS batch_candidates
             WHERE batch_candidates.batch_id = batches.batch_id
               AND (batch_candidates.status IN (?, ?) OR batch_candidates.probe_kind = ?)
         ) THEN 1
        ELSE 0
    END), 0),
    batches.initial_connected_count,
    COALESCE(SUM(CASE
        WHEN nodes.status IN (?, ?, ?, ?) THEN 1
        ELSE 0
    END), 0)
FROM ip_batches AS batches
LEFT JOIN ip_nodes AS nodes ON nodes.batch_id = batches.batch_id
GROUP BY batches.batch_id, batches.sequence_number, batches.created_at, batches.total_count, batches.duplicate_count, batches.input_error_count, batches.delete_non_us, batches.initial_probe_completed_count, batches.initial_connected_count
ORDER BY batches.sequence_number DESC, batches.created_at DESC, batches.batch_id DESC`,
		statusHealthy,
		statusConnected,
		statusCooldown,
		statusHealthyCandidate,
		statusUnprobed,
		statusProbing,
		probeKindInitial,
		statusHealthy,
		statusConnected,
		statusCooldown,
		statusHealthyCandidate,
	)
	if err != nil {
		return nil, fmt.Errorf("list sqlite batches: %w", err)
	}
	defer rows.Close()

	items := make([]ipBatch, 0)
	for rows.Next() {
		var item ipBatch
		if err := rows.Scan(
			&item.ID,
			&item.SequenceNumber,
			&item.CreatedAt,
			&item.DuplicateCount,
			&item.InputErrorCount,
			&item.DeleteNonUS,
			&item.TotalCount,
			&item.CompletedCount,
			&item.PendingCount,
			&item.CandidateCount,
			&item.InitialConnectedCount,
			&item.RealtimeConnectedCount,
		); err != nil {
			return nil, fmt.Errorf("scan sqlite batch: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite batches: %w", err)
	}
	return items, nil
}

func (store *ipStore) listNodesByBatch(batchID string) ([]proxyNode, error) {
	rows, err := store.database.Query(`
SELECT nodes.id, nodes.node_name, nodes.proxy_url, nodes.batch_id, nodes.protocol, nodes.status, nodes.initial_connected,
       nodes.latency_ms, nodes.entered_at, nodes.probe_started_at, nodes.probe_time, nodes.exit_ip, nodes.exit_country,
       nodes.revive_failure_count, nodes.error_reason, nodes.error_detail,
       COALESCE(slots.slot_id, 0), COALESCE(slots.fallback_origin, 0)
FROM ip_nodes AS nodes
LEFT JOIN ip_slots AS slots ON slots.node_id = nodes.id OR slots.claim_node_id = nodes.id
WHERE nodes.batch_id = ? ORDER BY nodes.id DESC`, batchID)
	if err != nil {
		return nil, fmt.Errorf("list sqlite batch nodes: %w", err)
	}
	defer rows.Close()

	items := make([]proxyNode, 0)
	for rows.Next() {
		var node proxyNode
		if err := rows.Scan(
			&node.ID,
			&node.Name,
			&node.ProxyURL,
			&node.BatchID,
			&node.Protocol,
			&node.Status,
			&node.InitialConnected,
			&node.LatencyMs,
			&node.EnteredAt,
			&node.ProbeStartedAt,
			&node.ProbeTime,
			&node.ExitIP,
			&node.ExitCountry,
			&node.ReviveFailureCount,
			&node.ErrorReason,
			&node.ErrorDetail,
			&node.SlotID,
			&node.FallbackOrigin,
		); err != nil {
			return nil, fmt.Errorf("scan sqlite batch node: %w", err)
		}
		node.Name = displayProxyNodeName(node.ProxyURL, node.Name)
		items = append(items, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite batch nodes: %w", err)
	}
	return items, nil
}

func (store *ipStore) deleteErrorNodes(errorReason string) (int64, error) {
	result, err := store.database.Exec(`
DELETE FROM ip_nodes WHERE status = ? AND error_reason = ?`, statusError, errorReason)
	if err != nil {
		return 0, fmt.Errorf("delete sqlite error nodes: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read sqlite delete result: %w", err)
	}
	return deleted, nil
}

func (store *ipStore) batchDeletesNonUS(batchID string) (bool, error) {
	var deleteNonUS int64
	if err := store.database.QueryRow(`SELECT delete_non_us FROM ip_batches WHERE batch_id = ?`, batchID).Scan(&deleteNonUS); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("read sqlite batch delete option: %w", err)
	}
	return deleteNonUS == 1, nil
}

func (store *ipStore) getNode(id int64) (proxyNode, bool, error) {
	var node proxyNode
	err := store.database.QueryRow(`
SELECT nodes.id, nodes.node_name, nodes.proxy_url, nodes.batch_id, nodes.protocol, nodes.status, nodes.initial_connected,
       nodes.latency_ms, nodes.entered_at, nodes.probe_started_at, nodes.probe_time, nodes.exit_ip, nodes.exit_country,
       nodes.revive_failure_count, nodes.error_reason, nodes.error_detail,
       COALESCE(slots.slot_id, 0), COALESCE(slots.fallback_origin, 0)
FROM ip_nodes AS nodes
LEFT JOIN ip_slots AS slots ON slots.node_id = nodes.id OR slots.claim_node_id = nodes.id
WHERE nodes.id = ?`, id).Scan(
		&node.ID,
		&node.Name,
		&node.ProxyURL,
		&node.BatchID,
		&node.Protocol,
		&node.Status,
		&node.InitialConnected,
		&node.LatencyMs,
		&node.EnteredAt,
		&node.ProbeStartedAt,
		&node.ProbeTime,
		&node.ExitIP,
		&node.ExitCountry,
		&node.ReviveFailureCount,
		&node.ErrorReason,
		&node.ErrorDetail,
		&node.SlotID,
		&node.FallbackOrigin,
	)
	if err == sql.ErrNoRows {
		return proxyNode{}, false, nil
	}
	if err != nil {
		return proxyNode{}, false, fmt.Errorf("get sqlite node: %w", err)
	}
	node.Name = displayProxyNodeName(node.ProxyURL, node.Name)
	return node, true, nil
}

func (store *ipStore) claimNext() (*proxyNode, error) {
	tx, err := store.database.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin sqlite claim: %w", err)
	}
	defer tx.Rollback()

	var node proxyNode
	err = tx.QueryRow(`
SELECT id, node_name, proxy_url, host, input_ip, port, protocol, domain, batch_id
FROM ip_nodes WHERE status = ? ORDER BY id ASC LIMIT 1`, statusUnprobed).Scan(
		&node.ID,
		&node.Name,
		&node.ProxyURL,
		&node.Host,
		&node.InputIP,
		&node.Port,
		&node.Protocol,
		&node.Domain,
		&node.BatchID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select sqlite claim: %w", err)
	}
	node.Name = displayProxyNodeName(node.ProxyURL, node.Name)

	startedAt := time.Now().UnixMilli()
	result, err := tx.Exec(`
UPDATE ip_nodes SET status = ?, probe_started_at = ?, probe_kind = ?, probe_return_status = '', keepalive_round_id = 0, error_reason = '', error_detail = ''
WHERE id = ? AND status = ?`, statusProbing, startedAt, probeKindInitial, node.ID, statusUnprobed)
	if err != nil {
		return nil, fmt.Errorf("update sqlite claim: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read sqlite claim result: %w", err)
	}
	if rowsAffected != 1 {
		return nil, fmt.Errorf("sqlite claim lost node %d", node.ID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit sqlite claim: %w", err)
	}
	node.Status = statusProbing
	node.ProbeKind = probeKindInitial
	node.ProbeReturnStatus = ""
	node.ProbeStartedAt = startedAt
	return &node, nil
}

func (store *ipStore) snapshotKeepaliveRound(roundID int64) (int64, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin sqlite keepalive snapshot: %w", err)
	}
	defer transaction.Rollback()

	if _, err := transaction.Exec(`DELETE FROM keepalive_round_nodes WHERE round_id = ?`, roundID); err != nil {
		return 0, fmt.Errorf("clear sqlite keepalive snapshot: %w", err)
	}
	result, err := transaction.Exec(`
INSERT INTO keepalive_round_nodes(round_id, node_id)
SELECT ?, candidates.id
FROM ip_nodes AS candidates
WHERE candidates.initial_connected = 1
  AND candidates.status IN (?, ?, ?, ?)
  AND NOT EXISTS (
      SELECT 1
      FROM ip_nodes AS batch_nodes
      WHERE batch_nodes.batch_id = candidates.batch_id
        AND (batch_nodes.status IN (?, ?) OR batch_nodes.probe_kind = ?)
  )`, roundID, statusHealthy, statusHealthyCandidate, statusConnected, statusCooldown, statusUnprobed, statusProbing, probeKindInitial)
	if err != nil {
		return 0, fmt.Errorf("snapshot sqlite keepalive candidates: %w", err)
	}
	candidateCount, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read sqlite keepalive snapshot result: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return 0, fmt.Errorf("commit sqlite keepalive snapshot: %w", err)
	}
	return candidateCount, nil
}

func (store *ipStore) claimNextKeepalive(roundID int64) (*proxyNode, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin sqlite keepalive claim: %w", err)
	}
	defer transaction.Rollback()

	var node proxyNode
	var previousStatus string
	err = transaction.QueryRow(`
SELECT candidates.node_id, node_name, proxy_url, host, input_ip, port, protocol, domain, batch_id, status
FROM keepalive_round_nodes AS candidates
JOIN ip_nodes ON ip_nodes.id = candidates.node_id
WHERE candidates.round_id = ?
  AND status IN (?, ?, ?, ?)
  AND keepalive_round_id <> ?
ORDER BY node_id ASC LIMIT 1`, roundID, statusHealthy, statusHealthyCandidate, statusConnected, statusCooldown, roundID).Scan(
		&node.ID,
		&node.Name,
		&node.ProxyURL,
		&node.Host,
		&node.InputIP,
		&node.Port,
		&node.Protocol,
		&node.Domain,
		&node.BatchID,
		&previousStatus,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select sqlite keepalive claim: %w", err)
	}
	node.Name = displayProxyNodeName(node.ProxyURL, node.Name)

	startedAt := time.Now().UnixMilli()
	result, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = ?, probe_kind = ?, probe_return_status = ?, keepalive_round_id = ?, error_reason = '', error_detail = ''
WHERE id = ? AND status = ? AND keepalive_round_id <> ?`, statusKeepaliveProbing, startedAt, probeKindKeepalive, previousStatus, roundID, node.ID, previousStatus, roundID)
	if err != nil {
		return nil, fmt.Errorf("update sqlite keepalive claim: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read sqlite keepalive claim result: %w", err)
	}
	if rowsAffected != 1 {
		return nil, fmt.Errorf("sqlite keepalive claim lost node %d", node.ID)
	}
	if err := transaction.Commit(); err != nil {
		return nil, fmt.Errorf("commit sqlite keepalive claim: %w", err)
	}
	node.Status = statusKeepaliveProbing
	node.ProbeKind = probeKindKeepalive
	node.ProbeReturnStatus = previousStatus
	node.KeepaliveRoundID = roundID
	node.ProbeStartedAt = startedAt
	return &node, nil
}

func (store *ipStore) incrementInitialProbeCounters(transaction *sql.Tx, batchID string, connected bool) error {
	query := `
UPDATE ip_batches
SET initial_probe_completed_count = initial_probe_completed_count + 1`
	if connected {
		query += `,
    initial_connected_count = initial_connected_count + 1`
	}
	query += ` WHERE batch_id = ?`
	result, err := transaction.Exec(query, batchID)
	if err != nil {
		return fmt.Errorf("update sqlite initial batch counters: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read sqlite initial batch counter result: %w", err)
	}
	if rowsAffected != 1 {
		return fmt.Errorf("sqlite initial batch counter batch %s not found", batchID)
	}
	return nil
}

func (store *ipStore) completeProbe(node proxyNode, result probeResult) error {
	probeTime := time.Now().UnixMilli()
	if node.ProbeKind == probeKindKeepalive {
		if result.PreserveStatus {
			_, err := store.database.Exec(`
UPDATE ip_nodes SET status = ?, latency_ms = ?, probe_time = ?, probe_started_at = 0,
    probe_kind = '', probe_return_status = '',
    exit_ip = CASE WHEN ? <> '' THEN ? ELSE exit_ip END,
    error_reason = '', error_detail = ?
WHERE id = ? AND status = ? AND probe_kind = ? AND keepalive_round_id = ?`, node.ProbeReturnStatus, result.LatencyMs, probeTime, result.ExitIP, result.ExitIP, result.Detail, node.ID, statusKeepaliveProbing, probeKindKeepalive, node.KeepaliveRoundID)
			if err != nil {
				return fmt.Errorf("save unconfirmed keepalive probe: %w", err)
			}
			_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(node.KeepaliveRoundID), logStatusProbing, logLevelWarn, "keepalive.unconfirmed", node.ID, node.Name, "保活探测暂不可判定，节点保持原状态", formatProbeResultDetail(result))
			return nil
		}
		if result.Success {
			_, err := store.database.Exec(`
UPDATE ip_nodes SET status = ?, latency_ms = ?, probe_time = ?, probe_started_at = 0,
    probe_kind = '', probe_return_status = '', exit_ip = CASE WHEN ? <> '' THEN ? ELSE exit_ip END, error_reason = '', error_detail = ?
WHERE id = ? AND status = ? AND probe_kind = ? AND keepalive_round_id = ?`, node.ProbeReturnStatus, result.LatencyMs, probeTime, result.ExitIP, result.ExitIP, result.Detail, node.ID, statusKeepaliveProbing, probeKindKeepalive, node.KeepaliveRoundID)
			if err != nil {
				return fmt.Errorf("save keepalive success: %w", err)
			}
			_ = store.appendProbeLog(logCategoryKeepaliveProbe, keepaliveGroupID(node.KeepaliveRoundID), logStatusConnected, logLevelInfo, "keepalive.completed", node.ID, node.Name, "节点保活探测成功，状态保持不变", formatProbeResultDetail(result))
			return nil
		}
		return store.completeKeepaliveFailure(node, result)
	}
	deleteNonUS, err := store.batchDeletesNonUS(node.BatchID)
	if err != nil {
		return err
	}
	if !result.Success && deleteNonUS && result.Reason == "非us出口" {
		transaction, err := store.database.Begin()
		if err != nil {
			return fmt.Errorf("begin non-US sqlite node deletion: %w", err)
		}
		defer transaction.Rollback()
		deleteResult, err := transaction.Exec(`
DELETE FROM ip_nodes WHERE id = ? AND status = ? AND probe_kind = ?`, node.ID, statusProbing, probeKindInitial)
		if err != nil {
			return fmt.Errorf("delete non-US sqlite node: %w", err)
		}
		deletedRows, err := deleteResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("read non-US sqlite delete result: %w", err)
		}
		if deletedRows != 1 {
			return fmt.Errorf("non-US sqlite node %d was not deleted", node.ID)
		}
		if err := store.incrementInitialProbeCounters(transaction, node.BatchID, false); err != nil {
			return err
		}
		if err := transaction.Commit(); err != nil {
			return fmt.Errorf("commit non-US sqlite node deletion: %w", err)
		}
		_ = store.appendProbeLog(logCategoryBatchProbe, node.BatchID, logStatusError, logLevelWarn, "probe.deleted_non_us", node.ID, node.Name, "节点出口非 US，已按导入选项删除", formatProbeResultDetail(result))
		return nil
	}
	if result.Success {
		transaction, err := store.database.Begin()
		if err != nil {
			return fmt.Errorf("begin connected sqlite probe: %w", err)
		}
		defer transaction.Rollback()
		resultUpdate, err := transaction.Exec(`
UPDATE ip_nodes SET status = ?, initial_connected = 1, latency_ms = ?, probe_time = ?, probe_started_at = 0,
    probe_kind = '', probe_return_status = '', exit_ip = ?, exit_country = ?, revive_failure_count = 0, error_reason = '', error_detail = ?
WHERE id = ? AND status = ? AND probe_kind = ?`, statusConnected, result.LatencyMs, probeTime, result.ExitIP, result.CountryCode, result.Detail, node.ID, statusProbing, probeKindInitial)
		if err != nil {
			return fmt.Errorf("save connected probe: %w", err)
		}
		rowsAffected, err := resultUpdate.RowsAffected()
		if err != nil {
			return fmt.Errorf("read connected probe result: %w", err)
		}
		if rowsAffected != 1 {
			return fmt.Errorf("connected sqlite probe %d was not updated", node.ID)
		}
		if err := store.incrementInitialProbeCounters(transaction, node.BatchID, true); err != nil {
			return err
		}
		if err := transaction.Commit(); err != nil {
			return fmt.Errorf("commit connected sqlite probe: %w", err)
		}
		_ = store.appendProbeLog(logCategoryBatchProbe, node.BatchID, logStatusConnected, logLevelInfo, "probe.completed", node.ID, node.Name, "节点探测成功", formatProbeResultDetail(result))
		return nil
	}
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin failed sqlite probe: %w", err)
	}
	defer transaction.Rollback()
	resultUpdate, err := transaction.Exec(`
UPDATE ip_nodes SET status = ?, initial_connected = 0, latency_ms = ?, probe_time = ?, probe_started_at = 0,
    probe_kind = '', probe_return_status = '', exit_ip = ?, exit_country = ?, revive_failure_count = 0, error_reason = ?, error_detail = ?
WHERE id = ? AND status = ? AND probe_kind = ?`, statusError, result.LatencyMs, probeTime, result.ExitIP, result.CountryCode, result.Reason, result.Detail, node.ID, statusProbing, probeKindInitial)
	if err != nil {
		return fmt.Errorf("save failed probe: %w", err)
	}
	rowsAffected, err := resultUpdate.RowsAffected()
	if err != nil {
		return fmt.Errorf("read failed probe result: %w", err)
	}
	if rowsAffected != 1 {
		return fmt.Errorf("failed sqlite probe %d was not updated", node.ID)
	}
	if err := store.incrementInitialProbeCounters(transaction, node.BatchID, false); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit failed sqlite probe: %w", err)
	}
	_ = store.appendProbeLog(
		logCategoryBatchProbe,
		node.BatchID,
		logStatusError,
		logLevelWarn,
		"probe.failed",
		node.ID,
		node.Name,
		fmt.Sprintf("节点探测失败：%s", result.Reason),
		formatProbeResultDetail(result),
	)
	return nil
}

func (store *ipStore) resetProbe(node proxyNode) error {
	if node.ProbeKind == probeKindKeepalive {
		_, err := store.database.Exec(`
UPDATE ip_nodes SET status = ?, probe_started_at = 0, probe_kind = '', probe_return_status = '', keepalive_round_id = 0
WHERE id = ? AND status = ? AND probe_kind = ? AND keepalive_round_id = ?`, node.ProbeReturnStatus, node.ID, statusKeepaliveProbing, node.ProbeKind, node.KeepaliveRoundID)
		if err != nil {
			return fmt.Errorf("reset interrupted %s probe: %w", node.ProbeKind, err)
		}
		return nil
	}
	_, err := store.database.Exec(`
UPDATE ip_nodes SET status = ?, probe_started_at = 0, probe_kind = '', probe_return_status = ''
WHERE id = ? AND status = ? AND probe_kind = ?`, statusUnprobed, node.ID, statusProbing, probeKindInitial)
	if err != nil {
		return fmt.Errorf("reset interrupted probe: %w", err)
	}
	return nil
}

func (store *ipStore) summary() (map[string]any, error) {
	counts := map[string]any{
		statusHealthy:          int64(0),
		statusHealthyCandidate: int64(0),
		statusHealthyFallback:  int64(0),
		statusConnected:        int64(0),
		statusCooldown:         int64(0),
		statusProbing:          int64(0),
		statusKeepaliveProbing: int64(0),
		statusQualityProbing:   int64(0),
		statusReviveProbing:    int64(0),
		statusUnprobed:         int64(0),
		statusError:            int64(0),
	}
	rows, err := store.database.Query(`SELECT status, COUNT(*) FROM ip_nodes GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("summarize sqlite nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scan sqlite summary: %w", err)
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite summary: %w", err)
	}

	slotRows, err := store.database.Query(`
SELECT slot_id, slot_kind, node_id
FROM ip_slots
WHERE slot_kind IN (?, ?)
ORDER BY slot_id ASC`, statusHealthy, statusHealthyCandidate)
	if err != nil {
		return nil, fmt.Errorf("list sqlite summary slots: %w", err)
	}
	defer slotRows.Close()
	healthyEmptySlotIDs := make([]int64, 0)
	healthyCandidateEmptySlotIDs := make([]int64, 0)
	var healthySlotCount, healthyCandidateSlotCount int64
	var healthyOccupiedSlotCount, healthyCandidateOccupiedSlotCount int64
	for slotRows.Next() {
		var slotID, nodeID int64
		var slotKind string
		if err := slotRows.Scan(&slotID, &slotKind, &nodeID); err != nil {
			return nil, fmt.Errorf("scan sqlite summary slot: %w", err)
		}
		if slotKind == statusHealthy {
			healthySlotCount++
			if nodeID == 0 {
				healthyEmptySlotIDs = append(healthyEmptySlotIDs, slotID)
			} else {
				healthyOccupiedSlotCount++
			}
			continue
		}
		healthyCandidateSlotCount++
		if nodeID == 0 {
			healthyCandidateEmptySlotIDs = append(healthyCandidateEmptySlotIDs, slotID)
		} else {
			healthyCandidateOccupiedSlotCount++
		}
	}
	if err := slotRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite summary slots: %w", err)
	}
	counts["healthySlotCount"] = healthySlotCount
	counts["healthyOccupiedSlotCount"] = healthyOccupiedSlotCount
	counts["healthyEmptySlotIds"] = healthyEmptySlotIDs
	counts["healthyCandidateSlotCount"] = healthyCandidateSlotCount
	counts["healthyCandidateOccupiedSlotCount"] = healthyCandidateOccupiedSlotCount
	counts["healthyCandidateEmptySlotIds"] = healthyCandidateEmptySlotIDs
	return counts, nil
}

func (store *ipStore) settings() (pluginSettings, error) {
	settings := defaultPluginSettings()
	rows, err := store.database.Query(`
SELECT setting_key, setting_value
FROM plugin_settings
WHERE setting_key IN (
    'worker_count', 'refresh_interval_seconds', 'keepalive_worker_count', 'keepalive_interval_seconds',
    'revive_interval_seconds', 'probe_retry_count', 'healthy_slot_count', 'healthy_candidate_slot_count',
    'quality_worker_count', 'quality_probe_timeout_seconds', 'quality_soft_tps', 'quality_hard_tps',
    'grok2api_sync_enabled', 'grok2api_base_url', 'grok2api_admin_username', 'grok2api_admin_password'
)`)
	if err != nil {
		return pluginSettings{}, fmt.Errorf("read plugin settings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var settingKey, rawValue string
		if err := rows.Scan(&settingKey, &rawValue); err != nil {
			return pluginSettings{}, fmt.Errorf("scan plugin settings: %w", err)
		}
		switch settingKey {
		case "quality_soft_tps", "quality_hard_tps":
			value, parseErr := strconv.ParseFloat(strings.TrimSpace(rawValue), 64)
			if parseErr != nil {
				continue
			}
			if settingKey == "quality_soft_tps" {
				settings.QualitySoftTPS = value
			} else {
				settings.QualityHardTPS = value
			}
		case "grok2api_sync_enabled":
			settings.Grok2apiSyncEnabled = parseSettingBool(rawValue)
		case "grok2api_base_url":
			settings.Grok2apiBaseUrl = normalizeGrok2apiBaseURL(rawValue)
		case "grok2api_admin_username":
			settings.Grok2apiAdminUsername = strings.TrimSpace(rawValue)
		case "grok2api_admin_password":
			settings.Grok2apiAdminPassword = rawValue
		default:
			value, parseErr := strconv.Atoi(strings.TrimSpace(rawValue))
			if parseErr != nil {
				continue
			}
			switch settingKey {
			case "worker_count":
				settings.WorkerCount = value
			case "refresh_interval_seconds":
				settings.RefreshIntervalSeconds = value
			case "keepalive_worker_count":
				settings.KeepaliveWorkerCount = value
			case "keepalive_interval_seconds":
				settings.KeepaliveIntervalSeconds = value
			case "revive_interval_seconds":
				settings.ReviveIntervalSeconds = value
			case "probe_retry_count":
				settings.ProbeRetryCount = value
			case "healthy_slot_count":
				settings.HealthySlotCount = value
			case "healthy_candidate_slot_count":
				settings.HealthyCandidateSlotCount = value
			case "quality_worker_count":
				settings.QualityWorkerCount = value
			case "quality_probe_timeout_seconds":
				settings.QualityProbeTimeoutSeconds = value
			}
		}
	}
	if err := rows.Err(); err != nil {
		return pluginSettings{}, fmt.Errorf("iterate plugin settings: %w", err)
	}
	if err := rows.Close(); err != nil {
		return pluginSettings{}, fmt.Errorf("close plugin settings: %w", err)
	}
	if err := validatePluginSettings(settings); err != nil {
		settings = defaultPluginSettings()
		if err := store.setSettings(settings); err != nil {
			return pluginSettings{}, err
		}
	}
	return settings, nil
}

func (store *ipStore) setSettings(settings pluginSettings) error {
	if err := validatePluginSettings(settings); err != nil {
		return err
	}
	transaction, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin settings update: %w", err)
	}
	defer transaction.Rollback()

	settingsToSave := map[string]string{
		"worker_count":                  strconv.Itoa(settings.WorkerCount),
		"refresh_interval_seconds":      strconv.Itoa(settings.RefreshIntervalSeconds),
		"keepalive_worker_count":        strconv.Itoa(settings.KeepaliveWorkerCount),
		"keepalive_interval_seconds":    strconv.Itoa(settings.KeepaliveIntervalSeconds),
		"revive_interval_seconds":       strconv.Itoa(settings.ReviveIntervalSeconds),
		"probe_retry_count":             strconv.Itoa(settings.ProbeRetryCount),
		"healthy_slot_count":            strconv.Itoa(settings.HealthySlotCount),
		"healthy_candidate_slot_count":  strconv.Itoa(settings.HealthyCandidateSlotCount),
		"quality_worker_count":          strconv.Itoa(settings.QualityWorkerCount),
		"quality_probe_timeout_seconds": strconv.Itoa(settings.QualityProbeTimeoutSeconds),
		"quality_soft_tps":              strconv.FormatFloat(settings.QualitySoftTPS, 'f', -1, 64),
		"quality_hard_tps":              strconv.FormatFloat(settings.QualityHardTPS, 'f', -1, 64),
		"grok2api_sync_enabled":         formatSettingBool(settings.Grok2apiSyncEnabled),
		"grok2api_base_url":             normalizeGrok2apiBaseURL(settings.Grok2apiBaseUrl),
		"grok2api_admin_username":       strings.TrimSpace(settings.Grok2apiAdminUsername),
		"grok2api_admin_password":       settings.Grok2apiAdminPassword,
	}
	for settingKey, settingValue := range settingsToSave {
		if _, err := transaction.Exec(`
INSERT INTO plugin_settings(setting_key, setting_value) VALUES (?, ?)
ON CONFLICT(setting_key) DO UPDATE SET setting_value = excluded.setting_value`, settingKey, settingValue); err != nil {
			return fmt.Errorf("save plugin setting %s: %w", settingKey, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit settings update: %w", err)
	}
	return nil
}

func validatePluginSettings(settings pluginSettings) error {
	if settings.WorkerCount < 1 || settings.WorkerCount > maxProbeWorkers {
		return fmt.Errorf("worker count must be between 1 and %d", maxProbeWorkers)
	}
	if settings.RefreshIntervalSeconds < 5 || settings.RefreshIntervalSeconds > maxRefreshIntervalSeconds {
		return fmt.Errorf("refresh interval must be between 5 and %d seconds", maxRefreshIntervalSeconds)
	}
	if settings.KeepaliveWorkerCount < 1 || settings.KeepaliveWorkerCount > maxProbeWorkers {
		return fmt.Errorf("keepalive worker count must be between 1 and %d", maxProbeWorkers)
	}
	if settings.KeepaliveIntervalSeconds < 1 || settings.KeepaliveIntervalSeconds > maxKeepaliveIntervalSeconds {
		return fmt.Errorf("keepalive interval must be between 1 and %d seconds", maxKeepaliveIntervalSeconds)
	}
	if settings.ReviveIntervalSeconds < 1 || settings.ReviveIntervalSeconds > maxKeepaliveIntervalSeconds {
		return fmt.Errorf("revive interval must be between 1 and %d seconds", maxKeepaliveIntervalSeconds)
	}
	if settings.ProbeRetryCount < 1 || settings.ProbeRetryCount > maxProbeRetryCount {
		return fmt.Errorf("probe retry count must be between 1 and %d", maxProbeRetryCount)
	}
	return validateSlotSettings(settings)
}
