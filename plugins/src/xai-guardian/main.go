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
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginName          = "xai-guardian"
	pluginVersion       = "0.1.0"
	resourcePath        = "/status"
	managementAPIPath   = "/xai-guardian/api"
	resourceContentType = "text/html; charset=utf-8"
	pluginUIHeader      = "X-CPA-Plugin-UI"
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

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ManagementAPI             bool `json:"management_api"`
	Scheduler                 bool `json:"scheduler"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities"`
	RequestLifecyclePlugin    bool `json:"request_lifecycle_plugin"`
	XAIStreamGuard            bool `json:"xai_stream_guard"`
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
	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
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
	guardianRuntime.shutdown()
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
		code := "host_callback_failed"
		if callbackEnvelope.Error != nil && strings.TrimSpace(callbackEnvelope.Error.Code) != "" {
			code = strings.TrimSpace(callbackEnvelope.Error.Code)
		}
		return nil, fmt.Errorf("%s", code)
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
		if err := guardianRuntime.configure(config); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodManagementRegister:
		return okEnvelope(pluginapi.ManagementRegistrationResponse{
			Routes: []pluginapi.ManagementRoute{
				{Method: http.MethodPost, Path: managementAPIPath, Description: "xAI Guardian API"},
			},
			Resources: []pluginapi.ResourceRoute{{Path: resourcePath, Menu: "xAI降智守护", Description: "xAI Guardian：账号状态、服务端巡检和降智守护"}},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	case pluginabi.MethodXAIStreamPrepare:
		var prepare pluginapi.XAIStreamPrepareRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &prepare); err != nil {
				return nil, fmt.Errorf("decode xAI prepare request: %w", err)
			}
		}
		response, err := prepareXAIStream(context.Background(), prepare)
		if err != nil {
			return nil, err
		}
		return okEnvelope(response)
	case pluginabi.MethodXAIStreamComplete:
		var completion pluginapi.XAIStreamCompletionRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &completion); err != nil {
				return nil, fmt.Errorf("decode xAI completion request: %w", err)
			}
		}
		response, err := completeXAIStream(context.Background(), completion)
		if err != nil {
			return nil, err
		}
		return okEnvelope(response)
	case pluginabi.MethodSchedulerPick:
		var pickRequest pluginapi.SchedulerPickRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &pickRequest); err != nil {
				return nil, fmt.Errorf("decode scheduler pick request: %w", err)
			}
		}
		response, err := guardianRuntime.schedulerPick(pickRequest)
		if err != nil {
			return nil, err
		}
		return okEnvelope(response)
	case pluginabi.MethodRequestComplete:
		var completion pluginapi.RequestCompletion
		if len(request) > 0 {
			if err := json.Unmarshal(request, &completion); err != nil {
				return nil, fmt.Errorf("decode request completion: %w", err)
			}
		}
		guardianRuntime.scheduleGroups.release(guardianRuntime.currentStore(), completion)
		return okEnvelope(map[string]any{})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func parsePluginConfig(raw []byte) (pluginConfig, error) {
	config := pluginConfig{DatabasePath: defaultDatabasePath}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &config); err != nil {
			return pluginConfig{}, fmt.Errorf("decode plugin config: %w", err)
		}
		var fields map[string]any
		if err := yaml.Unmarshal(raw, &fields); err != nil {
			return pluginConfig{}, fmt.Errorf("decode plugin config fields: %w", err)
		}
		_, config.inspectionIntervalSet = fields["inspection_interval_seconds"]
	}
	if strings.TrimSpace(config.DatabasePath) == "" {
		config.DatabasePath = defaultDatabasePath
	}
	if config.inspectionIntervalSet && (config.InspectionIntervalSeconds < 0 || config.InspectionIntervalSeconds > maxInspectionIntervalSeconds) {
		return pluginConfig{}, fmt.Errorf("inspection_interval_seconds must be between 0 and %d", maxInspectionIntervalSeconds)
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
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "database_path", Type: pluginapi.ConfigFieldTypeString, Description: "Plugin-owned SQLite path. Credentials are never stored here."},
				{Name: "inspection_interval_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Automatic server inspection interval; 0 disables the worker."},
			},
		},
		Capabilities: registrationCapabilities{ManagementAPI: true, Scheduler: true, SchedulerAcrossPriorities: true, RequestLifecyclePlugin: true, XAIStreamGuard: true},
	}
}

func handleManagement(request []byte) ([]byte, error) {
	var management managementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &management); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
	}
	path := strings.TrimSuffix(strings.TrimSpace(management.Path), "/")
	resourceBasePath := "/v0/resource/plugins/" + pluginName
	path = stripManagementBasePath(path, "/v0/management")
	path = stripManagementBasePath(path, resourceBasePath)
	switch {
	case path == "", path == resourcePath, path == "/":
		return okEnvelope(managementResponse{StatusCode: http.StatusOK, Headers: http.Header{"cache-control": []string{"no-store"}, "content-type": []string{resourceContentType}}, Body: []byte(strings.Replace(pageTemplate, "/*__HALLMARK_TOKENS__*/", tokenCSS, 1))})
	case path == managementAPIPath:
		return handleUIProxy(management)
	default:
		return okEnvelope(managementResponse{StatusCode: http.StatusNotFound, Headers: http.Header{"content-type": []string{"text/plain; charset=utf-8"}}, Body: []byte("not found")})
	}
}

func stripManagementBasePath(path, base string) string {
	if path == base {
		return "/"
	}
	if strings.HasPrefix(path, base+"/") {
		return strings.TrimPrefix(path, base)
	}
	return path
}

func handleUIProxy(request managementRequest) ([]byte, error) {
	if !strings.EqualFold(strings.TrimSpace(request.Method), http.MethodPost) || strings.TrimSpace(request.Headers.Get(pluginUIHeader)) != pluginName {
		return managementJSON(http.StatusForbidden, errorMessage("forbidden", "forbidden"))
	}
	var proxyRequest uiProxyRequest
	if len(request.Body) == 0 {
		return managementJSON(http.StatusBadRequest, errorMessage("invalid_request", "request body is required"))
	}
	if err := json.Unmarshal(request.Body, &proxyRequest); err != nil {
		return managementJSON(http.StatusBadRequest, errorMessage("invalid_request", "invalid request body"))
	}
	parsed, err := url.ParseRequestURI(strings.TrimSpace(proxyRequest.Path))
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/") {
		return managementJSON(http.StatusBadRequest, errorMessage("invalid_path", "invalid API path"))
	}
	method := strings.ToUpper(strings.TrimSpace(proxyRequest.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !isAllowedUIPath(method, parsed.Path) {
		return managementJSON(http.StatusNotFound, errorMessage("not_found", "API path not found"))
	}
	body := []byte(proxyRequest.Body)
	if len(proxyRequest.Body) == 0 || string(proxyRequest.Body) == "null" {
		body = nil
	}
	statusCode, responseBody, err := guardianRuntime.api(method, parsed.Path, parsed.Query(), body)
	if err != nil {
		return managementJSON(http.StatusInternalServerError, errorMessage("plugin_error", err.Error()))
	}
	return managementJSON(statusCode, json.RawMessage(responseBody))
}

func isAllowedUIPath(method, path string) bool {
	switch method {
	case http.MethodGet:
		switch path {
		case "/api/summary", "/api/schedule-groups/counters", "/api/settings", "/api/accounts", "/api/batch-nodes", "/api/batches", "/api/inspection", "/api/inspection/runs", "/api/keepalive", "/api/degradation", "/api/logs", "/api/logs/groups", "/api/auths/refresh-proxy-urls/status":
			return true
		default:
			return (strings.HasPrefix(path, "/api/batches/") && strings.HasSuffix(path, "/nodes")) || (strings.HasPrefix(path, "/api/nodes/") && strings.HasSuffix(path, "/auth-bindings"))
		}
	case http.MethodPut:
		return path == "/api/settings"
	case http.MethodPost:
		if path == "/api/accounts/refresh" || path == "/api/accounts/degradation-check" || path == "/api/batches" || path == "/api/inspection" || path == "/api/keepalive/run" || path == "/api/degradation/clear" || path == "/api/auths/refresh-proxy-urls" {
			return true
		}
		return false
	default:
		return false
	}
}

func managementJSON(statusCode int, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{StatusCode: statusCode, Headers: http.Header{"content-type": []string{"application/json; charset=utf-8"}}, Body: raw})
}

func errorMessage(code, message string) map[string]any {
	return map[string]any{"ok": false, "error": map[string]string{"code": code, "message": message}}
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
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
