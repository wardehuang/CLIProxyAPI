package helps

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestResolveRefreshProxyURLForceDirect(t *testing.T) {
	t.Cleanup(func() { SetForceRefreshDirect(false) })

	SetForceRefreshDirect(false)
	if got := ResolveRefreshProxyURL("socks5://proxy.example:1080"); got != "socks5://proxy.example:1080" {
		t.Fatalf("ResolveRefreshProxyURL() = %q, want original proxy", got)
	}
	if got := ResolveRefreshProxyURL("  "); got != "" {
		t.Fatalf("ResolveRefreshProxyURL(blank) = %q, want empty", got)
	}

	SetForceRefreshDirect(true)
	if got := ResolveRefreshProxyURL("socks5://proxy.example:1080"); got != refreshDirectProxyURL {
		t.Fatalf("ResolveRefreshProxyURL() = %q, want %q", got, refreshDirectProxyURL)
	}
	if got := ResolveRefreshProxyURL(""); got != refreshDirectProxyURL {
		t.Fatalf("ResolveRefreshProxyURL(empty) = %q, want %q", got, refreshDirectProxyURL)
	}
}

func TestAuthForTokenRefreshForceDirect(t *testing.T) {
	t.Cleanup(func() { SetForceRefreshDirect(false) })

	original := &cliproxyauth.Auth{
		ID:       "auth-1",
		ProxyURL: "http://proxy.example:8080",
		Metadata: map[string]any{"access_token": "token"},
	}

	SetForceRefreshDirect(false)
	if got := AuthForTokenRefresh(original); got != original {
		t.Fatal("expected same auth pointer when force-direct is off")
	}

	SetForceRefreshDirect(true)
	got := AuthForTokenRefresh(original)
	if got == nil || got == original {
		t.Fatal("expected cloned auth when force-direct is on")
	}
	if got.ProxyURL != refreshDirectProxyURL {
		t.Fatalf("cloned ProxyURL = %q, want %q", got.ProxyURL, refreshDirectProxyURL)
	}
	if original.ProxyURL != "http://proxy.example:8080" {
		t.Fatalf("original ProxyURL mutated to %q", original.ProxyURL)
	}
}
