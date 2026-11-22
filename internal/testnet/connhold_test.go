package testnet

import (
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// The address holdByConnection gives stays its own while the test runs: no
// other server can take it (requireHeld says how, per platform), and a dial
// to it is refused.
func TestAPortHeldByAConnectionCannotBeTakenAndRefusesADial(t *testing.T) {
	addr := holdByConnection(t)
	requireHeld(t, addr)
	requireDialRefused(t, addr)
}

// requireDialRefused checks that a TCP dial to addr is refused.
func requireDialRefused(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err == nil {
		_ = c.Close()
		t.Fatalf("connected to %s; want a refusal", addr)
	}
	require.ErrorIs(t, err, errConnRefused, "dial %s: want the connection refused", addr)
}

// The held port on macOS lies above the low range's top and below the start
// of both ephemeral ranges, whichever is lower, and none is held when no port
// lies there or a bound cannot be read.
func TestUnpickedPortsLieBetweenThePortZeroRanges(t *testing.T) {
	errUnread := errors.New("no such sysctl")
	reader := func(lowfirst, first, hifirst uint32) func(string) (uint32, error) {
		v := map[string]uint32{
			"net.inet.ip.portrange.lowfirst": lowfirst,
			"net.inet.ip.portrange.first":    first,
			"net.inet.ip.portrange.hifirst":  hifirst,
		}
		return func(name string) (uint32, error) {
			n, ok := v[name]
			if !ok {
				return 0, errUnread
			}
			return n, nil
		}
	}
	for _, c := range []struct {
		name                     string
		lowfirst, first, hifirst uint32
		lo, hi                   int
	}{
		{"macOS's defaults", 1023, 49152, 49152, 1024, 49152},
		{"first starts lower", 1023, 40000, 49152, 1024, 40000},
		{"hifirst starts lower", 1023, 49152, 30000, 1024, 30000},
		{"one port between", 49150, 49152, 49152, 49151, 49152},
	} {
		t.Run(c.name, func(t *testing.T) {
			lo, hi, err := unpickedPorts(reader(c.lowfirst, c.first, c.hifirst))
			require.NoError(t, err)
			require.Equal(t, [2]int{c.lo, c.hi}, [2]int{lo, hi})
		})
	}
	for _, c := range []struct {
		name                     string
		lowfirst, first, hifirst uint32
	}{
		{"no port between", 49151, 49152, 49152},
		{"first at the low range's top", 1023, 1023, 49152},
		{"the low range above first", 60000, 49152, 49152},
	} {
		t.Run(c.name, func(t *testing.T) {
			lo, hi, err := unpickedPorts(reader(c.lowfirst, c.first, c.hifirst))
			require.ErrorContains(t, err, "no port lies", "got [%d, %d)", lo, hi)
		})
	}
	t.Run("a bound that cannot be read", func(t *testing.T) {
		read := func(name string) (uint32, error) {
			if name == "net.inet.ip.portrange.hifirst" {
				return 0, errUnread
			}
			return reader(1023, 49152, 49152)(name)
		}
		c, err := connectUnpicked(holdByConnection(t), read)
		if err == nil {
			_ = c.Close()
			t.Fatalf("held %s with hifirst unread", c.LocalAddr())
		}
		require.ErrorIs(t, err, errUnread)
		require.ErrorContains(t, err, "net.inet.ip.portrange.hifirst")
	})
}

// connectFrom passes over a port another socket holds and stops at any other
// failure.
func TestConnectFromTakesTheLowestPortNoSocketHolds(t *testing.T) {
	peer, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = taken.Close() })
	p := taken.Addr().(*net.TCPAddr).Port

	t.Run("every port in use", func(t *testing.T) {
		c, err := connectFrom(peer.Addr().String(), p, p+1)
		if err == nil {
			_ = c.Close()
			t.Fatalf("connected from %s; port %d is in use", c.LocalAddr(), p)
		}
		require.ErrorIs(t, err, errNoFreePort)
	})
	t.Run("a free port", func(t *testing.T) {
		// Port 0 binds whatever port the system gives.
		c, err := connectFrom(peer.Addr().String(), 0, 1)
		require.NoError(t, err)
		_ = c.Close()
	})
	t.Run("a refused peer", func(t *testing.T) {
		c, err := connectFrom(holdByConnection(t), 0, 1)
		if err == nil {
			_ = c.Close()
			t.Fatal("connected to a held port")
		}
		require.ErrorIs(t, err, errConnRefused, "want the refusal itself")
	})
}
