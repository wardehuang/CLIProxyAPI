package main

type pluginConfig struct {
	DatabasePath              string `yaml:"database_path" json:"database_path"`
	InspectionIntervalSeconds int    `yaml:"inspection_interval_seconds" json:"inspection_interval_seconds"`
	inspectionIntervalSet     bool
}
