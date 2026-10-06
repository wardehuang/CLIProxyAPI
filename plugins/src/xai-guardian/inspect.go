package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

const (
	xAIProbeURL      = "https://grok.com/"
	exitTraceURL     = "http://163.192.9.157:2261/trace"
	probeHTTPTimeout = 25 * time.Second
	probeDialTimeout = 15 * time.Second
)

var inspectionMutex sync.Mutex

type inputLineError struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

func parseProxyLines(text string) ([]proxyNode, []inputLineError) {
	nodes := make([]proxyNode, 0)
	errors := make([]inputLineError, 0)
	for lineIndex, rawLine := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		for _, raw := range strings.Split(rawLine, ";") {
			line := strings.TrimSpace(raw)
			if line == "" {
				continue
			}
			var node proxyNode
			var err error
			if strings.Contains(line, ",") {
				node, err = parseCSVProxyAddress(line)
			} else {
				node, err = parseProxyAddress(line)
			}
			if err != nil {
				errors = append(errors, inputLineError{Line: lineIndex + 1, Message: err.Error()})
				continue
			}
			nodes = append(nodes, node)
		}
	}
	return nodes, errors
}

func parseProxyAddress(raw string) (proxyNode, error) {
	address := strings.Trim(strings.TrimSpace(raw), "\"'")
	if !strings.Contains(address, "://") {
		address = "http://" + address
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return proxyNode{}, fmt.Errorf("invalid proxy address")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return proxyNode{}, fmt.Errorf("unsupported proxy protocol")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return proxyNode{}, fmt.Errorf("proxy address does not support path, query, or fragment")
	}
	if parsed.Port() == "" {
		return proxyNode{}, fmt.Errorf("proxy address must include a port")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return proxyNode{}, fmt.Errorf("invalid proxy port")
	}
	canonical := parsed.Scheme + "://"
	if parsed.User != nil {
		canonical += parsed.User.String() + "@"
	}
	canonical += parsed.Host
	return proxyNode{Address: canonical, Protocol: strings.ToLower(parsed.Scheme), Host: parsed.Hostname(), Port: port}, nil
}

func parseCSVProxyAddress(raw string) (proxyNode, error) {
	fields := strings.Split(raw, ",")
	if len(fields) == 3 {
		for index := range fields {
			fields[index] = strings.Trim(strings.TrimSpace(fields[index]), "\"'")
		}
		return parseProxyAddress(fields[2] + "://" + fields[0] + ":" + fields[1])
	}
	if len(fields) != 4 && len(fields) != 5 {
		return proxyNode{}, fmt.Errorf("CSV proxy format must be host:port,ip,port,protocol[,domain]")
	}
	for index := range fields {
		fields[index] = strings.TrimSpace(fields[index])
	}
	proxyURL, err := parseProxyAddress(fields[3] + "://" + fields[0])
	if err != nil {
		return proxyNode{}, err
	}
	if net.ParseIP(fields[1]) == nil {
		return proxyNode{}, fmt.Errorf("CSV proxy exit IP is invalid")
	}
	declaredPort, err := strconv.Atoi(fields[2])
	if err != nil || declaredPort < 1 || declaredPort > 65535 || declaredPort != proxyURL.Port {
		return proxyNode{}, fmt.Errorf("CSV proxy port is invalid")
	}
	return proxyURL, nil
}

func runInspection(ctx context.Context, store *guardianStore) error {
	inspectionMutex.Lock()
	defer inspectionMutex.Unlock()
	nodes, err := store.listNodes()
	if err != nil {
		return err
	}
	runID, err := store.startInspection(len(nodes))
	if err != nil {
		return err
	}
	var healthy, unhealthy int64
	for _, node := range nodes {
		select {
		case <-ctx.Done():
			_ = store.markInspectionFailed(runID, healthy, unhealthy, ctx.Err().Error())
			return ctx.Err()
		default:
		}
		result := inspectNode(ctx, node, runID)
		if result.Status == statusHealthy {
			healthy++
		} else {
			unhealthy++
		}
		if err := store.updateNodeResult(result); err != nil {
			_ = store.markInspectionFailed(runID, healthy, unhealthy, err.Error())
			return err
		}
	}
	if err := store.finishInspection(runID, healthy, unhealthy); err != nil {
		return err
	}
	inspectionAt := time.Now().UnixMilli()
	if len(nodes) > 0 {
		if entries, authErr := listXAIAuthEntries(); authErr == nil {
			if bindingErr := syncAuthBindings(store, entries, runID, inspectionAt); bindingErr != nil {
				_ = store.appendLog(logLevelWarn, "accounts.binding_failed", "账号绑定状态更新失败", bindingErr.Error())
			}
		} else {
			_ = store.appendLog(logLevelWarn, "accounts.list_failed", "无法读取 CPA xAI 账号状态", authErr.Error())
		}
	}
	return store.appendLog(logLevelInfo, "inspection.completed", "服务端巡检完成", fmt.Sprintf("总数 %d，健康 %d，异常 %d", len(nodes), healthy, unhealthy))
}

func inspectNode(ctx context.Context, node proxyNode, runID int64) inspectionResult {
	checkedAt := time.Now().UnixMilli()
	result := inspectionResult{RunID: runID, NodeID: node.ID, Status: statusUnhealthy, CheckedAt: checkedAt}
	client, err := newProxyHTTPClient(node.Address)
	if err != nil {
		result.Error = sanitizeLogText(err.Error())
		return result
	}
	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, xAIProbeURL, nil)
	if err != nil {
		result.Error = "create_probe_request"
		return result
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "xAI-Guardian/0.1.0")
	response, err := client.Do(request)
	if err != nil {
		result.LatencyMS = time.Since(started).Milliseconds()
		result.Error = sanitizeLogText(err.Error())
		return result
	}
	result.LatencyMS = time.Since(started).Milliseconds()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		result.Error = fmt.Sprintf("xai_http_%d", response.StatusCode)
		return result
	}
	result.ExitIP, result.Country, err = probeExitTrace(ctx, client)
	if err != nil {
		result.Error = sanitizeLogText(err.Error())
		return result
	}
	result.Status = statusHealthy
	return result
}

func newProxyHTTPClient(proxyURL string) (*http.Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid proxy address")
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: probeDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   probeDialTimeout,
		ResponseHeaderTimeout: probeHTTPTimeout,
		IdleConnTimeout:       30 * time.Second,
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
	case "socks5", "socks5h":
		var authentication *proxy.Auth
		if parsed.User != nil {
			password, _ := parsed.User.Password()
			authentication = &proxy.Auth{User: parsed.User.Username(), Password: password}
		}
		socksDialer, err := proxy.SOCKS5("tcp", parsed.Host, authentication, &net.Dialer{Timeout: probeDialTimeout, KeepAlive: 30 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("create socks5 proxy: %w", err)
		}
		transport.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
			return socksDialer.Dial(network, address)
		}
	default:
		return nil, fmt.Errorf("unsupported proxy protocol")
	}
	return &http.Client{Transport: transport, Timeout: probeHTTPTimeout}, nil
}

func probeExitTrace(ctx context.Context, client *http.Client) (string, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, exitTraceURL, nil)
	if err != nil {
		return "", "", err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "xAI-Guardian/0.1.0")
	response, err := client.Do(request)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 8192))
	if readErr != nil {
		return "", "", readErr
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", "", fmt.Errorf("exit trace service returned HTTP %d", response.StatusCode)
	}
	var identity struct {
		IP          string `json:"ip"`
		CountryCode string `json:"country_code"`
	}
	if json.Unmarshal(payload, &identity) != nil {
		return "", "", fmt.Errorf("exit trace response is invalid")
	}
	identity.IP = strings.TrimSpace(identity.IP)
	identity.CountryCode = strings.ToUpper(strings.TrimSpace(identity.CountryCode))
	if net.ParseIP(identity.IP) == nil {
		return identity.IP, identity.CountryCode, fmt.Errorf("exit trace response has no valid IP")
	}
	if identity.CountryCode == "" {
		return identity.IP, identity.CountryCode, fmt.Errorf("exit trace response has no country code")
	}
	return identity.IP, identity.CountryCode, nil
}
