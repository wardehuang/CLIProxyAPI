package main

import (
	"bytes"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type pluginConfig struct {
	Debug           bool     `json:"debug" yaml:"debug"`
	Enabled         *bool    `json:"enabled" yaml:"enabled"`
	PadPlaceholder  string   `json:"pad_placeholder" yaml:"pad_placeholder"`
	MaxEntries      int      `json:"max_entries" yaml:"max_entries"`
	TTLSeconds      int      `json:"ttl_seconds" yaml:"ttl_seconds"`
	ModelSubstrings []string `json:"model_substrings" yaml:"model_substrings"`
}

var pluginConfigState = struct {
	sync.RWMutex
	config pluginConfig
}{
	config: defaultPluginConfig(),
}

func defaultPluginConfig() pluginConfig {
	enabled := true
	return pluginConfig{
		Enabled:         &enabled,
		PadPlaceholder:  " ",
		MaxEntries:      4096,
		TTLSeconds:      3600,
		ModelSubstrings: []string{"deepseek"},
	}
}

func configurePlugin(configYAML []byte) {
	config := defaultPluginConfig()
	if len(bytes.TrimSpace(configYAML)) > 0 {
		var parsed pluginConfig
		if errUnmarshal := yaml.Unmarshal(configYAML, &parsed); errUnmarshal == nil {
			config.Debug = parsed.Debug
			if parsed.Enabled != nil {
				config.Enabled = parsed.Enabled
			}
			if strings.TrimSpace(parsed.PadPlaceholder) != "" {
				config.PadPlaceholder = parsed.PadPlaceholder
			}
			if parsed.MaxEntries > 0 {
				config.MaxEntries = parsed.MaxEntries
			}
			if parsed.TTLSeconds > 0 {
				config.TTLSeconds = parsed.TTLSeconds
			}
			if len(parsed.ModelSubstrings) > 0 {
				normalized := make([]string, 0, len(parsed.ModelSubstrings))
				for _, item := range parsed.ModelSubstrings {
					item = strings.ToLower(strings.TrimSpace(item))
					if item == "" {
						continue
					}
					normalized = append(normalized, item)
				}
				if len(normalized) > 0 {
					config.ModelSubstrings = normalized
				}
			}
		}
	}
	pluginConfigState.Lock()
	pluginConfigState.config = config
	pluginConfigState.Unlock()
	resetReasoningCache(config.MaxEntries, time.Duration(config.TTLSeconds)*time.Second)
	resetStreamAccumulators()
	logPluginInfo("", "deepseek reasoning replay configured", map[string]any{
		"enabled":          pluginEnabled(),
		"debug":            config.Debug,
		"pad_placeholder":  config.PadPlaceholder,
		"max_entries":      config.MaxEntries,
		"ttl_seconds":      config.TTLSeconds,
		"model_substrings": config.ModelSubstrings,
		"version":          pluginVersion,
	})
}

func currentPluginConfig() pluginConfig {
	pluginConfigState.RLock()
	defer pluginConfigState.RUnlock()
	return pluginConfigState.config
}

func pluginDebugEnabled() bool {
	return currentPluginConfig().Debug
}

func pluginEnabled() bool {
	config := currentPluginConfig()
	if config.Enabled == nil {
		return true
	}
	return *config.Enabled
}

func padPlaceholder() string {
	placeholder := currentPluginConfig().PadPlaceholder
	if placeholder == "" {
		return " "
	}
	return placeholder
}

func modelMatches(model string) bool {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return false
	}
	for _, needle := range currentPluginConfig().ModelSubstrings {
		if needle != "" && strings.Contains(normalized, needle) {
			return true
		}
	}
	return false
}
