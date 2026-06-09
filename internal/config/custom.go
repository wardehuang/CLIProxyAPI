package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultCodexCompactModel       = "gpt-5.4-mini"
	DefaultAntigravityCompactModel = "gemini-3.5-flash-low"
	customConfigFileName           = "custom.yaml"
)

// ApplyCustomDefaults initializes default custom settings before YAML overlays are applied.
func (cfg *Config) ApplyCustomDefaults() {
	if cfg == nil {
		return
	}
	cfg.CodexCompactModel = DefaultCodexCompactModel
	cfg.AntigravityCompactModel = DefaultAntigravityCompactModel
}

// LoadCustomConfigSibling overlays custom.yaml from the same directory as config.yaml.
func (cfg *Config) LoadCustomConfigSibling(configFile string, optional bool) error {
	if cfg == nil {
		return nil
	}
	configFile = strings.TrimSpace(configFile)
	if configFile == "" {
		return nil
	}
	customPath := filepath.Join(filepath.Dir(configFile), customConfigFileName)
	data, err := os.ReadFile(customPath)
	if err != nil {
		if os.IsNotExist(err) || optional {
			return nil
		}
		return fmt.Errorf("failed to read custom config file: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}

	values, err := parseCustomConfigScalars(data)
	if err != nil {
		if optional {
			return nil
		}
		return fmt.Errorf("failed to parse custom config file: %w", err)
	}
	if value, ok := values["codex-compact-model"]; ok {
		cfg.CodexCompactModel = value
	}
	if value, ok := values["antigravity-compact-model"]; ok {
		cfg.AntigravityCompactModel = value
	}
	return nil
}

func parseCustomConfigScalars(data []byte) (map[string]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	values := make(map[string]string)
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0] == nil || doc.Content[0].Kind != yaml.MappingNode {
		return values, nil
	}
	mapping := doc.Content[0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		keyNode := mapping.Content[i]
		valueNode := mapping.Content[i+1]
		if keyNode == nil || valueNode == nil {
			continue
		}
		key := strings.TrimSpace(keyNode.Value)
		switch key {
		case "codex-compact-model", "antigravity-compact-model":
			values[key] = strings.TrimSpace(valueNode.Value)
		}
	}
	return values, nil
}
