package main

import (
	"bytes"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const (
	strategyFillFirst  = "fill-first"
	strategyRoundRobin = "round-robin"
)

type pluginConfig struct {
	Debug    bool   `json:"debug" yaml:"debug"`
	Strategy string `json:"strategy" yaml:"strategy"`
}

var pluginConfigState = struct {
	sync.RWMutex
	config pluginConfig
}{config: pluginConfig{Strategy: strategyFillFirst}}

func configurePlugin(configYAML []byte) {
	config := pluginConfig{Strategy: strategyFillFirst}
	if len(bytes.TrimSpace(configYAML)) > 0 {
		var parsed pluginConfig
		if errUnmarshal := yaml.Unmarshal(configYAML, &parsed); errUnmarshal == nil {
			config.Debug = parsed.Debug
			config.Strategy = normalizeStrategy(parsed.Strategy)
		}
	}
	pluginConfigState.Lock()
	pluginConfigState.config = config
	pluginConfigState.Unlock()
}

func currentPluginConfig() pluginConfig {
	pluginConfigState.RLock()
	defer pluginConfigState.RUnlock()
	return pluginConfigState.config
}

func pluginDebugEnabled() bool {
	return currentPluginConfig().Debug
}

func normalizeStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case strategyRoundRobin:
		return strategyRoundRobin
	default:
		return strategyFillFirst
	}
}
