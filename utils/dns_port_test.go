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
	closed     bool
	closeFails bool
}

func (c *udpStandIn) Close() error {
	c.closed = true
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
			t.Cleanup(func() { portListen = wasTCP })
			var opened []*pickListener
			portListen = func(_ context.Context, network, address string) (net.Listener, error) {
				if network != "tcp" || address != fmt.Sprintf(":%d", port) {
					return nil, fmt.Errorf("unexpected TCP listen on %s %s", network, address)
				}
				l := &pickListener{port: port}
				opened = append(opened, l)
				return l, nil
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

// Where the OS picks a DNS server's port, as on Windows and macOS, it binds
// UDP ":0" first and checks TCP on that exact port while holding the UDP
// socket (so Windows UDP excluded port ranges at the TCP cursor are bypassed
// natively). If the UDP cursor is stuck in a TCP-blocked run, it falls back
// to TCP ":0" + UDP check; if both sequential cursors sit inside blocked runs,
// it jumps out via random probes over 49152-65535 and 16384-49151. Every held
// socket is closed before returning, Close errors surface immediately, and
// total exhaustion returns a clear error rather than hanging.
func TestTheOSsPickForADNSPortIsFreeOverUDP(t *testing.T) {
	unknown := func() (uint32, uint32, bool) { return 0, 0, false }

	t.Run("stage1_udp_first_skips_excluded_and_tcp_held", func(t *testing.T) {
		wasUDP, wasTCP := udpListen, portListen
		t.Cleanup(func() { udpListen, portListen = wasUDP, wasTCP })

		udpSeq := []int{59001, 59002, 59003}
		var openedUDP []*udpStandIn
		udpListen = func(network, address string) (net.PacketConn, error) {
			if network != "udp" || address != ":0" {
				return nil, fmt.Errorf("unexpected UDP bind on %s %s", network, address)
			}
			if n := len(openedUDP); n > 0 && openedUDP[n-1].closed {
				t.Errorf("previous rejected UDP pick %d was closed before next :0 ask", openedUDP[n-1].port)
			}
			if len(udpSeq) == 0 {
				return nil, errors.New("exhausted")
			}
			c := &udpStandIn{port: udpSeq[0]}
			udpSeq = udpSeq[1:]
			openedUDP = append(openedUDP, c)
			return c, nil
		}
		var checkedTCP []uint32
		portListen = func(_ context.Context, network, address string) (net.Listener, error) {
			var p uint32
			if _, err := fmt.Sscanf(address, ":%d", &p); err != nil || network != "tcp" {
				return nil, fmt.Errorf("unexpected TCP listen on %s %s", network, address)
			}
			// Verify the matching UDP socket is STILL open while TCP is bound.
			foundOpen := false
			for _, u := range openedUDP {
				if uint32(u.port) == p && !u.closed {
					foundOpen = true
				}
			}
			if !foundOpen {
				t.Errorf("TCP probe on %d ran without UDP socket held open", p)
			}
			checkedTCP = append(checkedTCP, p)
			if p == 59002 {
				return nil, errors.New("address already in use")
			}
			return &pickListener{port: int(p)}, nil
		}

		got, err := availablePortAbove(unknown, 0, true, 59001)
		if err != nil || got != 59003 || len(openedUDP) != 3 || !slices.Equal(checkedTCP, []uint32{59002, 59003}) {
			t.Fatalf("got port %d, err %v, udpPicks=%d tcpChecked=%v; want 59003", got, err, len(openedUDP), checkedTCP)
		}
		for _, u := range openedUDP {
			if !u.closed {
				t.Fatalf("UDP socket on %d was not closed", u.port)
			}
		}
	})

	t.Run("tcp_only_binds_no_udp_socket", func(t *testing.T) {
		picks := osPicks(t, 40002)
		asked := udpPicks(t, false, 40002)
		if got, err := availablePortAbove(unknown, 0, false); err != nil || got != 40002 || len(*picks) != 1 || len(*asked) != 0 {
			t.Fatalf("got port %d, err %v, after %d picks and UDP binds on %v; want 40002 with no UDP bind", got, err, len(*picks), *asked)
		}
	})

	t.Run("stage2_falls_back_to_tcp_first_when_udp_cursor_is_tcp_blocked", func(t *testing.T) {
		wasUDP, wasTCP := udpListen, portListen
		t.Cleanup(func() { udpListen, portListen = wasUDP, wasTCP })

		nextUDP := 55000
		nextTCP := 61000
		var openedTCP []*pickListener
		var checkedUDP []uint32
		udpListen = func(network, address string) (net.PacketConn, error) {
			if address == ":0" {
				p := nextUDP
				nextUDP++
				return &udpStandIn{port: p}, nil
			}
			var p uint32
			if _, err := fmt.Sscanf(address, ":%d", &p); err != nil {
				return nil, err
			}
			// Verify the matching TCP listener is STILL held open while UDP is probed.
			foundOpen := false
			for _, l := range openedTCP {
				if uint32(l.port) == p && !l.closed {
					foundOpen = true
				}
			}
			if !foundOpen {
				t.Errorf("UDP probe on %d ran without TCP listener held open", p)
			}
			checkedUDP = append(checkedUDP, p)
			if p == 61001 {
				return nil, errors.New("udp held")
			}
			return &udpStandIn{port: int(p)}, nil
		}
		portListen = func(_ context.Context, network, address string) (net.Listener, error) {
			if address == ":0" {
				// Verify every previously rejected TCP :0 pick stays held until stage 2 returns.
				if n := len(openedTCP); n > 0 && openedTCP[n-1].closed {
					t.Errorf("previous rejected TCP pick %d was closed before next :0 ask", openedTCP[n-1].port)
				}
				l := &pickListener{port: nextTCP}
				nextTCP++
				openedTCP = append(openedTCP, l)
				return l, nil
			}
			// Every port in the 55000..55099 UDP cursor block is excluded on TCP!
			var p uint32
			if _, err := fmt.Sscanf(address, ":%d", &p); err != nil {
				return nil, err
			}
			if p >= 55000 && p < 55100 {
				return nil, errors.New("tcp excluded")
			}
			return &pickListener{port: int(p)}, nil
		}

		// 61000 is excluded (must be skipped without probing UDP); 61001 is held over UDP; 61002 wins.
		got, err := availablePortAbove(unknown, 0, true, 61000)
		if err != nil || got != 61002 || len(openedTCP) != 3 || !slices.Equal(checkedUDP, []uint32{61001, 61002}) {
			t.Fatalf("stage2 fallback = %d, %v, tcpPicks=%d udpChecked=%v; want 61002 with udpChecked=[61001 61002]", got, err, len(openedTCP), checkedUDP)
		}
		for _, l := range openedTCP {
			if !l.closed {
				t.Fatalf("TCP listener on %d was not closed", l.port)
			}
		}
	})

	t.Run("stage3_random_probes_jump_out_of_double_excluded_blocks", func(t *testing.T) {
		wasUDP, wasTCP, wasOff, wasProbe := udpListen, portListen, portOffset, portProbe
		t.Cleanup(func() { udpListen, portListen, portOffset, portProbe = wasUDP, wasTCP, wasOff, wasProbe })

		nextUDP := 55000
		nextTCP := 63300
		udpListen = func(network, address string) (net.PacketConn, error) {
			if address == ":0" {
				p := nextUDP
				nextUDP++
				return &udpStandIn{port: p}, nil
			}
			// Every TCP cursor pick in 63300..63399 is excluded on UDP!
			return nil, errors.New("WSAEACCES udp excluded")
		}
		portListen = func(_ context.Context, network, address string) (net.Listener, error) {
			if address == ":0" {
				p := nextTCP
				nextTCP++
				return &pickListener{port: p}, nil
			}
			// Every UDP cursor pick in 55000..55099 is excluded on TCP!
			return nil, errors.New("WSAEACCES tcp excluded")
		}
		portOffset = func(span uint32) (uint32, bool) { return 1000, true }
		portProbe = func(port uint32, udp bool) (bool, error) {
			if !udp {
				t.Errorf("stage3 probe called with udp=false")
			}
			return port == dynamicPortLo+1000, nil
		}

		got, err := availablePortAbove(unknown, 0, true, 63313, 63307)
		if err != nil || got != dynamicPortLo+1000 {
			t.Fatalf("stage3 fallback = %d, %v; want %d", got, err, dynamicPortLo+1000)
		}
	})

	t.Run("stage3_unprivileged_range_fallback_probes_16384_to_49151_over_udp_and_tcp", func(t *testing.T) {
		wasUDP, wasTCP, wasOff, wasProbe := udpListen, portListen, portOffset, portProbe
		t.Cleanup(func() { udpListen, portListen, portOffset, portProbe = wasUDP, wasTCP, wasOff, wasProbe })

		udpListen = func(network, address string) (net.PacketConn, error) {
			return nil, errors.New("udp ephemeral exhausted")
		}
		portListen = func(_ context.Context, network, address string) (net.Listener, error) {
			return nil, errors.New("tcp ephemeral exhausted")
		}
		// Deterministic scan starting at each range's lower bound so a shifted floor
		// (e.g. 1024 instead of 16384) is caught on the very first probe of the fallback range.
		portOffset = func(uint32) (uint32, bool) { return 0, false }
		var probedBelowFloor []uint32
		var probedNonUDP []uint32
		var probedExcluded bool
		portProbe = func(port uint32, udp bool) (bool, error) {
			if !udp {
				probedNonUDP = append(probedNonUDP, port)
			}
			if port < 16384 {
				probedBelowFloor = append(probedBelowFloor, port)
			}
			if port == 16384 {
				probedExcluded = true
			}
			// Dynamic range 49152-65535 is fully taken; only 16385 in 16384-49151 is free.
			return port == 16385, nil
		}

		got, err := availablePortAbove(unknown, 0, true, 16384)
		if err != nil || got != 16385 {
			t.Fatalf("stage3 16384-49151 fallback = %d, %v; want 16385", got, err)
		}
		if len(probedNonUDP) > 0 {
			t.Fatalf("fallback range probed without UDP on ports (first 5): %v", probedNonUDP[:min(5, len(probedNonUDP))])
		}
		if len(probedBelowFloor) > 0 {
			t.Fatalf("fallback range probed below 16384 floor (first 5): %v", probedBelowFloor[:min(5, len(probedBelowFloor))])
		}
		if probedExcluded {
			t.Fatalf("fallback range probed excluded port 16384")
		}
	})

	t.Run("all_stages_exhausted_returns_error_without_hanging", func(t *testing.T) {
		wasUDP, wasTCP, wasOff, wasProbe := udpListen, portListen, portOffset, portProbe
		t.Cleanup(func() { udpListen, portListen, portOffset, portProbe = wasUDP, wasTCP, wasOff, wasProbe })

		nextUDP := 55000
		nextTCP := 63300
		udpListen = func(network, address string) (net.PacketConn, error) {
			if address == ":0" {
				p := nextUDP
				nextUDP++
				return &udpStandIn{port: p}, nil
			}
			return nil, errors.New("udp blocked")
		}
		portListen = func(_ context.Context, network, address string) (net.Listener, error) {
			if address == ":0" {
				p := nextTCP
				nextTCP++
				return &pickListener{port: p}, nil
			}
			return nil, errors.New("tcp blocked")
		}
		portOffset = func(uint32) (uint32, bool) { return 0, false }
		portProbe = func(uint32, bool) (bool, error) { return false, nil }

		got, err := availablePortAbove(unknown, 0, true)
		if err == nil || !strings.Contains(err.Error(), "free over UDP and TCP") ||
			!strings.Contains(err.Error(), "after 9 UDP and 9 TCP OS picks") ||
			!strings.Contains(err.Error(), "16384-65535") {
			t.Fatalf("exhausted = %d, %v; want error naming UDP and TCP, actual pick counts (9/9), and 16384-65535", got, err)
		}
	})

	t.Run("listen_zero_errors_preserved_and_actual_pick_counts_reported", func(t *testing.T) {
		wasUDP, wasTCP, wasOff, wasProbe := udpListen, portListen, portOffset, portProbe
		t.Cleanup(func() { udpListen, portListen, portOffset, portProbe = wasUDP, wasTCP, wasOff, wasProbe })

		errUDPZero := errors.New("WSAENOBUFS udp")
		errTCPZero := errors.New("WSAENOBUFS tcp")
		portOffset = func(uint32) (uint32, bool) { return 0, false }
		portProbe = func(uint32, bool) (bool, error) { return false, nil }

		// Both :0 listens fail immediately: 0 picks obtained, both OS errors wrapped, exact range named.
		udpListen = func(network, address string) (net.PacketConn, error) { return nil, errUDPZero }
		portListen = func(_ context.Context, network, address string) (net.Listener, error) { return nil, errTCPZero }

		got, err := availablePortAbove(unknown, 0, true, 16789, 26789)
		if err == nil || got != 0 {
			t.Fatalf("both :0 failed = %d, %v; want error", got, err)
		}
		if !errors.Is(err, errUDPZero) || !errors.Is(err, errTCPZero) {
			t.Fatalf("final error dropped :0 OS errors: %v", err)
		}
		if !strings.Contains(err.Error(), "after 0 UDP and 0 TCP OS picks") || !strings.Contains(err.Error(), "16384-65535") {
			t.Fatalf("final error has inaccurate counts or range: %v", err)
		}

		// Partial stage 1 (2 UDP picks before :0 fails) + full stage 2 (9 TCP picks, all UDP-blocked).
		udpAsks := 0
		nextTCP := 62000
		udpListen = func(network, address string) (net.PacketConn, error) {
			if address == ":0" {
				udpAsks++
				if udpAsks <= 2 {
					return &udpStandIn{port: 54000 + udpAsks}, nil
				}
				return nil, errUDPZero
			}
			return nil, errors.New("udp blocked")
		}
		portListen = func(_ context.Context, network, address string) (net.Listener, error) {
			if address == ":0" {
				p := nextTCP
				nextTCP++
				return &pickListener{port: p}, nil
			}
			return nil, errors.New("tcp blocked")
		}

		got, err = availablePortAbove(unknown, 0, true)
		if err == nil || got != 0 {
			t.Fatalf("partial stage1 + full stage2 = %d, %v; want error", got, err)
		}
		if !errors.Is(err, errUDPZero) {
			t.Fatalf("final error dropped stage1 :0 OS error: %v", err)
		}
		if !strings.Contains(err.Error(), "after 2 UDP and 9 TCP OS picks") || !strings.Contains(err.Error(), "16384-65535") {
			t.Fatalf("final error has inaccurate counts or range: %v", err)
		}

		// Full stage 1 (9 UDP picks, all TCP-blocked) + stage 2 :0 fails immediately (0 TCP picks).
		nextUDP := 56000
		udpListen = func(network, address string) (net.PacketConn, error) {
			if address == ":0" {
				p := nextUDP
				nextUDP++
				return &udpStandIn{port: p}, nil
			}
			return nil, errors.New("udp blocked")
		}
		portListen = func(_ context.Context, network, address string) (net.Listener, error) {
			if address == ":0" {
				return nil, errTCPZero
			}
			return nil, errors.New("tcp blocked")
		}

		got, err = availablePortAbove(unknown, 0, true)
		if err == nil || got != 0 {
			t.Fatalf("full stage1 + stage2 :0 failed = %d, %v; want error", got, err)
		}
		if !errors.Is(err, errTCPZero) || errors.Is(err, errUDPZero) {
			t.Fatalf("final error did not preserve only stage2 :0 OS error: %v", err)
		}
		if !strings.Contains(err.Error(), "after 9 UDP and 0 TCP OS picks") || !strings.Contains(err.Error(), "16384-65535") {
			t.Fatalf("final error has inaccurate counts or range: %v", err)
		}
	})

	t.Run("close_error_surfaces_immediately", func(t *testing.T) {
		wasUDP, wasTCP := udpListen, portListen
		t.Cleanup(func() { udpListen, portListen = wasUDP, wasTCP })

		udpListen = func(network, address string) (net.PacketConn, error) {
			if address == ":0" {
				return &udpStandIn{port: 40001, closeFails: true}, nil
			}
			return nil, errors.New("unexpected")
		}
		portListen = func(_ context.Context, network, address string) (net.Listener, error) {
			return &pickListener{port: 40001}, nil
		}

		if got, err := availablePortAbove(unknown, 0, true); err == nil || !strings.Contains(err.Error(), "UDP port 40001") {
			t.Fatalf("got port %d, err %v; want UDP port 40001 close error", got, err)
		}
	})
}

// TestGetAvailableDNSPortSkipsContiguousUDPBlock holds a contiguous block of
// UDP ports right ahead of the OS's next TCP :0 pick (reproducing Windows's
// sequential TCP cursor entering a 100-port Hyper-V/WinNAT UDP excluded range)
// and proves availablePortAbove(unknown, 0, true, ...) succeeds every time.
func TestGetAvailableDNSPortSkipsContiguousUDPBlock(t *testing.T) {
	unknown := func() (uint32, uint32, bool) { return 0, 0, false }

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	cursor := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	var held []net.PacketConn
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	for p := cursor + 1; p <= cursor+24 && p <= 65535; p++ {
		if c, berr := net.ListenPacket("udp", fmt.Sprintf(":%d", p)); berr == nil {
			held = append(held, c)
		}
	}
	for i := 0; i < 20; i++ {
		got, err := availablePortAbove(unknown, 0, true, 16789, 26789)
		if err != nil {
			t.Fatalf("iter %d: availablePortAbove failed with contiguous UDP block held: %v", i, err)
		}
		if got == 0 || got == 16789 || got == 26789 {
			t.Fatalf("iter %d: got invalid/excluded port %d", i, got)
		}
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
