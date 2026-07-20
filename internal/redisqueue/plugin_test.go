package redisqueue

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestUsageQueuePluginPayloadIncludesStableFieldsAndSuccess(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-request-id")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)
		responseHeaders := http.Header{}
		responseHeaders.Add("X-Upstream-Request-Id", "upstream-req-1")
		responseHeaders.Add("Retry-After", "30")

		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:            "openai",
			ExecutorType:        "KimiExecutor",
			Model:               "gpt-5.4",
			Alias:               "client-gpt",
			APIKey:              "test-key",
			AuthIndex:           "0",
			AuthType:            "apikey",
			Source:              "user@example.com",
			ReasoningEffort:     "medium",
			ServiceTier:         "auto",
			ResponseServiceTier: "default",
			Generate:            coreusage.GenerateFlag(true),
			RequestedAt:         time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			Latency:             1500 * time.Millisecond,
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
			ResponseHeaders: responseHeaders.Clone(),
		})
		responseHeaders.Set("Retry-After", "999")

		payload := popSinglePayload(t)
		requireStringField(t, payload, "provider", "openai")
		requireStringField(t, payload, "executor_type", "KimiExecutor")
		requireStringField(t, payload, "model", "gpt-5.4")
		requireStringField(t, payload, "alias", "client-gpt")
		requireStringField(t, payload, "endpoint", "POST /v1/chat/completions")
		requireStringField(t, payload, "auth_type", "apikey")
		requireMissingField(t, payload, "user_api_key")
		requireStringField(t, payload, "request_id", "ctx-request-id")
		requireStringField(t, payload, "reasoning_effort", "medium")
		requireStringField(t, payload, "service_tier", "auto")
		requireMissingField(t, payload, "request_service_tier")
		requireStringField(t, payload, "response_service_tier", "default")
		requireTokensBoolField(t, payload, "cache_read_tokens_present", true)
		requireHeaderField(t, payload, "response_headers", "X-Upstream-Request-Id", []string{"upstream-req-1"})
		requireHeaderField(t, payload, "response_headers", "Retry-After", []string{"30"})
		requireBoolField(t, payload, "failed", false)
		requireBoolField(t, payload, "generate", true)
		requireFailField(t, payload, http.StatusOK, "")
	})
}

func TestUsageQueuePluginPayloadIncludesMetadata(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-request-id")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/responses")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		metadata := map[string]any{
			"cpa.project_id":              "cpa",
			"cpa.prompt_cache_key":        "project:gpt-5-codex:cpa",
			"cpa.compact.detected":        true,
			"cpa.compact.route_rewritten": true,
		}
		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:    "codex",
			Model:       "gpt-5.4-mini",
			Alias:       "gpt-5.4-mini",
			APIKey:      "test-key",
			AuthIndex:   "0",
			AuthType:    "oauth",
			Source:      "user@example.com",
			RequestedAt: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			Detail:      coreusage.Detail{InputTokens: 10, TotalTokens: 10},
			Metadata:    metadata,
		})
		metadata["cpa.project_id"] = "mutated"

		payload := popSinglePayload(t)
		requireMetadataField(t, payload, "cpa.project_id", "cpa")
		requireMetadataField(t, payload, "cpa.prompt_cache_key", "project:gpt-5-codex:cpa")
		requireMetadataBoolField(t, payload, "cpa.compact.detected", true)
		requireMetadataBoolField(t, payload, "cpa.compact.route_rewritten", true)
	})
}

func TestUsageQueuePluginDropsNonJSONMetadataAndStillEnqueues(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-request-id")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		callbackInvoked := false
		metadata := map[string]any{
			"cpa.project_id":                 "cpa",
			"selected_auth_callback":         func(authID string) { callbackInvoked = true },
			"selected_auth_index_callback":   func(authIndex string) {},
			"nested":                         map[string]any{"ok": "value", "bad": func() {}},
		}
		(&usageQueuePlugin{}).HandleUsage(ctx, coreusage.Record{
			Provider:    "antigravity",
			Model:       "gemini-3.1-flash-lite",
			Alias:       "gemini-3.1-flash-lite",
			APIKey:      "test-key",
			AuthIndex:   "0",
			AuthType:    "oauth",
			Source:      "user@example.com",
			RequestedAt: time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC),
			Detail:      coreusage.Detail{InputTokens: 3, TotalTokens: 3},
			Metadata:    metadata,
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "provider", "antigravity")
		requireStringField(t, payload, "model", "gemini-3.1-flash-lite")
		requireMetadataField(t, payload, "cpa.project_id", "cpa")
		queuedMetadata := metadataPayload(t, payload)
		for _, key := range []string{"selected_auth_callback", "selected_auth_index_callback", "nested"} {
			if _, exists := queuedMetadata[key]; exists {
				t.Fatalf("metadata unexpectedly contains %q", key)
			}
		}
		if callbackInvoked {
			t.Fatalf("callback should not be invoked by usage queue marshaling")
		}
	})
}

func TestUsageQueuePluginPayloadIncludesGenerateFalse(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithResponseStatusHolder(context.Background())
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		(&usageQueuePlugin{}).HandleUsage(ctx, coreusage.Record{
			Provider: "openai",
			Model:    "gpt-5.4",
			Generate: coreusage.GenerateFlag(false),
			Detail: coreusage.Detail{
				InputTokens: 1,
				TotalTokens: 1,
			},
		})

		payload := popSinglePayload(t)
		requireBoolField(t, payload, "generate", false)
	})
}

func TestUsageQueuePluginPayloadDefaultsGenerateTrueWhenOmitted(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithResponseStatusHolder(context.Background())
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		(&usageQueuePlugin{}).HandleUsage(ctx, coreusage.Record{
			Provider: "openai",
			Model:    "gpt-5.4",
			Detail: coreusage.Detail{
				InputTokens: 1,
				TotalTokens: 1,
			},
		})

		payload := popSinglePayload(t)
		requireBoolField(t, payload, "generate", true)
	})
}

func TestUsageQueuePluginMarksCanonicalZeroCacheRead(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithResponseStatusHolder(context.Background())
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		(&usageQueuePlugin{}).HandleUsage(ctx, coreusage.Record{
			Provider: "openai",
			Model:    "gpt-5.4",
			Detail: coreusage.Detail{
				CachedTokens:    13,
				CacheReadTokens: 0,
			},
		})

		payload := popSinglePayload(t)
		requireTokensBoolField(t, payload, "cache_read_tokens_present", true)
		tokens := requireTokensPayload(t, payload)
		var cacheReadTokens int64
		if errUnmarshal := json.Unmarshal(tokens["cache_read_tokens"], &cacheReadTokens); errUnmarshal != nil {
			t.Fatalf("unmarshal cache_read_tokens: %v", errUnmarshal)
		}
		if cacheReadTokens != 0 {
			t.Fatalf("cache_read_tokens = %d, want 0", cacheReadTokens)
		}
	})
}

func TestUsageQueuePluginEmitsSingleCanonicalAutoTier(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := coreusage.WithServiceTier(context.Background(), coreusage.AutoServiceTier)
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		(&usageQueuePlugin{}).HandleUsage(ctx, coreusage.Record{
			Provider: "openai",
			Model:    "gpt-5.4",
			Detail: coreusage.Detail{
				InputTokens: 1,
				TotalTokens: 1,
			},
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "service_tier", "auto")
		requireMissingField(t, payload, "request_service_tier")
	})
}

func TestUsageQueuePluginAcceptsDeprecatedRequestTierRecordField(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithResponseStatusHolder(context.Background())
		internallogging.SetResponseStatus(ctx, http.StatusOK)

		(&usageQueuePlugin{}).HandleUsage(ctx, coreusage.Record{
			Provider:           "openai",
			Model:              "gpt-5.4",
			RequestServiceTier: "priority",
			Detail:             coreusage.Detail{InputTokens: 1, TotalTokens: 1},
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "service_tier", "priority")
		requireMissingField(t, payload, "request_service_tier")
	})
}

func TestUsageQueuePluginAsyncUsesRecordResponseHeaders(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "ctx-request-id")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		ctx = internallogging.WithResponseHeadersHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusOK)
		initialHeaders := http.Header{}
		initialHeaders.Set("X-Upstream-Request-Id", "upstream-req-1")
		internallogging.SetResponseHeaders(ctx, initialHeaders)

		mgr := coreusage.NewManager(16)
		defer mgr.Stop()

		mgr.Register(pluginFunc(func(ctx context.Context, _ coreusage.Record) {
			nextHeaders := http.Header{}
			nextHeaders.Set("X-Upstream-Request-Id", "upstream-req-2")
			internallogging.SetResponseHeaders(ctx, nextHeaders)
		}))
		mgr.Register(&usageQueuePlugin{})

		mgr.Publish(ctx, coreusage.Record{
			Provider:    "openai",
			Model:       "gpt-5.4",
			Alias:       "client-gpt",
			APIKey:      "test-key",
			AuthIndex:   "0",
			AuthType:    "apikey",
			Source:      "user@example.com",
			RequestedAt: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			Latency:     1500 * time.Millisecond,
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
			ResponseHeaders: internallogging.GetResponseHeaders(ctx),
		})

		payload := waitForSinglePayload(t, 2*time.Second)
		requireHeaderField(t, payload, "response_headers", "X-Upstream-Request-Id", []string{"upstream-req-1"})
	})
}

func TestUsageQueuePluginPayloadIncludesStableFieldsAndFailureAndGinRequestID(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := internallogging.WithRequestID(context.Background(), "gin-request-id")
		ctx = internallogging.WithEndpoint(ctx, "GET /v1/responses")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusInternalServerError)

		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:    "openai",
			Model:       "gpt-5.4-mini",
			Alias:       "client-mini",
			APIKey:      "test-key",
			AuthIndex:   "0",
			AuthType:    "apikey",
			Source:      "user@example.com",
			RequestedAt: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			Latency:     2500 * time.Millisecond,
			Fail: coreusage.Failure{
				StatusCode: http.StatusInternalServerError,
				Body:       "upstream failed",
			},
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "provider", "openai")
		requireStringField(t, payload, "model", "gpt-5.4-mini")
		requireStringField(t, payload, "alias", "client-mini")
		requireStringField(t, payload, "endpoint", "GET /v1/responses")
		requireStringField(t, payload, "auth_type", "apikey")
		requireMissingField(t, payload, "user_api_key")
		requireStringField(t, payload, "request_id", "gin-request-id")
		requireBoolField(t, payload, "failed", true)
		requireFailField(t, payload, http.StatusInternalServerError, "upstream failed")
	})
}

func TestUsageQueuePluginAsyncIgnoresRecycledGinContext(t *testing.T) {
	withEnabledQueue(t, func() {
		ginCtx := newTestGinContext(t, http.MethodPost, "/v1/chat/completions", http.StatusOK)
		ctx := context.WithValue(context.Background(), "gin", ginCtx)
		ctx = internallogging.WithRequestID(ctx, "ctx-request-id")
		ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
		ctx = internallogging.WithResponseStatusHolder(ctx)
		internallogging.SetResponseStatus(ctx, http.StatusInternalServerError)

		mgr := coreusage.NewManager(16)
		defer mgr.Stop()

		mgr.Register(pluginFunc(func(_ context.Context, _ coreusage.Record) {
			ginCtx.Request = httptest.NewRequest(http.MethodGet, "http://example.com/v1/responses", nil)
			ginCtx.Status(http.StatusOK)
		}))
		mgr.Register(&usageQueuePlugin{})

		mgr.Publish(ctx, coreusage.Record{
			Provider:    "openai",
			Model:       "gpt-5.4",
			Alias:       "client-gpt",
			APIKey:      "test-key",
			AuthIndex:   "0",
			AuthType:    "apikey",
			Source:      "user@example.com",
			RequestedAt: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			Latency:     1500 * time.Millisecond,
			Fail: coreusage.Failure{
				StatusCode: http.StatusBadGateway,
				Body:       "bad gateway",
			},
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
		})

		payload := waitForSinglePayload(t, 2*time.Second)
		requireStringField(t, payload, "endpoint", "POST /v1/chat/completions")
		requireStringField(t, payload, "alias", "client-gpt")
		requireMissingField(t, payload, "user_api_key")
		requireStringField(t, payload, "request_id", "ctx-request-id")
		requireBoolField(t, payload, "failed", true)
		requireFailField(t, payload, http.StatusBadGateway, "bad gateway")
	})
}

func withEnabledQueue(t *testing.T, fn func()) {
	t.Helper()

	prevQueueEnabled := Enabled()
	prevUsageEnabled := UsageStatisticsEnabled()

	SetEnabled(false)
	SetEnabled(true)
	SetUsageStatisticsEnabled(true)

	defer func() {
		SetEnabled(false)
		SetEnabled(prevQueueEnabled)
		SetUsageStatisticsEnabled(prevUsageEnabled)
	}()

	fn()
}

func newTestGinContext(t *testing.T, method, path string, status int) *gin.Context {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(method, "http://example.com"+path, nil)
	if status != 0 {
		ginCtx.Status(status)
	}
	return ginCtx
}

func popSinglePayload(t *testing.T) map[string]json.RawMessage {
	t.Helper()

	items := PopOldest(10)
	if len(items) != 1 {
		t.Fatalf("PopOldest() items = %d, want 1", len(items))
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(items[0], &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return payload
}

func waitForSinglePayload(t *testing.T, timeout time.Duration) map[string]json.RawMessage {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		items := PopOldest(10)
		if len(items) == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if len(items) != 1 {
			t.Fatalf("PopOldest() items = %d, want 1", len(items))
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(items[0], &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		return payload
	}
	t.Fatalf("timeout waiting for queued payload")
	return nil
}

func requireStringField(t *testing.T, payload map[string]json.RawMessage, key, want string) {
	t.Helper()

	raw, ok := payload[key]
	if !ok {
		t.Fatalf("payload missing %q", key)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", key, err)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}

func requireMissingField(t *testing.T, payload map[string]json.RawMessage, key string) {
	t.Helper()

	if _, ok := payload[key]; ok {
		t.Fatalf("payload unexpectedly contains %q", key)
	}
}

type pluginFunc func(context.Context, coreusage.Record)

func (fn pluginFunc) HandleUsage(ctx context.Context, record coreusage.Record) {
	fn(ctx, record)
}

func requireBoolField(t *testing.T, payload map[string]json.RawMessage, key string, want bool) {
	t.Helper()

	raw, ok := payload[key]
	if !ok {
		t.Fatalf("payload missing %q", key)
	}
	var got bool
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", key, err)
	}
	if got != want {
		t.Fatalf("%s = %t, want %t", key, got, want)
	}
}

func requireTokensPayload(t *testing.T, payload map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	raw, ok := payload["tokens"]
	if !ok {
		t.Fatal("payload missing tokens")
	}
	var tokens map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &tokens); errUnmarshal != nil {
		t.Fatalf("unmarshal tokens: %v", errUnmarshal)
	}
	return tokens
}

func requireTokensBoolField(t *testing.T, payload map[string]json.RawMessage, key string, want bool) {
	t.Helper()
	requireBoolField(t, requireTokensPayload(t, payload), key, want)
}

func requireFailField(t *testing.T, payload map[string]json.RawMessage, wantStatus int, wantBody string) {
	t.Helper()

	raw, ok := payload["fail"]
	if !ok {
		t.Fatalf("payload missing %q", "fail")
	}
	var got struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal fail: %v", err)
	}
	if got.StatusCode != wantStatus || got.Body != wantBody {
		t.Fatalf("fail = {status_code:%d body:%q}, want {status_code:%d body:%q}", got.StatusCode, got.Body, wantStatus, wantBody)
	}
}

func requireHeaderField(t *testing.T, payload map[string]json.RawMessage, field, key string, want []string) {
	t.Helper()

	raw, ok := payload[field]
	if !ok {
		t.Fatalf("payload missing %q", field)
	}
	var headers map[string][]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		t.Fatalf("unmarshal %q: %v", field, err)
	}
	got, ok := headers[key]
	if !ok {
		t.Fatalf("%s missing header %q", field, key)
	}
	if len(got) != len(want) {
		t.Fatalf("%s[%q] = %v, want %v", field, key, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%q] = %v, want %v", field, key, got, want)
		}
	}
}

func requireMetadataField(t *testing.T, payload map[string]json.RawMessage, key, want string) {
	t.Helper()

	metadata := metadataPayload(t, payload)
	raw, ok := metadata[key]
	if !ok {
		t.Fatalf("metadata missing %q", key)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal metadata %q: %v", key, err)
	}
	if got != want {
		t.Fatalf("metadata[%q] = %q, want %q", key, got, want)
	}
}

func requireMetadataBoolField(t *testing.T, payload map[string]json.RawMessage, key string, want bool) {
	t.Helper()

	metadata := metadataPayload(t, payload)
	raw, ok := metadata[key]
	if !ok {
		t.Fatalf("metadata missing %q", key)
	}
	var got bool
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal metadata %q: %v", key, err)
	}
	if got != want {
		t.Fatalf("metadata[%q] = %t, want %t", key, got, want)
	}
}

func metadataPayload(t *testing.T, payload map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()

	raw, ok := payload["metadata"]
	if !ok {
		t.Fatalf("payload missing %q", "metadata")
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	return metadata
}
