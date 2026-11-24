package containerkey

import (
	"net"
	"net/netip"
)

// While a watch's first list of the daemon is out (a daemon slow to list: a
// Docker Desktop waking its VM), a native agent's connection waits for it
// only if its destination may be a container's, as this host tells without
// the daemon (MayBeContainer): an app that reaches no container never waits
// on Docker.
//
// A container is reached in one of two ways. At its own address, on one of
// the daemon's networks, which a host routes to the network's bridge (or, a
// Docker in a VM that routes its containers' addresses, to the VM): never
// through the host's default route, nor on the network the default route
// leaves by. Or at a TCP port the daemon publishes on this host's own
// addresses (loopback among them): this host cannot tell which ports those
// are without the daemon (a VM's forwarder, docker-proxy, NAT rules), so
// every port there may be one, and its connection waits for the list. An
// address this host has no route to, or sends to through its default route
// (a public host, a database in the VPC, a host on the LAN), is no
// container's, and never waits.

// reach is how a dockerIndex tells, without the daemon, whether an address
// may be a container's own (MayBeContainer); a variable for tests.
type reach struct {
	// source is the address this host sends from to reach a, ok false where
	// it has no route to a (sourceAddr); nil cannot tell.
	source func(a netip.Addr) (src netip.Addr, ok bool)
}

// hostReach is how this host tells it: by its routes.
func hostReach() reach { return reach{source: sourceAddr} }

// MayBeContainer reports whether a container may be at dst as this host
// tells, while the current watch's first list is out: a connection to dst
// waits for that list (Settle) only if so (Keyer.place, the handshake hold).
// A port on this host's own addresses may always be one a container
// publishes. Once the list is in, the index alone says what dst is, and so
// does it for a watch no native session started or one that cannot be read
// (a docker-mode agent's, whose loopback is the app container's own): it
// reports true.
func (r *dockerIndex) MayBeContainer(dst netip.AddrPort) bool {
	r.mu.Lock()
	st, rc := r.cur, r.reach
	unlisted := st != nil && st.err == nil && st.api != nil && !st.listed && st.loopbackIsHost && st.native
	local := unlisted && st.local[dst.Addr().Unmap()]
	r.mu.Unlock()
	a := dst.Addr().Unmap()
	if !unlisted || a.IsLoopback() || a.IsUnspecified() || local || rc.source == nil {
		return true
	}
	src, ok := rc.source(a)
	if !ok {
		// No route from this host: nothing it reaches, a container least
		// of all.
		return false
	}
	// This host sending to a from a itself makes a one of its own.
	return src == a || rc.routedApart(a, src)
}

// routedApart reports whether this host sends to a, from src, apart from its
// default route: through another interface than the one the default route
// leaves by, as to a Docker network's bridge. A host with no default route
// routes every address apart.
func (rc reach) routedApart(a, src netip.Addr) bool {
	probe := defaultProbe4
	if a.Is6() {
		probe = defaultProbe6
	}
	def, ok := rc.source(probe)
	return !ok || src != def
}

// The addresses whose route is the host's default one: documentation
// addresses (RFC 5737, RFC 3849), which no network routes on its own. Asking
// the route to one sends nothing (sourceAddr).
var (
	defaultProbe4 = netip.MustParseAddr("192.0.2.1")
	defaultProbe6 = netip.MustParseAddr("2001:db8::1")
)

// sourceAddr is the address this host sends from to reach a, as its routes
// pick it: a datagram socket connected to a, which sends nothing. ok is false
// where the host has no route to a.
func sourceAddr(a netip.Addr) (netip.Addr, bool) {
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(a, 9)))
	if err != nil {
		return netip.Addr{}, false
	}
	defer func() { _ = c.Close() }()
	la, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	src := la.AddrPort().Addr().Unmap()
	return src, src.IsValid() && !src.IsUnspecified()
}
