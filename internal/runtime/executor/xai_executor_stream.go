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
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func (e *XAIExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	if xaiIsSpeechRequest(opts) {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /audio/speech"}
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
	// BEGIN xAI Guardian core extension: guarded attempts publish through the reporter
	// once the conductor knows the guard verdict, so early failures carry no tokens.
	publishGuardedAttempt := func(verdict cliproxyexecutor.XAIAttemptVerdict) {
		reporter.PublishXAIAttempt(ctx, coreusage.Detail{}, verdict.Guard, verdict.GuardReason, verdict.Degraded, verdict.Failed, verdict.StatusCode, verdict.FailBody)
	}
	// END xAI Guardian core extension.

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	prepared.body = prepared.finalizePayload(prepared.body)
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
			return xAIStreamGuardFailureResult(auth, opts, prepared, guardRuntime, upstreamStartedAt, time.Time{}, nil, nil, 0, err, publishGuardedAttempt), nil
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
				return xAIStreamGuardFailureResult(auth, opts, prepared, guardRuntime, upstreamStartedAt, firstResponseByteAt, httpResp.Header, nil, httpResp.StatusCode, errRead, publishGuardedAttempt), nil
			}
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		statusErr := xaiStatusErr(httpResp.StatusCode, data)
		if guardRuntime != nil {
			return xAIStreamGuardFailureResult(auth, opts, prepared, guardRuntime, upstreamStartedAt, firstResponseByteAt, httpResp.Header, data, httpResp.StatusCode, statusErr, publishGuardedAttempt), nil
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
		// guardUsageDetail carries the terminal token breakdown to the guard verdict
		// publisher; guarded attempts never publish from the executor.
		var guardUsageDetail coreusage.Detail
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
					// BEGIN xAI Guardian core extension: the conductor publishes this
					// attempt through the reporter once the guard verdict is known.
					Publish: func(verdict cliproxyexecutor.XAIAttemptVerdict) {
						reporter.PublishXAIAttempt(ctx, guardUsageDetail, verdict.Guard, verdict.GuardReason, verdict.Degraded, verdict.Failed, verdict.StatusCode, verdict.FailBody)
					},
					// END xAI Guardian core extension.
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
		var streamUsage helps.StreamUsageBuffer
		defer func() {
			// BEGIN xAI Guardian core extension: the guard verdict decides this
			// attempt's usage record, so only carry the tokens forward here.
			if guardRuntime != nil {
				if detail, ok := streamUsage.Detail(); ok {
					guardUsageDetail = detail
				}
				return
			}
			// END xAI Guardian core extension.
			streamUsage.Publish(ctx, reporter)
		}()
		var param any
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		responseFilter := newXAIInternalXSearchResponseFilter(prepared.filterInternalXSearch, prepared.clientDeclaredTools)
		namespaceRestorer := newXAINamespaceRestorer(prepared.namespaceTools)
		var pendingEventLine []byte
		// Translated lines belong to one SSE frame. Buffer them and hand downstream one
		// frame boundary-aligned chunk: the frame validator drops standalone blank-line
		// chunks, and the replay/heartbeat rewrite only advances on a chunk that ends a
		// frame, so a line-per-chunk stream loses every event of a buffered xAI stream.
		var frameBuf []byte
		flushFrame := func() bool {
			if len(frameBuf) == 0 {
				return true
			}
			payload := frameBuf
			frameBuf = nil
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: payload}:
			case <-ctx.Done():
				return false
			}
			return true
		}
		emitTranslatedLine := func(translatedLine []byte) bool {
			// BEGIN xAI Guardian core extension: record xAI visible output.
			if guardRuntime != nil && xAIStreamLineHasVisibleOutput(translatedLine) {
				guardRuntime.markVisible()
			}
			// END xAI Guardian core extension.
			lines, errBridge := prepared.applyPatch.Stream(translatedLine)
			if errBridge != nil && guardRuntime == nil {
				streamUsage.PublishFailure(ctx, reporter, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage})
			}
			var chunks [][]byte
			for _, line := range lines {
				if bytes.HasPrefix(line, xaiDataTag) {
					eventData := bytes.TrimSpace(line[len(xaiDataTag):])
					switch gjson.GetBytes(eventData, "type").String() {
					case "response.output_item.done":
						xaiCollectOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
					case "response.completed", "response.incomplete":
						// Reconstruct only after the bridge has restored dispatcher children.
						eventData = xaiPatchCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
						eventData = xaiNormalizeReasoningSummaryData(eventData)
						if gjson.GetBytes(eventData, "type").String() == "response.completed" {
							// Only completed responses carry replayable terminal state.
							cacheXAIReasoningReplayFromCompleted(ctx, prepared.replayScope, eventData)
						}
						ending := line[len(bytes.TrimRight(line, "\r\n")):]
						line = append(append([]byte("data: "), eventData...), ending...)
					}
				}
				chunks = append(chunks, helps.TranslateStreamWithClaudeInputTokens(ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, line, &param, claudeInputTokens)...)
			}
			if guardRuntime == nil {
				helps.RecordApplyPatchStreamFailureWithUsage(ctx, param, reporter, &streamUsage, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage})
			}
			for i := range chunks {
				// scanner.Bytes excludes the source newline, so every translated line is
				// re-terminated; the trailing element keeps the frame's blank line.
				frameBuf = append(frameBuf, chunks[i]...)
				frameBuf = append(frameBuf, '\n')
			}
			if bytes.HasSuffix(frameBuf, []byte("\n\n")) {
				if !flushFrame() {
					return false
				}
			}
			gatewayErr := statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}
			if guardRuntime == nil {
				if helps.StopApplyPatchStreamWithUsage(ctx, param, reporter, &streamUsage, out, gatewayErr) {
					return false
				}
				if errBridge != nil {
					errBridge = gatewayErr
					streamUsage.PublishFailure(ctx, reporter, errBridge)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errBridge}:
					case <-ctx.Done():
					}
					return false
				}
			} else if errBridge != nil || helps.ApplyPatchTranslationError(param) != nil {
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: gatewayErr}:
				case <-ctx.Done():
				}
				return false
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
					prepared.applyPatch.RememberDispatcherEvent(eventData)
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
					if normalizedEventName == "response.completed" || normalizedEventName == "response.incomplete" {
						// BEGIN xAI Guardian core extension: record terminal xAI response.
						guardCompleted = true
						// END xAI Guardian core extension.
						if detail, ok := helps.ParseCodexUsage(eventData); ok {
							streamUsage.Observe(detail, true)
						}
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
		finishEvents, errFinish := prepared.applyPatch.FinishStream()
		if errFinish != nil && guardRuntime == nil {
			streamUsage.PublishFailure(ctx, reporter, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage})
		}
		for _, event := range finishEvents {
			for _, chunk := range helps.TranslateStreamWithClaudeInputTokens(ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, event, &param, claudeInputTokens) {
				frameBuf = append(frameBuf, chunk...)
				frameBuf = append(frameBuf, '\n')
				if bytes.HasSuffix(frameBuf, []byte("\n\n")) {
					if !flushFrame() {
						return
					}
				}
			}
		}
		// A stream that ends mid-frame still owes the consumer its last frame, and an
		// unterminated tail would be dropped by the replay rewrite.
		if len(frameBuf) > 0 {
			if !bytes.HasSuffix(frameBuf, []byte("\n\n")) {
				frameBuf = append(frameBuf, '\n')
			}
			if !flushFrame() {
				return
			}
		}
		if errFinish != nil {
			errFinish = statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}
			if guardRuntime == nil {
				streamUsage.PublishFailure(ctx, reporter, errFinish)
			} else {
				guardErr = errFinish
			}
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errFinish}:
			case <-ctx.Done():
			}
			return
		}
		if errScan := scanner.Err(); errScan != nil {
			// BEGIN xAI Guardian core extension: preserve xAI stream read failure.
			guardErr = errScan
			// END xAI Guardian core extension.
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			// BEGIN xAI Guardian core extension: a guarded attempt publishes once the
			// guard verdict is known, carrying this read failure.
			if guardRuntime == nil {
				reporter.PublishFailure(ctx, errScan)
			}
			// END xAI Guardian core extension.
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
