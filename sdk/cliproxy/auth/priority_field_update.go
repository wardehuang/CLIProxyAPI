package auth

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// BEGIN xAI Guardian core extension: priority-only auth metadata update.
// UpdateAuthPriority changes only the priority field for one persisted auth credential.
func (m *Manager) UpdateAuthPriority(ctx context.Context, authIndex string, priority int) (*Auth, error) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return nil, fmt.Errorf("auth priority update: auth_index is required")
	}
	if m.store == nil {
		return nil, fmt.Errorf("auth priority update: auth store unavailable")
	}
	m.mu.Lock()
	var current *Auth
	for _, candidate := range m.auths {
		if candidate == nil {
			continue
		}
		candidate.EnsureIndex()
		if candidate.Index != authIndex {
			continue
		}
		if current != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("auth priority update: auth_index is ambiguous")
		}
		current = candidate
	}
	if current == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("auth priority update: auth_index not found")
	}
	if IsConfigAPIKeyAuth(current) || IsPluginVirtualAuth(current) || strings.EqualFold(strings.TrimSpace(current.Attributes["runtime_only"]), "true") {
		m.mu.Unlock()
		return nil, fmt.Errorf("auth priority update: auth is not a physical file credential")
	}
	switch current.AuthSourceKind() {
	case AuthSourceFile, AuthSourceGit, AuthSourceObjectStore, AuthSourcePostgres:
	default:
		m.mu.Unlock()
		return nil, fmt.Errorf("auth priority update: auth is not persisted")
	}
	if authAttribute(current, AttributePath) == "" && strings.TrimSpace(current.FileName) == "" {
		m.mu.Unlock()
		return nil, fmt.Errorf("auth priority update: auth path is unavailable")
	}
	updated := current.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["priority"] = float64(priority)
	ApplyAuthPriorityMetadata(updated, updated.Metadata)
	updated.Generation = current.Generation + 1
	updated.UpdatedAt = time.Now()

	lockValue, _ := m.persistLocks.LoadOrStore(current.ID, &authPersistLock{})
	persistLock := lockValue.(*authPersistLock)
	persistLock.mu.Lock()
	if current.RegistrationEpoch < persistLock.lastEpoch || (current.RegistrationEpoch == persistLock.lastEpoch && current.Generation < persistLock.lastGeneration) {
		persistLock.mu.Unlock()
		m.mu.Unlock()
		return nil, fmt.Errorf("auth priority update: stale auth generation")
	}
	updater, ok := m.store.(interface {
		UpdatePriorityOnly(context.Context, *Auth, int) error
	})
	if !ok {
		persistLock.mu.Unlock()
		m.mu.Unlock()
		return nil, fmt.Errorf("auth priority update: store does not support priority-only persistence")
	}
	if persistErr := updater.UpdatePriorityOnly(ctx, updated, priority); persistErr != nil {
		persistLock.mu.Unlock()
		m.mu.Unlock()
		return nil, fmt.Errorf("auth priority update: persist priority: %w", persistErr)
	}

	m.auths[current.ID] = updated.Clone()
	persistLock.lastEpoch = updated.RegistrationEpoch
	persistLock.lastGeneration = updated.Generation
	m.mu.Unlock()

	if m.scheduler != nil {
		m.scheduler.upsertAuth(updated.Clone())
	}
	m.structuralEpoch.Add(1)
	persistLock.mu.Unlock()
	return updated.Clone(), nil
}

// END xAI Guardian core extension.
