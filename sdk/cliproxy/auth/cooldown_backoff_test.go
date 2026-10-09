package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func withQuotaCooldownEnabled(t *testing.T) {
	t.Helper()
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })
}

func quotaResult(authID, model string) Result {
	return Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Success:  false,
		Error: &Error{
			Code:       "rate_limit",
			Message:    "quota",
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
		},
	}
}

func TestMarkResultQuotaUsesFixedCooldown(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-quota-window",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	firstFailureAt := time.Now()
	manager.MarkResult(context.Background(), quotaResult(auth.ID, "gpt-5"))
	first, ok := manager.GetByID(auth.ID)
	if !ok || first == nil || first.ModelStates["gpt-5"] == nil {
		t.Fatalf("expected model state after first failure")
	}
	firstState := first.ModelStates["gpt-5"]
	if firstState.Quota.BackoffLevel != 0 {
		t.Fatalf("expected fixed cooldown backoff level 0, got %d", firstState.Quota.BackoffLevel)
	}
	if firstState.Quota.NextRecoverAt.Before(firstFailureAt.Add(29*time.Minute)) || firstState.Quota.NextRecoverAt.After(firstFailureAt.Add(31*time.Minute)) {
		t.Fatalf("expected fixed 30-minute cooldown, got %v", firstState.Quota.NextRecoverAt.Sub(firstFailureAt))
	}

	secondFailureAt := time.Now()
	manager.MarkResult(context.Background(), quotaResult(auth.ID, "gpt-5"))
	second, ok := manager.GetByID(auth.ID)
	if !ok || second == nil || second.ModelStates["gpt-5"] == nil {
		t.Fatalf("expected model state after second failure")
	}
	secondState := second.ModelStates["gpt-5"]
	if secondState.Quota.BackoffLevel != 0 {
		t.Fatalf("expected backoff level to stay 0, got %d", secondState.Quota.BackoffLevel)
	}
	if secondState.Quota.NextRecoverAt.Before(secondFailureAt.Add(29*time.Minute)) || secondState.Quota.NextRecoverAt.After(secondFailureAt.Add(31*time.Minute)) {
		t.Fatalf("second 429 did not reset to fixed 30-minute cooldown: %v", secondState.Quota.NextRecoverAt.Sub(secondFailureAt))
	}
	if !secondState.NextRetryAfter.Equal(secondState.Quota.NextRecoverAt) {
		t.Fatalf("NextRetryAfter = %v, want fixed quota deadline %v", secondState.NextRetryAfter, secondState.Quota.NextRecoverAt)
	}
}

func TestMarkResultQuotaReplacesLongerPriorCooldown(t *testing.T) {
	withQuotaCooldownEnabled(t)

	priorDeadline := time.Now().Add(7 * 24 * time.Hour)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-quota-prior-longer",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: priorDeadline,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: priorDeadline, BackoffLevel: 3},
			},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	failureAt := time.Now()
	manager.MarkResult(context.Background(), quotaResult(auth.ID, "gpt-5"))
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil || updated.ModelStates["gpt-5"] == nil {
		t.Fatalf("expected model state after failure")
	}
	state := updated.ModelStates["gpt-5"]
	if state.Quota.BackoffLevel != 0 {
		t.Fatalf("expected fixed cooldown backoff level 0, got %d", state.Quota.BackoffLevel)
	}
	if state.Quota.NextRecoverAt.Before(failureAt.Add(29*time.Minute)) || state.Quota.NextRecoverAt.After(failureAt.Add(31*time.Minute)) {
		t.Fatalf("prior long cooldown was not replaced by fixed 30-minute duration: %v", state.Quota.NextRecoverAt.Sub(failureAt))
	}
	if state.NextRetryAfter.Before(failureAt.Add(29*time.Minute)) || state.NextRetryAfter.After(failureAt.Add(31*time.Minute)) {
		t.Fatalf("prior long retry deadline was not replaced: %v", state.NextRetryAfter.Sub(failureAt))
	}
}

func TestCredentialScope429ResetsPriorRateLimitCooldowns(t *testing.T) {
	withQuotaCooldownEnabled(t)

	priorDeadline := time.Now().Add(7 * 24 * time.Hour)
	independentModelDeadline := time.Now().Add(12 * time.Hour)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:             "auth-credential-quota-fixed",
		Provider:       "codex",
		Metadata:       map[string]any{"type": "codex"},
		Unavailable:    true,
		NextRetryAfter: priorDeadline,
		Quota: QuotaState{
			Exceeded: true, Reason: "credential_quota", NextRecoverAt: priorDeadline, BackoffLevel: 4,
		},
		ModelStates: map[string]*ModelState{
			"model-a": {
				Status: StatusError, Unavailable: true, NextRetryAfter: priorDeadline,
				Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: priorDeadline, BackoffLevel: 4},
			},
			"model-b": {
				Status: StatusError, Unavailable: true, NextRetryAfter: priorDeadline,
				Quota: QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: priorDeadline, BackoffLevel: 4},
			},
			"model-c": {
				Status: StatusError, Unavailable: true, NextRetryAfter: independentModelDeadline,
			},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	failureAt := time.Now()
	retryAfter := 7 * 24 * time.Hour
	result := quotaResult(auth.ID, "model-a")
	result.CredentialScope = true
	result.RetryAfter = &retryAfter
	manager.MarkResult(context.Background(), result)

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth after credential-scoped 429")
	}
	if updated.Quota.NextRecoverAt.Before(failureAt.Add(29*time.Minute)) || updated.Quota.NextRecoverAt.After(failureAt.Add(31*time.Minute)) ||
		updated.NextRetryAfter.Before(failureAt.Add(29*time.Minute)) || updated.NextRetryAfter.After(failureAt.Add(31*time.Minute)) {
		t.Fatalf("credential cooldown did not reset to 30 minutes: quota=%v retry=%v", updated.Quota.NextRecoverAt, updated.NextRetryAfter)
	}
	for _, model := range []string{"model-a", "model-b"} {
		state := updated.ModelStates[model]
		if state.Quota.NextRecoverAt.Before(failureAt.Add(29*time.Minute)) || state.Quota.NextRecoverAt.After(failureAt.Add(31*time.Minute)) ||
			state.NextRetryAfter.Before(failureAt.Add(29*time.Minute)) || state.NextRetryAfter.After(failureAt.Add(31*time.Minute)) {
			t.Fatalf("model %s retained prior rate-limit deadline: quota=%v retry=%v", model, state.Quota.NextRecoverAt, state.NextRetryAfter)
		}
	}
	if updated.ModelStates["model-c"].NextRetryAfter.Before(failureAt.Add(11 * time.Hour)) {
		t.Fatalf("credential 429 shortened model-c's independent cooldown: %v", updated.ModelStates["model-c"].NextRetryAfter)
	}
}

func TestApplyAuthFailureStateQuotaUsesFixedCooldown(t *testing.T) {
	now := time.Now()
	quotaErr := &Error{Code: "rate_limit", Message: "quota", HTTPStatus: http.StatusTooManyRequests}
	auth := &Auth{ID: "auth-level-quota"}

	applyAuthFailureState(auth, quotaErr, nil, now, false)
	if auth.Quota.BackoffLevel != 0 {
		t.Fatalf("expected fixed cooldown backoff level 0, got %d", auth.Quota.BackoffLevel)
	}
	firstRecover := auth.Quota.NextRecoverAt
	if !firstRecover.Equal(now.Add(quotaRateLimitCooldown)) {
		t.Fatalf("expected first cooldown to close at %v, got %v", now.Add(quotaRateLimitCooldown), firstRecover)
	}

	// A later 429 resets the fixed duration from its own occurrence.
	secondFailureAt := now.Add(100 * time.Millisecond)
	applyAuthFailureState(auth, quotaErr, nil, secondFailureAt, false)
	if auth.Quota.BackoffLevel != 0 {
		t.Fatalf("expected backoff level 0 for repeated 429, got %d", auth.Quota.BackoffLevel)
	}
	if !auth.Quota.NextRecoverAt.Equal(secondFailureAt.Add(quotaRateLimitCooldown)) {
		t.Fatalf("expected repeated cooldown to close at %v, got %v", secondFailureAt.Add(quotaRateLimitCooldown), auth.Quota.NextRecoverAt)
	}

	// Retry-After does not override the fixed 30-minute duration.
	retryAfter := 10 * time.Second
	hintedFailureAt := now.Add(time.Minute)
	applyAuthFailureState(auth, quotaErr, &retryAfter, hintedFailureAt, false)
	if auth.Quota.BackoffLevel != 0 {
		t.Fatalf("expected backoff level 0 with retry hint, got %d", auth.Quota.BackoffLevel)
	}
	if !auth.Quota.NextRecoverAt.Equal(hintedFailureAt.Add(quotaRateLimitCooldown)) {
		t.Fatalf("expected fixed cooldown to close at %v, got %v", hintedFailureAt.Add(quotaRateLimitCooldown), auth.Quota.NextRecoverAt)
	}

	priorDeadline := now.Add(7 * 24 * time.Hour)
	auth.NextRetryAfter = priorDeadline
	auth.Quota.NextRecoverAt = priorDeadline
	longRetryAfter := 7 * 24 * time.Hour
	resetAt := now.Add(2 * time.Minute)
	applyAuthFailureState(auth, quotaErr, &longRetryAfter, resetAt, false)
	if !auth.Quota.NextRecoverAt.Equal(resetAt.Add(quotaRateLimitCooldown)) || !auth.NextRetryAfter.Equal(resetAt.Add(quotaRateLimitCooldown)) {
		t.Fatalf("existing cooldown or Retry-After overrode fixed 30-minute deadline: quota=%v retry=%v", auth.Quota.NextRecoverAt, auth.NextRetryAfter)
	}
}

func TestRecoverableUnknownFailuresHaveFiniteCooldown(t *testing.T) {
	withQuotaCooldownEnabled(t)
	previousTransient := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(0)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(previousTransient) })

	testCases := []struct {
		name      string
		model     string
		resultErr *Error
	}{
		{name: "model failure without error details", model: "gpt-5"},
		{name: "auth failure without status or transport signature", resultErr: &Error{Message: "upstream exploded"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth := &Auth{ID: "auth-unknown-" + testCase.name, Provider: "codex"}
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
				t.Fatalf("Register returned error: %v", errRegister)
			}

			manager.MarkResult(context.Background(), Result{
				AuthID:   auth.ID,
				Provider: auth.Provider,
				Model:    testCase.model,
				Success:  false,
				Error:    testCase.resultErr,
			})

			updated, ok := manager.GetByID(auth.ID)
			if !ok || updated == nil {
				t.Fatal("expected auth after failure")
			}
			var nextRetryAfter time.Time
			if testCase.model == "" {
				nextRetryAfter = updated.NextRetryAfter
			} else {
				state := updated.ModelStates[testCase.model]
				if state == nil {
					t.Fatalf("expected model state for %q", testCase.model)
				}
				nextRetryAfter = state.NextRetryAfter
			}
			if nextRetryAfter.IsZero() {
				t.Fatal("recoverable failure has no retry deadline")
			}
			if blocked, _, _ := isAuthBlockedForModel(updated, testCase.model, time.Now()); !blocked {
				t.Fatal("auth was not blocked during recoverable failure cooldown")
			}
			if blocked, _, _ := isAuthBlockedForModel(updated, testCase.model, nextRetryAfter.Add(time.Nanosecond)); blocked {
				t.Fatal("auth did not automatically recover after retry deadline")
			}
		})
	}
}

func TestSchedulerPromotesUnknownFailureAfterRetryDeadline(t *testing.T) {
	withQuotaCooldownEnabled(t)
	previousTransient := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(0)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(previousTransient) })

	const (
		provider = "gemini"
		model    = "scheduler-unknown-recovery-model"
		authID   = "scheduler-unknown-recovery-auth"
	)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: authID, Provider: provider}); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}
	if _, errPick := manager.scheduler.pickSingle(context.Background(), provider, model, cliproxyexecutor.Options{}, nil); errPick != nil {
		t.Fatalf("initial scheduler pick returned error: %v", errPick)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    model,
		Success:  false,
		Error:    &Error{Message: "transport closed"},
	})

	manager.scheduler.mu.Lock()
	defer manager.scheduler.mu.Unlock()
	providerScheduler := manager.scheduler.providers[provider]
	if providerScheduler == nil {
		t.Fatalf("scheduler provider %q is missing", provider)
	}
	shard := providerScheduler.modelShards[model]
	if shard == nil {
		t.Fatalf("scheduler model shard %q is missing", model)
	}
	entry := shard.entries[authID]
	if entry == nil {
		t.Fatalf("scheduler auth %q is missing", authID)
	}
	if entry.state != scheduledStateBlocked || entry.nextRetryAt.IsZero() {
		t.Fatalf("scheduler entry state = %v, retry = %v; want finite blocked state", entry.state, entry.nextRetryAt)
	}

	shard.promoteExpiredLocked(entry.nextRetryAt.Add(time.Nanosecond))
	if entry.state != scheduledStateReady {
		t.Fatalf("scheduler entry state after deadline = %v, want ready", entry.state)
	}
}

func TestJitteredCooldownWaitBounds(t *testing.T) {
	cases := []struct {
		wait      time.Duration
		maxWait   time.Duration
		maxJitter time.Duration
	}{
		{time.Second, 0, 250 * time.Millisecond},
		{8 * time.Second, 0, 2 * time.Second},
		{30 * time.Second, 0, 2 * time.Second},
		{time.Second, 30 * time.Second, 250 * time.Millisecond},
		{29 * time.Second, 30 * time.Second, time.Second},
	}
	for _, tc := range cases {
		for i := 0; i < 200; i++ {
			got := jitteredCooldownWait(tc.wait, tc.maxWait)
			if got < tc.wait || got >= tc.wait+tc.maxJitter {
				t.Fatalf("jitteredCooldownWait(%v, %v) = %v, want in [%v, %v)", tc.wait, tc.maxWait, got, tc.wait, tc.wait+tc.maxJitter)
			}
			if tc.maxWait > 0 && got > tc.maxWait {
				t.Fatalf("jitteredCooldownWait(%v, %v) = %v exceeds maxWait", tc.wait, tc.maxWait, got)
			}
		}
	}

	// maxWait is a hard ceiling: zero headroom disables jitter entirely.
	for i := 0; i < 50; i++ {
		if got := jitteredCooldownWait(30*time.Second, 30*time.Second); got != 30*time.Second {
			t.Fatalf("expected wait at maxWait to stay unjittered, got %v", got)
		}
	}

	if got := jitteredCooldownWait(0, time.Minute); got != 0 {
		t.Fatalf("expected zero wait to stay zero, got %v", got)
	}
	if got := jitteredCooldownWait(-time.Second, time.Minute); got != -time.Second {
		t.Fatalf("expected negative wait to pass through, got %v", got)
	}
	if got := jitteredCooldownWait(3, 0); got != 3 {
		t.Fatalf("expected sub-4ns wait to stay unchanged, got %v", got)
	}
}
