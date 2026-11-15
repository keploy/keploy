//go:build darwin

package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// hideFromTable makes macOS's TCP table, as keploy reads it, list none of the
// sockets whose local end is one of ends: what a process that macOS lists
// only its own sockets reads (on macOS 27, an agent keploy's CLI starts
// without root), the application's ends not among them.
func hideFromTable(t *testing.T, ends ...net.Addr) {
	t.Helper()
	was := readXNUTable
	readXNUTable = func() ([]xnuPCB, error) {
		table, err := was()
		var kept []xnuPCB
	next:
		for _, b := range table {
			for _, e := range ends {
				if a, _ := addrPortOf(e); b.local == a {
					continue next
				}
			}
			kept = append(kept, b)
		}
		return kept, err
	}
	t.Cleanup(func() { readXNUTable = was })
}

// hideSockets hides the sockets whose local end is one of ends from the
// table (hideFromTable) and from the descriptor tables of the processes that
// hold them (heldSocketsRead): what the agent reads of an application process
// of another user, whose descriptors it may not read.
func hideSockets(t *testing.T, ends ...net.Addr) {
	t.Helper()
	hideFromTable(t, ends...)
	was := heldSocketsRead
	heldSocketsRead = func() (map[[2]netip.AddrPort]heldSocket, error) {
		by, err := was()
		for k := range by {
			for _, e := range ends {
				if a, _ := addrPortOf(e); k[0] == unmapped(a) {
					delete(by, k)
				}
			}
		}
		return by, err
	}
	t.Cleanup(func() { heldSocketsRead = was })
}

// On macOS 27 a process keploy's CLI starts without root is listed only its
// own sockets in macOS's TCP table: the application's end of every connection
// the proxy holds is missing from it, as one gone would be. darwin assumed one
// table listing every socket: a wait read each live connection's application
// end as gone and waited for its FIN, and a close found nothing on its way, a
// silent pass. An application socket the table does not list is now gone only
// once one of the application's has been listed (proveAppVisible, at the
// accept, as on Linux); before, the wait and the close are unknown, unless the
// proxy's side took the application's FIN, after which nothing it wrote is on
// its way.
func TestAHiddenApplicationSocketIsUnknownNotGoneOnDarwin(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seen: an application socket was listed before the table hid them.
		seen bool
		// fin: the proxy's side took the application's FIN before the close.
		fin bool
		// close: the proxy closes the connection; else a wait reads it.
		close bool
		// proxyHidden: the table does not list the proxy's end either.
		proxyHidden bool
		unknown     bool
	}{
		{name: "a wait, none ever listed", unknown: true},
		{name: "a wait, one listed before", seen: true},
		{name: "a close, none ever listed", close: true, unknown: true},
		{name: "a close, one listed before", seen: true, close: true},
		{name: "a close after the application's FIN, none ever listed", fin: true, close: true},
		{name: "a close that lists neither end, none ever listed", fin: true, close: true, proxyHidden: true, unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, app, proxySide, reader := darwinPair(t)
			if !p.paired.visible.Load() {
				t.Fatal("the accept did not find the application's socket in a table that lists it")
			}
			if !tc.seen {
				p.paired.visible.Store(false)
			}
			if tc.proxyHidden {
				hideSockets(t, app.LocalAddr(), proxySide.LocalAddr())
			} else {
				hideSockets(t, app.LocalAddr())
			}
			p.proveAppVisible(proxySide)
			if got := p.paired.visible.Load(); got != tc.seen {
				t.Fatalf("visible = %v once the accept read a table that does not list the application's socket, want %v", got, tc.seen)
			}
			if tc.fin {
				if err := app.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				untilStateLeaves(t, proxySide, xnuTCPEstablished)
			}
			var err error
			if tc.close {
				_ = reader.Close()
				err = p.PairedBytesErr()
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

// The accept reads no table in a layout the proof did not find, and a close
// that finds the application's socket listed proves the table lists them as
// the accept's read does.
func TestWhatProvesTheApplicationsSocketsListedOnDarwin(t *testing.T) {
	t.Run("an unproven layout", func(t *testing.T) {
		p, _, proxySide, _ := darwinPair(t)
		p.paired.visible.Store(false)
		readable := xnuTableReadable
		xnuTableReadable = func() error { return errors.New("not the layout keploy reads") }
		t.Cleanup(func() { xnuTableReadable = readable })
		p.proveAppVisible(proxySide)
		if p.paired.visible.Load() {
			t.Fatal("the accept read a table in a layout the proof did not find")
		}
	})
	t.Run("a close", func(t *testing.T) {
		p, _, _, reader := darwinPair(t)
		p.paired.visible.Store(false)
		_ = reader.Close()
		if !p.paired.visible.Load() {
			t.Fatal("a close that found the application's socket in the table did not note the table lists them")
		}
		if err := p.PairedBytesErr(); err != nil {
			t.Fatalf("PairedBytesErr = %v after a close that found the application's socket", err)
		}
	})
}

// What the application had written to a connection is a distance between
// sequence numbers, which wrap at 2^32; the counts a wait's cut end and a
// close keep go on from the connection's first byte, so past 4 GiB a close's
// loss is not placed among the ends cut short before the wrap (lostBy).
// Before, a close past the wrap counted from 0 again, and a loss of the
// test open was charged to an end cut short gigabytes earlier.
func TestTheCountsOfWhatWasWrittenHoldPastTheWrapOnDarwin(t *testing.T) {
	p, app, _, _ := darwinPair(t)
	pc := pairedConnOf(t, p, 1)
	a, _ := addrPortOf(app.LocalAddr())
	read := readXNUTable
	t.Cleanup(func() { readXNUTable = read })
	// wrote lists the application's socket as having written d, counted
	// from the one after its first sequence number.
	wrote := func(d uint32) {
		readXNUTable = func() ([]xnuPCB, error) {
			table, err := read()
			for i := range table {
				if table[i].local == a {
					table[i].sndUna = table[i].iss + 1 + d - table[i].sndCC
				}
			}
			return table, err
		}
	}
	wrote(1<<32 - 10)
	table, err := readXNUTable()
	if err != nil {
		t.Fatal(err)
	}
	tg, err := p.pairTarget(table, pc, &heldSockets{})
	if err != nil || tg == nil || tg.want != 1<<32-10 {
		t.Fatalf("pairTarget = %+v, %v, want a target of %d", tg, err, uint64(1<<32-10))
	}
	pc.cutShort("test-1", tg.want)
	wrote(100)
	p.noteWroteAtClose(pc)
	w := pc.wroteAtClose.Load()
	if w == nil || w.want != 1<<32+100 {
		t.Fatalf("the close counted %+v, want %d written: 110 past the wait's count", w, uint64(1<<32+100))
	}
	if owners, open := pc.lostBy(w.want-50, w.want); len(owners) != 0 || !open {
		t.Fatalf("lostBy = %v, %v for bytes written past the wrap, want the open test's alone", owners, open)
	}
}

// Each read counts from the last any read found: a step back of less than
// 2^31 is an earlier read's; any other step is ahead.
func TestAWroteCountStepsBackOnlyForAnEarlierReadOnDarwin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reads []uint32
		want  uint64
	}{
		{name: "the first read", reads: []uint32{1<<31 + 5}, want: 1<<31 + 5},
		{name: "ahead past the wrap", reads: []uint32{1<<32 - 10, 100}, want: 1<<32 + 100},
		{name: "an earlier read's, back across the wrap", reads: []uint32{1<<32 - 10, 100, 1<<32 - 20}, want: 1<<32 - 20},
		{name: "an earlier read's, back to the first byte", reads: []uint32{100, 0}, want: 0},
		{name: "ahead by more than what came before", reads: []uint32{100, 1<<32 - 50}, want: 1<<32 - 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c wroteCount
			var got uint64
			for _, d := range tc.reads {
				got = c.count(d)
			}
			if got != tc.want {
				t.Fatalf("the reads %v count %d, want %d", tc.reads, got, tc.want)
			}
		})
	}
}

// Bytes lost at a closed connection past the wrap run from the first its side
// did not receive, as a count from the connection's first byte: none were
// written before an end cut short before the wrap, so they are the open
// test's, not that end's.
func TestALossPastTheWrapIsTheOpenTestsOnDarwin(t *testing.T) {
	p, pc, _, at, let := closeBegunNotDoneOnDarwin(t)
	pc.cutShort("test-1", 1<<32-5)
	base := pc.wroteAtClose.Load().base
	want := 1<<32 + at.received + 2
	pc.wroteAtClose.Store(&appWrote{want: want, base: base})
	proxyPCBIn(t, pc, xnuTCPEstablished)
	if reached, err := p.pairReached(&pairTarget{pc: pc, base: base, want: want}); err != nil || !reached {
		t.Fatalf("pairReached = %v, %v, want settled as a loss", reached, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
		t.Fatalf("%d app_bytes_lost charged to the open test, want 1", l)
	}
	if owned := p.TakeOwnedLosses(); len(owned) != 0 {
		t.Fatalf("the loss is owned by %+v, want the open test's alone: no byte of it was written before the end cut short", owned)
	}
	let()
}
