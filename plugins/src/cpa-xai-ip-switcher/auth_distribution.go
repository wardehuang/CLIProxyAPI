package main

import (
	"encoding/json"
	"fmt"
	"time"
)

type authDistributionUpdate struct {
	auth        authFile
	originalRaw map[string]any
	proxyURL    string
	binding     authBinding
}

func refreshHealthyAuthDistribution(store *ipStore, healthySlotCount int) error {
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
	for _, update := range updates {
		if verifyErr := verifyAuthProxyURLDirect(update.auth, update.proxyURL); verifyErr != nil {
			if rollbackErr := rollbackAuthDistribution(updates[:appliedCount]); rollbackErr != nil {
				return fmt.Errorf("verify xAI auth %s: %w; rollback failed: %v", update.auth.Name, verifyErr, rollbackErr)
			}
			return fmt.Errorf("verify xAI auth %s: %w", update.auth.Name, verifyErr)
		}
		bindings = append(bindings, update.binding)
	}
	if err := store.replaceAuthBindings(bindings); err != nil {
		if rollbackErr := rollbackAuthDistribution(updates[:appliedCount]); rollbackErr != nil {
			return fmt.Errorf("replace xAI auth bindings: %w; rollback failed: %v", err, rollbackErr)
		}
		return fmt.Errorf("replace xAI auth bindings: %w", err)
	}
	_ = store.appendLog(logLevelInfo, "auth_distribution.verified", 0, "", "xAI auth proxy_url 已直接写入文本并完整验证", fmt.Sprintf("auth=%d；健康槽位=%d；空槽写入空 proxy_url", len(bindings), healthySlotCount))
	return nil
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
