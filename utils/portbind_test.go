package utils

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// The bind is the check: a taken port is passed over for the next, and any
// other failure ends the search with it.
func TestBindFreePortPassesOverTakenPorts(t *testing.T) {
	var tried []uint32
	port, err := BindFreePort(func(p uint32) error {
		tried = append(tried, p)
		if len(tried) < 3 {
			return syscall.EADDRINUSE
		}
		return nil
	})
	if err != nil || len(tried) != 3 || port != tried[2] || tried[0] == tried[1] {
		t.Fatalf("got port %d, err %v, tried %v", port, err, tried)
	}

	boom := errors.New("boom")
	tried = nil
	if _, err := BindFreePort(func(p uint32) error { tried = append(tried, p); return boom }); !errors.Is(err, boom) || len(tried) != 1 {
		t.Fatalf("a failure other than a taken port: err %v after %d tries", err, len(tried))
	}

	if _, err := BindFreePort(func(uint32) error { return syscall.EADDRINUSE }); !errors.Is(err, syscall.EADDRINUSE) || !strings.Contains(err.Error(), "every port from") {
		t.Fatalf("every port taken: %v", err)
	}
}

// The candidates are the ports above the ephemeral range where it is known
// and ends below 65535, else the ports from 1024 up; every candidate is tried
// once, from a random start, so that runs started together try different
// ones first.
func TestBindFreePortTriesEachPortAboveTheEphemeralRangeOnce(t *testing.T) {
	for _, c := range []struct {
		name      string
		hi        uint32
		known     bool
		lo, count uint32
	}{
		{"a known range", 60999, true, 61000, 4536},
		{"a range up to 65535", 65535, true, 1024, 64512},
		{"an unknown range", 0, false, 1024, 64512},
	} {
		firsts := map[uint32]bool{}
		for draw := 0; draw < 20; draw++ {
			seen := map[uint32]bool{}
			_, err := bindFreePortAbove(func() (uint32, uint32, bool) { return 32768, c.hi, c.known }, func(p uint32) error {
				if len(seen) == 0 {
					firsts[p] = true
				}
				if p < c.lo || seen[p] {
					t.Fatalf("%s: tried port %d (below %d, or again)", c.name, p, c.lo)
				}
				seen[p] = true
				return syscall.EADDRINUSE
			})
			if err == nil || uint32(len(seen)) != c.count {
				t.Fatalf("%s: tried %d ports, want %d (err %v)", c.name, len(seen), c.count, err)
			}
		}
		if len(firsts) < 2 {
			t.Fatalf("%s: 20 searches all began at %v: not from a random start", c.name, firsts)
		}
	}
}

// PortTaken names what holds the port where it can, and says it cannot
// where it cannot.
func TestPortTakenNamesTheHolderWhereItCan(t *testing.T) {
	if got := PortTaken(1); got != "port 1 is taken ("+unnamedHolder+")" {
		t.Fatalf("a port nothing here holds: %q", got)
	}
}
