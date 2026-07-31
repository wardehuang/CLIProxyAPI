package main

const (
	pluginName    = "cpa-refresh-direct"
	pluginVersion = "0.1.0"
)

type registrationCapabilities struct {
	ForceRefreshDirect bool `json:"force_refresh_direct"`
}