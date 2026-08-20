package cliproxy

import (
	"sort"
	"strings"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const xaiWatcherTraceSampleRate uint64 = 64

var xaiWatcherTraceSeq atomic.Uint64

// xaiCooldownStateSummary provides watcher diagnostics without logging secrets
// from the auth metadata or token storage.
func xaiCooldownStateSummary(auth *coreauth.Auth) string {
	if auth == nil {
		return "auth_missing"
	}
	modelKeys := make([]string, 0, len(auth.ModelStates))
	for modelKey := range auth.ModelStates {
		modelKeys = append(modelKeys, modelKey)
	}
	sort.Strings(modelKeys)
	modelSummaries := make([]string, 0, len(modelKeys))
	for _, modelKey := range modelKeys {
		state := auth.ModelStates[modelKey]
		if state == nil {
			modelSummaries = append(modelSummaries, modelKey+":nil")
			continue
		}
		modelSummaries = append(modelSummaries, strings.Join([]string{
			modelKey,
			"status=" + string(state.Status),
			"unavailable=" + boolString(state.Unavailable),
			"next_retry_after=" + cooldownDiagnosticTime(state.NextRetryAfter),
			"quota_exceeded=" + boolString(state.Quota.Exceeded),
			"quota_recover_at=" + cooldownDiagnosticTime(state.Quota.NextRecoverAt),
		}, ";"))
	}
	return strings.Join([]string{
		"auth_unavailable=" + boolString(auth.Unavailable),
		"auth_next_retry_after=" + cooldownDiagnosticTime(auth.NextRetryAfter),
		"model_states=[" + strings.Join(modelSummaries, "|") + "]",
	}, " ")
}

func cooldownDiagnosticTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func logXAIWatcherTrace(sample bool, format string, args ...any) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	if !sample {
		log.Debugf(format, args...)
		return
	}
	n := xaiWatcherTraceSeq.Add(1)
	if n%xaiWatcherTraceSampleRate != 1 {
		return
	}
	sampledArgs := append(append([]any{}, args...), n, xaiWatcherTraceSampleRate)
	log.Debugf(format+" sample_n=%d sample_rate=%d", sampledArgs...)
}
