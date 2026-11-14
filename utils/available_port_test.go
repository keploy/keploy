package utils

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils/plaintcp/plaintcptest"
)

// A container's port published as itself is above the container's own
// ephemeral range (32768-60999, whatever the host's), where Docker's embedded
// DNS resolver holds a TCP and a UDP port before the container's process
// runs, and above the host's: for a host range that ends below the
// container's, at it, above it, at 65535, and one that is unknown. A host's
// port, with no floor, is above the host's range only, and the OS's pick
// (:0, in the host's range) where that range is unknown, as before.
func TestAContainersPortIsAboveTheContainersEphemeralRange(t *testing.T) {
	for _, c := range []struct {
		name  string
		hi    uint32
		known bool
		floor uint32
		above uint32
	}{
		{"a container, a host range ending below the container's", 49999, true, containerEphemeralHi, 60999},
		{"a container, the default host range", 60999, true, containerEphemeralHi, 60999},
		{"a container, a host range ending above the container's", 62000, true, containerEphemeralHi, 62000},
		{"a container, a host range up to 65535", 65535, true, containerEphemeralHi, 60999},
		{"a container, an unknown host range", 0, false, containerEphemeralHi, 60999},
		{"the host, a host range ending below the container's", 49999, true, 0, 49999},
	} {
		below := false
		for i := 0; i < 50; i++ {
			p, err := availablePortAbove(func() (uint32, uint32, bool) { return 32768, c.hi, c.known }, c.floor)
			if err != nil || p <= c.above {
				t.Fatalf("%s: got port %d, err %v; want one above %d", c.name, p, err, c.above)
			}
			below = below || p <= 60999
		}
		if c.floor == 0 && !below {
			t.Fatalf("%s: 50 ports all above 60999: the container's floor is applied to the host's port", c.name)
		}
	}
	if containerEphemeralHi != 60999 {
		t.Fatalf("a container's ephemeral range ends at %d, want Linux's default, 60999", containerEphemeralHi)
	}

	raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		t.Skipf("no ip_local_port_range here (%v): the OS's pick cannot be checked", err)
	}
	f := strings.Fields(string(raw))
	if len(f) != 2 {
		t.Fatalf("unexpected ip_local_port_range %q", raw)
	}
	lo, err1 := strconv.ParseUint(f[0], 10, 32)
	hi, err2 := strconv.ParseUint(f[1], 10, 32)
	if err1 != nil || err2 != nil {
		t.Fatalf("unparseable ip_local_port_range %q", raw)
	}
	for i := 0; i < 50; i++ {
		// An unknown range's numbers are not read, whatever they are.
		p, err := availablePortAbove(func() (uint32, uint32, bool) { return 32768, 49999, false }, 0)
		if err != nil || uint64(p) < lo || uint64(p) > hi {
			t.Fatalf("the host, an unknown host range: got port %d, err %v; want the OS's pick, in %d-%d", p, err, lo, hi)
		}
	}
}

// GetAvailableContainerPort's ports are above the container's range where
// the host's ends below it; GetAvailablePort's, above the host's only.
func TestGetAvailableContainerPortIsAboveTheContainersRange(t *testing.T) {
	defer func(r func() (uint32, uint32, bool)) { hostEphemeralRange = r }(hostEphemeralRange)
	hostEphemeralRange = func() (uint32, uint32, bool) { return 32768, 49999, true }
	below := false
	for i := 0; i < 50; i++ {
		if p, err := GetAvailableContainerPort(); err != nil || p <= 60999 {
			t.Fatalf("a container's port: got %d, err %v; want one above 60999", p, err)
		}
		p, err := GetAvailablePort()
		if err != nil || p <= 49999 {
			t.Fatalf("a host's port: got %d, err %v; want one above 49999", p, err)
		}
		below = below || p <= 60999
	}
	if !below {
		t.Fatal("50 host ports all above 60999: the container's floor is applied to the host's port")
	}
}

// drawsAndProbes sets availablePortAbove's draws, which return offsets in
// turn and then none, and its probe, which finds a port free where free says,
// and records the ports it probed.
func drawsAndProbes(t *testing.T, offsets []uint32, free func(port uint32) (bool, error)) *[]uint32 {
	t.Helper()
	o, p := portOffset, portProbe
	t.Cleanup(func() { portOffset, portProbe = o, p })
	portOffset = func(uint32) (uint32, bool) {
		if len(offsets) == 0 {
			return 0, false
		}
		off := offsets[0]
		offsets = offsets[1:]
		return off, true
	}
	probed := &[]uint32{}
	portProbe = func(port uint32) (bool, error) {
		*probed = append(*probed, port)
		return free(port)
	}
	return probed
}

// Past a run of held ports above the range, a host's port is drawn at
// random again, not taken from the port after the run, which a scan from
// every start inside the run reached: two processes started together were
// handed that one port.
func TestAPortIsDrawnAgainPastARunOfHeldPorts(t *testing.T) {
	ephemeral := func() (uint32, uint32, bool) { return 32768, 60999, true }
	pastTheRun := func(port uint32) (bool, error) { return port >= 63000, nil } // 61000-62999 held
	probed := drawsAndProbes(t, []uint32{3, 1500, 2500}, pastTheRun)
	if p, err := availablePortAbove(ephemeral, 0); err != nil || p != 63500 || len(*probed) != 3 {
		t.Fatalf("got port %d, err %v, after %d probes; want 63500, the third draw, after 61003 and 62500", p, err, len(*probed))
	}

	// Every draw misses: every port is still tried, from a random start.
	only := func(free uint32) func(uint32) (bool, error) {
		return func(port uint32) (bool, error) { return port == free, nil }
	}
	misses := make([]uint32, randomProbes+1)
	for i := range misses {
		misses[i] = 7
	}
	probed = drawsAndProbes(t, misses, only(61006))
	if p, err := availablePortAbove(ephemeral, 0); err != nil || p != 61006 || len(*probed) != randomProbes+4536 {
		t.Fatalf("got port %d, err %v, after %d probes; want 61006, after %d draws and a scan of all 4536 ports from 61007", p, err, len(*probed), randomProbes)
	}

	// No random number: the ports from the bottom.
	probed = drawsAndProbes(t, nil, only(61002))
	if p, err := availablePortAbove(ephemeral, 0); err != nil || p != 61002 || len(*probed) != 3 {
		t.Fatalf("got port %d, err %v, after probing %v; want 61002, the third from 61000", p, err, *probed)
	}

	// A probe that fails is the error, at a draw (61005) or in the scan
	// (61000), though the next port is free.
	broken := errors.New("close failed")
	for _, c := range []struct {
		offsets []uint32
		failing uint32
	}{{[]uint32{5}, 61005}, {nil, 61000}} {
		drawsAndProbes(t, c.offsets, func(port uint32) (bool, error) {
			if port == c.failing {
				return false, broken
			}
			return true, nil
		})
		if p, err := availablePortAbove(ephemeral, 0); !errors.Is(err, broken) {
			t.Fatalf("draws %v: got port %d, err %v; want the probe's error at %d", c.offsets, p, err, c.failing)
		}
	}
}

// EnsureAvailablePorts's search for another port skips the excluded ports:
// in a band whose free ports are an excluded one and one other, it returns
// the other, whether every draw returns the excluded port or no random
// number is read. It drew again while it got an excluded port, 16 times, and
// then failed, the other port free all along.
func TestEnsureAvailablePortsFindsTheFreePortBesideAnExcludedOne(t *testing.T) {
	defer func(r func() (uint32, uint32, bool)) { hostEphemeralRange = r }(hostEphemeralRange)
	hostEphemeralRange = func() (uint32, uint32, bool) { return 32768, 65530, true } // the band: 65531-65535
	const excluded, other uint32 = 65532, 65534
	free := func(port uint32) (bool, error) { return port == excluded || port == other, nil }
	everyDrawExcluded := make([]uint32, randomProbes+1)
	for i := range everyDrawExcluded {
		everyDrawExcluded[i] = excluded - 65531
	}
	for _, offsets := range [][]uint32{everyDrawExcluded, nil} {
		probed := drawsAndProbes(t, offsets, free)
		got, err := EnsureAvailablePorts(excluded, 65531, excluded)
		if err != nil || got != other {
			t.Fatalf("draws %v: EnsureAvailablePorts = %d, %v; want %d, the free port that is not excluded", offsets, got, err, other)
		}
		if slices.Contains(*probed, excluded) {
			t.Fatalf("draws %v: probed the excluded %d (%v); the search skips it", offsets, excluded, *probed)
		}
	}
}

// pickListener is a listener the OS's pick (portListen on ":0") returned in
// a row, on port, and whether it was let go.
type pickListener struct {
	port   int
	closed bool
}

func (l *pickListener) Accept() (net.Conn, error) { return nil, errors.New("not accepting") }
func (l *pickListener) Close() error              { l.closed = true; return nil }
func (l *pickListener) Addr() net.Addr            { return &net.TCPAddr{Port: l.port} }

// osPicks has portListen return listeners on ports, in turn, and records them.
// A pick asked for while the one before it was let go fails the row: an
// excluded pick is held so the OS cannot hand it back.
func osPicks(t *testing.T, ports ...int) *[]*pickListener {
	t.Helper()
	was := portListen
	t.Cleanup(func() { portListen = was })
	picks := &[]*pickListener{}
	portListen = func(_ context.Context, network, address string) (net.Listener, error) {
		if network != "tcp" || address != ":0" || len(ports) == 0 {
			return nil, fmt.Errorf("unexpected listen on %s %s", network, address)
		}
		if n := len(*picks); n > 0 && (*picks)[n-1].closed {
			t.Errorf("the OS was asked again after the excluded pick %d was let go", (*picks)[n-1].port)
		}
		l := &pickListener{port: ports[0]}
		ports = ports[1:]
		*picks = append(*picks, l)
		return l, nil
	}
	return picks
}

// Where no port above the range is left, or the range is unknown, the OS's
// pick stands in: one that is excluded is held while the OS is asked again,
// and every pick is let go before the port is returned. Where every pick is
// excluded, as the OS never does with them held, it is an error after one ask
// more than there are excluded ports, not a hang.
func TestTheOSsPickIsNeverAnExcludedPort(t *testing.T) {
	unknown := func() (uint32, uint32, bool) { return 0, 0, false }
	picks := osPicks(t, 40001, 40002)
	if got, err := availablePortAbove(unknown, 0, 40001); err != nil || got != 40002 || len(*picks) != 2 {
		t.Fatalf("got port %d, err %v, after %d picks; want 40002, the second pick, the excluded 40001 passed over", got, err, len(*picks))
	}
	for _, l := range *picks {
		if !l.closed {
			t.Fatalf("the pick on %d was not let go", l.port)
		}
	}

	picks = osPicks(t, 40001, 40001, 40001, 40001)
	if got, err := availablePortAbove(unknown, 0, 40001, 40003); err == nil || len(*picks) != 3 {
		t.Fatalf("got port %d, err %v, after %d picks; want an error after 3 picks, each the excluded 40001", got, err, len(*picks))
	}
}

// The probes and the OS's pick are plain TCP listeners, never Go's default
// MPTCP: for as long as one is open it listens on every interface.
func TestPortProbesArePlainTCP(t *testing.T) {
	was := portListen
	t.Cleanup(func() { portListen = was })
	opened := 0
	portListen = func(ctx context.Context, network, address string) (net.Listener, error) {
		ln, err := was(ctx, network, address)
		if err == nil {
			opened++
			plaintcptest.RequirePlainTCP(t, ln)
		}
		return ln, err
	}
	for _, c := range []struct {
		name  string
		known bool
	}{{"a probe above the range", true}, {"the OS's pick", false}} {
		opened = 0
		if _, err := availablePortAbove(func() (uint32, uint32, bool) { return 32768, 60999, c.known }, 0); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if opened == 0 {
			t.Fatalf("%s: no listener through portListen", c.name)
		}
	}
}
