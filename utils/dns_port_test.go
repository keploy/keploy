package utils

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
)

// udpPicks has udpListen refuse the ports in taken, as a UDP socket holding
// each would, and records every port it was asked for. The sockets it opens
// are stand-ins whose Close fails where closeFails says.
func udpPicks(t *testing.T, closeFails bool, taken ...uint32) *[]uint32 {
	t.Helper()
	was := udpListen
	t.Cleanup(func() { udpListen = was })
	asked := &[]uint32{}
	udpListen = func(network, address string) (net.PacketConn, error) {
		var port uint32
		if _, err := fmt.Sscanf(address, ":%d", &port); err != nil || network != "udp" {
			return nil, fmt.Errorf("unexpected UDP bind on %s %s", network, address)
		}
		*asked = append(*asked, port)
		if slices.Contains(taken, port) {
			return nil, fmt.Errorf("listen udp %s: bind: address already in use", address)
		}
		return &udpStandIn{port: int(port), closeFails: closeFails}, nil
	}
	return asked
}

// udpStandIn is a UDP socket udpPicks opened.
type udpStandIn struct {
	net.PacketConn
	port       int
	closeFails bool
}

func (c *udpStandIn) Close() error {
	if c.closeFails {
		return errors.New("close failed")
	}
	return nil
}

func (c *udpStandIn) LocalAddr() net.Addr { return &net.UDPAddr{Port: c.port} }

// A DNS server binds its port over UDP and over TCP: its probe binds a UDP
// socket too, while its TCP listener holds the port, and finds a port a UDP
// socket holds taken. A probe for TCP alone binds no UDP socket. A UDP socket
// that is not let go is an error, as a TCP listener's is.
func TestADNSPortsProbeBindsUDPBesideTCP(t *testing.T) {
	const port = 61001
	for _, c := range []struct {
		name                  string
		udp, held, closeFails bool
		free, failed          bool
		asked                 int
	}{
		{name: "for TCP alone, held over UDP", held: true, free: true},
		{name: "free over both", udp: true, free: true, asked: 1},
		{name: "held over UDP", udp: true, held: true, asked: 1},
		{name: "a UDP socket not let go", udp: true, closeFails: true, failed: true, asked: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			var taken []uint32
			if c.held {
				taken = []uint32{port}
			}
			asked := udpPicks(t, c.closeFails, taken...)
			wasTCP := portListen
			wasLoopback := loopbackListen
			t.Cleanup(func() { portListen = wasTCP; loopbackListen = wasLoopback })
			var opened []*pickListener
			portListen = func(_ context.Context, network, address string) (net.Listener, error) {
				if network != "tcp" || address != fmt.Sprintf(":%d", port) {
					return nil, fmt.Errorf("unexpected TCP listen on %s %s", network, address)
				}
				l := &pickListener{port: port}
				opened = append(opened, l)
				return l, nil
			}
			loopbackListen = func(_ context.Context, network, address string) (net.Listener, error) {
				if network != "tcp4" || address != fmt.Sprintf("127.0.0.1:%d", port) {
					return nil, fmt.Errorf("unexpected loopback listen on %s %s", network, address)
				}
				return &pickListener{port: port}, nil
			}
			wasUDP := udpListen
			udpListen = func(network, address string) (net.PacketConn, error) {
				if len(opened) != 1 || opened[0].closed {
					t.Errorf("a UDP socket was bound on %s with no TCP listener holding the port", address)
				}
				return wasUDP(network, address)
			}
			free, err := probePort(port, c.udp)
			if free != c.free || (err != nil) != c.failed || len(*asked) != c.asked {
				t.Fatalf("probePort(%d, udp %v) = %v, %v after %d UDP binds; want %v, failed %v, after %d", port, c.udp, free, err, len(*asked), c.free, c.failed, c.asked)
			}
			if len(opened) != 1 || !opened[0].closed {
				t.Fatalf("the probe's TCP listener was not let go")
			}
		})
	}
}

// Where the OS picks a DNS server's port, as off Linux, a pick a UDP socket
// holds is passed over, held while the OS is asked again so that it cannot
// hand it back; a pick for TCP alone binds no UDP socket. Where every pick is
// held over UDP it is an error after udpHeldPicks of them, not a hang.
func TestTheOSsPickForADNSPortIsFreeOverUDP(t *testing.T) {
	unknown := func() (uint32, uint32, bool) { return 0, 0, false }
	picks := osPicks(t, 40001, 40002, 40003)
	asked := udpPicks(t, false, 40002)
	if got, err := availablePortAbove(unknown, 0, true, 40001); err != nil || got != 40003 || len(*picks) != 3 || !slices.Equal(*asked, []uint32{40002, 40003}) {
		t.Fatalf("got port %d, err %v, after %d picks and UDP binds on %v; want 40003, the excluded 40001 and the UDP-held 40002 passed over", got, err, len(*picks), *asked)
	}
	for _, l := range *picks {
		if !l.closed {
			t.Fatalf("the pick on %d was not let go", l.port)
		}
	}

	picks = osPicks(t, 40002)
	asked = udpPicks(t, false, 40002)
	if got, err := availablePortAbove(unknown, 0, false); err != nil || got != 40002 || len(*picks) != 1 || len(*asked) != 0 {
		t.Fatalf("got port %d, err %v, after %d picks and UDP binds on %v; want 40002, the OS's one pick, with no UDP bind", got, err, len(*picks), *asked)
	}

	all := []int{}
	allHeld := []uint32{}
	for p := 40001; p <= 40001+udpHeldPicks+1; p++ {
		all, allHeld = append(all, p), append(allHeld, uint32(p))
	}
	picks = osPicks(t, all...)
	udpPicks(t, false, allHeld...)
	got, err := availablePortAbove(unknown, 0, true)
	if err == nil || !strings.Contains(err.Error(), "free over UDP and TCP") || len(*picks) != 1+udpHeldPicks || udpHeldPicks < 1 {
		t.Fatalf("got port %d, err %v, after %d picks, each held over UDP; want an error after %d", got, err, len(*picks), 1+udpHeldPicks)
	}

	picks = osPicks(t, 40001)
	udpPicks(t, true)
	if got, err := availablePortAbove(unknown, 0, true); err == nil || !strings.Contains(err.Error(), "UDP port 40001") || len(*picks) != 1 {
		t.Fatalf("got port %d, err %v, after %d picks, the UDP socket not let go; want its error", got, err, len(*picks))
	}
}

// The DNS server's pickers probe their candidates over UDP and TCP, above the
// range as off it; the others over TCP alone.
func TestTheDNSPickersProbeOverUDPToo(t *testing.T) {
	r, o, p := hostEphemeralRange, portOffset, portProbe
	t.Cleanup(func() { hostEphemeralRange, portOffset, portProbe = r, o, p })
	hostEphemeralRange = func() (uint32, uint32, bool) { return 32768, 60999, true }
	never := func(uint32) bool { return false }
	for _, c := range []struct {
		name string
		pick func() (uint32, error)
		udp  bool
	}{
		{"GetAvailablePortExcept", func() (uint32, error) { return GetAvailablePortExcept() }, false},
		{"GetAvailableContainerPortExcept", func() (uint32, error) { return GetAvailableContainerPortExcept() }, false},
		{"GetAvailableDNSPortExcept", func() (uint32, error) { return GetAvailableDNSPortExcept() }, true},
		{"EnsureFreePort", func() (uint32, error) { return EnsureFreePort(never, 61500) }, false},
		{"EnsureFreeDNSPort", func() (uint32, error) { return EnsureFreeDNSPort(never, 61500) }, true},
	} {
		// A random candidate, taken; then the scan's first, free.
		offsets := []uint32{5}
		portOffset = func(uint32) (uint32, bool) {
			if len(offsets) == 0 {
				return 0, false
			}
			off := offsets[0]
			offsets = offsets[1:]
			return off, true
		}
		var probes []bool
		portProbe = func(_ uint32, udp bool) (bool, error) {
			probes = append(probes, udp)
			return len(probes) == 2, nil
		}
		if _, err := c.pick(); err != nil || !slices.Equal(probes, []bool{c.udp, c.udp}) {
			t.Fatalf("%s: err %v, probes over UDP too %v; want 2 probes, each %v", c.name, err, probes, c.udp)
		}
	}
}

// A port a UDP socket alone holds is not a DNS server's: the check moves off
// it. The socket is this test's, held until the test ends.
func TestADNSPortHeldOverUDPAloneIsMovedOff(t *testing.T) {
	pc, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	held := uint32(pc.LocalAddr().(*net.UDPAddr).Port)
	if isDNSPortAvailable(held) {
		t.Fatalf("isDNSPortAvailable(%d) while this test holds it over UDP; want false", held)
	}
	if got, err := EnsureAvailableDNSPort(held); err != nil || got == held {
		t.Fatalf("EnsureAvailableDNSPort(%d) = %d, %v while this test holds it over UDP; want another port", held, got, err)
	}
}
