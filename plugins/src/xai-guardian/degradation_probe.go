package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	accountDegradationProbeURL             = "https://cli-chat-proxy.grok.com/v1/responses"
	accountDegradationProbePrompt          = "用中文回答：17 × 23 等于多少？只输出计算过程和答案。"
	accountDegradationProbeExpectedAnswer  = "391"
	accountDegradationProbeMaxOutputTokens = 96
	accountDegradationProbeTimeout         = 40 * time.Second
	accountDegradationProbeClientVersion   = "0.2.120" // Keep in sync with internal/runtime/executor/xai_executor.go.
	accountDegradationProbeReportBodyLimit = 64 * 1024
)

type accountDegradationProbeRequest struct {
	AuthIndex string `json:"authIndex"`
	Model     string `json:"model"`
}

type accountDegradationProbeTiming struct {
	TTFBMS            *int64  `json:"ttfbMs"`
	FirstGenerationMS *int64  `json:"firstGenerationMs"`
	GenerationMS      *int64  `json:"generationMs"`
	TotalMS           int64   `json:"totalMs"`
	TPS               float64 `json:"tps"`
}

type accountDegradationProbeMetrics struct {
	OutputTokens              int      `json:"outputTokens"`
	ReasoningTokens           int      `json:"reasoningTokens"`
	EvaluatedTokens           int      `json:"evaluatedTokens"`
	VisibleTokens             int      `json:"visibleTokens"`
	HasThinkingDelta          bool     `json:"hasThinkingDelta"`
	IsRealThinking            bool     `json:"isRealThinking"`
	ThinkingReason            string   `json:"thinkingReason"`
	CompletedToolCallEvidence bool     `json:"completedToolCallEvidence"`
	CompletedMutationEvidence bool     `json:"completedMutationEvidence"`
	BurstDump                 bool     `json:"burstDump"`
	SummaryChars              int      `json:"summaryChars"`
	EncryptedBytes            int      `json:"encryptedBytes"`
	EffectiveEncryptedFloor   int      `json:"effectiveEncryptedFloor"`
	ResponseType              string   `json:"responseType"`
	BodyChars                 int      `json:"bodyChars"`
	CompletedBodyCount        int      `json:"completedBodyCount"`
	CompletedToolCalls        int      `json:"completedToolCalls"`
	Tools                     []string `json:"tools"`
	Refusal                   bool     `json:"refusal"`
	VisibleDumpWindowMS       *int64   `json:"visibleDumpWindowMs"`
	AnswerMatchesExpected     bool     `json:"answerMatchesExpected"`
	ExpectedAnswer            string   `json:"expectedAnswer"`
	Answer                    string   `json:"answer"`
}

type accountDegradationProbeThresholds struct {
	SoftTPS                         *float64 `json:"softTps"`
	HardTPS                         *float64 `json:"hardTps"`
	TTFBSeconds                     float64  `json:"ttfbSeconds"`
	TTFBActive                      bool     `json:"ttfbActive"`
	GenerationSeconds               float64  `json:"generationSeconds"`
	GenerationActive                bool     `json:"generationActive"`
	MinOutputTokens                 int      `json:"minOutputTokens"`
	MinSummaryChars                 int      `json:"minSummaryChars"`
	MinEncryptedBytes               int      `json:"minEncryptedBytes"`
	EncryptedBytesPerReasoningToken int      `json:"encryptedBytesPerReasoningToken"`
	BurstMinReasoningTokens         int      `json:"burstMinReasoningTokens"`
	BurstMaxVisibleTokens           int      `json:"burstMaxVisibleTokens"`
	BurstMaxWindowMS                int      `json:"burstMaxWindowMs"`
}

type accountDegradationProbeResult struct {
	CheckID                 string                            `json:"checkId"`
	Account                 string                            `json:"account"`
	AuthIndex               string                            `json:"authIndex"`
	DetectedAt              time.Time                         `json:"detectedAt"`
	Endpoint                string                            `json:"endpoint"`
	Model                   string                            `json:"model"`
	Prompt                  string                            `json:"prompt"`
	Stream                  bool                              `json:"stream"`
	ProxySource             string                            `json:"proxySource"`
	ProxyAddress            string                            `json:"proxyAddress"`
	Classification          string                            `json:"classification"`
	Degraded                bool                              `json:"degraded"`
	Reason                  string                            `json:"reason"`
	FailureKind             string                            `json:"failureKind"`
	StatusCode              int                               `json:"statusCode"`
	Status                  string                            `json:"status"`
	ErrorCode               string                            `json:"errorCode"`
	ErrorText               string                            `json:"errorText"`
	TerminalEvent           bool                              `json:"terminalEvent"`
	Timing                  accountDegradationProbeTiming     `json:"timing"`
	Metrics                 accountDegradationProbeMetrics    `json:"metrics"`
	Thresholds              accountDegradationProbeThresholds `json:"thresholds"`
	RequestBody             string                            `json:"requestBody"`
	RequestHeaders          map[string][]string               `json:"requestHeaders"`
	UpstreamResponseHeaders map[string][]string               `json:"upstreamResponseHeaders"`
	UpstreamResponse        string                            `json:"upstreamResponse"`
	ResponseBodyTruncated   bool                              `json:"responseBodyTruncated"`
}

func accountDegradationProbeAPI(store *guardianStore, body []byte) (int, []byte, error) {
	var payload accountDegradationProbeRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		return jsonAPIError(http.StatusBadRequest, "invalid_request", "invalid request body")
	}
	payload.AuthIndex = strings.TrimSpace(payload.AuthIndex)
	payload.Model = strings.TrimSpace(payload.Model)
	if payload.AuthIndex == "" {
		return jsonAPIError(http.StatusBadRequest, "invalid_request", "authIndex is required")
	}
	if payload.Model == "" {
		return jsonAPIError(http.StatusBadRequest, "invalid_request", "model is required")
	}

	entry, err := findXAIAuthEntry(payload.AuthIndex)
	if err != nil {
		return jsonAPIError(http.StatusBadGateway, "auth_lookup_failed", sanitizeLogText(err.Error()))
	}
	file, err := getXAIAuthFile(entry)
	if err != nil {
		return jsonAPIError(http.StatusBadGateway, "auth_read_failed", sanitizeLogText(err.Error()))
	}
	accessToken := strings.TrimSpace(stringField(file.Raw, "access_token"))
	if accessToken == "" {
		return jsonAPIError(http.StatusUnprocessableEntity, "auth_token_missing", "xAI auth access_token is missing")
	}
	client, err := newAccountInspectionHTTPClient(file.ProxyURL)
	if err != nil {
		return jsonAPIError(http.StatusUnprocessableEntity, "auth_proxy_unavailable", sanitizeLogText(err.Error()))
	}
	defer client.CloseIdleConnections()

	settings, err := store.settings()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	result, err := runAccountDegradationProbe(client, file, payload.Model, accessToken, settings)
	if err != nil {
		return jsonAPIError(http.StatusInternalServerError, "probe_request_failed", sanitizeLogText(err.Error()))
	}
	return jsonAPIResult(result, nil)
}

type accountDegradationProbeExchange struct {
	CheckID                 string
	DetectedAt              time.Time
	RequestBody             string
	RequestHeaders          map[string][]string
	UpstreamResponseHeaders map[string][]string
	UpstreamResponse        string
	ResponseBodyTruncated   bool
}

func runAccountDegradationProbe(client *http.Client, file xaiAuthFile, model, accessToken string, settings pluginSettings) (accountDegradationProbeResult, error) {
	requestPayload := map[string]any{
		"model":             model,
		"input":             accountDegradationProbePrompt,
		"stream":            true,
		"reasoning":         map[string]string{"effort": "high", "summary": "detailed"},
		"max_output_tokens": accountDegradationProbeMaxOutputTokens,
		"temperature":       0,
	}
	requestBody, err := json.Marshal(requestPayload)
	if err != nil {
		return accountDegradationProbeResult{}, fmt.Errorf("encode xAI degradation probe request: %w", err)
	}
	var prettyRequestBody bytes.Buffer
	if err := json.Indent(&prettyRequestBody, requestBody, "", "  "); err != nil {
		return accountDegradationProbeResult{}, fmt.Errorf("format xAI degradation probe request: %w", err)
	}
	checkID, err := newAccountDegradationProbeCheckID()
	if err != nil {
		return accountDegradationProbeResult{}, fmt.Errorf("create xAI degradation probe check ID: %w", err)
	}

	startedAt := time.Now()
	requestContext, cancelRequest := context.WithTimeout(context.Background(), accountDegradationProbeTimeout)
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, accountDegradationProbeURL, bytes.NewReader(requestBody))
	if err != nil {
		return accountDegradationProbeResult{}, fmt.Errorf("create xAI degradation probe request: %w", err)
	}
	if err := applyAccountDegradationProbeHeaders(request, accessToken, file); err != nil {
		return accountDegradationProbeResult{}, err
	}
	var firstResponseByteUnixNano atomic.Int64
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() {
		firstResponseByteUnixNano.CompareAndSwap(0, time.Now().UnixNano())
	}}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	exchange := accountDegradationProbeExchange{
		CheckID:                 checkID,
		DetectedAt:              startedAt,
		RequestBody:             prettyRequestBody.String(),
		RequestHeaders:          redactedProbeHeaders(request.Header),
		UpstreamResponseHeaders: map[string][]string{},
	}

	response, err := client.Do(request)
	if err != nil {
		finishedAt := time.Now()
		completion := pluginapi.XAIStreamCompletionRequest{
			Provider:            "xai",
			Model:               model,
			RequestedModel:      model,
			OriginalRequest:     requestBody,
			RequestBody:         requestBody,
			Error:               sanitizeLogText(err.Error()),
			StartedAt:           startedAt,
			UpstreamStartedAt:   startedAt,
			FirstResponseByteAt: probeTraceTime(firstResponseByteUnixNano.Load()),
			FinishedAt:          finishedAt,
		}
		return buildAccountDegradationProbeResult(file, model, streamEvidence{}, completion, settings, exchange), nil
	}
	defer response.Body.Close()

	completion := pluginapi.XAIStreamCompletionRequest{
		Provider:            "xai",
		Model:               model,
		RequestedModel:      model,
		OriginalRequest:     requestBody,
		RequestBody:         requestBody,
		StatusCode:          response.StatusCode,
		ResponseHeaders:     response.Header.Clone(),
		StartedAt:           startedAt,
		UpstreamStartedAt:   startedAt,
		FirstResponseByteAt: probeTraceTime(firstResponseByteUnixNano.Load()),
	}
	exchange.UpstreamResponseHeaders = redactedProbeHeaders(response.Header)
	var responseBody []byte
	var firstVisibleAt time.Time
	var readErr error
	if response.StatusCode >= http.StatusBadRequest {
		responseBody, exchange.ResponseBodyTruncated, readErr = readAccountDegradationProbeReportBody(response.Body)
	} else {
		responseBody, firstVisibleAt, readErr = readAccountDegradationProbeStream(response.Body)
	}
	finishedAt := time.Now()
	completion.Body = responseBody
	completion.FirstVisibleAt = firstVisibleAt
	completion.FinishedAt = finishedAt
	evidence := parseStreamEvidence(responseBody)
	completion.Completed = evidence.TerminalEvent
	if readErr != nil {
		completion.Error = sanitizeLogText(readErr.Error())
	}
	if len([]rune(string(responseBody))) > accountDegradationProbeReportBodyLimit {
		exchange.ResponseBodyTruncated = true
	}
	exchange.UpstreamResponse = truncateInspectionText(sanitizeLogText(string(responseBody)), accountDegradationProbeReportBodyLimit)
	result := buildAccountDegradationProbeResult(file, model, evidence, completion, settings, exchange)
	if response.StatusCode >= http.StatusBadRequest {
		result.FailureKind = "request_error"
		result.ErrorCode, result.ErrorText = extractXAIAccountError(responseBody)
	} else if readErr != nil {
		result.FailureKind = "stream_error"
		result.ErrorText = sanitizeLogText(readErr.Error())
	}
	return result, nil
}

func applyAccountDegradationProbeHeaders(request *http.Request, accessToken string, file xaiAuthFile) error {
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connection", "Keep-Alive")
	request.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	request.Header.Set("x-grok-client-version", accountDegradationProbeClientVersion)
	request.Header.Set("User-Agent", "xai-grok-workspace/"+accountDegradationProbeClientVersion)
	request.Header.Set("x-grok-client-identifier", "grok-shell")
	request.Header.Set("x-authenticateresponse", "authenticate-response")
	return applyXAIAuthJSONHeaders(request, file.Raw)
}

func readAccountDegradationProbeStream(reader io.Reader) ([]byte, time.Time, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(nil, 52_428_800)
	var body bytes.Buffer
	var firstVisibleAt time.Time
	for scanner.Scan() {
		line := scanner.Bytes()
		if firstVisibleAt.IsZero() && accountDegradationProbeLineHasVisibleOutput(line) {
			firstVisibleAt = time.Now()
		}
		body.Write(line)
		body.WriteByte('\n')
		if accountDegradationProbeLineIsTerminal(line) {
			break
		}
	}
	return append([]byte(nil), body.Bytes()...), firstVisibleAt, scanner.Err()
}

func accountDegradationProbeLineIsTerminal(line []byte) bool {
	data := bytes.TrimSpace(line)
	if bytes.HasPrefix(data, []byte("data:")) {
		data = bytes.TrimSpace(data[len("data:"):])
	}
	object, ok := decodePayload(data).(map[string]any)
	if !ok {
		return false
	}
	switch stringValue(object["type"]) {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	default:
		return false
	}
}

func accountDegradationProbeLineHasVisibleOutput(line []byte) bool {
	data := bytes.TrimSpace(line)
	if bytes.HasPrefix(data, []byte("data:")) {
		data = bytes.TrimSpace(data[len("data:"):])
	}
	object, ok := decodePayload(data).(map[string]any)
	if !ok {
		return false
	}
	switch stringValue(object["type"]) {
	case "response.output_text.delta":
		return stringValue(object["delta"]) != ""
	case "response.output_text.done":
		return stringValue(object["text"]) != ""
	default:
		return false
	}
}

func buildAccountDegradationProbeResult(file xaiAuthFile, model string, evidence streamEvidence, completion pluginapi.XAIStreamCompletionRequest, settings pluginSettings, exchange accountDegradationProbeExchange) accountDegradationProbeResult {
	account := firstNonEmpty(file.Entry.Email, file.Entry.Label, file.Entry.Account, file.Entry.Name, file.Name, file.Index)
	proxySource := "direct"
	if strings.TrimSpace(file.ProxyURL) != "" {
		proxySource = "auth"
	}
	status := "请求错误"
	if completion.StatusCode > 0 {
		status = fmt.Sprintf("HTTP %d", completion.StatusCode)
	}
	result := accountDegradationProbeResult{
		CheckID:                 exchange.CheckID,
		Account:                 sanitizeLogText(account),
		AuthIndex:               file.Index,
		DetectedAt:              exchange.DetectedAt,
		Endpoint:                accountDegradationProbeURL,
		Model:                   model,
		Prompt:                  accountDegradationProbePrompt,
		Stream:                  true,
		ProxySource:             proxySource,
		ProxyAddress:            redactDegradationProbeProxyURL(file.ProxyURL),
		Classification:          "unknown",
		StatusCode:              completion.StatusCode,
		Status:                  status,
		TerminalEvent:           evidence.TerminalEvent,
		RequestBody:             exchange.RequestBody,
		RequestHeaders:          exchange.RequestHeaders,
		UpstreamResponseHeaders: exchange.UpstreamResponseHeaders,
		UpstreamResponse:        exchange.UpstreamResponse,
		ResponseBodyTruncated:   exchange.ResponseBodyTruncated,
		Thresholds: accountDegradationProbeThresholds{
			TTFBSeconds:                     settings.RealtimeGuardTTFBSeconds,
			TTFBActive:                      false,
			GenerationSeconds:               settings.RealtimeGuardGenerationSeconds,
			GenerationActive:                false,
			MinOutputTokens:                 settings.RealtimeGuardMinOutputTokens,
			MinSummaryChars:                 settings.RealtimeGuardMinSummaryChars,
			MinEncryptedBytes:               settings.RealtimeGuardMinEncryptedBytes,
			EncryptedBytesPerReasoningToken: settings.RealtimeGuardEncryptedBytesPerReasoningToken,
			BurstMinReasoningTokens:         settings.RealtimeGuardBurstMinReasoningTokens,
			BurstMaxVisibleTokens:           settings.RealtimeGuardBurstMaxVisibleTokens,
			BurstMaxWindowMS:                settings.RealtimeGuardBurstMaxWindowMS,
		},
	}
	if completion.Error != "" {
		result.FailureKind = "request_error"
		result.ErrorText = sanitizeLogText(completion.Error)
	} else if completion.StatusCode >= http.StatusBadRequest {
		result.FailureKind = "request_error"
	} else if !completion.Completed || evidence.StreamError != "" {
		result.FailureKind = "stream_error"
	}
	if result.FailureKind != "" {
		result.Reason = realtimeGuardFailureReason(completion)
	} else {
		classification := classifyRealtimeGuardEvidence(evidence, completion, settings)
		result.Classification = "normal"
		if classification.Degraded {
			result.Classification = "degraded"
		}
		result.Degraded = classification.Degraded
		result.Reason = classification.Reason
		result.Metrics.IsRealThinking = classification.IsRealThinking
		result.Metrics.CompletedToolCallEvidence = classification.CompletedToolCallEvidence
		result.Metrics.CompletedMutationEvidence = classification.CompletedMutationEvidence
		result.Metrics.BurstDump = classification.BurstDump
	}

	evaluatedTokens := evidence.OutputTokens + evidence.ReasoningTokens
	visibleTokens := evidence.OutputTokens - evidence.ReasoningTokens
	if visibleTokens < 0 {
		visibleTokens = 0
	}
	isRealThinking, thinkingReason := realtimeThinkingAssessment(evidence, completion, settings)
	if evidence.BurstDump || isBurstDump(evidence, completion, settings) {
		isRealThinking = true
		thinkingReason = "burst_dump"
	}
	if result.FailureKind == "" {
		isRealThinking = result.Metrics.IsRealThinking || result.Metrics.BurstDump
	}
	tools := append([]string{}, evidence.FunctionCallToolNames...)
	responseType := "empty"
	hasBody := len(completion.Body) > 0 || strings.TrimSpace(evidence.OutputText) != "" || evidence.CompletedMessage
	hasTools := evidence.CompletedFunctionCalls > 0
	switch {
	case hasBody && hasTools:
		responseType = "body_and_tools"
	case hasBody:
		responseType = "body"
	case hasTools:
		responseType = "tools"
	}
	completedBodies := 0
	if evidence.CompletedMessage {
		completedBodies = 1
	}
	result.Metrics.OutputTokens = evidence.OutputTokens
	result.Metrics.ReasoningTokens = evidence.ReasoningTokens
	result.Metrics.EvaluatedTokens = evaluatedTokens
	result.Metrics.VisibleTokens = visibleTokens
	result.Metrics.HasThinkingDelta = evidence.ReasoningDelta
	result.Metrics.IsRealThinking = isRealThinking
	result.Metrics.ThinkingReason = thinkingReason
	result.Metrics.SummaryChars = evidence.SummaryChars
	result.Metrics.EncryptedBytes = evidence.EncryptedBytes
	result.Metrics.EffectiveEncryptedFloor = effectiveRealtimeEncryptedFloor(evidence, settings)
	result.Metrics.ResponseType = responseType
	result.Metrics.BodyChars = len([]rune(evidence.OutputText))
	result.Metrics.CompletedBodyCount = completedBodies
	result.Metrics.CompletedToolCalls = evidence.CompletedFunctionCalls
	result.Metrics.Tools = tools
	result.Metrics.Refusal = evidence.RefusalDetected
	result.Metrics.VisibleDumpWindowMS = elapsedProbeMilliseconds(completion.FirstVisibleAt, completion.FinishedAt)
	result.Metrics.ExpectedAnswer = accountDegradationProbeExpectedAnswer
	result.Metrics.Answer = evidence.OutputText
	result.Metrics.AnswerMatchesExpected = accountDegradationProbeAnswerMatchesExpected(evidence.OutputText)
	result.Timing.TTFBMS = elapsedProbeMilliseconds(completion.StartedAt, completion.FirstResponseByteAt)
	result.Timing.FirstGenerationMS = elapsedProbeMilliseconds(completion.StartedAt, completion.FirstVisibleAt)
	result.Timing.GenerationMS = elapsedProbeMilliseconds(completion.FirstVisibleAt, completion.FinishedAt)
	if total := elapsedProbeMilliseconds(completion.StartedAt, completion.FinishedAt); total != nil {
		result.Timing.TotalMS = *total
	}
	if generationMS := result.Timing.GenerationMS; generationMS != nil && *generationMS > 0 {
		result.Timing.TPS = float64(evaluatedTokens) * 1000 / float64(*generationMS)
	}
	return result
}

func realtimeThinkingAssessment(evidence streamEvidence, completion pluginapi.XAIStreamCompletionRequest, settings pluginSettings) (bool, string) {
	if evidence.BurstDump || isBurstDump(evidence, completion, settings) {
		return true, "burst_dump"
	}
	if evidence.RefusalDetected {
		return false, "refusal_detected"
	}
	if evidence.OutputTokens < settings.RealtimeGuardMinOutputTokens {
		return true, "below_minimum_output_tokens"
	}
	encryptedFloor := effectiveRealtimeEncryptedFloor(evidence, settings)
	hasSummaryEvidence := !evidence.ReasoningMetadataError && evidence.SummaryChars >= settings.RealtimeGuardMinSummaryChars && !isPlaceholderSummary(evidence.SummaryText)
	if hasSummaryEvidence {
		return true, "summary_evidence"
	}
	hasEncryptedEvidence := !evidence.ReasoningMetadataError && evidence.ReasoningItemCompleted && evidence.EncryptedBytes >= encryptedFloor
	if hasEncryptedEvidence {
		return true, "encrypted_content_evidence"
	}
	if evidence.ReasoningMetadataError {
		return false, "reasoning_metadata_error"
	}
	if evidence.SummaryChars > 0 {
		return false, "summary_below_minimum_chars"
	}
	if evidence.EncryptedBytes > 0 {
		return false, "encrypted_below_effective_floor"
	}
	return false, "missing_thinking_evidence"
}

func effectiveRealtimeEncryptedFloor(evidence streamEvidence, settings pluginSettings) int {
	floor := settings.RealtimeGuardMinEncryptedBytes
	calculated := evidence.ReasoningTokens * settings.RealtimeGuardEncryptedBytesPerReasoningToken
	if calculated > floor {
		return calculated
	}
	return floor
}

func elapsedProbeMilliseconds(startedAt, finishedAt time.Time) *int64 {
	if startedAt.IsZero() || finishedAt.IsZero() {
		return nil
	}
	elapsed := finishedAt.Sub(startedAt).Milliseconds()
	if elapsed < 0 {
		return nil
	}
	return &elapsed
}

func probeTraceTime(unixNano int64) time.Time {
	if unixNano == 0 {
		return time.Time{}
	}
	return time.Unix(0, unixNano)
}

func newAccountDegradationProbeCheckID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

func redactDegradationProbeProxyURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[REDACTED]"
	}
	if parsed.User != nil {
		parsed.User = url.User("redacted")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func redactedProbeHeaders(headers http.Header) map[string][]string {
	redacted := make(map[string][]string, len(headers))
	for name, values := range headers {
		canonicalName := http.CanonicalHeaderKey(name)
		if isSensitiveDegradationProbeHeader(canonicalName) {
			redacted[canonicalName] = []string{"[REDACTED]"}
			continue
		}
		cleanValues := make([]string, len(values))
		for index, value := range values {
			cleanValues[index] = sanitizeLogText(value)
		}
		redacted[canonicalName] = cleanValues
	}
	return redacted
}

func isSensitiveDegradationProbeHeader(name string) bool {
	lowerName := strings.ToLower(name)
	if lowerName == "www-authenticate" || lowerName == "x-xai-token-auth" {
		return false
	}
	if lowerName == "authorization" || lowerName == "proxy-authorization" || lowerName == "cookie" || lowerName == "set-cookie" {
		return true
	}
	return strings.Contains(lowerName, "token") || strings.Contains(lowerName, "secret") || strings.Contains(lowerName, "password") || strings.Contains(lowerName, "api-key") || strings.Contains(lowerName, "credential") || strings.Contains(lowerName, "auth") || strings.Contains(lowerName, "session")
}

func readAccountDegradationProbeReportBody(reader io.Reader) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(reader, accountDegradationProbeReportBodyLimit+1))
	if len(body) > accountDegradationProbeReportBodyLimit {
		return body[:accountDegradationProbeReportBodyLimit], true, err
	}
	return body, false, err
}

func accountDegradationProbeAnswerMatchesExpected(answer string) bool {
	for _, number := range strings.FieldsFunc(answer, func(value rune) bool {
		return value < '0' || value > '9'
	}) {
		if number == accountDegradationProbeExpectedAnswer {
			return true
		}
	}
	return false
}
