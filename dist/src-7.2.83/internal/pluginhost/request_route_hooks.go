package pluginhost

import (
	"bytes"
	"context"
	"reflect"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

func (h *Host) callRequestMetadataEnricher(ctx context.Context, pluginID string, enricher pluginapi.RequestMetadataEnricher, req pluginapi.RequestMetadataEnrichRequest) (out pluginapi.RequestMetadataEnrichResponse, ok bool) {
	if h == nil || enricher == nil || h.isPluginFused(pluginID) {
		return pluginapi.RequestMetadataEnrichResponse{}, false
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(pluginID, "RequestMetadataEnricher.EnrichRequestMetadata", recovered)
			out = pluginapi.RequestMetadataEnrichResponse{}
			ok = false
		}
	}()
	resp, errEnrich := enricher.EnrichRequestMetadata(ctx, req)
	if errEnrich != nil {
		log.Warnf("pluginhost: request metadata enricher %s failed: %v", pluginID, errEnrich)
		return pluginapi.RequestMetadataEnrichResponse{}, false
	}
	return resp, true
}

func (h *Host) callRouteRewriter(ctx context.Context, pluginID string, rewriter pluginapi.RouteRewriter, req pluginapi.RouteRewriteRequest) (out pluginapi.RouteRewriteResponse, ok bool) {
	if h == nil || rewriter == nil || h.isPluginFused(pluginID) {
		return pluginapi.RouteRewriteResponse{}, false
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(pluginID, "RouteRewriter.RewriteRoute", recovered)
			out = pluginapi.RouteRewriteResponse{}
			ok = false
		}
	}()
	resp, errRewrite := rewriter.RewriteRoute(ctx, req)
	if errRewrite != nil {
		log.Warnf("pluginhost: route rewriter %s failed: %v", pluginID, errRewrite)
		return pluginapi.RouteRewriteResponse{}, false
	}
	return resp, true
}

func (h *Host) EnrichRequestMetadata(ctx context.Context, req pluginapi.RequestMetadataEnrichRequest) pluginapi.RequestMetadataEnrichResponse {
	return h.EnrichRequestMetadataExcept(ctx, req, "")
}

func (h *Host) EnrichRequestMetadataExcept(ctx context.Context, req pluginapi.RequestMetadataEnrichRequest, skipPluginID string) pluginapi.RequestMetadataEnrichResponse {
	current := pluginapi.RequestMetadataEnrichResponse{Metadata: cloneInterceptorMetadata(req.Metadata)}
	skipPluginID = strings.TrimSpace(skipPluginID)
	for _, record := range h.Snapshot().records {
		enricher := record.plugin.Capabilities.RequestMetadataEnricher
		if h.isPluginFused(record.id) || enricher == nil || record.id == skipPluginID {
			continue
		}
		nextReq := req
		nextReq.Headers = cloneHeader(req.Headers)
		nextReq.Query = cloneValues(req.Query)
		nextReq.Body = bytes.Clone(req.Body)
		nextReq.Metadata = cloneInterceptorMetadata(current.Metadata)
		if resp, ok := h.callRequestMetadataEnricher(ctx, record.id, enricher, nextReq); ok {
			current.Metadata = mergePluginMetadata(current.Metadata, resp.Metadata, resp.ClearMetadata)
		}
	}
	return current
}

func (h *Host) RewriteRoute(ctx context.Context, req pluginapi.RouteRewriteRequest) pluginapi.RouteRewriteResponse {
	return h.RewriteRouteExcept(ctx, req, "")
}

func (h *Host) RewriteRouteExcept(ctx context.Context, req pluginapi.RouteRewriteRequest, skipPluginID string) pluginapi.RouteRewriteResponse {
	current := pluginapi.RouteRewriteResponse{
		RequestedModel:  req.RequestedModel,
		NormalizedModel: req.NormalizedModel,
		Providers:       cloneStringSlice(req.Providers),
		ResponseFormat:  req.ResponseFormat,
		Metadata:        cloneInterceptorMetadata(req.Metadata),
	}
	skipPluginID = strings.TrimSpace(skipPluginID)
	for _, record := range h.Snapshot().records {
		rewriter := record.plugin.Capabilities.RouteRewriter
		if h.isPluginFused(record.id) || rewriter == nil || record.id == skipPluginID {
			continue
		}
		nextReq := req
		nextReq.RequestedModel = current.RequestedModel
		nextReq.NormalizedModel = current.NormalizedModel
		nextReq.Providers = cloneStringSlice(current.Providers)
		nextReq.ResponseFormat = current.ResponseFormat
		nextReq.Headers = cloneHeader(req.Headers)
		nextReq.Query = cloneValues(req.Query)
		nextReq.Body = bytes.Clone(req.Body)
		nextReq.Metadata = cloneInterceptorMetadata(current.Metadata)
		if resp, ok := h.callRouteRewriter(ctx, record.id, rewriter, nextReq); ok {
			if strings.TrimSpace(resp.RequestedModel) != "" {
				current.RequestedModel = strings.TrimSpace(resp.RequestedModel)
			}
			if strings.TrimSpace(resp.NormalizedModel) != "" {
				current.NormalizedModel = strings.TrimSpace(resp.NormalizedModel)
			}
			if len(resp.Providers) > 0 {
				current.Providers = cloneStringSlice(resp.Providers)
			}
			if strings.TrimSpace(resp.ResponseFormat) != "" {
				current.ResponseFormat = strings.TrimSpace(resp.ResponseFormat)
			}
			current.Metadata = mergePluginMetadata(current.Metadata, resp.Metadata, resp.ClearMetadata)
		}
	}
	return current
}

func mergePluginMetadata(current, updates map[string]any, clear []string) map[string]any {
	out := cloneInterceptorMetadata(current)
	if out == nil && (len(updates) > 0 || len(clear) > 0) {
		out = make(map[string]any)
	}
	for _, key := range clear {
		delete(out, key)
	}
	for key, value := range updates {
		out[key] = cloneInterceptorMetadataAny(reflect.ValueOf(value), make(map[metadataCloneVisit]reflect.Value))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizeUsageMetadata(src map[string]any) map[string]any {
	return sanitizePluginMetadata(src)
}
