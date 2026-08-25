package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	grok2apiDegradedEgressNodesPath      = "/api/admin/v1/settings/console-guard/degraded-egress-nodes"
	grok2apiDegradedEgressNodesClearPath = "/api/admin/v1/settings/console-guard/degraded-egress-nodes/clear"
)

type grok2apiDegradedEgressNodesResult struct {
	Path    string
	Content string
	Nodes   []string
}

type grok2apiDegradedEgressNodesResponse struct {
	Data struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"data"`
	Error *grok2apiAPIError `json:"error"`
}

type grok2apiDegradedEgressNodesClearResponse struct {
	Data struct {
		Path    string `json:"path"`
		Cleared bool   `json:"cleared"`
	} `json:"data"`
	Error *grok2apiAPIError `json:"error"`
}

type grok2apiDegradedNodeProcessSummary struct {
	RequestedCount      int
	MatchedHealthyCount int
	ReplacedCount       int
	EmptySlotCount      int
}

func fetchGrok2apiDegradedEgressNodes(settings pluginSettings) (grok2apiDegradedEgressNodesResult, error) {
	if !grok2apiSyncMutex.TryLock() {
		return grok2apiDegradedEgressNodesResult{}, fmt.Errorf("grok2api 操作正在进行中")
	}
	defer grok2apiSyncMutex.Unlock()
	result, _, err := fetchGrok2apiDegradedEgressNodesUnlocked(settings)
	return result, err
}

func fetchGrok2apiDegradedEgressNodesUnlocked(settings pluginSettings) (grok2apiDegradedEgressNodesResult, string, error) {
	if err := validateGrok2apiConnectionSettings(settings); err != nil {
		return grok2apiDegradedEgressNodesResult{}, "", err
	}
	baseURL := normalizeGrok2apiBaseURL(settings.Grok2apiBaseUrl)
	accessToken, err := loginGrok2apiAdmin(baseURL, settings.Grok2apiAdminUsername, settings.Grok2apiAdminPassword)
	if err != nil {
		return grok2apiDegradedEgressNodesResult{}, "", err
	}

	result, statusCode, apiError, err := getGrok2apiDegradedEgressNodesWithToken(baseURL, accessToken)
	if err != nil {
		return grok2apiDegradedEgressNodesResult{}, "", err
	}
	if statusCode == http.StatusUnauthorized {
		accessToken, err = loginGrok2apiAdmin(baseURL, settings.Grok2apiAdminUsername, settings.Grok2apiAdminPassword)
		if err != nil {
			return grok2apiDegradedEgressNodesResult{}, "", err
		}
		result, statusCode, apiError, err = getGrok2apiDegradedEgressNodesWithToken(baseURL, accessToken)
		if err != nil {
			return grok2apiDegradedEgressNodesResult{}, "", err
		}
	}
	if statusCode != http.StatusOK {
		return grok2apiDegradedEgressNodesResult{}, "", formatGrok2apiHTTPError(statusCode, apiError)
	}
	return result, accessToken, nil
}

func getGrok2apiDegradedEgressNodesWithToken(baseURL, accessToken string) (grok2apiDegradedEgressNodesResult, int, *grok2apiAPIError, error) {
	statusCode, responseBody, err := doGrok2apiJSONRequest(http.MethodGet, baseURL+grok2apiDegradedEgressNodesPath, accessToken, nil)
	if err != nil {
		return grok2apiDegradedEgressNodesResult{}, 0, nil, fmt.Errorf("grok2api 读取降智节点: %w", err)
	}
	var response grok2apiDegradedEgressNodesResponse
	if len(responseBody) > 0 {
		if err := json.Unmarshal(responseBody, &response); err != nil {
			return grok2apiDegradedEgressNodesResult{}, statusCode, nil, fmt.Errorf("解析 grok2api 降智节点响应: %w", err)
		}
	}
	if statusCode != http.StatusOK {
		return grok2apiDegradedEgressNodesResult{}, statusCode, response.Error, nil
	}
	content := response.Data.Content
	return grok2apiDegradedEgressNodesResult{
		Path:    response.Data.Path,
		Content: content,
		Nodes:   parseGrok2apiDegradedEgressNodeLines(content),
	}, statusCode, response.Error, nil
}

func parseGrok2apiDegradedEgressNodeLines(content string) []string {
	seen := make(map[string]struct{})
	nodes := make([]string, 0)
	for _, line := range strings.Split(content, "\n") {
		node := strings.TrimSpace(line)
		if node == "" {
			continue
		}
		if _, exists := seen[node]; exists {
			continue
		}
		seen[node] = struct{}{}
		nodes = append(nodes, node)
	}
	return nodes
}

func syncGrok2apiDegradedNodesBeforeKeepalive(store *ipStore, settings pluginSettings) error {
	if !settings.Grok2apiSyncEnabled {
		_ = store.appendLog(logLevelInfo, "grok2api.degraded_sync_skipped", 0, "", "跳过保活前降智节点同步", "grok2api 同步未启用")
		return nil
	}
	grok2apiSyncMutex.Lock()
	defer grok2apiSyncMutex.Unlock()

	result, accessToken, err := fetchGrok2apiDegradedEgressNodesUnlocked(settings)
	if err != nil {
		return err
	}
	processSummary, err := store.moveGrok2apiDegradedNodesToCooldown(result.Nodes)
	if err != nil {
		return err
	}
	if len(result.Nodes) > 0 {
		if err := refreshHealthyAuthDistribution(store, settings.HealthySlotCount); err != nil {
			return fmt.Errorf("保活前同步降智节点后更新所有 auth 文件: %w", err)
		}
	}

	baseURL := normalizeGrok2apiBaseURL(settings.Grok2apiBaseUrl)
	statusCode, apiError, err := clearGrok2apiDegradedEgressNodesWithToken(baseURL, accessToken)
	if err != nil {
		return err
	}
	if statusCode == http.StatusUnauthorized {
		accessToken, err = loginGrok2apiAdmin(baseURL, settings.Grok2apiAdminUsername, settings.Grok2apiAdminPassword)
		if err != nil {
			return err
		}
		statusCode, apiError, err = clearGrok2apiDegradedEgressNodesWithToken(baseURL, accessToken)
		if err != nil {
			return err
		}
	}
	if statusCode != http.StatusOK {
		return formatGrok2apiHTTPError(statusCode, apiError)
	}
	_ = store.appendLog(
		logLevelInfo,
		"grok2api.degraded_nodes_processed",
		0,
		"",
		"保活前降智节点同步完成",
		fmt.Sprintf("远端降智节点 %d 个；匹配健康节点 %d 个；替换健康槽位 %d 个；无健康备选空槽 %d 个；已清理远端记录", processSummary.RequestedCount, processSummary.MatchedHealthyCount, processSummary.ReplacedCount, processSummary.EmptySlotCount),
	)
	return nil
}

func clearGrok2apiDegradedEgressNodesWithToken(baseURL, accessToken string) (int, *grok2apiAPIError, error) {
	statusCode, responseBody, err := doGrok2apiJSONRequest(http.MethodPost, baseURL+grok2apiDegradedEgressNodesClearPath, accessToken, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("grok2api 清理降智节点: %w", err)
	}
	var response grok2apiDegradedEgressNodesClearResponse
	if len(responseBody) > 0 {
		if err := json.Unmarshal(responseBody, &response); err != nil {
			return statusCode, nil, fmt.Errorf("解析 grok2api 清理降智节点响应: %w", err)
		}
	}
	return statusCode, response.Error, nil
}

func (store *ipStore) moveGrok2apiDegradedNodesToCooldown(proxyURLs []string) (grok2apiDegradedNodeProcessSummary, error) {
	summary := grok2apiDegradedNodeProcessSummary{RequestedCount: len(proxyURLs)}
	if len(proxyURLs) == 0 {
		return summary, nil
	}
	transaction, err := store.database.Begin()
	if err != nil {
		return summary, fmt.Errorf("开始保活前降智节点迁移事务: %w", err)
	}
	defer transaction.Rollback()

	for _, proxyURL := range proxyURLs {
		node, slot, found, err := findGrok2apiHealthySlot(transaction, proxyURL)
		if err != nil {
			return summary, err
		}
		if !found {
			continue
		}
		summary.MatchedHealthyCount++
		nodeUpdate, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, probe_started_at = 0, probe_kind = '', probe_return_status = '',
    error_reason = ?, error_detail = ?, revive_target_status = ?
WHERE id = ? AND status = ?`, statusCooldown, "grok2api_degraded", "grok2api Console Guard 返回该节点已降智", statusCooldown, node.ID, statusHealthy)
		if err != nil {
			return summary, fmt.Errorf("迁移 grok2api 降智节点 %d 到冷却中: %w", node.ID, err)
		}
		if rowsAffected, err := nodeUpdate.RowsAffected(); err != nil {
			return summary, fmt.Errorf("读取 grok2api 降智节点 %d 更新结果: %w", node.ID, err)
		} else if rowsAffected != 1 {
			return summary, fmt.Errorf("grok2api 降智节点 %d 状态更新丢失", node.ID)
		}

		slotUpdate, err := transaction.Exec(`
UPDATE ip_slots
SET `+slotNodeIDAssignment()+`, claim_node_id = 0, fallback_origin = 0, fallback_entered_round_id = 0,
    claim_token = '', claim_stage = '', claim_started_at = 0
WHERE slot_id = ? AND node_id = ?`, slotNodeIDArgs(0, slot.ID, node.ID)...)
		if err != nil {
			return summary, fmt.Errorf("清空 grok2api 降智健康槽位 %d: %w", slot.ID, err)
		}
		if rowsAffected, err := slotUpdate.RowsAffected(); err != nil {
			return summary, fmt.Errorf("读取 grok2api 降智健康槽位 %d 更新结果: %w", slot.ID, err)
		} else if rowsAffected != 1 {
			return summary, fmt.Errorf("grok2api 降智健康槽位 %d 清空丢失", slot.ID)
		}

		candidate, candidateSlot, candidateFound, err := findRealtimeCandidate(transaction)
		if err != nil {
			return summary, err
		}
		if !candidateFound {
			summary.EmptySlotCount++
			continue
		}
		candidateSlotUpdate, err := transaction.Exec(`UPDATE ip_slots SET `+slotNodeIDAssignment()+` WHERE slot_id = ? AND node_id = ?`, slotNodeIDArgs(0, candidateSlot.ID, candidate.ID)...)
		if err != nil {
			return summary, fmt.Errorf("清空 grok2api 健康备选槽位 %d: %w", candidateSlot.ID, err)
		}
		if rowsAffected, err := candidateSlotUpdate.RowsAffected(); err != nil {
			return summary, fmt.Errorf("读取 grok2api 健康备选槽位 %d 更新结果: %w", candidateSlot.ID, err)
		} else if rowsAffected != 1 {
			return summary, fmt.Errorf("grok2api 健康备选槽位 %d 清空丢失", candidateSlot.ID)
		}
		candidateUpdate, err := transaction.Exec(`
UPDATE ip_nodes
SET status = ?, error_reason = '', error_detail = '', revive_target_status = ?
WHERE id = ? AND status = ?`, statusHealthy, statusHealthy, candidate.ID, statusHealthyCandidate)
		if err != nil {
			return summary, fmt.Errorf("提升 grok2api 健康备选节点 %d: %w", candidate.ID, err)
		}
		if rowsAffected, err := candidateUpdate.RowsAffected(); err != nil {
			return summary, fmt.Errorf("读取 grok2api 健康备选节点 %d 更新结果: %w", candidate.ID, err)
		} else if rowsAffected != 1 {
			return summary, fmt.Errorf("grok2api 健康备选节点 %d 提升丢失", candidate.ID)
		}
		healthySlotUpdate, err := transaction.Exec(`UPDATE ip_slots SET `+slotNodeIDAssignment()+` WHERE slot_id = ? AND node_id = 0`, slotNodeIDArgs(candidate.ID, slot.ID)...)
		if err != nil {
			return summary, fmt.Errorf("写入 grok2api 健康替换槽位 %d: %w", slot.ID, err)
		}
		if rowsAffected, err := healthySlotUpdate.RowsAffected(); err != nil {
			return summary, fmt.Errorf("读取 grok2api 健康替换槽位 %d 更新结果: %w", slot.ID, err)
		} else if rowsAffected != 1 {
			return summary, fmt.Errorf("grok2api 健康替换槽位 %d 写入丢失", slot.ID)
		}
		summary.ReplacedCount++
	}
	if err := transaction.Commit(); err != nil {
		return summary, fmt.Errorf("提交保活前降智节点迁移事务: %w", err)
	}
	return summary, nil
}

func findGrok2apiHealthySlot(transaction *sql.Tx, proxyURL string) (proxyNode, slotRecord, bool, error) {
	var node proxyNode
	var slot slotRecord
	err := transaction.QueryRow(`
SELECT slots.slot_id, slots.slot_kind, slots.node_id, slots.claim_node_id, slots.fallback_origin, slots.fallback_entered_round_id,
       slots.claim_token, slots.claim_stage, slots.claim_started_at, slots.last_processed_round_id, slots.blocked_round_id, slots.refresh_at,
       nodes.id, nodes.node_name, nodes.proxy_url, nodes.host, nodes.input_ip, nodes.port, nodes.protocol, nodes.domain,
       nodes.batch_id, nodes.status, nodes.initial_connected, nodes.probe_kind, nodes.probe_return_status,
       nodes.keepalive_round_id, nodes.revive_round_id, nodes.revive_failure_count, nodes.latency_ms, nodes.entered_at,
       nodes.probe_started_at, nodes.probe_time, nodes.exit_ip, nodes.exit_country, nodes.error_reason, nodes.error_detail,
       nodes.revive_target_status
FROM ip_slots AS slots
JOIN ip_nodes AS nodes ON nodes.id = slots.node_id
WHERE slots.slot_kind = ? AND nodes.status = ? AND nodes.proxy_url = ?
ORDER BY slots.slot_id ASC
LIMIT 1`, statusHealthy, statusHealthy, proxyURL).Scan(
		&slot.ID, &slot.Kind, &slot.NodeID, &slot.ClaimNodeID, &slot.FallbackOrigin, &slot.FallbackEnteredRoundID,
		&slot.ClaimToken, &slot.ClaimStage, &slot.ClaimStartedAt, &slot.LastProcessedRoundID, &slot.BlockedRoundID, &slot.RefreshAt,
		&node.ID, &node.Name, &node.ProxyURL, &node.Host, &node.InputIP, &node.Port, &node.Protocol,
		&node.Domain, &node.BatchID, &node.Status, &node.InitialConnected, &node.ProbeKind,
		&node.ProbeReturnStatus, &node.KeepaliveRoundID, &node.ReviveRoundID, &node.ReviveFailureCount,
		&node.LatencyMs, &node.EnteredAt, &node.ProbeStartedAt, &node.ProbeTime, &node.ExitIP,
		&node.ExitCountry, &node.ErrorReason, &node.ErrorDetail, &node.ReviveTargetStatus,
	)
	if err == sql.ErrNoRows {
		return proxyNode{}, slotRecord{}, false, nil
	}
	if err != nil {
		return proxyNode{}, slotRecord{}, false, fmt.Errorf("查找 grok2api 降智健康槽位: %w", err)
	}
	return node, slot, true, nil
}
