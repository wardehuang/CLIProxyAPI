package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const xAIProbeURL = "https://grok.com/"

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
	return runAccountInspection(ctx, store, "scheduled")
}
