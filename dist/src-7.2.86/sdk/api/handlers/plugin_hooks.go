package handlers

import (
	"net/http"
	"net/url"
	"strings"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"golang.org/x/net/context"
)

type pluginRequestMetadataEnricherHost interface {
	EnrichRequestMetadata(context.Context, pluginapi.RequestMetadataEnrichRequest) pluginapi.RequestMetadataEnrichResponse
}

type pluginRequestMetadataEnricherSkipHost interface {
	EnrichRequestMetadataExcept(context.Context, pluginapi.RequestMetadataEnrichRequest, string) pluginapi.RequestMetadataEnrichResponse
}

type pluginRouteRewriterHost interface {
	RewriteRoute(context.Context, pluginapi.RouteRewriteRequest) pluginapi.RouteRewriteResponse
}

type pluginRouteRewriterSkipHost interface {
	RewriteRouteExcept(context.Context, pluginapi.RouteRewriteRequest, string) pluginapi.RouteRewriteResponse
}

type routeRewriteInput struct {
	EntryProtocol         string
	ResponseProtocol      string
	RequestedModel        string
	NormalizedModel       string
	Providers             []string
	Stream                bool
	Alt                   string
	Headers               http.Header
	Query                 url.Values
	Body                  []byte
	Metadata              map[string]any
	SkipInterceptorPlugin string
}

func (h *BaseAPIHandler) applyExecutionPluginHooks(ctx context.Context, entryProtocol, responseProtocol, modelName, normalizedModel string, providers []string, stream bool, alt string, headers http.Header, query url.Values, rawJSON []byte, metadata map[string]any, skipPluginID string) (string, string, []string, string, map[string]any) {
	metadata = h.enrichRequestMetadata(ctx, entryProtocol, modelName, stream, headers, query, rawJSON, metadata, skipPluginID)
	return h.rewriteRoute(ctx, routeRewriteInput{
		EntryProtocol:         entryProtocol,
		ResponseProtocol:      responseProtocol,
		RequestedModel:        modelName,
		NormalizedModel:       normalizedModel,
		Providers:             providers,
		Stream:                stream,
		Alt:                   alt,
		Headers:               headers,
		Query:                 query,
		Body:                  rawJSON,
		Metadata:              metadata,
		SkipInterceptorPlugin: skipPluginID,
	})
}

func (h *BaseAPIHandler) enrichRequestMetadata(ctx context.Context, sourceFormat, model string, stream bool, headers http.Header, query url.Values, body []byte, metadata map[string]any, skipPluginID string) map[string]any {
	host, ok := h.PluginHost.(pluginRequestMetadataEnricherHost)
	if !ok || host == nil {
		return metadata
	}
	req := pluginapi.RequestMetadataEnrichRequest{
		SourceFormat: sourceFormat,
		Model:        model,
		Stream:       stream,
		Headers:      cloneHeader(headers),
		Query:        cloneURLValues(query),
		Body:         cloneBytes(body),
		Metadata:     cloneMetadataMap(metadata),
	}
	var resp pluginapi.RequestMetadataEnrichResponse
	if skipPluginID != "" {
		if skipper, okSkip := h.PluginHost.(pluginRequestMetadataEnricherSkipHost); okSkip {
			resp = skipper.EnrichRequestMetadataExcept(ctx, req, skipPluginID)
		} else {
			resp = host.EnrichRequestMetadata(ctx, req)
		}
	} else {
		resp = host.EnrichRequestMetadata(ctx, req)
	}
	return mergeMetadataMap(metadata, resp.Metadata, resp.ClearMetadata)
}

func (h *BaseAPIHandler) rewriteRoute(ctx context.Context, in routeRewriteInput) (string, string, []string, string, map[string]any) {
	host, ok := h.PluginHost.(pluginRouteRewriterHost)
	if !ok || host == nil {
		return in.RequestedModel, in.NormalizedModel, in.Providers, in.ResponseProtocol, in.Metadata
	}
	req := pluginapi.RouteRewriteRequest{
		SourceFormat:    in.EntryProtocol,
		ResponseFormat:  in.ResponseProtocol,
		RequestedModel:  in.RequestedModel,
		NormalizedModel: in.NormalizedModel,
		Providers:       cloneStringSlice(in.Providers),
		Stream:          in.Stream,
		Alt:             in.Alt,
		Headers:         cloneHeader(in.Headers),
		Query:           cloneURLValues(in.Query),
		Body:            cloneBytes(in.Body),
		Metadata:        cloneMetadataMap(in.Metadata),
	}
	var resp pluginapi.RouteRewriteResponse
	if in.SkipInterceptorPlugin != "" {
		if skipper, okSkip := h.PluginHost.(pluginRouteRewriterSkipHost); okSkip {
			resp = skipper.RewriteRouteExcept(ctx, req, in.SkipInterceptorPlugin)
		} else {
			resp = host.RewriteRoute(ctx, req)
		}
	} else {
		resp = host.RewriteRoute(ctx, req)
	}
	requestedModel := in.RequestedModel
	if value := strings.TrimSpace(resp.RequestedModel); value != "" {
		requestedModel = value
	}
	normalizedModel := in.NormalizedModel
	if value := strings.TrimSpace(resp.NormalizedModel); value != "" {
		normalizedModel = value
	}
	providers := in.Providers
	if len(resp.Providers) > 0 {
		providers = cloneStringSlice(resp.Providers)
	}
	responseProtocol := in.ResponseProtocol
	if value := strings.TrimSpace(resp.ResponseFormat); value != "" {
		responseProtocol = value
	}
	metadata := mergeMetadataMap(in.Metadata, resp.Metadata, resp.ClearMetadata)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata[coreexecutor.RequestedModelMetadataKey] = requestedModel
	return requestedModel, normalizedModel, providers, responseProtocol, metadata
}

func cloneMetadataMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func mergeMetadataMap(current, updates map[string]any, clear []string) map[string]any {
	out := cloneMetadataMap(current)
	if out == nil && (len(updates) > 0 || len(clear) > 0) {
		out = make(map[string]any)
	}
	for _, key := range clear {
		delete(out, key)
	}
	for key, value := range updates {
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneStringSlice(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	return append([]string(nil), src...)
}
