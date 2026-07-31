package pluginhost

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// syncForceRefreshDirectFromRecords enables force-direct OAuth refresh when any
// active plugin registers the ForceRefreshDirect capability flag.
func syncForceRefreshDirectFromRecords(records []capabilityRecord) {
	forceDirect := false
	for _, record := range records {
		if record.plugin.Capabilities.ForceRefreshDirect {
			forceDirect = true
			break
		}
	}
	helps.SetForceRefreshDirect(forceDirect)
}
