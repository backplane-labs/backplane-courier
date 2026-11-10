package admin

// HostsText is hostsText, for the tests.
func HostsText(hosts []string, insecure bool, otherwise string) string {
	return hostsText(hosts, insecure, otherwise)
}
