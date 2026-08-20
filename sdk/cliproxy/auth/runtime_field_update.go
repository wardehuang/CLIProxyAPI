package auth

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// IsProxyOrPriorityOnlyChange reports whether incoming differs from existing only in
// proxy_url and/or priority (including mirrored metadata keys).
// Runtime cooldown/model state and timestamps are ignored so file-watcher reloads of
// distribution/priority patches can take a lightweight path.
func IsProxyOrPriorityOnlyChange(existing, incoming *Auth) bool {
	if existing == nil || incoming == nil {
		return false
	}
	if strings.TrimSpace(existing.ID) == "" || existing.ID != incoming.ID {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(existing.Provider), strings.TrimSpace(incoming.Provider)) {
		return false
	}

	left := cloneForRuntimeFieldCompare(existing)
	right := cloneForRuntimeFieldCompare(incoming)

	proxyChanged := strings.TrimSpace(left.ProxyURL) != strings.TrimSpace(right.ProxyURL)
	priorityChanged := authPriorityAttribute(left) != authPriorityAttribute(right) ||
		metadataPriorityString(left.Metadata) != metadataPriorityString(right.Metadata)
	if !proxyChanged && !priorityChanged {
		return false
	}

	neutralizeProxyAndPriority(left)
	neutralizeProxyAndPriority(right)
	return reflect.DeepEqual(left, right)
}

// ApplyProxyAndPriorityUpdate copies proxy_url and priority from incoming onto the
// existing runtime auth, refreshes the scheduler entry, and skips model registration
// and auth-file persistence.
func (m *Manager) ApplyProxyAndPriorityUpdate(ctx context.Context, incoming *Auth) (*Auth, error) {
	if m == nil {
		return nil, fmt.Errorf("auth manager is nil")
	}
	if incoming == nil || strings.TrimSpace(incoming.ID) == "" {
		return nil, fmt.Errorf("auth ID is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	authID := strings.TrimSpace(incoming.ID)
	m.mu.Lock()
	existing := m.auths[authID]
	if existing == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("auth %s was not found", authID)
	}

	current := existing.Clone()
	proxyChanged := strings.TrimSpace(current.ProxyURL) != strings.TrimSpace(incoming.ProxyURL)
	current.ProxyURL = strings.TrimSpace(incoming.ProxyURL)

	if current.Attributes == nil {
		current.Attributes = make(map[string]string)
	}
	if incoming.Attributes != nil {
		if priority, ok := incoming.Attributes["priority"]; ok {
			current.Attributes["priority"] = priority
		} else {
			delete(current.Attributes, "priority")
		}
	} else {
		delete(current.Attributes, "priority")
	}

	if current.Metadata == nil {
		current.Metadata = make(map[string]any)
	}
	syncProxyPriorityMetadata(current.Metadata, incoming.Metadata, current.ProxyURL, current.Attributes["priority"])
	current.UpdatedAt = time.Now()

	// Preserve live counters already present on existing.
	current.Success = existing.Success
	current.Failed = existing.Failed
	current.recentRequests = existing.recentRequests

	stored := current.Clone()
	m.auths[authID] = stored
	m.mu.Unlock()

	if m.scheduler != nil {
		m.scheduler.upsertAuth(stored)
	}
	if proxyChanged {
		m.invalidateSessionAffinity(authID)
	}
	m.hook.OnAuthUpdated(ctx, stored.Clone())
	return stored.Clone(), nil
}

func cloneForRuntimeFieldCompare(auth *Auth) *Auth {
	clone := auth.Clone()
	clone.CreatedAt = time.Time{}
	clone.UpdatedAt = time.Time{}
	clone.LastRefreshedAt = time.Time{}
	clone.NextRefreshAfter = time.Time{}
	clone.NextRetryAfter = time.Time{}
	clone.Runtime = nil
	clone.Success = 0
	clone.Failed = 0
	clone.recentRequests = recentRequestRing{}
	clone.ModelStates = nil
	clone.Quota = QuotaState{}
	clone.Unavailable = false
	clone.LastError = nil
	clone.StatusMessage = ""
	clone.Index = ""
	clone.indexAssigned = false
	clone.Storage = nil
	clone.FileName = ""
	// File reloads always synthesize Active/Disabled from the disabled flag.
	if clone.Disabled {
		clone.Status = StatusDisabled
	} else {
		clone.Status = StatusActive
	}
	return clone
}

func neutralizeProxyAndPriority(auth *Auth) {
	if auth == nil {
		return
	}
	auth.ProxyURL = ""
	if auth.Attributes != nil {
		delete(auth.Attributes, "priority")
	}
	if auth.Metadata != nil {
		delete(auth.Metadata, "proxy_url")
		delete(auth.Metadata, "proxyUrl")
		delete(auth.Metadata, "proxy-url")
		delete(auth.Metadata, "priority")
	}
}

func authPriorityAttribute(auth *Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["priority"])
}

func metadataPriorityString(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	raw, ok := metadata["priority"]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		if value != float64(int(value)) {
			return fmt.Sprintf("%v", value)
		}
		return strconv.Itoa(int(value))
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	default:
		return fmt.Sprintf("%v", value)
	}
}

func syncProxyPriorityMetadata(dst, src map[string]any, proxyURL, priorityAttr string) {
	if dst == nil {
		return
	}
	if strings.TrimSpace(proxyURL) == "" {
		delete(dst, "proxy_url")
		delete(dst, "proxyUrl")
		delete(dst, "proxy-url")
	} else {
		dst["proxy_url"] = proxyURL
		// Drop alternate keys so runtime metadata matches file synthesizer shape.
		delete(dst, "proxyUrl")
		delete(dst, "proxy-url")
	}

	if priorityAttr == "" {
		// Prefer explicit absence from incoming metadata when attribute is cleared.
		if src == nil {
			delete(dst, "priority")
			return
		}
		if _, exists := src["priority"]; !exists {
			delete(dst, "priority")
			return
		}
	}
	if src != nil {
		if raw, ok := src["priority"]; ok {
			dst["priority"] = raw
			return
		}
	}
	if priorityAttr != "" {
		if priority, err := strconv.Atoi(priorityAttr); err == nil {
			dst["priority"] = priority
			return
		}
		dst["priority"] = priorityAttr
	}
}
