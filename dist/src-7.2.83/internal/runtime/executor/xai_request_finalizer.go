package executor

import (
	"context"
	"net/http"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type xaiFinalizedRequest struct {
	Headers   http.Header
	Body      []byte
	SessionID string
}

func finalizeXAIRequest(ctx context.Context, opts cliproxyexecutor.Options, prepared *xaiPreparedRequest, headers http.Header) xaiFinalizedRequest {
	finalHeaders, finalBody := applyRequestFinalizer(ctx, opts, prepared.to, prepared.baseModel, headers, prepared.body)
	finalSessionID := prepared.sessionID
	if promptCacheKey := strings.TrimSpace(gjson.GetBytes(finalBody, "prompt_cache_key").String()); promptCacheKey != "" {
		finalSessionID = promptCacheKey
	}

	return xaiFinalizedRequest{
		Headers:   finalHeaders,
		Body:      finalBody,
		SessionID: finalSessionID,
	}
}

func applyFinalizedXAIRequest(prepared *xaiPreparedRequest, finalized xaiFinalizedRequest) {
	prepared.body = finalized.Body
	prepared.sessionID = finalized.SessionID
}
