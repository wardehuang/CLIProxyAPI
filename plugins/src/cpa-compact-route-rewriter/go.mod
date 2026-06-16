module github.com/wardehuang/cpa-compact-route-rewriter

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v7 v7.0.0
	github.com/tidwall/gjson v1.18.0
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/router-for-me/CLIProxyAPI/v7 => ../../..
