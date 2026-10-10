package executor

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func (e *XAIExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	if xaiInputHasItemType(req.Payload, "compaction_trigger") {
		return e.executeCompactionTriggerStream(ctx, auth, req, opts)
	}

	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)
	logXAIResolvedBaseURL(ctx, baseURL)

	prepared, err := e.prepareResponsesRequest(ctx, req, opts, true)
	if err != nil {
		return nil, err
	}

	// BEGIN xAI Guardian core extension: only the xAI stream executor installs
	// the synchronous guard context and progress policy.
	guardCtx, guardRuntime, _, errGuard := prepareXAIStreamGuard(ctx, auth, prepared, req, opts)
	if errGuard != nil {
		return nil, errGuard
	}
	ctx = guardCtx
	// END xAI Guardian core extension.

	reporter := helps.NewExecutorUsageReporter(ctx, e, prepared.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(prepared.body, e.Identifier())

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(prepared.body))
	if err != nil {
		return nil, err
	}
	applyXAIChatHeaders(httpReq, auth, token, true, prepared.sessionID, opts.Headers)
	e.recordXAIRequest(ctx, auth, url, httpReq.Header.Clone(), prepared.body)

	// BEGIN xAI Guardian core extension: capture xAI attempt failures for the guard.
	upstreamStartedAt := time.Now()
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if guardRuntime != nil {
			return xAIStreamGuardFailureResult(auth, opts, prepared, guardRuntime, upstreamStartedAt, time.Time{}, nil, nil, 0, err), nil
		}
		return nil, err
	}
	firstResponseByteAt := time.Time{}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		firstResponseByteAt = time.Now()
		data, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			if guardRuntime != nil {
				return xAIStreamGuardFailureResult(auth, opts, prepared, guardRuntime, upstreamStartedAt, firstResponseByteAt, httpResp.Header, nil, httpResp.StatusCode, errRead), nil
			}
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		statusErr := xaiStatusErr(httpResp.StatusCode, data)
		if guardRuntime != nil {
			return xAIStreamGuardFailureResult(auth, opts, prepared, guardRuntime, upstreamStartedAt, firstResponseByteAt, httpResp.Header, data, httpResp.StatusCode, statusErr), nil
		}
		return nil, statusErr
	}
	// END xAI Guardian core extension.

	if guardRuntime != nil && opts.XAIResponsesStreamHeartbeat != nil && cliproxyexecutor.ResponseFormatOrSource(opts) == "openai-response" {
		opts.XAIResponsesStreamHeartbeat.Start(opts.RequestID, prepared.baseModel, httpResp.Header.Clone())
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	// BEGIN xAI Guardian core extension: expose one terminal completion record.
	var completion chan cliproxyexecutor.XAIStreamCompletion
	if guardRuntime != nil {
		completion = make(chan cliproxyexecutor.XAIStreamCompletion, 1)
	}
	// END xAI Guardian core extension.
	go func() {
		var guardBody bytes.Buffer
		var guardErr error
		var guardCompleted bool
		defer func() {
			// BEGIN xAI Guardian core extension: publish buffered xAI stream state.
			if guardRuntime != nil {
				guardRuntime.stop()
				if guardRuntime.timeoutError() != nil {
					guardErr = guardRuntime.timeoutError()
				}
				completion <- cliproxyexecutor.XAIStreamCompletion{
					Provider:            "xai",
					AuthID:              auth.ID,
					AuthIndex:           auth.Index,
					AuthFileName:        auth.FileName,
					ProxyURL:            auth.ProxyURL,
					ResponseHeaders:     httpResp.Header.Clone(),
					Body:                guardBody.Bytes(),
					StatusCode:          httpResp.StatusCode,
					Err:                 guardErr,
					Completed:           guardCompleted,
					StartedAt:           upstreamStartedAt,
					UpstreamStartedAt:   upstreamStartedAt,
					FirstResponseByteAt: firstResponseByteAt,
					FirstPayloadAt:      guardRuntime.firstPayloadTime(),
					FirstVisibleAt:      guardRuntime.firstVisibleTime(),
					FinishedAt:          time.Now(),
					MaxRetries:          guardRuntime.maxRetries,
					Metadata:            guardRuntime.metadata,
				}
				close(completion)
			}
			// END xAI Guardian core extension.
			close(out)
		}()
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("xai executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		claudeInputTokens := helps.NewClaudeInputTokenState(prepared.from, prepared.to, prepared.responseFormat, prepared.originalPayload)
		var param any
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		responseFilter := newXAIInternalXSearchResponseFilter(prepared.filterInternalXSearch, prepared.clientDeclaredTools)
		namespaceRestorer := newXAINamespaceRestorer(prepared.namespaceTools)
		var pendingEventLine []byte
		emitTranslatedLine := func(translatedLine []byte) bool {
			// BEGIN xAI Guardian core extension: record xAI visible output.
			if guardRuntime != nil && xAIStreamLineHasVisibleOutput(translatedLine) {
				guardRuntime.markVisible()
			}
			// END xAI Guardian core extension.
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, translatedLine, &param, claudeInputTokens)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			// BEGIN xAI Guardian core extension: observe xAI payload progress.
			if firstResponseByteAt.IsZero() {
				firstResponseByteAt = time.Now()
			}
			if guardRuntime != nil {
				if xAIStreamLineHasPayload(line) {
					guardRuntime.observeFirstPayload()
				}
				if xAIStreamLineHasProgress(line) {
					guardRuntime.observeProgress()
				}
				guardBody.Write(line)
				guardBody.WriteByte('\n')
			}
			// END xAI Guardian core extension.
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)

			if bytes.HasPrefix(line, xaiEventTag) {
				if pendingEventLine != nil && !emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, "")) {
					return
				}
				pendingEventLine = bytes.Clone(line)
				continue
			}

			if bytes.HasPrefix(line, xaiDataTag) {
				eventDataList := xaiNormalizeReasoningSummaryDataEvents(bytes.TrimSpace(line[len(xaiDataTag):]))
				hasPendingEventLine := pendingEventLine != nil
				for i, eventData := range eventDataList {
					eventData = namespaceRestorer.restore(eventData)
					if prepared.webSearchAlias != "" {
						eventData = restoreXAIClientWebSearchName(eventData, prepared.webSearchAlias)
					}
					eventData = responseFilter.apply(eventData)
					if len(eventData) == 0 {
						if hasPendingEventLine && i == 0 {
							pendingEventLine = nil
						}
						continue
					}
					reporter.ObserveResponseModel(eventData)
					normalizedEventName := gjson.GetBytes(eventData, "type").String()
					switch normalizedEventName {
					case "response.output_item.done":
						xaiCollectOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
					case "response.completed", "response.incomplete":
						// BEGIN xAI Guardian core extension: record terminal xAI response.
						guardCompleted = true
						// END xAI Guardian core extension.
						if detail, ok := helps.ParseCodexUsage(eventData); ok {
							reporter.Publish(ctx, detail)
						}
						eventData = xaiPatchCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
						eventData = xaiNormalizeReasoningSummaryData(eventData)
						if normalizedEventName == "response.completed" {
							// A truncated turn carries no replayable terminal state, so only a
							// completed response may refresh the reasoning replay cache.
							cacheXAIReasoningReplayFromCompleted(ctx, prepared.replayScope, eventData)
						}
						normalizedEventName = gjson.GetBytes(eventData, "type").String()
					}

					if hasPendingEventLine {
						eventLine := []byte("event: " + normalizedEventName)
						if i == 0 {
							eventLine = xaiNormalizeReasoningSummaryEventLine(pendingEventLine, normalizedEventName)
							pendingEventLine = nil
						}
						if !emitTranslatedLine(eventLine) {
							return
						}
					}
					if !emitTranslatedLine(append([]byte("data: "), eventData...)) {
						return
					}
				}
				continue
			}

			if pendingEventLine != nil {
				if !emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, "")) {
					return
				}
				pendingEventLine = nil
			}
			if !emitTranslatedLine(bytes.Clone(line)) {
				return
			}
		}
		if pendingEventLine != nil {
			emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, ""))
		}
		if errScan := scanner.Err(); errScan != nil {
			// BEGIN xAI Guardian core extension: preserve xAI stream read failure.
			guardErr = errScan
			// END xAI Guardian core extension.
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
	}()
	// BEGIN xAI Guardian core extension: attach completion only to xAI streams.
	result := &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out, XAICompletion: completion}
	// END xAI Guardian core extension.
	return result, nil
}
