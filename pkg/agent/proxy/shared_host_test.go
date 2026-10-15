package proxy

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"testing"
)

// The routing tables of two containers, read on a Linux Docker host: one on
// Docker's default bridge, where host.docker.internal (--add-host
// host.docker.internal:host-gateway) resolved to its gateway, 172.17.0.1; one
// on a network of its own with IPv6, whose gateways are 172.19.0.1 and
// fde2:5ad0:1727::1. The IPv4 words are as an amd64 (little-endian) kernel
// prints them.
const (
	bridgeRoute = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	bridgeIPv6Route = "00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo\n" +
		"00000000000000000000000000000001 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000002 00000000 80200001       lo\n"
	networkRoute = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t010013AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t000013AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	networkIPv6Route = "fde25ad0172700000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000003 00000000 00000001     eth0\n" +
		"fe800000000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0\n" +
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 fde25ad0172700000000000000000001 00000400 00000001 00000000 00000003     eth0\n" +
		"00000000000000000000000000000001 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000004 00000000 80200001       lo\n" +
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo\n"
)

// A container's gateway is read from its default routes: the IPv4 one's
// gateway, and the IPv6 one's next hop. A route to a subnet, or a default
// route with no gateway (the IPv6 reject routes on lo), names none.
func TestDefaultGatewaysAreTheDefaultRoutesGateways(t *testing.T) {
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("the IPv4 words below are as a little-endian kernel prints them")
	}
	for _, tc := range []struct {
		name   string
		v4, v6 string
		want   []string
	}{
		{"the default bridge", bridgeRoute, bridgeIPv6Route, []string{"172.17.0.1"}},
		{"a network of its own, with IPv6", networkRoute, networkIPv6Route, []string{"172.19.0.1", "fde2:5ad0:1727::1"}},
		{"no table", "", "", nil},
		{"no default route", "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\neth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n", "", nil},
		{"a default route without the gateway flag", "eth0\t00000000\t010011AC\t0001\t0\t0\t0\t00000000\t0\t0\t0\n", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, a := range defaultGateways([]byte(tc.v4), []byte(tc.v6)) {
				got = append(got, a.String())
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("gateways %v, want %v", got, tc.want)
			}
		})
	}
}

// The kernel prints an IPv4 route's words in the machine's own byte order, so
// a gateway reads back as the address on any machine.
func TestDefaultGatewaysReadTheKernelsByteOrder(t *testing.T) {
	gw := netip.MustParseAddr("172.17.0.1")
	word := binary.NativeEndian.Uint32(gw.AsSlice())
	v4 := fmt.Sprintf("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\neth0\t00000000\t%08X\t0003\t0\t0\t0\t00000000\t0\t0\t0\n", word)
	if got := defaultGateways([]byte(v4), nil); len(got) != 1 || got[0] != gw {
		t.Fatalf("gateways %v, want [%v]", got, gw)
	}
}

// This machine is a loopback or unspecified address, and every address of the
// namespace the agent runs in, read afresh at each call: the read takes the
// interfaces' addresses (lo's 127.0.0.1 is one in any namespace, and does not
// come and go while the test runs, as a veth's or a bridge's does on a busy
// host).
func TestThisMachineIsEveryAddressOfItsOwn(t *testing.T) {
	p := &Proxy{}
	for _, a := range []string{"127.0.0.1", "::1", "0.0.0.0", "::", "::ffff:127.0.0.1"} {
		if _, ok := p.sharedHost(netip.MustParseAddr(a)); !ok {
			t.Errorf("%s is not this machine", a)
		}
	}
	if _, ok := p.sharedHost(netip.Addr{}); ok {
		t.Error("no address is a shared host")
	}
	if own, _ := readNetns(); !slices.Contains(own, netip.MustParseAddr("127.0.0.1")) {
		t.Errorf("the namespace's addresses %v hold no 127.0.0.1: its interfaces were not read", own)
	}

	lan := netip.MustParseAddr("192.168.1.10")
	reads := 0
	p.readNetns = func() (own, gateways []netip.Addr) { reads++; return []netip.Addr{lan}, nil }
	for range 2 {
		if name, ok := p.sharedHost(netip.MustParseAddr("::ffff:192.168.1.10")); !ok || name != "this machine (192.168.1.10, an address of its own)" {
			t.Fatalf("an address of this machine's own is %q, %v", name, ok)
		}
	}
	if reads != 2 {
		t.Fatalf("two calls read the namespace %d time(s); each reads it afresh", reads)
	}
	if name, ok := p.sharedHost(netip.MustParseAddr("192.168.1.11")); ok {
		t.Fatalf("another machine's address was taken for this one (%s)", name)
	}
}

// A gateway is the Docker host only in docker mode.
func TestAGatewayIsTheDockerHostOnlyInDockerMode(t *testing.T) {
	gw := netip.MustParseAddr("172.17.0.1")
	p := &Proxy{readNetns: func() (own, gateways []netip.Addr) { return nil, []netip.Addr{gw} }}
	if name, ok := p.sharedHost(gw); ok {
		t.Fatalf("a native application's gateway was taken for a shared host (%s)", name)
	}
	p.IsDocker = true
	name, ok := p.sharedHost(netip.MustParseAddr("::ffff:172.17.0.1"))
	if !ok || name != "the Docker host (172.17.0.1, the container's gateway)" {
		t.Fatalf("in docker mode the gateway is %q, %v; want the Docker host", name, ok)
	}
	if name, ok := p.sharedHost(netip.MustParseAddr("10.1.2.3")); ok {
		t.Fatalf("another host was taken for a shared one (%s)", name)
	}
}

// On a network of its own a container's gateway is that network's, but the
// Docker host is reached at its other addresses too (the default bridge's
// 172.17.0.1, where host-gateway points; its LAN address): those the keploy
// CLI gives a docker-mode agent are the Docker host, and only in docker mode.
func TestTheDockerHostsOwnAddressesAreTheDockerHost(t *testing.T) {
	bridge := netip.MustParseAddr("172.17.0.1")
	p := &Proxy{
		readNetns:      func() (own, gateways []netip.Addr) { return nil, []netip.Addr{netip.MustParseAddr("172.19.0.1")} },
		dockerHostAddr: func(a netip.Addr) bool { return a == bridge },
	}
	if name, ok := p.sharedHost(bridge); ok {
		t.Fatalf("natively, an address the CLI gave was taken for the Docker host (%s)", name)
	}
	p.IsDocker = true
	if name, ok := p.sharedHost(netip.MustParseAddr("::ffff:172.17.0.1")); !ok || name != "the Docker host (172.17.0.1, an address of its own)" {
		t.Fatalf("the Docker host's own address is %q, %v; want the Docker host", name, ok)
	}
}
