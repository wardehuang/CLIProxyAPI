package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func applyRequestFinalizer(ctx context.Context, opts cliproxyexecutor.Options, toFormat sdktranslator.Format, model string, headers http.Header, body []byte) (http.Header, []byte) {
	if opts.RequestFinalizer == nil {
		return headers, body
	}
	requestedModel := metadataString(opts.Metadata, cliproxyexecutor.RequestedModelMetadataKey)
	if requestedModel == "" {
		requestedModel = model
	}
	resp := opts.RequestFinalizer(ctx, cliproxyexecutor.RequestFinalizeRequest{
		SourceFormat:   opts.SourceFormat,
		ToFormat:       toFormat,
		Model:          model,
		RequestedModel: requestedModel,
		Stream:         opts.Stream,
		Headers:        cloneFinalizerHeaders(headers),
		Body:           bytes.Clone(body),
		Metadata:       opts.Metadata,
	})
	headers = mergeFinalizerHeaders(headers, resp.Headers, resp.ClearHeaders)
	mergeFinalizerMetadata(opts.Metadata, resp.Metadata, resp.ClearMetadata)
	if len(resp.Body) > 0 {
		body = bytes.Clone(resp.Body)
	}
	return headers, body
}

func cloneFinalizerHeaders(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func mergeFinalizerHeaders(current, updates http.Header, clear []string) http.Header {
	if updates == nil && len(clear) == 0 {
		return current
	}
	out := cloneFinalizerHeaders(current)
	if out == nil {
		out = make(http.Header)
	}
	for _, key := range clear {
		deleteFinalizerHeader(out, key)
	}
	for key, values := range updates {
		deleteFinalizerHeader(out, key)
		out[key] = append([]string(nil), values...)
	}
	return out
}

func deleteFinalizerHeader(headers http.Header, key string) {
	if headers == nil || strings.TrimSpace(key) == "" {
		return
	}
	delete(headers, key)
	delete(headers, http.CanonicalHeaderKey(key))
	for existing := range headers {
		if strings.EqualFold(existing, key) {
			delete(headers, existing)
		}
	}
}

func mergeFinalizerMetadata(current, updates map[string]any, clear []string) {
	if current == nil {
		return
	}
	for _, key := range clear {
		delete(current, key)
	}
	for key, value := range updates {
		current[key] = value
	}
}

func setRequestBody(req *http.Request, body []byte) {
	if req == nil {
		return
	}
	req.Body = http.NoBody
	req.GetBody = nil
	req.ContentLength = 0
	if len(body) == 0 {
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))
}
