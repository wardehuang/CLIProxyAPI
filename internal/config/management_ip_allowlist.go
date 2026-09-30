package config

import (
	"fmt"
	"net"
	"strings"
)

func validateManagementIPAllowlist(entries []string) error {
	for _, entry := range entries {
		if entry == "" || strings.TrimSpace(entry) != entry {
			return fmt.Errorf("invalid management.ip-allowlist entry %q: expected an IP address or CIDR", entry)
		}
		if net.ParseIP(entry) != nil {
			continue
		}
		if _, _, errParseCIDR := net.ParseCIDR(entry); errParseCIDR != nil {
			return fmt.Errorf("invalid management.ip-allowlist entry %q: %w", entry, errParseCIDR)
		}
	}
	return nil
}
