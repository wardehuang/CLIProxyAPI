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

type patchDetailLogInput struct {
	RequestID      string
	RequestPath    string
	SourceFormat   string
	ToFormat       string
	Model          string
	RequestedModel string
	Stream         bool
	Stage          string
	Result         patchResult
}

var unsafeFilenameCharacters = regexp.MustCompile(`[<>:"|?*\s]+`)

func writePatchDetailLog(input patchDetailLogInput) (string, error) {
	logsDir := resolveLogsDir()
	if errMkdir := os.MkdirAll(logsDir, 0o755); errMkdir != nil {
		return "", fmt.Errorf("create logs dir %s: %w", logsDir, errMkdir)
	}

	filename := buildPatchDetailLogFilename(input.RequestPath, input.RequestID, input.Stage)
	logPath := filepath.Join(logsDir, filename)

	content := buildPatchDetailLogContent(input)
	if errWrite := os.WriteFile(logPath, []byte(content), 0o644); errWrite != nil {
		return "", fmt.Errorf("write mcp schema patch detail log %s: %w", logPath, errWrite)
	}
	return logPath, nil
}

func buildPatchDetailLogFilename(requestPath, requestID, stage string) string {
	pathPart := sanitizeForFilename(requestPath)
	if pathPart == "" {
		pathPart = "request"
	}
	stagePart := sanitizeForFilename(stage)
	if stagePart == "" {
		stagePart = "patch"
	}
	timestamp := time.Now().Format("2006-01-02T150405")
	idPart := strings.TrimSpace(requestID)
	if idPart == "" {
		idPart = generateFallbackRequestID()
	}
	idPart = sanitizeForFilename(idPart)
	return fmt.Sprintf("%s-%s-%s-%s-mcp-schema.log", pathPart, timestamp, idPart, stagePart)
}

func buildPatchDetailLogContent(input patchDetailLogInput) string {
	var builder strings.Builder
	builder.WriteString("=== MCP TOOL SCHEMA PATCH ===\n")
	builder.WriteString(fmt.Sprintf("Plugin: %s@%s\n", pluginName, pluginVersion))
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	builder.WriteString(fmt.Sprintf("Stage: %s\n", emptyAsDash(input.Stage)))
	builder.WriteString(fmt.Sprintf("Request ID: %s\n", emptyAsDash(input.RequestID)))
	builder.WriteString(fmt.Sprintf("Request Path: %s\n", emptyAsDash(input.RequestPath)))
	builder.WriteString(fmt.Sprintf("Source Format: %s\n", emptyAsDash(input.SourceFormat)))
	builder.WriteString(fmt.Sprintf("To Format: %s\n", emptyAsDash(input.ToFormat)))
	builder.WriteString(fmt.Sprintf("Model: %s\n", emptyAsDash(input.Model)))
	builder.WriteString(fmt.Sprintf("Requested Model: %s\n", emptyAsDash(input.RequestedModel)))
	builder.WriteString(fmt.Sprintf("Stream: %t\n", input.Stream))
	builder.WriteString(fmt.Sprintf("Only Empty: %t\n", onlyEmptyEnabled()))
	builder.WriteString(fmt.Sprintf("Inject Missing: %t\n", injectMissingEnabled()))
	builder.WriteString(fmt.Sprintf("Before Bytes: %d\n", len(input.Result.BeforeBody)))
	builder.WriteString(fmt.Sprintf("After Bytes: %d\n", len(input.Result.AfterBody)))
	builder.WriteString(fmt.Sprintf("Patched Tools: %s\n", joinOrDash(input.Result.PatchedTools)))
	builder.WriteString(fmt.Sprintf("Injected Tools: %s\n", joinOrDash(input.Result.InjectedTools)))
	builder.WriteString(fmt.Sprintf("Skipped Tools: %s\n", joinOrDash(input.Result.SkippedTools)))
	builder.WriteString(fmt.Sprintf("Missing Tools: %s\n", joinOrDash(input.Result.MissingTools)))
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

func joinOrDash(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ", ")
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
