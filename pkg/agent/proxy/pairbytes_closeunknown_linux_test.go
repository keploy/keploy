//go:build linux

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// closeUnknownLines is p's log from now on, where wantCloseUnknown finds the
// lines a close that cannot tell what the application had written says
// (closeUnknown).
func closeUnknownLines(p *Proxy) *observer.ObservedLogs {
	core, logs := observer.New(zapcore.DebugLevel)
	p.logger = zap.New(core)
	return logs
}

// wantCloseUnknown checks that the close of app's connection to the proxy
// could not tell what the application had written, said in one line at level
// naming the connection and an error that says why (has), and that it is a
// loss of the tests the connection could have served, here the test open, and
// of no later one: PairedBytesErr, whose error every later wait would end
// with, stays nil. A proxy whose reads do not wait for the paired
// connections' bytes says it at debug level and charges nothing.
func wantCloseUnknown(t *testing.T, p *Proxy, lines *observer.ObservedLogs, app, proxySide net.Conn, level zapcore.Level, has string) {
	t.Helper()
	if err := p.PairedBytesErr(); err != nil {
		t.Fatalf("PairedBytesErr = %v after a close that could not tell what was written, want nil: every later wait would end with it", err)
	}
	conn := app.LocalAddr().String() + " -> " + proxySide.LocalAddr().String()
	var all []observer.LoggedEntry
	for _, l := range lines.FilterMessageSnippet("keploy cannot tell what the application had written").All() {
		if l.ContextMap()["connection"] == conn {
			all = append(all, l)
		}
	}
	if len(all) != 1 {
		t.Fatalf("%d lines say the close of %s cannot tell what was written, want one: %+v", len(all), conn, lines.All())
	}
	if all[0].Level != level {
		t.Errorf("the line is at level %v, want %v", all[0].Level, level)
	}
	if e, _ := all[0].ContextMap()["error"].(string); !strings.Contains(e, has) {
		t.Errorf("the line's error is %q, want it to say %q", e, has)
	}
	want := uint64(0)
	if level == zapcore.WarnLevel {
		want = 1
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != want {
		t.Errorf("%d bytes losses, want %d: the close is a loss of the test open, for a proxy that holds the paired bytes", l, want)
	}
}

// A close whose read of the application's socket fails, not one that does not
// find it, cannot tell what was on its way: before, it recorded nothing and
// answered nothing, and a wait judged the connection by what was written when
// it began, so bytes on their way at the close passed unseen. It is a loss
// of the test open now, said in one line naming the connection, the read and
// its error.
func TestACloseWhoseReadFailsIsUnknown(t *testing.T) {
	t.Run("no sock_diag socket", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		p.settleReads = true
		lines := closeUnknownLines(p)
		emptyDiagFree(t)
		failOpenDiag(t)
		_ = reader.Close()
		wantCloseUnknown(t, p, lines, app, proxySide, zapcore.WarnLevel, "sock_diag socket: too many open files")
	})
	t.Run("no new socket once a kept one's lookup failed", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		p.settleReads = true
		lines := closeUnknownLines(p)
		emptyDiagFree(t)
		ds, err := openDiag()
		if err != nil {
			t.Fatal(err)
		}
		diagFree <- ds
		failLookupWritten(t)
		failOpenDiag(t)
		_ = reader.Close()
		wantCloseUnknown(t, p, lines, app, proxySide, zapcore.WarnLevel, "sock_diag socket: too many open files")
	})
	t.Run("a kept socket's lookup that fails, and the new one's too", func(t *testing.T) {
		// The close drops the kept socket for a new one, and the new
		// one's lookup fails as well: unknown, not a pass on the error
		// of the first.
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		p.settleReads = true
		lines := closeUnknownLines(p)
		emptyDiagFree(t)
		ds, err := openDiag()
		if err != nil {
			t.Fatal(err)
		}
		diagFree <- ds
		var looked []*diagSock
		was := lookupWritten
		lookupWritten = func(ds *diagSock, _ *pairedConn, _ bool) (diagTCP, error) {
			looked = append(looked, ds)
			return diagTCP{}, errors.New("netlink: no buffer space")
		}
		t.Cleanup(func() { lookupWritten = was })
		_ = reader.Close()
		if len(looked) != 2 || looked[0] == looked[1] {
			t.Fatalf("the close looked up on %d sockets (%v), want the kept one and a new one", len(looked), looked)
		}
		wantCloseUnknown(t, p, lines, app, proxySide, zapcore.WarnLevel, "netlink: no buffer space")
	})
	t.Run("a lookup that fails", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		p.settleReads = true
		lines := closeUnknownLines(p)
		emptyDiagFree(t)
		failLookupWritten(t)
		_ = reader.Close()
		wantCloseUnknown(t, p, lines, app, proxySide, zapcore.WarnLevel, "netlink: no buffer space")
	})
}

// Each close that cannot tell is said in its own line naming its connection,
// and is a loss of the tests that connection could have served: one per
// connection, never an answer for every wait. A proxy whose reads do not wait
// for the paired connections' bytes vouches for none: it says so at debug
// level and charges nothing.
func TestEachCloseThatCannotTellIsALossOfItsOwnConnection(t *testing.T) {
	for _, settle := range []bool{true, false} {
		level := zapcore.WarnLevel
		if !settle {
			level = zapcore.DebugLevel
		}
		t.Run(level.String(), func(t *testing.T) {
			if !settle && provenswitch.On() {
				t.Skip("under the proven switch every proxy holds the paired bytes")
			}
			p := &Proxy{logger: zap.NewNop(), settleReads: settle}
			app, proxySide, reader := heldPairOn(t, p, 1, "tcp4", "127.0.0.1:0")
			_, _, reader2 := heldPairOn(t, p, 2, "tcp4", "127.0.0.1:0")
			lines := closeUnknownLines(p)
			emptyDiagFree(t)
			failLookupWritten(t)
			_ = reader.Close()
			wantCloseUnknown(t, p, lines, app, proxySide, level, "netlink: no buffer space")
			_ = reader2.Close()
			if n := len(lines.FilterMessageSnippet("keploy cannot tell what the application had written").All()); n != 2 {
				t.Fatalf("%d lines say a close cannot tell what was written, want one per connection: %+v", n, lines.All())
			}
			want := uint64(0)
			if settle {
				want = 2
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != want {
				t.Fatalf("%d bytes losses after two such closes, want %d", l, want)
			}
		})
	}
}

// A close that cannot tell what was on its way leaves no other wait without
// its answer: a later wait on another connection ends once that connection's
// bytes are in, as it would with no such close. Before, the first such close
// was every later wait's error, so one connection the agent could not account
// for left every later test without a verdict.
func TestACloseThatCannotTellLeavesLaterWaitsTheirOwnAnswer(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), settleReads: true}
	app, proxySide, reader := heldPairOn(t, p, 1, "tcp4", "127.0.0.1:0")
	app2, _, reader2 := heldPairOn(t, p, 2, "tcp4", "127.0.0.1:0")
	lines := closeUnknownLines(p)
	emptyDiagFree(t)
	was := lookupWritten
	failLookupWritten(t)
	_ = reader.Close()
	lookupWritten = was
	wantCloseUnknown(t, p, lines, app, proxySide, zapcore.WarnLevel, "netlink: no buffer space")
	if _, err := app2.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader2, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.WaitPairedBytes(ctx, 2); err != nil {
		t.Fatalf("a wait on a connection whose bytes are in ended with %v, want nil: another connection's close that could not tell is not its answer", err)
	}
}

func failOpenDiag(t *testing.T) {
	t.Helper()
	was := openDiag
	openDiag = func() (*diagSock, error) { return nil, errors.New("sock_diag socket: too many open files") }
	t.Cleanup(func() { openDiag = was })
}

func failLookupWritten(t *testing.T) {
	t.Helper()
	was := lookupWritten
	lookupWritten = func(*diagSock, *pairedConn, bool) (diagTCP, error) {
		return diagTCP{}, errors.New("netlink: no buffer space")
	}
	t.Cleanup(func() { lookupWritten = was })
}

// A close whose read of the bytes its socket holds unread fails cannot tell
// whether it tossed bytes the application wrote: that is a loss of the tests
// the connection could have served, said in one line, and never every later
// wait's answer. Before, it failed every wait from then on.
func TestACloseWhoseUnreadCountCannotBeReadIsALossOfItsOwnConnection(t *testing.T) {
	p, _, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	p.settleReads = true
	lines := closeUnknownLines(p)
	was := lookupWritten
	lookupWritten = func(ds *diagSock, pc *pairedConn, atClose bool) (diagTCP, error) {
		d, err := was(ds, pc, atClose)
		// The descriptor goes after the close read the application's
		// socket, before it reads its own unread count.
		_ = proxySide.Close()
		return d, err
	}
	t.Cleanup(func() { lookupWritten = was })
	_ = reader.Close()
	if err := p.PairedBytesErr(); err != nil {
		t.Fatalf("PairedBytesErr = %v after a close whose unread count could not be read, want nil: every later wait would end with it", err)
	}
	said := lines.FilterMessageSnippet("keploy cannot tell whether its proxy closed a connection with bytes the application wrote still unread").All()
	if len(said) != 1 {
		t.Fatalf("%d lines say the close cannot tell what it left unread, want one: %+v", len(said), lines.All())
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
		t.Fatalf("%d bytes losses, want 1: the close is a loss of the test open", l)
	}
}
