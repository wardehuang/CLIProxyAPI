package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

const (
	xAIProbeURL       = "https://grok.com/"
	probeHTTPTimeout  = 25 * time.Second
	probeDialTimeout  = 15 * time.Second
	probePollInterval = 750 * time.Millisecond
)

type probeResult struct {
	Success            bool
	LatencyMs          int64
	EndpointStatusCode int
	ExitIP             string
	CountryCode        string
	Reason             string
	Detail             string
	PreserveStatus     bool
}

func parseProxyLines(text string) ([]proxyNode, []inputLineError) {
	lines := strings.Split(text, "\n")
	nodes := make([]proxyNode, 0, len(lines))
	inputErrors := make([]inputLineError, 0)
	for lineIndex, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		node, err := parseProxyLine(line)
		if err != nil {
			inputErrors = append(inputErrors, inputLineError{Line: lineIndex + 1, Message: err.Error()})
			continue
		}
		nodes = append(nodes, node)
	}
	return nodes, inputErrors
}

func parseProxyLine(line string) (proxyNode, error) {
	if strings.Contains(line, ",") {
		return parseCSVProxyLine(line)
	}
	parsedURL, err := url.Parse(line)
	if err != nil {
		return proxyNode{}, fmt.Errorf("代理 URL 无效")
	}
	protocol := strings.ToLower(strings.TrimSpace(parsedURL.Scheme))
	if !isSupportedProxyProtocol(protocol) {
		return proxyNode{}, fmt.Errorf("协议必须是 http、https、socks5 或 socks5h")
	}
	if parsedURL.Host == "" || parsedURL.Hostname() == "" || parsedURL.Port() == "" {
		return proxyNode{}, fmt.Errorf("代理 URL 必须包含 host 和 port")
	}
	if (parsedURL.Path != "" && parsedURL.Path != "/") || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return proxyNode{}, fmt.Errorf("代理 URL 不支持路径、查询参数或片段")
	}
	port, err := strconv.Atoi(parsedURL.Port())
	if err != nil || port < 1 || port > 65535 {
		return proxyNode{}, fmt.Errorf("代理端口无效")
	}
	return proxyNode{
		Name:     proxyURLDisplayName(parsedURL),
		ProxyURL: parsedURL.String(),
		Host:     parsedURL.Hostname(),
		InputIP:  parsedURL.Hostname(),
		Port:     port,
		Protocol: protocol,
	}, nil
}

func proxyURLDisplayName(parsedURL *url.URL) string {
	name := parsedURL.Host
	if parsedURL.User != nil {
		name = parsedURL.User.String() + "@" + name
	}
	return name
}

func displayProxyNodeName(proxyURL, storedName string) string {
	parsedURL, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil || parsedURL.Host == "" {
		return storedName
	}
	return proxyURLDisplayName(parsedURL)
}

func parseCSVProxyLine(line string) (proxyNode, error) {
	fields := strings.Split(line, ",")
	if len(fields) != 4 && len(fields) != 5 {
		return proxyNode{}, fmt.Errorf("逗号格式必须是 host:port,ip,port,protocol[,domain]")
	}
	for index := range fields {
		fields[index] = strings.TrimSpace(fields[index])
	}
	hostPort := fields[0]
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil || host == "" {
		return proxyNode{}, fmt.Errorf("host:port 无效")
	}
	inputIP := net.ParseIP(fields[1])
	if inputIP == nil {
		return proxyNode{}, fmt.Errorf("ip 无效")
	}
	port, err := strconv.Atoi(fields[2])
	if err != nil || port < 1 || port > 65535 {
		return proxyNode{}, fmt.Errorf("port 无效")
	}
	protocol := strings.ToLower(fields[3])
	if !isSupportedProxyProtocol(protocol) {
		return proxyNode{}, fmt.Errorf("protocol 必须是 http、https、socks5 或 socks5h")
	}
	domain := ""
	if len(fields) == 5 {
		domain = fields[4]
	}
	return proxyNode{
		Name:     hostPort,
		ProxyURL: protocol + "://" + hostPort,
		Host:     host,
		InputIP:  inputIP.String(),
		Port:     port,
		Protocol: protocol,
		Domain:   domain,
	}, nil
}

func isSupportedProxyProtocol(protocol string) bool {
	switch protocol {
	case "http", "https", "socks5", "socks5h":
		return true
	default:
		return false
	}
}

func runProbeWorker(ctx context.Context, store *ipStore, probeRetryCount int) {
	for {
		if ctx.Err() != nil {
			return
		}
		node, err := store.claimNext()
		if err != nil {
			_ = store.appendLog(logLevelError, "probe.claim_failed", 0, "", "领取待探测节点失败", err.Error())
			if !waitForProbePoll(ctx) {
				return
			}
			continue
		}
		if node == nil {
			if !waitForProbePoll(ctx) {
				return
			}
			continue
		}

		result := probeNodeWithRetries(ctx, *node, probeRetryCount)
		if ctx.Err() != nil {
			if resetErr := store.resetProbe(*node); resetErr != nil {
				_ = store.appendProbeLog(logCategoryBatchProbe, node.BatchID, logStatusError, logLevelError, "probe.reset_failed", node.ID, node.Name, "取消探测后重置节点失败", resetErr.Error())
			}
			_ = store.appendProbeLog(logCategoryBatchProbe, node.BatchID, logStatusProbing, logLevelWarn, "probe.cancelled", node.ID, node.Name, "节点探测已取消", "插件配置变更或插件关闭中断探测")
			return
		}
		if err := store.completeProbe(*node, result); err != nil {
			if resetErr := store.resetProbe(*node); resetErr != nil {
				_ = store.appendProbeLog(logCategoryBatchProbe, node.BatchID, logStatusError, logLevelError, "probe.reset_failed", node.ID, node.Name, "保存探测结果后重置节点失败", resetErr.Error())
			}
			_ = store.appendProbeLog(logCategoryBatchProbe, node.BatchID, logStatusError, logLevelError, "probe.save_failed", node.ID, node.Name, "保存探测结果失败", err.Error())
		}
	}
}

func waitForProbePoll(ctx context.Context) bool {
	timer := time.NewTimer(probePollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func shouldProbeExitCountry(node proxyNode) bool {
	switch node.ProbeKind {
	case probeKindInitial:
		return true
	case probeKindRevive:
		return node.ExitCountry == ""
	default:
		return false
	}
}

func probeNodeWithRetries(ctx context.Context, node proxyNode, totalAttempts int) probeResult {
	client, err := newProxyHTTPClient(node.ProxyURL)
	if err != nil {
		return failedProbe("连接失败", err.Error())
	}

	var lastResult probeResult
	for attempt := 1; attempt <= totalAttempts; attempt++ {
		lastResult = probeGrokEndpoint(ctx, client)
		if !lastResult.Success {
			if ctx.Err() != nil {
				return lastResult
			}
			continue
		}

		if shouldProbeExitCountry(node) {
			exitIP, countryCode, traceErr := probeExitTrace(ctx, client)
			if traceErr != nil {
				return probeResult{
					Reason:             "出口IP探测失败",
					Detail:             truncateProbeDetail(traceErr.Error()),
					LatencyMs:          lastResult.LatencyMs,
					EndpointStatusCode: lastResult.EndpointStatusCode,
					ExitIP:             exitIP,
					CountryCode:        countryCode,
				}
			}
			if countryCode != "US" {
				return probeResult{
					Reason:             "非us出口",
					Detail:             fmt.Sprintf("出口 IP %s，国家代码 %s", exitIP, countryCode),
					LatencyMs:          lastResult.LatencyMs,
					EndpointStatusCode: lastResult.EndpointStatusCode,
					ExitIP:             exitIP,
					CountryCode:        countryCode,
				}
			}
			lastResult.ExitIP = exitIP
			lastResult.CountryCode = countryCode
		}

		if attempt > 1 {
			lastResult.Detail = fmt.Sprintf("第 %d/%d 次探测成功；%s", attempt, totalAttempts, lastResult.Detail)
		}
		return lastResult
	}
	lastResult.Detail = fmt.Sprintf("共探测 %d 次均失败；最后一次：%s", totalAttempts, lastResult.Detail)
	return lastResult
}

func probeGrokEndpoint(ctx context.Context, client *http.Client) probeResult {
	connectStart := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, xAIProbeURL, nil)
	if err != nil {
		return failedProbe("连接失败", err.Error())
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CPA-xai-ip-switcher/0.1.0")
	response, err := client.Do(request)
	latencyMs := time.Since(connectStart).Milliseconds()
	if err != nil {
		return probeResult{
			Reason:         "连接失败",
			Detail:         truncateProbeDetail(err.Error()),
			LatencyMs:      latencyMs,
			PreserveStatus: shouldPreserveKeepaliveStatus(err.Error()),
		}
	}
	endpointStatusCode := response.StatusCode
	io.Copy(io.Discard, io.LimitReader(response.Body, 2048))
	_ = response.Body.Close()
	if response.StatusCode == http.StatusProxyAuthRequired {
		return probeResult{
			Reason:             "连接失败",
			Detail:             "代理返回 HTTP 407，需要代理认证",
			LatencyMs:          latencyMs,
			EndpointStatusCode: endpointStatusCode,
		}
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return probeResult{
			Reason:             fmt.Sprintf("xAI endpoint HTTP %d", response.StatusCode),
			Detail:             "探测端点未返回 2xx 成功响应",
			LatencyMs:          latencyMs,
			EndpointStatusCode: endpointStatusCode,
		}
	}
	return probeResult{
		Success:            true,
		LatencyMs:          latencyMs,
		EndpointStatusCode: endpointStatusCode,
	}
}

func newProxyHTTPClient(proxyURL string) (*http.Client, error) {
	return newProxyHTTPClientWithTimeout(proxyURL, probeHTTPTimeout)
}

func newProxyHTTPClientWithTimeout(proxyURL string, timeout time.Duration) (*http.Client, error) {
	parsedURL, err := url.Parse(proxyURL)
	if err != nil || parsedURL.Host == "" {
		return nil, fmt.Errorf("代理 URL 无效")
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: probeDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   probeDialTimeout,
		ResponseHeaderTimeout: timeout,
		IdleConnTimeout:       30 * time.Second,
	}
	if parsedURL.Scheme == "http" || parsedURL.Scheme == "https" {
		transport.Proxy = http.ProxyURL(parsedURL)
	} else if parsedURL.Scheme == "socks5" || parsedURL.Scheme == "socks5h" {
		var authentication *proxy.Auth
		if parsedURL.User != nil {
			password, _ := parsedURL.User.Password()
			authentication = &proxy.Auth{User: parsedURL.User.Username(), Password: password}
		}
		socksDialer, err := proxy.SOCKS5("tcp", parsedURL.Host, authentication, &net.Dialer{Timeout: probeDialTimeout, KeepAlive: 30 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("创建 socks5 代理失败: %w", err)
		}
		transport.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
			return socksDialer.Dial(network, address)
		}
	} else {
		return nil, fmt.Errorf("不支持的代理协议 %s", parsedURL.Scheme)
	}
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}

func formatProbeResultDetail(result probeResult) string {
	details := make([]string, 0, 4)
	if result.EndpointStatusCode > 0 {
		details = append(details, fmt.Sprintf("xAI endpoint HTTP %d", result.EndpointStatusCode))
	}
	if result.LatencyMs > 0 {
		details = append(details, fmt.Sprintf("延迟 %d ms", result.LatencyMs))
	}
	if result.ExitIP != "" {
		details = append(details, fmt.Sprintf("出口 IP %s", result.ExitIP))
	}
	if result.CountryCode != "" {
		details = append(details, fmt.Sprintf("国家代码 %s", result.CountryCode))
	}
	if result.Detail != "" {
		details = append(details, result.Detail)
	}
	if len(details) == 0 {
		return "无更多详情"
	}
	return strings.Join(details, "；")
}

func shouldPreserveKeepaliveStatus(detail string) bool {
	normalizedDetail := strings.ToLower(detail)
	return strings.Contains(normalizedDetail, "not enough connections")
}

func failedProbe(reason, detail string) probeResult {
	return probeResult{Reason: reason, Detail: truncateProbeDetail(detail)}
}

func truncateProbeDetail(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 512 {
		return value
	}
	return value[:512] + "…"
}
