package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
