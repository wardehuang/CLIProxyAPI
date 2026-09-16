package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

const (
	realtimeGuardSlotIDMetadataKey           = "cpa_xai_ip_switcher.guard_slot_id"
	realtimeGuardNodeIDMetadataKey           = "cpa_xai_ip_switcher.guard_node_id"
	realtimeGuardAuthIdentityMetadataKey     = "cpa_xai_ip_switcher.guard_auth_identity"
	realtimeGuardProxyURLMetadataKey         = "cpa_xai_ip_switcher.guard_proxy_url"
	realtimeGuardBindingUpdatedAtMetadataKey = "cpa_xai_ip_switcher.guard_binding_updated_at"
	realtimeGuardSlotRefreshAtMetadataKey    = "cpa_xai_ip_switcher.guard_slot_refresh_at"
	realtimeGuardSourceErrorMetadataKey      = "cpa_xai_ip_switcher.guard_source_error"
)

type realtimeGuardSourceSnapshot struct {
	SlotID           int64
	NodeID           int64
	AuthIdentity     string
	ProxyURL         string
	BindingUpdatedAt int64
	SlotRefreshAt    int64
}

func finalizeRealtimeGuardRequest(request pluginapi.RequestFinalizeRequest) (pluginapi.RequestFinalizeResponse, error) {
	if !request.Stream || !strings.EqualFold(realtimeGuardMetadataString(request.Metadata, executor.SelectedAuthProviderMetadataKey), "xai") {
		return pluginapi.RequestFinalizeResponse{}, nil
	}
	timeoutMetadata := map[string]any{}
	response := pluginapi.RequestFinalizeResponse{
		Metadata: timeoutMetadata,
		ClearMetadata: []string{
			realtimeGuardSlotIDMetadataKey, realtimeGuardNodeIDMetadataKey,
			realtimeGuardAuthIdentityMetadataKey, realtimeGuardProxyURLMetadataKey,
			realtimeGuardBindingUpdatedAtMetadataKey, realtimeGuardSlotRefreshAtMetadataKey,
			realtimeGuardSourceErrorMetadataKey,
		},
	}
	if settings, settingsErr := pluginRuntime.currentSettings(); settingsErr == nil {
		timeoutMetadata[executor.StreamCompletionTimeoutSecondsMetadataKey] = settings.RealtimeGuardTimeoutSeconds
		timeoutMetadata[executor.StreamProgressTimeoutSecondsMetadataKey] = settings.RealtimeGuardIdleTimeoutSeconds
	}

	authIndex := realtimeGuardMetadataString(request.Metadata, executor.SelectedAuthIndexMetadataKey)
	proxyURL := realtimeGuardMetadataString(request.Metadata, executor.SelectedAuthProxyURLMetadataKey)
	if authIndex == "" || proxyURL == "" {
		log.WithField("auth_index", authIndex).Warn("Realtime guard source metadata is incomplete")
		return response, nil
	}

	var snapshot realtimeGuardSourceSnapshot
	_, err := pluginRuntime.withStore(func(store *ipStore) ([]byte, error) {
		var lookupErr error
		snapshot, lookupErr = store.lookupRealtimeGuardSource(authIndex, proxyURL)
		return nil, lookupErr
	})
	if err != nil {
		log.WithError(err).WithField("auth_index", authIndex).Warn("Failed to capture realtime guard source snapshot")
		if !errors.Is(err, sql.ErrNoRows) {
			// Finalizer errors are fail-open in pluginhost. Carry the failure to
			// the completion guard instead of disguising it as a missing binding.
			timeoutMetadata[realtimeGuardSourceErrorMetadataKey] = "Realtime guard source snapshot lookup failed"
		}
		return response, nil
	}

	timeoutMetadata[realtimeGuardSlotIDMetadataKey] = snapshot.SlotID
	timeoutMetadata[realtimeGuardNodeIDMetadataKey] = snapshot.NodeID
	timeoutMetadata[realtimeGuardAuthIdentityMetadataKey] = snapshot.AuthIdentity
	timeoutMetadata[realtimeGuardProxyURLMetadataKey] = snapshot.ProxyURL
	timeoutMetadata[realtimeGuardBindingUpdatedAtMetadataKey] = snapshot.BindingUpdatedAt
	timeoutMetadata[realtimeGuardSlotRefreshAtMetadataKey] = snapshot.SlotRefreshAt
	return response, nil
}

func (store *ipStore) lookupRealtimeGuardSource(authIndex, proxyURL string) (realtimeGuardSourceSnapshot, error) {
	var snapshot realtimeGuardSourceSnapshot
	err := store.database.QueryRow(`
SELECT bindings.slot_id, bindings.node_id, bindings.auth_identity, bindings.proxy_url, bindings.updated_at, slots.refresh_at
FROM ip_slot_auth_bindings AS bindings
JOIN ip_slots AS slots ON slots.slot_id = bindings.slot_id
JOIN ip_nodes AS nodes ON nodes.id = slots.node_id
WHERE bindings.auth_index = ?
  AND bindings.proxy_url = ?
  AND bindings.node_id = slots.node_id
  AND nodes.proxy_url = ?
  AND slots.slot_kind = ?
LIMIT 1`, strings.TrimSpace(authIndex), strings.TrimSpace(proxyURL), strings.TrimSpace(proxyURL), statusHealthy).Scan(
		&snapshot.SlotID,
		&snapshot.NodeID,
		&snapshot.AuthIdentity,
		&snapshot.ProxyURL,
		&snapshot.BindingUpdatedAt,
		&snapshot.SlotRefreshAt,
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
		SlotRefreshAt:    metadataInt64(metadata, realtimeGuardSlotRefreshAtMetadataKey),
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
