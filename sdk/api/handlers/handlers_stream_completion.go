package handlers

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"golang.org/x/net/context"
)

const maxRealtimeGuardDegradedAccounts = 5

type streamCompletionHost interface {
	InterceptStreamCompletion(context.Context, pluginapi.StreamCompletionInterceptRequest) pluginapi.StreamCompletionInterceptResponse
	InterceptStreamCompletionRequired(context.Context, pluginapi.StreamCompletionInterceptRequest) (pluginapi.StreamCompletionInterceptResponse, error)
}

func streamCompletionGuardEnabled(_ PluginInterceptorHost, providers []string) bool {
	for _, provider := range providers {
		if strings.EqualFold(strings.TrimSpace(provider), "xai") {
			return true
		}
	}
	return false
}

func streamCompletionProvider(providers []string, metadata map[string]any) string {
	if selectedProvider := metadataString(metadata, coreexecutor.SelectedAuthProviderMetadataKey); selectedProvider != "" {
		return selectedProvider
	}
	for _, provider := range providers {
		if strings.EqualFold(strings.TrimSpace(provider), "xai") {
			return provider
		}
	}
	return ""
}

func (h *BaseAPIHandler) executeBufferedStreamWithGuard(
	ctx context.Context,
	providers []string,
	responseProtocol string,
	normalizedModel string,
	originalRequestedModel string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
	initialResult *coreexecutor.StreamResult,
	initialErr error,
	lifecycle *requestLifecycleTracker,
	execOptions modelExecutionOptions,
) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage, 1)
	streamHeaders := make(http.Header)
	if initialResult != nil {
		copyHeaderMap(streamHeaders, initialResult.Headers)
	}
	interceptorHost := h.interceptorHost()
	completionHost, completionGuardAvailable := interceptorHost.(streamCompletionHost)
	streamInterceptorsActive := streamInterceptorsEnabled(interceptorHost)

	if !completionGuardAvailable {
		errChan <- &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("xAI realtime guard host is unavailable")}
		close(dataChan)
		close(errChan)
		return dataChan, streamHeaders, errChan
	}

	go func() {
		completionOutcome := pluginapi.RequestCompletionSucceeded
		completionStatus := http.StatusOK
		var completionErr error
		defer func() {
			lifecycle.complete(completionOutcome, completionStatus, completionErr)
			close(dataChan)
			close(errChan)
		}()

		retryCount := 0
		currentResult := initialResult
		currentErr := initialErr
		for {
			if ctx != nil && ctx.Err() != nil {
				completionOutcome = pluginapi.RequestCompletionCanceled
				completionStatus = 0
				completionErr = ctx.Err()
				return
			}
			if currentErr == nil && currentResult == nil {
				currentResult, currentErr = h.AuthManager.ExecuteStream(ctx, providers, req, opts)
			}
			mergeStreamAttemptAuthMetadata(opts.Metadata, currentResult)

			attempt := collectBufferedStreamAttempt(ctx, interceptorHost, streamInterceptorsActive, responseProtocol, normalizedModel, originalRequestedModel, req, opts, currentResult, currentErr, execOptions.SkipInterceptorPluginID)
			decision, guardErr := completionHost.InterceptStreamCompletionRequired(ctx, buildStreamCompletionRequest(lifecycle, providers, responseProtocol, normalizedModel, originalRequestedModel, req, opts, attempt, retryCount))
			if guardErr != nil {
				decision = pluginapi.StreamCompletionInterceptResponse{
					Action:     pluginapi.StreamCompletionActionFail,
					Reason:     "realtime_guard_unavailable",
					StatusCode: http.StatusBadGateway,
					Error:      guardErr.Error(),
				}
			}
			if decision.Action == "" {
				decision.Action = pluginapi.StreamCompletionActionFail
			}

			switch decision.Action {
			case pluginapi.StreamCompletionActionFlush:
				if attempt.Err != nil {
					completionOutcome = pluginapi.RequestCompletionFailed
					completionStatus = attempt.StatusCode
					completionErr = attempt.Err
					sendBufferedStreamError(ctx, errChan, &interfaces.ErrorMessage{StatusCode: attempt.StatusCode, Error: attempt.Err})
					return
				}
				for _, payload := range attempt.Payloads {
					if !sendBufferedStreamPayload(ctx, dataChan, payload) {
						completionOutcome = pluginapi.RequestCompletionCanceled
						completionStatus = 0
						completionErr = ctx.Err()
						return
					}
				}
				return
			case pluginapi.StreamCompletionActionRetry:
				degradedAccountCount := retryCount + 1
				if degradedAccountCount >= maxRealtimeGuardDegradedAccounts {
					failure := realtimeGuardFailure(decision, "实时降智守护已处理 5 个降智账号")
					completionOutcome = pluginapi.RequestCompletionFailed
					completionStatus = failure.StatusCode
					completionErr = failure.Error
					sendBufferedStreamError(ctx, errChan, failure)
					return
				}
				retryCount++
				if errReload := h.reloadSelectedAuthForStreamRetry(ctx, opts.Metadata); errReload != nil {
					failure := realtimeGuardFailure(decision, errReload.Error())
					completionOutcome = pluginapi.RequestCompletionFailed
					completionStatus = failure.StatusCode
					completionErr = failure.Error
					sendBufferedStreamError(ctx, errChan, failure)
					return
				}
				currentResult, currentErr = h.AuthManager.ExecuteStream(ctx, providers, req, opts)
				continue
			case pluginapi.StreamCompletionActionFail:
				failure := realtimeGuardFailure(decision, attemptErrorMessage(attempt))
				completionOutcome = pluginapi.RequestCompletionFailed
				completionStatus = failure.StatusCode
				completionErr = failure.Error
				sendBufferedStreamError(ctx, errChan, failure)
				return
			default:
				failure := realtimeGuardFailure(decision, "实时降智守护返回了未知动作")
				completionOutcome = pluginapi.RequestCompletionFailed
				completionStatus = failure.StatusCode
				completionErr = failure.Error
				sendBufferedStreamError(ctx, errChan, failure)
				return
			}
		}
	}()

	return dataChan, streamHeaders, errChan
}

type bufferedStreamAttempt struct {
	Payloads          [][]byte
	DownstreamHeaders http.Header
	StatusCode        int
	Err               error
	Completed         bool
	StartedAt         time.Time
	FirstPayloadAt    time.Time
	FinishedAt        time.Time
	Body              []byte
	GuardBody         []byte
}

func collectBufferedStreamAttempt(
	ctx context.Context,
	interceptorHost PluginInterceptorHost,
	streamInterceptorsActive bool,
	responseProtocol string,
	normalizedModel string,
	originalRequestedModel string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
	streamResult *coreexecutor.StreamResult,
	streamErr error,
	skipPluginID string,
) bufferedStreamAttempt {
	attempt := bufferedStreamAttempt{
		StartedAt:  time.Now(),
		StatusCode: http.StatusOK,
		Err:        streamErr,
	}
	if streamErr != nil {
		attempt.StatusCode = statusFromError(streamErr)
		if attempt.StatusCode == 0 {
			attempt.StatusCode = http.StatusBadGateway
		}
		attempt.FinishedAt = time.Now()
		return attempt
	}
	if streamResult == nil {
		attempt.Err = fmt.Errorf("auth manager returned nil stream")
		attempt.StatusCode = http.StatusBadGateway
		attempt.FinishedAt = time.Now()
		return attempt
	}

	rawHeaders := cloneHeader(streamResult.Headers)
	baseHeaders := cloneHeader(streamResult.Headers)
	streamHeaderInitialized := false
	chunkIndex := 0
	var historyChunks [][]byte
	initializeHeaders := func() {
		if !streamInterceptorsActive || streamHeaderInitialized {
			return
		}
		intercepted := interceptStreamChunk(ctx, interceptorHost, pluginapi.StreamChunkInterceptRequest{
			RequestID:       "",
			SourceFormat:    responseProtocol,
			Model:           normalizedModel,
			RequestedModel:  originalRequestedModel,
			RequestHeaders:  cloneHeader(opts.Headers),
			ResponseHeaders: cloneHeader(rawHeaders),
			OriginalRequest: cloneBytes(opts.OriginalRequest),
			RequestBody:     cloneBytes(req.Payload),
			ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
			Metadata:        opts.Metadata,
		}, skipPluginID)
		rawHeaders = finalInterceptorHeaders(rawHeaders, intercepted.Headers)
		streamHeaderInitialized = true
	}
	chunks := streamResult.Chunks
	if chunks == nil {
		closed := make(chan coreexecutor.StreamChunk)
		close(closed)
		chunks = closed
	}
	for {
		chunk, ok := <-chunks
		if !ok {
			initializeHeaders()
			break
		}
		if chunk.Err != nil {
			attempt.Err = chunk.Err
			attempt.StatusCode = statusFromError(chunk.Err)
			if attempt.StatusCode == 0 {
				attempt.StatusCode = http.StatusBadGateway
			}
			break
		}
		if len(chunk.Payload) == 0 {
			continue
		}
		if attempt.FirstPayloadAt.IsZero() {
			attempt.FirstPayloadAt = time.Now()
		}
		payload := cloneBytes(chunk.Payload)
		initializeHeaders()
		if streamInterceptorsActive {
			intercepted := interceptStreamChunk(ctx, interceptorHost, pluginapi.StreamChunkInterceptRequest{
				RequestID:       "",
				SourceFormat:    responseProtocol,
				Model:           normalizedModel,
				RequestedModel:  originalRequestedModel,
				RequestHeaders:  cloneHeader(opts.Headers),
				ResponseHeaders: cloneHeader(rawHeaders),
				OriginalRequest: cloneBytes(opts.OriginalRequest),
				RequestBody:     cloneBytes(req.Payload),
				Body:            payload,
				HistoryChunks:   cloneByteSlices(historyChunks),
				ChunkIndex:      chunkIndex,
				Metadata:        opts.Metadata,
			}, skipPluginID)
			rawHeaders = finalInterceptorHeaders(rawHeaders, intercepted.Headers)
			if len(intercepted.Body) > 0 {
				payload = cloneBytes(intercepted.Body)
			}
			chunkIndex++
			if intercepted.DropChunk {
				continue
			}
		} else {
			chunkIndex++
		}
		if responseProtocol == "openai-response" {
			if errValidate := validateSSEDataJSON(payload); errValidate != nil {
				attempt.Err = errValidate
				attempt.StatusCode = http.StatusBadGateway
				break
			}
		}
		attempt.Payloads = append(attempt.Payloads, payload)
		historyChunks = appendStreamInterceptorHistory(historyChunks, payload)
	}
	attempt.FinishedAt = time.Now()
	attempt.Body = joinStreamPayloads(attempt.Payloads)
	attempt.GuardBody = attempt.Body
	attempt.Completed = bytes.Contains(attempt.Body, []byte("response.completed"))
	if attempt.Err == nil && streamResult.Completion != nil {
		sourceCompletion, received := <-streamResult.Completion
		if !received {
			attempt.Err = fmt.Errorf("xAI source stream completion state is unavailable")
			attempt.StatusCode = http.StatusBadGateway
		} else {
			attempt.Completed = sourceCompletion.Completed
			if !sourceCompletion.FirstPayloadAt.IsZero() {
				attempt.FirstPayloadAt = sourceCompletion.FirstPayloadAt
			}
			if !sourceCompletion.FinishedAt.IsZero() {
				attempt.FinishedAt = sourceCompletion.FinishedAt
			}
			attempt.GuardBody = cloneBytes(sourceCompletion.Body)
			if sourceCompletion.Err != nil && attempt.Err == nil {
				attempt.Err = sourceCompletion.Err
				attempt.StatusCode = http.StatusBadGateway
			}
		}
	}
	if attempt.Err == nil && !attempt.Completed {
		attempt.Err = fmt.Errorf("xAI stream disconnected before response.completed")
		attempt.StatusCode = http.StatusBadGateway
	}
	attempt.DownstreamHeaders = downstreamHeadersAfterInterceptors(baseHeaders, rawHeaders, false)
	return attempt
}

func buildStreamCompletionRequest(
	lifecycle *requestLifecycleTracker,
	providers []string,
	responseProtocol string,
	normalizedModel string,
	originalRequestedModel string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
	attempt bufferedStreamAttempt,
	retryCount int,
) pluginapi.StreamCompletionInterceptRequest {
	requestID := ""
	traceID := ""
	if lifecycle != nil {
		requestID = lifecycle.requestID()
		traceID = lifecycle.completion.TraceID
	}
	return pluginapi.StreamCompletionInterceptRequest{
		RequestID:       requestID,
		TraceID:         traceID,
		Provider:        streamCompletionProvider(providers, opts.Metadata),
		SourceFormat:    opts.SourceFormat.String(),
		Model:           normalizedModel,
		RequestedModel:  originalRequestedModel,
		AuthID:          metadataString(opts.Metadata, coreexecutor.SelectedAuthMetadataKey),
		AuthIndex:       metadataString(opts.Metadata, coreexecutor.SelectedAuthIndexMetadataKey),
		AuthFileName:    metadataString(opts.Metadata, coreexecutor.SelectedAuthFileNameMetadataKey),
		ProxyURL:        metadataString(opts.Metadata, coreexecutor.SelectedAuthProxyURLMetadataKey),
		RequestHeaders:  cloneHeader(opts.Headers),
		ResponseHeaders: cloneHeader(attempt.DownstreamHeaders),
		OriginalRequest: cloneBytes(opts.OriginalRequest),
		RequestBody:     cloneBytes(req.Payload),
		Body:            cloneBytes(attempt.GuardBody),
		StatusCode:      attempt.StatusCode,
		Error:           errorString(attempt.Err),
		Completed:       attempt.Completed,
		StartedAt:       attempt.StartedAt,
		FirstPayloadAt:  attempt.FirstPayloadAt,
		FinishedAt:      attempt.FinishedAt,
		RetryCount:      retryCount,
		MaxRetries:      maxRealtimeGuardDegradedAccounts,
		Metadata:        opts.Metadata,
	}
}

func mergeStreamAttemptAuthMetadata(metadata map[string]any, streamResult *coreexecutor.StreamResult) {
	if metadata == nil || streamResult == nil {
		return
	}
	for _, key := range []string{
		coreexecutor.SelectedAuthMetadataKey,
		coreexecutor.SelectedAuthIndexMetadataKey,
		coreexecutor.SelectedAuthProxyURLMetadataKey,
		coreexecutor.SelectedAuthProviderMetadataKey,
		coreexecutor.SelectedAuthFileNameMetadataKey,
	} {
		if value, exists := streamResult.Metadata[key]; exists {
			metadata[key] = value
		}
	}
}

func (h *BaseAPIHandler) reloadSelectedAuthForStreamRetry(ctx context.Context, metadata map[string]any) error {
	authID := metadataString(metadata, coreexecutor.SelectedAuthMetadataKey)
	if authID == "" {
		return fmt.Errorf("实时降智守护重试缺少当前 auth")
	}
	if _, errReload := h.AuthManager.ReloadAuthRuntimeFromFile(ctx, authID); errReload != nil {
		return errReload
	}
	delete(metadata, coreexecutor.SelectedAuthMetadataKey)
	delete(metadata, coreexecutor.SelectedAuthIndexMetadataKey)
	delete(metadata, coreexecutor.SelectedAuthProxyURLMetadataKey)
	delete(metadata, coreexecutor.SelectedAuthProviderMetadataKey)
	return nil
}

func realtimeGuardFailure(decision pluginapi.StreamCompletionInterceptResponse, fallback string) *interfaces.ErrorMessage {
	statusCode := decision.StatusCode
	if statusCode < http.StatusBadRequest || statusCode > 599 {
		statusCode = http.StatusBadGateway
	}
	message := strings.TrimSpace(decision.Error)
	if message == "" {
		message = strings.TrimSpace(decision.Reason)
	}
	if message == "" {
		message = fallback
	}
	return &interfaces.ErrorMessage{StatusCode: statusCode, Error: fmt.Errorf("%s", message)}
}

func attemptErrorMessage(attempt bufferedStreamAttempt) string {
	if attempt.Err != nil {
		return attempt.Err.Error()
	}
	return "实时降智守护拒绝输出当前响应"
}

func sendBufferedStreamPayload(ctx context.Context, dataChan chan<- []byte, payload []byte) bool {
	if ctx == nil {
		dataChan <- payload
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case dataChan <- payload:
		return true
	}
}

func sendBufferedStreamError(ctx context.Context, errChan chan<- *interfaces.ErrorMessage, message *interfaces.ErrorMessage) bool {
	if ctx == nil {
		errChan <- message
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case errChan <- message:
		return true
	}
}

func joinStreamPayloads(payloads [][]byte) []byte {
	var body []byte
	for _, payload := range payloads {
		body = append(body, payload...)
	}
	return body
}

func copyHeaderMap(destination, source http.Header) {
	for key := range destination {
		delete(destination, key)
	}
	for key, values := range source {
		destination[key] = append([]string(nil), values...)
	}
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
