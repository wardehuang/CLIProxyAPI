package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

type customConfig struct {
	CodexCompactModel       string `yaml:"codex-compact-model"`
	AntigravityCompactModel string `yaml:"antigravity-compact-model"`
}

// ApplyCustomDefaults is kept as a hook for local-only defaults.
func (cfg *Config) ApplyCustomDefaults() {}

// LoadCustomConfigSibling loads optional local-only overrides from custom.yaml next to config.yaml.
func (cfg *Config) LoadCustomConfigSibling(configFile string, optional bool) error {
	if cfg == nil || strings.TrimSpace(configFile) == "" {
		return nil
	}

	customPath := filepath.Join(filepath.Dir(configFile), "custom.yaml")
	data, err := os.ReadFile(customPath)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.EISDIR) {
			return nil
		}
		if optional {
			return nil
		}
		return fmt.Errorf("failed to read custom config file: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}

	var custom customConfig
	if err := yaml.Unmarshal(data, &custom); err != nil {
		if optional {
			return nil
		}
		return fmt.Errorf("failed to parse custom config file: %w", err)
	}

	if model := strings.TrimSpace(custom.CodexCompactModel); model != "" {
		cfg.CodexCompactModel = model
	}
	if model := strings.TrimSpace(custom.AntigravityCompactModel); model != "" {
		cfg.AntigravityCompactModel = model
	}
	return nil
}
