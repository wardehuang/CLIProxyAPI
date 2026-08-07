module github.com/wardehuang/cpa-xai-ip-switcher

go 1.26.0

require (
	github.com/mattn/go-sqlite3 v1.14.49
	github.com/router-for-me/CLIProxyAPI/v7 v7.0.0
	golang.org/x/net v0.57.0
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/router-for-me/CLIProxyAPI/v7 => ../../..
