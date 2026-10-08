package authjsonaudit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

const refreshProxyURLKey = "refresh_proxy_url"

type sourceContextKey struct{}

func WithSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, sourceContextKey{}, source)
}

func Source(ctx context.Context, fallback string) string {
	if source, ok := ctx.Value(sourceContextKey{}).(string); ok {
		return source
	}
	return fallback
}

func HasRefreshProxyURL(raw []byte) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}
	_, exists := object[refreshProxyURLKey]
	return exists
}

func HasRefreshProxyURLInMap(object map[string]any) bool {
	_, exists := object[refreshProxyURLKey]
	return exists
}

func LogRefreshProxyURLDrop(writer, path string, beforePresent, incomingPresent bool, writeErr error) {
	if !beforePresent {
		return
	}

	after, errRead := os.ReadFile(path)
	afterPresent := errRead == nil && HasRefreshProxyURL(after)
	if incomingPresent && errRead == nil && afterPresent {
		return
	}

	fileID := sha256.Sum256([]byte(filepath.Clean(path)))
	log.WithFields(log.Fields{
		"writer":                     writer,
		"auth_file_id":               hex.EncodeToString(fileID[:8]),
		"refresh_proxy_url_before":   beforePresent,
		"refresh_proxy_url_incoming": incomingPresent,
		"refresh_proxy_url_after":    afterPresent,
		"after_read_ok":              errRead == nil,
		"write_succeeded":            writeErr == nil,
	}).Warn("auth JSON save attempted to remove refresh_proxy_url")
}
