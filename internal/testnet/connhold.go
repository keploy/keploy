package testnet

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// holdByConnection returns a loopback address, 127.0.0.1:P, that nothing
// listens on for the rest of the test: a dial to it is refused, and no server
// another test starts on an ephemeral port can take P meanwhile. RefusedAddr
// holds a TCP port this way where a socket that is only bound does not
// refuse a dial (see the package doc).
//
// P is the local end of a TCP connection to a listener opened for it on
// 127.0.0.1. The socket has no SO_REUSEADDR. The connection waits in the
// listener's queue, never accepted, and both stay open until the test ends;
// closing the listener earlier would reset the connection and free the port.
// A connected socket answers only its peer, so a dial from anywhere else
// finds no listener and is refused. The connection closes with a reset, so
// the port is free once the test ends.
//
// On Linux no other socket can bind P while it is held. On Windows the
// holding socket binds P exclusively (holdControl): Go binds it at the
// wildcard address, and without the option a listener that sets none, as
// Go's there do, binds 127.0.0.1:P beside it. macOS lets a socket that sets
// SO_REUSEADDR, as Go's listeners there do, bind the port of a connected
// socket, so there P is a port the system never gives a bind to port 0 (see
// connectUnpicked). A server that names P itself can still take it on macOS.
func holdByConnection(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	held, err := connectHolder(ln.Addr().String())
	require.NoError(t, err, "hold a port")
	t.Cleanup(func() { _ = held.Close() })
	require.NoError(t, held.SetLinger(0))
	return held.LocalAddr().String()
}

// holdUDPByConnection is holdByConnection for UDP: the port is the local end
// of a UDP socket connected to a peer socket on 127.0.0.1, both open until
// the test ends. A connected socket takes only its peer's datagrams, so the
// kernel answers any other sender's as it answers one to a port nothing
// holds: port unreachable. Go's UDP sockets do not set SO_REUSEADDR, so on
// Linux and macOS no other one can bind the port meanwhile; on Windows the
// holding socket binds it exclusively (holdControl), as a TCP hold does,
// since a UDP socket that sets no option binds 127.0.0.1:P beside one that
// does not.
func holdUDPByConnection(t *testing.T) string {
	t.Helper()
	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	d := net.Dialer{Control: holdControl}
	held, err := d.Dial("udp4", peer.LocalAddr().String())
	require.NoError(t, err, "hold a port")
	t.Cleanup(func() { _ = held.Close() })
	return held.LocalAddr().String()
}

// The sysctls that bound, on macOS, the ranges a bind to port 0 is given a
// port from: a privileged socket that asks for a low port gets one at or
// below lowfirst; any other gets one from first or from hifirst up.
var portZeroRangeStarts = [...]string{
	"net.inet.ip.portrange.lowfirst",
	"net.inet.ip.portrange.first",
	"net.inet.ip.portrange.hifirst",
}

// connectUnpicked connects to peer from the lowest port above lowfirst and
// below both first and hifirst, as read reads them, that no other socket
// holds. No bind to port 0 is given such a port. It fails when a sysctl
// cannot be read or no port lies between them, rather than hold a port a
// server can be given.
func connectUnpicked(peer string, read func(name string) (uint32, error)) (*net.TCPConn, error) {
	lo, hi, err := unpickedPorts(read)
	if err != nil {
		return nil, err
	}
	return connectFrom(peer, lo, hi)
}

// unpickedPorts returns the ports [lo, hi) above lowfirst and below both
// first and hifirst.
func unpickedPorts(read func(name string) (uint32, error)) (lo, hi int, err error) {
	var v [len(portZeroRangeStarts)]int
	for i, name := range portZeroRangeStarts {
		n, err := read(name)
		if err != nil {
			return 0, 0, fmt.Errorf("read %s: %w", name, err)
		}
		v[i] = int(n)
	}
	lo, hi = v[0]+1, min(v[1], v[2])
	if lo >= hi {
		return 0, 0, fmt.Errorf("no port lies above lowfirst (%d) and below first and hifirst (%d)", v[0], hi)
	}
	return lo, hi, nil
}

// errNoFreePort: every port of the range is held by another socket.
var errNoFreePort = errors.New("every port is in use")

// connectFrom connects to peer from the lowest port of [lo, hi) that no
// other socket holds on 127.0.0.1. Any failure but the port being in use
// ends it.
func connectFrom(peer string, lo, hi int) (*net.TCPConn, error) {
	for port := lo; port < hi; port++ {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}}
		c, err := d.Dial("tcp", peer)
		if err == nil {
			return c.(*net.TCPConn), nil
		}
		if !errors.Is(err, errAddrInUse) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("from 127.0.0.1 ports %d to %d: %w", lo, hi-1, errNoFreePort)
}
