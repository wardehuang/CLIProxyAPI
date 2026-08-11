package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	xAIAuthProxyPollInterval = 3 * time.Second
	xAIAuthProxyWaitTimeout  = 5 * time.Minute
)

func waitForXAIAuthProxyURL(ctx context.Context, auth *cliproxyauth.Auth) error {
	if auth == nil {
		return fmt.Errorf("xAI auth is nil")
	}
	if strings.TrimSpace(auth.ProxyURL) != "" {
		return nil
	}

	deadline := time.NewTimer(xAIAuthProxyWaitTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(xAIAuthProxyPollInterval)
	defer poll.Stop()

	for {
		proxyURL, err := cliproxyauth.ReadAuthProxyURLFromFile(auth)
		if err != nil {
			return fmt.Errorf("xAI auth proxy_url wait failed: %w", err)
		}
		if strings.TrimSpace(proxyURL) != "" {
			auth.ProxyURL = strings.TrimSpace(proxyURL)
			return nil
		}

		// 实时智商探测或节点替换期间，守护插件可能短暂清空 auth 的 proxy_url。
		// 等待文件恢复有效代理，避免请求绕过节点代理而直连上游。
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("xAI auth proxy_url 等待超时")
		case <-poll.C:
		}
	}
}
