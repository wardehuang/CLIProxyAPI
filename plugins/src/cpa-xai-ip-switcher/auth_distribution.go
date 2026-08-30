package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type authDistributionUpdate struct {
	auth        authFile
	originalRaw map[string]any
	proxyURL    string
	binding     authBinding
}

func refreshHealthyAuthDistribution(store *ipStore, healthySlotCount int) error {
	return refreshHealthyAuthDistributionWithRejectedProxy(store, healthySlotCount, "")
}

func refreshHealthyAuthDistributionAfterRealtimeGuard(store *ipStore, healthySlotCount int, rejectedProxyURL string) error {
	rejectedProxyURL = strings.TrimSpace(rejectedProxyURL)
	if rejectedProxyURL == "" {
		return fmt.Errorf("realtime guard rejected proxy_url is required")
	}
	return refreshHealthyAuthDistributionWithRejectedProxy(store, healthySlotCount, rejectedProxyURL)
}

func refreshHealthyAuthDistributionWithRejectedProxy(store *ipStore, healthySlotCount int, rejectedProxyURL string) error {
	if healthySlotCount < 1 {
		return fmt.Errorf("healthy slot count must be positive")
	}
	authEntries, err := listXAIAuthEntries()
	if err != nil {
		if isHostAuthIndexUnavailable(err) {
			_ = store.appendLog(logLevelWarn, "auth_distribution.deferred", 0, "", "Host Auth 暂未提供稳定 auth_index，保留当前绑定并跳过本轮文本同步", err.Error())
			return nil
		}
		return fmt.Errorf("list xAI auth files for distribution: %w", err)
	}

	updates := make([]authDistributionUpdate, 0, len(authEntries))
	for fileIndex, entry := range authEntries {
		slotID := int64(fileIndex%healthySlotCount + 1)
		slot, found, findErr := store.findSlotByID(slotID)
		if findErr != nil {
			return findErr
		}
		if !found || slot.Kind != statusHealthy {
			return fmt.Errorf("healthy auth distribution slot %d is unavailable", slotID)
		}
		proxyURL, proxyErr := store.healthySlotProxyURL(slot)
		if proxyErr != nil {
			return proxyErr
		}
		auth, readErr := authFileFromDirectWriteEntry(entry)
		if readErr != nil {
			return fmt.Errorf("read xAI auth text for distribution: %w", readErr)
		}
		updates = append(updates, authDistributionUpdate{
			auth:        auth,
			originalRaw: cloneAuthJSON(auth.Raw),
			proxyURL:    proxyURL,
			binding:     buildVerifiedAuthBinding(slotID, slot.NodeID, auth, proxyURL),
		})
	}

	appliedCount := 0
	for updateIndex := range updates {
		update := &updates[updateIndex]
		if update.auth.ProxyURL == update.proxyURL {
			appliedCount++
			continue
		}
		if err := setAuthProxyURLDirect(update.auth, update.proxyURL); err != nil {
			if rollbackErr := rollbackAuthDistribution(updates[:appliedCount]); rollbackErr != nil {
				return fmt.Errorf("sync xAI auth %s: %w; rollback failed: %v", update.auth.Name, err, rollbackErr)
			}
			return fmt.Errorf("sync xAI auth %s: %w", update.auth.Name, err)
		}
		appliedCount++
	}
	bindings := make([]authBinding, 0, len(updates))
	externallyChangedCount := 0
	for _, update := range updates {
		verifiedAuth, verifyErr := loadAuthFileForDirectWrite(update.auth)
		if verifyErr == nil && rejectedProxyURL == "" && verifiedAuth.ProxyURL != update.proxyURL {
			verifyErr = fmt.Errorf("proxy_url mismatch: expected %q, actual %q", update.proxyURL, verifiedAuth.ProxyURL)
		}
		if verifyErr == nil && rejectedProxyURL != "" && verifiedAuth.ProxyURL == rejectedProxyURL {
			verifyErr = fmt.Errorf("proxy_url still points to replaced node")
		}
		if verifyErr != nil {
			if rollbackErr := rollbackAuthDistribution(updates[:appliedCount]); rollbackErr != nil {
				return fmt.Errorf("verify xAI auth %s: %w; rollback failed: %v", update.auth.Name, verifyErr, rollbackErr)
			}
			return fmt.Errorf("verify xAI auth %s: %w", update.auth.Name, verifyErr)
		}
		binding := update.binding
		if rejectedProxyURL != "" && verifiedAuth.ProxyURL != update.proxyURL {
			binding, verifyErr = store.buildObservedAuthBinding(verifiedAuth)
			if verifyErr != nil {
				return fmt.Errorf("resolve observed xAI auth %s binding: %w", update.auth.Name, verifyErr)
			}
			externallyChangedCount++
		}
		bindings = append(bindings, binding)
	}
	if err := store.replaceAuthBindings(bindings); err != nil {
		if rollbackErr := rollbackAuthDistribution(updates[:appliedCount]); rollbackErr != nil {
			return fmt.Errorf("replace xAI auth bindings: %w; rollback failed: %v", err, rollbackErr)
		}
		return fmt.Errorf("replace xAI auth bindings: %w", err)
	}
	message := "xAI auth proxy_url 已直接写入文本并完整验证"
	detail := fmt.Sprintf("auth=%d；健康槽位=%d；空槽写入空 proxy_url", len(bindings), healthySlotCount)
	if rejectedProxyURL != "" {
		message = "实时守护已确认所有 xAI auth 不再指向被替换节点"
		detail += fmt.Sprintf("；外部改写=%d", externallyChangedCount)
	}
	_ = store.appendLog(logLevelInfo, "auth_distribution.verified", 0, "", message, detail)
	return nil
}

func (store *ipStore) buildObservedAuthBinding(auth authFile) (authBinding, error) {
	var slotID, nodeID int64
	err := store.database.QueryRow(`
SELECT slots.slot_id, nodes.id
FROM ip_slots AS slots
JOIN ip_nodes AS nodes ON nodes.id = slots.node_id
WHERE slots.slot_kind = ? AND nodes.status = ? AND nodes.proxy_url = ?
ORDER BY slots.slot_id ASC
LIMIT 1`, statusHealthy, statusHealthy, auth.ProxyURL).Scan(&slotID, &nodeID)
	if err == sql.ErrNoRows {
		binding := buildVerifiedAuthBinding(0, 0, auth, auth.ProxyURL)
		binding.SyncStatus = "observed"
		return binding, nil
	}
	if err != nil {
		return authBinding{}, err
	}
	return buildVerifiedAuthBinding(slotID, nodeID, auth, auth.ProxyURL), nil
}

func buildVerifiedAuthBinding(slotID, nodeID int64, auth authFile, proxyURL string) authBinding {
	now := time.Now().UnixMilli()
	return authBinding{
		SlotID:       slotID,
		NodeID:       nodeID,
		AuthName:     auth.Name,
		AuthIndex:    auth.Index,
		AuthIdentity: auth.Identity(),
		ProxyURL:     proxyURL,
		SyncStatus:   "verified",
		VerifiedAt:   now,
		UpdatedAt:    now,
	}
}

func rollbackAuthDistribution(updates []authDistributionUpdate) error {
	for updateIndex := len(updates) - 1; updateIndex >= 0; updateIndex-- {
		update := updates[updateIndex]
		rollbackAuth := update.auth
		rollbackAuth.Raw = cloneAuthJSON(update.originalRaw)
		if err := saveAuthFileDirect(rollbackAuth); err != nil {
			return fmt.Errorf("restore xAI auth %s: %w", rollbackAuth.Name, err)
		}
	}
	return nil
}

func cloneAuthJSON(value map[string]any) map[string]any {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	cloned := make(map[string]any)
	if err := json.Unmarshal(raw, &cloned); err != nil {
		panic(err)
	}
	return cloned
}

func (store *ipStore) healthySlotProxyURL(slot slotRecord) (string, error) {
	if slot.NodeID == 0 {
		return "", nil
	}
	var proxyURL string
	if err := store.database.QueryRow(`SELECT proxy_url FROM ip_nodes WHERE id = ? AND status = ?`, slot.NodeID, statusHealthy).Scan(&proxyURL); err != nil {
		return "", fmt.Errorf("read healthy slot %d node: %w", slot.ID, err)
	}
	return proxyURL, nil
}
