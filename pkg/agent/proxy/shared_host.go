package proxy

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
)

// sharedHost names the machine a is an address of when every service on that
// machine is reached at it: a MySQL moved to another port there says nothing
// about which service it is, so replay never takes it for the recorded one
// (recordedEndpointMoved), and only mysqlPorts makes it MySQL. That machine is
//
//   - this one: a loopback or unspecified address, or an address of the
//     network namespace the application runs in, which the agent shares
//     (natively, the host's own, the Docker bridge's 172.17.0.1 among them;
//     in docker mode, the application container's own);
//   - in docker mode, the Docker host, at a default gateway of that
//     namespace: a container reaches the host at its network's gateway, which
//     on Docker's default bridge is where host.docker.internal and
//     host-gateway point too (172.17.0.1); or at the host's other addresses,
//     as a container on a network of its own reaches them (the default
//     bridge's 172.17.0.1, where host-gateway still points, or the host's LAN
//     address), where the keploy CLI gave them to the agent
//     (containerkey.HostAddr: it gives them under the proven switch).
//
// A native application's default gateway is a router, another machine, so a
// gateway counts only in docker mode. The host's other addresses where the
// CLI gave the agent none (the proven switch off, or a daemon the CLI cannot
// read as this host's: its list then carries no addresses), and Docker
// Desktop's host.docker.internal, which is no gateway, are not told from
// another machine's here. The namespace is read at
// each call, for a host the recording holds (recordedEndpointMoved) or for the
// next step of a connection keploy could not tie to the recording
// (unlistedNextStep): nothing is cached.
func (p *Proxy) sharedHost(a netip.Addr) (name string, ok bool) {
	a = a.Unmap()
	if !a.IsValid() {
		return "", false
	}
	if a.IsLoopback() || a.IsUnspecified() {
		return "this machine (localhost)", true
	}
	read := p.readNetns
	if read == nil {
		read = readNetns
	}
	own, gateways := read()
	if slices.Contains(own, a) {
		return "this machine (" + a.String() + ", an address of its own)", true
	}
	if !p.IsDocker {
		return "", false
	}
	if slices.Contains(gateways, a) {
		return "the Docker host (" + a.String() + ", the container's gateway)", true
	}
	hostAddr := p.dockerHostAddr
	if hostAddr == nil {
		hostAddr = containerkey.HostAddr
	}
	if hostAddr(a) {
		return "the Docker host (" + a.String() + ", an address of its own)", true
	}
	return "", false
}

// readNetns reads the network namespace the agent runs in: its interfaces'
// addresses, and the gateways of its default routes. A read that fails gives
// nothing.
func readNetns() (own, gateways []netip.Addr) {
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok {
					own = append(own, ip.Unmap())
				}
			}
		}
	}
	v4, _ := os.ReadFile("/proc/net/route")
	v6, _ := os.ReadFile("/proc/net/ipv6_route")
	return own, defaultGateways(v4, v6)
}

// rtfGateway is the kernel's RTF_GATEWAY route flag: the route goes through
// a gateway.
const rtfGateway = 0x2

// defaultGateways is the gateways of the default routes in the kernel's IPv4
// (/proc/net/route) and IPv6 (/proc/net/ipv6_route) routing tables.
func defaultGateways(v4, v6 []byte) []netip.Addr {
	var out []netip.Addr
	// Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT,
	// the addresses as the kernel's 32-bit words, printed in host byte order.
	// The header line parses as no route.
	for _, line := range strings.Split(string(v4), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		dst, err1 := strconv.ParseUint(f[1], 16, 32)
		gw, err2 := strconv.ParseUint(f[2], 16, 32)
		flags, err3 := strconv.ParseUint(f[3], 16, 32)
		mask, err4 := strconv.ParseUint(f[7], 16, 32)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || dst != 0 || mask != 0 || flags&rtfGateway == 0 {
			continue
		}
		var b [4]byte
		binary.NativeEndian.PutUint32(b[:], uint32(gw))
		if a := netip.AddrFrom4(b); !a.IsUnspecified() {
			out = append(out, a)
		}
	}
	// Destination, its prefix length, source, its prefix length, next hop,
	// metric, refcount, use, flags, iface: the addresses in network byte
	// order.
	for _, line := range strings.Split(string(v6), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[1] != "00" || strings.Trim(f[0], "0") != "" {
			continue
		}
		flags, err := strconv.ParseUint(f[8], 16, 32)
		hop, err2 := hex.DecodeString(f[4])
		if err != nil || err2 != nil || len(hop) != 16 || flags&rtfGateway == 0 {
			continue
		}
		if a := netip.AddrFrom16([16]byte(hop)).Unmap(); !a.IsUnspecified() {
			out = append(out, a)
		}
	}
	return out
}
