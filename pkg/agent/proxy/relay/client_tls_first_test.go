package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
)

// recordingUpgrader returns a TLSUpgradeFn that appends "dest"/"client" to a
// shared slice in call order, optionally failing one side.
func recordingUpgrader(mu *sync.Mutex, order *[]string, failSide string) (TLSUpgradeFn, *[]net.Conn) {
	var produced []net.Conn
	fn := func(_ context.Context, conn net.Conn, isClient bool, _ *tls.Config) (net.Conn, error) {
		side := "client"
		if isClient {
			side = "dest"
		}
		mu.Lock()
		*order = append(*order, side)
		mu.Unlock()
		if side == failSide {
			return nil, errors.New(side + " upgrade failed (simulated)")
		}
		c := &closeTrackingConn{Conn: conn}
		mu.Lock()
		produced = append(produced, c)
		mu.Unlock()
		return c, nil
	}
	return fn, &produced
}

type closeTrackingConn struct {
	net.Conn
	mu     sync.Mutex
	closed bool
}

func (c *closeTrackingConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	// Deliberately do NOT close the underlying pipe: the harness owns it and
	// closing it here would tear the relay down mid-test.
	return nil
}

func (c *closeTrackingConn) wasClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func awaitAck(t *testing.T, h *relayHarness) directive.Ack {
	t.Helper()
	select {
	case ack := <-h.r.Acks():
		return ack
	case <-time.After(2 * time.Second):
		t.Fatal("no ack from the TLS upgrade directive")
		return directive.Ack{}
	}
}

// TestDirectiveUpgradeTLS_HandshakeOrder pins which side handshakes first.
//
// Destination-first is the historical order and stays the DEFAULT, so nothing
// about a run with record.upstreamTls.verify off changes. Client-first is
// selected by Config.ClientTLSFirst, which upstream verification turns on for a
// concrete reason: keploy only learns the hostname the application intended —
// its SNI — by terminating the CLIENT side. Verifying the upstream before that
// leaves it with nothing but the IP eBPF reported, which fails against any
// DNS-SAN-only certificate and drops the mock through the supervisor's
// passthrough fallback.
//
// FAILS BEFORE THE FIX: handleUpgradeTLS ran the two blocks in a fixed
// destination-then-client order with no way to invert it, so the client-first
// subtest observed ["dest" "client"].
func TestDirectiveUpgradeTLS_HandshakeOrder(t *testing.T) {
	t.Run("default is destination first", func(t *testing.T) {
		var mu sync.Mutex
		var order []string
		fn, _ := recordingUpgrader(&mu, &order, "")

		h := newHarness(t, Config{TLSUpgradeFn: fn})
		h.r.Directives() <- directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "upgrade")
		if ack := awaitAck(t, h); !ack.OK {
			t.Fatalf("TLS upgrade failed: %+v", ack)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(order) != 2 || order[0] != "dest" || order[1] != "client" {
			t.Fatalf("handshake order = %v, want [dest client]", order)
		}
	})

	t.Run("ClientTLSFirst inverts it", func(t *testing.T) {
		var mu sync.Mutex
		var order []string
		fn, _ := recordingUpgrader(&mu, &order, "")

		h := newHarness(t, Config{TLSUpgradeFn: fn, ClientTLSFirst: true})
		h.r.Directives() <- directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "upgrade")
		if ack := awaitAck(t, h); !ack.OK {
			t.Fatalf("TLS upgrade failed: %+v", ack)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(order) != 2 || order[0] != "client" || order[1] != "dest" {
			t.Fatalf("handshake order = %v, want [client dest]", order)
		}
	})
}

// TestDirectiveUpgradeTLS_SecondFailureClosesTheFirst is the invariant that
// makes the order swappable at all: whichever handshake runs second must close
// what the first produced, because neither conn is ever published until BOTH
// have succeeded. Without it, client-first leaks a live *tls.Conn wrapper on
// every dest-side verification failure — and dest-side failure is the EXPECTED
// outcome when the operator's CA is wrong, so the leak would be routine.
//
// FAILS BEFORE THE FIX in the ClientTLSFirst direction: only the client block
// closed the other side's conn, so a dest failure after a successful client
// handshake left the client wrapper open.
func TestDirectiveUpgradeTLS_SecondFailureClosesTheFirst(t *testing.T) {
	t.Run("default order: client failure closes the dest conn", func(t *testing.T) {
		var mu sync.Mutex
		var order []string
		fn, produced := recordingUpgrader(&mu, &order, "client")

		h := newHarness(t, Config{TLSUpgradeFn: fn})
		h.r.Directives() <- directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "upgrade")
		if ack := awaitAck(t, h); ack.OK {
			t.Fatal("expected the directive to fail")
		}

		mu.Lock()
		defer mu.Unlock()
		if len(*produced) != 1 {
			t.Fatalf("expected exactly one successful upgrade, got %d", len(*produced))
		}
		if !(*produced)[0].(*closeTrackingConn).wasClosed() {
			t.Fatal("the dest-side conn was not closed after the client-side handshake failed")
		}
	})

	t.Run("ClientTLSFirst: dest failure closes the client conn", func(t *testing.T) {
		var mu sync.Mutex
		var order []string
		fn, produced := recordingUpgrader(&mu, &order, "dest")

		h := newHarness(t, Config{TLSUpgradeFn: fn, ClientTLSFirst: true})
		h.r.Directives() <- directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "upgrade")
		if ack := awaitAck(t, h); ack.OK {
			t.Fatal("expected the directive to fail")
		}

		mu.Lock()
		defer mu.Unlock()
		if len(*produced) != 1 {
			t.Fatalf("expected exactly one successful upgrade, got %d", len(*produced))
		}
		if !(*produced)[0].(*closeTrackingConn).wasClosed() {
			t.Fatal("the client-side conn was not closed after the dest-side handshake failed")
		}
	})
}
