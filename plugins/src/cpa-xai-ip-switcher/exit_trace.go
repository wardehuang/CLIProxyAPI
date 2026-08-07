package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

const exitTraceURL = "http://163.192.9.157:2261/trace"

type exitTraceResponse struct {
	IP          string `json:"ip"`
	CountryCode string `json:"country_code"`
}

func probeExitTrace(ctx context.Context, client *http.Client) (exitIP, countryCode string, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, exitTraceURL, nil)
	if err != nil {
		return "", "", err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CPA-xai-ip-switcher/0.1.0")

	response, err := client.Do(request)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	if err != nil {
		return "", "", err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", "", fmt.Errorf("出口 IP 服务返回 HTTP %d", response.StatusCode)
	}

	var trace exitTraceResponse
	if err := json.Unmarshal(body, &trace); err != nil {
		return "", "", fmt.Errorf("出口 IP 响应无效")
	}
	trace.IP = strings.TrimSpace(trace.IP)
	trace.CountryCode = strings.ToUpper(strings.TrimSpace(trace.CountryCode))
	if net.ParseIP(trace.IP) == nil {
		return trace.IP, trace.CountryCode, fmt.Errorf("出口 IP 响应缺少有效 IP")
	}
	if trace.CountryCode == "" {
		return trace.IP, trace.CountryCode, fmt.Errorf("出口 IP 响应缺少国家代码")
	}
	return trace.IP, trace.CountryCode, nil
}
