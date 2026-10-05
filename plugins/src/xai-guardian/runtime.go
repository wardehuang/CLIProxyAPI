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
	mutex            sync.RWMutex
	store            *guardianStore
	inspectionCancel context.CancelFunc
	inspectionGroup  sync.WaitGroup
	probeCancel      context.CancelFunc
	probeGroup       sync.WaitGroup
	probeTrigger     chan struct{}
	keepaliveCancel  context.CancelFunc
	keepaliveGroup   sync.WaitGroup
	reviveCancel     context.CancelFunc
	reviveGroup      sync.WaitGroup
	keepaliveState   *keepaliveScheduleState
	config           pluginConfig
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
	controller.stopWorkersLocked()
	if settings.InspectionIntervalSeconds > 0 {
		controller.startInspectionWorkerLocked(settings.InspectionIntervalSeconds)
	}
	controller.startInitialProbeWorkerLocked(settings)
	if settings.KeepaliveIntervalSeconds > 0 {
		controller.startKeepaliveWorkerLocked(settings)
	}
	if settings.ReviveIntervalSeconds > 0 {
		controller.startReviveWorkerLocked(settings)
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
		controller.startInspectionWorkerLocked(settings.InspectionIntervalSeconds)
	}
	controller.startInitialProbeWorkerLocked(settings)
	if settings.KeepaliveIntervalSeconds > 0 {
		controller.startKeepaliveWorkerLocked(settings)
	}
	if settings.ReviveIntervalSeconds > 0 {
		controller.startReviveWorkerLocked(settings)
	}
	return nil
}

func (controller *runtimeController) currentStore() *guardianStore {
	controller.mutex.RLock()
	defer controller.mutex.RUnlock()
	return controller.store
}

func (controller *runtimeController) stopWorkersLocked() {
	if controller.inspectionCancel != nil {
		controller.inspectionCancel()
		controller.inspectionGroup.Wait()
		controller.inspectionCancel = nil
	}
	if controller.probeCancel != nil {
		controller.probeCancel()
		controller.probeGroup.Wait()
		controller.probeCancel = nil
		controller.probeTrigger = nil
	}
	if controller.keepaliveCancel != nil {
		controller.keepaliveCancel()
		controller.keepaliveGroup.Wait()
		controller.keepaliveCancel = nil
	}
	if controller.reviveCancel != nil {
		controller.reviveCancel()
		controller.reviveGroup.Wait()
		controller.reviveCancel = nil
	}
	controller.keepaliveState = nil
}

func (controller *runtimeController) startInspectionWorkerLocked(intervalSeconds int) {
	workerContext, cancel := context.WithCancel(context.Background())
	controller.inspectionCancel = cancel
	store := controller.store
	controller.inspectionGroup.Add(1)
	go func() {
		defer controller.inspectionGroup.Done()
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

func (controller *runtimeController) startKeepaliveWorkerLocked(settings pluginSettings) {
	workerContext, cancel := context.WithCancel(context.Background())
	controller.keepaliveCancel = cancel
	controller.keepaliveState = newKeepaliveScheduleState()
	store := controller.store
	state := controller.keepaliveState
	controller.keepaliveGroup.Add(1)
	go func() {
		defer controller.keepaliveGroup.Done()
		runKeepaliveScheduler(workerContext, store, state, settings)
	}()
}

func (controller *runtimeController) startInitialProbeWorkerLocked(settings pluginSettings) {
	workerContext, cancel := context.WithCancel(context.Background())
	controller.probeCancel = cancel
	controller.probeTrigger = make(chan struct{}, 1)
	store := controller.store
	trigger := controller.probeTrigger
	controller.probeGroup.Add(1)
	go func() {
		defer controller.probeGroup.Done()
		runInitialProbeScheduler(workerContext, store, settings, trigger)
	}()
}

func (controller *runtimeController) startReviveWorkerLocked(settings pluginSettings) {
	workerContext, cancel := context.WithCancel(context.Background())
	controller.reviveCancel = cancel
	store := controller.store
	controller.reviveGroup.Add(1)
	go func() {
		defer controller.reviveGroup.Done()
		runReviveScheduler(workerContext, store, settings)
	}()
}

func (controller *runtimeController) shutdown() {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	controller.stopWorkersLocked()
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
	if method == http.MethodGet && path == "/api/batch-nodes" {
		nodes, err := store.listIPNodes()
		return jsonAPIResult(publicNodes(nodes), err)
	}
	if method == http.MethodGet && path == "/api/batches" {
		batches, err := store.listIPBatches()
		if err != nil {
			return http.StatusInternalServerError, nil, err
		}
		settings, err := store.settings()
		if err != nil {
			return http.StatusInternalServerError, nil, err
		}
		return jsonAPIResult(map[string]any{"items": publicIPBatches(batches, settings.IPBatchRetentionDays), "total": len(batches), "max": maxIPBatches, "retentionDays": settings.IPBatchRetentionDays}, nil)
	}
	if method == http.MethodPost && path == "/api/batches" {
		return controller.addIPBatch(store, body)
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if method == http.MethodGet && len(parts) == 4 && parts[0] == "api" && parts[1] == "batches" && parts[3] == "nodes" {
		batchID := strings.TrimSpace(parts[2])
		if batchID == "" {
			return http.StatusBadRequest, nil, fmt.Errorf("batch ID is required")
		}
		nodes, err := store.listIPBatchNodes(batchID)
		return jsonAPIResult(map[string]any{"batchId": batchID, "items": publicNodes(nodes), "total": len(nodes)}, err)
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
	if method == http.MethodGet && path == "/api/keepalive" {
		return controller.keepaliveAPI(store)
	}
	if method == http.MethodPost && path == "/api/keepalive/run" {
		return controller.runKeepaliveNow()
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
		WorkerCount                                  int     `json:"workerCount"`
		RefreshIntervalSeconds                       int     `json:"refreshIntervalSeconds"`
		InspectionIntervalSeconds                    int     `json:"inspectionIntervalSeconds"`
		KeepaliveWorkerCount                         int     `json:"keepaliveWorkerCount"`
		KeepaliveIntervalSeconds                     int     `json:"keepaliveIntervalSeconds"`
		ReviveIntervalSeconds                        int     `json:"reviveIntervalSeconds"`
		ProbeRetryCount                              int     `json:"probeRetryCount"`
		HealthySlotCount                             int     `json:"healthySlotCount"`
		HealthyCandidateSlotCount                    int     `json:"healthyCandidateSlotCount"`
		HealthySlotMaxAgeMinutes                     int     `json:"healthySlotMaxAgeMinutes"`
		QualityWorkerCount                           int     `json:"qualityWorkerCount"`
		QualityProbeTimeoutSeconds                   int     `json:"qualityProbeTimeoutSeconds"`
		QualityProbeModel                            string  `json:"qualityProbeModel"`
		QualitySoftTPS                               float64 `json:"qualitySoftTPS"`
		QualityHardTPS                               float64 `json:"qualityHardTPS"`
		QualityLLMProbeEnabled                       bool    `json:"qualityLLMProbeEnabled"`
		RealtimeGuardTTFBSeconds                     float64 `json:"realtimeGuardTTFBSeconds"`
		RealtimeGuardGenerationSeconds               float64 `json:"realtimeGuardGenerationSeconds"`
		RealtimeGuardTokenThreshold                  int     `json:"realtimeGuardTokenThreshold"`
		RealtimeGuardTimeoutSeconds                  int     `json:"realtimeGuardTimeoutSeconds"`
		RealtimeGuardIdleTimeoutSeconds              int     `json:"realtimeGuardIdleTimeoutSeconds"`
		RealtimeGuardMinSummaryChars                 int     `json:"realtimeGuardMinSummaryChars"`
		RealtimeGuardMinEncryptedBytes               int     `json:"realtimeGuardMinEncryptedBytes"`
		RealtimeGuardEncryptedBytesPerReasoningToken int     `json:"realtimeGuardEncryptedBytesPerReasoningToken"`
		RealtimeGuardMinOutputTokens                 int     `json:"realtimeGuardMinOutputTokens"`
		RealtimeGuardBurstMinReasoningTokens         int     `json:"realtimeGuardBurstMinReasoningTokens"`
		RealtimeGuardBurstMaxVisibleTokens           int     `json:"realtimeGuardBurstMaxVisibleTokens"`
		RealtimeGuardBurstMaxWindowMS                int     `json:"realtimeGuardBurstMaxWindowMs"`
		IPBatchRetentionDays                         int     `json:"ipBatchRetentionDays"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return http.StatusBadRequest, nil, err
	}
	settings := pluginSettings{
		WorkerCount:                                  payload.WorkerCount,
		RefreshIntervalSeconds:                       payload.RefreshIntervalSeconds,
		InspectionIntervalSeconds:                    payload.InspectionIntervalSeconds,
		KeepaliveWorkerCount:                         payload.KeepaliveWorkerCount,
		KeepaliveIntervalSeconds:                     payload.KeepaliveIntervalSeconds,
		ReviveIntervalSeconds:                        payload.ReviveIntervalSeconds,
		ProbeRetryCount:                              payload.ProbeRetryCount,
		HealthySlotCount:                             payload.HealthySlotCount,
		HealthyCandidateSlotCount:                    payload.HealthyCandidateSlotCount,
		HealthySlotMaxAgeMinutes:                     payload.HealthySlotMaxAgeMinutes,
		QualityWorkerCount:                           payload.QualityWorkerCount,
		QualityProbeTimeoutSeconds:                   payload.QualityProbeTimeoutSeconds,
		QualityProbeModel:                            strings.TrimSpace(payload.QualityProbeModel),
		QualitySoftTPS:                               payload.QualitySoftTPS,
		QualityHardTPS:                               payload.QualityHardTPS,
		QualityLLMProbeEnabled:                       payload.QualityLLMProbeEnabled,
		RealtimeGuardTTFBSeconds:                     payload.RealtimeGuardTTFBSeconds,
		RealtimeGuardGenerationSeconds:               payload.RealtimeGuardGenerationSeconds,
		RealtimeGuardTokenThreshold:                  payload.RealtimeGuardTokenThreshold,
		RealtimeGuardTimeoutSeconds:                  payload.RealtimeGuardTimeoutSeconds,
		RealtimeGuardIdleTimeoutSeconds:              payload.RealtimeGuardIdleTimeoutSeconds,
		RealtimeGuardMinSummaryChars:                 payload.RealtimeGuardMinSummaryChars,
		RealtimeGuardMinEncryptedBytes:               payload.RealtimeGuardMinEncryptedBytes,
		RealtimeGuardEncryptedBytesPerReasoningToken: payload.RealtimeGuardEncryptedBytesPerReasoningToken,
		RealtimeGuardMinOutputTokens:                 payload.RealtimeGuardMinOutputTokens,
		RealtimeGuardBurstMinReasoningTokens:         payload.RealtimeGuardBurstMinReasoningTokens,
		RealtimeGuardBurstMaxVisibleTokens:           payload.RealtimeGuardBurstMaxVisibleTokens,
		RealtimeGuardBurstMaxWindowMS:                payload.RealtimeGuardBurstMaxWindowMS,
		IPBatchRetentionDays:                         payload.IPBatchRetentionDays,
	}
	if err := store.setSettings(settings); err != nil {
		return http.StatusBadRequest, nil, err
	}
	controller.mutex.Lock()
	controller.stopWorkersLocked()
	if settings.InspectionIntervalSeconds > 0 {
		controller.startInspectionWorkerLocked(settings.InspectionIntervalSeconds)
	}
	controller.startInitialProbeWorkerLocked(settings)
	if settings.KeepaliveIntervalSeconds > 0 {
		controller.startKeepaliveWorkerLocked(settings)
	}
	if settings.ReviveIntervalSeconds > 0 {
		controller.startReviveWorkerLocked(settings)
	}
	controller.mutex.Unlock()
	_ = store.appendLog(logLevelInfo, "settings.updated", "插件配置已保存", "")
	return jsonAPIResult(publicSettings(settings), nil)
}

func (controller *runtimeController) keepaliveAPI(store *guardianStore) (int, []byte, error) {
	settings, err := store.settings()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	round, found, err := store.latestKeepaliveRound()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	controller.mutex.RLock()
	state := controller.keepaliveState
	controller.mutex.RUnlock()
	return jsonAPIResult(map[string]any{
		"settings": publicSettings(settings),
		"schedule": state.snapshot(settings.KeepaliveIntervalSeconds),
		"round":    mapKeepaliveRound(round, found),
	}, nil)
}

func (controller *runtimeController) runKeepaliveNow() (int, []byte, error) {
	controller.mutex.RLock()
	state := controller.keepaliveState
	controller.mutex.RUnlock()
	accepted, running := state.requestNow()
	if running {
		_, body, err := jsonAPIResult(map[string]any{"accepted": false, "running": true}, nil)
		return http.StatusConflict, body, err
	}
	return jsonAPIResult(map[string]any{"accepted": accepted, "running": false}, nil)
}

func (controller *runtimeController) queueInitialProbeAfterBatch() {
	controller.mutex.RLock()
	trigger := controller.probeTrigger
	controller.mutex.RUnlock()
	select {
	case trigger <- struct{}{}:
	default:
	}
}

func mapKeepaliveRound(round keepaliveRound, found bool) any {
	if !found {
		return nil
	}
	return round
}

func (controller *runtimeController) accountsAPI(store *guardianStore) (int, []byte, error) {
	run, found, err := store.latestCompletedInspection()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	if !found {
		return jsonAPIResult([]map[string]any{}, nil)
	}
	bindings, err := store.listAuthBindings(run.ID)
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

func (controller *runtimeController) addIPBatch(store *guardianStore, body []byte) (int, []byte, error) {
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
	if text == "" {
		return http.StatusBadRequest, nil, fmt.Errorf("at least one IP is required")
	}
	nodes, inputErrors := parseProxyLines(text)
	if len(nodes) == 0 {
		return http.StatusBadRequest, nil, fmt.Errorf("no valid proxy nodes")
	}
	batchID, added, duplicates, err := store.insertIPBatch(nodes, len(inputErrors))
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	_ = store.appendLog(logLevelInfo, "ip_batch.created", "降智守护 IP 批次已创建", fmt.Sprintf("批次 %s，新增 %d，重复 %d，格式错误 %d", batchID, added, duplicates, len(inputErrors)))
	if added > 0 {
		controller.queueInitialProbeAfterBatch()
	}
	return jsonAPIResult(map[string]any{"batchId": batchID, "added": added, "duplicates": duplicates, "errors": inputErrors}, nil)
}

func jsonAPIResult(value any, err error) (int, []byte, error) {
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	raw, err := json.Marshal(map[string]any{"ok": true, "data": value})
	return http.StatusOK, raw, err
}

func publicSettings(settings pluginSettings) map[string]any {
	return map[string]any{
		"workerCount": settings.WorkerCount, "refreshIntervalSeconds": settings.RefreshIntervalSeconds,
		"inspectionIntervalSeconds": settings.InspectionIntervalSeconds,
		"keepaliveWorkerCount":      settings.KeepaliveWorkerCount, "keepaliveIntervalSeconds": settings.KeepaliveIntervalSeconds,
		"reviveIntervalSeconds": settings.ReviveIntervalSeconds, "probeRetryCount": settings.ProbeRetryCount,
		"healthySlotCount": settings.HealthySlotCount, "healthyCandidateSlotCount": settings.HealthyCandidateSlotCount, "healthySlotMaxAgeMinutes": settings.HealthySlotMaxAgeMinutes,
		"qualityWorkerCount": settings.QualityWorkerCount, "qualityProbeTimeoutSeconds": settings.QualityProbeTimeoutSeconds, "qualityProbeModel": settings.QualityProbeModel,
		"qualitySoftTPS": settings.QualitySoftTPS, "qualityHardTPS": settings.QualityHardTPS, "qualityLLMProbeEnabled": settings.QualityLLMProbeEnabled,
		"realtimeGuardTTFBSeconds": settings.RealtimeGuardTTFBSeconds, "realtimeGuardGenerationSeconds": settings.RealtimeGuardGenerationSeconds,
		"realtimeGuardTokenThreshold": settings.RealtimeGuardTokenThreshold, "realtimeGuardTimeoutSeconds": settings.RealtimeGuardTimeoutSeconds,
		"realtimeGuardIdleTimeoutSeconds": settings.RealtimeGuardIdleTimeoutSeconds, "realtimeGuardMinSummaryChars": settings.RealtimeGuardMinSummaryChars,
		"realtimeGuardMinEncryptedBytes": settings.RealtimeGuardMinEncryptedBytes, "realtimeGuardEncryptedBytesPerReasoningToken": settings.RealtimeGuardEncryptedBytesPerReasoningToken,
		"realtimeGuardMinOutputTokens": settings.RealtimeGuardMinOutputTokens, "realtimeGuardBurstMinReasoningTokens": settings.RealtimeGuardBurstMinReasoningTokens,
		"realtimeGuardBurstMaxVisibleTokens": settings.RealtimeGuardBurstMaxVisibleTokens, "realtimeGuardBurstMaxWindowMs": settings.RealtimeGuardBurstMaxWindowMS,
		"ipBatchRetentionDays": settings.IPBatchRetentionDays,
	}
}

func publicNodes(nodes []proxyNode) []map[string]any {
	items := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		items = append(items, map[string]any{"id": node.ID, "address": redactProxyURL(node.Address), "protocol": node.Protocol, "host": node.Host, "port": node.Port, "status": node.Status, "latencyMs": node.LatencyMS, "exitIp": node.ExitIP, "country": node.Country, "lastChecked": node.LastChecked, "lastError": sanitizeLogText(node.LastError), "createdAt": node.CreatedAt, "slotId": node.SlotID, "slotKind": node.SlotKind})
	}
	return items
}

func publicIPBatches(batches []ipBatch, retentionDays int) []map[string]any {
	items := make([]map[string]any, 0, len(batches))
	for _, batch := range batches {
		items = append(items, map[string]any{
			"batchId":                batch.ID,
			"sequenceNumber":         batch.SequenceNumber,
			"createdAt":              batch.CreatedAt,
			"expiresAt":              time.UnixMilli(batch.CreatedAt).AddDate(0, 0, retentionDays).UnixMilli(),
			"totalCount":             batch.TotalCount,
			"duplicateCount":         batch.DuplicateCount,
			"inputErrorCount":        batch.InputErrorCount,
			"completedCount":         batch.CompletedCount,
			"pendingCount":           batch.TotalCount - batch.CompletedCount,
			"initialConnectedCount":  batch.InitialConnectedCount,
			"realtimeConnectedCount": batch.RealtimeConnectedCount,
		})
	}
	return items
}

func publicAccounts(bindings []authBinding) []map[string]any {
	items := make([]map[string]any, 0, len(bindings))
	for _, binding := range bindings {
		items = append(items, map[string]any{
			"authIndex":      binding.AuthIndex,
			"authName":       binding.AuthName,
			"exitIp":         binding.ExitIP,
			"status":         binding.Status,
			"priority":       binding.Priority,
			"accountType":    binding.AccountType,
			"scheduleGroup":  binding.ScheduleGroup,
			"lastInspection": binding.LastInspection,
		})
	}
	return items
}

func publicDegradation(state degradationState) map[string]any {
	return map[string]any{"authIndex": state.AuthIndex, "authName": state.AuthName, "count": state.Count, "lastReason": state.LastReason, "lastRequestId": state.LastRequest, "lastSeen": state.LastSeen, "coolingUntil": state.CoolingUntil}
}
