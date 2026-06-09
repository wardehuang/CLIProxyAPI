package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigAppliesCustomDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.CodexCompactModel != DefaultCodexCompactModel {
		t.Fatalf("codex compact model = %q, want %q", cfg.CodexCompactModel, DefaultCodexCompactModel)
	}
	if cfg.AntigravityCompactModel != DefaultAntigravityCompactModel {
		t.Fatalf("antigravity compact model = %q, want %q", cfg.AntigravityCompactModel, DefaultAntigravityCompactModel)
	}
}

func TestLoadConfigOverlaysCustomYaml(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	customPath := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(customPath, []byte("codex-compact-model: gpt-compact\nantigravity-compact-model: ag-compact\n"), 0o600); err != nil {
		t.Fatalf("WriteFile custom: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.CodexCompactModel != "gpt-compact" {
		t.Fatalf("codex compact model = %q, want gpt-compact", cfg.CodexCompactModel)
	}
	if cfg.AntigravityCompactModel != "ag-compact" {
		t.Fatalf("antigravity compact model = %q, want ag-compact", cfg.AntigravityCompactModel)
	}
}

func TestLoadConfigCustomYamlEmptyValueSkipsOverride(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	customPath := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(customPath, []byte("codex-compact-model: \n"), 0o600); err != nil {
		t.Fatalf("WriteFile custom: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.CodexCompactModel != "" {
		t.Fatalf("codex compact model = %q, want empty", cfg.CodexCompactModel)
	}
	if cfg.AntigravityCompactModel != DefaultAntigravityCompactModel {
		t.Fatalf("antigravity compact model = %q, want default", cfg.AntigravityCompactModel)
	}
}
