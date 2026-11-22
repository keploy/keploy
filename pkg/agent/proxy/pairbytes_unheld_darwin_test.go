//go:build darwin

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
)

// An HTTP client that reads the proxy's answer with Connection: close (the
// 502 of a call no mock answers) closes its socket at once, and the proxy's
// close of its side comes right after. Its socket is then held by no process
// keploy's agent may read, and its FIN, sent at once, reaches the proxy's side
// only when loopback's input thread hands it over: on a loaded host, after
// the proxy's close has read the side. That close was unknown, and the test
// open lost its verdict (macos-native-e2e's miss test and TestClock came out
// NOT JUDGED). Now, once the application's sockets have been read from their
// holders, the close waits for that FIN while keploy could run for
// orphanFINBudget.

// unheldPair is heldPair with "hello" written and read, the application's
// send buffer empty, and its socket then held by no process the agent may read
// (hideSockets), as one its holder closed: the close's wait begins from it.
// proven says the application's sockets were read from their holders before
// (paired.held).
func unheldPair(t *testing.T, proven bool) (p *Proxy, app *net.TCPConn, reader *trackedConn, logs *observer.ObservedLogs) {
	t.Helper()
	p, app, side, reader := heldPair(t)
	p.settleReads = true
	core, logs := observer.New(zapcore.WarnLevel)
	p.logger = zap.New(core)
	if _, err := app.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	untilUnread(t, side, 5)
	if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	pcs, _ := p.paired.upTo(1)
	untilSendBufferEmpty(t, pcs[0])
	pcs[0].holder.Store(nil)
	hideSockets(t, app.LocalAddr())
	p.paired.held.Store(proven)
	return p, app, reader, logs
}

// closeWithin closes reader, failing the row if the close takes over d.
func closeWithin(t *testing.T, app *net.TCPConn, reader *trackedConn, d time.Duration, why string) {
	t.Helper()
	closed := make(chan struct{})
	go func() { _ = reader.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(d):
		_ = app.Close()
		<-closed
		t.Fatal(why)
	}
}

// wantNoCloseUnknown checks that a close was accounted for: no line says it
// cannot tell what was written, and nothing is charged.
func wantNoCloseUnknown(t *testing.T, p *Proxy, logs *observer.ObservedLogs) {
	t.Helper()
	if n := len(logs.FilterMessageSnippet("keploy cannot tell what the application had written").All()); n != 0 {
		t.Fatalf("%d unknown close lines, want none: %+v", n, logs.All())
	}
	if l := lossesOf(p, models.CaptureLossAppBytesUntold); l != 0 {
		t.Fatalf("%d untold closes charged, want none", l)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d bytes losses, want none: nothing was on its way", l)
	}
	if err := p.PairedBytesErr(); err != nil {
		t.Fatalf("PairedBytesErr = %v, want nil", err)
	}
}

func TestACloseWaitsForTheFINOfASocketItsHolderClosedOnDarwin(t *testing.T) {
	t.Run("its FIN taken while the close waits", func(t *testing.T) {
		// The FIN comes after the close's first read of the side, as
		// loopback hands it over late: the close waits, sees it taken,
		// and makes no record, as for a side the table shows past it.
		p, app, reader, logs := unheldPair(t, true)
		was := readHeldProxySide
		var reads atomic.Int32
		readHeldProxySide = func(c *net.TCPConn) proxySide {
			if reads.Add(1) == 1 {
				go func() {
					time.Sleep(30 * time.Millisecond)
					_ = app.CloseWrite()
				}()
			}
			return was(c)
		}
		t.Cleanup(func() { readHeldProxySide = was })
		pcs, _ := p.paired.upTo(1)
		closeWithin(t, app, reader, 10*time.Second, "the close waited 10s for a FIN the application sent 30ms in")
		if reads.Load() < 2 {
			t.Fatalf("the close read the proxy's side %d times, want it to look again until the FIN", reads.Load())
		}
		if pcs[0].wroteAtClose.Load() != nil {
			t.Fatalf("the close's record = %+v, want none: the side took the FIN", pcs[0].wroteAtClose.Load())
		}
		wantNoCloseUnknown(t, p, logs)
	})
	t.Run("its FIN taken before the close's first look", func(t *testing.T) {
		// The FIN reaches the proxy's side after the table's read, before
		// the close looks at the side: the close does not wait.
		p, app, reader, logs := unheldPair(t, true)
		clk := &stall.ManualClock{Step: stall.OnTime}
		p.paired.clk = clk
		was := readHeldProxySide
		var reads atomic.Int32
		readHeldProxySide = func(c *net.TCPConn) proxySide {
			if reads.Add(1) == 1 {
				_ = app.CloseWrite()
				deadline := time.Now().Add(10 * time.Second)
				for ps := was(c); ps.err == nil && !peerEnded(ps.state) && time.Now().Before(deadline); ps = was(c) {
					time.Sleep(time.Millisecond)
				}
			}
			return was(c)
		}
		t.Cleanup(func() { readHeldProxySide = was })
		closeWithin(t, app, reader, 10*time.Second, "the close waited for a FIN its side had taken")
		if reads.Load() != 1 || clk.Waits != 0 {
			t.Fatalf("the close read the proxy's side %d times and waited %d times, want one read and no wait: the side had taken the FIN", reads.Load(), clk.Waits)
		}
		wantNoCloseUnknown(t, p, logs)
	})
	t.Run("no application socket read from its holder yet", func(t *testing.T) {
		// A process the agent may not read may hold it open, sending no
		// FIN: the close does not wait, and is unknown at once.
		p, app, reader, logs := unheldPair(t, false)
		closeWithin(t, app, reader, 10*time.Second, "the close waited for a FIN with no application socket ever read from its holder")
		wantOneCloseUnknownLine(t, p, logs, "no application socket has been read from its holder yet")
	})
	t.Run("bytes unread in the proxy's side", func(t *testing.T) {
		// The close tosses them, a loss it charges; it does not wait for
		// a FIN behind them, which a receive window they filled may hold
		// back for good.
		p, app, reader, logs := unheldPair(t, true)
		if _, err := app.Write([]byte("again")); err != nil {
			t.Fatal(err)
		}
		pcs, _ := p.paired.upTo(1)
		untilUnread(t, pcs[0].conn, 5)
		closeWithin(t, app, reader, 10*time.Second, "the close waited for a FIN behind bytes it tosses")
		lines := logs.FilterMessageSnippet("keploy cannot tell what the application had written").All()
		if len(lines) != 1 {
			t.Fatalf("%d unknown close lines, want one: %+v", len(lines), logs.All())
		}
		if e, _ := lines[0].ContextMap()["error"].(string); !strings.Contains(e, "holds bytes unread") {
			t.Fatalf("the line's error is %q, want it to say the side holds bytes unread", e)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("%d bytes losses, want the bytes the close tossed", l)
		}
	})
	t.Run("keploy stops while the close waits", func(t *testing.T) {
		// keploy's own stop ends the wait and charges nothing: nothing
		// more of the run is judged.
		p, app, reader, logs := unheldPair(t, true)
		clk := &stall.ManualClock{}
		p.paired.clk = clk
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pcs, _ := p.paired.upTo(1)
		reader.beforeClose = func() { p.pairClosing(ctx, pcs[0].conn, reader) }
		closed := make(chan struct{})
		go func() { _ = reader.Close(); close(closed) }()
		deadline := time.Now().Add(10 * time.Second)
		for clk.Pending() == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the close never waited for the FIN")
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			_ = app.Close()
			t.Fatal("keploy's stop did not end the close's wait")
		}
		wantNoCloseUnknown(t, p, logs)
	})
	t.Run("the proxy's side cannot be read while the close waits", func(t *testing.T) {
		p, app, reader, logs := unheldPair(t, true)
		was := readHeldProxySide
		var reads atomic.Int32
		readHeldProxySide = func(c *net.TCPConn) proxySide {
			if reads.Add(1) == 1 {
				return was(c)
			}
			return proxySide{err: errors.New("bad file descriptor")}
		}
		t.Cleanup(func() { readHeldProxySide = was })
		closeWithin(t, app, reader, 10*time.Second, "the close waited on a side it cannot read")
		wantOneCloseUnknownLine(t, p, logs, "the proxy's side of it cannot be read: bad file descriptor")
	})
	t.Run("the proxy's side cannot be read in a test that moved nothing on the connection", func(t *testing.T) {
		// The FIN went unseen, so bytes of the open test's may be on
		// their way: it is charged though the proxy moved nothing on the
		// connection since the last end was done with it (pairUntold: the
		// side has not taken the FIN).
		p, app, reader, logs := unheldPair(t, true)
		pc := pairedConnOf(t, p, 1)
		p.paired.markEnd(1, 0)
		if pc.usedSinceEnd() {
			t.Fatal("premise: the open test moved nothing on the connection")
		}
		was := readHeldProxySide
		var reads atomic.Int32
		readHeldProxySide = func(c *net.TCPConn) proxySide {
			if reads.Add(1) == 1 {
				return was(c)
			}
			return proxySide{err: errors.New("bad file descriptor")}
		}
		t.Cleanup(func() { readHeldProxySide = was })
		closeWithin(t, app, reader, 10*time.Second, "the close waited on a side it cannot read")
		wantOneCloseUnknownLine(t, p, logs, "the proxy's side of it cannot be read: bad file descriptor")
	})
	t.Run("the proxy's side's tcp_info cannot be read in a test that moved nothing on the connection", func(t *testing.T) {
		// A read of the side's tcp_info that fails gives XNU state 0,
		// CLOSED, which is what a side that took the application's reset
		// shows (peerEnded). A failed read saw no FIN: the open test is
		// charged though it moved nothing on the connection and the
		// side holds no byte unread.
		p, app, reader, logs := unheldPair(t, true)
		pc := pairedConnOf(t, p, 1)
		p.paired.markEnd(1, 0)
		if pc.usedSinceEnd() {
			t.Fatal("premise: the open test moved nothing on the connection")
		}
		if n, err := readUnread(pc.conn); err != nil || n != 0 {
			t.Fatalf("premise: the side holds %d bytes unread (%v), want 0 and a read: only the read of its tcp_info fails", n, err)
		}
		var spoilt atomic.Bool
		onTCPOptRead(t, func(fd, opt int, buf []byte, macOS func(int, int, []byte) (uint32, error)) (uint32, error) {
			if spoilt.Load() && opt == xnuTCPInfoOpt {
				return 0, errors.New("bad file descriptor")
			}
			return macOS(fd, opt, buf)
		})
		was := readHeldProxySide
		var reads atomic.Int32
		readHeldProxySide = func(c *net.TCPConn) proxySide {
			// The close's first look reads the side; every later read of
			// its tcp_info fails, pairUntold's among them.
			if reads.Add(1) > 1 {
				spoilt.Store(true)
			}
			return was(c)
		}
		t.Cleanup(func() { readHeldProxySide = was })
		closeWithin(t, app, reader, 10*time.Second, "the close waited on a side it cannot read")
		wantOneCloseUnknownLine(t, p, logs, "the proxy's side of it cannot be read: getsockopt TCP_INFO: bad file descriptor")
	})
}

// A test's end marks the connections its bytes wait answers for (markEnd), on
// macOS as on Linux. A close in a later test that never used the connection
// that waited for the application's FIN, sent after every byte it wrote, and
// saw it, is accounted for (TestACloseWaitsForTheFINOfASocketItsHolderClosedOnDarwin).
// One that did not wait for it, no application socket read from its holder
// yet, cannot rule out bytes of the open test's on their way unseen: it
// charges the open test (pairUntold: the side has not taken the FIN), left
// NOT JUDGED.
func TestAnEndsWaitMarksTheConnectionsItAnswersForOnDarwin(t *testing.T) {
	p, app, _, reader := darwinPair(t)
	p.settleReads = true
	pc := pairedConnOf(t, p, 1)
	if !pc.usedSinceEnd() {
		t.Fatal("premise: the open test used the connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.WaitPairedBytes(ctx, 1); err != nil {
		t.Fatalf("the end's wait: %v", err)
	}
	if pc.usedSinceEnd() {
		t.Fatal("the end's wait did not mark the connection it answers for")
	}
	// The next test's close of it cannot tell what was on its way: no
	// application socket was read from its holder.
	hideSockets(t, app.LocalAddr())
	p.paired.visible.Store(false)
	p.paired.held.Store(false)
	pc.holder.Store(nil)
	core, logs := observer.New(zapcore.InfoLevel)
	p.logger = zap.New(core)
	closeWithin(t, app, reader, 10*time.Second, "the close waited with no application socket read from its holder")
	if l := lossesOf(p, models.CaptureLossAppBytesUntold); l != 1 {
		t.Errorf("%d untold closes charged, want one: the close did not see the FIN, so bytes of the open test's may have been on their way", l)
	}
	said := logs.FilterMessageSnippet("keploy cannot tell what the application had written").All()
	if len(said) != 1 || said[0].Level != zap.WarnLevel {
		t.Errorf("lines %+v, want one warn line: the close is charged to the open test", said)
	}
}
