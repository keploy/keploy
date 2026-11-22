//go:build linux

package proxy

import (
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent"
)

// dualStackPeer connects an IPv6 socket, dual-stack (IPV6_V6ONLY off, as
// Java opens them), to IPv4 address 127.0.0.1:port and returns the peer the
// kernel reports for it: ::ffff:127.0.0.1, what keploy's hook records as the
// destination of such a connection.
func dualStackPeer(t *testing.T, port int) [16]byte {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no IPv6 sockets on this host: %v", err)
	}
	defer func() { _ = syscall.Close(fd) }()
	require.NoError(t, syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0))
	to := &syscall.SockaddrInet6{Port: port}
	copy(to.Addr[:], net.ParseIP("::ffff:127.0.0.1").To16())
	if err := syscall.Connect(fd, to); err != nil {
		t.Skipf("no dual-stack connect on this host: %v", err)
	}
	sa, err := syscall.Getpeername(fd)
	require.NoError(t, err)
	peer, ok := sa.(*syscall.SockaddrInet6)
	require.True(t, ok, "a dual-stack socket's peer is an IPv6 address: %T", sa)
	return peer.Addr
}

// ipv6Dest is the hook's record of a connection over an IPv6 socket to ip,
// as handleConnection reads it.
func ipv6Dest(ip [16]byte, port int) *agent.NetworkAddress {
	var words [4]uint32
	for i := range words {
		words[i] = uint32(ip[i*4])<<24 | uint32(ip[i*4+1])<<16 | uint32(ip[i*4+2])<<8 | uint32(ip[i*4+3])
	}
	return &agent.NetworkAddress{Version: 6, IPv6Addr: words, Port: uint32(port)}
}

// A dual-stack socket's connection to an IPv4 destination (an IPv6 socket to
// ::ffff:a.b.c.d, Java's default) reaches the proxy written [a.b.c.d]:p.
// mock.real reads it as the IPv4 address it is: listed by that address, it
// is decided REAL at the handshake and relayed for real at the accept, never
// recorded or mocked.
func TestADualStackConnectionToAListedAddressIsReal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				b, _ := io.ReadAll(c)
				_, _ = c.Write(append([]byte("real:"), b...))
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	dest := ipv6Dest(dualStackPeer(t, port), port)
	dstAddr := destAddr(dest)
	require.Equal(t, fmt.Sprintf("[127.0.0.1]:%d", port), dstAddr, "the form the proxy writes a dual-stack destination in")

	listed := fmt.Sprintf("127.0.0.1:%d", port)
	p := realProxy(t, []string{listed}, nil)
	label, ok := p.realAtHandshake(dest)
	require.True(t, ok, "the handshake hold does not take a listed dual-stack destination for REAL")
	require.Equal(t, listed, label)

	app, accepted := realConnPair(t)
	done := serveRealAsync(p, accepted, dstAddr)
	_, err = app.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, app.(*net.TCPConn).CloseWrite())
	// Not taken for REAL, serveReal hands the connection back at once, and
	// nothing would answer the read below.
	s := <-done
	require.True(t, s.handled, "the dual-stack connection was not taken for REAL")
	require.NoError(t, s.err)
	got, err := io.ReadAll(app)
	require.NoError(t, err)
	require.Equal(t, "real:hello", string(got), "the dual-stack connection was not relayed to the real destination")
}
