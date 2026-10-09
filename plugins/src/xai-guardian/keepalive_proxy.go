package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

const (
	exitTraceURL     = "http://163.192.9.157:2261/trace"
	probeHTTPTimeout = 25 * time.Second
	probeDialTimeout = 15 * time.Second
)

// newProxyHTTPClient is shared by the Guard's initial and keepalive node probes.
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

// probeExitTrace reports the egress identity used by the Guard node probes.
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
