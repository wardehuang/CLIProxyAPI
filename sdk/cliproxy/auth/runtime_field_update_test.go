package auth

import (
	"context"
	"testing"
	"time"
)

func TestIsProxyOrPriorityOnlyChange_ProxyURL(t *testing.T) {
	t.Parallel()

	existing := sampleRuntimeAuth("auth-1", "http://old:1", "1")
	incoming := sampleRuntimeAuth("auth-1", "http://new:2", "1")
	if !IsProxyOrPriorityOnlyChange(existing, incoming) {
		t.Fatal("expected proxy_url-only change")
	}
}

func TestIsProxyOrPriorityOnlyChange_Priority(t *testing.T) {
	t.Parallel()

	existing := sampleRuntimeAuth("auth-1", "http://proxy:1", "1")
	incoming := sampleRuntimeAuth("auth-1", "http://proxy:1", "-8")
	if !IsProxyOrPriorityOnlyChange(existing, incoming) {
		t.Fatal("expected priority-only change")
	}
}

func TestIsProxyOrPriorityOnlyChange_RejectsTokenChange(t *testing.T) {
	t.Parallel()

	existing := sampleRuntimeAuth("auth-1", "http://proxy:1", "1")
	incoming := sampleRuntimeAuth("auth-1", "http://proxy:2", "1")
	incoming.Metadata["access_token"] = "rotated"
	if IsProxyOrPriorityOnlyChange(existing, incoming) {
		t.Fatal("token change must not be treated as runtime-only")
	}
}

func TestIsProxyOrPriorityOnlyChange_IgnoresCooldownRuntime(t *testing.T) {
	t.Parallel()

	existing := sampleRuntimeAuth("auth-1", "http://old:1", "1")
	existing.Unavailable = true
	existing.NextRetryAfter = time.Now().Add(time.Hour)
	existing.ModelStates = map[string]*ModelState{
		"grok-4.5": {Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)},
	}
	incoming := sampleRuntimeAuth("auth-1", "http://new:2", "1")
	if !IsProxyOrPriorityOnlyChange(existing, incoming) {
		t.Fatal("cooldown runtime state must not block proxy_url-only path")
	}
}

func TestApplyProxyAndPriorityUpdate(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	existing := sampleRuntimeAuth("auth-1", "http://old:1", "1")
	if _, err := manager.Register(WithSkipPersist(context.Background()), existing); err != nil {
		t.Fatalf("register: %v", err)
	}

	incoming := sampleRuntimeAuth("auth-1", "http://new:2", "-8")
	updated, err := manager.ApplyProxyAndPriorityUpdate(context.Background(), incoming)
	if err != nil {
		t.Fatalf("ApplyProxyAndPriorityUpdate: %v", err)
	}
	if updated.ProxyURL != "http://new:2" {
		t.Fatalf("proxy_url=%q", updated.ProxyURL)
	}
	if updated.Attributes["priority"] != "-8" {
		t.Fatalf("priority attr=%q", updated.Attributes["priority"])
	}

	got, ok := manager.GetByID("auth-1")
	if !ok || got.ProxyURL != "http://new:2" || got.Attributes["priority"] != "-8" {
		t.Fatalf("stored auth mismatch: %+v", got)
	}
}

func sampleRuntimeAuth(id, proxyURL, priority string) *Auth {
	auth := &Auth{
		ID:       id,
		Provider: "xai",
		Label:    "user@example.com",
		Status:   StatusActive,
		ProxyURL: proxyURL,
		Attributes: map[string]string{
			"path":     "/tmp/" + id + ".json",
			"priority": priority,
		},
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token",
			"proxy_url":    proxyURL,
			"priority":     priority,
		},
		CreatedAt: time.Unix(1, 0).UTC(),
		UpdatedAt: time.Unix(2, 0).UTC(),
	}
	return auth
}
