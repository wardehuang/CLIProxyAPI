package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type rpcHostStorageGetRequest struct {
	pluginapi.HostStorageGetRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcHostStorageSetRequest struct {
	pluginapi.HostStorageSetRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcHostStorageDeleteRequest struct {
	pluginapi.HostStorageDeleteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcHostStorageListRequest struct {
	pluginapi.HostStorageListRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (h *Host) callHostStorageGet(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostStorageGetRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host storage get request: %w", errUnmarshal)
	}
	pluginID := h.callbackCallerPluginID(ctx, req.HostCallbackID)
	value, found, errGet := h.storage.get(h.storageRoot(), pluginID, req.Key)
	if errGet != nil {
		return nil, errGet
	}
	return marshalRPCResult(pluginapi.HostStorageGetResponse{Value: value, Found: found})
}

func (h *Host) callHostStorageSet(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostStorageSetRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host storage set request: %w", errUnmarshal)
	}
	pluginID := h.callbackCallerPluginID(ctx, req.HostCallbackID)
	if errSet := h.storage.set(h.storageRoot(), pluginID, req.Key, req.Value); errSet != nil {
		return nil, errSet
	}
	return marshalRPCResult(rpcEmptyResponse{})
}

func (h *Host) callHostStorageDelete(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostStorageDeleteRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host storage delete request: %w", errUnmarshal)
	}
	pluginID := h.callbackCallerPluginID(ctx, req.HostCallbackID)
	if errDelete := h.storage.delete(h.storageRoot(), pluginID, req.Key); errDelete != nil {
		return nil, errDelete
	}
	return marshalRPCResult(rpcEmptyResponse{})
}

func (h *Host) callHostStorageList(ctx context.Context, request []byte) ([]byte, error) {
	var req rpcHostStorageListRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host storage list request: %w", errUnmarshal)
	}
	pluginID := h.callbackCallerPluginID(ctx, req.HostCallbackID)
	keys, errList := h.storage.list(h.storageRoot(), pluginID, req.Prefix)
	if errList != nil {
		return nil, errList
	}
	return marshalRPCResult(pluginapi.HostStorageListResponse{Keys: keys})
}
