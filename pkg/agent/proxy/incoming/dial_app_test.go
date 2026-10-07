package proxy

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func newDialTestPM() *IngressProxyManager {
	return &IngressProxyManager{appAddr: map[uint16]string{}}
}

// listenOn starts a TCP listener on addr:0 and returns its address.
func listenOn(t *testing.T, host string) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s in this environment: %v", host, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln, ln.Addr().String()
}

// nonLoopbackIP returns a usable non-loopback address of this host, or skips.
func nonLoopbackIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP != nil && !n.IP.IsLoopback() && n.IP.IsGlobalUnicast() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address on this host")
	return ""
}

// The happy path must be untouched: an app on loopback is reached on the first
// dial, with no probing and nothing cached.
func TestDialApp_LoopbackIsUnchanged(t *testing.T) {
	pm := newDialTestPM()
	_, addr := listenOn(t, "127.0.0.1")

	conn, err := pm.dialApp(context.Background(), addr, zap.NewNop())
	if err != nil {
		t.Fatalf("dialApp on a loopback app: %v", err)
	}
	_ = conn.Close()

	if len(pm.appAddr) != 0 {
		t.Errorf("the loopback path must not populate the cache, got %v", pm.appAddr)
	}
}

// The bug: the app bound its pod IP, the forwarder was told loopback. The dial
// must find it rather than failing every connection.
func TestDialApp_FindsAnAppBoundToANonLoopbackAddress(t *testing.T) {
	ip := nonLoopbackIP(t)
	pm := newDialTestPM()
	_, realAddr := listenOn(t, ip)

	_, port, err := net.SplitHostPort(realAddr)
	if err != nil {
		t.Fatalf("split %q: %v", realAddr, err)
	}
	assumed := net.JoinHostPort("127.0.0.1", port)

	conn, err := pm.dialApp(context.Background(), assumed, zap.NewNop())
	if err != nil {
		t.Fatalf("dialApp did not find the app on %s (dialled %s): %v", realAddr, assumed, err)
	}
	_ = conn.Close()

	p, _ := portOf(assumed)
	if got := pm.appAddr[p]; got != realAddr {
		t.Errorf("resolved address not cached: got %q want %q", got, realAddr)
	}
}

// Once resolved, later connections must go straight there — the probe is a
// recovery, not a per-connection cost.
func TestDialApp_UsesTheCacheOnLaterConnections(t *testing.T) {
	ip := nonLoopbackIP(t)
	pm := newDialTestPM()
	_, realAddr := listenOn(t, ip)
	_, port, _ := net.SplitHostPort(realAddr)
	assumed := net.JoinHostPort("127.0.0.1", port)

	for i := 0; i < 3; i++ {
		conn, err := pm.dialApp(context.Background(), assumed, zap.NewNop())
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		_ = conn.Close()
	}
	if len(pm.appAddr) != 1 {
		t.Errorf("expected exactly one cached port, got %v", pm.appAddr)
	}
}

// A stale cache entry must not wedge the forwarder: if the app is gone from the
// remembered address, the entry is dropped so the next attempt starts fresh.
func TestDialApp_DropsAStaleCacheEntry(t *testing.T) {
	pm := newDialTestPM()
	// Point the cache at a port nothing is listening on.
	ln, dead := listenOn(t, "127.0.0.1")
	_, port, _ := net.SplitHostPort(dead)
	_ = ln.Close()

	p, _ := portOf(dead)
	pm.appAddr[p] = dead

	if _, err := pm.dialApp(context.Background(), net.JoinHostPort("127.0.0.1", port), zap.NewNop()); err == nil {
		t.Fatal("expected the dial to fail when nothing is listening anywhere")
	}
	if _, still := pm.appAddr[p]; still {
		t.Error("a stale cache entry must be dropped, or the forwarder retries a dead address forever")
	}
}

// When nothing is listening anywhere, the caller must get the error naming the
// address IT asked for — the existing log lines and their expectations are
// written against that.
func TestDialApp_ReportsTheOriginalAddressWhenNothingIsListening(t *testing.T) {
	pm := newDialTestPM()
	ln, addr := listenOn(t, "127.0.0.1")
	_ = ln.Close()

	_, err := pm.dialApp(context.Background(), addr, zap.NewNop())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("error should name the requested address %q, got %v", addr, err)
	}
}

func TestPortOf(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint16
		ok   bool
	}{
		{"127.0.0.1:35721", 35721, true},
		{"[::1]:8080", 8080, true},
		{"garbage", 0, false},
		{"127.0.0.1:notaport", 0, false},
	} {
		got, ok := portOf(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("portOf(%q) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	_ = fmt.Sprint()
}
