//go:build !windows

package proxy

import "errors"

// machineDNSAdapters has no adapters to read outside Windows, whose resolvers
// are in resolv.conf: upstreamConfig asks it only on Windows.
func machineDNSAdapters() ([]dnsAdapter, error) {
	return nil, errors.New("the DNS servers of network adapters are read only on Windows")
}
