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
	defaultInspectionScheduleEnabled                    = false
	defaultInspectionScheduleMode                       = inspectionScheduleModeInterval
	defaultInspectionDailyTime                          = "04:00"
	defaultInspectionWorkerCount                        = 4
	defaultInspectionTimeoutSeconds                     = 60
	defaultWorkerCount                                  = 4
	defaultScheduleGroupCount                           = 4
	defaultDebugEnabled                                 = false
	defaultRefreshIntervalSeconds                       = 30
	defaultKeepaliveWorkerCount                         = 8
	defaultKeepaliveIntervalSeconds                     = 1800
	defaultReviveIntervalSeconds                        = 1800
	defaultProbeRetryCount                              = 3
	defaultMaxReviveFailureCount                        = 3
	defaultHealthySlotCount                             = 50
	defaultHealthyCandidateCount                        = 20
	defaultHealthySlotMaxAgeMinutes                     = 350
	defaultRealtimeGuardTTFBSeconds                     = 5.0
	defaultRealtimeGuardGenerationSeconds               = 1.25
	defaultRealtimeGuardTokenThreshold                  = 300
	defaultQualityHardTPS                               = 1000.0
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
	maxInspectionWorkerCount                            = 64
	maxInspectionTimeoutSeconds                         = 600
	maxProbeWorkers                                     = 64
	maxScheduleGroupCount                               = 1000
	maxRefreshIntervalSeconds                           = 3600
	maxKeepaliveIntervalSeconds                         = 86400
	maxReviveIntervalSeconds                            = 86400
	maxMaxReviveFailureCount                            = 100
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

	inspectionScheduleModeInterval  = "interval"
	inspectionScheduleModeDailyTime = "daily_time"

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

var logURLPattern = regexp.MustCompile(`(?i)\b(?:https?|socks5h?)://[^\s"'<>]+`)

type pluginSettings struct {
	WorkerCount                                  int
	ScheduleGroupCount                           int
	DebugEnabled                                 bool
	RefreshIntervalSeconds                       int
	InspectionIntervalSeconds                    int
	InspectionScheduleEnabled                    bool
	InspectionScheduleMode                       string
	InspectionDailyTime                          string
	InspectionWorkerCount                        int
	InspectionTimeoutSeconds                     int
	KeepaliveWorkerCount                         int
	KeepaliveIntervalSeconds                     int
	ReviveIntervalSeconds                        int
	ProbeRetryCount                              int
	MaxReviveFailureCount                        int
	HealthySlotCount                             int
	HealthyCandidateSlotCount                    int
	HealthySlotMaxAgeMinutes                     int
	RealtimeGuardTTFBSeconds                     float64
	RealtimeGuardGenerationSeconds               float64
	RealtimeGuardTokenThreshold                  int
	QualityHardTPS                               float64
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

type ipBatch struct {
	ID                  string `json:"id"`
	SequenceNumber      int64  `json:"sequenceNumber"`
	CreatedAt           int64  `json:"createdAt"`
	ExpiresAt           int64  `json:"expiresAt"`
	TotalCount          int64  `json:"totalCount"`
	DuplicateCount      int64  `json:"duplicateCount"`
	InputErrorCount     int64  `json:"inputErrorCount"`
	CompletedCount      int64  `json:"completedCount"`
	CurrentHealthyCount int64  `json:"currentHealthyCount"`
}

type authBinding struct {
	AuthIndex          string `json:"authIndex"`
	AuthName           string `json:"authName"`
	SlotID             int64  `json:"slotId"`
	NodeID             int64  `json:"nodeId"`
	ProxyURL           string `json:"proxyUrl"`
	ExitIP             string `json:"exitIp"`
	Status             string `json:"status"`
	Priority           int    `json:"priority"`
	Success            int64  `json:"success"`
	Failed             int64  `json:"failed"`
	UpdatedAt          int64  `json:"updatedAt"`
	LastChecked        int64  `json:"lastChecked"`
	InspectionRunID    int64  `json:"-"`
	AccountType        string `json:"accountType"`
	ScheduleGroup      *int   `json:"scheduleGroup,omitempty"`
	LastInspection     int64  `json:"lastInspection"`
	ProxyWriteAttempts int    `json:"-"`
	ProxyWriteError    string `json:"-"`
	ProxyWriteAt       int64  `json:"-"`
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

type degradationPage struct {
	Items      []degradationState `json:"items"`
	Total      int                `json:"total"`
	Page       int                `json:"page"`
	PageSize   int                `json:"pageSize"`
	TotalPages int                `json:"totalPages"`
}

type pluginLog struct {
	ID           int64  `json:"id"`
	CreatedAt    int64  `json:"createdAt"`
	Level        string `json:"level"`
	Event        string `json:"event"`
	Message      string `json:"message"`
	Detail       string `json:"detail"`
	Category     string `json:"category"`
	GroupID      string `json:"groupId"`
	Status       string `json:"status"`
	RequestLogID string `json:"requestLogId"`
	NodeID       int64  `json:"nodeId"`
	NodeName     string `json:"nodeName"`
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
	database                *sql.DB
	path                    string
	mutex                   sync.Mutex
	realtimeDegradationLock sync.Mutex
	authDistributionMutex   sync.Mutex
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
CREATE TABLE IF NOT EXISTS account_inspection_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    trigger_type TEXT NOT NULL DEFAULT 'manual',
    started_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    total INTEGER NOT NULL DEFAULT 0,
    processed INTEGER NOT NULL DEFAULT 0,
    probed INTEGER NOT NULL DEFAULT 0,
    healthy INTEGER NOT NULL DEFAULT 0,
    quota_exhausted INTEGER NOT NULL DEFAULT 0,
    abnormal INTEGER NOT NULL DEFAULT 0,
    skipped INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_account_inspection_runs_started ON account_inspection_runs(started_at DESC, id DESC);
CREATE TABLE IF NOT EXISTS account_inspection_results (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL,
    account_key TEXT NOT NULL,
    file_name TEXT NOT NULL DEFAULT '',
    display_account TEXT NOT NULL DEFAULT '',
    auth_index TEXT NOT NULL,
    account_id TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT 'xai',
    disabled INTEGER NOT NULL DEFAULT 0,
    probed INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL DEFAULT 'keep',
    action_reason TEXT NOT NULL DEFAULT '',
    action_status TEXT NOT NULL DEFAULT 'none',
    executed_action TEXT NOT NULL DEFAULT '',
    action_error TEXT NOT NULL DEFAULT '',
    status_code INTEGER,
    used_percent REAL,
    is_quota INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    plan_type TEXT NOT NULL DEFAULT 'unknown',
    quota_windows_json TEXT NOT NULL DEFAULT '[]',
    monthly_limit_cents REAL,
    monthly_used_cents REAL,
    error_kind TEXT NOT NULL DEFAULT '',
    error_detail TEXT NOT NULL DEFAULT '',
    schedule_group INTEGER,
    priority INTEGER,
    original_priority INTEGER,
    recover_at_ms INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    FOREIGN KEY(run_id) REFERENCES account_inspection_runs(id) ON DELETE CASCADE,
    UNIQUE(run_id, auth_index)
);
CREATE INDEX IF NOT EXISTS idx_account_inspection_results_run ON account_inspection_results(run_id, id);
CREATE INDEX IF NOT EXISTS idx_account_inspection_results_auth ON account_inspection_results(auth_index, id DESC);
CREATE TABLE IF NOT EXISTS account_degradation_probe_results (
    auth_index TEXT PRIMARY KEY,
    check_id TEXT NOT NULL,
    detected_at_ms INTEGER NOT NULL,
    result_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS account_inspection_profiles (
    auth_index TEXT PRIMARY KEY,
    account_type TEXT NOT NULL,
    updated_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS account_inspection_priority_adjustments (
    auth_index TEXT PRIMARY KEY,
    file_name TEXT NOT NULL DEFAULT '',
    original_priority INTEGER,
    adjusted_priority INTEGER NOT NULL,
    recover_at_ms INTEGER NOT NULL
);
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
CREATE TABLE IF NOT EXISTS auth_selection_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    selected_at INTEGER NOT NULL,
    auth_index TEXT NOT NULL,
    auth_identity TEXT NOT NULL DEFAULT '',
    selection_source TEXT NOT NULL,
    node_id INTEGER NOT NULL,
    slot_id INTEGER NOT NULL,
    was_success INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_auth_selection_history_node ON auth_selection_history(node_id, was_success, selected_at DESC, id DESC);
CREATE TABLE IF NOT EXISTS auth_bindings (
    auth_index TEXT PRIMARY KEY,
    auth_name TEXT NOT NULL,
    slot_id INTEGER NOT NULL DEFAULT 0,
    node_id INTEGER NOT NULL DEFAULT 0,
    proxy_url TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    success_count INTEGER NOT NULL DEFAULT 0,
    failed_count INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    last_checked INTEGER NOT NULL DEFAULT 0,
    proxy_write_attempts INTEGER NOT NULL DEFAULT 0,
    proxy_write_error TEXT NOT NULL DEFAULT '',
    proxy_write_at INTEGER NOT NULL DEFAULT 0
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
CREATE TABLE IF NOT EXISTS realtime_degradation_priority_adjustments (
    auth_index TEXT PRIMARY KEY,
    file_name TEXT NOT NULL DEFAULT '',
    original_priority INTEGER,
    adjusted_priority INTEGER NOT NULL,
    recover_at_ms INTEGER NOT NULL
);
INSERT OR IGNORE INTO realtime_degradation_priority_adjustments(auth_index, file_name, original_priority, adjusted_priority, recover_at_ms)
SELECT auth_index, file_name, original_priority, adjusted_priority, recover_at_ms
FROM account_inspection_priority_adjustments WHERE adjusted_priority = -8;
DELETE FROM account_inspection_priority_adjustments WHERE adjusted_priority = -8;
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
    node_name TEXT NOT NULL DEFAULT '',
    request_log_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_plugin_logs_created ON plugin_logs(created_at DESC, id DESC);
INSERT OR IGNORE INTO plugin_settings(setting_key, setting_value) VALUES
    ('inspection_interval_seconds', '0'),
    ('inspection_schedule_mode', 'interval'),
    ('inspection_daily_time', '04:00'),
    ('inspection_worker_count', '4'),
    ('inspection_timeout_seconds', '60'),
    ('worker_count', '4'),
    ('schedule_group_count', '4'),
    ('debug_enabled', '0'),
    ('refresh_interval_seconds', '30'),
    ('keepalive_worker_count', '8'),
    ('keepalive_interval_seconds', '1800'),
    ('revive_interval_seconds', '1800'),
    ('probe_retry_count', '3'),
    ('max_revive_failure_count', '3'),
    ('healthy_slot_count', '50'),
    ('healthy_candidate_slot_count', '20'),
    ('healthy_slot_max_age_minutes', '350'),
    ('realtime_guard_ttfb_seconds', '5'),
    ('realtime_guard_generation_seconds', '1.25'),
    ('realtime_guard_token_threshold', '300'),
    ('quality_hard_tps', '1000'),
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
	if _, err := store.database.Exec(`INSERT OR IGNORE INTO plugin_settings(setting_key, setting_value)
SELECT 'inspection_schedule_enabled', CASE WHEN CAST(setting_value AS INTEGER) > 0 THEN '1' ELSE '0' END
FROM plugin_settings WHERE setting_key = 'inspection_interval_seconds'`); err != nil {
		return fmt.Errorf("migrate inspection schedule enabled setting: %w", err)
	}
	if _, err := store.database.Exec(`DELETE FROM plugin_settings WHERE setting_key IN ('quality_worker_count', 'quality_probe_timeout_seconds', 'quality_probe_model', 'quality_soft_tps', 'quality_llm_probe_enabled')`); err != nil {
		return fmt.Errorf("remove obsolete quality settings: %w", err)
	}
	if err := store.ensurePluginLogColumns(); err != nil {
		return err
	}
	if err := store.ensureAccountInspectionResultColumns(); err != nil {
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
	if err := store.ensureAuthSelectionHistoryColumns(); err != nil {
		return err
	}
	if err := store.ensureNodeReviveColumns(); err != nil {
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
		{name: "request_log_id", sql: `ALTER TABLE plugin_logs ADD COLUMN request_log_id TEXT NOT NULL DEFAULT ''`},
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

func (store *guardianStore) ensureAccountInspectionResultColumns() error {
	rows, err := store.database.Query(`PRAGMA table_info(account_inspection_results)`)
	if err != nil {
		return fmt.Errorf("inspect account inspection result schema: %w", err)
	}
	hasProbed := false
	for rows.Next() {
		var columnID, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&columnID, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan account inspection result schema: %w", err)
		}
		if columnName == "probed" {
			hasProbed = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate account inspection result schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close account inspection result schema: %w", err)
	}
	if !hasProbed {
		if _, err := store.database.Exec(`ALTER TABLE account_inspection_results ADD COLUMN probed INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add account inspection result probed column: %w", err)
		}
		if _, err := store.database.Exec(`UPDATE account_inspection_results SET probed = 1 WHERE probed = 0 AND status <> 'skipped' AND action_status <> 'skipped'`); err != nil {
			return fmt.Errorf("backfill account inspection probe candidates: %w", err)
		}
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
		{name: "slot_id", definition: "integer not null default 0"},
		{name: "inspection_run_id", definition: "integer not null default 0"},
		{name: "account_type", definition: "text not null default ''"},
		{name: "schedule_group", definition: "integer"},
		{name: "last_inspection", definition: "integer not null default 0"},
		{name: "proxy_write_attempts", definition: "integer not null default 0"},
		{name: "proxy_write_error", definition: "text not null default ''"},
		{name: "proxy_write_at", definition: "integer not null default 0"},
	}
	for _, column := range columns {
		if _, exists := existingColumns[column.name]; exists {
			continue
		}
		if _, err := store.database.Exec(`ALTER TABLE auth_bindings ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
			return fmt.Errorf("add auth binding column %s: %w", column.name, err)
		}
	}
	if _, err := store.database.Exec(`UPDATE auth_bindings
SET slot_id = COALESCE((SELECT healthy_slots.slot_id FROM healthy_slots WHERE healthy_slots.node_id = auth_bindings.node_id AND healthy_slots.slot_kind = 'primary' LIMIT 1), 0)
WHERE auth_bindings.node_id <> 0 AND auth_bindings.slot_id = 0`); err != nil {
		return fmt.Errorf("backfill auth binding slots: %w", err)
	}
	return nil
}

func (store *guardianStore) ensureAuthSelectionHistoryColumns() error {
	rows, err := store.database.Query(`PRAGMA table_info(auth_selection_history)`)
	if err != nil {
		return fmt.Errorf("inspect auth selection history schema: %w", err)
	}
	existingColumns := make(map[string]struct{})
	for rows.Next() {
		var columnID, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&columnID, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan auth selection history schema: %w", err)
		}
		existingColumns[columnName] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate auth selection history schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close auth selection history schema: %w", err)
	}
	if _, exists := existingColumns["auth_identity"]; exists {
		return nil
	}
	if _, err := store.database.Exec(`ALTER TABLE auth_selection_history ADD COLUMN auth_identity TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add auth selection identity column: %w", err)
	}
	return nil
}

func (store *guardianStore) ensureNodeReviveColumns() error {
	rows, err := store.database.Query(`PRAGMA table_info(nodes)`)
	if err != nil {
		return fmt.Errorf("inspect node revive schema: %w", err)
	}
	existingColumns := make(map[string]struct{})
	for rows.Next() {
		var columnID, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&columnID, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan node revive schema: %w", err)
		}
		existingColumns[columnName] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate node revive schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close node revive schema: %w", err)
	}
	if _, exists := existingColumns["revive_failure_count"]; exists {
		return nil
	}
	if _, err := store.database.Exec(`ALTER TABLE nodes ADD COLUMN revive_failure_count INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add node revive failure column: %w", err)
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
		case "inspection_schedule_enabled":
			settings.InspectionScheduleEnabled = strings.TrimSpace(value) == "1" || strings.EqualFold(strings.TrimSpace(value), "true")
		case "inspection_schedule_mode":
			settings.InspectionScheduleMode = strings.TrimSpace(value)
		case "inspection_daily_time":
			settings.InspectionDailyTime = strings.TrimSpace(value)
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
		case "quality_hard_tps":
			parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if parseErr == nil {
				settings.QualityHardTPS = parsed
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
			case "inspection_worker_count":
				settings.InspectionWorkerCount = parsed
			case "inspection_timeout_seconds":
				settings.InspectionTimeoutSeconds = parsed
			case "keepalive_worker_count":
				settings.KeepaliveWorkerCount = parsed
			case "keepalive_interval_seconds":
				settings.KeepaliveIntervalSeconds = parsed
			case "revive_interval_seconds":
				settings.ReviveIntervalSeconds = parsed
			case "probe_retry_count":
				settings.ProbeRetryCount = parsed
			case "max_revive_failure_count":
				settings.MaxReviveFailureCount = parsed
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
		"inspection_schedule_enabled":                        strconv.FormatBool(settings.InspectionScheduleEnabled),
		"inspection_schedule_mode":                           settings.InspectionScheduleMode,
		"inspection_daily_time":                              settings.InspectionDailyTime,
		"inspection_worker_count":                            strconv.Itoa(settings.InspectionWorkerCount),
		"inspection_timeout_seconds":                         strconv.Itoa(settings.InspectionTimeoutSeconds),
		"keepalive_worker_count":                             strconv.Itoa(settings.KeepaliveWorkerCount),
		"keepalive_interval_seconds":                         strconv.Itoa(settings.KeepaliveIntervalSeconds),
		"revive_interval_seconds":                            strconv.Itoa(settings.ReviveIntervalSeconds),
		"probe_retry_count":                                  strconv.Itoa(settings.ProbeRetryCount),
		"max_revive_failure_count":                           strconv.Itoa(settings.MaxReviveFailureCount),
		"healthy_slot_count":                                 strconv.Itoa(settings.HealthySlotCount),
		"healthy_candidate_slot_count":                       strconv.Itoa(settings.HealthyCandidateSlotCount),
		"healthy_slot_max_age_minutes":                       strconv.Itoa(settings.HealthySlotMaxAgeMinutes),
		"realtime_guard_ttfb_seconds":                        strconv.FormatFloat(settings.RealtimeGuardTTFBSeconds, 'f', -1, 64),
		"realtime_guard_generation_seconds":                  strconv.FormatFloat(settings.RealtimeGuardGenerationSeconds, 'f', -1, 64),
		"realtime_guard_token_threshold":                     strconv.Itoa(settings.RealtimeGuardTokenThreshold),
		"quality_hard_tps":                                   strconv.FormatFloat(settings.QualityHardTPS, 'f', -1, 64),
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
	if settings.InspectionScheduleMode != inspectionScheduleModeInterval && settings.InspectionScheduleMode != inspectionScheduleModeDailyTime {
		return fmt.Errorf("inspection schedule mode is invalid")
	}
	parsedDailyTime, err := time.Parse("15:04", settings.InspectionDailyTime)
	if err != nil || parsedDailyTime.Format("15:04") != settings.InspectionDailyTime {
		return fmt.Errorf("inspection daily time must use HH:MM")
	}
	if settings.InspectionScheduleEnabled && settings.InspectionScheduleMode == inspectionScheduleModeInterval && settings.InspectionIntervalSeconds < 1 {
		return fmt.Errorf("enabled inspection interval must be positive")
	}
	if settings.InspectionWorkerCount < 1 || settings.InspectionWorkerCount > maxInspectionWorkerCount {
		return fmt.Errorf("inspection worker count is out of range")
	}
	if settings.InspectionTimeoutSeconds < 1 || settings.InspectionTimeoutSeconds > maxInspectionTimeoutSeconds {
		return fmt.Errorf("inspection timeout is out of range")
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
	if settings.MaxReviveFailureCount < 1 || settings.MaxReviveFailureCount > maxMaxReviveFailureCount {
		return fmt.Errorf("max revive failure count is out of range")
	}
	if settings.HealthySlotCount < 1 || settings.HealthySlotCount > maxSlotCount || settings.HealthyCandidateSlotCount < 0 || settings.HealthyCandidateSlotCount > maxSlotCount || settings.HealthySlotCount+settings.HealthyCandidateSlotCount > maxSlotCount {
		return fmt.Errorf("healthy slot counts are out of range")
	}
	if settings.HealthySlotMaxAgeMinutes < 1 || settings.HealthySlotMaxAgeMinutes > maxHealthySlotMaxAgeMinutes {
		return fmt.Errorf("healthy slot max age is out of range")
	}
	if math.IsNaN(settings.QualityHardTPS) || math.IsInf(settings.QualityHardTPS, 0) || settings.QualityHardTPS <= 0 {
		return fmt.Errorf("realtime guard hard TPS must be positive")
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

func (store *guardianStore) appendLogWithRequestLogID(level, event, message, detail, requestLogID string) error {
	category, status := classifyLogEvent(event, level)
	return store.appendCategorizedLogWithRequestLogID(category, "", status, level, event, 0, "", message, detail, requestLogID)
}

func (store *guardianStore) appendRoundLog(category string, roundID int64, status, level, event, message, detail string) error {
	return store.appendCategorizedLog(category, strconv.FormatInt(roundID, 10), status, level, event, 0, "", message, detail)
}

func (store *guardianStore) appendRoundNodeLog(category string, roundID int64, status, level, event string, nodeID int64, nodeName, message, detail string) error {
	return store.appendCategorizedLog(category, strconv.FormatInt(roundID, 10), status, level, event, nodeID, nodeName, message, detail)
}

func (store *guardianStore) appendCategorizedLog(category, groupID, status, level, event string, nodeID int64, nodeName, message, detail string) error {
	return store.appendCategorizedLogWithRequestLogID(category, groupID, status, level, event, nodeID, nodeName, message, detail, "")
}

func (store *guardianStore) appendCategorizedLogWithRequestLogID(category, groupID, status, level, event string, nodeID int64, nodeName, message, detail, requestLogID string) error {
	_, err := store.database.Exec(`INSERT INTO plugin_logs(created_at, level, event, message, detail, category, group_id, log_status, node_id, node_name, request_log_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, time.Now().UnixMilli(), level, event, sanitizeLogText(message), sanitizeLogText(detail), category, groupID, status, nodeID, sanitizeLogText(nodeName), shortRequestLogID(requestLogID))
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

func shortRequestLogID(requestLogID string) string {
	requestLogID = strings.TrimSpace(requestLogID)
	if len(requestLogID) > 8 {
		return requestLogID[len(requestLogID)-8:]
	}
	return requestLogID
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
		conditions = append(conditions, `(CAST(id AS TEXT) LIKE ? OR lower(request_log_id) LIKE ? OR lower(level) LIKE ? OR lower(event) LIKE ? OR lower(node_name) LIKE ? OR lower(message) LIKE ? OR lower(detail) LIKE ?)`)
		for index := 0; index < 7; index++ {
			args = append(args, pattern)
		}
	}
	args = append(args, limit)
	query := `SELECT id, created_at, level, event, message, detail, category, group_id, log_status, request_log_id, node_id, node_name FROM plugin_logs WHERE ` + strings.Join(conditions, " AND ") + ` ORDER BY id DESC LIMIT ?`
	rows, err := store.database.Query(query, args...)
	if err != nil {
		return pluginLogList{}, fmt.Errorf("list plugin logs: %w", err)
	}
	defer rows.Close()
	items := make([]pluginLog, 0)
	for rows.Next() {
		var item pluginLog
		if err := rows.Scan(&item.ID, &item.CreatedAt, &item.Level, &item.Event, &item.Message, &item.Detail, &item.Category, &item.GroupID, &item.Status, &item.RequestLogID, &item.NodeID, &item.NodeName); err != nil {
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

	if _, err := tx.Exec(`DELETE FROM healthy_slots
WHERE node_id IN (
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
  )`, cutoff, cutoff); err != nil {
		return 0, 0, fmt.Errorf("clear expired IP batch slots: %w", err)
	}
	if _, err := tx.Exec(`UPDATE auth_bindings SET slot_id = 0, node_id = 0, proxy_url = '', updated_at = ?
WHERE node_id IN (
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
  )`, time.Now().UnixMilli(), cutoff, cutoff); err != nil {
		return 0, 0, fmt.Errorf("clear expired IP batch auth bindings: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM auth_selection_history
WHERE node_id IN (
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
  )`, cutoff, cutoff); err != nil {
		return 0, 0, fmt.Errorf("clear expired IP batch auth history: %w", err)
	}

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

	batchResult, err := tx.Exec(`DELETE FROM ip_batches WHERE created_at <= ?`, cutoff)
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
	settings, err := store.settings()
	if err != nil {
		return nil, fmt.Errorf("read IP batch retention settings: %w", err)
	}
	rows, err := store.database.Query(`
SELECT batches.batch_id, batches.sequence_number, batches.created_at, batches.total_count, batches.duplicate_count, batches.input_error_count,
       COALESCE(SUM(CASE WHEN nodes.last_checked > 0 THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN nodes.status IN (?, ?, ?, ?) THEN 1 ELSE 0 END), 0)
FROM ip_batches AS batches
LEFT JOIN ip_batch_nodes AS batch_nodes ON batch_nodes.batch_id = batches.batch_id
LEFT JOIN nodes ON nodes.id = batch_nodes.node_id AND nodes.scope = 'guard'
GROUP BY batches.batch_id, batches.sequence_number, batches.created_at, batches.total_count, batches.duplicate_count, batches.input_error_count
ORDER BY batches.sequence_number DESC, batches.created_at DESC, batches.batch_id DESC
LIMIT ?`, statusHealthy, statusConnected, statusHealthyCandidate, statusHealthyFallback, maxIPBatches)
	if err != nil {
		return nil, fmt.Errorf("list IP batches: %w", err)
	}
	defer rows.Close()
	items := make([]ipBatch, 0)
	for rows.Next() {
		var item ipBatch
		if err := rows.Scan(&item.ID, &item.SequenceNumber, &item.CreatedAt, &item.TotalCount, &item.DuplicateCount, &item.InputErrorCount, &item.CompletedCount, &item.CurrentHealthyCount); err != nil {
			return nil, fmt.Errorf("scan IP batch: %w", err)
		}
		item.ExpiresAt = time.UnixMilli(item.CreatedAt).AddDate(0, 0, settings.IPBatchRetentionDays).UnixMilli()
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

func (store *guardianStore) upsertAuthBindings(bindings []authBinding) error {
	return store.upsertAuthBindingsWith(bindings, true)
}

// upsertAuthBindingsSubset saves part of the binding snapshot without pruning rows that are
// absent from the list. The realtime guard re-points a single healthy slot at a time, so
// pruning the table from a partial snapshot would drop every other account's binding.
func (store *guardianStore) upsertAuthBindingsSubset(bindings []authBinding) error {
	if len(bindings) == 0 {
		return nil
	}
	return store.upsertAuthBindingsWith(bindings, false)
}

func (store *guardianStore) upsertAuthBindingsWith(bindings []authBinding, pruneStale bool) error {
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
		_, err := tx.Exec(`INSERT INTO auth_bindings(auth_index, auth_name, slot_id, node_id, proxy_url, status, priority, success_count, failed_count, updated_at, last_checked, inspection_run_id, account_type, schedule_group, last_inspection)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(auth_index) DO UPDATE SET auth_name=excluded.auth_name, slot_id=CASE WHEN excluded.slot_id <> 0 THEN excluded.slot_id ELSE auth_bindings.slot_id END, node_id=CASE WHEN excluded.node_id <> 0 THEN excluded.node_id ELSE auth_bindings.node_id END, proxy_url=CASE WHEN excluded.proxy_url <> '' THEN excluded.proxy_url ELSE auth_bindings.proxy_url END, status=excluded.status, priority=excluded.priority, success_count=excluded.success_count, failed_count=excluded.failed_count, updated_at=excluded.updated_at, last_checked=CASE WHEN excluded.last_checked <> 0 THEN excluded.last_checked ELSE auth_bindings.last_checked END, inspection_run_id=CASE WHEN excluded.inspection_run_id <> 0 THEN excluded.inspection_run_id ELSE auth_bindings.inspection_run_id END, account_type=CASE WHEN excluded.account_type <> '' THEN excluded.account_type ELSE auth_bindings.account_type END, schedule_group=COALESCE(excluded.schedule_group, auth_bindings.schedule_group), last_inspection=CASE WHEN excluded.last_inspection <> 0 THEN excluded.last_inspection ELSE auth_bindings.last_inspection END`, binding.AuthIndex, binding.AuthName, binding.SlotID, binding.NodeID, binding.ProxyURL, binding.Status, binding.Priority, binding.Success, binding.Failed, binding.UpdatedAt, binding.LastChecked, binding.InspectionRunID, binding.AccountType, binding.ScheduleGroup, binding.LastInspection)
		if err != nil {
			return fmt.Errorf("save auth binding %s: %w", binding.AuthIndex, err)
		}
	}
	if pruneStale && len(bindings) > 0 {
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

func (store *guardianStore) replaceAuthDistribution(assignments map[string]authAssignment) error {
	tx, err := store.database.Begin()
	if err != nil {
		return fmt.Errorf("begin auth distribution replacement: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if _, err := tx.Exec(`UPDATE auth_bindings SET slot_id = 0, node_id = 0, proxy_url = '', updated_at = ?`, now); err != nil {
		return fmt.Errorf("clear auth distribution: %w", err)
	}
	for authIndex, assignment := range assignments {
		updated, err := tx.Exec(`UPDATE auth_bindings SET slot_id = ?, node_id = ?, proxy_url = ?, updated_at = ? WHERE auth_index = ?`, assignment.SlotID, assignment.NodeID, assignment.ProxyURL, now, authIndex)
		if err != nil {
			return fmt.Errorf("bind auth %s to healthy slot: %w", authIndex, err)
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return fmt.Errorf("read auth %s binding result: %w", authIndex, err)
		}
		if count != 1 {
			return fmt.Errorf("auth %s is not present in the current auth binding snapshot", authIndex)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit auth distribution replacement: %w", err)
	}
	return nil
}

func (store *guardianStore) recordAuthProxyWriteFailure(authIndex string, err error, attempts int) {
	_, _ = store.database.Exec(`UPDATE auth_bindings SET proxy_write_attempts = proxy_write_attempts + ?, proxy_write_error = ?, proxy_write_at = ?, status = 'write_failed', updated_at = ? WHERE auth_index = ?`, attempts, sanitizeLogText(err.Error()), time.Now().UnixMilli(), time.Now().UnixMilli(), authIndex)
}

func (store *guardianStore) recordAuthProxyWriteSuccess(authIndex string) {
	_, _ = store.database.Exec(`UPDATE auth_bindings SET proxy_write_error = '', proxy_write_at = ?, status = CASE WHEN status = 'write_failed' THEN 'available' ELSE status END, updated_at = ? WHERE auth_index = ?`, time.Now().UnixMilli(), time.Now().UnixMilli(), authIndex)
}

func (store *guardianStore) observeAuthAttempt(authIndex, authName, proxyURL string) error {
	redactedProxy := redactProxyURL(proxyURL)
	nodeID := int64(0)
	if strings.TrimSpace(proxyURL) != "" {
		_ = store.database.QueryRow(`SELECT id FROM nodes WHERE address = ? AND scope = 'guard' ORDER BY id DESC LIMIT 1`, strings.TrimSpace(proxyURL)).Scan(&nodeID)
	}
	if nodeID == 0 && redactedProxy != "" {
		_ = store.database.QueryRow(`SELECT id FROM nodes WHERE address = ? AND scope = 'guard' ORDER BY id DESC LIMIT 1`, redactedProxy).Scan(&nodeID)
	}
	observedProxy := strings.TrimSpace(proxyURL)
	if observedProxy == "" {
		observedProxy = redactedProxy
	}
	now := time.Now().UnixMilli()
	_, err := store.database.Exec(`INSERT INTO auth_bindings(auth_index, auth_name, slot_id, node_id, proxy_url, status, updated_at, last_checked)
VALUES (?, ?, 0, ?, ?, ?, ?, ?)
ON CONFLICT(auth_index) DO UPDATE SET auth_name=CASE WHEN excluded.auth_name <> '' THEN excluded.auth_name ELSE auth_bindings.auth_name END, slot_id=CASE WHEN excluded.node_id <> 0 AND excluded.node_id <> auth_bindings.node_id THEN 0 ELSE auth_bindings.slot_id END, node_id=CASE WHEN excluded.node_id <> 0 THEN excluded.node_id ELSE auth_bindings.node_id END, proxy_url=CASE WHEN excluded.proxy_url <> '' THEN excluded.proxy_url ELSE auth_bindings.proxy_url END, status=excluded.status, updated_at=excluded.updated_at, last_checked=excluded.last_checked`, authIndex, authName, nodeID, observedProxy, "observed", now, now)
	if err != nil {
		return fmt.Errorf("observe auth attempt: %w", err)
	}
	return nil
}

func (store *guardianStore) listAuthBindings(inspectionRunID int64) ([]authBinding, error) {
	rows, err := store.database.Query(`SELECT auth_bindings.auth_index, auth_bindings.auth_name, auth_bindings.slot_id, auth_bindings.node_id, auth_bindings.proxy_url, COALESCE(nodes.exit_ip, ''), auth_bindings.status, auth_bindings.priority, auth_bindings.success_count, auth_bindings.failed_count, auth_bindings.updated_at, auth_bindings.last_checked, auth_bindings.inspection_run_id, auth_bindings.account_type, auth_bindings.schedule_group, auth_bindings.last_inspection, auth_bindings.proxy_write_attempts, auth_bindings.proxy_write_error, auth_bindings.proxy_write_at FROM auth_bindings LEFT JOIN nodes ON nodes.id = auth_bindings.node_id AND nodes.scope = 'guard' WHERE (? = 0 OR auth_bindings.inspection_run_id = ?) ORDER BY auth_bindings.priority DESC, auth_bindings.auth_name ASC`, inspectionRunID, inspectionRunID)
	if err != nil {
		return nil, fmt.Errorf("list auth bindings: %w", err)
	}
	defer rows.Close()
	return scanAuthBindings(rows)
}

func (store *guardianStore) listAuthBindingsByNode(nodeID int64) ([]authBinding, error) {
	rows, err := store.database.Query(`SELECT auth_bindings.auth_index, auth_bindings.auth_name, auth_bindings.slot_id, auth_bindings.node_id, auth_bindings.proxy_url, COALESCE(nodes.exit_ip, ''), auth_bindings.status, auth_bindings.priority, auth_bindings.success_count, auth_bindings.failed_count, auth_bindings.updated_at, auth_bindings.last_checked, auth_bindings.inspection_run_id, auth_bindings.account_type, auth_bindings.schedule_group, auth_bindings.last_inspection, auth_bindings.proxy_write_attempts, auth_bindings.proxy_write_error, auth_bindings.proxy_write_at FROM auth_bindings INNER JOIN nodes ON nodes.id = auth_bindings.node_id AND nodes.scope = 'guard' WHERE auth_bindings.node_id = ? ORDER BY auth_bindings.slot_id, auth_bindings.priority DESC, auth_bindings.auth_name ASC`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list node auth bindings: %w", err)
	}
	defer rows.Close()
	return scanAuthBindings(rows)
}

func scanAuthBindings(rows *sql.Rows) ([]authBinding, error) {
	items := make([]authBinding, 0)
	for rows.Next() {
		var item authBinding
		var scheduleGroup sql.NullInt64
		if err := rows.Scan(&item.AuthIndex, &item.AuthName, &item.SlotID, &item.NodeID, &item.ProxyURL, &item.ExitIP, &item.Status, &item.Priority, &item.Success, &item.Failed, &item.UpdatedAt, &item.LastChecked, &item.InspectionRunID, &item.AccountType, &scheduleGroup, &item.LastInspection, &item.ProxyWriteAttempts, &item.ProxyWriteError, &item.ProxyWriteAt); err != nil {
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

func (store *guardianStore) degradationStateForAuth(authIndex string) (degradationState, bool, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	var state degradationState
	err := store.database.QueryRow(`SELECT auth_index, auth_name, count, last_reason, last_request_id, last_seen, cooling_until FROM degradation_states WHERE auth_index = ?`, authIndex).Scan(
		&state.AuthIndex, &state.AuthName, &state.Count, &state.LastReason, &state.LastRequest, &state.LastSeen, &state.CoolingUntil)
	if err == sql.ErrNoRows {
		return degradationState{}, false, nil
	}
	if err != nil {
		return degradationState{}, false, fmt.Errorf("read degradation state: %w", err)
	}
	return state, true, nil
}

func (store *guardianStore) restoreDegradationState(authIndex string, previous degradationState, existed bool) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if !existed {
		if _, err := store.database.Exec(`DELETE FROM degradation_states WHERE auth_index = ?`, authIndex); err != nil {
			return fmt.Errorf("rollback degradation state: %w", err)
		}
		return nil
	}
	_, err := store.database.Exec(`INSERT INTO degradation_states(auth_index, auth_name, count, last_reason, last_request_id, last_seen, cooling_until) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(auth_index) DO UPDATE SET auth_name=excluded.auth_name, count=excluded.count, last_reason=excluded.last_reason, last_request_id=excluded.last_request_id, last_seen=excluded.last_seen, cooling_until=excluded.cooling_until`,
		previous.AuthIndex, previous.AuthName, previous.Count, previous.LastReason, previous.LastRequest, previous.LastSeen, previous.CoolingUntil)
	if err != nil {
		return fmt.Errorf("rollback degradation state: %w", err)
	}
	return nil
}

func (store *guardianStore) recordDegradation(authIndex, authName, requestID, reason string, advanceAfterCooldown bool) (degradationState, bool, error) {
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
		return degradationState{}, false, fmt.Errorf("read degradation state: %w", err)
	}
	duplicateRequest := requestID != "" && requestID == state.LastRequest
	cooldownExpired := state.CoolingUntil > 0 && state.CoolingUntil <= now
	advance := state.Count < 3 && (state.CoolingUntil == 0 || (cooldownExpired && advanceAfterCooldown))
	advanced := false
	if !duplicateRequest && advance {
		state.Count++
		advanced = true
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
		return degradationState{}, false, fmt.Errorf("save degradation state: %w", err)
	}
	return state, advanced, nil
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

func (store *guardianStore) clearDegradationIfRecovered(authIndex string, priority int) (bool, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	var coolingUntil int64
	var count int
	err := store.database.QueryRow(`SELECT cooling_until, count FROM degradation_states WHERE auth_index = ?`, authIndex).Scan(&coolingUntil, &count)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read recovered degradation state: %w", err)
	}
	if count >= 3 || priority != 1 || coolingUntil < 0 || (coolingUntil > 0 && coolingUntil > time.Now().UnixMilli()) {
		return false, nil
	}
	result, err := store.database.Exec(`DELETE FROM degradation_states WHERE auth_index = ?`, authIndex)
	if err != nil {
		return false, fmt.Errorf("clear recovered degradation state: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read recovered degradation delete result: %w", err)
	}
	return deleted == 1, nil
}

func (store *guardianStore) listDegradations(page, pageSize int) (degradationPage, error) {
	if page < 1 {
		page = 1
	}
	if pageSize != 20 && pageSize != 50 && pageSize != 100 {
		pageSize = 20
	}
	var total int
	if err := store.database.QueryRow(`SELECT COUNT(*) FROM degradation_states`).Scan(&total); err != nil {
		return degradationPage{}, fmt.Errorf("count degradation states: %w", err)
	}
	rows, err := store.database.Query(`SELECT auth_index, auth_name, count, last_reason, last_request_id, last_seen, cooling_until FROM degradation_states ORDER BY count DESC, last_seen DESC LIMIT ? OFFSET ?`, pageSize, (page-1)*pageSize)
	if err != nil {
		return degradationPage{}, fmt.Errorf("list degradation states: %w", err)
	}
	defer rows.Close()
	items := make([]degradationState, 0)
	for rows.Next() {
		var item degradationState
		if err := rows.Scan(&item.AuthIndex, &item.AuthName, &item.Count, &item.LastReason, &item.LastRequest, &item.LastSeen, &item.CoolingUntil); err != nil {
			return degradationPage{}, fmt.Errorf("scan degradation state: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return degradationPage{}, fmt.Errorf("read degradation states: %w", err)
	}
	return degradationPage{Items: items, Total: total, Page: page, PageSize: pageSize, TotalPages: max(1, (total+pageSize-1)/pageSize)}, nil
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
	if err := store.database.QueryRow(`SELECT COUNT(DISTINCT auth_bindings.auth_index) FROM auth_bindings
INNER JOIN healthy_slots ON healthy_slots.slot_id = auth_bindings.slot_id AND healthy_slots.node_id = auth_bindings.node_id AND healthy_slots.slot_kind = 'primary'
INNER JOIN nodes ON nodes.id = healthy_slots.node_id AND nodes.scope = 'guard'
WHERE nodes.status = 'healthy' OR (nodes.status = 'keepalive_probing' AND EXISTS (
    SELECT 1 FROM keepalive_round_nodes AS round_nodes
    INNER JOIN keepalive_rounds AS rounds ON rounds.id = round_nodes.round_id
    WHERE round_nodes.node_id = nodes.id
      AND round_nodes.completed_at = 0
      AND rounds.status = 'running'
      AND round_nodes.previous_status = 'healthy'
))`).Scan(&accounts); err != nil {
		return nil, fmt.Errorf("count auth bindings: %w", err)
	}
	if err := store.database.QueryRow(`SELECT COUNT(*) FROM degradation_states`).Scan(&degraded); err != nil {
		return nil, fmt.Errorf("count degradation states: %w", err)
	}
	return map[string]any{"nodes": counts, "guardNodes": guardCounts, "healthySlots": slotCounts, "accountCount": accounts, "degradedAccountCount": degraded}, nil
}
