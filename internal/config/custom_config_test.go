package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigReadsCustomCompactModelOverrides(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	customPath := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(customPath, []byte("codex-compact-model: gpt-5.4-mini\nantigravity-compact-model: gemini-3.1-flash-lite\n"), 0o600); err != nil {
		t.Fatalf("WriteFile custom: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.CodexCompactModel != "gpt-5.4-mini" {
		t.Fatalf("CodexCompactModel = %q, want gpt-5.4-mini", cfg.CodexCompactModel)
	}
	if cfg.AntigravityCompactModel != "gemini-3.1-flash-lite" {
		t.Fatalf("AntigravityCompactModel = %q, want gemini-3.1-flash-lite", cfg.AntigravityCompactModel)
	}
}

func TestLoadConfigAllowsMissingCustomConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.CodexCompactModel != "" || cfg.AntigravityCompactModel != "" {
		t.Fatalf("compact overrides = (%q, %q), want empty", cfg.CodexCompactModel, cfg.AntigravityCompactModel)
	}
}
