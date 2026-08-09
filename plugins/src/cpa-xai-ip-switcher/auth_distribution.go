package main

import (
	"fmt"
	"strings"
	"time"
)

func refreshHealthyAuthDistribution(store *ipStore, healthySlotCount int) error {
	if healthySlotCount < 1 {
		return fmt.Errorf("healthy slot count must be positive")
	}
	authEntries, err := listXAIAuthEntries()
	if err != nil {
		return fmt.Errorf("list xAI auth files for distribution: %w", err)
	}
	currentAuthNames := make(map[string]struct{}, len(authEntries))
	for fileIndex, entry := range authEntries {
		authName := authEntryName(entry)
		authIndex := authEntryIdentity(entry)
		currentAuthNames[authName] = struct{}{}
		slotID := int64(fileIndex%healthySlotCount + 1)
		slot, found, err := store.findSlotByID(slotID)
		if err != nil {
			return err
		}
		if !found || slot.Kind != statusHealthy {
			return fmt.Errorf("healthy auth distribution slot %d is unavailable", slotID)
		}
		nodeID := slot.NodeID
		proxyURL := ""
		if nodeID > 0 {
			if err := store.database.QueryRow(`SELECT proxy_url FROM ip_nodes WHERE id = ? AND status = ?`, nodeID, statusHealthy).Scan(&proxyURL); err != nil {
				return fmt.Errorf("read healthy slot %d node: %w", slotID, err)
			}
		}
		binding := authBinding{
			SlotID:       slotID,
			NodeID:       nodeID,
			AuthName:     authName,
			AuthIndex:    authIndex,
			AuthIdentity: authName + "#" + authIndex,
			ProxyURL:     proxyURL,
			SyncStatus:   "pending",
			UpdatedAt:    time.Now().UnixMilli(),
		}
		auth, getErr := getAuthFileForEntry(entry)
		if getErr != nil {
			binding.SyncStatus = "failed"
			binding.SyncError = fmt.Sprintf("读取 auth 失败：%s", getErr.Error())
			if saveErr := store.upsertAuthBinding(binding); saveErr != nil {
				return saveErr
			}
			_ = store.appendLog(logLevelError, "auth_distribution.read_failed", nodeID, "", "读取 xAI auth 失败，已记录同步失败", fmt.Sprintf("auth=%s；auth_index=%s；slot=%d；%s", authName, authIndex, slotID, getErr.Error()))
			continue
		}
		binding.AuthIndex = auth.Index
		binding.AuthIdentity = auth.Identity()
		if err := setAuthProxyURL(auth, proxyURL); err != nil {
			binding.SyncStatus = "failed"
			binding.SyncError = err.Error()
			if saveErr := store.upsertAuthBinding(binding); saveErr != nil {
				return saveErr
			}
			_ = store.appendLog(logLevelError, "auth_distribution.save_failed", nodeID, "", "同步 xAI auth proxy_url 失败", fmt.Sprintf("auth=%s；slot=%d；%s", authName, slotID, err.Error()))
			continue
		}
		verified, err := getAuthFileForEntry(entry)
		if err != nil {
			binding.SyncStatus = "failed"
			binding.SyncError = fmt.Sprintf("保存后重新读取失败：%s", err.Error())
		} else if strings.TrimSpace(verified.ProxyURL) != proxyURL {
			binding.SyncStatus = "failed"
			binding.SyncError = fmt.Sprintf("保存后 proxy_url 校验不一致：期望 %q，实际 %q", proxyURL, verified.ProxyURL)
		} else {
			binding.SyncStatus = "verified"
			binding.VerifiedAt = time.Now().UnixMilli()
		}
		if err := store.upsertAuthBinding(binding); err != nil {
			return err
		}
		if binding.SyncStatus == "verified" {
			_ = store.appendLog(logLevelInfo, "auth_distribution.verified", nodeID, "", "xAI auth proxy_url 已验证同步", fmt.Sprintf("auth=%s；slot=%d；proxy=%s", authName, slotID, proxyURL))
		} else {
			_ = store.appendLog(logLevelError, "auth_distribution.verify_failed", nodeID, "", "xAI auth proxy_url 校验失败", fmt.Sprintf("auth=%s；slot=%d；%s", authName, slotID, binding.SyncError))
		}
	}
	if err := deleteStaleAuthBindings(store, currentAuthNames); err != nil {
		return err
	}
	return nil
}

func deleteStaleAuthBindings(store *ipStore, currentAuthNames map[string]struct{}) error {
	if len(currentAuthNames) == 0 {
		if _, err := store.database.Exec(`DELETE FROM ip_slot_auth_bindings`); err != nil {
			return fmt.Errorf("clear stale sqlite auth bindings: %w", err)
		}
		return nil
	}
	placeholders := make([]string, 0, len(currentAuthNames))
	arguments := make([]any, 0, len(currentAuthNames))
	for authName := range currentAuthNames {
		placeholders = append(placeholders, "?")
		arguments = append(arguments, authName)
	}
	query := `DELETE FROM ip_slot_auth_bindings WHERE auth_name NOT IN (` + strings.Join(placeholders, ",") + `)`
	if _, err := store.database.Exec(query, arguments...); err != nil {
		return fmt.Errorf("delete stale sqlite auth bindings: %w", err)
	}
	return nil
}
