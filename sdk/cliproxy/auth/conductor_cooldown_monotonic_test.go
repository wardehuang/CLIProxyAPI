package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// Non-429 failure writes must never shorten a still-live cooldown; they may only
// extend it (#5501). A 429 resets its rate-limit window to the fixed duration.

func newCooldownMonotonicManager(t *testing.T, models ...string) (*Manager, *Auth) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-monotonic-" + models[0], Provider: "claude"}
	reg := registry.GetGlobalRegistry()
	infos := make([]*registry.ModelInfo, 0, len(models))
	now := time.Now().Unix()
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model, Created: now})
	}
	reg.RegisterClient(auth.ID, auth.Provider, infos)
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	return m, auth
}

func TestManager_MarkResult_CredentialScopeKeepsLongerSiblingDeadline(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	m, auth := newCooldownMonotonicManager(t, "model-a", "model-b")

	// Model B has a longer per-model deadline from a 404.
	m.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: auth.Provider, Model: "model-b",
		Success: false, Error: &Error{HTTPStatus: http.StatusNotFound, Message: "model not found"},
	})
	before := time.Now()
	sibling, _ := m.GetByID(auth.ID)
	bState := existingModelState(sibling, canonicalModelKey("model-b"))
	if bState == nil || bState.NextRetryAfter.Before(before.Add(11*time.Hour)) {
		t.Fatalf("precondition failed: model-b long deadline missing: %+v", bState)
	}

	// Credential-scoped 429 uses the fixed 30-minute cooldown without shortening model B.
	retryAfter := 5 * time.Minute
	m.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: auth.Provider, Model: "model-a",
		Success: false, RetryAfter: &retryAfter, CredentialScope: true,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential 429"},
	})

	updated, _ := m.GetByID(auth.ID)
	bStateAfter := existingModelState(updated, canonicalModelKey("model-b"))
	if bStateAfter == nil {
		t.Fatal("model-b state missing after sibling failure")
	}
	if bStateAfter.NextRetryAfter.Before(before.Add(11 * time.Hour)) {
		t.Fatalf("credential-scoped failure shortened model-b deadline to %v", bStateAfter.NextRetryAfter.Sub(before))
	}

	blockedA, _, _ := isAuthBlockedForModel(updated, "model-a", before.Add(31*time.Minute))
	if blockedA {
		t.Fatal("model-a remained blocked beyond the fixed 30-minute credential cooldown")
	}
	blockedB, _, _ := isAuthBlockedForModel(updated, "model-b", before.Add(31*time.Minute))
	if !blockedB {
		t.Fatal("model-b should remain blocked by its longer per-model deadline")
	}
}

// Ensure that a sibling model's long non-quota deadline (such as a 12h 404) is NOT
// promoted into its Quota.NextRecoverAt, which would cause subsequent credential-scoped
// writes on that sibling to elevate other models to 12h.
func TestManager_MarkResult_CredentialScope_DoesNotPromoteSiblingDeadlineToQuota(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	m, auth := newCooldownMonotonicManager(t, "model-a", "model-b")

	// 1. Model B gets a 404 (12h deadline).
	m.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: auth.Provider, Model: "model-b",
		Success: false, Error: &Error{HTTPStatus: http.StatusNotFound, Message: "model not found"},
	})
	before := time.Now()

	// 2. Model A gets a credential-scoped 429 (fixed 30-minute deadline).
	retryAfter := 5 * time.Minute
	m.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: auth.Provider, Model: "model-a",
		Success: false, RetryAfter: &retryAfter, CredentialScope: true,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential 429"},
	})

	snap1, _ := m.GetByID(auth.ID)
	bState1 := existingModelState(snap1, canonicalModelKey("model-b"))
	if bState1 == nil {
		t.Fatal("model-b state missing")
	}
	// Model B's NextRetryAfter should still be ~12h.
	if bState1.NextRetryAfter.Before(before.Add(11 * time.Hour)) {
		t.Fatalf("model-b NextRetryAfter shortened: %v", bState1.NextRetryAfter.Sub(before))
	}
	if bState1.Quota.NextRecoverAt.Before(before.Add(29*time.Minute)) || bState1.Quota.NextRecoverAt.After(before.Add(31*time.Minute)) {
		t.Fatalf("model-b Quota.NextRecoverAt should be the fixed 30-minute deadline: %v", bState1.Quota.NextRecoverAt.Sub(before))
	}

	// 3. A subsequent in-flight request on Model B also returns credential-scoped 429.
	m.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: auth.Provider, Model: "model-b", CredentialScope: true,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota exceeded again"},
	})

	secondFailureAt := time.Now()
	snap2, _ := m.GetByID(auth.ID)
	aState2 := existingModelState(snap2, canonicalModelKey("model-a"))
	if aState2 == nil {
		t.Fatal("model-a state missing")
	}
	if aState2.NextRetryAfter.Before(secondFailureAt.Add(29*time.Minute)) || aState2.NextRetryAfter.After(secondFailureAt.Add(31*time.Minute)) {
		t.Fatalf("model-a did not receive the fixed 30-minute credential cooldown: %v", aState2.NextRetryAfter.Sub(secondFailureAt))
	}
	if snap2.Quota.NextRecoverAt.Before(secondFailureAt.Add(29*time.Minute)) || snap2.Quota.NextRecoverAt.After(secondFailureAt.Add(31*time.Minute)) {
		t.Fatalf("auth quota did not receive the fixed 30-minute credential cooldown: %v", snap2.Quota.NextRecoverAt.Sub(secondFailureAt))
	}

	blockedA, _, _ := isAuthBlockedForModel(snap2, "model-a", secondFailureAt.Add(31*time.Minute))
	if blockedA {
		t.Fatal("model-a should unblock after the fixed 30-minute credential cooldown")
	}
	blockedB, _, _ := isAuthBlockedForModel(snap2, "model-b", secondFailureAt.Add(31*time.Minute))
	if !blockedB {
		t.Fatal("model-b should remain blocked by its independent 12-hour deadline")
	}
}

func TestManager_MarkResult_LaterShorterFailureKeepsLongerModelDeadline(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	tests := []struct {
		name   string
		second func(m *Manager, authID string)
	}{
		{
			name: "401_then_short_429",
			second: func(m *Manager, authID string) {
				short := 2 * time.Minute
				m.MarkResult(context.Background(), Result{
					AuthID: authID, Provider: "claude", Model: "model-a",
					Success: false, RetryAfter: &short,
					Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "short 429"},
				})
			},
		},
		{
			name: "401_then_transient_500",
			second: func(m *Manager, authID string) {
				m.MarkResult(context.Background(), Result{
					AuthID: authID, Provider: "claude", Model: "model-a",
					Success: false, Error: &Error{HTTPStatus: http.StatusInternalServerError, Message: "transient 500"},
				})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, auth := newCooldownMonotonicManager(t, "model-a")
			m.MarkResult(context.Background(), Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "model-a",
				Success: false, Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "long 401"},
			})
			before := time.Now()
			snap, _ := m.GetByID(auth.ID)
			state := existingModelState(snap, canonicalModelKey("model-a"))
			if state == nil || state.NextRetryAfter.Before(before.Add(25*time.Minute)) {
				t.Fatalf("precondition failed: 401 deadline missing: %+v", state)
			}

			tc.second(m, auth.ID)

			updated, _ := m.GetByID(auth.ID)
			stateAfter := existingModelState(updated, canonicalModelKey("model-a"))
			if stateAfter == nil {
				t.Fatal("model state missing after second writer")
			}
			if stateAfter.NextRetryAfter.Before(before.Add(25 * time.Minute)) {
				t.Fatalf("second writer shortened live deadline to %v", stateAfter.NextRetryAfter.Sub(before))
			}
		})
	}
}

// The client-facing projection must agree with scheduling: a live credential-wide
// cooldown (auth.Unavailable + auth.NextRetryAfter) suspends models that carry no
// per-model state (#5501).
func TestManager_ClientModelProjection_ReflectsAuthLevelCooldown(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	m, auth := newCooldownMonotonicManager(t, "model-a")

	m.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: auth.Provider, Model: "",
		Success: false, Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "credential-wide 401"},
	})

	snap, _ := m.GetByID(auth.ID)
	if !snap.Unavailable || snap.NextRetryAfter.Before(time.Now().Add(25*time.Minute)) {
		t.Fatalf("precondition failed: expected live auth-level cooldown, got Unavailable=%v NextRetryAfter=%v", snap.Unavailable, snap.NextRetryAfter)
	}

	blocked, _, _ := isAuthBlockedForModel(snap, "model-a", time.Now())
	projection := m.clientModelProjectionForAuth(snap, "model-a", time.Now())
	if blocked && !projection.Suspended {
		t.Fatalf("scheduling blocks the model while projection reports Suspended=false: %+v", projection)
	}
}

func TestManager_ApplyAuthFailureState_PreservesLongerCredentialDeadline(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	tests := []struct {
		name   string
		second *Error
	}{
		{
			name:   "404_then_invalid_grant",
			second: &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_grant"},
		},
		{
			name:   "404_then_cloudflare",
			second: &Error{HTTPStatus: http.StatusForbidden, Message: "just a moment... cloudflare challenge"},
		},
		{
			name:   "404_then_transient_500",
			second: &Error{HTTPStatus: http.StatusInternalServerError, Message: "internal server error"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, auth := newCooldownMonotonicManager(t, "model-a")
			// Credential-wide 404 (12h deadline).
			m.MarkResult(context.Background(), Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "",
				Success: false, Error: &Error{HTTPStatus: http.StatusNotFound, Message: "credential not found"},
			})
			before := time.Now()
			snap, _ := m.GetByID(auth.ID)
			if !snap.Unavailable || snap.NextRetryAfter.Before(before.Add(11*time.Hour)) {
				t.Fatalf("precondition failed: expected ~12h deadline, got: %v", snap.NextRetryAfter.Sub(before))
			}

			// Follow up with a shorter credential failure.
			m.MarkResult(context.Background(), Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "",
				Success: false, Error: tc.second,
			})

			updated, _ := m.GetByID(auth.ID)
			if updated.NextRetryAfter.Before(before.Add(11 * time.Hour)) {
				t.Fatalf("shorter failure shortened credential-level deadline to %v", updated.NextRetryAfter.Sub(before))
			}
		})
	}
}

func TestManager_MarkResult_CredentialScopeDoesNotInheritModelQuotaDeadline(t *testing.T) {
	withQuotaCooldownEnabled(t)

	testCases := []struct {
		name            string
		credentialModel string
	}{
		{name: "cross_model", credentialModel: "claude-sonnet-5"},
		{name: "same_model", credentialModel: "claude-fable-5-1"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			m, auth := newCooldownMonotonicManager(t, "claude-fable-5-1", "claude-sonnet-5", "claude-opus-5-5", "claude-sonnet-4")
			ctx := context.Background()
			for _, model := range []string{"claude-fable-5-1", "claude-sonnet-5", "claude-opus-5-5"} {
				m.MarkResult(ctx, Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Success: true})
			}

			firstFailureAt := time.Now()
			modelOnlyRetry := 8 * 24 * time.Hour
			m.MarkResult(ctx, Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "claude-fable-5-1",
				RetryAfter: &modelOnlyRetry,
				Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "Usage credits are required for this model."},
			})

			modelOnlyAuth, _ := m.GetByID(auth.ID)
			if modelOnlyAuth.Quota.Reason != "quota" ||
				modelOnlyAuth.Quota.NextRecoverAt.Before(firstFailureAt.Add(29*time.Minute)) ||
				modelOnlyAuth.Quota.NextRecoverAt.After(firstFailureAt.Add(31*time.Minute)) {
				t.Fatalf("model-only quota did not use fixed 30-minute cooldown: quota=%+v", modelOnlyAuth.Quota)
			}
			if blocked, _, _ := isAuthBlockedForModel(modelOnlyAuth, "claude-opus-5-5", time.Now()); blocked {
				t.Fatal("model-only cooldown should not block an unrelated model")
			}

			credentialRetry := 3 * time.Hour
			credentialFailureAt := time.Now()
			m.MarkResult(ctx, Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: testCase.credentialModel,
				RetryAfter: &credentialRetry, CredentialScope: true,
				Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "shared window rejected"},
			})

			updated, _ := m.GetByID(auth.ID)
			if updated.Quota.Reason != "credential_quota" ||
				updated.Quota.NextRecoverAt.Before(credentialFailureAt.Add(29*time.Minute)) ||
				updated.Quota.NextRecoverAt.After(credentialFailureAt.Add(31*time.Minute)) {
				t.Fatalf("credential cooldown inherited a model-only deadline or Retry-After: reason=%q deadline=%v (in %v)",
					updated.Quota.Reason, updated.Quota.NextRecoverAt, updated.Quota.NextRecoverAt.Sub(credentialFailureAt))
			}

			opusBlocked, _, opusNext := isAuthBlockedForModel(updated, "claude-opus-5-5", credentialFailureAt.Add(31*time.Minute))
			if opusBlocked {
				t.Fatalf("opus remained blocked beyond credential cooldown: next=%v (in %v)", opusNext, opusNext.Sub(credentialFailureAt))
			}
			fableBlocked, _, fableNext := isAuthBlockedForModel(updated, "claude-fable-5-1", credentialFailureAt.Add(31*time.Minute))
			if fableBlocked {
				t.Fatalf("model-only cooldown was not limited to 30 minutes: next=%v", fableNext)
			}

			secondRetryHint := 30 * time.Minute
			secondFailureAt := time.Now()
			m.MarkResult(ctx, Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "claude-sonnet-4",
				RetryAfter: &secondRetryHint, CredentialScope: true,
				Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "another shared window rejection"},
			})
			updated, _ = m.GetByID(auth.ID)
			if updated.Quota.NextRecoverAt.Before(secondFailureAt.Add(29*time.Minute)) || updated.Quota.NextRecoverAt.After(secondFailureAt.Add(31*time.Minute)) {
				t.Fatalf("later credential-scoped 429 did not reset to 30 minutes: deadline=%v", updated.Quota.NextRecoverAt)
			}
		})
	}
}

func TestManager_MarkResult_CredentialScopeResetsExpiredBackoffToFixedCooldown(t *testing.T) {
	withQuotaCooldownEnabled(t)

	m, auth := newCooldownMonotonicManager(t, "model-a", "model-b")
	ctx := context.Background()
	m.MarkResult(ctx, Result{
		AuthID: auth.ID, Provider: auth.Provider, Model: "model-a", CredentialScope: true,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota exceeded"},
	})

	updated, _ := m.GetByID(auth.ID)
	if updated.Quota.BackoffLevel != 0 {
		t.Fatalf("fixed cooldown must not retain a backoff level: quota=%+v", updated.Quota)
	}

	expired := time.Now().Add(-time.Minute)
	m.mu.Lock()
	storedAuth := m.auths[auth.ID]
	storedAuth.Quota.NextRecoverAt = expired
	storedAuth.NextRetryAfter = expired
	for _, state := range storedAuth.ModelStates {
		if state != nil {
			state.Quota.NextRecoverAt = expired
			state.NextRetryAfter = expired
		}
	}
	m.mu.Unlock()

	secondFailureAt := time.Now()
	m.MarkResult(ctx, Result{
		AuthID: auth.ID, Provider: auth.Provider, Model: "model-b", CredentialScope: true,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota exceeded again"},
	})

	updated, _ = m.GetByID(auth.ID)
	if updated.Quota.BackoffLevel != 0 {
		t.Fatalf("second 429 changed the fixed cooldown backoff level: quota=%+v", updated.Quota)
	}
	if updated.Quota.NextRecoverAt.Before(secondFailureAt.Add(29*time.Minute)) || updated.Quota.NextRecoverAt.After(secondFailureAt.Add(31*time.Minute)) {
		t.Fatalf("second 429 did not use the fixed 30-minute cooldown: deadline=%v", updated.Quota.NextRecoverAt)
	}
}

func TestManager_MarkResult_CredentialScopeUsesFixedCooldown(t *testing.T) {
	withQuotaCooldownEnabled(t)

	retryAfter := 10 * time.Second
	longRetryAfter := 3 * time.Hour
	tests := []struct {
		name       string
		retryAfter *time.Duration
	}{
		{name: "without_retry_hint"},
		{name: "with_short_retry_hint", retryAfter: &retryAfter},
		{name: "with_long_retry_hint", retryAfter: &longRetryAfter},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			m, auth := newCooldownMonotonicManager(t, "model-a", "model-b")
			ctx := context.Background()
			m.MarkResult(ctx, Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "model-a",
				Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "model-only quota exceeded"},
			})

			modelOnlyAuth, _ := m.GetByID(auth.ID)
			modelState := existingModelState(modelOnlyAuth, canonicalModelKey("model-a"))
			if modelOnlyAuth.Quota.Reason != "quota" || modelState == nil || modelState.Quota.BackoffLevel != 0 {
				t.Fatalf("precondition failed: model-only fixed quota cooldown missing: auth=%+v state=%+v", modelOnlyAuth.Quota, modelState)
			}

			secondFailureAt := time.Now()
			m.MarkResult(ctx, Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "model-a", RetryAfter: testCase.retryAfter,
				CredentialScope: true,
				Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota exceeded"},
			})

			updated, _ := m.GetByID(auth.ID)
			if updated.Quota.BackoffLevel != 0 {
				t.Fatalf("credential cooldown changed fixed backoff level: got %d", updated.Quota.BackoffLevel)
			}
			if updated.Quota.NextRecoverAt.Before(secondFailureAt.Add(29*time.Minute)) || updated.Quota.NextRecoverAt.After(secondFailureAt.Add(31*time.Minute)) {
				t.Fatalf("credential cooldown ignored fixed 30-minute duration: deadline=%v", updated.Quota.NextRecoverAt)
			}
		})
	}
}
