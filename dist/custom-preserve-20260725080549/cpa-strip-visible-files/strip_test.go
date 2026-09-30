package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripVisibleFilesFromClaudeMessagesBody(t *testing.T) {
	body := []byte(`{
  "model":"grok-4.5",
  "messages":[
    {
      "role":"user",
      "content":"<user_info>ok</user_info>\n<visible_files>\n<file path=\"data/accounts.txt\">secret-line-1\nsecret-line-2</file>\n</visible_files>\n"
    },
    {
      "role":"user",
      "content":"<user_query>你好</user_query>"
    }
  ]
}`)

	result, errStrip := stripVisibleFilesFromRequestBody(body)
	if errStrip != nil {
		t.Fatalf("stripVisibleFilesFromRequestBody() error = %v", errStrip)
	}
	if !result.Changed {
		t.Fatal("expected strip change")
	}
	if result.RemovedBlocks != 1 {
		t.Fatalf("RemovedBlocks = %d, want 1", result.RemovedBlocks)
	}
	if len(result.RemovedFiles) != 1 || result.RemovedFiles[0].Path != "data/accounts.txt" {
		t.Fatalf("RemovedFiles = %#v", result.RemovedFiles)
	}
	if strings.Contains(string(result.AfterBody), "secret-line-1") {
		t.Fatalf("after body still contains secret content: %s", string(result.AfterBody))
	}
	if !strings.Contains(string(result.AfterBody), strippedVisibleFilesPlaceholder) {
		t.Fatalf("after body missing placeholder: %s", string(result.AfterBody))
	}
	if !strings.Contains(string(result.AfterBody), "你好") {
		t.Fatalf("after body lost user query: %s", string(result.AfterBody))
	}
	if result.AfterBytes >= result.BeforeBytes {
		t.Fatalf("after bytes %d should be smaller than before %d", result.AfterBytes, result.BeforeBytes)
	}

	var parsed map[string]any
	if errUnmarshal := json.Unmarshal(result.AfterBody, &parsed); errUnmarshal != nil {
		t.Fatalf("after body is not valid JSON: %v", errUnmarshal)
	}
}

func TestStripVisibleFilesNoopWhenMissing(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hello"}]}`)
	result, errStrip := stripVisibleFilesFromRequestBody(body)
	if errStrip != nil {
		t.Fatalf("stripVisibleFilesFromRequestBody() error = %v", errStrip)
	}
	if result.Changed {
		t.Fatal("expected no change")
	}
}

func TestBuildStripDetailLogFilename(t *testing.T) {
	name := buildStripDetailLogFilename("/v1/messages?beta=true", "5edbba20")
	if !strings.HasPrefix(name, "v1-messages-") {
		t.Fatalf("filename prefix = %q", name)
	}
	if !strings.HasSuffix(name, "-5edbba20-strip.log") {
		t.Fatalf("filename suffix = %q", name)
	}
}

func TestWriteStripDetailLog(t *testing.T) {
	tempDir := t.TempDir()
	configurePlugin([]byte("logs-dir: " + filepath.ToSlash(tempDir) + "\ndetail-log: true\n"))

	path, errWrite := writeStripDetailLog(stripDetailLogInput{
		RequestID:   "5edbba20",
		RequestPath: "/v1/messages",
		Model:       "grok-4.5",
		Result: stripResult{
			BeforeBody:    []byte(`{"before":true}`),
			AfterBody:     []byte(`{"after":true}`),
			BeforeBytes:   14,
			AfterBytes:    13,
			RemovedBytes:  1,
			RemovedBlocks: 1,
			RemovedFiles:  []strippedFileInfo{{Path: "data/a.txt", Bytes: 100}},
		},
	})
	if errWrite != nil {
		t.Fatalf("writeStripDetailLog() error = %v", errWrite)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	content := string(raw)
	if !strings.Contains(content, "=== BEFORE BODY ===") || !strings.Contains(content, `{"before":true}`) {
		t.Fatalf("missing before body section: %s", content)
	}
	if !strings.Contains(content, "=== AFTER BODY ===") || !strings.Contains(content, `{"after":true}`) {
		t.Fatalf("missing after body section: %s", content)
	}
	if !strings.Contains(content, "data/a.txt") {
		t.Fatalf("missing removed file: %s", content)
	}
}

func TestDetailLogAutoUsesHostFlags(t *testing.T) {
	tempDir := t.TempDir()
	hostConfigPath := filepath.Join(tempDir, "config.yaml")
	if errWrite := os.WriteFile(hostConfigPath, []byte("request-log: true\ncommercial-mode: false\n"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile host config: %v", errWrite)
	}
	configurePlugin([]byte("detail-log: auto\nhost-config-path: " + filepath.ToSlash(hostConfigPath) + "\n"))
	if !detailLogEnabled() {
		t.Fatal("detailLogEnabled() = false, want true")
	}

	if errWrite := os.WriteFile(hostConfigPath, []byte("request-log: true\ncommercial-mode: true\n"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile host config commercial: %v", errWrite)
	}
	if detailLogEnabled() {
		t.Fatal("detailLogEnabled() = true with commercial-mode, want false")
	}
}
