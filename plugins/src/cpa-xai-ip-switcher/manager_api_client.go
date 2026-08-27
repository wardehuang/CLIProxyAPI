package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const managerAccountAbnormalPriority = -8

const managerRealtimeDegradationEndpoint = "/v0/management/wxai-inspection/realtime-degradation"
const managerRealtimeHealthyEndpoint = "/v0/management/wxai-inspection/realtime-healthy"
const managerLatestScheduledRunEndpoint = "/v0/management/wxai-inspection/scheduled/latest-completed"

type managerAPIClient struct {
	baseURL       string
	managementKey string
	httpClient    *http.Client
}

type managerLatestScheduledRunResponse struct {
	Found bool `json:"found"`
	Run   struct {
		ID int64 `json:"id"`
	} `json:"run"`
}

type managerRealtimeDegradationRequest struct {
	AccountKey       string  `json:"accountKey"`
	FileName         string  `json:"fileName"`
	DisplayAccount   string  `json:"displayAccount"`
	AuthIndex        string  `json:"authIndex"`
	AccountID        string  `json:"accountId"`
	OriginalPriority *int    `json:"originalPriority,omitempty"`
	Reason           string  `json:"reason"`
	QualityLevel     string  `json:"qualityLevel"`
	TokensPerSecond  float64 `json:"tokensPerSecond"`
	RequestID        string  `json:"requestId"`
	ProxyURL         string  `json:"proxyUrl"`
}

type managerRealtimeHealthyRequest struct {
	AccountKey string `json:"accountKey"`
	FileName   string `json:"fileName"`
	AuthIndex  string `json:"authIndex"`
	AccountID  string `json:"accountId"`
}

type managerRealtimeHealthyResponse struct {
	Cleared bool `json:"cleared"`
}

func (controller *runtimeController) managerAPISettings() (string, string) {
	controller.mutex.RLock()
	defer controller.mutex.RUnlock()
	return controller.managerBaseURL, controller.managerManagementKey
}

func latestManagerScheduledInspectionRunID(managerBaseURL, managerManagementKey string) (int64, error) {
	client, err := newManagerAPIClient(managerBaseURL, managerManagementKey)
	if err != nil {
		return 0, err
	}
	request, err := client.newRequest(http.MethodGet, managerLatestScheduledRunEndpoint, nil)
	if err != nil {
		return 0, err
	}
	var response managerLatestScheduledRunResponse
	if err := client.execute(request, &response); err != nil {
		return 0, err
	}
	if !response.Found {
		return 0, fmt.Errorf("Manager 没有已完成的 xAi 服务器巡检 run")
	}
	return response.Run.ID, nil
}

func syncManagerRealtimeDegradation(managerBaseURL, managerManagementKey string, auth authFile, originalPriority *int, probe realtimeGuardProbe, decision realtimeGuardDecision) error {
	client, err := newManagerAPIClient(managerBaseURL, managerManagementKey)
	if err != nil {
		return err
	}
	accountKey, displayAccount, accountID := managerAccountIdentity(auth)
	payload := managerRealtimeDegradationRequest{
		AccountKey:       accountKey,
		FileName:         auth.Name,
		DisplayAccount:   displayAccount,
		AuthIndex:        auth.Index,
		AccountID:        accountID,
		OriginalPriority: originalPriority,
		Reason:           decision.Reason,
		QualityLevel:     string(decision.QualityLevel),
		TokensPerSecond:  decision.TPS,
		RequestID:        probe.RequestID,
		ProxyURL:         probe.ProxyURL,
	}
	request, err := client.newRequest(http.MethodPost, managerRealtimeDegradationEndpoint, payload)
	if err != nil {
		return err
	}
	return client.execute(request, nil)
}

func syncManagerRealtimeHealthy(managerBaseURL, managerManagementKey string, auth authFile) (bool, error) {
	client, err := newManagerAPIClient(managerBaseURL, managerManagementKey)
	if err != nil {
		return false, err
	}
	accountKey, _, accountID := managerAccountIdentity(auth)
	payload := managerRealtimeHealthyRequest{
		AccountKey: accountKey,
		FileName:   auth.Name,
		AuthIndex:  auth.Index,
		AccountID:  accountID,
	}
	request, err := client.newRequest(http.MethodPost, managerRealtimeHealthyEndpoint, payload)
	if err != nil {
		return false, err
	}
	var response managerRealtimeHealthyResponse
	if err := client.execute(request, &response); err != nil {
		return false, err
	}
	return response.Cleared, nil
}

func notifyManagerRealtimeHealthyAsync(probe realtimeGuardProbe) {
	go func() {
		shouldSync := false
		if _, err := pluginRuntime.withStore(func(store *ipStore) ([]byte, error) {
			marked, markedErr := store.hasRealtimeDegradedAuth(probe.AuthIndex)
			shouldSync = marked
			return nil, markedErr
		}); err != nil {
			logManagerRealtimeHealthyFailure(probe, err)
			return
		}
		if !shouldSync {
			return
		}
		auth, err := getAuthFile(probe.AuthIndex)
		if err != nil {
			logManagerRealtimeHealthyFailure(probe, err)
			return
		}
		managerBaseURL, managerManagementKey := pluginRuntime.managerAPISettings()
		cleared, syncErr := syncManagerRealtimeHealthy(managerBaseURL, managerManagementKey, auth)
		if syncErr != nil {
			logManagerRealtimeHealthyFailure(probe, syncErr)
			return
		}
		if !cleared {
			return
		}
		if _, clearErr := pluginRuntime.withStore(func(store *ipStore) ([]byte, error) {
			return nil, store.clearRealtimeDegradedAuth(probe.AuthIndex)
		}); clearErr != nil {
			logManagerRealtimeHealthyFailure(probe, clearErr)
		}
	}()
}

func logManagerRealtimeHealthyFailure(probe realtimeGuardProbe, err error) {
	_, _ = pluginRuntime.withStore(func(store *ipStore) ([]byte, error) {
		return nil, store.appendLog(
			logLevelWarn,
			"realtime_guard.manager_healthy_sync_failed",
			0,
			probe.ProxyURL,
			"实时正常结果已下发，但 Manager 连续降智次数清零通知失败",
			fmt.Sprintf("request_id=%s；auth_index=%s；error=%v", probe.RequestID, probe.AuthIndex, err),
		)
	})
}

func newManagerAPIClient(managerBaseURL, managerManagementKey string) (managerAPIClient, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(managerBaseURL), "/")
	parsedURL, err := url.Parse(baseURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return managerAPIClient{}, fmt.Errorf("Manager 地址必须是绝对 URL")
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return managerAPIClient{}, fmt.Errorf("Manager 地址必须以 http:// 或 https:// 开头")
	}
	if strings.TrimSpace(managerManagementKey) == "" {
		return managerAPIClient{}, fmt.Errorf("请先输入 Manager 管理密钥")
	}
	return managerAPIClient{
		baseURL:       baseURL,
		managementKey: managerManagementKey,
		httpClient:    &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (client managerAPIClient) newRequest(method, endpoint string, payload any) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		encodedPayload, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("编码 Manager 请求: %w", err)
		}
		body = bytes.NewReader(encodedPayload)
	}
	request, err := http.NewRequest(method, client.baseURL+endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("创建 Manager 请求: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+client.managementKey)
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func (client managerAPIClient) execute(request *http.Request, responsePayload any) error {
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("调用 Manager API: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("读取 Manager API 响应: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var errorPayload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &errorPayload) == nil && strings.TrimSpace(errorPayload.Error) != "" {
			return fmt.Errorf("Manager API HTTP %d: %s", response.StatusCode, errorPayload.Error)
		}
		return fmt.Errorf("Manager API HTTP %d", response.StatusCode)
	}
	if responsePayload == nil {
		return nil
	}
	if err := json.Unmarshal(body, responsePayload); err != nil {
		return fmt.Errorf("解析 Manager API 响应: %w", err)
	}
	return nil
}

func markRealtimeGuardAuthDegraded(probe realtimeGuardProbe) (authFile, *int, error) {
	if strings.TrimSpace(probe.AuthIndex) == "" {
		return authFile{}, nil, fmt.Errorf("实时守护缺少 auth_index，不能设置账号 priority=-8")
	}
	auth, err := getAuthFile(probe.AuthIndex)
	if err != nil {
		return authFile{}, nil, err
	}
	var originalPriority *int
	if auth.PrioritySet {
		priority := auth.Priority
		originalPriority = &priority
	}
	if auth.Priority != managerAccountAbnormalPriority {
		auth.Raw["priority"] = managerAccountAbnormalPriority
		if err := saveAuthFileDirect(auth); err != nil {
			return authFile{}, nil, fmt.Errorf("写入 xAI auth priority=-8: %w", err)
		}
	}
	return auth, originalPriority, nil
}

func managerAccountIdentity(auth authFile) (string, string, string) {
	displayAccount := strings.TrimSpace(auth.Email)
	if displayAccount == "" {
		displayAccount = auth.Name
	}
	accountID := firstManagerAuthString(auth.Raw, "account_id", "accountId", "sub", "subject", "user_id", "userId")
	return strings.Join([]string{auth.Name, displayAccount, auth.Index, accountID}, "|"), displayAccount, accountID
}

func firstManagerAuthString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(stringField(values, key)); value != "" {
			return value
		}
	}
	return ""
}
