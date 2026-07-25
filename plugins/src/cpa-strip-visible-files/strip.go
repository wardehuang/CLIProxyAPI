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

type stripTarget struct {
	Name        string
	Needle      string
	Pattern     *regexp.Regexp
	Placeholder string
}

var (
	stripTargets = []stripTarget{
		{
			Name:        "visible_files",
			Needle:      "<visible_files",
			Pattern:     regexp.MustCompile(`(?is)<visible_files\b[^>]*>.*?</visible_files\s*>`),
			Placeholder: "[cpa-strip-visible-files: removed visible_files]",
		},
		{
			Name:        "open_and_recently_viewed_files",
			Needle:      "<open_and_recently_viewed_files",
			Pattern:     regexp.MustCompile(`(?is)<open_and_recently_viewed_files\b[^>]*>.*?</open_and_recently_viewed_files\s*>`),
			Placeholder: "[cpa-strip-visible-files: removed open_and_recently_viewed_files]",
		},
	}
	filePathInBlockPattern = regexp.MustCompile(`(?is)<file\b[^>]*\bpath\s*=\s*"([^"]+)"[^>]*>`)
)

// kept for existing tests/call sites that reference the visible_files placeholder.
const strippedVisibleFilesPlaceholder = "[cpa-strip-visible-files: removed visible_files]"

type strippedFileInfo struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
	Tag   string `json:"tag,omitempty"`
}

type stripResult struct {
	Changed       bool
	BeforeBody    []byte
	AfterBody     []byte
	BeforeBytes   int
	AfterBytes    int
	RemovedBytes  int
	RemovedBlocks int
	RemovedTags   []string
	RemovedFiles  []strippedFileInfo
}

func interceptRequestBeforeAuth(ctx context.Context, req pluginapi.RequestInterceptRequest, hostCallbackID string) pluginapi.RequestInterceptResponse {
	_ = ctx
	if len(bytes.TrimSpace(req.Body)) == 0 {
		return pluginapi.RequestInterceptResponse{}
	}
	result, errStrip := stripVisibleFilesFromRequestBody(req.Body)
	if errStrip != nil {
		logPluginDebug(hostCallbackID, "context strip skipped", map[string]any{
			"reason": "strip_error",
			"error":  errStrip.Error(),
			"model":  req.Model,
		})
		return pluginapi.RequestInterceptResponse{}
	}
	if !result.Changed {
		logPluginDebug(hostCallbackID, "context strip skipped", map[string]any{
			"reason":        "no_strip_targets",
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

	logPluginInfo(hostCallbackID, "context blocks stripped", map[string]any{
		"model":          req.Model,
		"source_format":  req.SourceFormat,
		"request_id":     requestID,
		"request_path":   requestPath,
		"before_bytes":   result.BeforeBytes,
		"after_bytes":    result.AfterBytes,
		"removed_bytes":  result.RemovedBytes,
		"removed_blocks": result.RemovedBlocks,
		"removed_tags":   result.RemovedTags,
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
			logPluginInfo(hostCallbackID, "context strip detail log failed", map[string]any{
				"error":      errWrite.Error(),
				"request_id": requestID,
			})
		} else {
			logPluginDebug(hostCallbackID, "context strip detail log written", map[string]any{
				"path":       logPath,
				"request_id": requestID,
			})
		}
	}

	return pluginapi.RequestInterceptResponse{Body: result.AfterBody}
}

func bodyContainsAnyStripTarget(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// Clients usually send raw '<' in JSON strings. Go's encoding/json.Marshal
	// HTML-escapes '<' as \u003c, so also detect the escaped form.
	lowerBody := strings.ToLower(string(body))
	for _, target := range stripTargets {
		if strings.Contains(lowerBody, target.Needle) {
			return true
		}
		escapedNeedle := strings.ReplaceAll(target.Needle, "<", `\u003c`)
		if strings.Contains(lowerBody, strings.ToLower(escapedNeedle)) {
			return true
		}
	}
	return false
}

func stringContainsAnyStripTarget(text string) bool {
	lowerText := strings.ToLower(text)
	for _, target := range stripTargets {
		if strings.Contains(lowerText, target.Needle) {
			return true
		}
	}
	return false
}

func stripVisibleFilesFromRequestBody(body []byte) (stripResult, error) {
	result := stripResult{
		BeforeBody:  bytes.Clone(body),
		BeforeBytes: len(body),
	}
	if !bodyContainsAnyStripTarget(body) {
		result.AfterBody = bytes.Clone(body)
		result.AfterBytes = len(body)
		return result, nil
	}

	var root any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		after, files, blocks, tags := stripContextBlocksInText(string(body))
		result.AfterBody = []byte(after)
		result.AfterBytes = len(result.AfterBody)
		result.RemovedFiles = files
		result.RemovedBlocks = blocks
		result.RemovedTags = tags
		result.RemovedBytes = result.BeforeBytes - result.AfterBytes
		result.Changed = !bytes.Equal(result.BeforeBody, result.AfterBody)
		return result, nil
	}

	changed, files, blocks, tags := stripContextBlocksInValue(root)
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
	result.RemovedTags = tags
	result.RemovedBytes = result.BeforeBytes - result.AfterBytes
	result.Changed = true
	return result, nil
}

func stripContextBlocksInValue(value any) (bool, []strippedFileInfo, int, []string) {
	return stripContextBlocksInValueMutable(&value)
}

func stripContextBlocksInValueMutable(valuePointer *any) (bool, []strippedFileInfo, int, []string) {
	if valuePointer == nil || *valuePointer == nil {
		return false, nil, 0, nil
	}
	switch typed := (*valuePointer).(type) {
	case map[string]any:
		changed := false
		var files []strippedFileInfo
		var tags []string
		blocks := 0
		for key, child := range typed {
			childCopy := any(child)
			childChanged, childFiles, childBlocks, childTags := stripContextBlocksInValueMutable(&childCopy)
			if childChanged {
				typed[key] = childCopy
				changed = true
			}
			files = append(files, childFiles...)
			blocks += childBlocks
			tags = appendUniqueStrings(tags, childTags...)
		}
		return changed, files, blocks, tags
	case []any:
		changed := false
		var files []strippedFileInfo
		var tags []string
		blocks := 0
		for index := range typed {
			childCopy := any(typed[index])
			childChanged, childFiles, childBlocks, childTags := stripContextBlocksInValueMutable(&childCopy)
			if childChanged {
				typed[index] = childCopy
				changed = true
			}
			files = append(files, childFiles...)
			blocks += childBlocks
			tags = appendUniqueStrings(tags, childTags...)
		}
		return changed, files, blocks, tags
	case string:
		if !stringContainsAnyStripTarget(typed) {
			return false, nil, 0, nil
		}
		after, files, blocks, tags := stripContextBlocksInText(typed)
		if after == typed {
			return false, files, blocks, tags
		}
		*valuePointer = after
		return true, files, blocks, tags
	default:
		return false, nil, 0, nil
	}
}

func stripContextBlocksInText(text string) (string, []strippedFileInfo, int, []string) {
	after := text
	files := make([]strippedFileInfo, 0, 4)
	tags := make([]string, 0, len(stripTargets))
	totalBlocks := 0

	for _, target := range stripTargets {
		matches := target.Pattern.FindAllString(after, -1)
		if len(matches) == 0 {
			continue
		}
		totalBlocks += len(matches)
		tags = appendUniqueStrings(tags, target.Name)
		for _, match := range matches {
			files = append(files, extractFileInfosFromBlock(match, target.Name)...)
		}
		after = target.Pattern.ReplaceAllString(after, target.Placeholder)
	}
	return after, files, totalBlocks, tags
}

func extractFileInfosFromBlock(block string, tagName string) []strippedFileInfo {
	files := make([]strippedFileInfo, 0, 2)
	for _, pathMatch := range filePathInBlockPattern.FindAllStringSubmatch(block, -1) {
		if len(pathMatch) < 2 {
			continue
		}
		pathValue := strings.TrimSpace(pathMatch[1])
		if pathValue == "" {
			continue
		}
		fileBytes := len(block)
		if start := strings.Index(block, pathMatch[0]); start >= 0 {
			from := start
			rest := block[from+len(pathMatch[0]):]
			next := filePathInBlockPattern.FindStringIndex(rest)
			if next != nil {
				fileBytes = len(pathMatch[0]) + next[0]
			} else {
				fileBytes = len(block) - from
			}
		}
		files = append(files, strippedFileInfo{
			Path:  pathValue,
			Bytes: fileBytes,
			Tag:   tagName,
		})
	}
	return files
}

func appendUniqueStrings(values []string, additions ...string) []string {
	for _, addition := range additions {
		addition = strings.TrimSpace(addition)
		if addition == "" {
			continue
		}
		exists := false
		for _, existing := range values {
			if existing == addition {
				exists = true
				break
			}
		}
		if !exists {
			values = append(values, addition)
		}
	}
	return values
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
