package main

import (
	"database/sql"
	"fmt"
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
	defaultDatabasePath              = "/opt/cli-proxy-api/plugin-data/xai-guardian/xai-guardian.sqlite3"
	defaultInspectionIntervalSeconds = 0
	defaultFirstPayloadTimeout       = 120
	defaultProgressTimeout           = 500
	defaultMinSummaryChars           = 32
	defaultMinEncryptedBytes         = 64
	defaultEncryptedBytesPerToken    = 4
	defaultMinOutputTokens           = 8
	defaultHardTPS                   = 1000
	maxInspectionIntervalSeconds     = 86400
	degradationFirstCooling          = 24 * time.Hour
	degradationSecondCooling         = 48 * time.Hour
	degradationPermanentCoolingUntil = int64(-1)

	statusUninspected = "uninspected"
	statusInspecting  = "inspecting"
	statusHealthy     = "healthy"
	statusUnhealthy   = "unhealthy"
	statusDisabled    = "disabled"

	logLevelInfo  = "info"
	logLevelWarn  = "warn"
	logLevelError = "error"
)

var logURLPattern = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)

type pluginSettings struct {
	InspectionIntervalSeconds int
	FirstPayloadTimeout       int
	ProgressTimeout           int
	MinSummaryChars           int
	MinEncryptedBytes         int
	EncryptedBytesPerToken    int
	MinOutputTokens           int
	HardTPS                   int
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

type authBinding struct {
	AuthIndex   string `json:"authIndex"`
	AuthName    string `json:"authName"`
	NodeID      int64  `json:"nodeId"`
	ProxyURL    string `json:"proxyUrl"`
	Status      string `json:"status"`
	Priority    int    `json:"priority"`
	Success     int64  `json:"success"`
	Failed      int64  `json:"failed"`
	UpdatedAt   int64  `json:"updatedAt"`
	LastChecked int64  `json:"lastChecked"`
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
    address TEXT NOT NULL UNIQUE,
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
    detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_plugin_logs_created ON plugin_logs(created_at DESC, id DESC);
INSERT OR IGNORE INTO plugin_settings(setting_key, setting_value) VALUES
    ('inspection_interval_seconds', '0'),

    ('first_payload_timeout_seconds', '120'),
    ('progress_timeout_seconds', '500'),
    ('min_summary_chars', '32'),
    ('min_encrypted_bytes', '64'),
    ('encrypted_bytes_per_reasoning_token', '4'),
    ('min_output_tokens', '8'),
    ('hard_tps', '1000');
`)
	if err != nil {
		return fmt.Errorf("initialize sqlite database: %w", err)
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
	settings := pluginSettings{
		InspectionIntervalSeconds: defaultInspectionIntervalSeconds,

		FirstPayloadTimeout:    defaultFirstPayloadTimeout,
		ProgressTimeout:        defaultProgressTimeout,
		MinSummaryChars:        defaultMinSummaryChars,
		MinEncryptedBytes:      defaultMinEncryptedBytes,
		EncryptedBytesPerToken: defaultEncryptedBytesPerToken,
		MinOutputTokens:        defaultMinOutputTokens,
		HardTPS:                defaultHardTPS,
	}
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
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		switch key {
		case "inspection_interval_seconds":
			settings.InspectionIntervalSeconds = parsed

		case "first_payload_timeout_seconds":
			settings.FirstPayloadTimeout = parsed
		case "progress_timeout_seconds":
			settings.ProgressTimeout = parsed
		case "min_summary_chars":
			settings.MinSummaryChars = parsed
		case "min_encrypted_bytes":
			settings.MinEncryptedBytes = parsed
		case "encrypted_bytes_per_reasoning_token":
			settings.EncryptedBytesPerToken = parsed
		case "min_output_tokens":
			settings.MinOutputTokens = parsed
		case "hard_tps":
			settings.HardTPS = parsed
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
		"inspection_interval_seconds": strconv.Itoa(settings.InspectionIntervalSeconds),

		"first_payload_timeout_seconds":       strconv.Itoa(settings.FirstPayloadTimeout),
		"progress_timeout_seconds":            strconv.Itoa(settings.ProgressTimeout),
		"min_summary_chars":                   strconv.Itoa(settings.MinSummaryChars),
		"min_encrypted_bytes":                 strconv.Itoa(settings.MinEncryptedBytes),
		"encrypted_bytes_per_reasoning_token": strconv.Itoa(settings.EncryptedBytesPerToken),
		"min_output_tokens":                   strconv.Itoa(settings.MinOutputTokens),
		"hard_tps":                            strconv.Itoa(settings.HardTPS),
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

	if settings.FirstPayloadTimeout < 1 || settings.ProgressTimeout < 1 {
		return fmt.Errorf("stream timeouts must be positive")
	}
	if settings.MinSummaryChars < 1 || settings.MinEncryptedBytes < 1 || settings.EncryptedBytesPerToken < 1 || settings.MinOutputTokens < 1 || settings.HardTPS < 1 {
		return fmt.Errorf("xAI evidence thresholds must be positive")
	}
	return nil
}

func (store *guardianStore) appendLog(level, event, message, detail string) error {
	_, err := store.database.Exec(`INSERT INTO plugin_logs(created_at, level, event, message, detail) VALUES (?, ?, ?, ?, ?)`, time.Now().UnixMilli(), level, event, sanitizeLogText(message), sanitizeLogText(detail))
	if err != nil {
		return fmt.Errorf("append plugin log: %w", err)
	}
	_, err = store.database.Exec(`DELETE FROM plugin_logs WHERE id NOT IN (SELECT id FROM plugin_logs ORDER BY id DESC LIMIT 1000)`)
	if err != nil {
		return fmt.Errorf("prune plugin logs: %w", err)
	}
	return nil
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

func (store *guardianStore) listLogs(limit int) ([]pluginLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := store.database.Query(`SELECT id, created_at, level, event, message, detail FROM plugin_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list plugin logs: %w", err)
	}
	defer rows.Close()
	items := make([]pluginLog, 0)
	for rows.Next() {
		var item pluginLog
		if err := rows.Scan(&item.ID, &item.CreatedAt, &item.Level, &item.Event, &item.Message, &item.Detail); err != nil {
			return nil, fmt.Errorf("scan plugin log: %w", err)
		}
		item.Message = sanitizeLogText(item.Message)
		item.Detail = sanitizeLogText(item.Detail)
		items = append(items, item)
	}
	return items, rows.Err()
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
		result, err := tx.Exec(`INSERT OR IGNORE INTO nodes(address, protocol, host, port, status, created_at) VALUES (?, ?, ?, ?, ?, ?)`, node.Address, node.Protocol, node.Host, node.Port, statusUninspected, time.Now().UnixMilli())
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

func (store *guardianStore) listNodes() ([]proxyNode, error) {
	rows, err := store.database.Query(`SELECT id, address, protocol, host, port, status, latency_ms, exit_ip, country, last_checked, last_error, created_at FROM nodes ORDER BY id DESC`)
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
	err := store.database.QueryRow(`SELECT id, address, protocol, host, port, status, latency_ms, exit_ip, country, last_checked, last_error, created_at FROM nodes WHERE id = ?`, id).Scan(&item.ID, &item.Address, &item.Protocol, &item.Host, &item.Port, &item.Status, &item.LatencyMS, &item.ExitIP, &item.Country, &item.LastChecked, &item.LastError, &item.CreatedAt)
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
	result, err := tx.Exec(`DELETE FROM nodes WHERE id = ?`, id)
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
	_, err := store.database.Exec(`UPDATE nodes SET status = ?, latency_ms = ?, exit_ip = ?, country = ?, last_checked = ?, last_error = ? WHERE id = ?`, result.Status, result.LatencyMS, result.ExitIP, result.Country, result.CheckedAt, result.Error, result.NodeID)
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
		_, err := tx.Exec(`INSERT INTO auth_bindings(auth_index, auth_name, node_id, proxy_url, status, priority, success_count, failed_count, updated_at, last_checked)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(auth_index) DO UPDATE SET auth_name=excluded.auth_name, node_id=CASE WHEN excluded.node_id <> 0 THEN excluded.node_id ELSE auth_bindings.node_id END, proxy_url=CASE WHEN excluded.proxy_url <> '' THEN excluded.proxy_url ELSE auth_bindings.proxy_url END, status=excluded.status, priority=excluded.priority, success_count=excluded.success_count, failed_count=excluded.failed_count, updated_at=excluded.updated_at, last_checked=excluded.last_checked`, binding.AuthIndex, binding.AuthName, binding.NodeID, binding.ProxyURL, binding.Status, binding.Priority, binding.Success, binding.Failed, binding.UpdatedAt, binding.LastChecked)
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
		_ = store.database.QueryRow(`SELECT id FROM nodes WHERE address = ?`, redactedProxy).Scan(&nodeID)
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

func (store *guardianStore) listAuthBindings() ([]authBinding, error) {
	rows, err := store.database.Query(`SELECT auth_index, auth_name, node_id, proxy_url, status, priority, success_count, failed_count, updated_at, last_checked FROM auth_bindings ORDER BY priority DESC, auth_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list auth bindings: %w", err)
	}
	defer rows.Close()
	items := make([]authBinding, 0)
	for rows.Next() {
		var item authBinding
		if err := rows.Scan(&item.AuthIndex, &item.AuthName, &item.NodeID, &item.ProxyURL, &item.Status, &item.Priority, &item.Success, &item.Failed, &item.UpdatedAt, &item.LastChecked); err != nil {
			return nil, fmt.Errorf("scan auth binding: %w", err)
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
	rows, err := store.database.Query(`SELECT status, COUNT(*) FROM nodes GROUP BY status`)
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
	var accounts, degraded int64
	if err := store.database.QueryRow(`SELECT COUNT(*) FROM auth_bindings`).Scan(&accounts); err != nil {
		return nil, fmt.Errorf("count auth bindings: %w", err)
	}
	if err := store.database.QueryRow(`SELECT COUNT(*) FROM degradation_states`).Scan(&degraded); err != nil {
		return nil, fmt.Errorf("count degradation states: %w", err)
	}
	return map[string]any{"nodes": counts, "accountCount": accounts, "degradedAccountCount": degraded}, nil
}
