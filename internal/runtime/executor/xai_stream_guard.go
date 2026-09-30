package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// BEGIN xAI Guardian core extension: xAI-only stream timing guard.

const xAIStreamGuardDefaultMaxRetries = 5

func prepareXAIStreamGuard(ctx context.Context, auth *cliproxyauth.Auth, prepared *xaiPreparedRequest, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (context.Context, *xAIStreamGuardRuntime, int, error) {
	if opts.XAIStreamGuard == nil {
		return ctx, nil, 0, nil
	}
	response, errPrepare := opts.XAIStreamGuard.PrepareXAIStream(ctx, cliproxyexecutor.XAIStreamPrepareRequest{
		RequestID:       opts.RequestID,
		TraceID:         opts.TraceID,
		Provider:        "xai",
		SourceFormat:    opts.SourceFormat.String(),
		Model:           prepared.baseModel,
		RequestedModel:  xaiRequestedModel(opts, req),
		AuthID:          auth.ID,
		AuthIndex:       auth.Index,
		AuthFileName:    auth.FileName,
		ProxyURL:        auth.ProxyURL,
		RequestHeaders:  opts.Headers,
		OriginalRequest: prepared.originalPayload,
		RequestBody:     prepared.body,
		StartedAt:       time.Now(),
		Metadata:        opts.Metadata,
	})
	if errPrepare != nil {
		return nil, nil, 0, errPrepare
	}
	maxRetries := response.MaxRetries
	if maxRetries <= 0 {
		maxRetries = xAIStreamGuardDefaultMaxRetries
	}
	guardCtx, runtime := newXAIStreamGuardRuntime(ctx, response.FirstPayloadTimeoutSeconds, response.ProgressTimeoutSeconds)
	runtime.metadata = response.Metadata
	runtime.maxRetries = maxRetries
	return guardCtx, runtime, maxRetries, nil
}

func xaiRequestedModel(opts cliproxyexecutor.Options, req cliproxyexecutor.Request) string {
	if opts.Metadata != nil {
		if requested, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey].(string); ok {
			return requested
		}
	}
	return req.Model
}

func xAIStreamGuardFailureResult(auth *cliproxyauth.Auth, opts cliproxyexecutor.Options, prepared *xaiPreparedRequest, runtime *xAIStreamGuardRuntime, upstreamStartedAt, firstResponseByteAt time.Time, responseHeaders http.Header, body []byte, statusCode int, err error) *cliproxyexecutor.StreamResult {
	runtime.stop()
	if timeoutErr := runtime.timeoutError(); timeoutErr != nil {
		err = timeoutErr
	}
	out := make(chan cliproxyexecutor.StreamChunk, 1)
	out <- cliproxyexecutor.StreamChunk{Err: err}
	close(out)
	completion := make(chan cliproxyexecutor.XAIStreamCompletion, 1)
	completion <- cliproxyexecutor.XAIStreamCompletion{
		Provider:            "xai",
		AuthID:              auth.ID,
		AuthIndex:           auth.Index,
		AuthFileName:        auth.FileName,
		ProxyURL:            auth.ProxyURL,
		ResponseHeaders:     responseHeaders.Clone(),
		Body:                bytes.Clone(body),
		StatusCode:          statusCode,
		Err:                 err,
		Completed:           false,
		StartedAt:           upstreamStartedAt,
		UpstreamStartedAt:   upstreamStartedAt,
		FirstResponseByteAt: firstResponseByteAt,
		FirstPayloadAt:      runtime.firstPayloadTime(),
		FirstVisibleAt:      runtime.firstVisibleTime(),
		FinishedAt:          time.Now(),
		MaxRetries:          runtime.maxRetries,
		Metadata:            runtime.metadata,
	}
	close(completion)
	return &cliproxyexecutor.StreamResult{Headers: responseHeaders.Clone(), Chunks: out, XAICompletion: completion}
}

func xAIStreamLineHasVisibleOutput(line []byte) bool {
	data := bytes.TrimSpace(line)
	if bytes.HasPrefix(data, []byte("data:")) {
		data = bytes.TrimSpace(data[len("data:"):])
	}
	eventType := gjson.GetBytes(data, "type").String()
	if eventType == "response.output_text.delta" {
		return gjson.GetBytes(data, "delta").String() != ""
	}
	if eventType == "response.output_text.done" {
		return gjson.GetBytes(data, "text").String() != ""
	}
	return false
}

func xAIStreamLineHasPayload(line []byte) bool {
	data := bytes.TrimSpace(line)
	if bytes.HasPrefix(data, []byte("event:")) {
		return false
	}
	if bytes.HasPrefix(data, []byte("data:")) {
		data = bytes.TrimSpace(data[len("data:"):])
	}
	return len(data) > 0 && !bytes.Equal(data, []byte("[DONE]"))
}

func xAIStreamLineHasProgress(line []byte) bool {
	data := bytes.TrimSpace(line)
	if bytes.HasPrefix(data, []byte("event:")) {
		return false
	}
	if bytes.HasPrefix(data, []byte("data:")) {
		data = bytes.TrimSpace(data[len("data:"):])
	}
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return false
	}
	eventType := gjson.GetBytes(data, "type").String()
	switch eventType {
	case "response.created", "response.queued", "response.in_progress", "response.completed", "response.incomplete":
		return false
	default:
		return true
	}
}

type xAIStreamGuardRuntime struct {
	cancel context.CancelCauseFunc

	firstPayloadTimeout time.Duration
	progressTimeout     time.Duration

	mu                sync.Mutex
	firstPayloadAt    time.Time
	firstVisibleAt    time.Time
	firstPayloadTimer *time.Timer
	progressTimer     *time.Timer
	timeoutErr        error
	metadata          map[string]any
	maxRetries        int
	finished          bool
}

func newXAIStreamGuardRuntime(ctx context.Context, firstPayloadSeconds, progressSeconds int) (context.Context, *xAIStreamGuardRuntime) {
	guardCtx, cancel := context.WithCancelCause(ctx)
	runtime := &xAIStreamGuardRuntime{
		cancel:              cancel,
		firstPayloadTimeout: time.Duration(firstPayloadSeconds) * time.Second,
		progressTimeout:     time.Duration(progressSeconds) * time.Second,
	}
	if runtime.firstPayloadTimeout > 0 {
		runtime.firstPayloadTimer = time.AfterFunc(runtime.firstPayloadTimeout, func() {
			runtime.timeout(fmt.Errorf("xAI stream first payload timeout after %s", runtime.firstPayloadTimeout))
		})
	}
	return guardCtx, runtime
}

func (r *xAIStreamGuardRuntime) observeFirstPayload() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	if r.firstPayloadAt.IsZero() {
		r.firstPayloadAt = time.Now()
		if r.firstPayloadTimer != nil {
			r.firstPayloadTimer.Stop()
		}
	}
	r.mu.Unlock()
}

func (r *xAIStreamGuardRuntime) observeProgress() {
	if r == nil {
		return
	}
	now := time.Now()
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	if r.firstPayloadAt.IsZero() {
		r.firstPayloadAt = now
		if r.firstPayloadTimer != nil {
			r.firstPayloadTimer.Stop()
		}
	}
	if r.progressTimer == nil && r.progressTimeout > 0 {
		r.progressTimer = time.AfterFunc(r.progressTimeout, func() {
			r.timeout(fmt.Errorf("xAI stream progress timeout after %s", r.progressTimeout))
		})
	} else if r.progressTimer != nil {
		r.progressTimer.Stop()
		r.progressTimer = time.AfterFunc(r.progressTimeout, func() {
			r.timeout(fmt.Errorf("xAI stream progress timeout after %s", r.progressTimeout))
		})
	}
	r.mu.Unlock()
}

func (r *xAIStreamGuardRuntime) timeout(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.finished || r.timeoutErr != nil {
		r.mu.Unlock()
		return
	}
	r.timeoutErr = err
	r.mu.Unlock()
	r.cancel(err)
}

func (r *xAIStreamGuardRuntime) stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	if r.firstPayloadTimer != nil {
		r.firstPayloadTimer.Stop()
	}
	if r.progressTimer != nil {
		r.progressTimer.Stop()
	}
	r.mu.Unlock()
	r.cancel(context.Canceled)
}

func (r *xAIStreamGuardRuntime) firstPayloadTime() time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstPayloadAt
}

func (r *xAIStreamGuardRuntime) timeoutError() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timeoutErr
}

func (r *xAIStreamGuardRuntime) markVisible() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.firstVisibleAt.IsZero() {
		r.firstVisibleAt = time.Now()
	}
	r.mu.Unlock()
}

func (r *xAIStreamGuardRuntime) firstVisibleTime() time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstVisibleAt
}

// END xAI Guardian core extension.
