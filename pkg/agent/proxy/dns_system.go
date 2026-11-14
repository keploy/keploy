package proxy

import (
	"cmp"
	"net/netip"
	"slices"
	"strconv"

	"github.com/miekg/dns"
)

// dnsAdapter is what upstreamConfig reads of one of the machine's network
// adapters on Windows, where the resolvers are the adapters' DNS servers
// (GetAdaptersAddresses) rather than lines of /etc/resolv.conf.
type dnsAdapter struct {
	// up is whether the adapter is up (IfOperStatusUp): Windows asks no
	// server of an adapter that is down.
	up bool
	// loopback is whether it is the software loopback interface.
	loopback bool
	// servers are its DNS servers, in the order Windows lists them.
	servers []dnsAdapterServer
}

// dnsAdapterServer is one DNS server of an adapter. A link-local IPv6 server
// carries its adapter's zone, without which it cannot be reached.
type dnsAdapterServer struct {
	addr netip.Addr
	// metric is the adapter's interface metric for the server's family
	// (Ipv4Metric or Ipv6Metric): the lower, the more Windows prefers it.
	metric uint32
}

// readDNSAdapters lists the machine's adapters and their DNS servers. Only
// Windows has one (dns_system_windows.go); elsewhere it fails, and nothing
// asks it. A variable so a row can hand upstreamConfig the adapters of a
// machine it is not on.
var readDNSAdapters = machineDNSAdapters

// upstreamConfig is the machine's resolver configuration, the servers a DNS
// query no mock answers is forwarded to: /etc/resolv.conf (resolvConfPath),
// or on Windows, which has none, the DNS servers of its adapters that are up
// (resolversOfAdapters). It also says where it read them, for the log.
//
// On Windows it gives the adapter each server is asked on (its index in the
// list GetAdaptersAddresses gives): a name one adapter's server denies is
// still asked on the others (upstreamSearch), as Windows asks them.
// resolv.conf's servers are on no adapter: they are one list, and the first
// answer that is not a failure or a refusal to answer is the answer, as
// Linux's resolver takes it.
//
// A row that points resolvConfPath at its own file reads that file on every
// GOOS, so the parsing rows run on Windows as well.
func upstreamConfig(goos string) (*dns.ClientConfig, map[string]int, string, error) {
	if goos == "windows" && resolvConfPath == defaultResolvConfPath {
		const source = "the DNS servers of the machine's network adapters"
		adapters, err := readDNSAdapters()
		if err != nil {
			return nil, nil, source, err
		}
		servers, adapter := resolversOfAdapters(adapters)
		return &dns.ClientConfig{Servers: servers, Port: "53"}, adapter, source, nil
	}
	config, err := dns.ClientConfigFromFile(resolvConfPath)
	return config, nil, resolvConfPath, err
}

// resolversOfAdapters lists the DNS servers Windows asks, the way Linux's
// resolver reads them from resolv.conf: the servers of every adapter that is
// up and not the loopback, those of the adapter Windows prefers (the lowest
// interface metric) first, each adapter's in the order it lists them, each
// server once. A site-local IPv6 server (fec0::/10, deprecated by RFC 3879)
// is not one: Windows lists fec0:0:0:ffff::1 to ::3 on an adapter with no
// IPv6 DNS server configured, and no server answers there.
//
// It also says which adapter each server is asked on (its index in
// adapters): a server two adapters list is asked on the one it is asked on
// first.
func resolversOfAdapters(adapters []dnsAdapter) ([]string, map[string]int) {
	type adapterServer struct {
		dnsAdapterServer
		adapter int
	}
	var servers []adapterServer
	for i, a := range adapters {
		if !a.up || a.loopback {
			continue
		}
		for _, s := range a.servers {
			if !s.addr.IsValid() || s.addr.IsUnspecified() || isSiteLocal(s.addr) {
				continue
			}
			servers = append(servers, adapterServer{s, i})
		}
	}
	slices.SortStableFunc(servers, func(a, b adapterServer) int {
		return cmp.Compare(a.metric, b.metric)
	})
	out := make([]string, 0, len(servers))
	adapter := make(map[string]int, len(servers))
	for _, s := range servers {
		if name := s.addr.String(); !slices.Contains(out, name) {
			out = append(out, name)
			adapter[name] = s.adapter
		}
	}
	return out, adapter
}

// The values of GetAdaptersAddresses's fields that adapterOf and serverOf
// read, from Windows's ifdef.h, ipifcons.h and ws2def.h.
const (
	winIfOperStatusUp                = 1
	winIfTypeSoftwareLoopback        = 24
	winAFInet                 uint16 = 2
	winAFInet6                uint16 = 23
)

// adapterOf is what resolversOfAdapters needs of an adapter Windows lists
// with the operational status and interface type it gives.
func adapterOf(operStatus, ifType uint32) dnsAdapter {
	return dnsAdapter{up: operStatus == winIfOperStatusUp, loopback: ifType == winIfTypeSoftwareLoopback}
}

// serverOf reads one DNS server of an adapter from the socket address Windows
// gives (its family, and its IPv4 or IPv6 address and scope ID), with the
// adapter's metric for that family. A link-local IPv6 server keeps its scope
// ID as its zone, without which it cannot be reached. A family other than
// IPv4 and IPv6 is no server.
func serverOf(family uint16, v4 [4]byte, v6 [16]byte, scope, v4Metric, v6Metric uint32) (dnsAdapterServer, bool) {
	switch family {
	case winAFInet:
		return dnsAdapterServer{addr: netip.AddrFrom4(v4), metric: v4Metric}, true
	case winAFInet6:
		addr := netip.AddrFrom16(v6)
		if addr.IsLinkLocalUnicast() && scope != 0 {
			addr = addr.WithZone(strconv.FormatUint(uint64(scope), 10))
		}
		return dnsAdapterServer{addr: addr, metric: v6Metric}, true
	}
	return dnsAdapterServer{}, false
}

// isSiteLocal reports whether addr is an IPv6 site-local address, fec0::/10.
// An IPv4 address is not one: As16 gives it as ::ffff:a.b.c.d.
func isSiteLocal(addr netip.Addr) bool {
	b := addr.As16()
	return b[0] == 0xfe && b[1]&0xc0 == 0xc0
}
