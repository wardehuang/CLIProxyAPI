package helps

import (
	"strings"
	"sync/atomic"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const refreshDirectProxyURL = "direct"

var forceRefreshDirect atomic.Bool

// SetForceRefreshDirect enables or disables forced direct connectivity for OAuth token refresh.
// When enabled, refresh HTTP clients ignore auth-level and global proxy-url settings.
func SetForceRefreshDirect(enabled bool) {
	forceRefreshDirect.Store(enabled)
}

// ForceRefreshDirectEnabled reports whether OAuth token refresh must bypass all proxy-url settings.
func ForceRefreshDirectEnabled() bool {
	return forceRefreshDirect.Load()
}

// ResolveRefreshProxyURL returns the proxy URL that token refresh should use.
// When force-direct is enabled, it always returns "direct" so refresh ignores
// both auth.ProxyURL and cfg.ProxyURL fallbacks.
func ResolveRefreshProxyURL(authProxyURL string) string {
	if forceRefreshDirect.Load() {
		return refreshDirectProxyURL
	}
	return strings.TrimSpace(authProxyURL)
}

// AuthForTokenRefresh returns an auth suitable for token-refresh HTTP clients.
// When force-direct is enabled, the returned auth has ProxyURL set to "direct"
// so proxy-aware clients ignore global proxy-url. The original auth is never mutated.
func AuthForTokenRefresh(auth *cliproxyauth.Auth) *cliproxyauth.Auth {
	if auth == nil || !forceRefreshDirect.Load() {
		return auth
	}
	cloned := auth.Clone()
	if cloned == nil {
		return auth
	}
	cloned.ProxyURL = refreshDirectProxyURL
	return cloned
}
