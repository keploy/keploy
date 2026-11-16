//go:build linux

package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"

	"go.keploy.io/server/v3/pkg/models"
)

// hideAppSockets makes sock_diag, as keploy's agent reads the application's
// sockets, list none whose local end is one of ends, while the proxy's own
// stay listed: what an agent whose proxy is in another network namespace than
// the application reads. The lookups of an application socket are the wait's
// and the close's (lookupWritten), the accept's (lookupAccepted) and the one
// asking whether its FIN was acknowledged (lookupOrphan, asked with its ends).
func hideAppSockets(t *testing.T, ends ...net.Addr) {
	t.Helper()
	hidden := func(a netip.AddrPort) bool {
		for _, e := range ends {
			if h, ok := addrPortOf(e); ok && h.Addr().Unmap() == a.Addr().Unmap() && h.Port() == a.Port() {
				return true
			}
		}
		return false
	}
	written, accepted, orphan := lookupWritten, lookupAccepted, lookupOrphan
	lookupWritten = func(ds *diagSock, pc *pairedConn, atClose bool) (diagTCP, error) {
		if hidden(pc.app) {
			return diagTCP{}, errNoSocket
		}
		return written(ds, pc, atClose)
	}
	lookupAccepted = func(local, remote netip.AddrPort) (diagTCP, error) {
		if hidden(local) {
			return diagTCP{}, errNoSocket
		}
		return accepted(local, remote)
	}
	lookupOrphan = func(dev uint32, local, remote netip.AddrPort) (diagTCP, error) {
		if hidden(local) {
			return diagTCP{}, errNoSocket
		}
		return orphan(dev, local, remote)
	}
	t.Cleanup(func() { lookupWritten, lookupAccepted, lookupOrphan = written, accepted, orphan })
}

// untilPeerEnded waits until the proxy's side of a connection has taken the
// application's FIN or reset.
func untilPeerEnded(t *testing.T, proxySide *net.TCPConn) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ps := readProxySide(proxySide)
		if ps.err != nil {
			t.Fatal(ps.err)
		}
		if peerEnded(ps.state) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the proxy's side is still in TCP state %d", ps.state)
		}
		time.Sleep(time.Millisecond)
	}
}

// An agent whose proxy is in another network namespace than the application
// finds none of the application's sockets: the application's end of every
// connection the proxy holds is missing from sock_diag's answers, as one gone
// would be. Until an application socket has been found, one not found is
// unknown, not gone, in a wait and at a close alike, as on darwin: the wait
// answered so, but a close found nothing on its way and charged nothing, a
// silent pass. Once one has been found, one not found is gone: its FIN or
// reset is on its way (the wait waits for it), and a close's bytes on their
// way end with it. A close after the proxy's side took the application's FIN
// or reset is known either way: nothing the application wrote is on its way.
// A close that answers unknown says so in one line naming the connection, as
// on darwin.
func TestAHiddenApplicationSocketIsUnknownNotGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seen: an application socket was found before the lookups hid them.
		seen bool
		// fin: the proxy's side took the application's FIN before the close.
		fin bool
		// close: the proxy closes the connection; else a wait reads it.
		close   bool
		unknown bool
	}{
		{name: "a wait, none ever found", unknown: true},
		{name: "a wait, one found before", seen: true},
		{name: "a close, none ever found", close: true, unknown: true},
		{name: "a close, one found before", seen: true, close: true},
		{name: "a close after the application's FIN, none ever found", fin: true, close: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
			p.proveAppVisible(proxySide)
			if !p.paired.visible.Load() {
				t.Fatal("the accept did not find the application's socket where sock_diag lists it")
			}
			if !tc.seen {
				p.paired.visible.Store(false)
			}
			hideAppSockets(t, app.LocalAddr())
			p.proveAppVisible(proxySide)
			if got := p.paired.visible.Load(); got != tc.seen {
				t.Fatalf("visible = %v once the accept could not find the application's socket, want %v", got, tc.seen)
			}
			if tc.fin {
				if err := app.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				untilPeerEnded(t, proxySide)
			}
			var err error
			lines := closeUnknownLines(p)
			if tc.close {
				_ = reader.Close()
				err = p.PairedBytesErr()
				switch {
				case !tc.unknown:
					if all := lines.FilterMessageSnippet("keploy cannot tell what the application had written").All(); len(all) != 0 {
						t.Fatalf("%d lines say the close cannot tell what was written, want none: %+v", len(all), all)
					}
				case p.holdsPairedBytes():
					wantCloseUnknown(t, p, lines, app, proxySide, zapcore.WarnLevel, "cannot be seen from the proxy's network namespace")
				default:
					wantCloseUnknown(t, p, lines, app, proxySide, zapcore.DebugLevel, "cannot be seen from the proxy's network namespace")
				}
			} else {
				if _, werr := app.Write([]byte("x")); werr != nil {
					t.Fatal(werr)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				err = p.WaitPairedBytes(ctx, 1)
				if !tc.unknown && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("the wait ended with %v, want it to wait on for the application's FIN or reset", err)
				}
			}
			if got := errors.Is(err, models.ErrProxyConnsUnsupported); got != tc.unknown {
				t.Fatalf("unknown = %v (%v), want %v", got, err, tc.unknown)
			}
		})
	}
}

// A close whose sock_diag socket, kept from an earlier close (diagFree), does
// not find the application's socket keeps that socket and looks no more: a
// socket not found is an answer, not a failed read, and a new socket would
// find no more. Only a lookup that fails drops a kept socket for a new one.
func TestACloseThatMissesTheApplicationsSocketKeepsItsListedSocket(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	p.paired.visible.Store(true)
	hideAppSockets(t, app.LocalAddr())
	emptyDiagFree(t)
	ds, err := openDiag()
	if err != nil {
		t.Fatal(err)
	}
	diagFree <- ds
	var lookups atomic.Int32
	hidden := lookupWritten
	lookupWritten = func(d *diagSock, pc *pairedConn, atClose bool) (diagTCP, error) {
		lookups.Add(1)
		return hidden(d, pc, atClose)
	}
	t.Cleanup(func() { lookupWritten = hidden })
	_ = reader.Close()
	if n := lookups.Load(); n != 1 {
		t.Fatalf("the close looked the hidden application socket up %d times, want once", n)
	}
	select {
	case got := <-diagFree:
		diagFree <- got
		if got != ds {
			t.Fatal("the close dropped its kept sock_diag socket for a socket it did not find")
		}
	default:
		t.Fatal("the close did not give its kept sock_diag socket back")
	}
	if err := p.PairedBytesErr(); err != nil {
		t.Fatalf("PairedBytesErr = %v, want nil once an application socket has been seen", err)
	}
}

// A close that finds the application's socket proves the application's
// sockets seen from the proxy's network namespace, as the accept's read does:
// a later connection's application socket not found is gone, not hidden.
func TestACloseThatFindsTheApplicationsSocketProvesThemSeen(t *testing.T) {
	p, _, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	if p.paired.visible.Load() {
		t.Fatal("seen before any proof")
	}
	_ = reader.Close()
	if !p.paired.visible.Load() {
		t.Fatal("a close that found the application's socket did not prove the application's sockets seen")
	}
	if err := p.PairedBytesErr(); err != nil {
		t.Fatalf("PairedBytesErr = %v, want nil", err)
	}
}
