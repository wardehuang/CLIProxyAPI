package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	grok2apiHTTPTimeout          = 20 * time.Second
	grok2apiAuthLoginPath        = "/api/admin/v1/auth/login"
	grok2apiAutoProxySlotsPath   = "/api/admin/v1/cpa-auto-proxy/slots"
	grok2apiSyncTriggerKeepalive = "keepalive"
	grok2apiSyncTriggerManual    = "manual"
)

var grok2apiSyncMutex sync.Mutex

type grok2apiSlotPayload struct {
	Slot     int      `json:"slot"`
	IP       string   `json:"ip"`
	Accounts []string `json:"accounts"`
}

type grok2apiLoginResponse struct {
	Data struct {
		Tokens struct {
			AccessToken string `json:"accessToken"`
		} `json:"tokens"`
	} `json:"data"`
	Error *grok2apiAPIError `json:"error"`
}

type grok2apiAPIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
}

type grok2apiSyncResponse struct {
	Data struct {
		Results []grok2apiSlotResult `json:"results"`
	} `json:"data"`
	Error *grok2apiAPIError `json:"error"`
}

type grok2apiSlotResult struct {
	Slot             int      `json:"slot"`
	Action           string   `json:"action"`
	NodeName         string   `json:"nodeName"`
	NodeID           string   `json:"nodeId"`
	Assigned         int      `json:"assigned"`
	SkippedAccounts  []string `json:"skippedAccounts"`
	Error            string   `json:"error"`
}

type grok2apiSyncSummary struct {
	Trigger        string               `json:"trigger"`
	SlotCount      int                  `json:"slotCount"`
	FailedCount    int                  `json:"failedCount"`
	SkippedEmails  []string             `json:"skippedEmails"`
	Results        []grok2apiSlotResult `json:"results"`
	Message        string               `json:"message"`
}

func normalizeGrok2apiBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func parseSettingBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func formatSettingBool(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func validateGrok2apiConnectionSettings(settings pluginSettings) error {
	baseURL := normalizeGrok2apiBaseURL(settings.Grok2apiBaseUrl)
	if baseURL == "" {
		return fmt.Errorf("请先填写 grok2api 连接信息")
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return fmt.Errorf("grok2api 远端地址必须是绝对 URL")
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("grok2api 远端地址必须以 http:// 或 https:// 开头")
	}
	if strings.TrimSpace(settings.Grok2apiAdminUsername) == "" || settings.Grok2apiAdminPassword == "" {
		return fmt.Errorf("请先填写 grok2api 连接信息")
	}
	return nil
}

func buildGrok2apiHealthySlotPayloads(store *ipStore, healthySlotCount int) ([]grok2apiSlotPayload, error) {
	if healthySlotCount < 1 {
		return nil, fmt.Errorf("healthy slot count must be positive")
	}

	accountsBySlot := make(map[int64][]string, healthySlotCount)
	authFiles, err := listAuthFiles()
	if err != nil {
		if isHostAuthIndexUnavailable(err) {
			_ = store.appendLog(logLevelWarn, "grok2api.auth_list_deferred", 0, "", "Host Auth 暂未提供稳定 auth_index，本轮 grok2api 同步仅推送代理地址", err.Error())
		} else {
			return nil, fmt.Errorf("list xAI auth files for grok2api sync: %w", err)
		}
	} else {
		for fileIndex, auth := range authFiles {
			slotID := int64(fileIndex%healthySlotCount + 1)
			email := strings.TrimSpace(auth.Email)
			if email == "" {
				continue
			}
			accountsBySlot[slotID] = append(accountsBySlot[slotID], email)
		}
	}

	payloads := make([]grok2apiSlotPayload, 0, healthySlotCount)
	for slotID := int64(1); slotID <= int64(healthySlotCount); slotID++ {
		slot, found, findErr := store.findSlotByID(slotID)
		if findErr != nil {
			return nil, findErr
		}
		if !found || slot.Kind != statusHealthy {
			return nil, fmt.Errorf("healthy slot %d is unavailable for grok2api sync", slotID)
		}
		proxyURL, proxyErr := store.healthySlotProxyURL(slot)
		if proxyErr != nil {
			return nil, proxyErr
		}
		accounts := accountsBySlot[slotID]
		if accounts == nil {
			accounts = []string{}
		}
		payloads = append(payloads, grok2apiSlotPayload{
			Slot:     int(slotID),
			IP:       strings.TrimSpace(proxyURL),
			Accounts: accounts,
		})
	}
	return payloads, nil
}

func syncHealthySlotsToGrok2api(store *ipStore, trigger string) (grok2apiSyncSummary, error) {
	if !grok2apiSyncMutex.TryLock() {
		return grok2apiSyncSummary{}, fmt.Errorf("grok2api 同步正在进行中")
	}
	defer grok2apiSyncMutex.Unlock()

	settings, err := store.settings()
	if err != nil {
		return grok2apiSyncSummary{}, err
	}
	if !settings.Grok2apiSyncEnabled {
		summary := grok2apiSyncSummary{
			Trigger: trigger,
			Message: "grok2api 同步未启用",
		}
		_ = store.appendLog(logLevelInfo, "grok2api.sync_skipped", 0, "", "跳过 grok2api 同步", fmt.Sprintf("trigger=%s；同步未启用", trigger))
		return summary, nil
	}
	if err := validateGrok2apiConnectionSettings(settings); err != nil {
		_ = store.appendLog(logLevelWarn, "grok2api.sync_skipped", 0, "", "跳过 grok2api 同步", fmt.Sprintf("trigger=%s；%s", trigger, err.Error()))
		return grok2apiSyncSummary{}, err
	}

	payloads, err := buildGrok2apiHealthySlotPayloads(store, settings.HealthySlotCount)
	if err != nil {
		return grok2apiSyncSummary{}, err
	}
	if len(payloads) == 0 {
		return grok2apiSyncSummary{Trigger: trigger, Message: "无可同步健康槽位"}, nil
	}

	_ = store.appendLog(
		logLevelInfo,
		"grok2api.sync_started",
		0,
		"",
		"开始同步 grok2api 代理槽",
		fmt.Sprintf("trigger=%s；健康槽位数=%d", trigger, len(payloads)),
	)

	results, err := pushGrok2apiSlots(settings, payloads)
	if err != nil {
		_ = store.appendLog(logLevelError, "grok2api.sync_failed", 0, "", "grok2api 同步失败", fmt.Sprintf("trigger=%s；%s", trigger, err.Error()))
		return grok2apiSyncSummary{}, err
	}

	summary := summarizeGrok2apiResults(trigger, results)
	logLevel := logLevelInfo
	if summary.FailedCount > 0 || len(summary.SkippedEmails) > 0 {
		logLevel = logLevelWarn
	}
	_ = store.appendLog(logLevel, "grok2api.sync_completed", 0, "", "grok2api 同步完成", summary.Message)
	return summary, nil
}

func summarizeGrok2apiResults(trigger string, results []grok2apiSlotResult) grok2apiSyncSummary {
	failedCount := 0
	skippedEmails := make([]string, 0)
	detailParts := make([]string, 0)
	for _, result := range results {
		if result.Action == "failed" {
			failedCount++
			errorText := strings.TrimSpace(result.Error)
			if errorText == "" {
				errorText = "未知错误"
			}
			detailParts = append(detailParts, fmt.Sprintf("槽位 %d 失败：%s", result.Slot, errorText))
		}
		for _, email := range result.SkippedAccounts {
			email = strings.TrimSpace(email)
			if email == "" {
				continue
			}
			skippedEmails = append(skippedEmails, email)
		}
	}
	message := fmt.Sprintf("trigger=%s；槽位 %d；失败 %d；跳过邮箱 %d", trigger, len(results), failedCount, len(skippedEmails))
	if len(detailParts) > 0 {
		message += "；" + strings.Join(detailParts, "；")
	}
	if len(skippedEmails) > 0 {
		message += "；以下邮箱在 Grok Console 中不存在，已跳过：" + strings.Join(skippedEmails, ", ")
	}
	return grok2apiSyncSummary{
		Trigger:       trigger,
		SlotCount:     len(results),
		FailedCount:   failedCount,
		SkippedEmails: skippedEmails,
		Results:       results,
		Message:       message,
	}
}

func pushGrok2apiSlots(settings pluginSettings, payloads []grok2apiSlotPayload) ([]grok2apiSlotResult, error) {
	baseURL := normalizeGrok2apiBaseURL(settings.Grok2apiBaseUrl)
	accessToken, err := loginGrok2apiAdmin(baseURL, settings.Grok2apiAdminUsername, settings.Grok2apiAdminPassword)
	if err != nil {
		return nil, err
	}
	results, statusCode, apiError, err := postGrok2apiSlots(baseURL, accessToken, payloads)
	if err != nil {
		return nil, err
	}
	if statusCode == http.StatusUnauthorized {
		accessToken, err = loginGrok2apiAdmin(baseURL, settings.Grok2apiAdminUsername, settings.Grok2apiAdminPassword)
		if err != nil {
			return nil, err
		}
		results, statusCode, apiError, err = postGrok2apiSlots(baseURL, accessToken, payloads)
		if err != nil {
			return nil, err
		}
	}
	if statusCode != http.StatusOK {
		return nil, formatGrok2apiHTTPError(statusCode, apiError)
	}
	return results, nil
}

func loginGrok2apiAdmin(baseURL, username, password string) (string, error) {
	requestBody, err := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	if err != nil {
		return "", fmt.Errorf("encode grok2api login body: %w", err)
	}
	statusCode, responseBody, err := doGrok2apiJSONRequest(http.MethodPost, baseURL+grok2apiAuthLoginPath, "", requestBody)
	if err != nil {
		return "", fmt.Errorf("grok2api login request: %w", err)
	}
	var loginResponse grok2apiLoginResponse
	if len(responseBody) > 0 {
		if decodeErr := json.Unmarshal(responseBody, &loginResponse); decodeErr != nil {
			return "", fmt.Errorf("decode grok2api login response: %w", decodeErr)
		}
	}
	if statusCode != http.StatusOK {
		return "", formatGrok2apiHTTPError(statusCode, loginResponse.Error)
	}
	accessToken := strings.TrimSpace(loginResponse.Data.Tokens.AccessToken)
	if accessToken == "" {
		return "", fmt.Errorf("grok2api login succeeded without accessToken")
	}
	return accessToken, nil
}

func postGrok2apiSlots(baseURL, accessToken string, payloads []grok2apiSlotPayload) ([]grok2apiSlotResult, int, *grok2apiAPIError, error) {
	requestBody, err := json.Marshal(payloads)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encode grok2api slots body: %w", err)
	}
	statusCode, responseBody, err := doGrok2apiJSONRequest(http.MethodPost, baseURL+grok2apiAutoProxySlotsPath, accessToken, requestBody)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("grok2api slots request: %w", err)
	}
	var syncResponse grok2apiSyncResponse
	if len(responseBody) > 0 {
		if decodeErr := json.Unmarshal(responseBody, &syncResponse); decodeErr != nil {
			return nil, statusCode, nil, fmt.Errorf("decode grok2api slots response: %w", decodeErr)
		}
	}
	if statusCode != http.StatusOK {
		return nil, statusCode, syncResponse.Error, nil
	}
	return syncResponse.Data.Results, statusCode, syncResponse.Error, nil
}

func doGrok2apiJSONRequest(method, requestURL, accessToken string, body []byte) (int, []byte, error) {
	requestContext, cancel := context.WithTimeout(context.Background(), grok2apiHTTPTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if strings.TrimSpace(accessToken) != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, responseBody, nil
}

func formatGrok2apiHTTPError(statusCode int, apiError *grok2apiAPIError) error {
	if apiError != nil {
		code := strings.TrimSpace(apiError.Code)
		message := strings.TrimSpace(apiError.Message)
		switch code {
		case "invalidCredentials":
			return fmt.Errorf("管理员用户名或密码错误")
		case "loginRateLimited":
			return fmt.Errorf("登录过于频繁，请稍后重试")
		case "adminUnauthorized":
			return fmt.Errorf("管理员鉴权失败，请重新检查账号密码")
		}
		if message != "" && code != "" {
			return fmt.Errorf("grok2api HTTP %d：%s（%s）", statusCode, message, code)
		}
		if message != "" {
			return fmt.Errorf("grok2api HTTP %d：%s", statusCode, message)
		}
		if code != "" {
			return fmt.Errorf("grok2api HTTP %d：%s", statusCode, code)
		}
	}
	return fmt.Errorf("grok2api HTTP %d", statusCode)
}
