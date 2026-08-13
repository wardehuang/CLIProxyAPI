package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// AuthFilePath returns the physical JSON path associated with a file-backed auth.
func AuthFilePath(auth *Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["path"])
}

// ReadAuthProxyURLFromFile reads the current proxy_url from the auth JSON file.
func ReadAuthProxyURLFromFile(auth *Auth) (string, error) {
	path := AuthFilePath(auth)
	if path == "" {
		return "", fmt.Errorf("auth file path is unavailable")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read auth file: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("decode auth file: %w", err)
	}
	proxyURL, ok := payload["proxy_url"].(string)
	if !ok {
		return "", nil
	}
	return strings.TrimSpace(proxyURL), nil
}

// ReloadAuthRuntimeFromFile refreshes proxy_url and priority before an externally controlled retry.
func (m *Manager) ReloadAuthRuntimeFromFile(ctx context.Context, authID string) (*Auth, error) {
	if m == nil {
		return nil, fmt.Errorf("auth manager is nil")
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, fmt.Errorf("auth ID is required")
	}
	current, ok := m.GetByID(authID)
	if !ok || current == nil {
		return nil, fmt.Errorf("auth %s was not found", authID)
	}
	path := AuthFilePath(current)
	if path == "" {
		return nil, fmt.Errorf("auth file path is unavailable")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read auth file: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode auth file: %w", err)
	}
	proxyURL, _ := payload["proxy_url"].(string)
	priority, priorityExists, err := authFilePriority(payload)
	if err != nil {
		return nil, err
	}
	if current.Attributes == nil {
		return nil, fmt.Errorf("auth %s attributes are unavailable", authID)
	}
	if priorityExists {
		current.Attributes["priority"] = strconv.Itoa(priority)
	} else {
		delete(current.Attributes, "priority")
	}
	current.ProxyURL = strings.TrimSpace(proxyURL)
	current.UpdatedAt = time.Now()
	m.mu.Lock()
	if existing := m.auths[authID]; existing != nil {
		current.Success = existing.Success
		current.Failed = existing.Failed
		current.recentRequests = existing.recentRequests
	}
	stored := current.Clone()
	m.auths[authID] = stored
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(stored)
	}
	m.invalidateSessionAffinity(authID)
	m.hook.OnAuthUpdated(ctx, stored.Clone())
	return stored.Clone(), nil
}

func authFilePriority(payload map[string]any) (int, bool, error) {
	rawPriority, exists := payload["priority"]
	if !exists || rawPriority == nil {
		return 0, false, nil
	}
	switch value := rawPriority.(type) {
	case float64:
		if value != float64(int(value)) {
			return 0, false, fmt.Errorf("auth priority must be an integer")
		}
		return int(value), true, nil
	case string:
		priority, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, false, fmt.Errorf("decode auth priority: %w", err)
		}
		return priority, true, nil
	default:
		return 0, false, fmt.Errorf("auth priority has unsupported type %T", rawPriority)
	}
}

// ReloadAuthProxyURLFromFile refreshes one runtime auth's proxy URL without rewriting its JSON file.
func (m *Manager) ReloadAuthProxyURLFromFile(ctx context.Context, authID string) (*Auth, error) {
	if m == nil {
		return nil, fmt.Errorf("auth manager is nil")
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, fmt.Errorf("auth ID is required")
	}
	current, ok := m.GetByID(authID)
	if !ok || current == nil {
		return nil, fmt.Errorf("auth %s was not found", authID)
	}
	proxyURL, err := ReadAuthProxyURLFromFile(current)
	if err != nil {
		return nil, err
	}
	current.ProxyURL = proxyURL
	current.UpdatedAt = time.Now()
	m.mu.Lock()
	if existing := m.auths[authID]; existing != nil {
		current.Success = existing.Success
		current.Failed = existing.Failed
		current.recentRequests = existing.recentRequests
	}
	stored := current.Clone()
	m.auths[authID] = stored
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(stored)
	}
	m.hook.OnAuthUpdated(ctx, stored.Clone())
	return stored.Clone(), nil
}
