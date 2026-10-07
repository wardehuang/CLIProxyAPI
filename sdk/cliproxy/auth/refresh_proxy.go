package auth

import "strings"

// RefreshProxyURL returns the refresh-specific proxy configured in auth metadata.
func (a *Auth) RefreshProxyURL() string {
	if a == nil || a.Metadata == nil {
		return ""
	}
	proxyURL, _ := a.Metadata["refresh_proxy_url"].(string)
	return strings.TrimSpace(proxyURL)
}

// TokenRefreshProxyURL returns the refresh-specific proxy, falling back to the auth proxy.
func (a *Auth) TokenRefreshProxyURL() string {
	if proxyURL := a.RefreshProxyURL(); proxyURL != "" {
		return proxyURL
	}
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.ProxyURL)
}
