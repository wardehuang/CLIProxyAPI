package executor

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const xAIAuthMissingProxyCooldown = 5 * time.Minute

type xAIAuthMissingProxyError struct{}

func (xAIAuthMissingProxyError) Error() string {
	return "xAI auth 缺少 proxy_url"
}

func (xAIAuthMissingProxyError) StatusCode() int {
	return http.StatusTooManyRequests
}

func (xAIAuthMissingProxyError) RetryAfter() *time.Duration {
	cooldown := xAIAuthMissingProxyCooldown
	return &cooldown
}

func (xAIAuthMissingProxyError) IsCredentialScoped() bool {
	return true
}

func requireXAIAuthProxyURL(auth *cliproxyauth.Auth) error {
	if auth == nil {
		return fmt.Errorf("xAI auth is nil")
	}
	proxyURL, err := cliproxyauth.ReadAuthProxyURLFromFile(auth)
	if err != nil {
		return fmt.Errorf("read xAI auth proxy_url: %w", err)
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return xAIAuthMissingProxyError{}
	}
	auth.ProxyURL = proxyURL
	return nil
}
