package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	visibleFilesBlockPattern    = regexp.MustCompile(`(?is)<visible_files\b[^>]*>.*?</visible_files\s*>`)
	visibleFilesFilePathPattern = regexp.MustCompile(`(?is)<file\b[^>]*\bpath\s*=\s*"([^"]+)"[^>]*>`)
)

const strippedVisibleFilesPlaceholder = "[cpa-strip-visible-files: removed visible_files]"

type strippedFileInfo struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

type stripResult struct {
	Changed       bool
	BeforeBody    []byte
	AfterBody     []byte
	BeforeBytes   int
	AfterBytes    int
	RemovedBytes  int
	RemovedBlocks int
	RemovedFiles  []strippedFileInfo
}

func interceptRequestBeforeAuth(ctx context.Context, req pluginapi.RequestInterceptRequest, hostCallbackID string) pluginapi.RequestInterceptResponse {
	_ = ctx
	if len(bytes.TrimSpace(req.Body)) == 0 {
		return pluginapi.RequestInterceptResponse{}
	}
	result, errStrip := stripVisibleFilesFromRequestBody(req.Body)
	if errStrip != nil {
		logPluginDebug(hostCallbackID, "visible_files strip skipped", map[string]any{
			"reason": "strip_error",
			"error":  errStrip.Error(),
			"model":  req.Model,
		})
		return pluginapi.RequestInterceptResponse{}
	}
	if !result.Changed {
		logPluginDebug(hostCallbackID, "visible_files strip skipped", map[string]any{
			"reason":        "no_visible_files",
			"model":         req.Model,
			"source_format": req.SourceFormat,
			"body_bytes":    result.BeforeBytes,
		})
		return pluginapi.RequestInterceptResponse{}
	}

	requestID := metadataString(req.Metadata, "request_id")
	requestPath := metadataString(req.Metadata, "request_path")
	if requestPath == "" {
		requestPath = req.SourceFormat
	}

	logPluginInfo(hostCallbackID, "visible_files stripped", map[string]any{
		"model":          req.Model,
		"source_format":  req.SourceFormat,
		"request_id":     requestID,
		"request_path":   requestPath,
		"before_bytes":   result.BeforeBytes,
		"after_bytes":    result.AfterBytes,
		"removed_bytes":  result.RemovedBytes,
		"removed_blocks": result.RemovedBlocks,
		"removed_files":  result.RemovedFiles,
	})

	if detailLogEnabled() {
		logPath, errWrite := writeStripDetailLog(stripDetailLogInput{
			RequestID:      requestID,
			RequestPath:    requestPath,
			SourceFormat:   req.SourceFormat,
			Model:          req.Model,
			RequestedModel: req.RequestedModel,
			Stream:         req.Stream,
			Result:         result,
		})
		if errWrite != nil {
			logPluginInfo(hostCallbackID, "visible_files strip detail log failed", map[string]any{
				"error":      errWrite.Error(),
				"request_id": requestID,
			})
		} else {
			logPluginDebug(hostCallbackID, "visible_files strip detail log written", map[string]any{
				"path":       logPath,
				"request_id": requestID,
			})
		}
	}

	return pluginapi.RequestInterceptResponse{Body: result.AfterBody}
}

func stripVisibleFilesFromRequestBody(body []byte) (stripResult, error) {
	result := stripResult{
		BeforeBody:  bytes.Clone(body),
		BeforeBytes: len(body),
	}
	if !visibleFilesBlockPattern.Match(body) {
		result.AfterBody = bytes.Clone(body)
		result.AfterBytes = len(body)
		return result, nil
	}

	var root any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		after, files, blocks := stripVisibleFilesInText(string(body))
		result.AfterBody = []byte(after)
		result.AfterBytes = len(result.AfterBody)
		result.RemovedFiles = files
		result.RemovedBlocks = blocks
		result.RemovedBytes = result.BeforeBytes - result.AfterBytes
		result.Changed = !bytes.Equal(result.BeforeBody, result.AfterBody)
		return result, nil
	}

	changed, files, blocks := stripVisibleFilesInValue(root)
	if !changed {
		result.AfterBody = bytes.Clone(body)
		result.AfterBytes = len(body)
		return result, nil
	}

	afterBody, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return result, fmt.Errorf("marshal stripped request body: %w", errMarshal)
	}
	result.AfterBody = afterBody
	result.AfterBytes = len(afterBody)
	result.RemovedFiles = files
	result.RemovedBlocks = blocks
	result.RemovedBytes = result.BeforeBytes - result.AfterBytes
	result.Changed = true
	return result, nil
}

func stripVisibleFilesInValue(value any) (bool, []strippedFileInfo, int) {
	return stripVisibleFilesInValueMutable(&value)
}

func stripVisibleFilesInValueMutable(valuePointer *any) (bool, []strippedFileInfo, int) {
	if valuePointer == nil || *valuePointer == nil {
		return false, nil, 0
	}
	switch typed := (*valuePointer).(type) {
	case map[string]any:
		changed := false
		var files []strippedFileInfo
		blocks := 0
		for key, child := range typed {
			childCopy := any(child)
			childChanged, childFiles, childBlocks := stripVisibleFilesInValueMutable(&childCopy)
			if childChanged {
				typed[key] = childCopy
				changed = true
			}
			files = append(files, childFiles...)
			blocks += childBlocks
		}
		return changed, files, blocks
	case []any:
		changed := false
		var files []strippedFileInfo
		blocks := 0
		for index := range typed {
			childCopy := any(typed[index])
			childChanged, childFiles, childBlocks := stripVisibleFilesInValueMutable(&childCopy)
			if childChanged {
				typed[index] = childCopy
				changed = true
			}
			files = append(files, childFiles...)
			blocks += childBlocks
		}
		return changed, files, blocks
	case string:
		if !strings.Contains(strings.ToLower(typed), "<visible_files") {
			return false, nil, 0
		}
		after, files, blocks := stripVisibleFilesInText(typed)
		if after == typed {
			return false, files, blocks
		}
		*valuePointer = after
		return true, files, blocks
	default:
		return false, nil, 0
	}
}

func stripVisibleFilesInText(text string) (string, []strippedFileInfo, int) {
	matches := visibleFilesBlockPattern.FindAllString(text, -1)
	if len(matches) == 0 {
		return text, nil, 0
	}
	files := make([]strippedFileInfo, 0, 4)
	for _, match := range matches {
		for _, pathMatch := range visibleFilesFilePathPattern.FindAllStringSubmatch(match, -1) {
			if len(pathMatch) < 2 {
				continue
			}
			pathValue := strings.TrimSpace(pathMatch[1])
			if pathValue == "" {
				continue
			}
			fileBytes := len(match)
			if start := strings.Index(match, pathMatch[0]); start >= 0 {
				from := start
				rest := match[from+len(pathMatch[0]):]
				next := visibleFilesFilePathPattern.FindStringIndex(rest)
				if next != nil {
					fileBytes = len(pathMatch[0]) + next[0]
				} else {
					fileBytes = len(match) - from
				}
			}
			files = append(files, strippedFileInfo{Path: pathValue, Bytes: fileBytes})
		}
	}
	after := visibleFilesBlockPattern.ReplaceAllString(text, strippedVisibleFilesPlaceholder)
	return after, files, len(matches)
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}
