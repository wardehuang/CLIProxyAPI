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
	"encoding/json"
	"fmt"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	RequestFinalizer       bool `json:"request_finalizer"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	ResponseInterceptor    bool `json:"response_interceptor"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
}

type requestFinalizeRequest struct {
	pluginapi.RequestFinalizeRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type responseInterceptRequest struct {
	pluginapi.ResponseInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type streamChunkInterceptRequest struct {
	pluginapi.StreamChunkInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type requestCompletionRequest struct {
	pluginapi.RequestCompletion
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
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
	raw, errHandle := handleMethod(context.Background(), C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	resetReasoningCache(currentPluginConfig().MaxEntries, 0)
}

func handleMethod(ctx context.Context, method string, request []byte) ([]byte, error) {
	_ = ctx
	started := time.Now()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
			}
		}
		configurePlugin(req.ConfigYAML)
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodRequestFinalize:
		var req requestFinalizeRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode request finalize request: %w", errUnmarshal)
		}
		resp := finalizeDeepseekRequest(req.RequestFinalizeRequest, req.HostCallbackID)
		logPluginDebug(req.HostCallbackID, "method request.finalize done", map[string]any{
			"elapsed_ms":     elapsedMS(started),
			"body_rewritten": len(resp.Body) > 0,
			"request_bytes":  len(request),
		})
		return okEnvelope(resp)
	case pluginabi.MethodResponseInterceptAfter:
		var req responseInterceptRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode response intercept request: %w", errUnmarshal)
		}
		resp := cacheFromResponseBody(req.ResponseInterceptRequest, req.HostCallbackID)
		logPluginDebug(req.HostCallbackID, "method response.intercept_after done", map[string]any{
			"elapsed_ms":    elapsedMS(started),
			"request_bytes": len(request),
			"status_code":   req.StatusCode,
			"body_rewrite":  len(resp.Body) > 0,
		})
		return okEnvelope(resp)
	case pluginabi.MethodResponseInterceptStreamChunk:
		var req streamChunkInterceptRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode stream chunk intercept request: %w", errUnmarshal)
		}
		resp := observeStreamChunk(req.StreamChunkInterceptRequest, req.HostCallbackID)
		// observeStreamChunk already logs details when debug is on.
		_ = resp
		return okEnvelope(resp)
	case pluginabi.MethodRequestComplete:
		var req requestCompletionRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode request completion: %w", errUnmarshal)
		}
		completeRequestLifecycle(req.RequestCompletion, req.HostCallbackID)
		logPluginDebug(req.HostCallbackID, "method request.complete done", map[string]any{
			"elapsed_ms": elapsedMS(started),
			"outcome":    string(req.Outcome),
			"request_id": req.RequestID,
		})
		return okEnvelope(struct{}{})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
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
					Name:        "enabled",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Enable DeepSeek reasoning_content cache and inject/pad on upstream chat requests.",
				},
				{
					Name:        "debug",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Write detailed cache/inject diagnostics to the host log.",
				},
				{
					Name:        "pad_placeholder",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Fallback reasoning_content when cache misses. Default is a single space.",
				},
				{
					Name:        "max_entries",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "In-memory cache capacity. Default 4096.",
				},
				{
					Name:        "ttl_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Cache entry TTL in seconds. Default 3600.",
				},
				{
					Name:        "model_substrings",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Comma-separated model name needles (matched case-insensitively). Default: deepseek",
				},
			},
		},
		Capabilities: registrationCapabilities{
			RequestFinalizer:       true,
			RequestLifecyclePlugin: true,
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
		},
	}
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback payload %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
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

	var env envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host callback envelope %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
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
