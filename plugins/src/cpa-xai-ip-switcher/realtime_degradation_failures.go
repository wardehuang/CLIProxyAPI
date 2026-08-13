package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const realtimeDegradationReplacementThreshold = 2

type realtimeDegradationFailure struct {
	NodeID                  int64
	NodeName                string
	ProxyURL                string
	ConsecutiveFailureCount int
}

func (store *ipStore) ensureRealtimeDegradationFailureStorage() error {
	_, err := store.database.Exec(`
CREATE TABLE IF NOT EXISTS realtime_degradation_failures (
    proxy_url TEXT PRIMARY KEY,
    node_id INTEGER NOT NULL DEFAULT 0,
    input_ip TEXT NOT NULL DEFAULT '',
    exit_ip TEXT NOT NULL DEFAULT '',
    consecutive_failure_count INTEGER NOT NULL DEFAULT 0,
    first_failure_at INTEGER NOT NULL DEFAULT 0,
    last_failure_at INTEGER NOT NULL DEFAULT 0,
    last_failure_reason TEXT NOT NULL DEFAULT '',
    last_failure_quality_level TEXT NOT NULL DEFAULT '',
    last_success_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS realtime_degraded_auths (
    auth_index TEXT PRIMARY KEY,
    original_priority INTEGER,
    marked_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_realtime_degraded_auths_updated ON realtime_degraded_auths(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_realtime_degradation_failures_node ON realtime_degradation_failures(node_id);
CREATE INDEX IF NOT EXISTS idx_realtime_degradation_failures_updated ON realtime_degradation_failures(updated_at DESC);
`)
	if err != nil {
		return fmt.Errorf("initialize realtime degradation failure storage: %w", err)
	}
	return nil
}

func (store *ipStore) rememberRealtimeDegradedAuth(auth authFile) (*int, error) {
	if strings.TrimSpace(auth.Index) == "" {
		return nil, fmt.Errorf("实时降智账号缺少 auth_index")
	}
	var storedOriginalPriority sql.NullInt64
	err := store.database.QueryRow(`
SELECT original_priority
FROM realtime_degraded_auths
WHERE auth_index = ?`, auth.Index).Scan(&storedOriginalPriority)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("读取实时降智账号原 priority: %w", err)
	}
	if err == nil {
		if !storedOriginalPriority.Valid {
			return nil, nil
		}
		storedPriority := int(storedOriginalPriority.Int64)
		return &storedPriority, nil
	}
	var originalPriority any
	if auth.PrioritySet {
		originalPriority = auth.Priority
	}
	now := time.Now().UnixMilli()
	if _, err := store.database.Exec(`
INSERT INTO realtime_degraded_auths (auth_index, original_priority, marked_at, updated_at)
VALUES (?, ?, ?, ?)`, auth.Index, originalPriority, now, now); err != nil {
		return nil, fmt.Errorf("保存实时降智账号原 priority: %w", err)
	}
	if !auth.PrioritySet {
		return nil, nil
	}
	return &auth.Priority, nil
}

func (store *ipStore) recordRealtimeDegradationFailure(probe realtimeGuardProbe, decision realtimeGuardDecision) (realtimeDegradationFailure, error) {
	proxyURL := strings.TrimSpace(probe.ProxyURL)
	if proxyURL == "" {
		return realtimeDegradationFailure{}, fmt.Errorf("实时降智记录缺少 proxy_url")
	}
	transaction, err := store.database.Begin()
	if err != nil {
		return realtimeDegradationFailure{}, fmt.Errorf("开始实时降智记录事务: %w", err)
	}
	defer transaction.Rollback()

	failure := realtimeDegradationFailure{ProxyURL: proxyURL}
	var inputIP string
	var exitIP string
	err = transaction.QueryRow(`
SELECT id, node_name, input_ip, exit_ip
FROM ip_nodes
WHERE proxy_url = ?
LIMIT 1`, proxyURL).Scan(&failure.NodeID, &failure.NodeName, &inputIP, &exitIP)
	if err != nil && err != sql.ErrNoRows {
		return realtimeDegradationFailure{}, fmt.Errorf("查询实时降智节点: %w", err)
	}
	if err == sql.ErrNoRows {
		return realtimeDegradationFailure{}, fmt.Errorf("实时降智节点不存在: %s", proxyURL)
	}

	now := time.Now().UnixMilli()
	if _, err := transaction.Exec(`
INSERT INTO realtime_degradation_failures (
    proxy_url, node_id, input_ip, exit_ip, consecutive_failure_count,
    first_failure_at, last_failure_at, last_failure_reason,
    last_failure_quality_level, last_success_at, updated_at
) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, 0, ?)
ON CONFLICT(proxy_url) DO UPDATE SET
    node_id = excluded.node_id,
    input_ip = excluded.input_ip,
    exit_ip = excluded.exit_ip,
    consecutive_failure_count = realtime_degradation_failures.consecutive_failure_count + 1,
    last_failure_at = excluded.last_failure_at,
    last_failure_reason = excluded.last_failure_reason,
    last_failure_quality_level = excluded.last_failure_quality_level,
    updated_at = excluded.updated_at`,
		proxyURL,
		failure.NodeID,
		inputIP,
		exitIP,
		now,
		now,
		decision.Reason,
		decision.QualityLevel,
		now,
	); err != nil {
		return realtimeDegradationFailure{}, fmt.Errorf("保存实时降智记录: %w", err)
	}
	if err := transaction.QueryRow(`
SELECT consecutive_failure_count
FROM realtime_degradation_failures
WHERE proxy_url = ?`, proxyURL).Scan(&failure.ConsecutiveFailureCount); err != nil {
		return realtimeDegradationFailure{}, fmt.Errorf("读取实时降智计数: %w", err)
	}
	if _, err := transaction.Exec(`
DELETE FROM realtime_degradation_failures
WHERE proxy_url IN (
    SELECT proxy_url
    FROM realtime_degradation_failures
    ORDER BY updated_at DESC, proxy_url DESC
    LIMIT -1 OFFSET 1000
)`); err != nil {
		return realtimeDegradationFailure{}, fmt.Errorf("清理实时降智记录: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return realtimeDegradationFailure{}, fmt.Errorf("提交实时降智记录: %w", err)
	}
	return failure, nil
}

func (store *ipStore) clearRealtimeDegradationFailure(proxyURL string) error {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	_, err := store.database.Exec(`
UPDATE realtime_degradation_failures
SET consecutive_failure_count = 0,
    first_failure_at = 0,
    last_success_at = ?,
    last_failure_reason = '',
    last_failure_quality_level = '',
    updated_at = ?
WHERE proxy_url = ?`, time.Now().UnixMilli(), time.Now().UnixMilli(), proxyURL)
	if err != nil {
		return fmt.Errorf("清零实时降智记录: %w", err)
	}
	return nil
}
