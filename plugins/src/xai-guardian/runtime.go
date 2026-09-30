package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var guardianRuntime = &runtimeController{}

type runtimeController struct {
	mutex        sync.RWMutex
	store        *guardianStore
	workerCancel context.CancelFunc
	workerGroup  sync.WaitGroup
	config       pluginConfig
}

func (controller *runtimeController) configure(config pluginConfig) error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.store == nil {
		store, err := openGuardianStore(config.DatabasePath)
		if err != nil {
			return err
		}
		controller.store = store
	} else {
		configuredPath, err := normalizedDatabasePath(config.DatabasePath)
		if err != nil {
			return err
		}
		if controller.store.path != configuredPath {
			return fmt.Errorf("database path changes require plugin restart")
		}
	}
	settings, err := controller.store.settings()
	if err != nil {
		return err
	}
	if config.inspectionIntervalSet {
		settings.InspectionIntervalSeconds = config.InspectionIntervalSeconds
	}
	if err := controller.store.setSettings(settings); err != nil {
		return err
	}
	controller.config = config
	controller.stopWorkerLocked()
	if settings.InspectionIntervalSeconds > 0 {
		controller.startWorkerLocked(settings.InspectionIntervalSeconds)
	}
	return controller.store.appendLog(logLevelInfo, "plugin.configured", "xAI Guardian 已初始化", fmt.Sprintf("数据库 %s", controller.store.path))
}

func (controller *runtimeController) ensure() error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.store != nil {
		return nil
	}
	store, err := openGuardianStore(defaultDatabasePath)
	if err != nil {
		return err
	}
	controller.store = store
	settings, err := store.settings()
	if err != nil {
		_ = store.close()
		controller.store = nil
		return err
	}
	if settings.InspectionIntervalSeconds > 0 {
		controller.startWorkerLocked(settings.InspectionIntervalSeconds)
	}
	return nil
}

func (controller *runtimeController) currentStore() *guardianStore {
	controller.mutex.RLock()
	defer controller.mutex.RUnlock()
	return controller.store
}

func (controller *runtimeController) stopWorkerLocked() {
	if controller.workerCancel == nil {
		return
	}
	controller.workerCancel()
	controller.workerGroup.Wait()
	controller.workerCancel = nil
}

func (controller *runtimeController) startWorkerLocked(intervalSeconds int) {
	workerContext, cancel := context.WithCancel(context.Background())
	controller.workerCancel = cancel
	store := controller.store
	controller.workerGroup.Add(1)
	go func() {
		defer controller.workerGroup.Done()
		ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-workerContext.Done():
				return
			case <-ticker.C:
				if err := runInspection(workerContext, store); err != nil {
					_ = store.appendLog(logLevelError, "inspection.worker_failed", "自动服务端巡检失败", err.Error())
				}
			}
		}
	}()
}

func (controller *runtimeController) shutdown() {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	controller.stopWorkerLocked()
	if controller.store != nil {
		_ = controller.store.appendLog(logLevelInfo, "plugin.shutdown", "xAI Guardian 正在停止", "")
		_ = controller.store.close()
		controller.store = nil
	}
}

func (controller *runtimeController) api(method, path string, query url.Values, body []byte) (int, []byte, error) {
	if err := controller.ensure(); err != nil {
		return http.StatusInternalServerError, nil, err
	}
	controller.mutex.RLock()
	store := controller.store
	controller.mutex.RUnlock()
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		path = "/"
	}
	if method == http.MethodGet && path == "/api/summary" {
		value, err := store.summary()
		return jsonAPIResult(value, err)
	}
	if method == http.MethodGet && path == "/api/settings" {
		settings, err := store.settings()
		if err != nil {
			return http.StatusInternalServerError, nil, err
		}
		return jsonAPIResult(publicSettings(settings), nil)
	}
	if (method == http.MethodPut || method == http.MethodPost) && path == "/api/settings" {
		return controller.updateSettings(store, body)
	}
	if method == http.MethodGet && path == "/api/accounts" {
		return controller.accountsAPI(store)
	}
	if method == http.MethodPost && path == "/api/accounts/refresh" {
		return controller.accountsAPI(store)
	}
	if method == http.MethodGet && path == "/api/nodes" {
		nodes, err := store.listNodes()
		return jsonAPIResult(publicNodes(nodes), err)
	}
	if method == http.MethodPost && path == "/api/nodes" {
		return controller.addNodes(store, body)
	}
	if method == http.MethodGet && path == "/api/inspection" {
		return controller.inspectionAPI(store)
	}
	if method == http.MethodPost && path == "/api/inspection" {
		if err := runInspection(context.Background(), store); err != nil {
			return http.StatusBadGateway, nil, err
		}
		return controller.inspectionAPI(store)
	}
	if method == http.MethodGet && path == "/api/degradation" {
		states, err := store.listDegradations()
		public := make([]map[string]any, 0, len(states))
		for _, state := range states {
			public = append(public, publicDegradation(state))
		}
		return jsonAPIResult(public, err)
	}
	if method == http.MethodPost && path == "/api/degradation/clear" {
		var payload struct {
			AuthIndex string `json:"authIndex"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.AuthIndex) == "" {
			return http.StatusBadRequest, nil, fmt.Errorf("authIndex is required")
		}
		if err := store.clearDegradation(strings.TrimSpace(payload.AuthIndex)); err != nil {
			return http.StatusInternalServerError, nil, err
		}
		return jsonAPIResult(map[string]any{"cleared": true}, nil)
	}
	if method == http.MethodGet && path == "/api/logs" {
		limit, _ := strconv.Atoi(query.Get("limit"))
		logs, err := store.listLogs(limit)
		return jsonAPIResult(logs, err)
	}
	if len(strings.Split(strings.Trim(path, "/"), "/")) == 4 && strings.HasPrefix(path, "/api/nodes/") && strings.HasSuffix(path, "/delete") && method == http.MethodPost {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || id <= 0 {
			return http.StatusBadRequest, nil, fmt.Errorf("invalid node id")
		}
		if err := store.deleteNode(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return http.StatusNotFound, nil, fmt.Errorf("node not found")
			}
			return http.StatusInternalServerError, nil, err
		}
		return jsonAPIResult(map[string]any{"deleted": true}, nil)
	}
	return http.StatusNotFound, nil, fmt.Errorf("API path not found")
}

func (controller *runtimeController) updateSettings(store *guardianStore, body []byte) (int, []byte, error) {
	var payload struct {
		InspectionIntervalSeconds int `json:"inspectionIntervalSeconds"`
		FirstPayloadTimeout       int `json:"firstPayloadTimeout"`
		ProgressTimeout           int `json:"progressTimeout"`
		MinSummaryChars           int `json:"minSummaryChars"`
		MinEncryptedBytes         int `json:"minEncryptedBytes"`
		EncryptedBytesPerToken    int `json:"encryptedBytesPerToken"`
		MinOutputTokens           int `json:"minOutputTokens"`
		HardTPS                   int `json:"hardTPS"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return http.StatusBadRequest, nil, err
	}
	settings := pluginSettings{InspectionIntervalSeconds: payload.InspectionIntervalSeconds, FirstPayloadTimeout: payload.FirstPayloadTimeout, ProgressTimeout: payload.ProgressTimeout, MinSummaryChars: payload.MinSummaryChars, MinEncryptedBytes: payload.MinEncryptedBytes, EncryptedBytesPerToken: payload.EncryptedBytesPerToken, MinOutputTokens: payload.MinOutputTokens, HardTPS: payload.HardTPS}
	if err := store.setSettings(settings); err != nil {
		return http.StatusBadRequest, nil, err
	}
	controller.mutex.Lock()
	controller.stopWorkerLocked()
	if settings.InspectionIntervalSeconds > 0 {
		controller.startWorkerLocked(settings.InspectionIntervalSeconds)
	}
	controller.mutex.Unlock()
	_ = store.appendLog(logLevelInfo, "settings.updated", "插件配置已保存", "")
	return jsonAPIResult(publicSettings(settings), nil)
}

func (controller *runtimeController) accountsAPI(store *guardianStore) (int, []byte, error) {
	entries, err := listXAIAuthEntries()
	if err != nil {
		return http.StatusBadGateway, nil, err
	}
	if err := syncAuthBindings(store, entries); err != nil {
		return http.StatusInternalServerError, nil, err
	}
	bindings, err := store.listAuthBindings()
	return jsonAPIResult(publicAccounts(bindings), err)
}

func (controller *runtimeController) inspectionAPI(store *guardianStore) (int, []byte, error) {
	run, results, err := store.latestInspection()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	if run.ID == 0 {
		return jsonAPIResult(map[string]any{"run": nil, "results": results}, nil)
	}
	return jsonAPIResult(map[string]any{"run": run, "results": results}, nil)
}

func (controller *runtimeController) addNodes(store *guardianStore, body []byte) (int, []byte, error) {
	var payload struct {
		Text string `json:"text"`
		IPs  string `json:"ips"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return http.StatusBadRequest, nil, err
	}
	text := strings.TrimSpace(payload.Text)
	if text == "" {
		text = strings.TrimSpace(payload.IPs)
	}
	nodes, errors := parseProxyLines(text)
	if len(nodes) == 0 {
		return http.StatusBadRequest, nil, fmt.Errorf("no valid proxy nodes")
	}
	added, duplicates, err := store.insertNodes(nodes)
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	_ = store.appendLog(logLevelInfo, "nodes.added", "服务端巡检节点已录入", fmt.Sprintf("新增 %d，重复 %d，格式错误 %d", added, duplicates, len(errors)))
	return jsonAPIResult(map[string]any{"added": added, "duplicates": duplicates, "errors": errors}, nil)
}

func jsonAPIResult(value any, err error) (int, []byte, error) {
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	raw, err := json.Marshal(map[string]any{"ok": true, "data": value})
	return http.StatusOK, raw, err
}

func publicSettings(settings pluginSettings) map[string]any {
	return map[string]any{"inspectionIntervalSeconds": settings.InspectionIntervalSeconds, "firstPayloadTimeout": settings.FirstPayloadTimeout, "progressTimeout": settings.ProgressTimeout, "minSummaryChars": settings.MinSummaryChars, "minEncryptedBytes": settings.MinEncryptedBytes, "encryptedBytesPerToken": settings.EncryptedBytesPerToken, "minOutputTokens": settings.MinOutputTokens, "hardTPS": settings.HardTPS}
}

func publicNodes(nodes []proxyNode) []map[string]any {
	items := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		items = append(items, map[string]any{"id": node.ID, "address": redactProxyURL(node.Address), "protocol": node.Protocol, "host": node.Host, "port": node.Port, "status": node.Status, "latencyMs": node.LatencyMS, "exitIp": node.ExitIP, "country": node.Country, "lastChecked": node.LastChecked, "lastError": sanitizeLogText(node.LastError), "createdAt": node.CreatedAt})
	}
	return items
}

func publicAccounts(bindings []authBinding) []map[string]any {
	items := make([]map[string]any, 0, len(bindings))
	for _, binding := range bindings {
		items = append(items, map[string]any{"authIndex": binding.AuthIndex, "authName": binding.AuthName, "nodeId": binding.NodeID, "proxyUrl": redactProxyURL(binding.ProxyURL), "status": binding.Status, "priority": binding.Priority, "success": binding.Success, "failed": binding.Failed, "updatedAt": binding.UpdatedAt, "lastChecked": binding.LastChecked})
	}
	return items
}

func publicDegradation(state degradationState) map[string]any {
	return map[string]any{"authIndex": state.AuthIndex, "authName": state.AuthName, "count": state.Count, "lastReason": state.LastReason, "lastRequestId": state.LastRequest, "lastSeen": state.LastSeen, "coolingUntil": state.CoolingUntil}
}
