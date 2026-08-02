package helps

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

const (
	proxyMetadataModeKey               = "cpa.proxy.mode"
	proxyMetadataSourceKey             = "cpa.proxy.source"
	proxyMetadataSchemeKey             = "cpa.proxy.scheme"
	proxyMetadataEgressIPKey           = "cpa.proxy.egress_ip"
	proxyMetadataEgressIPStatusKey     = "cpa.proxy.egress_ip_status"
	proxyMetadataEgressIPObservedKey   = "cpa.proxy.egress_ip_observed_at_ms"
	proxyMetadataObservationVersionKey = "cpa.proxy.observation_version"

	egressIPProbeURL           = "https://api.ipify.org"
	egressIPProbeTimeout       = 2 * time.Second
	egressIPCacheSuccessTTL    = 5 * time.Minute
	egressIPCacheFailureTTL    = 15 * time.Second
	egressIPStatusVerified     = "verified"
	egressIPStatusUnavailable  = "unavailable"
	egressIPStatusNotSupported = "not_supported"
)

type networkObservation struct {
	mode           string
	source         string
	scheme         string
	identity       string
	probeSupported bool
	probeTransport http.RoundTripper
}

type networkObservationProvider interface {
	NetworkObservation(*http.Request) networkObservation
}

type proxyObservationRoundTripper struct {
	base        http.RoundTripper
	observation networkObservation
}

func (t *proxyObservationRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req)
}

func (t *proxyObservationRoundTripper) NetworkObservation(req *http.Request) networkObservation {
	observation := t.observation
	if observation.mode == "" || (observation.mode == "unknown" && observation.source == "context") {
		observation = resolveNetworkObservation(t.base, req)
	}
	if observation.probeTransport == nil {
		observation.probeTransport = t.base
	}
	return observation
}

type egressIPCacheEntry struct {
	ip         string
	status     string
	observedAt time.Time
	ready      chan struct{}
	complete   bool
}

var egressIPCache = struct {
	sync.Mutex
	entries map[string]*egressIPCacheEntry
}{
	entries: make(map[string]*egressIPCacheEntry),
}

func contextRoundTripper(ctx context.Context) (http.RoundTripper, bool) {
	if ctx == nil {
		return nil, false
	}
	roundTripper, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	return roundTripper, ok && roundTripper != nil
}

func resolveConfiguredProxy(cfg *config.Config, auth *cliproxyauth.Auth) (string, string) {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL, "auth"
		}
	}
	if cfg != nil {
		if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" {
			return proxyURL, "global"
		}
	}
	return "", ""
}

func observationFromProxyURL(proxyURL, source string) networkObservation {
	setting, errParse := proxyutil.Parse(proxyURL)
	if errParse != nil {
		return networkObservation{
			mode:     "unknown",
			source:   "fallback",
			identity: source + "|invalid",
		}
	}

	observation := networkObservation{
		source:         source,
		probeSupported: true,
	}
	switch setting.Mode {
	case proxyutil.ModeDirect:
		observation.mode = "direct"
		observation.identity = source + "|direct"
	case proxyutil.ModeProxy:
		observation.mode = "proxy"
		observation.identity = source + "|" + proxyutil.Redact(proxyURL)
		observation.scheme = strings.ToLower(setting.URL.Scheme)
	default:
		observation.mode = "unknown"
		observation.source = "fallback"
		observation.identity = source + "|invalid"
		observation.probeSupported = false
	}
	return observation
}

func resolveEnvironmentObservation(upstreamURL string) networkObservation {
	requestURL := strings.TrimSpace(upstreamURL)
	if requestURL == "" {
		return networkObservation{
			mode:     "unknown",
			source:   "environment",
			identity: "environment",
		}
	}
	parsedURL, errParse := url.Parse(requestURL)
	if errParse != nil || parsedURL.Host == "" {
		return networkObservation{
			mode:     "unknown",
			source:   "environment",
			identity: "environment|invalid",
		}
	}
	request := &http.Request{
		Method: http.MethodGet,
		URL:    parsedURL,
	}
	proxyURL, errProxy := http.ProxyFromEnvironment(request)
	if errProxy != nil {
		return networkObservation{
			mode:     "unknown",
			source:   "environment",
			identity: "environment|error",
		}
	}
	if proxyURL == nil {
		directTransport := proxyutil.NewDirectTransport()
		return networkObservation{
			mode:           "direct",
			source:         "environment",
			identity:       "environment|direct",
			probeSupported: true,
			probeTransport: directTransport,
		}
	}
	probeTransport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL.String())
	if errBuild != nil || probeTransport == nil {
		return networkObservation{
			mode:           "proxy",
			source:         "environment",
			scheme:         strings.ToLower(proxyURL.Scheme),
			identity:       "environment|" + proxyutil.Redact(proxyURL.String()),
			probeSupported: false,
		}
	}
	return networkObservation{
		mode:           "proxy",
		source:         "environment",
		scheme:         strings.ToLower(proxyURL.Scheme),
		identity:       "environment|" + proxyutil.Redact(proxyURL.String()),
		probeSupported: true,
		probeTransport: probeTransport,
	}
}

func resolveNetworkObservation(roundTripper http.RoundTripper, req *http.Request) networkObservation {
	if provider, ok := roundTripper.(networkObservationProvider); ok {
		return provider.NetworkObservation(req)
	}
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}

	transport, ok := roundTripper.(*http.Transport)
	if !ok || transport == nil {
		return networkObservation{
			mode:     "unknown",
			source:   "context",
			identity: "context",
		}
	}
	if transport.Proxy == nil {
		return networkObservation{
			mode:           "direct",
			source:         "default",
			identity:       "direct",
			probeSupported: true,
			probeTransport: transport,
		}
	}
	if req == nil {
		return networkObservation{
			mode:     "unknown",
			source:   "environment",
			identity: "environment",
		}
	}

	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		return networkObservation{
			mode:     "unknown",
			source:   "environment",
			identity: "environment-error",
		}
	}
	if proxyURL == nil {
		return networkObservation{
			mode:           "direct",
			source:         "environment",
			identity:       "environment|direct",
			probeSupported: true,
			probeTransport: transport,
		}
	}
	return networkObservation{
		mode:           "proxy",
		source:         "environment",
		scheme:         strings.ToLower(proxyURL.Scheme),
		identity:       "environment|" + proxyutil.Redact(proxyURL.String()),
		probeSupported: true,
		probeTransport: transport,
	}
}

func (r *UsageReporter) usageMetadataSnapshot() map[string]any {
	if r == nil {
		return nil
	}
	r.waitForNetworkProbe()
	r.metadataMu.RLock()
	defer r.metadataMu.RUnlock()
	return cloneUsageMetadata(r.metadata)
}

func (r *UsageReporter) startNetworkProbe(
	ctx context.Context,
	identity string,
	roundTripper http.RoundTripper,
	cleanup func(),
) {
	if r == nil || strings.TrimSpace(identity) == "" || roundTripper == nil {
		return
	}
	r.networkProbeMu.Lock()
	if r.networkProbeStarted {
		r.networkProbeMu.Unlock()
		return
	}
	done := make(chan struct{})
	r.networkProbeStarted = true
	r.networkProbeDone = done
	r.networkProbeMu.Unlock()

	go func() {
		defer close(done)
		if cleanup != nil {
			defer cleanup()
		}
		ip, status, observedAt := lookupEgressIP(ctx, identity, roundTripper)
		r.setEgressIPResult(ip, status, observedAt)
	}()
}

func (r *UsageReporter) waitForNetworkProbe() {
	if r == nil {
		return
	}
	r.networkProbeMu.Lock()
	done := r.networkProbeDone
	r.networkProbeMu.Unlock()
	if done == nil {
		return
	}
	timer := time.NewTimer(egressIPProbeTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (r *UsageReporter) setNetworkObservation(observation networkObservation) {
	if r == nil {
		return
	}
	if observation.mode == "" {
		observation.mode = "unknown"
	}
	if observation.source == "" {
		observation.source = "unknown"
	}

	r.metadataMu.Lock()
	defer r.metadataMu.Unlock()
	if r.metadata == nil {
		r.metadata = make(map[string]any)
	}
	r.metadata[proxyMetadataModeKey] = observation.mode
	r.metadata[proxyMetadataSourceKey] = observation.source
	if observation.scheme != "" {
		r.metadata[proxyMetadataSchemeKey] = observation.scheme
	}
	r.metadata[proxyMetadataObservationVersionKey] = 1
	if !observation.probeSupported {
		r.metadata[proxyMetadataEgressIPStatusKey] = egressIPStatusNotSupported
		return
	}
	r.metadata[proxyMetadataEgressIPStatusKey] = "pending"
}

func (r *UsageReporter) setEgressIPResult(ip, status string, observedAt time.Time) {
	if r == nil {
		return
	}
	r.metadataMu.Lock()
	defer r.metadataMu.Unlock()
	if r.metadata == nil {
		r.metadata = make(map[string]any)
	}
	if ip != "" {
		r.metadata[proxyMetadataEgressIPKey] = ip
	}
	r.metadata[proxyMetadataEgressIPStatusKey] = status
	if !observedAt.IsZero() {
		r.metadata[proxyMetadataEgressIPObservedKey] = observedAt.UnixMilli()
	}
}

// SetProxyRoute records the route used by an upstream WebSocket connection.
func (r *UsageReporter) SetProxyRoute(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, upstreamURL string) {
	proxyURL, source := resolveConfiguredProxy(cfg, auth)
	observation := resolveEnvironmentObservation(upstreamURL)
	var probeTransport http.RoundTripper
	var closableTransport *http.Transport

	if proxyURL != "" {
		observation = observationFromProxyURL(proxyURL, source)
		if observation.probeSupported {
			transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
			if errBuild != nil || transport == nil {
				observation.mode = "unknown"
				observation.source = "fallback"
				observation.probeSupported = false
			} else {
				probeTransport = transport
				closableTransport = transport
			}
		}
	} else if observation.probeSupported {
		probeTransport = observation.probeTransport
		if transport, ok := probeTransport.(*http.Transport); ok {
			closableTransport = transport
		}
	}

	r.setNetworkObservation(observation)
	if probeTransport == nil || !observation.probeSupported {
		return
	}
	cleanup := func() {}
	if closableTransport != nil {
		cleanup = closableTransport.CloseIdleConnections
	}
	r.startNetworkProbe(ctx, observation.identity, probeTransport, cleanup)
}

// SetRelayRoute records that the provider request was executed by an external
// WebSocket relay whose public egress IP is outside the CPA process.
func (r *UsageReporter) SetRelayRoute() {
	r.setNetworkObservation(networkObservation{
		mode:   "relay",
		source: "websocket",
	})
}

func (r *UsageReporter) observeNetworkRequest(ctx context.Context, roundTripper http.RoundTripper, req *http.Request) {
	observation := resolveNetworkObservation(roundTripper, req)
	r.setNetworkObservation(observation)
	if !observation.probeSupported || observation.probeTransport == nil {
		return
	}
	r.startNetworkProbe(ctx, observation.identity, observation.probeTransport, nil)
}

func lookupEgressIP(ctx context.Context, identity string, roundTripper http.RoundTripper) (string, string, time.Time) {
	if strings.TrimSpace(identity) == "" || roundTripper == nil {
		return "", egressIPStatusNotSupported, time.Time{}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now()
	egressIPCache.Lock()
	if entry, ok := egressIPCache.entries[identity]; ok {
		if entry.complete {
			ttl := egressIPCacheFailureTTL
			if entry.status == egressIPStatusVerified {
				ttl = egressIPCacheSuccessTTL
			}
			if now.Sub(entry.observedAt) < ttl {
				ip, status, observedAt := entry.ip, entry.status, entry.observedAt
				egressIPCache.Unlock()
				return ip, status, observedAt
			}
		} else {
			ready := entry.ready
			egressIPCache.Unlock()
			select {
			case <-ready:
				return lookupEgressIP(ctx, identity, roundTripper)
			case <-ctx.Done():
				return "", egressIPStatusUnavailable, time.Time{}
			}
		}
	}
	entry := &egressIPCacheEntry{ready: make(chan struct{})}
	egressIPCache.entries[identity] = entry
	egressIPCache.Unlock()

	ip, status := queryEgressIP(ctx, roundTripper)
	observedAt := time.Now()
	egressIPCache.Lock()
	entry.ip = ip
	entry.status = status
	entry.observedAt = observedAt
	entry.complete = true
	close(entry.ready)
	egressIPCache.Unlock()
	return ip, status, observedAt
}

func queryEgressIP(ctx context.Context, roundTripper http.RoundTripper) (string, string) {
	probeContext, cancel := context.WithTimeout(ctx, egressIPProbeTimeout)
	defer cancel()
	request, errRequest := http.NewRequestWithContext(probeContext, http.MethodGet, egressIPProbeURL, nil)
	if errRequest != nil {
		return "", egressIPStatusUnavailable
	}
	client := &http.Client{
		Transport: roundTripper,
		Timeout:   egressIPProbeTimeout,
	}
	response, errDo := client.Do(request)
	if errDo != nil {
		return "", egressIPStatusUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", egressIPStatusUnavailable
	}
	body, errRead := io.ReadAll(io.LimitReader(response.Body, 128))
	if errRead != nil {
		return "", egressIPStatusUnavailable
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", egressIPStatusUnavailable
	}
	return ip, egressIPStatusVerified
}
