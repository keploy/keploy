//go:build linux || darwin

package proxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// drainReturns runs PairedBytesDrain on ctx and hands its answer back.
func drainReturns(ctx context.Context, p *Proxy) <-chan error {
	got := make(chan error, 1)
	go func() { got <- p.PairedBytesDrain(ctx) }()
	return got
}

// drainHolds fails t if the drain answered within a while: it still waits for
// a close the proxy has not counted. A drain that does not wait answers at
// once, well within it.
func drainHolds(t *testing.T, got <-chan error, why string) {
	t.Helper()
	select {
	case err := <-got:
		t.Fatalf("the drain answered (%v) %s", err, why)
	case <-time.After(300 * time.Millisecond):
	}
}

// drainAnswers is the drain's answer, which must come once nothing holds it.
func drainAnswers(t *testing.T, got <-chan error, why string) error {
	t.Helper()
	select {
	case err := <-got:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("the drain did not answer %s", why)
		return nil
	}
}

// awaitState waits, bounded, until the proxy's side of conn is in a state ok
// says: the kernel's, which a write or close on loopback reaches at once.
func awaitState(t *testing.T, conn *net.TCPConn, ok func(uint8) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ps := readProxySide(conn)
		if ps.err == nil && ok(ps.state) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("premise: the proxy's side never reached %s (state %d, %v)", what, ps.state, ps.err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestADrainWaitsForEachWayAnApplicationEndCanEnd pins drainsFor, case by
// case: a drain waits for a connection whose close the proxy has begun, or
// whose application end has ended in any of the ways it can (its socket gone,
// its FIN sent, the proxy's side holding its FIN or reset, or a proxy side
// that cannot be read), and only for those. The proxy's side is read only
// when the rest does not tell. A socket row cannot reach every case: a gone
// application socket whose FIN the proxy's side has not taken yet lasts only
// while a reset is on its way.
func TestADrainWaitsForEachWayAnApplicationEndCanEnd(t *testing.T) {
	// The proxy side's states, whatever the OS numbers them.
	var ended, open uint8
	var haveEnded, haveOpen bool
	for s := range 256 {
		if peerEnded(uint8(s)) {
			ended, haveEnded = uint8(s), true
		} else if !haveOpen {
			open, haveOpen = uint8(s), true
		}
	}
	if !haveEnded || !haveOpen {
		t.Fatalf("peerEnded tells no state apart (ended %v, open %v)", haveEnded, haveOpen)
	}
	openSide := proxySide{state: open}
	for _, c := range []struct {
		name       string
		closeBegun bool
		app        *pairTarget
		side       proxySide
		want       bool
		readsSide  bool
	}{
		{name: "its close begun", closeBegun: true, app: &pairTarget{}, side: openSide, want: true},
		{name: "its application socket gone", app: &pairTarget{gone: true}, side: openSide, want: true},
		{name: "its application socket sent its FIN", app: &pairTarget{finCounted: true}, side: openSide, want: true},
		{name: "the proxy's side took its FIN or reset", app: &pairTarget{}, side: proxySide{state: ended}, want: true, readsSide: true},
		{name: "the proxy's side cannot be read", app: &pairTarget{}, side: proxySide{state: open, err: errors.New("bad file descriptor")}, want: true, readsSide: true},
		{name: "its application sockets not read, the proxy's side ended", side: proxySide{state: ended}, want: true, readsSide: true},
		{name: "held open by the application", app: &pairTarget{}, side: openSide, want: false, readsSide: true},
		{name: "its application sockets not read, held open", side: openSide, want: false, readsSide: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			read := false
			got := drainsFor(c.closeBegun, c.app, func() proxySide { read = true; return c.side })
			if got != c.want {
				t.Errorf("drainsFor = %v, want %v", got, c.want)
			}
			if read != c.readsSide {
				t.Errorf("the proxy's side read: %v, want %v", read, c.readsSide)
			}
		})
	}
}
