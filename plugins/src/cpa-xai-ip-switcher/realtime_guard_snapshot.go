package main

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	realtimeGuardSlotIDMetadataKey           = "cpa_xai_ip_switcher.guard_slot_id"
	realtimeGuardNodeIDMetadataKey           = "cpa_xai_ip_switcher.guard_node_id"
	realtimeGuardAuthIdentityMetadataKey     = "cpa_xai_ip_switcher.guard_auth_identity"
	realtimeGuardProxyURLMetadataKey         = "cpa_xai_ip_switcher.guard_proxy_url"
	realtimeGuardBindingUpdatedAtMetadataKey = "cpa_xai_ip_switcher.guard_binding_updated_at"
)

type realtimeGuardSourceSnapshot struct {
	SlotID           int64
	NodeID           int64
	AuthIdentity     string
	ProxyURL         string
	BindingUpdatedAt int64
}

func finalizeRealtimeGuardRequest(request pluginapi.RequestFinalizeRequest) (pluginapi.RequestFinalizeResponse, error) {
	if !request.Stream || !strings.EqualFold(realtimeGuardMetadataString(request.Metadata, executor.SelectedAuthProviderMetadataKey), "xai") {
		return pluginapi.RequestFinalizeResponse{}, nil
	}

	authID := realtimeGuardMetadataString(request.Metadata, executor.SelectedAuthMetadataKey)
	authIndex := realtimeGuardMetadataString(request.Metadata, executor.SelectedAuthIndexMetadataKey)
	proxyURL := realtimeGuardMetadataString(request.Metadata, executor.SelectedAuthProxyURLMetadataKey)
	if authID == "" || proxyURL == "" {
		return pluginapi.RequestFinalizeResponse{}, nil
	}

	var snapshot realtimeGuardSourceSnapshot
	_, err := pluginRuntime.withStore(func(store *ipStore) ([]byte, error) {
		var lookupErr error
		snapshot, lookupErr = store.lookupRealtimeGuardSource(authID, authIndex, proxyURL)
		return nil, lookupErr
	})
	if err != nil {
		return pluginapi.RequestFinalizeResponse{}, nil
	}

	return pluginapi.RequestFinalizeResponse{Metadata: map[string]any{
		realtimeGuardSlotIDMetadataKey:           snapshot.SlotID,
		realtimeGuardNodeIDMetadataKey:           snapshot.NodeID,
		realtimeGuardAuthIdentityMetadataKey:     snapshot.AuthIdentity,
		realtimeGuardProxyURLMetadataKey:         snapshot.ProxyURL,
		realtimeGuardBindingUpdatedAtMetadataKey: snapshot.BindingUpdatedAt,
	}}, nil
}

func (store *ipStore) lookupRealtimeGuardSource(authID, authIndex, proxyURL string) (realtimeGuardSourceSnapshot, error) {
	authIdentity := strings.TrimSpace(authID)
	if strings.TrimSpace(authIndex) != "" {
		authIdentity += "#" + strings.TrimSpace(authIndex)
	}
	var snapshot realtimeGuardSourceSnapshot
	err := store.database.QueryRow(`
SELECT bindings.slot_id, bindings.node_id, bindings.auth_identity, bindings.proxy_url, bindings.updated_at
FROM ip_slot_auth_bindings AS bindings
JOIN ip_slots AS slots ON slots.slot_id = bindings.slot_id
JOIN ip_nodes AS nodes ON nodes.id = slots.node_id
WHERE bindings.auth_identity = ?
  AND bindings.proxy_url = ?
  AND bindings.node_id = slots.node_id
  AND nodes.proxy_url = ?
  AND slots.slot_kind = ?
  AND nodes.status = ?
LIMIT 1`, authIdentity, strings.TrimSpace(proxyURL), strings.TrimSpace(proxyURL), statusHealthy, statusHealthy).Scan(
		&snapshot.SlotID,
		&snapshot.NodeID,
		&snapshot.AuthIdentity,
		&snapshot.ProxyURL,
		&snapshot.BindingUpdatedAt,
	)
	if err != nil {
		return realtimeGuardSourceSnapshot{}, fmt.Errorf("resolve xAI realtime guard source binding: %w", err)
	}
	return snapshot, nil
}

func realtimeGuardSnapshotFromMetadata(metadata map[string]any) realtimeGuardSourceSnapshot {
	return realtimeGuardSourceSnapshot{
		SlotID:           metadataInt64(metadata, realtimeGuardSlotIDMetadataKey),
		NodeID:           metadataInt64(metadata, realtimeGuardNodeIDMetadataKey),
		AuthIdentity:     realtimeGuardMetadataString(metadata, realtimeGuardAuthIdentityMetadataKey),
		ProxyURL:         realtimeGuardMetadataString(metadata, realtimeGuardProxyURLMetadataKey),
		BindingUpdatedAt: metadataInt64(metadata, realtimeGuardBindingUpdatedAtMetadataKey),
	}
}

func realtimeGuardMetadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func metadataInt64(metadata map[string]any, key string) int64 {
	if metadata == nil {
		return 0
	}
	switch value := metadata[key].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	default:
		return 0
	}
}
