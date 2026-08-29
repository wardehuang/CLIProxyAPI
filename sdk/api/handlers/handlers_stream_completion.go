package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/context"
)

const (
	maxRealtimeGuardDegradedAccounts      = 5
	realtimeGuardVirtualHeartbeatInterval = 10 * time.Second
)

type virtualResponsesStreamResponse struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	CreatedAt  int64  `json:"created_at"`
	Status     string `json:"status"`
	Background bool   `json:"background"`
	Error      any    `json:"error"`
	Output     []any  `json:"output"`
	Model      string `json:"model,omitempty"`
}

type virtualResponsesStreamEvent struct {
	Type           string                         `json:"type"`
	SequenceNumber int64                          `json:"sequence_number"`
	Response       virtualResponsesStreamResponse `json:"response"`
}

type virtualResponsesStreamHeartbeat struct {
	responseID   string
	model        string
	createdAt    int64
	nextSequence int64
	stopChan     chan struct{}
	doneChan     chan struct{}
	stopOnce     sync.Once
	started      bool
}

func newVirtualResponsesStreamHeartbeat(model string, lifecycle *requestLifecycleTracker) virtualResponsesStreamHeartbeat {
	return virtualResponsesStreamHeartbeat{
		responseID: "resp_" + lifecycle.requestID(),
		model:      model,
		createdAt:  time.Now().Unix(),
		stopChan:   make(chan struct{}),
		doneChan:   make(chan struct{}),
	}
}

func (h *virtualResponsesStreamHeartbeat) event(eventType string, sequenceNumber int64) []byte {
	payload := virtualResponsesStreamEvent{
		Type:           eventType,
		SequenceNumber: sequenceNumber,
		Response: virtualResponsesStreamResponse{
			ID:         h.responseID,
			Object:     "response",
			CreatedAt:  h.createdAt,
			Status:     "in_progress",
			Background: false,
			Error:      nil,
			Output:     []any{},
			Model:      h.model,
		},
	}
	data, _ := json.Marshal(payload)
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data))
}

func (h *virtualResponsesStreamHeartbeat) bootstrap(ctx context.Context, dataChan chan<- []byte) bool {
	for _, eventType := range []string{"response.created", "response.in_progress"} {
		if !sendBufferedStreamPayload(ctx, dataChan, h.event(eventType, h.nextSequence)) {
			return false
		}
		h.nextSequence++
	}
	h.started = true
	go h.run(ctx, dataChan)
	return true
}

func (h *virtualResponsesStreamHeartbeat) run(ctx context.Context, dataChan chan<- []byte) {
	defer close(h.doneChan)
	ticker := time.NewTicker(realtimeGuardVirtualHeartbeatInterval)
	defer ticker.Stop()
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	for {
		select {
		case <-h.stopChan:
			return
		case <-done:
			return
		case <-ticker.C:
			payload := h.event("response.in_progress", h.nextSequence)
			h.nextSequence++
			if !sendVirtualResponsesHeartbeatPayload(ctx, h.stopChan, dataChan, payload) {
				return
			}
		}
	}
}

func (h *virtualResponsesStreamHeartbeat) stopAndWait() {
	if !h.started {
		return
	}
	h.stopOnce.Do(func() {
		close(h.stopChan)
	})
	<-h.doneChan
}

func sendVirtualResponsesHeartbeatPayload(ctx context.Context, stop <-chan struct{}, dataChan chan<- []byte, payload []byte) bool {
	if ctx == nil {
		select {
		case <-stop:
			return false
		case dataChan <- payload:
			return true
		}
	}
	select {
	case <-stop:
		return false
	case <-ctx.Done():
		return false
	case dataChan <- payload:
		return true
	}
}

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
		virtualHeartbeat := newVirtualResponsesStreamHeartbeat(normalizedModel, lifecycle)
		defer virtualHeartbeat.stopAndWait()

		retryCount := 0
		currentResult := initialResult
		currentErr := initialErr
		attemptStartedAt := lifecycle.startedAt()
		if attemptStartedAt.IsZero() {
			attemptStartedAt = time.Now()
		}
		for {
			if ctx != nil && ctx.Err() != nil {
				completionOutcome = pluginapi.RequestCompletionCanceled
				completionStatus = 0
				completionErr = ctx.Err()
				return
			}
			if currentErr == nil && currentResult == nil {
				attemptStartedAt = time.Now()
				currentResult, currentErr = h.AuthManager.ExecuteStream(ctx, providers, req, opts)
			}
			mergeStreamAttemptAuthMetadata(opts.Metadata, currentResult)
			if responseProtocol == "openai-response" && strings.EqualFold(streamCompletionProvider(providers, opts.Metadata), "xai") && currentErr == nil && currentResult != nil && !virtualHeartbeat.started {
				if !virtualHeartbeat.bootstrap(ctx, dataChan) {
					completionOutcome = pluginapi.RequestCompletionCanceled
					completionStatus = 0
					if ctx != nil {
						completionErr = ctx.Err()
					}
					return
				}
			}

			attempt := collectBufferedStreamAttempt(ctx, interceptorHost, streamInterceptorsActive, responseProtocol, normalizedModel, originalRequestedModel, req, opts, currentResult, currentErr, attemptStartedAt, execOptions.SkipInterceptorPluginID)
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
					virtualHeartbeat.stopAndWait()
					completionOutcome = pluginapi.RequestCompletionFailed
					completionStatus = attempt.StatusCode
					completionErr = attempt.Err
					sendBufferedStreamError(ctx, errChan, &interfaces.ErrorMessage{StatusCode: attempt.StatusCode, Error: attempt.Err})
					return
				}
				virtualHeartbeat.stopAndWait()
				payloads := attempt.Payloads
				if virtualHeartbeat.started {
					payloads = rewriteVirtualResponsesReplay(attempt.Payloads, virtualHeartbeat.responseID, virtualHeartbeat.createdAt, &virtualHeartbeat.nextSequence)
				}
				for _, payload := range payloads {
					if !sendBufferedStreamPayload(ctx, dataChan, payload) {
						completionOutcome = pluginapi.RequestCompletionCanceled
						completionStatus = 0
						completionErr = ctx.Err()
						return
					}
				}
				return
			case pluginapi.StreamCompletionActionRetry:
				attemptedAccountCount := retryCount + 1
				if attemptedAccountCount >= maxRealtimeGuardDegradedAccounts {
					virtualHeartbeat.stopAndWait()
					failure := realtimeGuardFailure(decision, "xAI 流重试已处理 5 个账号")
					completionOutcome = pluginapi.RequestCompletionFailed
					completionStatus = failure.StatusCode
					completionErr = failure.Error
					sendBufferedStreamError(ctx, errChan, failure)
					return
				}
				retryCount++
				if errPrepare := h.prepareSelectedAuthForStreamRetry(ctx, opts.Metadata, decision.RetryMode); errPrepare != nil {
					virtualHeartbeat.stopAndWait()
					failure := realtimeGuardFailure(decision, errPrepare.Error())
					completionOutcome = pluginapi.RequestCompletionFailed
					completionStatus = failure.StatusCode
					completionErr = failure.Error
					sendBufferedStreamError(ctx, errChan, failure)
					return
				}
				attemptStartedAt = time.Now()
				currentResult, currentErr = h.AuthManager.ExecuteStream(ctx, providers, req, opts)
				continue
			case pluginapi.StreamCompletionActionFail:
				virtualHeartbeat.stopAndWait()
				failure := realtimeGuardFailure(decision, attemptErrorMessage(attempt))
				completionOutcome = pluginapi.RequestCompletionFailed
				completionStatus = failure.StatusCode
				completionErr = failure.Error
				sendBufferedStreamError(ctx, errChan, failure)
				return
			default:
				virtualHeartbeat.stopAndWait()
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
	startedAt time.Time,
	skipPluginID string,
) bufferedStreamAttempt {
	attempt := bufferedStreamAttempt{
		StartedAt:  startedAt,
		StatusCode: http.StatusOK,
		Err:        streamErr,
	}
	if attempt.StartedAt.IsZero() {
		attempt.StartedAt = time.Now()
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
	clearSelectedAuthMetadata(metadata)
	return nil
}

func (h *BaseAPIHandler) prepareSelectedAuthForStreamRetry(ctx context.Context, metadata map[string]any, retryMode pluginapi.StreamCompletionRetryMode) error {
	switch retryMode {
	case pluginapi.StreamCompletionRetryModeReloadSelectedAuth:
		return h.reloadSelectedAuthForStreamRetry(ctx, metadata)
	case pluginapi.StreamCompletionRetryModeExcludeSelectedAuth:
		return excludeSelectedAuthForStreamRetry(metadata)
	default:
		return fmt.Errorf("实时流重试模式无效: %q", retryMode)
	}
}

func excludeSelectedAuthForStreamRetry(metadata map[string]any) error {
	authID := metadataString(metadata, coreexecutor.SelectedAuthMetadataKey)
	if authID == "" {
		return fmt.Errorf("实时流换号重试缺少当前 auth")
	}
	excluded := make(map[string]struct{})
	if raw, exists := metadata[coreexecutor.ExcludedAuthIDsMetadataKey]; exists {
		authIDs, ok := raw.([]string)
		if !ok {
			return fmt.Errorf("实时流换号重试的 excluded_auth_ids 类型无效")
		}
		for _, excludedAuthID := range authIDs {
			excludedAuthID = strings.TrimSpace(excludedAuthID)
			if excludedAuthID != "" {
				excluded[excludedAuthID] = struct{}{}
			}
		}
	}
	excluded[authID] = struct{}{}
	authIDs := make([]string, 0, len(excluded))
	for excludedAuthID := range excluded {
		authIDs = append(authIDs, excludedAuthID)
	}
	sort.Strings(authIDs)
	metadata[coreexecutor.ExcludedAuthIDsMetadataKey] = authIDs
	clearSelectedAuthMetadata(metadata)
	return nil
}

func clearSelectedAuthMetadata(metadata map[string]any) {
	delete(metadata, coreexecutor.SelectedAuthMetadataKey)
	delete(metadata, coreexecutor.SelectedAuthIndexMetadataKey)
	delete(metadata, coreexecutor.SelectedAuthProxyURLMetadataKey)
	delete(metadata, coreexecutor.SelectedAuthProviderMetadataKey)
	delete(metadata, coreexecutor.SelectedAuthFileNameMetadataKey)
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

func rewriteVirtualResponsesReplay(payloads [][]byte, responseID string, createdAt int64, nextSequence *int64) [][]byte {
	rewritten := make([][]byte, 0, len(payloads))
	dropHandshakeData := false
	for _, payload := range payloads {
		trimmed := bytes.TrimSpace(payload)
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			eventType := strings.TrimSpace(string(trimmed[len("event:"):]))
			dropHandshakeData = eventType == "response.created" || eventType == "response.in_progress"
			if dropHandshakeData {
				continue
			}
			rewritten = append(rewritten, cloneBytes(payload))
			continue
		}
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			rewritten = append(rewritten, cloneBytes(payload))
			continue
		}
		data := bytes.TrimSpace(trimmed[len("data:"):])
		if dropHandshakeData {
			dropHandshakeData = false
			continue
		}
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !json.Valid(data) {
			rewritten = append(rewritten, cloneBytes(payload))
			continue
		}
		eventType := gjson.GetBytes(data, "type").String()
		if eventType == "response.created" || eventType == "response.in_progress" {
			continue
		}
		if gjson.GetBytes(data, "sequence_number").Exists() {
			data, _ = sjson.SetBytes(data, "sequence_number", *nextSequence)
			*nextSequence = *nextSequence + 1
		}
		if gjson.GetBytes(data, "response.id").Exists() {
			data, _ = sjson.SetBytes(data, "response.id", responseID)
		}
		if gjson.GetBytes(data, "response.created_at").Exists() {
			data, _ = sjson.SetBytes(data, "response.created_at", createdAt)
		}
		out := make([]byte, 0, len(data)+len("data: "))
		out = append(out, "data: "...)
		out = append(out, data...)
		rewritten = append(rewritten, out)
	}
	return rewritten
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
