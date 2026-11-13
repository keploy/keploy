//go:build windows

package proxy

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dnsAdaptersBufSize is the size of the first buffer machineDNSAdapters hands
// GetAdaptersAddresses, which says the size it needs when it is too small. A
// variable so a row can start it too small.
var dnsAdaptersBufSize uint32 = 15 * 1024

// getAdaptersAddresses is windows.GetAdaptersAddresses; a row hands
// machineDNSAdapters a failure or adapters of its own through it.
var getAdaptersAddresses = windows.GetAdaptersAddresses

// machineDNSAdapters reads the machine's adapters and their DNS servers with
// GetAdaptersAddresses, which needs no elevation. A buffer too small is grown
// to the size Windows asks for; the adapter list can grow between two calls,
// so it asks again until the list fits.
func machineDNSAdapters() ([]dnsAdapter, error) {
	const flags = windows.GAA_FLAG_SKIP_UNICAST | windows.GAA_FLAG_SKIP_ANYCAST |
		windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_FRIENDLY_NAME
	size := dnsAdaptersBufSize
	for {
		buf := make([]byte, size)
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := getAdaptersAddresses(windows.AF_UNSPEC, flags, 0, first, &size)
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("GetAdaptersAddresses: %w", err)
		}
		var adapters []dnsAdapter
		for a := first; a != nil; a = a.Next {
			adapter := adapterOf(a.OperStatus, a.IfType)
			for s := a.FirstDnsServerAddress; s != nil; s = s.Next {
				if server, ok := adapterServer(s.Address, a.Ipv4Metric, a.Ipv6Metric); ok {
					adapter.servers = append(adapter.servers, server)
				}
			}
			adapters = append(adapters, adapter)
		}
		return adapters, nil
	}
}

// adapterServer reads a DNS server's socket address (serverOf). One with no
// address is no server.
func adapterServer(sa windows.SocketAddress, v4Metric, v6Metric uint32) (dnsAdapterServer, bool) {
	if sa.Sockaddr == nil {
		return dnsAdapterServer{}, false
	}
	switch sa.Sockaddr.Addr.Family {
	case windows.AF_INET:
		in4 := (*windows.RawSockaddrInet4)(unsafe.Pointer(sa.Sockaddr))
		return serverOf(winAFInet, in4.Addr, [16]byte{}, 0, v4Metric, v6Metric)
	case windows.AF_INET6:
		in6 := (*windows.RawSockaddrInet6)(unsafe.Pointer(sa.Sockaddr))
		return serverOf(winAFInet6, [4]byte{}, in6.Addr, in6.Scope_id, v4Metric, v6Metric)
	}
	return dnsAdapterServer{}, false
}
