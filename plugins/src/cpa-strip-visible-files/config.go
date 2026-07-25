package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type detailLogMode string

const (
	detailLogModeAuto  detailLogMode = "auto"
	detailLogModeTrue  detailLogMode = "true"
	detailLogModeFalse detailLogMode = "false"
)

type pluginConfig struct {
	Debug          bool          `json:"debug" yaml:"debug"`
	LogsDir        string        `json:"logs-dir" yaml:"logs-dir"`
	HostConfigPath string        `json:"host-config-path" yaml:"host-config-path"`
	DetailLog      detailLogMode `json:"detail-log" yaml:"detail-log"`
}

type hostLoggingFlags struct {
	RequestLog     bool `yaml:"request-log"`
	CommercialMode bool `yaml:"commercial-mode"`
}

var pluginConfigState = struct {
	sync.RWMutex
	config pluginConfig
}{
	config: defaultPluginConfig(),
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		LogsDir:   "logs",
		DetailLog: detailLogModeAuto,
	}
}

func configurePlugin(configYAML []byte) {
	config := defaultPluginConfig()
	if len(bytes.TrimSpace(configYAML)) > 0 {
		var parsed pluginConfig
		if errUnmarshal := yaml.Unmarshal(configYAML, &parsed); errUnmarshal == nil {
			config.Debug = parsed.Debug
			if strings.TrimSpace(parsed.LogsDir) != "" {
				config.LogsDir = strings.TrimSpace(parsed.LogsDir)
			}
			config.HostConfigPath = strings.TrimSpace(parsed.HostConfigPath)
			config.DetailLog = normalizeDetailLogMode(parsed.DetailLog)
		}
	}
	pluginConfigState.Lock()
	pluginConfigState.config = config
	pluginConfigState.Unlock()
}

func normalizeDetailLogMode(mode detailLogMode) detailLogMode {
	switch strings.ToLower(strings.TrimSpace(string(mode))) {
	case "", "auto":
		return detailLogModeAuto
	case "true", "1", "yes", "on":
		return detailLogModeTrue
	case "false", "0", "no", "off":
		return detailLogModeFalse
	default:
		return detailLogModeAuto
	}
}

func currentPluginConfig() pluginConfig {
	pluginConfigState.RLock()
	defer pluginConfigState.RUnlock()
	return pluginConfigState.config
}

func pluginDebugEnabled() bool {
	return currentPluginConfig().Debug
}

func resolveLogsDir() string {
	config := currentPluginConfig()
	logsDir := strings.TrimSpace(config.LogsDir)
	if logsDir == "" {
		logsDir = "logs"
	}
	if filepath.IsAbs(logsDir) {
		return logsDir
	}
	workingDirectory, errWorkingDirectory := os.Getwd()
	if errWorkingDirectory != nil || strings.TrimSpace(workingDirectory) == "" {
		return logsDir
	}
	return filepath.Join(workingDirectory, logsDir)
}

func detailLogEnabled() bool {
	config := currentPluginConfig()
	switch config.DetailLog {
	case detailLogModeTrue:
		return true
	case detailLogModeFalse:
		return false
	default:
		flags, ok := loadHostLoggingFlags(config.HostConfigPath)
		if !ok {
			return false
		}
		return flags.RequestLog && !flags.CommercialMode
	}
}

func loadHostLoggingFlags(hostConfigPath string) (hostLoggingFlags, bool) {
	candidates := hostConfigCandidates(hostConfigPath)
	for _, candidate := range candidates {
		raw, errRead := os.ReadFile(candidate)
		if errRead != nil || len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var flags hostLoggingFlags
		if errUnmarshal := yaml.Unmarshal(raw, &flags); errUnmarshal != nil {
			continue
		}
		return flags, true
	}
	return hostLoggingFlags{}, false
}

func hostConfigCandidates(hostConfigPath string) []string {
	candidates := make([]string, 0, 4)
	if trimmed := strings.TrimSpace(hostConfigPath); trimmed != "" {
		candidates = append(candidates, trimmed)
	}
	if workingDirectory, errWorkingDirectory := os.Getwd(); errWorkingDirectory == nil && strings.TrimSpace(workingDirectory) != "" {
		candidates = append(candidates, filepath.Join(workingDirectory, "config.yaml"))
	}
	candidates = append(candidates, "config.yaml")
	return uniqueNonEmptyStrings(candidates)
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}
