package utils

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestContainerNameFromDockerRun(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"space form", "docker run --rm --name dedup-go-test dedup-go:latest", "dedup-go-test"},
		{"equals form", "docker run --name=my-app img", "my-app"},
		{"name mid-flags", "docker run -d --name x --network y img", "x"},
		{"no name", "docker run --rm img", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContainerNameFromDockerRun(tc.cmd); got != tc.want {
				t.Fatalf("ContainerNameFromDockerRun(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

// GetAvailablePort must not hand back a port from the kernel's local port
// range.
//
// The oracle is read straight from /proc, NOT from ephemeralPortRange: using
// the production helper as its own oracle makes the test self-referential, and
// a helper that mis-reports the range (say 32768-40000) would then pass while
// handing out 40001-65534 — most of it inside the real range, i.e. exactly the
// bug this test exists to catch.
func TestGetAvailablePortAvoidsTheEphemeralRange(t *testing.T) {
	raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		t.Skipf("no ip_local_port_range on this platform (%v); GetAvailablePort "+
			"correctly falls back to :0 there and there is nothing to assert", err)
	}
	f := strings.Fields(string(raw))
	if len(f) != 2 {
		t.Fatalf("unexpected ip_local_port_range content %q", raw)
	}
	lo64, err1 := strconv.ParseUint(f[0], 10, 32)
	hi64, err2 := strconv.ParseUint(f[1], 10, 32)
	if err1 != nil || err2 != nil {
		t.Fatalf("unparseable ip_local_port_range %q", raw)
	}
	lo, hi := uint32(lo64), uint32(hi64)
	if hi >= 65535 {
		t.Skipf("kernel range reaches %d, leaving no space above it; the fallback is "+
			"correct here and there is nothing to assert", hi)
	}

	for i := 0; i < 50; i++ {
		port, err := GetAvailablePort()
		if err != nil {
			t.Fatalf("GetAvailablePort: %v", err)
		}
		if port == 0 {
			t.Fatal("GetAvailablePort returned port 0")
		}
		if port >= lo && port <= hi {
			t.Fatalf("GetAvailablePort returned %d, inside the kernel's local port range "+
				"%d-%d read from /proc: every other bind(0) allocator on the machine — a "+
				"second keploy process, Docker's host-port publisher — draws from there "+
				"and can take it before the agent binds", port, lo, hi)
		}
	}
}

// The returned port must be one the function VERIFIED, not merely a number in
// the right band.
//
// Asserting "the returned port is bindable" is not enough: on an idle box
// almost any port above the ephemeral range binds, so that assertion holds by
// luck even if the bindability probe is deleted entirely. Instead, hold a block
// of candidates and require the function to return something outside it.
func TestGetAvailablePortSkipsPortsAlreadyHeld(t *testing.T) {
	if _, _, known := ephemeralPortRange(); !known {
		t.Skip("platform range unknown; GetAvailablePort falls back to :0 here")
	}
	_, hi, _ := ephemeralPortRange()
	if hi >= 65535 {
		t.Skip("no space above the ephemeral range on this host")
	}

	// Occupy a contiguous block so a probe-less implementation, which returns
	// whatever candidate it lands on, has a high chance of returning a held one.
	held := map[uint32]bool{}
	var lns []net.Listener
	for p := hi + 1; p <= 65535 && len(lns) < 2000; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			continue
		}
		lns = append(lns, ln)
		held[p] = true
	}
	defer func() {
		for _, ln := range lns {
			_ = ln.Close()
		}
	}()
	if len(held) < 100 {
		t.Skipf("could only hold %d ports; too few to discriminate", len(held))
	}

	for i := 0; i < 200; i++ {
		port, err := GetAvailablePort()
		if err != nil {
			t.Fatalf("GetAvailablePort: %v", err)
		}
		if held[port] {
			t.Fatalf("GetAvailablePort returned %d, a port THIS TEST is holding: the "+
				"candidate was never verified bindable, so callers are handed ports "+
				"that are already in use", port)
		}
	}
}

// Successive callers must not all be handed the SAME port.
//
// Scanning upward from a fixed floor returns the lowest free port every time,
// so two keploy processes started moments apart — the ordinary CI pattern —
// are handed an identical number, each having verified it free. Whichever
// binds second loses, and the agent's bind retry cannot recover because the
// winner is a live holder, not a departing one.
//
// Sequential rather than concurrent: concurrent callers are separated anyway
// by the brief hold of the verification listener, so a concurrent test passes
// even with a fixed floor and proves nothing.
func TestGetAvailablePortDoesNotHandEveryCallerTheSamePort(t *testing.T) {
	const n = 12
	seen := map[uint32]int{}
	for i := 0; i < n; i++ {
		p, err := GetAvailablePort()
		if err != nil {
			t.Fatalf("GetAvailablePort: %v", err)
		}
		seen[p]++
	}
	if len(seen) < 2 {
		t.Fatalf("%d successive calls returned %d distinct port(s): candidates are not "+
			"randomised, so keploy processes started moments apart are handed the same "+
			"port and one of them fails to bind", n, len(seen))
	}
}

func TestEphemeralPortRangeIsSane(t *testing.T) {
	lo, hi, known := ephemeralPortRange()
	if !known {
		// Correct on any platform without ip_local_port_range. It MUST report
		// unknown rather than guessing: macOS and Windows both default to
		// 49152-65535, so assuming the Linux range would allocate entirely
		// inside the real ephemeral range there.
		if lo != 0 || hi != 0 {
			t.Fatalf("unknown range still reported %d-%d; callers must not act on it", lo, hi)
		}
		return
	}
	if lo == 0 || hi == 0 || hi < lo || hi > 65535 {
		t.Fatalf("ephemeralPortRange returned %d-%d, want a sane range", lo, hi)
	}
}

// The fallback branches are unreachable on a normal Linux box — the /proc read
// always succeeds — so they are tested through the pure parser instead. Without
// this, a broken default would be dead code that no test can reach.
func TestParseEphemeralPortRange(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		lo, hi uint32
	}{
		{"normal", "32768\t60999\n", 32768, 60999},
		{"widened", "1024 65000\n", 1024, 65000},
		{"one field", "32768\n", defaultEphemeralLo, defaultEphemeralHi},
		{"three fields", "1 2 3\n", defaultEphemeralLo, defaultEphemeralHi},
		{"empty", "", defaultEphemeralLo, defaultEphemeralHi},
		{"non-numeric", "lo hi\n", defaultEphemeralLo, defaultEphemeralHi},
		{"inverted", "60999 32768\n", defaultEphemeralLo, defaultEphemeralHi},
		{"zero low", "0 60999\n", defaultEphemeralLo, defaultEphemeralHi},
		{"out of range high", "32768 70000\n", defaultEphemeralLo, defaultEphemeralHi},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi, ok := parseEphemeralPortRange(tc.in)
			if !ok {
				t.Fatal("parseEphemeralPortRange reported unknown; it is only called " +
					"when the file was read, so it must always resolve to a range")
			}
			if lo != tc.lo || hi != tc.hi {
				t.Fatalf("parseEphemeralPortRange(%q) = %d-%d, want %d-%d", tc.in, lo, hi, tc.lo, tc.hi)
			}
		})
	}

	// The Linux default must leave usable space above it, or GetAvailablePort
	// has nowhere to draw from on a host whose file is malformed and silently
	// falls back to the racy :0 path. This constrains the LINUX default only —
	// unknown platforms report known=false and fall back deliberately.
	if defaultEphemeralHi >= 65535 {
		t.Fatalf("Linux default high-water mark %d leaves no room above it",
			defaultEphemeralHi)
	}
}

// The agent, proxy and DNS ports are all allocated before ANY of them is bound,
// so a later draw can legitimately return a port an earlier one already
// claimed — isPortAvailable says yes, because nothing is listening on it yet.
// Narrowing allocation to above the ephemeral range shrinks the pool and makes
// that coincidence correspondingly more likely.
func TestEnsureAvailablePortsHonoursTheExclusionSet(t *testing.T) {
	// A port that IS free: without the exclusion set it is returned unchanged.
	free, err := GetAvailablePort()
	if err != nil {
		t.Fatalf("GetAvailablePort: %v", err)
	}

	if got, err := EnsureAvailablePorts(free); err != nil || got != free {
		t.Fatalf("precondition: a free port must be returned unchanged, got %d (%v)", got, err)
	}

	got, err := EnsureAvailablePorts(free, free)
	if err != nil {
		t.Fatalf("EnsureAvailablePorts: %v", err)
	}
	if got == free {
		t.Fatalf("EnsureAvailablePorts returned %d, which the caller had already claimed: "+
			"nothing is bound to it yet, so isPortAvailable says free and two subsystems "+
			"end up racing for the same port", got)
	}
}

// A platform without ip_local_port_range must report UNKNOWN, not guess.
//
// This is the branch that decides whether the allocator acts at all, and on
// Linux the read always succeeds so it is dead code no ordinary test reaches.
// Guessing the Linux range there is actively harmful: macOS and Windows both
// default to 49152-65535, so 61000-65534 would be entirely INSIDE their real
// ephemeral range while shrinking the pool from 16384 to 4535 — strictly worse
// than the plain :0 the fallback gives them.
func TestEphemeralPortRangeReportsUnknownWhenUnreadable(t *testing.T) {
	lo, hi, known := ephemeralPortRangeFrom(func(string) ([]byte, error) {
		return nil, os.ErrNotExist
	})
	if known {
		t.Fatalf("reported a known range %d-%d on a platform with no "+
			"ip_local_port_range; GetAvailablePort would then allocate from a band that "+
			"is inside the real ephemeral range on macOS and Windows", lo, hi)
	}
	if lo != 0 || hi != 0 {
		t.Fatalf("unknown range still returned %d-%d; callers must not act on it", lo, hi)
	}

	// A readable-but-malformed file is a different case: the platform IS Linux,
	// so the documented default is the right answer.
	lo, hi, known = ephemeralPortRangeFrom(func(string) ([]byte, error) {
		return []byte("garbage\n"), nil
	})
	if !known || lo != defaultEphemeralLo || hi != defaultEphemeralHi {
		t.Fatalf("malformed content gave %d-%d known=%v, want the Linux default %d-%d known=true",
			lo, hi, known, defaultEphemeralLo, defaultEphemeralHi)
	}
}
