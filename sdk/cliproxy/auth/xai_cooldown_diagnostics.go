package auth

import (
	"context"
	"sort"
	"strings"
	"time"
)

// logXAIAvailabilityDecision emits the complete state used by legacy selection.
// Keep the diagnostic values in the message because the deployed log formatter
// omits structured fields other than request_id.
func logXAIAvailabilityDecision(ctx context.Context, phase string, auth *Auth, routeModel, checkModel string, blocked bool, reason blockReason, nextRetryAfter time.Time) {
	if auth == nil || !strings.EqualFold(auth.Provider, "xai") {
		return
	}
	state, stateKey := xaiModelStateForDiagnostic(auth, checkModel)
	logEntryWithRequestID(ctx).Infof(
		"XAI_COOLDOWN_TRACE phase=%s auth_id=%s route_model=%s check_model=%s blocked=%t block_reason=%s next_retry_after=%s auth_unavailable=%t auth_next_retry_after=%s model_state_key=%s model_state_present=%t model_state_status=%s model_state_unavailable=%t model_state_next_retry_after=%s model_state_quota_exceeded=%t model_state_quota_recover_at=%s model_state_keys=%s",
		phase,
		auth.ID,
		routeModel,
		checkModel,
		blocked,
		blockReasonName(reason),
		diagnosticTime(nextRetryAfter),
		auth.Unavailable,
		diagnosticTime(auth.NextRetryAfter),
		stateKey,
		state != nil,
		diagnosticModelStateStatus(state),
		diagnosticModelStateUnavailable(state),
		diagnosticModelStateRetryAfter(state),
		diagnosticModelStateQuotaExceeded(state),
		diagnosticModelStateQuotaRecoverAt(state),
		strings.Join(xaiModelStateKeys(auth), ","),
	)
}

func logXAISelectionDecision(ctx context.Context, phase string, auth *Auth, routeModel string, candidateCount, availableCount int, pluginHandled bool) {
	if auth == nil || !strings.EqualFold(auth.Provider, "xai") {
		return
	}
	checkModel := canonicalModelKey(routeModel)
	state, stateKey := xaiModelStateForDiagnostic(auth, checkModel)
	logEntryWithRequestID(ctx).Infof(
		"XAI_COOLDOWN_TRACE phase=%s selected_auth_id=%s route_model=%s check_model=%s candidate_count=%d available_count=%d plugin_handled=%t selected_model_state_key=%s selected_model_state_present=%t selected_model_state_unavailable=%t selected_model_state_next_retry_after=%s selected_model_state_quota_exceeded=%t",
		phase,
		auth.ID,
		routeModel,
		checkModel,
		candidateCount,
		availableCount,
		pluginHandled,
		stateKey,
		state != nil,
		diagnosticModelStateUnavailable(state),
		diagnosticModelStateRetryAfter(state),
		diagnosticModelStateQuotaExceeded(state),
	)
}

func xaiModelStateForDiagnostic(auth *Auth, model string) (*ModelState, string) {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil, ""
	}
	if state := auth.ModelStates[model]; state != nil {
		return state, model
	}
	canonicalModel := canonicalModelKey(model)
	if canonicalModel != "" && canonicalModel != model {
		if state := auth.ModelStates[canonicalModel]; state != nil {
			return state, canonicalModel
		}
	}
	return nil, ""
}

func xaiModelStateKeys(auth *Auth) []string {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	keys := make([]string, 0, len(auth.ModelStates))
	for key := range auth.ModelStates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func blockReasonName(reason blockReason) string {
	switch reason {
	case blockReasonCooldown:
		return "cooldown"
	case blockReasonDisabled:
		return "disabled"
	case blockReasonOther:
		return "other"
	default:
		return "none"
	}
}

func diagnosticTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func diagnosticModelStateStatus(state *ModelState) Status {
	if state == nil {
		return ""
	}
	return state.Status
}

func diagnosticModelStateUnavailable(state *ModelState) bool {
	return state != nil && state.Unavailable
}

func diagnosticModelStateRetryAfter(state *ModelState) string {
	if state == nil {
		return ""
	}
	return diagnosticTime(state.NextRetryAfter)
}

func diagnosticModelStateQuotaExceeded(state *ModelState) bool {
	return state != nil && state.Quota.Exceeded
}

func diagnosticModelStateQuotaRecoverAt(state *ModelState) string {
	if state == nil {
		return ""
	}
	return diagnosticTime(state.Quota.NextRecoverAt)
}
