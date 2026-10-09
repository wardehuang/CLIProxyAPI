package main

/*
#include <stdint.h>
#include <stdlib.h>

// BEGIN detailed-logs plugin ABI bridge.
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
// END detailed-logs plugin ABI bridge.
*/
import "C"

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginName          = "detailed-logs"
	pluginVersion       = "0.1.0"
	resourcePath        = "/logs"
	managementAPIPath   = "/detailed-logs/api"
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

type managementRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
	Body    []byte      `json:"Body"`
}

type uiProxyRequest struct {
	Operation string     `json:"operation"`
	Query     url.Values `json:"query,omitempty"`
	Name      string     `json:"name,omitempty"`
	ID        string     `json:"id,omitempty"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ManagementAPI      bool `json:"management_api"`
	HostManagementLogs bool `json:"host_management_logs"`
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
func cliproxyPluginShutdown() {}

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
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(pluginapi.ManagementRegistrationResponse{
			Routes: []pluginapi.ManagementRoute{{
				Method:      http.MethodPost,
				Path:        managementAPIPath,
				Description: "Detailed server and request logs",
			}},
			Resources: []pluginapi.ResourceRoute{{
				Path:        resourcePath,
				Menu:        "详细日志",
				Description: "查看服务器日志和全部请求日志，并按需下载单个文件",
			}},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return nil, fmt.Errorf("unsupported plugin method %s", method)
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "wardehuang",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
		},
		Capabilities: registrationCapabilities{ManagementAPI: true, HostManagementLogs: true},
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
	switch path {
	case "", resourcePath, "/":
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Cache-Control": []string{"no-store"},
				"Content-Type":  []string{resourceContentType},
			},
			Body: []byte(strings.Replace(pageTemplate, "/*__HALLMARK_TOKENS__*/", tokenCSS, 1)),
		})
	case managementAPIPath:
		return handleUIProxy(management)
	default:
		return okEnvelope(managementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"content-type": []string{"text/plain; charset=utf-8"}},
			Body:       []byte("not found"),
		})
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
	if len(request.Body) == 0 {
		return managementJSON(http.StatusBadRequest, errorMessage("invalid_request", "request body is required"))
	}
	var proxyRequest uiProxyRequest
	if err := json.Unmarshal(request.Body, &proxyRequest); err != nil {
		return managementJSON(http.StatusBadRequest, errorMessage("invalid_request", "invalid request body"))
	}
	if !validUIOperation(proxyRequest.Operation) {
		return managementJSON(http.StatusNotFound, errorMessage("not_found", "log operation not found"))
	}

	raw, err := callHost(pluginabi.MethodHostManagementLogs, pluginapi.HostManagementLogsRequest{
		Operation: proxyRequest.Operation,
		Query:     proxyRequest.Query,
		Name:      proxyRequest.Name,
		ID:        proxyRequest.ID,
	})
	if err != nil {
		return managementJSON(http.StatusBadGateway, errorMessage("host_error", "host log service failed"))
	}
	var response pluginapi.HostManagementLogsResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return managementJSON(http.StatusBadGateway, errorMessage("host_error", "invalid host log response"))
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if !json.Valid(response.Body) {
			return managementJSON(http.StatusBadGateway, errorMessage("host_error", "invalid host error response"))
		}
		return managementJSON(response.StatusCode, json.RawMessage(response.Body))
	}

	switch proxyRequest.Operation {
	case pluginapi.HostManagementLogsOperationServerFile,
		pluginapi.HostManagementLogsOperationRequestLogFile:
		return managementJSON(response.StatusCode, map[string]string{
			"name":           proxyRequest.Name,
			"content_base64": base64.StdEncoding.EncodeToString(response.Body),
		})
	case pluginapi.HostManagementLogsOperationRequestFile:
		return managementJSON(response.StatusCode, map[string]string{
			"name":           "request-" + proxyRequest.ID + ".log",
			"content_base64": base64.StdEncoding.EncodeToString(response.Body),
		})
	default:
		if !json.Valid(response.Body) {
			return managementJSON(http.StatusBadGateway, errorMessage("host_error", "invalid host log response"))
		}
		return managementJSON(response.StatusCode, json.RawMessage(response.Body))
	}
}

func validUIOperation(operation string) bool {
	switch operation {
	case pluginapi.HostManagementLogsOperationStatus,
		pluginapi.HostManagementLogsOperationLogs,
		pluginapi.HostManagementLogsOperationServerFiles,
		pluginapi.HostManagementLogsOperationServerFile,
		pluginapi.HostManagementLogsOperationRequestFiles,
		pluginapi.HostManagementLogsOperationRequestLogFile,
		pluginapi.HostManagementLogsOperationRequestFile:
		return true
	default:
		return false
	}
}

func managementJSON(statusCode int, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{
		StatusCode: statusCode,
		Headers:    http.Header{"content-type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	})
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
