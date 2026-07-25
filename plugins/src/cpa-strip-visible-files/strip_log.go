package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type stripDetailLogInput struct {
	RequestID      string
	RequestPath    string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Result         stripResult
}

var unsafeFilenameCharacters = regexp.MustCompile(`[<>:"|?*\s]+`)

func writeStripDetailLog(input stripDetailLogInput) (string, error) {
	logsDir := resolveLogsDir()
	if errMkdir := os.MkdirAll(logsDir, 0o755); errMkdir != nil {
		return "", fmt.Errorf("create logs dir %s: %w", logsDir, errMkdir)
	}

	filename := buildStripDetailLogFilename(input.RequestPath, input.RequestID)
	logPath := filepath.Join(logsDir, filename)

	content := buildStripDetailLogContent(input)
	if errWrite := os.WriteFile(logPath, []byte(content), 0o644); errWrite != nil {
		return "", fmt.Errorf("write strip detail log %s: %w", logPath, errWrite)
	}
	return logPath, nil
}

func buildStripDetailLogFilename(requestPath, requestID string) string {
	pathPart := sanitizeForFilename(requestPath)
	if pathPart == "" {
		pathPart = "request"
	}
	timestamp := time.Now().Format("2006-01-02T150405")
	idPart := strings.TrimSpace(requestID)
	if idPart == "" {
		idPart = generateFallbackRequestID()
	}
	idPart = sanitizeForFilename(idPart)
	return fmt.Sprintf("%s-%s-%s-strip.log", pathPart, timestamp, idPart)
}

func buildStripDetailLogContent(input stripDetailLogInput) string {
	var builder strings.Builder
	builder.WriteString("=== STRIP CONTEXT BLOCKS ===\n")
	builder.WriteString(fmt.Sprintf("Plugin: %s@%s\n", pluginName, pluginVersion))
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	builder.WriteString(fmt.Sprintf("Request ID: %s\n", emptyAsDash(input.RequestID)))
	builder.WriteString(fmt.Sprintf("Request Path: %s\n", emptyAsDash(input.RequestPath)))
	builder.WriteString(fmt.Sprintf("Source Format: %s\n", emptyAsDash(input.SourceFormat)))
	builder.WriteString(fmt.Sprintf("Model: %s\n", emptyAsDash(input.Model)))
	builder.WriteString(fmt.Sprintf("Requested Model: %s\n", emptyAsDash(input.RequestedModel)))
	builder.WriteString(fmt.Sprintf("Stream: %t\n", input.Stream))
	builder.WriteString(fmt.Sprintf("Before Bytes: %d\n", input.Result.BeforeBytes))
	builder.WriteString(fmt.Sprintf("After Bytes: %d\n", input.Result.AfterBytes))
	builder.WriteString(fmt.Sprintf("Removed Bytes: %d\n", input.Result.RemovedBytes))
	builder.WriteString(fmt.Sprintf("Removed Blocks: %d\n", input.Result.RemovedBlocks))
	builder.WriteString(fmt.Sprintf("Removed Tags: %s\n", strings.Join(input.Result.RemovedTags, ", ")))
	builder.WriteString("Removed Files:\n")
	if len(input.Result.RemovedFiles) == 0 {
		builder.WriteString("  (none detected)\n")
	} else {
		for _, fileInfo := range input.Result.RemovedFiles {
			if fileInfo.Tag != "" {
				builder.WriteString(fmt.Sprintf("  - [%s] %s (%d bytes)\n", fileInfo.Tag, fileInfo.Path, fileInfo.Bytes))
			} else {
				builder.WriteString(fmt.Sprintf("  - %s (%d bytes)\n", fileInfo.Path, fileInfo.Bytes))
			}
		}
	}
	builder.WriteString("\n=== BEFORE BODY ===\n")
	builder.Write(input.Result.BeforeBody)
	if len(input.Result.BeforeBody) == 0 || input.Result.BeforeBody[len(input.Result.BeforeBody)-1] != '\n' {
		builder.WriteByte('\n')
	}
	builder.WriteString("\n=== AFTER BODY ===\n")
	builder.Write(input.Result.AfterBody)
	if len(input.Result.AfterBody) == 0 || input.Result.AfterBody[len(input.Result.AfterBody)-1] != '\n' {
		builder.WriteByte('\n')
	}
	return builder.String()
}

func sanitizeForFilename(value string) string {
	path := strings.TrimSpace(value)
	if strings.Contains(path, "?") {
		path = strings.Split(path, "?")[0]
	}
	path = strings.TrimPrefix(path, "/")
	path = strings.ReplaceAll(path, "/", "-")
	path = strings.ReplaceAll(path, ":", "-")
	path = unsafeFilenameCharacters.ReplaceAllString(path, "-")
	for strings.Contains(path, "--") {
		path = strings.ReplaceAll(path, "--", "-")
	}
	path = strings.Trim(path, "-")
	return path
}

func generateFallbackRequestID() string {
	buffer := make([]byte, 4)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return "00000000"
	}
	return hex.EncodeToString(buffer)
}

func emptyAsDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
