//go:build linux

package util

import (
	"net"
	"net/netip"
	"slices"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/internal/testnet"
)

// TCPListeners gives the addresses of the sockets that listen on the port,
// IPv4 and IPv6, and nothing else: not a connection on the port (the
// accepted one has it as its local port), not a socket bound to it that
// never listened, and nothing once the listener has closed.
//
// The port is held for the test's life at every address (testnet.SharedPort)
// and its listeners open beside the hold: once the IPv4 listener closes, a
// socket another test or process binds to port 0 is not given the port, at
// any address. The hold only binds: a socket bound to the port that never
// listened.
func TestTCPListenersGivesTheListenersOnThePortAlone(t *testing.T) {
	shared := testnet.SharedPort(t)
	ln := testnet.ListenShared(t, "tcp4", net.JoinHostPort("127.0.0.2", strconv.Itoa(shared)))
	port := uint16(shared)
	want := []netip.Addr{netip.MustParseAddr("127.0.0.2")}
	if v6, err := testnet.OpenShared("tcp6", net.JoinHostPort("::1", strconv.Itoa(shared))); err == nil {
		defer func() { _ = v6.Close() }()
		want = append(want, netip.IPv6Loopback())
	} else {
		t.Logf("no [::1] listener here (%v): IPv4 alone", err)
	}
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	got, err := TCPListeners(port)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, netip.Addr.Compare)
	if !slices.Equal(got, want) {
		t.Fatalf("TCPListeners(%d) = %v, want %v", port, got, want)
	}

	_ = ln.Close()
	got, err = TCPListeners(port)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.Is4() {
			t.Fatalf("TCPListeners(%d) = %v after the IPv4 listener closed", port, got)
		}
	}

	// A port a socket that never listens holds at every address, so no
	// other socket can listen there meanwhile.
	_, bound, err := net.SplitHostPort(testnet.RefusedAddr(t, "tcp"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseUint(bound, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	boundPort := uint16(n)
	if got, err := TCPListeners(boundPort); err != nil || len(got) != 0 {
		t.Fatalf("TCPListeners of a port a socket is bound to but does not listen on = %v, %v", got, err)
	}
}
