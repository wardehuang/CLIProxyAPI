package main

import (
	"bytes"
	"sync"

	"gopkg.in/yaml.v3"
)

type pluginConfig struct {
	Debug bool `json:"debug" yaml:"debug"`
}

var pluginConfigState = struct {
	sync.RWMutex
	config pluginConfig
}{}

func configurePlugin(configYAML []byte) {
	config := pluginConfig{}
	if len(bytes.TrimSpace(configYAML)) > 0 {
		var parsed pluginConfig
		if errUnmarshal := yaml.Unmarshal(configYAML, &parsed); errUnmarshal == nil {
			config.Debug = parsed.Debug
		}
	}
	pluginConfigState.Lock()
	pluginConfigState.config = config
	pluginConfigState.Unlock()
}

func pluginDebugEnabled() bool {
	pluginConfigState.RLock()
	defer pluginConfigState.RUnlock()
	return pluginConfigState.config.Debug
}
