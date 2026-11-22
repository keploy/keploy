//go:build linux || darwin

package proxy

import (
	"net"
	"testing"
)

// A proxy's side keploy cannot read never took the application's FIN
// (finSeen): bytes the open test wrote may still be on their way, so a close
// keploy cannot account for charges it. A failed read carries the zero
// state, which on macOS is CLOSED, an ended peer's: only the read's error
// tells it from a side that took the FIN.
func TestASideThatCannotBeReadNeverTookTheFIN(t *testing.T) {
	app, served := loopbackPair(t)
	side := served.(*net.TCPConn)
	pc := &pairedConn{conn: side}
	halfCloseOf(t, app)
	awaitState(t, side, peerEnded, "the application's FIN")
	if !finSeen(pc) {
		t.Fatal("premise: a side that took the application's FIN has seen it")
	}
	_ = side.Close()
	if ps := readProxySide(side); ps.err == nil {
		t.Fatalf("premise: a closed side cannot be read, read state %d", ps.state)
	}
	if finSeen(pc) {
		t.Error("a side keploy cannot read counted as one that took the application's FIN: the open test would be spared bytes keploy could not see")
	}
}
