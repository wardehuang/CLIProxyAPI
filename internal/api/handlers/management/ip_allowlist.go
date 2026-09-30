package management

import "net"

func isManagementIPAllowlisted(clientIP string, entries []string) bool {
	client := net.ParseIP(clientIP)
	if client == nil {
		return false
	}
	if client4 := client.To4(); client4 != nil {
		client = client4
	}

	for _, entry := range entries {
		if allowed := net.ParseIP(entry); allowed != nil {
			if allowed4 := allowed.To4(); allowed4 != nil {
				allowed = allowed4
			}
			if allowed.Equal(client) {
				return true
			}
			continue
		}

		_, network, errParseCIDR := net.ParseCIDR(entry)
		if errParseCIDR == nil && network.Contains(client) {
			return true
		}
	}

	return false
}
