package executor

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type xaiFinalizedRequest struct {
	Headers             http.Header
	Body                []byte
	SessionID           string
	FirstPayloadTimeout time.Duration
}

func finalizeXAIRequest(ctx context.Context, opts cliproxyexecutor.Options, prepared *xaiPreparedRequest, headers http.Header) xaiFinalizedRequest {
	finalHeaders, finalBody := applyRequestFinalizer(ctx, opts, prepared.to, prepared.baseModel, headers, prepared.body)
	finalSessionID := prepared.sessionID
	if promptCacheKey := strings.TrimSpace(gjson.GetBytes(finalBody, "prompt_cache_key").String()); promptCacheKey != "" {
		finalSessionID = promptCacheKey
	}

	return xaiFinalizedRequest{
		Headers:             finalHeaders,
		Body:                finalBody,
		SessionID:           finalSessionID,
		FirstPayloadTimeout: realtimeGuardFirstPayloadTimeout(opts.Metadata),
	}
}

func realtimeGuardFirstPayloadTimeout(metadata map[string]any) time.Duration {
	if metadata == nil {
		return 0
	}
	value, exists := metadata[cliproxyexecutor.StreamCompletionTimeoutSecondsMetadataKey]
	if !exists {
		return 0
	}
	var seconds int64
	switch typed := value.(type) {
	case int:
		seconds = int64(typed)
	case int64:
		seconds = typed
	case float64:
		seconds = int64(typed)
	case string:
		seconds, _ = strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
	}
	if seconds < 1 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func applyFinalizedXAIRequest(prepared *xaiPreparedRequest, finalized xaiFinalizedRequest) {
	prepared.body = finalized.Body
	prepared.sessionID = finalized.SessionID
}
