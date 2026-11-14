//go:build darwin

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Plaintext the application wrote to a connection before the proxy began to
// close it, still on its way to the proxy's socket, was charged by nothing on
// macOS: the close read nothing of the application's socket, so it found
// nothing on its way, and a connection the close dropped was judged by no
// wait, a silent loss. The close now reads what the application had written
// from macOS's TCP table and waits for those bytes by reading the socket, as
// on Linux: data that arrives is a loss; the application's FIN or its reset
// with none before it is none, and so is keploy's own stop. Its FIN alone on
// its way is none, and not read at all.
//
// A fixture on loopback cannot hold bytes on their way with nothing unread on
// demand (a zero window holds them only behind a full receive buffer), so
// the close's read of the table is made to count more than the application
// wrote: 24 bytes, unless named, and its FIN once its socket reads as closed
// (FIN_WAIT_1). The application ends them once the close has sent its FIN.
// The loss is the bytes past what the proxy had received at the close, up to
// what the application had written by then, counted from the connection's
// first sequence number: a test whose end was cut short owns it only when
// some of those were written before that end (pairedConn.lostBy), and the
// test open now is charged when some were written after it. On main the
// data cases count none.
func TestPlaintextOnItsWayAtTheProxysCloseIsReadThroughOnDarwin(t *testing.T) {
	type onItsWay struct {
		n   uint32
		fin bool
	}
	cases := []struct {
		name  string
		more  onItsWay
		cut   uint64 // an end cut short after a byte written and read, waiting for cut
		owner string
		want  uint64
	}{
		{name: "data", more: onItsWay{24, false}, want: 1},
		{name: "its FIN", more: onItsWay{24, false}},
		{name: "its reset", more: onItsWay{24, false}},
		{name: "keploy's stop", more: onItsWay{24, false}},
		{name: "a failed read", more: onItsWay{24, false}, want: 1},
		{name: "its FIN alone on its way", more: onItsWay{0, true}},
		{name: "a byte and its FIN on their way", more: onItsWay{1, true}, want: 1},
		{name: "data, after a cut end's bytes arrived", more: onItsWay{24, false}, cut: 1, want: 1},
		{name: "data, all written before a cut end", more: onItsWay{24, false}, cut: 25, owner: "test-1"},
		{name: "data, written before a cut end and after it", more: onItsWay{24, false}, cut: 13, owner: "test-1", want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, app, proxySide, reader := darwinPair(t)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			reader.beforeClose = func() { p.pairClosing(ctx, proxySide, reader) }
			pc := pairedConnOf(t, p, 1)
			if tc.cut > 0 {
				// A byte written and read, then an end cut short that
				// waited for cut: 1, that byte; 25, that byte and the
				// 24 the close counts on their way; 13, that byte and
				// the first 12 of those, the rest written after the
				// cut end, in the test open now.
				if _, err := app.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
					t.Fatal(err)
				}
				pc.cutShort("test-1", tc.cut)
			}
			a, _ := addrPortOf(app.LocalAddr())
			r, _ := addrPortOf(app.RemoteAddr())
			was := readXNUTable
			readXNUTable = func() ([]xnuPCB, error) {
				table, err := was()
				for i := range table {
					if table[i].local == a && table[i].remote == r {
						table[i].sndCC += tc.more.n
						if tc.more.fin {
							table[i].state = xnuTCPFinWait1
						}
					}
				}
				return table, err
			}
			t.Cleanup(func() { readXNUTable = was })
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				_ = reader.Close()
			}()
			for pc.atClose.Load() == nil {
				runtime.Gosched()
			}
			readXNUTable = was
			if _, err := io.ReadAll(app); err != nil { // the close's FIN
				t.Fatal(err)
			}
			switch tc.name {
			case "its FIN":
				if err := app.CloseWrite(); err != nil {
					t.Fatal(err)
				}
			case "its reset":
				if err := app.SetLinger(0); err != nil {
					t.Fatal(err)
				}
				_ = app.Close()
			case "keploy's stop":
				stop()
			case "a failed read":
				_ = proxySide.Close()
			default:
				if _, err := app.Write([]byte("publish 2\n")); err != nil {
					t.Fatal(err)
				}
			}
			<-closed
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != tc.want {
				t.Fatalf("app_bytes_lost = %d once the proxy's close of a connection with bytes on their way ended, want %d", l, tc.want)
			}
			var owners []string
			for _, o := range p.TakeOwnedLosses() {
				owners = append(owners, o.Test)
			}
			if fmt.Sprint(owners) != fmt.Sprint(map[bool][]string{true: {tc.owner}}[tc.owner != ""]) {
				t.Fatalf("the loss is owned by %v, want %q's alone (or none)", owners, tc.owner)
			}
			if tc.name == "its FIN" || tc.name == "its reset" {
				if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
					t.Fatalf("WaitPairedBytes: %v, want settled: the close read the connection to its end", err)
				}
			}
		})
	}
	t.Run("nothing on its way", func(t *testing.T) {
		// The application has written and the proxy read it all; its
		// socket stays open: a close that read the socket would wait
		// until the application closes.
		p, app, _, reader := darwinPair(t)
		if _, err := app.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			_ = reader.Close()
		}()
		select {
		case <-closed:
		case <-time.After(30 * time.Second):
			t.Fatal("the close of a connection with nothing on its way waited for the application")
		}
		if _, err := io.ReadAll(app); err != nil {
			t.Fatal(err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("app_bytes_lost = %d, want 0", l)
		}
	})
	t.Run("its socket gone from the table", func(t *testing.T) {
		// The table lists the application's socket with another first
		// sequence number than the one the proxy's side began from: not
		// the socket the proxy holds the other end of, so nothing is
		// counted, and the close does not wait.
		p, app, _, reader := darwinPair(t)
		a, _ := addrPortOf(app.LocalAddr())
		was := readXNUTable
		readXNUTable = func() ([]xnuPCB, error) {
			table, err := was()
			for i := range table {
				if table[i].local == a {
					table[i].sndCC += 24
					table[i].iss++
				}
			}
			return table, err
		}
		t.Cleanup(func() { readXNUTable = was })
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			_ = reader.Close()
		}()
		select {
		case <-closed:
		case <-time.After(30 * time.Second):
			t.Fatal("the close waited on a count read from another connection's socket")
		}
		readXNUTable = was
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("app_bytes_lost = %d, want 0", l)
		}
		if w := pairedConnOf(t, p, 1).wroteAtClose.Load(); w != nil {
			t.Fatalf("the close counted %+v from another connection's socket", *w)
		}
	})
}

// Bytes the close tosses unread beside the application's FIN are placed among
// the ends cut short on the connection, as on Linux: the close reads where the
// connection began from the table even once the proxy's side has the FIN, as
// the side's own count does not say. A byte written and read, then an end cut
// short that waited for cut, then 10 bytes and the FIN the handler never
// reads: 5, that byte and the first 4 of the 10, the rest written after it, in
// the test open now; 11, all of them. On main the cut end owns none.
func TestUnreadBytesBesideTheFINArePlacedAmongTheCutEndsOnDarwin(t *testing.T) {
	for _, tc := range []struct {
		cut  uint64
		want uint64
	}{{5, 1}, {11, 0}} {
		t.Run(fmt.Sprint("cut at ", tc.cut), func(t *testing.T) {
			p, app, proxySide, reader := darwinPair(t)
			pc := pairedConnOf(t, p, 1)
			if _, err := app.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			pc.cutShort("test-1", tc.cut)
			if _, err := app.Write([]byte("0123456789")); err != nil {
				t.Fatal(err)
			}
			if err := app.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			untilUnread(t, proxySide, 10)
			if s := untilStateLeaves(t, proxySide, xnuTCPEstablished); s != xnuTCPCloseWait {
				t.Fatalf("the proxy's side is in TCP state %d, want CLOSE_WAIT", s)
			}
			_ = reader.Close()
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != tc.want {
				t.Fatalf("app_bytes_lost = %d, want %d", l, tc.want)
			}
			var owners []string
			for _, o := range p.TakeOwnedLosses() {
				owners = append(owners, o.Test)
			}
			if fmt.Sprint(owners) != "[test-1]" {
				t.Fatalf("the loss is owned by %v, want test-1's", owners)
			}
		})
	}
}

// A macOS whose table keploy cannot read in the layout it reads leaves every
// wait unknown (xnuTableReadable): the close counts nothing from it either,
// so counts read in another layout never hold a close.
func TestAnUnprovenTableHoldsNoCloseOnDarwin(t *testing.T) {
	p, app, _, reader := darwinPair(t)
	a, _ := addrPortOf(app.LocalAddr())
	readable, read := xnuTableReadable, readXNUTable
	xnuTableReadable = func() error { return errors.New("not the layout keploy reads") }
	readXNUTable = func() ([]xnuPCB, error) {
		table, err := read()
		for i := range table {
			if table[i].local == a {
				table[i].sndCC += 24
			}
		}
		return table, err
	}
	t.Cleanup(func() { xnuTableReadable, readXNUTable = readable, read })
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = reader.Close()
	}()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("the close waited on counts read in a layout the proof did not find")
	}
	if w := pairedConnOf(t, p, 1).wroteAtClose.Load(); w != nil {
		t.Fatalf("the close counted %+v from a table in a layout the proof did not find", *w)
	}
}

// What a proxy's side received of the application's data, at a close that
// counted it, leaves out the application's FIN once the side took it; with no
// count it is the most it could be.
func TestDataReceivedLeavesOutTheFINOnDarwin(t *testing.T) {
	for _, tc := range []struct {
		ps   proxySide
		want uint64
	}{
		{proxySide{received: 11}, math.MaxUint64},
		{proxySide{received: 11, proxySideOS: proxySideOS{counted: true}, state: xnuTCPEstablished}, 11},
		{proxySide{received: 11, proxySideOS: proxySideOS{counted: true}, state: xnuTCPFinWait1}, 11},
		{proxySide{received: 11, proxySideOS: proxySideOS{counted: true}, state: xnuTCPCloseWait}, 10},
		{proxySide{received: 11, proxySideOS: proxySideOS{counted: true}, state: xnuTCPLastAck}, 10},
		{proxySide{received: 11, proxySideOS: proxySideOS{counted: true}, state: xnuTCPClosing}, 10},
		{proxySide{received: 11, proxySideOS: proxySideOS{counted: true}, state: xnuTCPTimeWait}, 10},
		{proxySide{received: 0, proxySideOS: proxySideOS{counted: true}, state: xnuTCPCloseWait}, 0},
	} {
		if got := dataReceived(tc.ps); got != tc.want {
			t.Errorf("dataReceived(%+v) = %d, want %d", tc.ps, got, tc.want)
		}
	}
}

// The close's count of what the proxy's side received is a distance between
// sequence numbers from the application socket's first, as what it wrote is:
// it holds across the wrap at 2^32.
func TestTheClosesCountHoldsAcrossTheSequenceWrapOnDarwin(t *testing.T) {
	for _, tc := range []struct {
		isn, rcvNxt uint32
		want, got   uint64
	}{
		{isn: 1000, rcvNxt: 1001 + 10, want: 34, got: 10},
		{isn: 1<<32 - 5, rcvNxt: 20, want: 40, got: 24},
		{isn: 1<<32 - 5, rcvNxt: 1<<32 - 2, want: 40, got: 2},
		{isn: 7, rcvNxt: 8 + 50, want: 40, got: 50}, // past what was written: written after the read
		{isn: 7, rcvNxt: 8, want: 0, got: 0},
		// What was written wrapped and what was received has not: short
		// of it by more than its count, nothing counts as received.
		{isn: 7, rcvNxt: 6, want: 3, got: 0},
	} {
		w := appWrote{want: tc.want, appWroteOS: appWroteOS{isn: tc.isn}}
		if got := w.received(tc.rcvNxt); got != tc.got {
			t.Errorf("received(%d) from isn %d with %d written = %d, want %d", tc.rcvNxt, tc.isn, tc.want, got, tc.got)
		}
	}
}
