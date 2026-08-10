package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginName          = "cpa-xai-ip-switcher"
	pluginVersion       = "0.1.0"
	resourcePath        = "/status"
	managementAPIPath   = "/v0/management/cpa-xai-ip-switcher/api"
	resourceContentType = "text/html; charset=utf-8"
)

//go:embed page.html
var pageTemplate string

//go:embed tokens.css
var tokenCSS string

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type pluginConfig struct {
	DatabasePath string `yaml:"database_path" json:"database_path"`
	WorkerCount  int    `yaml:"worker_count" json:"worker_count"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ManagementAPI bool `json:"management_api"`
}

type managementRegistration struct {
	Routes    []managementRoute    `json:"Routes,omitempty"`
	Resources []managementResource `json:"Resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
	Body    []byte      `json:"Body"`
}

type uiProxyRequest struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	pluginRuntime.shutdown()
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host callback payload %s: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var requestPointer *C.uint8_t
	if len(rawPayload) > 0 {
		payloadPointer := C.CBytes(rawPayload)
		if payloadPointer == nil {
			return nil, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(payloadPointer)
		requestPointer = (*C.uint8_t)(payloadPointer)
	}
	var response C.cliproxy_buffer
	callCode := C.call_host_api(cMethod, requestPointer, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}
	var callbackEnvelope envelope
	if err := json.Unmarshal(rawResponse, &callbackEnvelope); err != nil {
		return nil, fmt.Errorf("decode host callback envelope %s: %w", method, err)
	}
	if !callbackEnvelope.OK {
		if callbackEnvelope.Error != nil {
			return nil, fmt.Errorf("%s: %s", callbackEnvelope.Error.Code, callbackEnvelope.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), callbackEnvelope.Result...), nil
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var lifecycle lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &lifecycle); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		config, err := parsePluginConfig(lifecycle.ConfigYAML)
		if err != nil {
			return nil, err
		}
		if err := pluginRuntime.configure(config); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Routes: []managementRoute{{
				Method:      http.MethodPost,
				Path:        "/cpa-xai-ip-switcher/api",
				Description: "xAi出口守护管理接口",
			}},
			Resources: []managementResource{{
				Path:        resourcePath,
				Menu:        "xAi出口守护",
				Description: "xAi代理出口IP列表、初次连通性探测与定时保活复查",
			}},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func parsePluginConfig(raw []byte) (pluginConfig, error) {
	config := pluginConfig{
		DatabasePath: defaultDatabasePath,
	}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &config); err != nil {
			return pluginConfig{}, fmt.Errorf("decode plugin config: %w", err)
		}
	}
	if strings.TrimSpace(config.DatabasePath) == "" {
		config.DatabasePath = defaultDatabasePath
	}
	if config.WorkerCount < 0 || config.WorkerCount > maxProbeWorkers {
		return pluginConfig{}, fmt.Errorf("worker_count must be between 0 and %d", maxProbeWorkers)
	}
	return config, nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "wardehuang",
			GitHubRepository: "https://github.com/wardehuang/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "database_path",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "插件 SQLite 数据库路径，默认 /opt/cli-proxy-api/plugin-data/cpa-xai-ip-switcher/ip-switcher.sqlite3。",
				},
				{
					Name:        "worker_count",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "启动时的探测线程数；0 表示使用 SQLite 中保存的界面设置。",
				},
			},
		},
		Capabilities: registrationCapabilities{ManagementAPI: true},
	}
}

func handleManagement(request []byte) ([]byte, error) {
	var management managementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &management); err != nil {
			return nil, err
		}
	}

	path := strings.TrimSpace(management.Path)
	if path == "" {
		path = resourcePath
	}
	resourceBasePath := "/v0/resource/plugins/" + pluginName
	switch {
	case path == resourcePath,
		path == "/",
		path == resourceBasePath,
		path == resourceBasePath+"/",
		path == resourceBasePath+resourcePath,
		strings.HasSuffix(path, resourcePath):
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"cache-control": []string{"no-store"},
				"content-type":  []string{resourceContentType},
			},
			Body: []byte(strings.Replace(pageTemplate, "/*__HALLMARK_TOKENS__*/", tokenCSS, 1)),
		})
	case path == managementAPIPath:
		return handleUIProxy(management)
	default:
		return okEnvelope(managementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"content-type": []string{"text/plain; charset=utf-8"}},
			Body:       []byte("not found"),
		})
	}
}

func handleUIProxy(request managementRequest) ([]byte, error) {
	if !strings.EqualFold(strings.TrimSpace(request.Method), http.MethodPost) || request.Headers.Get("X-CPA-XAI-IP-Switcher-UI") != "1" {
		return managementJSON(http.StatusForbidden, errorMessage("forbidden", "forbidden"))
	}

	var proxyRequest uiProxyRequest
	if len(request.Body) == 0 {
		return managementJSON(http.StatusBadRequest, errorMessage("invalidRequest", "invalid request"))
	}
	if err := json.Unmarshal(request.Body, &proxyRequest); err != nil {
		return managementJSON(http.StatusBadRequest, errorMessage("invalidRequest", "invalid request"))
	}
	parsed, err := url.ParseRequestURI(strings.TrimSpace(proxyRequest.Path))
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/") {
		return managementJSON(http.StatusBadRequest, errorMessage("invalidPath", "invalid path"))
	}
	method := strings.ToUpper(strings.TrimSpace(proxyRequest.Method))
	if method == "" {
		method = http.MethodGet
	}
	return dispatchAPI(method, parsed.Path, parsed.Query(), proxyRequest.Body)
}

func dispatchAPI(method, path string, query url.Values, body json.RawMessage) ([]byte, error) {
	if err := pluginRuntime.ensure(); err != nil {
		return nil, err
	}
	normalizedPath := strings.TrimSuffix(path, "/")
	if normalizedPath == "/settings" && (method == http.MethodPut || method == http.MethodPost) {
		return updatePluginSettings(body)
	}
	return pluginRuntime.withStore(func(store *ipStore) ([]byte, error) {
		return dispatchAPIWithStore(store, method, path, query, body)
	})
}

func updatePluginSettings(body json.RawMessage) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return managementJSON(http.StatusBadRequest, errorMessage("invalidBody", "invalid body"))
	}
	settings, err := settingsFromPayload(payload)
	if err != nil {
		return managementJSON(http.StatusBadRequest, errorMessage("invalidSettings", err.Error()))
	}
	pluginRuntime.mutex.RLock()
	store := pluginRuntime.store
	pluginRuntime.mutex.RUnlock()
	if store == nil {
		return managementJSON(http.StatusInternalServerError, errorMessage("settingsFailed", "plugin store is not initialized"))
	}
	settingsSaved, err := pluginRuntime.updateSettings(store, settings)
	if err != nil {
		_ = store.appendLog(logLevelError, "settings.update_failed", 0, "", "更新插件配置失败", err.Error())
		return managementJSON(http.StatusInternalServerError, errorMessage("settingsFailed", err.Error()))
	}
	_ = store.appendLog(
		logLevelInfo,
		"settings.updated",
		0,
		"",
		"插件配置已保存，将在下次启动时生效",
		fmt.Sprintf("探测线程数 %d，页面刷新 %d 秒，保活线程数 %d，保活间隔 %d 秒，复活间隔 %d 秒，探测重试次数 %d，配置变化 %t；当前进程未重启", settings.WorkerCount, settings.RefreshIntervalSeconds, settings.KeepaliveWorkerCount, settings.KeepaliveIntervalSeconds, settings.ReviveIntervalSeconds, settings.ProbeRetryCount, settingsSaved),
	)
	return managementJSON(http.StatusOK, map[string]any{"data": publicSettings(settings)})
}

func settingsFromPayload(payload map[string]any) (pluginSettings, error) {
	workerCount, workerOK := integerValue(firstValue(payload, "workerCount", "worker_count"))
	refreshIntervalSeconds, refreshOK := integerValue(firstValue(payload, "refreshIntervalSeconds", "refresh_interval_seconds"))
	keepaliveWorkerCount, keepaliveWorkersOK := integerValue(firstValue(payload, "keepaliveWorkerCount", "keepalive_worker_count"))
	keepaliveIntervalSeconds, keepaliveIntervalOK := integerValue(firstValue(payload, "keepaliveIntervalSeconds", "keepalive_interval_seconds"))
	reviveIntervalSeconds, reviveIntervalOK := integerValue(firstValue(payload, "reviveIntervalSeconds", "revive_interval_seconds"))
	probeRetryCount, retryOK := integerValue(firstValue(payload, "probeRetryCount", "probe_retry_count"))
	healthySlotCount, healthySlotOK := integerValue(firstValue(payload, "healthySlotCount", "healthy_slot_count"))
	healthyCandidateSlotCount, healthyCandidateSlotOK := integerValue(firstValue(payload, "healthyCandidateSlotCount", "healthy_candidate_slot_count"))
	qualityWorkerCount, qualityWorkerOK := integerValue(firstValue(payload, "qualityWorkerCount", "quality_worker_count"))
	qualityProbeTimeoutSeconds, qualityTimeoutOK := integerValue(firstValue(payload, "qualityProbeTimeoutSeconds", "quality_probe_timeout_seconds"))
	qualitySoftTPS, softTPSOK := floatValue(firstValue(payload, "qualitySoftTPS", "quality_soft_tps"))
	qualityHardTPS, hardTPSOK := floatValue(firstValue(payload, "qualityHardTPS", "quality_hard_tps"))
	if !workerOK || !refreshOK || !keepaliveWorkersOK || !keepaliveIntervalOK || !reviveIntervalOK || !retryOK ||
		!healthySlotOK || !healthyCandidateSlotOK || !qualityWorkerOK || !qualityTimeoutOK || !softTPSOK || !hardTPSOK {
		return pluginSettings{}, fmt.Errorf("必须同时提供基础调度、健康槽位和智商探测配置")
	}
	settings := pluginSettings{
		WorkerCount:                workerCount,
		RefreshIntervalSeconds:     refreshIntervalSeconds,
		KeepaliveWorkerCount:       keepaliveWorkerCount,
		KeepaliveIntervalSeconds:   keepaliveIntervalSeconds,
		ReviveIntervalSeconds:      reviveIntervalSeconds,
		ProbeRetryCount:            probeRetryCount,
		HealthySlotCount:           healthySlotCount,
		HealthyCandidateSlotCount:  healthyCandidateSlotCount,
		QualityWorkerCount:         qualityWorkerCount,
		QualityProbeTimeoutSeconds: qualityProbeTimeoutSeconds,
		QualitySoftTPS:             qualitySoftTPS,
		QualityHardTPS:             qualityHardTPS,
	}
	return settings, validatePluginSettings(settings)
}

func firstValue(payload map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, exists := payload[key]; exists {
			return value
		}
	}
	return nil
}

func floatValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, err := strconv.ParseFloat(typed.String(), 64)
		return parsed, err == nil
	case int:
		return float64(typed), true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func publicSettings(settings pluginSettings) map[string]any {
	return map[string]any{
		"workerCount":                settings.WorkerCount,
		"refreshIntervalSeconds":     settings.RefreshIntervalSeconds,
		"keepaliveWorkerCount":       settings.KeepaliveWorkerCount,
		"keepaliveIntervalSeconds":   settings.KeepaliveIntervalSeconds,
		"reviveIntervalSeconds":      settings.ReviveIntervalSeconds,
		"probeRetryCount":            settings.ProbeRetryCount,
		"healthySlotCount":           settings.HealthySlotCount,
		"healthyCandidateSlotCount":  settings.HealthyCandidateSlotCount,
		"qualityWorkerCount":         settings.QualityWorkerCount,
		"qualityProbeTimeoutSeconds": settings.QualityProbeTimeoutSeconds,
		"qualitySoftTPS":             settings.QualitySoftTPS,
		"qualityHardTPS":             settings.QualityHardTPS,
	}
}

func dispatchAPIWithStore(store *ipStore, method, path string, query url.Values, body json.RawMessage) ([]byte, error) {
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		path = "/"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")

	switch {
	case path == "/batches" && method == http.MethodGet:
		items, err := store.listBatches()
		if err != nil {
			return managementJSON(http.StatusInternalServerError, errorMessage("batchesFailed", err.Error()))
		}
		return managementJSON(http.StatusOK, map[string]any{
			"data": map[string]any{"items": publicBatches(items), "total": len(items)},
		})

	case len(parts) == 3 && parts[0] == "batches" && parts[2] == "nodes" && method == http.MethodGet:
		batchID := strings.TrimSpace(parts[1])
		if batchID == "" {
			return managementJSON(http.StatusBadRequest, errorMessage("invalidBatchID", "invalid batch id"))
		}
		items, err := store.listNodesByBatch(batchID)
		if err != nil {
			return managementJSON(http.StatusInternalServerError, errorMessage("batchNodesFailed", err.Error()))
		}
		return managementJSON(http.StatusOK, map[string]any{
			"data": map[string]any{"batchId": batchID, "items": publicNodes(items), "total": len(items)},
		})

	case path == "/nodes" && method == http.MethodGet:
		status := strings.TrimSpace(query.Get("status"))
		if status == "" {
			status = statusAll
		}
		items, err := store.listNodes(status)
		if err != nil {
			return managementJSON(http.StatusInternalServerError, errorMessage("listFailed", err.Error()))
		}
		return managementJSON(http.StatusOK, map[string]any{
			"data": map[string]any{"items": publicNodes(items), "total": len(items)},
		})

	case path == "/nodes" && method == http.MethodPost:
		return addNodes(store, body)

	case path == "/nodes/errors" && method == http.MethodDelete:
		errorReason := strings.TrimSpace(query.Get("reason"))
		if errorReason == "" {
			return managementJSON(http.StatusBadRequest, errorMessage("invalidErrorReason", "error reason is required"))
		}
		deleted, err := store.deleteErrorNodes(errorReason)
		if err != nil {
			_ = store.appendLog(logLevelError, "nodes.error_delete_failed", 0, "", "按异常原因删除节点失败", fmt.Sprintf("原因 %s；%s", errorReason, err.Error()))
			return managementJSON(http.StatusInternalServerError, errorMessage("deleteFailed", err.Error()))
		}
		_ = store.appendLog(logLevelInfo, "nodes.error_deleted", 0, "", fmt.Sprintf("按异常原因删除节点：%s，共 %d 个", errorReason, deleted), errorReason)
		return managementJSON(http.StatusOK, map[string]any{"data": map[string]any{
			"reason":  errorReason,
			"deleted": deleted,
		}})

	case path == "/summary" && method == http.MethodGet:
		counts, err := store.summary()
		if err != nil {
			return managementJSON(http.StatusInternalServerError, errorMessage("summaryFailed", err.Error()))
		}
		return managementJSON(http.StatusOK, map[string]any{"data": counts})

	case path == "/logs/groups" && method == http.MethodGet:
		category := strings.TrimSpace(query.Get("category"))
		groups, err := store.listLogGroups(category)
		if err != nil {
			return managementJSON(http.StatusBadRequest, errorMessage("logGroupsFailed", err.Error()))
		}
		return managementJSON(http.StatusOK, map[string]any{
			"data": map[string]any{
				"items":    publicLogGroups(groups),
				"total":    len(groups),
				"category": category,
				"max":      maxGroupedLogSets,
			},
		})

	case path == "/logs" && method == http.MethodGet:
		category := strings.TrimSpace(query.Get("category"))
		if category == "" {
			category = logCategoryGeneral
		}
		groupID := strings.TrimSpace(query.Get("groupId"))
		logStatus := strings.TrimSpace(query.Get("status"))
		if category != logCategoryGeneral && groupID == "" {
			return managementJSON(http.StatusBadRequest, errorMessage("invalidLogGroup", "group id is required for grouped logs"))
		}
		search := strings.TrimSpace(query.Get("search"))
		if search == "" {
			search = strings.TrimSpace(query.Get("q"))
		}
		var items []pluginLog
		var err error
		if category == logCategoryGeneral {
			items, err = store.listLogs(search)
		} else {
			items, err = store.listGroupedLogs(category, groupID, logStatus, search)
		}
		if err != nil {
			return managementJSON(http.StatusInternalServerError, errorMessage("logsFailed", err.Error()))
		}
		maxLogs := 0
		if category == logCategoryGeneral {
			maxLogs = maxPluginLogs
		}
		return managementJSON(http.StatusOK, map[string]any{
			"data": map[string]any{
				"items":    publicLogs(items),
				"total":    len(items),
				"max":      maxLogs,
				"search":   search,
				"category": category,
				"groupId":  groupID,
				"status":   logStatus,
			},
		})

	case path == "/settings" && method == http.MethodGet:
		settings, err := store.settings()
		if err != nil {
			return managementJSON(http.StatusInternalServerError, errorMessage("settingsFailed", err.Error()))
		}
		return managementJSON(http.StatusOK, map[string]any{"data": publicSettings(settings)})

	case len(parts) == 3 && parts[0] == "nodes" && parts[2] == "auth-bindings" && method == http.MethodGet:
		nodeID, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || nodeID <= 0 {
			return managementJSON(http.StatusBadRequest, errorMessage("invalidNodeID", "invalid node id"))
		}
		node, found, err := store.getNode(nodeID)
		if err != nil {
			return managementJSON(http.StatusInternalServerError, errorMessage("detailFailed", err.Error()))
		}
		if !found {
			return managementJSON(http.StatusNotFound, errorMessage("notFound", "node not found"))
		}
		if node.Status != statusHealthy {
			return managementJSON(http.StatusBadRequest, errorMessage("invalidNodeStatus", "only healthy nodes have auth bindings"))
		}
		bindings, err := store.listHealthyAuthBindings(nodeID)
		if err != nil {
			return managementJSON(http.StatusInternalServerError, errorMessage("authBindingsFailed", err.Error()))
		}
		verifiedCount := 0
		failedCount := 0
		for _, binding := range bindings {
			switch binding.SyncStatus {
			case "verified":
				verifiedCount++
			case "failed":
				failedCount++
			}
		}
		return managementJSON(http.StatusOK, map[string]any{
			"data": map[string]any{
				"nodeId":        node.ID,
				"nodeName":      node.Name,
				"proxyUrl":      node.ProxyURL,
				"total":         len(bindings),
				"verifiedCount": verifiedCount,
				"failedCount":   failedCount,
				"items":         publicAuthBindings(bindings),
			},
		})

	case len(parts) == 3 && parts[0] == "nodes" && parts[2] == "error" && method == http.MethodGet:
		nodeID, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || nodeID <= 0 {
			return managementJSON(http.StatusBadRequest, errorMessage("invalidNodeID", "invalid node id"))
		}
		node, found, err := store.getNode(nodeID)
		if err != nil {
			return managementJSON(http.StatusInternalServerError, errorMessage("detailFailed", err.Error()))
		}
		if !found {
			return managementJSON(http.StatusNotFound, errorMessage("notFound", "node not found"))
		}
		return managementJSON(http.StatusOK, map[string]any{"data": map[string]any{
			"id":     node.ID,
			"name":   node.Name,
			"reason": node.ErrorReason,
			"detail": node.ErrorDetail,
		}})
	}

	return managementJSON(http.StatusMethodNotAllowed, errorMessage("notFound", "not found"))
}

func addNodes(store *ipStore, body json.RawMessage) ([]byte, error) {
	var payload struct {
		Text           string `json:"text"`
		IPs            string `json:"ips"`
		DeleteNonUS    bool   `json:"deleteNonUS"`
		ManualFallback bool   `json:"manualFallback"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return managementJSON(http.StatusBadRequest, errorMessage("invalidBody", "invalid body"))
	}
	text := strings.TrimSpace(payload.Text)
	if text == "" {
		text = strings.TrimSpace(payload.IPs)
	}
	if text == "" {
		return managementJSON(http.StatusBadRequest, errorMessage("emptyInput", "至少录入一行 IP"))
	}

	nodes, inputErrors := parseProxyLines(text)
	batchID, added, duplicates, err := store.insertNodes(nodes, len(inputErrors), payload.DeleteNonUS, payload.ManualFallback)
	if err != nil {
		_ = store.appendLog(logLevelError, "nodes.insert_failed", 0, "", "录入节点失败", err.Error())
		return managementJSON(http.StatusInternalServerError, errorMessage("insertFailed", err.Error()))
	}
	result := map[string]any{
		"batchId":    batchID,
		"added":      added,
		"duplicates": duplicates,
		"errors":     inputErrors,
	}
	logLevel := logLevelInfo
	if len(inputErrors) > 0 {
		logLevel = logLevelWarn
	}
	inputMode := "初次探测"
	if payload.ManualFallback {
		inputMode = "手动健康保底"
	}
	_ = store.appendLog(
		logLevel,
		"nodes.inserted",
		0,
		"",
		fmt.Sprintf("节点录入完成：新增 %d，重复 %d，格式错误 %d；模式 %s", added, duplicates, len(inputErrors), inputMode),
		fmt.Sprintf("批次 %s；%s", batchID, formatInputErrors(inputErrors)),
	)
	if added > 0 && !payload.ManualFallback {
		_ = store.appendProbeLog(
			logCategoryBatchProbe,
			batchID,
			logStatusProbing,
			logLevelInfo,
			"probe.batch_started",
			0,
			"",
			"批次探测已创建",
			fmt.Sprintf("批次 %s，节点 %d 个", batchID, added),
		)
	}
	if added == 0 && duplicates == 0 && len(inputErrors) > 0 {
		return managementJSON(http.StatusBadRequest, map[string]any{
			"error": map[string]string{"code": "invalidInput", "message": "没有可录入的有效 IP"},
			"data":  result,
		})
	}
	return managementJSON(http.StatusOK, map[string]any{"data": result})
}

func formatInputErrors(inputErrors []inputLineError) string {
	if len(inputErrors) == 0 {
		return "无格式错误"
	}
	details := make([]string, 0, len(inputErrors))
	for _, inputError := range inputErrors {
		details = append(details, fmt.Sprintf("第 %d 行：%s", inputError.Line, inputError.Message))
	}
	return strings.Join(details, "；")
}

func publicBatches(batches []ipBatch) []map[string]any {
	items := make([]map[string]any, 0, len(batches))
	for _, batch := range batches {
		items = append(items, map[string]any{
			"batchId":                batch.ID,
			"sequenceNumber":         batch.SequenceNumber,
			"createdAt":              batch.CreatedAt,
			"totalCount":             batch.TotalCount,
			"duplicateCount":         batch.DuplicateCount,
			"inputErrorCount":        batch.InputErrorCount,
			"deleteNonUS":            batch.DeleteNonUS == 1,
			"completedCount":         batch.CompletedCount,
			"pendingCount":           batch.PendingCount,
			"candidateCount":         batch.CandidateCount,
			"initialConnectedCount":  batch.InitialConnectedCount,
			"realtimeConnectedCount": batch.RealtimeConnectedCount,
		})
	}
	return items
}

func publicNodes(nodes []proxyNode) []map[string]any {
	items := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		items = append(items, map[string]any{
			"id":                 node.ID,
			"name":               node.Name,
			"batchId":            node.BatchID,
			"protocol":           node.Protocol,
			"status":             node.Status,
			"initialConnected":   node.InitialConnected == 1,
			"latencyMs":          node.LatencyMs,
			"enteredAt":          node.EnteredAt,
			"probeStartedAt":     node.ProbeStartedAt,
			"probeTime":          node.ProbeTime,
			"exitIp":             node.ExitIP,
			"exitCountry":        node.ExitCountry,
			"reviveFailureCount": node.ReviveFailureCount,
			"slotId":             node.SlotID,
			"fallbackOrigin":     node.FallbackOrigin,
			"errorReason":        node.ErrorReason,
			"errorDetail":        node.ErrorDetail,
		})
	}
	return items
}

func publicAuthBindings(bindings []authBinding) []map[string]any {
	items := make([]map[string]any, 0, len(bindings))
	for _, binding := range bindings {
		items = append(items, map[string]any{
			"authName":   binding.AuthName,
			"authIndex":  binding.AuthIndex,
			"slotId":     binding.SlotID,
			"nodeId":     binding.NodeID,
			"proxyUrl":   binding.ProxyURL,
			"syncStatus": binding.SyncStatus,
			"syncError":  binding.SyncError,
			"verifiedAt": binding.VerifiedAt,
			"updatedAt":  binding.UpdatedAt,
		})
	}
	return items
}

func publicLogs(logs []pluginLog) []map[string]any {
	items := make([]map[string]any, 0, len(logs))
	for _, logEntry := range logs {
		items = append(items, map[string]any{
			"id":        logEntry.ID,
			"createdAt": logEntry.CreatedAt,
			"level":     logEntry.Level,
			"event":     logEntry.Event,
			"category":  logEntry.Category,
			"groupId":   logEntry.GroupID,
			"status":    logEntry.LogStatus,
			"nodeId":    logEntry.NodeID,
			"nodeName":  logEntry.NodeName,
			"message":   logEntry.Message,
			"detail":    logEntry.Detail,
		})
	}
	return items
}

func publicLogGroups(groups []logGroup) []map[string]any {
	items := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		items = append(items, map[string]any{
			"id":                      group.ID,
			"sequenceNumber":          group.SequenceNumber,
			"startedAt":               group.StartedAt,
			"completedAt":             group.CompletedAt,
			"status":                  group.Status,
			"logCount":                group.LogCount,
			"category":                group.Category,
			"candidateCount":          group.CandidateCount,
			"successCount":            group.SuccessCount,
			"failureCount":            group.FailureCount,
			"connectivityCompletedAt": group.ConnectivityCompletedAt,
			"qualityStartedAt":        group.QualityStartedAt,
			"qualityCompletedAt":      group.QualityCompletedAt,
			"qualityCandidateCount":   group.QualityCandidateCount,
			"qualitySuccessCount":     group.QualitySuccessCount,
			"qualityFailureCount":     group.QualityFailureCount,
		})
	}
	return items
}

func integerValue(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), typed == float64(int(typed))
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	case int:
		return typed, true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		return 0, false
	}
}

func managementJSON(statusCode int, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{
		StatusCode: statusCode,
		Headers:    http.Header{"content-type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	})
}

func errorMessage(code, message string) map[string]any {
	return map[string]any{"error": map[string]string{"code": code, "message": message}}
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	pointer := C.CBytes(raw)
	if pointer == nil {
		return
	}
	response.ptr = pointer
	response.len = C.size_t(len(raw))
}
