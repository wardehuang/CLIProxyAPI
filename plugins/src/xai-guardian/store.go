package main

import (
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	defaultDatabasePath                                 = "/opt/cli-proxy-api/plugin-data/xai-guardian/xai-guardian.sqlite3"
	defaultInspectionIntervalSeconds                    = 0
	defaultWorkerCount                                  = 4
	defaultScheduleGroupCount                           = 4
	defaultDebugEnabled                                 = false
	defaultRefreshIntervalSeconds                       = 30
	defaultKeepaliveWorkerCount                         = 8
	defaultKeepaliveIntervalSeconds                     = 1800
	defaultReviveIntervalSeconds                        = 1800
	defaultProbeRetryCount                              = 3
	defaultHealthySlotCount                             = 50
	defaultHealthyCandidateCount                        = 20
	defaultHealthySlotMaxAgeMinutes                     = 350
	defaultRealtimeGuardTTFBSeconds                     = 5.0
	defaultRealtimeGuardGenerationSeconds               = 1.25
	defaultRealtimeGuardTokenThreshold                  = 300
	defaultRealtimeGuardTimeoutSeconds                  = 120
	defaultRealtimeGuardIdleTimeoutSeconds              = 500
	defaultIPBatchRetentionDays                         = 6
	defaultRealtimeGuardMinSummaryChars                 = 32
	defaultRealtimeGuardMinEncryptedBytes               = 256
	defaultRealtimeGuardEncryptedBytesPerReasoningToken = 4
	defaultRealtimeGuardMinOutputTokens                 = 8
	defaultRealtimeGuardBurstMinReasoningTokens         = 80
	defaultRealtimeGuardBurstMaxVisibleTokens           = 32
	defaultRealtimeGuardBurstMaxWindowMS                = 1000
	maxInspectionIntervalSeconds                        = 86400
	maxProbeWorkers                                     = 64
	maxScheduleGroupCount                               = 1000
	maxRefreshIntervalSeconds                           = 3600
	maxKeepaliveIntervalSeconds                         = 86400
	maxReviveIntervalSeconds                            = 86400
	maxKeepaliveWorkerCount                             = 64
	maxKeepaliveProbeRetryCount                         = 10
	maxSlotCount                                        = 1000
	maxHealthySlotMaxAgeMinutes                         = 10080
	degradationFirstCooling                             = 24 * time.Hour
	degradationSecondCooling                            = 48 * time.Hour
	degradationPermanentCoolingUntil                    = int64(-1)
	maxIPBatches                                        = 5

	statusUninspected      = "uninspected"
	statusInspecting       = "inspecting"
	statusKeepaliveProbing = "keepalive_probing"
	statusProbing          = "probing"
	statusReviveProbing    = "revive_probing"
	statusHealthyCandidate = "healthy_candidate"
	statusHealthyFallback  = "healthy_fallback"
	statusCooldown         = "cooldown"
	statusHealthy          = "healthy"
	statusConnected        = "connected"
	statusUnhealthy        = "unhealthy"
	statusDisabled         = "disabled"

	logLevelInfo  = "info"
	logLevelWarn  = "warn"
	logLevelError = "error"

	logCategoryRealtimeGuard = "realtime_guard"
	logCategoryGeneral       = "general"
	logCategoryBatchProbe    = "batch_probe"
	logCategoryKeepalive     = "keepalive_probe"
	logCategoryRevive        = "revive_probe"

	logStatusConnected = "connected"
	logStatusProbing   = "probing"
	logStatusError     = "error"

	maxRealtimeGuardLogs = 100
	maxPluginLogs        = 1000
	maxGroupedLogSets    = 10
)

var logURLPattern = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)

type pluginSettings struct {
	WorkerCount                                  int
	ScheduleGroupCount                           int
	DebugEnabled                                 bool
	RefreshIntervalSeconds                       int
	InspectionIntervalSeconds                    int
	KeepaliveWorkerCount                         int
	KeepaliveIntervalSeconds                     int
	ReviveIntervalSeconds                        int
	ProbeRetryCount                              int
	HealthySlotCount                             int
	HealthyCandidateSlotCount                    int
	HealthySlotMaxAgeMinutes                     int
	RealtimeGuardTTFBSeconds                     float64
	RealtimeGuardGenerationSeconds               float64
	RealtimeGuardTokenThreshold                  int
	RealtimeGuardTimeoutSeconds                  int
	RealtimeGuardIdleTimeoutSeconds              int
	RealtimeGuardMinSummaryChars                 int
	RealtimeGuardMinEncryptedBytes               int
	RealtimeGuardEncryptedBytesPerReasoningToken int
	RealtimeGuardMinOutputTokens                 int
	RealtimeGuardBurstMinReasoningTokens         int
	RealtimeGuardBurstMaxVisibleTokens           int
	RealtimeGuardBurstMaxWindowMS                int
	IPBatchRetentionDays                         int
}

type proxyNode struct {
	ID          int64
	Address     string
	Protocol    string
	Host        string
	Port        int
	Status      string
	LatencyMS   int64
	ExitIP      string
	Country     string
	LastChecked int64
	LastError   string
	CreatedAt   int64
	SlotID      int64
	SlotKind    string
}

type inspectionRun struct {
	ID          int64  `json:"id"`
	StartedAt   int64  `json:"startedAt"`
	CompletedAt int64  `json:"completedAt"`
	Status      string `json:"status"`
	Total       int64  `json:"total"`
	Healthy     int64  `json:"healthy"`
	Unhealthy   int64  `json:"unhealthy"`
}

type keepaliveRound struct {
	ID             int64  `json:"id"`
	StartedAt      int64  `json:"startedAt"`
	CompletedAt    int64  `json:"completedAt"`
	Status         string `json:"status"`
	CandidateCount int64  `json:"candidateCount"`
	SuccessCount   int64  `json:"successCount"`
	FailureCount   int64  `json:"failureCount"`
	DeletedBatches int64  `json:"deletedBatches"`
	DeletedNodes   int64  `json:"deletedNodes"`
}

type nodeProbeResult struct {
	Status    string
	LatencyMS int64
	ExitIP    string
	Country   string
	Error     string
	CheckedAt int64
}

type keepaliveNodeClaim struct {
	Node           proxyNode
	PreviousStatus string
}

type inspectionResult struct {
	ID        int64  `json:"id"`
	RunID     int64  `json:"runId"`
	NodeID    int64  `json:"nodeId"`
	Status    string `json:"status"`
	LatencyMS int64  `json:"latencyMs"`
	ExitIP    string `json:"exitIp"`
	Country   string `json:"country"`
	Error     string `json:"error"`
	CheckedAt int64  `json:"checkedAt"`
}

type ipBatch struct {
	ID                     string `json:"id"`
	SequenceNumber         int64  `json:"sequenceNumber"`
	CreatedAt              int64  `json:"createdAt"`
	ExpiresAt              int64  `json:"expiresAt"`
	TotalCount             int64  `json:"totalCount"`
	DuplicateCount         int64  `json:"duplicateCount"`
	InputErrorCount        int64  `json:"inputErrorCount"`
	CompletedCount         int64  `json:"completedCount"`
	InitialConnectedCount  int64  `json:"initialConnectedCount"`
	RealtimeConnectedCount int64  `json:"realtimeConnectedCount"`
}

type authBinding struct {
	AuthIndex       string `json:"authIndex"`
	AuthName        string `json:"authName"`
	NodeID          int64  `json:"nodeId"`
	ProxyURL        string `json:"proxyUrl"`
	ExitIP          string `json:"exitIp"`
	Status          string `json:"status"`
	Priority        int    `json:"priority"`
	Success         int64  `json:"success"`
	Failed          int64  `json:"failed"`
	UpdatedAt       int64  `json:"updatedAt"`
	LastChecked     int64  `json:"lastChecked"`
	InspectionRunID int64  `json:"-"`
	AccountType     string `json:"accountType"`
	ScheduleGroup   *int   `json:"scheduleGroup,omitempty"`
	LastInspection  int64  `json:"lastInspection"`
}

type degradationState struct {
	AuthIndex    string `json:"authIndex"`
	AuthName     string `json:"authName"`
	Count        int    `json:"count"`
	LastReason   string `json:"lastReason"`
	LastRequest  string `json:"lastRequestId"`
	LastSeen     int64  `json:"lastSeen"`
	CoolingUntil int64  `json:"coolingUntil"`
}

type pluginLog struct {
	ID        int64  `json:"id"`
	CreatedAt int64  `json:"createdAt"`
	Level     string `json:"level"`
	Event     string `json:"event"`
	Message   string `json:"message"`
	Detail    string `json:"detail"`
	Category  string `json:"category"`
	GroupID   string `json:"groupId"`
	Status    string `json:"status"`
	NodeID    int64  `json:"nodeId"`
	NodeName  string `json:"nodeName"`
}

type pluginLogGroup struct {
	ID             string `json:"id"`
	SequenceNumber int64  `json:"sequenceNumber"`
	Category       string `json:"category"`
	Status         string `json:"status"`
	StartedAt      int64  `json:"startedAt"`
	CompletedAt    int64  `json:"completedAt"`
	UpdatedAt      int64  `json:"updatedAt"`
	LogCount       int64  `json:"logCount"`
	CandidateCount int64  `json:"candidateCount"`
	SuccessCount   int64  `json:"successCount"`
	FailureCount   int64  `json:"failureCount"`
}

type pluginLogList struct {
	Items    []pluginLog `json:"items"`
	Total    int         `json:"total"`
	Max      int         `json:"max"`
	Search   string      `json:"search"`
	Category string      `json:"category"`
	GroupID  string      `json:"groupId"`
	Status   string      `json:"status"`
}

type guardianStore struct {
	database *sql.DB
	path     string
	mutex    sync.Mutex
}

func openGuardianStore(path string) (*guardianStore, error) {
	var err error
	path, err = normalizedDatabasePath(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	database, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	store := &guardianStore{database: database, path: path}
	if err := store.initialize(); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("protect sqlite database: %w", err)
	}
	return store, nil
}

func normalizedDatabasePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultDatabasePath
	}
	if !filepath.IsAbs(path) {
		workingDirectory, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve database path: %w", err)
		}
		path = filepath.Join(workingDirectory, path)
	}
	return filepath.Clean(path), nil
}

func (store *guardianStore) initialize() error {
	_, err := store.database.Exec(`
CREATE TABLE IF NOT EXISTS plugin_settings (
    setting_key TEXT PRIMARY KEY,
    setting_value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS nodes (
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
);
CREATE INDEX IF NOT EXISTS idx_nodes_status ON nodes(status, id);
CREATE TABLE IF NOT EXISTS ip_batches (
    batch_id TEXT PRIMARY KEY,
    sequence_number INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    total_count INTEGER NOT NULL DEFAULT 0,
    duplicate_count INTEGER NOT NULL DEFAULT 0,
    input_error_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ip_batches_created_at ON ip_batches(created_at DESC);
CREATE TABLE IF NOT EXISTS ip_batch_nodes (
    batch_id TEXT NOT NULL,
    node_id INTEGER NOT NULL,
    PRIMARY KEY(batch_id, node_id),
    FOREIGN KEY(batch_id) REFERENCES ip_batches(batch_id) ON DELETE CASCADE,
    FOREIGN KEY(node_id) REFERENCES nodes(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_ip_batch_nodes_node_id ON ip_batch_nodes(node_id);
CREATE TABLE IF NOT EXISTS inspection_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    total INTEGER NOT NULL DEFAULT 0,
    healthy INTEGER NOT NULL DEFAULT 0,
    unhealthy INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS inspection_results (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL,
    node_id INTEGER NOT NULL,
    status TEXT NOT NULL,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    exit_ip TEXT NOT NULL DEFAULT '',
    country TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    checked_at INTEGER NOT NULL,
    FOREIGN KEY(run_id) REFERENCES inspection_runs(id) ON DELETE CASCADE,
    FOREIGN KEY(node_id) REFERENCES nodes(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_inspection_results_run ON inspection_results(run_id, id);
CREATE TABLE IF NOT EXISTS keepalive_rounds (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    candidate_count INTEGER NOT NULL DEFAULT 0,
    success_count INTEGER NOT NULL DEFAULT 0,
    failure_count INTEGER NOT NULL DEFAULT 0,
    deleted_batches INTEGER NOT NULL DEFAULT 0,
    deleted_nodes INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS keepalive_round_nodes (
    round_id INTEGER NOT NULL,
    node_id INTEGER NOT NULL,
    previous_status TEXT NOT NULL DEFAULT '',
    completed_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(round_id, node_id),
    FOREIGN KEY(round_id) REFERENCES keepalive_rounds(id) ON DELETE CASCADE,
    FOREIGN KEY(node_id) REFERENCES nodes(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_keepalive_round_nodes_node ON keepalive_round_nodes(node_id);
CREATE TABLE IF NOT EXISTS probe_rounds (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    candidate_count INTEGER NOT NULL DEFAULT 0,
    success_count INTEGER NOT NULL DEFAULT 0,
    failure_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS probe_round_nodes (
    round_id INTEGER NOT NULL,
    node_id INTEGER NOT NULL,
    previous_status TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(round_id, node_id),
    FOREIGN KEY(round_id) REFERENCES probe_rounds(id) ON DELETE CASCADE,
    FOREIGN KEY(node_id) REFERENCES nodes(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_probe_round_nodes_node ON probe_round_nodes(node_id);
CREATE TABLE IF NOT EXISTS revive_rounds (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    candidate_count INTEGER NOT NULL DEFAULT 0,
    success_count INTEGER NOT NULL DEFAULT 0,
    failure_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS revive_round_nodes (
    round_id INTEGER NOT NULL,
    node_id INTEGER NOT NULL,
    previous_status TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(round_id, node_id),
    FOREIGN KEY(round_id) REFERENCES revive_rounds(id) ON DELETE CASCADE,
    FOREIGN KEY(node_id) REFERENCES nodes(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_revive_round_nodes_node ON revive_round_nodes(node_id);
CREATE TABLE IF NOT EXISTS healthy_slots (
    slot_id INTEGER PRIMARY KEY,
    slot_kind TEXT NOT NULL,
    node_id INTEGER NOT NULL DEFAULT 0,
    refreshed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_healthy_slots_node ON healthy_slots(node_id);
CREATE TABLE IF NOT EXISTS auth_bindings (
    auth_index TEXT PRIMARY KEY,
    auth_name TEXT NOT NULL,
    node_id INTEGER NOT NULL DEFAULT 0,
    proxy_url TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    success_count INTEGER NOT NULL DEFAULT 0,
    failed_count INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    last_checked INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_auth_bindings_node ON auth_bindings(node_id);
CREATE TABLE IF NOT EXISTS degradation_states (
    auth_index TEXT PRIMARY KEY,
    auth_name TEXT NOT NULL,
    count INTEGER NOT NULL DEFAULT 0,
    last_reason TEXT NOT NULL DEFAULT '',
    last_request_id TEXT NOT NULL DEFAULT '',
    last_seen INTEGER NOT NULL DEFAULT 0,
    cooling_until INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS plugin_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at INTEGER NOT NULL,
    level TEXT NOT NULL,
    event TEXT NOT NULL,
    message TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    category TEXT NOT NULL DEFAULT 'general',
    group_id TEXT NOT NULL DEFAULT '',
    log_status TEXT NOT NULL DEFAULT '',
    node_id INTEGER NOT NULL DEFAULT 0,
    node_name TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_plugin_logs_created ON plugin_logs(created_at DESC, id DESC);
INSERT OR IGNORE INTO plugin_settings(setting_key, setting_value) VALUES
    ('inspection_interval_seconds', '0'),
    ('worker_count', '4'),
    ('schedule_group_count', '4'),
    ('debug_enabled', '0'),
    ('refresh_interval_seconds', '30'),
    ('keepalive_worker_count', '8'),
    ('keepalive_interval_seconds', '1800'),
    ('revive_interval_seconds', '1800'),
    ('probe_retry_count', '3'),
    ('healthy_slot_count', '50'),
    ('healthy_candidate_slot_count', '20'),
    ('healthy_slot_max_age_minutes', '350'),
    ('realtime_guard_ttfb_seconds', '5'),
    ('realtime_guard_generation_seconds', '1.25'),
    ('realtime_guard_token_threshold', '300'),
    ('realtime_guard_timeout_seconds', '120'),
    ('realtime_guard_idle_timeout_seconds', '500'),
    ('realtime_guard_min_summary_chars', '32'),
    ('realtime_guard_min_encrypted_bytes', '256'),
    ('realtime_guard_encrypted_bytes_per_reasoning_token', '4'),
    ('realtime_guard_min_output_tokens', '8'),
    ('realtime_guard_burst_min_reasoning_tokens', '80'),
    ('realtime_guard_burst_max_visible_tokens', '32'),
    ('realtime_guard_burst_max_window_ms', '1000'),
    ('ip_batch_retention_days', '6');
`)
	if err != nil {
		return fmt.Errorf("initialize sqlite database: %w", err)
	}
	if _, err := store.database.Exec(`DELETE FROM plugin_settings WHERE setting_key IN ('quality_worker_count', 'quality_probe_timeout_seconds', 'quality_probe_model', 'quality_soft_tps', 'quality_hard_tps', 'quality_llm_probe_enabled')`); err != nil {
		return fmt.Errorf("remove obsolete quality settings: %w", err)
	}
	if err := store.ensurePluginLogColumns(); err != nil {
		return err
	}
	if _, err := store.database.Exec(`
INSERT INTO plugin_settings(setting_key, setting_value)
SELECT 'probe_retry_count', setting_value FROM plugin_settings
WHERE setting_key = 'keepalive_probe_retry_count'
  AND NOT EXISTS (SELECT 1 FROM plugin_settings WHERE setting_key = 'probe_retry_count')`); err != nil {
		return fmt.Errorf("migrate probe retry setting: %w", err)
	}
	if err := store.ensureAuthBindingColumns(); err != nil {
		return err
	}
	if err := store.ensureNodeScopeColumn(); err != nil {
		return err
	}
	if err := store.ensureNodeScopeUniqueIndex(); err != nil {
		return err
	}
	if err := store.ensureKeepaliveRoundNodeColumns(); err != nil {
		return err
	}
	if err := store.ensureScheduleGroupStorage(); err != nil {
		return err
	}
	if err := store.reconcileScheduleGroupCounters(defaultScheduleGroupCount); err != nil {
		return err
	}
	if err := store.recoverKeepaliveState(); err != nil {
		return err
	}
	if err := store.recoverProbeAndReviveState(); err != nil {
		return err
	}
	return nil
}

func (store *guardianStore) ensurePluginLogColumns() error {
	rows, err := store.database.Query(`PRAGMA table_info(plugin_logs)`)
	if err != nil {
		return fmt.Errorf("inspect plugin log schema: %w", err)
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var columnID, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&columnID, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan plugin log schema: %w", err)
		}
		columns[columnName] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate plugin log schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close plugin log schema: %w", err)
	}
	for _, column := range []struct {
		name string
		sql  string
	}{
		{name: "category", sql: `ALTER TABLE plugin_logs ADD COLUMN category TEXT NOT NULL DEFAULT 'general'`},
		{name: "group_id", sql: `ALTER TABLE plugin_logs ADD COLUMN group_id TEXT NOT NULL DEFAULT ''`},
		{name: "log_status", sql: `ALTER TABLE plugin_logs ADD COLUMN log_status TEXT NOT NULL DEFAULT ''`},
		{name: "node_id", sql: `ALTER TABLE plugin_logs ADD COLUMN node_id INTEGER NOT NULL DEFAULT 0`},
		{name: "node_name", sql: `ALTER TABLE plugin_logs ADD COLUMN node_name TEXT NOT NULL DEFAULT ''`},
	} {
		if columns[column.name] {
			continue
		}
		if _, err := store.database.Exec(column.sql); err != nil {
			return fmt.Errorf("add plugin log column %s: %w", column.name, err)
		}
	}
	if _, err := store.database.Exec(`
UPDATE plugin_logs
SET category = CASE
    WHEN event LIKE 'guard.%' THEN 'realtime_guard'
    WHEN event LIKE 'probe.%' AND event NOT IN ('probe.scheduler_started', 'probe.round_failed') THEN 'batch_probe'
    WHEN event LIKE 'keepalive.%' AND event NOT IN ('keepalive.scheduler_started', 'keepalive.manual_triggered', 'keepalive.round_failed') THEN 'keepalive_probe'
    WHEN event LIKE 'revive.%' AND event NOT IN ('revive.scheduler_started', 'revive.round_failed') THEN 'revive_probe'
    ELSE 'general'
END,
log_status = CASE
    WHEN level = 'error' OR event LIKE '%failed%' THEN 'error'
    WHEN event LIKE '%completed%' THEN 'connected'
    WHEN event LIKE '%started%' OR event LIKE '%triggered%' THEN 'probing'
    ELSE ''
END`); err != nil {
		return fmt.Errorf("classify existing plugin logs: %w", err)
	}
	if _, err := store.database.Exec(`CREATE INDEX IF NOT EXISTS idx_plugin_logs_category ON plugin_logs(category, id DESC)`); err != nil {
		return fmt.Errorf("index plugin log category: %w", err)
	}
	if _, err := store.database.Exec(`CREATE INDEX IF NOT EXISTS idx_plugin_logs_group ON plugin_logs(category, group_id, id DESC)`); err != nil {
		return fmt.Errorf("index plugin log group: %w", err)
	}
	return nil
}

func (store *guardianStore) ensureNodeScopeColumn() error {
	rows, err := store.database.Query(`PRAGMA table_info(nodes)`)
	if err != nil {
		return fmt.Errorf("inspect node scope schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var columnID, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&columnID, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan node scope schema: %w", err)
		}
		if columnName == "scope" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate node scope schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close node scope schema: %w", err)
	}
	if _, err := store.database.Exec(`ALTER TABLE nodes ADD COLUMN scope TEXT NOT NULL DEFAULT 'inspection'`); err != nil {
		return fmt.Errorf("add node scope column: %w", err)
	}
	return nil
}

func (store *guardianStore) ensureAuthBindingColumns() error {
	rows, err := store.database.Query(`PRAGMA table_info(auth_bindings)`)
	if err != nil {
		return fmt.Errorf("inspect auth binding schema: %w", err)
	}
	defer rows.Close()
	existingColumns := make(map[string]struct{})
	for rows.Next() {
		var columnID int
		var columnName, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&columnID, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan auth binding schema: %w", err)
		}
		existingColumns[columnName] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate auth binding schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close auth binding schema: %w", err)
	}
	columns := []struct {
		name       string
		definition string
	}{
		{name: "inspection_run_id", definition: "integer not null default 0"},
		{name: "account_type", definition: "text not null default ''"},
		{name: "schedule_group", definition: "integer"},
		{name: "last_inspection", definition: "integer not null default 0"},
	}
	for _, column := range columns {
		if _, exists := existingColumns[column.name]; exists {
			continue
		}
		if _, err := store.database.Exec(`ALTER TABLE auth_bindings ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
			return fmt.Errorf("add auth binding column %s: %w", column.name, err)
		}
	}
	return nil
}

func (store *guardianStore) close() error {
	if store == nil || store.database == nil {
		return nil
	}
	return store.database.Close()
}

func (store *guardianStore) settings() (pluginSettings, error) {
	settings := defaultPluginSettings()
	rows, err := store.database.Query(`SELECT setting_key, setting_value FROM plugin_settings`)
	if err != nil {
		return pluginSettings{}, fmt.Errorf("read plugin settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return pluginSettings{}, fmt.Errorf("scan plugin settings: %w", err)
		}
		switch key {
		case "debug_enabled":
			settings.DebugEnabled = strings.TrimSpace(value) == "1" || strings.EqualFold(strings.TrimSpace(value), "true")
		case "realtime_guard_ttfb_seconds":
			parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if parseErr == nil {
				settings.RealtimeGuardTTFBSeconds = parsed
			}
		case "realtime_guard_generation_seconds":
			parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if parseErr == nil {
				settings.RealtimeGuardGenerationSeconds = parsed
			}
		default:
			parsed, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr != nil {
				continue
			}
			switch key {
			case "worker_count":
				settings.WorkerCount = parsed
			case "schedule_group_count":
				settings.ScheduleGroupCount = parsed
			case "refresh_interval_seconds":
				settings.RefreshIntervalSeconds = parsed
			case "inspection_interval_seconds":
				settings.InspectionIntervalSeconds = parsed
			case "keepalive_worker_count":
				settings.KeepaliveWorkerCount = parsed
			case "keepalive_interval_seconds":
				settings.KeepaliveIntervalSeconds = parsed
			case "revive_interval_seconds":
				settings.ReviveIntervalSeconds = parsed
			case "probe_retry_count":
				settings.ProbeRetryCount = parsed
			case "healthy_slot_count":
				settings.HealthySlotCount = parsed
			case "healthy_candidate_slot_count":
				settings.HealthyCandidateSlotCount = parsed
			case "healthy_slot_max_age_minutes":
				settings.HealthySlotMaxAgeMinutes = parsed
			case "realtime_guard_token_threshold":
				settings.RealtimeGuardTokenThreshold = parsed
			case "realtime_guard_timeout_seconds":
				settings.RealtimeGuardTimeoutSeconds = parsed
			case "realtime_guard_idle_timeout_seconds":
				settings.RealtimeGuardIdleTimeoutSeconds = parsed
			case "realtime_guard_min_summary_chars":
				settings.RealtimeGuardMinSummaryChars = parsed
			case "realtime_guard_min_encrypted_bytes":
				settings.RealtimeGuardMinEncryptedBytes = parsed
			case "realtime_guard_encrypted_bytes_per_reasoning_token":
				settings.RealtimeGuardEncryptedBytesPerReasoningToken = parsed
			case "realtime_guard_min_output_tokens":
				settings.RealtimeGuardMinOutputTokens = parsed
			case "realtime_guard_burst_min_reasoning_tokens":
				settings.RealtimeGuardBurstMinReasoningTokens = parsed
			case "realtime_guard_burst_max_visible_tokens":
				settings.RealtimeGuardBurstMaxVisibleTokens = parsed
			case "realtime_guard_burst_max_window_ms":
				settings.RealtimeGuardBurstMaxWindowMS = parsed
			case "ip_batch_retention_days":
				settings.IPBatchRetentionDays = parsed
			}
		}
	}
	if err := rows.Err(); err != nil {
		return pluginSettings{}, fmt.Errorf("iterate plugin settings: %w", err)
	}
	if err := validateSettings(settings); err != nil {
		return pluginSettings{}, err
	}
	return settings, nil
}

func (store *guardianStore) setSettings(settings pluginSettings) error {
	if err := validateSettings(settings); err != nil {
		return err
	}
	values := map[string]string{
		"worker_count":                                       strconv.Itoa(settings.WorkerCount),
		"schedule_group_count":                               strconv.Itoa(settings.ScheduleGroupCount),
		"debug_enabled":                                      strconv.FormatBool(settings.DebugEnabled),
		"refresh_interval_seconds":                           strconv.Itoa(settings.RefreshIntervalSeconds),
		"inspection_interval_seconds":                        strconv.Itoa(settings.InspectionIntervalSeconds),
		"keepalive_worker_count":                             strconv.Itoa(settings.KeepaliveWorkerCount),
		"keepalive_interval_seconds":                         strconv.Itoa(settings.KeepaliveIntervalSeconds),
		"revive_interval_seconds":                            strconv.Itoa(settings.ReviveIntervalSeconds),
		"probe_retry_count":                                  strconv.Itoa(settings.ProbeRetryCount),
		"healthy_slot_count":                                 strconv.Itoa(settings.HealthySlotCount),
		"healthy_candidate_slot_count":                       strconv.Itoa(settings.HealthyCandidateSlotCount),
		"healthy_slot_max_age_minutes":                       strconv.Itoa(settings.HealthySlotMaxAgeMinutes),
		"realtime_guard_ttfb_seconds":                        strconv.FormatFloat(settings.RealtimeGuardTTFBSeconds, 'f', -1, 64),
		"realtime_guard_generation_seconds":                  strconv.FormatFloat(settings.RealtimeGuardGenerationSeconds, 'f', -1, 64),
		"realtime_guard_token_threshold":                     strconv.Itoa(settings.RealtimeGuardTokenThreshold),
		"realtime_guard_timeout_seconds":                     strconv.Itoa(settings.RealtimeGuardTimeoutSeconds),
		"realtime_guard_idle_timeout_seconds":                strconv.Itoa(settings.RealtimeGuardIdleTimeoutSeconds),
		"realtime_guard_min_summary_chars":                   strconv.Itoa(settings.RealtimeGuardMinSummaryChars),
		"realtime_guard_min_encrypted_bytes":                 strconv.Itoa(settings.RealtimeGuardMinEncryptedBytes),
		"realtime_guard_encrypted_bytes_per_reasoning_token": strconv.Itoa(settings.RealtimeGuardEncryptedBytesPerReasoningToken),
		"realtime_guard_min_output_tokens":                   strconv.Itoa(settings.RealtimeGuardMinOutputTokens),
		"realtime_guard_burst_min_reasoning_tokens":          strconv.Itoa(settings.RealtimeGuardBurstMinReasoningTokens),
		"realtime_guard_burst_max_visible_tokens":            strconv.Itoa(settings.RealtimeGuardBurstMaxVisibleTokens),
		"realtime_guard_burst_max_window_ms":                 strconv.Itoa(settings.RealtimeGuardBurstMaxWindowMS),
		"ip_batch_retention_days":                            strconv.Itoa(settings.IPBatchRetentionDays),
	}
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin plugin settings update: %w", err)
	}
	defer tx.Rollback()
	for key, value := range values {
		if _, err := tx.Exec(`INSERT INTO plugin_settings(setting_key, setting_value) VALUES (?, ?)
ON CONFLICT(setting_key) DO UPDATE SET setting_value = excluded.setting_value`, key, value); err != nil {
			return fmt.Errorf("save plugin setting %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit plugin settings update: %w", err)
	}
	return nil
}

func validateSettings(settings pluginSettings) error {
	if settings.InspectionIntervalSeconds < 0 || settings.InspectionIntervalSeconds > maxInspectionIntervalSeconds {
		return fmt.Errorf("inspection interval is out of range")
	}
	if settings.WorkerCount < 1 || settings.WorkerCount > maxProbeWorkers {
		return fmt.Errorf("probe worker count is out of range")
	}
	if settings.ScheduleGroupCount < 1 || settings.ScheduleGroupCount > maxScheduleGroupCount {
		return fmt.Errorf("schedule group count is out of range")
	}
	if settings.RefreshIntervalSeconds < 5 || settings.RefreshIntervalSeconds > maxRefreshIntervalSeconds {
		return fmt.Errorf("page refresh interval is out of range")
	}
	if settings.KeepaliveWorkerCount < 1 || settings.KeepaliveWorkerCount > maxKeepaliveWorkerCount {
		return fmt.Errorf("keepalive worker count is out of range")
	}
	if settings.KeepaliveIntervalSeconds < 1 || settings.KeepaliveIntervalSeconds > maxKeepaliveIntervalSeconds {
		return fmt.Errorf("keepalive interval is out of range")
	}
	if settings.ReviveIntervalSeconds < 1 || settings.ReviveIntervalSeconds > maxReviveIntervalSeconds {
		return fmt.Errorf("revive interval is out of range")
	}
	if settings.ProbeRetryCount < 1 || settings.ProbeRetryCount > maxKeepaliveProbeRetryCount {
		return fmt.Errorf("probe retry count is out of range")
	}
	if settings.HealthySlotCount < 1 || settings.HealthySlotCount > maxSlotCount || settings.HealthyCandidateSlotCount < 0 || settings.HealthyCandidateSlotCount > maxSlotCount || settings.HealthySlotCount+settings.HealthyCandidateSlotCount > maxSlotCount {
		return fmt.Errorf("healthy slot counts are out of range")
	}
	if settings.HealthySlotMaxAgeMinutes < 1 || settings.HealthySlotMaxAgeMinutes > maxHealthySlotMaxAgeMinutes {
		return fmt.Errorf("healthy slot max age is out of range")
	}
	if math.IsNaN(settings.RealtimeGuardTTFBSeconds) || math.IsInf(settings.RealtimeGuardTTFBSeconds, 0) || settings.RealtimeGuardTTFBSeconds <= 0 || math.IsNaN(settings.RealtimeGuardGenerationSeconds) || math.IsInf(settings.RealtimeGuardGenerationSeconds, 0) || settings.RealtimeGuardGenerationSeconds <= 0 {
		return fmt.Errorf("realtime guard thresholds must be positive")
	}
	if settings.RealtimeGuardTokenThreshold < 1 || settings.RealtimeGuardTimeoutSeconds < 1 || settings.RealtimeGuardIdleTimeoutSeconds < 1 || settings.RealtimeGuardMinSummaryChars < 1 || settings.RealtimeGuardMinEncryptedBytes < 64 || settings.RealtimeGuardEncryptedBytesPerReasoningToken < 1 || settings.RealtimeGuardMinOutputTokens < 8 || settings.RealtimeGuardBurstMinReasoningTokens < 1 || settings.RealtimeGuardBurstMaxVisibleTokens < 1 || settings.RealtimeGuardBurstMaxWindowMS < 1 {
		return fmt.Errorf("realtime guard evidence thresholds are out of range")
	}
	if settings.IPBatchRetentionDays < 1 {
		return fmt.Errorf("IP batch retention days must be positive")
	}
	return nil
}

func (store *guardianStore) appendLog(level, event, message, detail string) error {
	category, status := classifyLogEvent(event, level)
	return store.appendCategorizedLog(category, "", status, level, event, 0, "", message, detail)
}

func (store *guardianStore) appendRoundLog(category string, roundID int64, status, level, event, message, detail string) error {
	return store.appendCategorizedLog(category, strconv.FormatInt(roundID, 10), status, level, event, 0, "", message, detail)
}

func (store *guardianStore) appendRoundNodeLog(category string, roundID int64, status, level, event string, nodeID int64, nodeName, message, detail string) error {
	return store.appendCategorizedLog(category, strconv.FormatInt(roundID, 10), status, level, event, nodeID, nodeName, message, detail)
}

func (store *guardianStore) appendCategorizedLog(category, groupID, status, level, event string, nodeID int64, nodeName, message, detail string) error {
	_, err := store.database.Exec(`INSERT INTO plugin_logs(created_at, level, event, message, detail, category, group_id, log_status, node_id, node_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, time.Now().UnixMilli(), level, event, sanitizeLogText(message), sanitizeLogText(detail), category, groupID, status, nodeID, sanitizeLogText(nodeName))
	if err != nil {
		return fmt.Errorf("append plugin log: %w", err)
	}
	if _, err := store.database.Exec(`DELETE FROM plugin_logs WHERE category = ? AND id NOT IN (SELECT id FROM plugin_logs WHERE category = ? ORDER BY id DESC LIMIT ?)`, logCategoryRealtimeGuard, logCategoryRealtimeGuard, maxRealtimeGuardLogs); err != nil {
		return fmt.Errorf("prune realtime guard logs: %w", err)
	}
	if _, err := store.database.Exec(`DELETE FROM plugin_logs WHERE category = ? AND group_id <> '' AND group_id NOT IN (SELECT group_id FROM plugin_logs WHERE category = ? AND group_id <> '' GROUP BY group_id ORDER BY MAX(id) DESC LIMIT ?)`, logCategoryBatchProbe, logCategoryBatchProbe, maxIPBatches); err != nil {
		return fmt.Errorf("prune batch probe logs: %w", err)
	}
	for _, groupedCategory := range []string{logCategoryKeepalive, logCategoryRevive} {
		if _, err := store.database.Exec(`DELETE FROM plugin_logs WHERE category = ? AND group_id <> '' AND group_id NOT IN (SELECT group_id FROM plugin_logs WHERE category = ? AND group_id <> '' GROUP BY group_id ORDER BY MAX(id) DESC LIMIT ?)`, groupedCategory, groupedCategory, maxGroupedLogSets); err != nil {
			return fmt.Errorf("prune %s logs: %w", groupedCategory, err)
		}
	}
	if _, err := store.database.Exec(`DELETE FROM plugin_logs WHERE id NOT IN (SELECT id FROM plugin_logs ORDER BY id DESC LIMIT ?)`, maxPluginLogs); err != nil {
		return fmt.Errorf("prune plugin logs: %w", err)
	}
	return nil
}

func classifyLogEvent(event, level string) (string, string) {
	category := logCategoryGeneral
	switch {
	case strings.HasPrefix(event, "guard."):
		category = logCategoryRealtimeGuard
	case strings.HasPrefix(event, "probe.") && event != "probe.scheduler_started" && event != "probe.round_failed":
		category = logCategoryBatchProbe
	case strings.HasPrefix(event, "keepalive.") && event != "keepalive.scheduler_started" && event != "keepalive.manual_triggered" && event != "keepalive.round_failed":
		category = logCategoryKeepalive
	case strings.HasPrefix(event, "revive.") && event != "revive.scheduler_started" && event != "revive.round_failed":
		category = logCategoryRevive
	}
	status := ""
	if level == logLevelError || strings.Contains(event, "failed") {
		status = logStatusError
	} else if strings.Contains(event, "completed") {
		status = logStatusConnected
	} else if strings.Contains(event, "started") || strings.Contains(event, "triggered") {
		status = logStatusProbing
	}
	return category, status
}

func logStatusForNodeResult(status string) string {
	if status == statusHealthy || status == statusHealthyCandidate || status == statusHealthyFallback || status == statusConnected {
		return logStatusConnected
	}
	if status == statusProbing || status == statusKeepaliveProbing || status == statusReviveProbing {
		return logStatusProbing
	}
	return logStatusError
}

func sanitizeLogText(value string) string {
	for _, key := range []string{"authorization", "access_token", "client_secret", "api_key", "password", "cookie", "bearer", "secret", "token"} {
		searchFrom := 0
		for searchFrom < len(value) {
			lower := strings.ToLower(value)
			relativeIndex := strings.Index(lower[searchFrom:], key)
			if relativeIndex < 0 {
				break
			}
			index := searchFrom + relativeIndex
			keyEnd := index + len(key)
			if !logKeyBoundary(value, index, keyEnd) {
				searchFrom = keyEnd
				continue
			}
			valueStart := keyEnd
			for valueStart < len(value) && strings.ContainsRune(" 	:=\"'", rune(value[valueStart])) {
				valueStart++
			}
			valueEnd := strings.IndexAny(value[valueStart:], "\r\n,;}")
			if valueEnd < 0 {
				valueEnd = len(value)
			} else {
				valueEnd += valueStart
			}
			value = value[:index] + "[REDACTED]" + value[valueEnd:]
			searchFrom = index + len("[REDACTED]")
		}
	}
	return logURLPattern.ReplaceAllStringFunc(value, redactLogURL)
}

func logKeyBoundary(value string, start, end int) bool {
	if start > 0 && isLogWordByte(value[start-1]) {
		return false
	}
	return end >= len(value) || !isLogWordByte(value[end])
}

func isLogWordByte(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9')
}

func redactLogURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[REDACTED_URL]"
	}
	return parsed.Scheme + "://" + parsed.Host
}

func (store *guardianStore) listLogs(limit int, debugEnabled bool, category, search, groupID, status string) (pluginLogList, error) {
	if category == "" {
		category = logCategoryRealtimeGuard
	}
	if !isLogCategory(category) {
		return pluginLogList{}, fmt.Errorf("unsupported log category")
	}
	max := maxPluginLogs
	if category == logCategoryRealtimeGuard {
		max = maxRealtimeGuardLogs
	}
	if !debugEnabled && category != logCategoryRealtimeGuard {
		return pluginLogList{Items: []pluginLog{}, Max: max, Search: search, Category: category, GroupID: groupID, Status: status}, nil
	}
	if isGroupedLogCategory(category) && groupID == "" {
		return pluginLogList{}, fmt.Errorf("group id is required for grouped logs")
	}
	if limit <= 0 || limit > max {
		limit = max
	}
	conditions := []string{"category = ?"}
	args := []any{category}
	if groupID != "" {
		conditions = append(conditions, "group_id = ?")
		args = append(args, groupID)
	}
	if status != "" {
		if status != logStatusConnected && status != logStatusProbing && status != logStatusError {
			return pluginLogList{}, fmt.Errorf("unsupported log status")
		}
		conditions = append(conditions, "log_status = ?")
		args = append(args, status)
	}
	if search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		conditions = append(conditions, `(CAST(id AS TEXT) LIKE ? OR lower(level) LIKE ? OR lower(event) LIKE ? OR lower(node_name) LIKE ? OR lower(message) LIKE ? OR lower(detail) LIKE ?)`)
		for index := 0; index < 6; index++ {
			args = append(args, pattern)
		}
	}
	args = append(args, limit)
	query := `SELECT id, created_at, level, event, message, detail, category, group_id, log_status, node_id, node_name FROM plugin_logs WHERE ` + strings.Join(conditions, " AND ") + ` ORDER BY id DESC LIMIT ?`
	rows, err := store.database.Query(query, args...)
	if err != nil {
		return pluginLogList{}, fmt.Errorf("list plugin logs: %w", err)
	}
	defer rows.Close()
	items := make([]pluginLog, 0)
	for rows.Next() {
		var item pluginLog
		if err := rows.Scan(&item.ID, &item.CreatedAt, &item.Level, &item.Event, &item.Message, &item.Detail, &item.Category, &item.GroupID, &item.Status, &item.NodeID, &item.NodeName); err != nil {
			return pluginLogList{}, fmt.Errorf("scan plugin log: %w", err)
		}
		item.Message = sanitizeLogText(item.Message)
		item.Detail = sanitizeLogText(item.Detail)
		item.NodeName = sanitizeLogText(item.NodeName)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return pluginLogList{}, fmt.Errorf("iterate plugin logs: %w", err)
	}
	return pluginLogList{Items: items, Total: len(items), Max: max, Search: search, Category: category, GroupID: groupID, Status: status}, nil
}

func (store *guardianStore) listLogGroups(category string) ([]pluginLogGroup, error) {
	if !isGroupedLogCategory(category) {
		return nil, fmt.Errorf("log category does not support groups")
	}
	limit := maxGroupedLogSets
	if category == logCategoryBatchProbe {
		limit = maxIPBatches
	}
	roundTable := map[string]string{
		logCategoryBatchProbe: "probe_rounds",
		logCategoryKeepalive:  "keepalive_rounds",
		logCategoryRevive:     "revive_rounds",
	}[category]
	query := fmt.Sprintf(`SELECT logs.group_id, MAX(logs.created_at), COALESCE(NULLIF(rounds.completed_at, 0), MAX(logs.created_at)), COUNT(*),
COALESCE(rounds.started_at, MIN(logs.created_at)),
COALESCE(rounds.status, CASE WHEN MAX(CASE WHEN logs.level = 'error' OR logs.log_status = 'error' THEN 1 ELSE 0 END) = 1 THEN 'failed' ELSE 'running' END),
COALESCE(rounds.candidate_count, 0), COALESCE(rounds.success_count, 0), COALESCE(rounds.failure_count, 0)
FROM plugin_logs AS logs
LEFT JOIN %s AS rounds ON CAST(rounds.id AS TEXT) = logs.group_id
WHERE logs.category = ? AND logs.group_id <> ''
GROUP BY logs.group_id
ORDER BY MAX(logs.id) DESC LIMIT ?`, roundTable)
	rows, err := store.database.Query(query, category, limit)
	if err != nil {
		return nil, fmt.Errorf("list plugin log groups: %w", err)
	}
	defer rows.Close()
	groups := make([]pluginLogGroup, 0)
	for rows.Next() {
		var groupID string
		var startedAt, completedAt, updatedAt, logCount, candidateCount, successCount, failureCount int64
		var groupStatus string
		if err := rows.Scan(&groupID, &updatedAt, &completedAt, &logCount, &startedAt, &groupStatus, &candidateCount, &successCount, &failureCount); err != nil {
			return nil, fmt.Errorf("scan plugin log group: %w", err)
		}
		sequenceNumber, parseErr := strconv.ParseInt(groupID, 10, 64)
		if parseErr != nil {
			sequenceNumber = int64(len(groups) + 1)
		}
		groups = append(groups, pluginLogGroup{ID: groupID, SequenceNumber: sequenceNumber, Category: category, Status: groupStatus, StartedAt: startedAt, CompletedAt: completedAt, UpdatedAt: updatedAt, LogCount: logCount, CandidateCount: candidateCount, SuccessCount: successCount, FailureCount: failureCount})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate plugin log groups: %w", err)
	}
	return groups, nil
}

func isLogCategory(category string) bool {
	switch category {
	case logCategoryRealtimeGuard, logCategoryGeneral, logCategoryBatchProbe, logCategoryKeepalive, logCategoryRevive:
		return true
	default:
		return false
	}
}

func isGroupedLogCategory(category string) bool {
	return category == logCategoryBatchProbe || category == logCategoryKeepalive || category == logCategoryRevive
}

func (store *guardianStore) insertNodes(nodes []proxyNode) (int, int, error) {
	if len(nodes) == 0 {
		return 0, 0, nil
	}
	tx, err := store.database.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("begin node insert: %w", err)
	}
	defer tx.Rollback()
	added, duplicates := 0, 0
	for _, node := range nodes {
		result, err := tx.Exec(`INSERT OR IGNORE INTO nodes(address, scope, protocol, host, port, status, created_at) VALUES (?, 'inspection', ?, ?, ?, ?, ?)`, node.Address, node.Protocol, node.Host, node.Port, statusUninspected, time.Now().UnixMilli())
		if err != nil {
			return 0, 0, fmt.Errorf("insert node: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, 0, fmt.Errorf("read node insert result: %w", err)
		}
		if count == 0 {
			duplicates++
		} else {
			added++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit node insert: %w", err)
	}
	return added, duplicates, nil
}

func newIPBatchID() string {
	return fmt.Sprintf("B%d", time.Now().UnixNano())
}

func (store *guardianStore) insertIPBatch(nodes []proxyNode, inputErrorCount int) (string, int, int, error) {
	if len(nodes) == 0 {
		return "", 0, 0, nil
	}
	batchID := newIPBatchID()
	tx, err := store.database.Begin()
	if err != nil {
		return batchID, 0, 0, fmt.Errorf("begin IP batch insert: %w", err)
	}
	defer tx.Rollback()
	createdAt := time.Now().UnixMilli()
	var sequenceNumber int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(sequence_number), 0) + 1 FROM ip_batches`).Scan(&sequenceNumber); err != nil {
		return batchID, 0, 0, fmt.Errorf("read IP batch sequence: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO ip_batches(batch_id, sequence_number, created_at, input_error_count) VALUES (?, ?, ?, ?)`, batchID, sequenceNumber, createdAt, inputErrorCount); err != nil {
		return batchID, 0, 0, fmt.Errorf("create IP batch: %w", err)
	}
	statement, err := tx.Prepare(`INSERT OR IGNORE INTO nodes(address, scope, protocol, host, port, status, created_at) VALUES (?, 'guard', ?, ?, ?, ?, ?)`)
	if err != nil {
		return batchID, 0, 0, fmt.Errorf("prepare IP batch node insert: %w", err)
	}
	defer statement.Close()
	added, duplicates := 0, 0
	for _, node := range nodes {
		result, execErr := statement.Exec(node.Address, node.Protocol, node.Host, node.Port, statusUninspected, createdAt)
		if execErr != nil {
			return batchID, 0, 0, fmt.Errorf("insert IP batch node: %w", execErr)
		}
		rowsAffected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return batchID, 0, 0, fmt.Errorf("read IP batch insert result: %w", rowsErr)
		}
		if rowsAffected == 0 {
			duplicates++
			continue
		}
		nodeID, idErr := result.LastInsertId()
		if idErr != nil {
			return batchID, 0, 0, fmt.Errorf("read IP batch node ID: %w", idErr)
		}
		if _, err := tx.Exec(`INSERT INTO ip_batch_nodes(batch_id, node_id) VALUES (?, ?)`, batchID, nodeID); err != nil {
			return batchID, 0, 0, fmt.Errorf("link IP batch node: %w", err)
		}
		added++
	}
	if _, err := tx.Exec(`UPDATE ip_batches SET total_count = ?, duplicate_count = ? WHERE batch_id = ?`, added, duplicates, batchID); err != nil {
		return batchID, 0, 0, fmt.Errorf("update IP batch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return batchID, 0, 0, fmt.Errorf("commit IP batch insert: %w", err)
	}
	return batchID, added, duplicates, nil
}

func (store *guardianStore) deleteExpiredIPBatches(now time.Time, retentionDays int) (int64, int64, error) {
	cutoff := now.AddDate(0, 0, -retentionDays).UnixMilli()
	tx, err := store.database.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("begin expired IP batch cleanup: %w", err)
	}
	defer tx.Rollback()

	nodeResult, err := tx.Exec(`
DELETE FROM nodes
WHERE scope = 'guard'
  AND id IN (
    SELECT expired_links.node_id
    FROM ip_batch_nodes AS expired_links
    INNER JOIN ip_batches AS expired_batches ON expired_batches.batch_id = expired_links.batch_id
    WHERE expired_batches.created_at <= ?
      AND NOT EXISTS (
        SELECT 1
        FROM ip_batch_nodes AS retained_links
        INNER JOIN ip_batches AS retained_batches ON retained_batches.batch_id = retained_links.batch_id
        WHERE retained_links.node_id = expired_links.node_id
          AND retained_batches.created_at > ?
      )
  )`, cutoff, cutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("delete expired IP batch nodes: %w", err)
	}
	deletedNodes, err := nodeResult.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("read expired IP batch node delete result: %w", err)
	}

	batchResult, err := tx.Exec(`DELETE FROM ip_batches WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("delete expired IP batches: %w", err)
	}
	deletedBatches, err := batchResult.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("read expired IP batch delete result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit expired IP batch cleanup: %w", err)
	}
	return deletedBatches, deletedNodes, nil
}

func (store *guardianStore) listIPNodes() ([]proxyNode, error) {
	rows, err := store.database.Query(`
SELECT DISTINCT nodes.id, nodes.address, nodes.protocol, nodes.host, nodes.port, nodes.status, nodes.latency_ms, nodes.exit_ip, nodes.country, nodes.last_checked, nodes.last_error, nodes.created_at, COALESCE(healthy_slots.slot_id, 0), COALESCE(healthy_slots.slot_kind, '')
FROM nodes
INNER JOIN ip_batch_nodes ON ip_batch_nodes.node_id = nodes.id
LEFT JOIN healthy_slots ON healthy_slots.node_id = nodes.id
WHERE nodes.scope = 'guard'
ORDER BY nodes.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list IP nodes: %w", err)
	}
	defer rows.Close()
	items := make([]proxyNode, 0)
	for rows.Next() {
		var item proxyNode
		if err := rows.Scan(&item.ID, &item.Address, &item.Protocol, &item.Host, &item.Port, &item.Status, &item.LatencyMS, &item.ExitIP, &item.Country, &item.LastChecked, &item.LastError, &item.CreatedAt, &item.SlotID, &item.SlotKind); err != nil {
			return nil, fmt.Errorf("scan IP node: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *guardianStore) listIPBatches() ([]ipBatch, error) {
	rows, err := store.database.Query(`
SELECT batches.batch_id, batches.sequence_number, batches.created_at, batches.total_count, batches.duplicate_count, batches.input_error_count,
       COALESCE(SUM(CASE WHEN nodes.status IN (?, ?, ?, ?, ?) THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN nodes.status IN (?, ?, ?) THEN 1 ELSE 0 END), 0)
FROM ip_batches AS batches
LEFT JOIN ip_batch_nodes AS batch_nodes ON batch_nodes.batch_id = batches.batch_id
LEFT JOIN nodes ON nodes.id = batch_nodes.node_id AND nodes.scope = 'guard'
GROUP BY batches.batch_id, batches.sequence_number, batches.created_at, batches.total_count, batches.duplicate_count, batches.input_error_count
ORDER BY batches.sequence_number DESC, batches.created_at DESC, batches.batch_id DESC
LIMIT ?`, statusHealthy, statusHealthyCandidate, statusHealthyFallback, statusUnhealthy, statusDisabled, statusHealthy, statusHealthyCandidate, statusHealthyFallback, maxIPBatches)
	if err != nil {
		return nil, fmt.Errorf("list IP batches: %w", err)
	}
	defer rows.Close()
	items := make([]ipBatch, 0)
	for rows.Next() {
		var item ipBatch
		if err := rows.Scan(&item.ID, &item.SequenceNumber, &item.CreatedAt, &item.TotalCount, &item.DuplicateCount, &item.InputErrorCount, &item.CompletedCount, &item.InitialConnectedCount); err != nil {
			return nil, fmt.Errorf("scan IP batch: %w", err)
		}
		item.ExpiresAt = time.UnixMilli(item.CreatedAt).AddDate(0, 0, defaultIPBatchRetentionDays).UnixMilli()
		item.RealtimeConnectedCount = item.InitialConnectedCount
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *guardianStore) listIPBatchNodes(batchID string) ([]proxyNode, error) {
	rows, err := store.database.Query(`
SELECT nodes.id, nodes.address, nodes.protocol, nodes.host, nodes.port, nodes.status, nodes.latency_ms, nodes.exit_ip, nodes.country, nodes.last_checked, nodes.last_error, nodes.created_at, COALESCE(healthy_slots.slot_id, 0), COALESCE(healthy_slots.slot_kind, '')
FROM nodes
INNER JOIN ip_batch_nodes ON ip_batch_nodes.node_id = nodes.id
LEFT JOIN healthy_slots ON healthy_slots.node_id = nodes.id
WHERE ip_batch_nodes.batch_id = ? AND nodes.scope = 'guard'
ORDER BY nodes.id DESC`, batchID)
	if err != nil {
		return nil, fmt.Errorf("list IP batch nodes: %w", err)
	}
	defer rows.Close()
	items := make([]proxyNode, 0)
	for rows.Next() {
		var item proxyNode
		if err := rows.Scan(&item.ID, &item.Address, &item.Protocol, &item.Host, &item.Port, &item.Status, &item.LatencyMS, &item.ExitIP, &item.Country, &item.LastChecked, &item.LastError, &item.CreatedAt, &item.SlotID, &item.SlotKind); err != nil {
			return nil, fmt.Errorf("scan IP batch node: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *guardianStore) listNodes() ([]proxyNode, error) {
	rows, err := store.database.Query(`SELECT id, address, protocol, host, port, status, latency_ms, exit_ip, country, last_checked, last_error, created_at FROM nodes WHERE scope = 'inspection' ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	items := make([]proxyNode, 0)
	for rows.Next() {
		var item proxyNode
		if err := rows.Scan(&item.ID, &item.Address, &item.Protocol, &item.Host, &item.Port, &item.Status, &item.LatencyMS, &item.ExitIP, &item.Country, &item.LastChecked, &item.LastError, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *guardianStore) markInspectionFailed(runID, healthy, unhealthy int64, reason string) error {
	_, err := store.database.Exec(`UPDATE inspection_runs SET completed_at = ?, status = ?, healthy = ?, unhealthy = ? WHERE id = ?`, time.Now().UnixMilli(), "failed: "+sanitizeLogText(reason), healthy, unhealthy, runID)
	return err
}

func (store *guardianStore) getNode(id int64) (proxyNode, bool, error) {
	var item proxyNode
	err := store.database.QueryRow(`SELECT id, address, protocol, host, port, status, latency_ms, exit_ip, country, last_checked, last_error, created_at FROM nodes WHERE id = ? AND scope = 'inspection'`, id).Scan(&item.ID, &item.Address, &item.Protocol, &item.Host, &item.Port, &item.Status, &item.LatencyMS, &item.ExitIP, &item.Country, &item.LastChecked, &item.LastError, &item.CreatedAt)
	if err == sql.ErrNoRows {
		return proxyNode{}, false, nil
	}
	if err != nil {
		return proxyNode{}, false, fmt.Errorf("get node: %w", err)
	}
	return item, true, nil
}

func (store *guardianStore) deleteNode(id int64) error {
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin node delete: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE auth_bindings SET node_id = 0, proxy_url = '' WHERE node_id = ?`, id); err != nil {
		return fmt.Errorf("clear node bindings: %w", err)
	}
	result, err := tx.Exec(`DELETE FROM nodes WHERE id = ? AND scope = 'inspection'`, id)
	if err != nil {
		return fmt.Errorf("delete node: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read node delete result: %w", err)
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit node delete: %w", err)
	}
	return nil
}

func (store *guardianStore) updateNodeResult(result inspectionResult) error {
	result.Error = sanitizeLogText(result.Error)
	_, err := store.database.Exec(`UPDATE nodes SET status = ?, latency_ms = ?, exit_ip = ?, country = ?, last_checked = ?, last_error = ? WHERE id = ? AND scope = 'inspection'`, result.Status, result.LatencyMS, result.ExitIP, result.Country, result.CheckedAt, result.Error, result.NodeID)
	if err != nil {
		return fmt.Errorf("update node result: %w", err)
	}
	_, err = store.database.Exec(`INSERT INTO inspection_results(run_id, node_id, status, latency_ms, exit_ip, country, error, checked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, result.RunID, result.NodeID, result.Status, result.LatencyMS, result.ExitIP, result.Country, result.Error, result.CheckedAt)
	if err != nil {
		return fmt.Errorf("save inspection result: %w", err)
	}
	return nil
}

func (store *guardianStore) startInspection(total int) (int64, error) {
	result, err := store.database.Exec(`INSERT INTO inspection_runs(started_at, status, total) VALUES (?, ?, ?)`, time.Now().UnixMilli(), "running", total)
	if err != nil {
		return 0, fmt.Errorf("start inspection run: %w", err)
	}
	return result.LastInsertId()
}

func (store *guardianStore) finishInspection(runID int64, healthy, unhealthy int64) error {
	_, err := store.database.Exec(`UPDATE inspection_runs SET completed_at = ?, status = ?, healthy = ?, unhealthy = ? WHERE id = ?`, time.Now().UnixMilli(), "completed", healthy, unhealthy, runID)
	if err != nil {
		return fmt.Errorf("finish inspection run: %w", err)
	}
	return nil
}

func (store *guardianStore) latestInspection() (inspectionRun, []inspectionResult, error) {
	var run inspectionRun
	err := store.database.QueryRow(`SELECT id, started_at, completed_at, status, total, healthy, unhealthy FROM inspection_runs ORDER BY id DESC LIMIT 1`).Scan(&run.ID, &run.StartedAt, &run.CompletedAt, &run.Status, &run.Total, &run.Healthy, &run.Unhealthy)
	if err == sql.ErrNoRows {
		return inspectionRun{}, []inspectionResult{}, nil
	}
	if err != nil {
		return inspectionRun{}, nil, fmt.Errorf("read latest inspection: %w", err)
	}
	rows, err := store.database.Query(`SELECT id, run_id, node_id, status, latency_ms, exit_ip, country, error, checked_at FROM inspection_results WHERE run_id = ? ORDER BY id`, run.ID)
	if err != nil {
		return inspectionRun{}, nil, fmt.Errorf("list inspection results: %w", err)
	}
	defer rows.Close()
	results := make([]inspectionResult, 0)
	for rows.Next() {
		var item inspectionResult
		if err := rows.Scan(&item.ID, &item.RunID, &item.NodeID, &item.Status, &item.LatencyMS, &item.ExitIP, &item.Country, &item.Error, &item.CheckedAt); err != nil {
			return inspectionRun{}, nil, fmt.Errorf("scan inspection result: %w", err)
		}
		results = append(results, item)
	}
	return run, results, rows.Err()
}

func (store *guardianStore) latestCompletedInspection() (inspectionRun, bool, error) {
	var run inspectionRun
	err := store.database.QueryRow(`SELECT id, started_at, completed_at, status, total, healthy, unhealthy FROM inspection_runs WHERE status = 'completed' ORDER BY id DESC LIMIT 1`).Scan(&run.ID, &run.StartedAt, &run.CompletedAt, &run.Status, &run.Total, &run.Healthy, &run.Unhealthy)
	if err == sql.ErrNoRows {
		return inspectionRun{}, false, nil
	}
	if err != nil {
		return inspectionRun{}, false, fmt.Errorf("read latest completed inspection: %w", err)
	}
	return run, true, nil
}

func (store *guardianStore) upsertAuthBindings(bindings []authBinding) error {
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin auth binding update: %w", err)
	}
	defer tx.Rollback()
	if len(bindings) == 0 {
		if _, err := tx.Exec(`DELETE FROM auth_bindings`); err != nil {
			return fmt.Errorf("clear auth bindings: %w", err)
		}
	}
	for _, binding := range bindings {
		_, err := tx.Exec(`INSERT INTO auth_bindings(auth_index, auth_name, node_id, proxy_url, status, priority, success_count, failed_count, updated_at, last_checked, inspection_run_id, account_type, schedule_group, last_inspection)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(auth_index) DO UPDATE SET auth_name=excluded.auth_name, node_id=CASE WHEN excluded.node_id <> 0 THEN excluded.node_id ELSE auth_bindings.node_id END, proxy_url=CASE WHEN excluded.proxy_url <> '' THEN excluded.proxy_url ELSE auth_bindings.proxy_url END, status=excluded.status, priority=excluded.priority, success_count=excluded.success_count, failed_count=excluded.failed_count, updated_at=excluded.updated_at, last_checked=excluded.last_checked, inspection_run_id=excluded.inspection_run_id, account_type=excluded.account_type, schedule_group=excluded.schedule_group, last_inspection=excluded.last_inspection`, binding.AuthIndex, binding.AuthName, binding.NodeID, binding.ProxyURL, binding.Status, binding.Priority, binding.Success, binding.Failed, binding.UpdatedAt, binding.LastChecked, binding.InspectionRunID, binding.AccountType, binding.ScheduleGroup, binding.LastInspection)
		if err != nil {
			return fmt.Errorf("save auth binding %s: %w", binding.AuthIndex, err)
		}
	}
	if len(bindings) > 0 {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(bindings)), ",")
		args := make([]any, 0, len(bindings))
		for _, binding := range bindings {
			args = append(args, binding.AuthIndex)
		}
		if _, err := tx.Exec(`DELETE FROM auth_bindings WHERE auth_index NOT IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("remove stale auth bindings: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit auth binding update: %w", err)
	}
	return nil
}

func (store *guardianStore) observeAuthAttempt(authIndex, authName, proxyURL string) error {
	redactedProxy := redactProxyURL(proxyURL)
	nodeID := int64(0)
	if redactedProxy != "" {
		_ = store.database.QueryRow(`SELECT id FROM nodes WHERE address = ? AND scope = 'inspection'`, redactedProxy).Scan(&nodeID)
	}
	now := time.Now().UnixMilli()
	_, err := store.database.Exec(`INSERT INTO auth_bindings(auth_index, auth_name, node_id, proxy_url, status, updated_at, last_checked)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(auth_index) DO UPDATE SET auth_name=CASE WHEN excluded.auth_name <> '' THEN excluded.auth_name ELSE auth_bindings.auth_name END, node_id=CASE WHEN excluded.node_id <> 0 THEN excluded.node_id ELSE auth_bindings.node_id END, proxy_url=CASE WHEN excluded.proxy_url <> '' THEN excluded.proxy_url ELSE auth_bindings.proxy_url END, status=excluded.status, updated_at=excluded.updated_at, last_checked=excluded.last_checked`, authIndex, authName, nodeID, redactedProxy, "observed", now, now)
	if err != nil {
		return fmt.Errorf("observe auth attempt: %w", err)
	}
	return nil
}

func (store *guardianStore) listAuthBindings(inspectionRunID int64) ([]authBinding, error) {
	rows, err := store.database.Query(`SELECT auth_bindings.auth_index, auth_bindings.auth_name, auth_bindings.node_id, auth_bindings.proxy_url, COALESCE(nodes.exit_ip, ''), auth_bindings.status, auth_bindings.priority, auth_bindings.success_count, auth_bindings.failed_count, auth_bindings.updated_at, auth_bindings.last_checked, auth_bindings.inspection_run_id, auth_bindings.account_type, auth_bindings.schedule_group, auth_bindings.last_inspection FROM auth_bindings LEFT JOIN nodes ON nodes.id = auth_bindings.node_id AND nodes.scope = 'inspection' WHERE auth_bindings.inspection_run_id = ? ORDER BY auth_bindings.priority DESC, auth_bindings.auth_name ASC`, inspectionRunID)
	if err != nil {
		return nil, fmt.Errorf("list auth bindings: %w", err)
	}
	defer rows.Close()
	items := make([]authBinding, 0)
	for rows.Next() {
		var item authBinding
		var scheduleGroup sql.NullInt64
		if err := rows.Scan(&item.AuthIndex, &item.AuthName, &item.NodeID, &item.ProxyURL, &item.ExitIP, &item.Status, &item.Priority, &item.Success, &item.Failed, &item.UpdatedAt, &item.LastChecked, &item.InspectionRunID, &item.AccountType, &scheduleGroup, &item.LastInspection); err != nil {
			return nil, fmt.Errorf("scan auth binding: %w", err)
		}
		if scheduleGroup.Valid {
			value := int(scheduleGroup.Int64)
			item.ScheduleGroup = &value
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *guardianStore) recordDegradation(authIndex, authName, requestID, reason string) (degradationState, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	now := time.Now().UnixMilli()
	var state degradationState
	err := store.database.QueryRow(`SELECT auth_index, auth_name, count, last_reason, last_request_id, last_seen, cooling_until FROM degradation_states WHERE auth_index = ?`, authIndex).Scan(&state.AuthIndex, &state.AuthName, &state.Count, &state.LastReason, &state.LastRequest, &state.LastSeen, &state.CoolingUntil)
	if err == sql.ErrNoRows {
		state.AuthIndex = authIndex
		state.AuthName = authName
	}
	if err != nil && err != sql.ErrNoRows {
		return degradationState{}, fmt.Errorf("read degradation state: %w", err)
	}
	duplicateRequest := requestID != "" && requestID == state.LastRequest
	if !duplicateRequest && state.Count < 3 && (state.CoolingUntil == 0 || state.CoolingUntil <= now) {
		state.Count++
		switch state.Count {
		case 1:
			state.CoolingUntil = now + degradationFirstCooling.Milliseconds()
		case 2:
			state.CoolingUntil = now + degradationSecondCooling.Milliseconds()
		default:
			state.CoolingUntil = degradationPermanentCoolingUntil
		}
	} else if state.Count >= 3 {
		state.Count = 3
		state.CoolingUntil = degradationPermanentCoolingUntil
	}
	state.LastReason = reason
	state.LastRequest = requestID
	state.LastSeen = now
	_, err = store.database.Exec(`INSERT INTO degradation_states(auth_index, auth_name, count, last_reason, last_request_id, last_seen, cooling_until) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(auth_index) DO UPDATE SET auth_name=CASE WHEN excluded.auth_name <> '' THEN excluded.auth_name ELSE degradation_states.auth_name END, count=excluded.count, last_reason=excluded.last_reason, last_request_id=excluded.last_request_id, last_seen=excluded.last_seen, cooling_until=excluded.cooling_until`, state.AuthIndex, state.AuthName, state.Count, state.LastReason, state.LastRequest, state.LastSeen, state.CoolingUntil)
	if err != nil {
		return degradationState{}, fmt.Errorf("save degradation state: %w", err)
	}
	return state, nil
}

func (store *guardianStore) clearDegradation(authIndex string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	_, err := store.database.Exec(`DELETE FROM degradation_states WHERE auth_index = ?`, authIndex)
	if err != nil {
		return fmt.Errorf("clear degradation state: %w", err)
	}
	return nil
}

func (store *guardianStore) clearDegradationIfRecovered(authIndex string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	var coolingUntil int64
	var priority int
	err := store.database.QueryRow(`SELECT degradation_states.cooling_until, COALESCE(auth_bindings.priority, 0) FROM degradation_states LEFT JOIN auth_bindings ON auth_bindings.auth_index = degradation_states.auth_index WHERE degradation_states.auth_index = ?`, authIndex).Scan(&coolingUntil, &priority)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read recovered degradation state: %w", err)
	}
	if priority != 1 || coolingUntil < 0 || (coolingUntil > 0 && coolingUntil > time.Now().UnixMilli()) {
		return nil
	}
	if _, err := store.database.Exec(`DELETE FROM degradation_states WHERE auth_index = ?`, authIndex); err != nil {
		return fmt.Errorf("clear recovered degradation state: %w", err)
	}
	return nil
}

func (store *guardianStore) listDegradations() ([]degradationState, error) {
	rows, err := store.database.Query(`SELECT auth_index, auth_name, count, last_reason, last_request_id, last_seen, cooling_until FROM degradation_states ORDER BY count DESC, last_seen DESC`)
	if err != nil {
		return nil, fmt.Errorf("list degradation states: %w", err)
	}
	defer rows.Close()
	items := make([]degradationState, 0)
	for rows.Next() {
		var item degradationState
		if err := rows.Scan(&item.AuthIndex, &item.AuthName, &item.Count, &item.LastReason, &item.LastRequest, &item.LastSeen, &item.CoolingUntil); err != nil {
			return nil, fmt.Errorf("scan degradation state: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *guardianStore) summary() (map[string]any, error) {
	rows, err := store.database.Query(`SELECT status, COUNT(*) FROM nodes WHERE scope = 'inspection' GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("read node summary: %w", err)
	}
	defer rows.Close()
	counts := map[string]int64{statusUninspected: 0, statusInspecting: 0, statusHealthy: 0, statusUnhealthy: 0, statusDisabled: 0}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scan node summary: %w", err)
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate node summary: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close node summary: %w", err)
	}
	guardRows, err := store.database.Query(`SELECT status, COUNT(*) FROM nodes WHERE scope = 'guard' GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("read guard node summary: %w", err)
	}
	guardCounts := map[string]int64{statusUninspected: 0, statusProbing: 0, statusKeepaliveProbing: 0, statusReviveProbing: 0, statusHealthy: 0, statusHealthyCandidate: 0, statusHealthyFallback: 0, statusConnected: 0, statusCooldown: 0, statusUnhealthy: 0}
	for guardRows.Next() {
		var status string
		var count int64
		if err := guardRows.Scan(&status, &count); err != nil {
			guardRows.Close()
			return nil, fmt.Errorf("scan guard node summary: %w", err)
		}
		guardCounts[status] = count
	}
	if err := guardRows.Err(); err != nil {
		guardRows.Close()
		return nil, fmt.Errorf("iterate guard node summary: %w", err)
	}
	if err := guardRows.Close(); err != nil {
		return nil, fmt.Errorf("close guard node summary: %w", err)
	}
	slotCounts, err := store.healthySlotSummary()
	if err != nil {
		return nil, err
	}
	settings, err := store.settings()
	if err != nil {
		return nil, err
	}
	slotCounts["primaryTotal"] = int64(settings.HealthySlotCount)
	slotCounts["candidateTotal"] = int64(settings.HealthyCandidateSlotCount)
	var accounts, degraded int64
	if err := store.database.QueryRow(`SELECT COUNT(*) FROM auth_bindings WHERE inspection_run_id = COALESCE((SELECT id FROM inspection_runs WHERE status = 'completed' ORDER BY id DESC LIMIT 1), 0)`).Scan(&accounts); err != nil {
		return nil, fmt.Errorf("count auth bindings: %w", err)
	}
	if err := store.database.QueryRow(`SELECT COUNT(*) FROM degradation_states`).Scan(&degraded); err != nil {
		return nil, fmt.Errorf("count degradation states: %w", err)
	}
	return map[string]any{"nodes": counts, "guardNodes": guardCounts, "healthySlots": slotCounts, "accountCount": accounts, "degradedAccountCount": degraded}, nil
}
