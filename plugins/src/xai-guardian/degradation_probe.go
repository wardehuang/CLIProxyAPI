package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
)

type accountDegradationProbeRequest struct {
	AuthIndex string `json:"authIndex"`
	Model     string `json:"model"`
}

type accountDegradationProbeResult struct {
	AuthIndex                 string `json:"authIndex"`
	Model                     string `json:"model"`
	Prompt                    string `json:"prompt"`
	Classification            string `json:"classification"`
	Degraded                  bool   `json:"degraded"`
	Reason                    string `json:"reason"`
	Answer                    string `json:"answer"`
	ExpectedAnswer            string `json:"expectedAnswer"`
	AnswerMatchesExpected     bool   `json:"answerMatchesExpected"`
	StatusCode                int    `json:"statusCode"`
	TerminalEvent             bool   `json:"terminalEvent"`
	OutputTokens              int    `json:"outputTokens"`
	ReasoningTokens           int    `json:"reasoningTokens"`
	SummaryChars              int    `json:"summaryChars"`
	EncryptedBytes            int    `json:"encryptedBytes"`
	IsRealThinking            bool   `json:"isRealThinking"`
	CompletedToolCallEvidence bool   `json:"completedToolCallEvidence"`
	CompletedMutationEvidence bool   `json:"completedMutationEvidence"`
	BurstDump                 bool   `json:"burstDump"`
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

	response, err := client.Do(request)
	if err != nil {
		completion := pluginapi.XAIStreamCompletionRequest{
			Provider:          "xai",
			Model:             model,
			RequestedModel:    model,
			OriginalRequest:   requestBody,
			RequestBody:       requestBody,
			Error:             sanitizeLogText(err.Error()),
			StartedAt:         startedAt,
			UpstreamStartedAt: startedAt,
			FinishedAt:        time.Now(),
		}
		return buildAccountDegradationProbeResult(file.Index, model, streamEvidence{}, completion, settings), nil
	}
	defer response.Body.Close()

	completion := pluginapi.XAIStreamCompletionRequest{
		Provider:          "xai",
		Model:             model,
		RequestedModel:    model,
		OriginalRequest:   requestBody,
		RequestBody:       requestBody,
		StatusCode:        response.StatusCode,
		StartedAt:         startedAt,
		UpstreamStartedAt: startedAt,
	}
	if response.StatusCode >= http.StatusBadRequest {
		completion.FinishedAt = time.Now()
		return buildAccountDegradationProbeResult(file.Index, model, streamEvidence{}, completion, settings), nil
	}

	streamBody, firstVisibleAt, readErr := readAccountDegradationProbeStream(response.Body)
	finishedAt := time.Now()
	evidence := parseStreamEvidence(streamBody)
	completion.Body = streamBody
	completion.Completed = evidence.TerminalEvent
	completion.FirstVisibleAt = firstVisibleAt
	completion.FinishedAt = finishedAt
	if readErr != nil {
		completion.Error = sanitizeLogText(readErr.Error())
	}
	return buildAccountDegradationProbeResult(file.Index, model, evidence, completion, settings), nil
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

	rawHeaders, exists := file.Raw["headers"]
	if !exists || rawHeaders == nil {
		return nil
	}
	headers, ok := rawHeaders.(map[string]any)
	if !ok {
		return fmt.Errorf("xAI auth headers must be an object")
	}
	for name, rawValue := range headers {
		headerName := strings.TrimSpace(name)
		if headerName == "" || rawValue == nil {
			continue
		}
		headerValue, ok := rawValue.(string)
		if !ok {
			return fmt.Errorf("xAI auth header %q must be a string", headerName)
		}
		headerValue = strings.TrimSpace(headerValue)
		if headerValue == "" {
			continue
		}
		if strings.HasPrefix(headerValue, "$") || strings.Contains(strings.ToUpper(headerValue), "$CPA-SESSION-ID") {
			return fmt.Errorf("xAI auth header %q requires request context", headerName)
		}
		if http.CanonicalHeaderKey(headerName) == "Host" {
			request.Host = headerValue
		}
		request.Header.Set(headerName, headerValue)
	}
	return nil
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

func buildAccountDegradationProbeResult(authIndex, model string, evidence streamEvidence, completion pluginapi.XAIStreamCompletionRequest, settings pluginSettings) accountDegradationProbeResult {
	result := accountDegradationProbeResult{
		AuthIndex:             authIndex,
		Model:                 model,
		Prompt:                accountDegradationProbePrompt,
		Answer:                evidence.OutputText,
		ExpectedAnswer:        accountDegradationProbeExpectedAnswer,
		AnswerMatchesExpected: accountDegradationProbeAnswerMatchesExpected(evidence.OutputText),
		StatusCode:            completion.StatusCode,
		TerminalEvent:         evidence.TerminalEvent,
		OutputTokens:          evidence.OutputTokens,
		ReasoningTokens:       evidence.ReasoningTokens,
		SummaryChars:          evidence.SummaryChars,
		EncryptedBytes:        evidence.EncryptedBytes,
	}
	if completion.Error != "" || completion.StatusCode >= http.StatusBadRequest || !completion.Completed || evidence.StreamError != "" {
		result.Classification = "unknown"
		result.Reason = realtimeGuardFailureReason(completion)
		return result
	}

	classification := classifyRealtimeGuardEvidence(evidence, completion, settings)
	result.Classification = "normal"
	if classification.Degraded {
		result.Classification = "degraded"
	}
	result.Degraded = classification.Degraded
	result.Reason = classification.Reason
	result.IsRealThinking = classification.IsRealThinking
	result.CompletedToolCallEvidence = classification.CompletedToolCallEvidence
	result.CompletedMutationEvidence = classification.CompletedMutationEvidence
	result.BurstDump = classification.BurstDump
	return result
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
