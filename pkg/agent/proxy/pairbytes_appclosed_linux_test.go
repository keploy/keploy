//go:build linux

package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// holdPastFIN is a handler of tracked that holds an answer past the
// application's FIN, as the HTTP parser holds a fault whose recording found
// the application had ended its side first (fault_serve.go's
// holdUntilClosed): it reads to the FIN, marks itself idle, and waits until
// the proxy says no process holds the application's socket
// (integrations.AppClosed), then closes the connection and is done with it.
// ended is closed once it is.
func holdPastFIN(t *testing.T, p *Proxy, tracked *trackedConn, served net.Conn) (ended <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	ctx := integrations.WithRequestHolder(context.Background(), tracked)
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, tracked)
		release := integrations.MarkIdle(ctx)
		select {
		case <-integrations.AppClosed(ctx):
		case <-stop:
			release()
			return
		}
		release()
		_ = tracked.Close()
		p.paired.remove(served)
	}()
	return done
}

// awaitEnded waits, bounded, for ended to close, turning a hang into a failure.
func awaitEnded(t *testing.T, ended <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatalf("the handler did not finish %s", why)
	}
}

// socketInode returns the sockfs inode number of c via fstat(2).
func socketInode(t *testing.T, c *net.TCPConn) uint32 {
	t.Helper()
	raw, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = syscall.Fstat(int(fd), &st)
	}); err != nil {
		t.Fatal(err)
	}
	if serr != nil {
		t.Fatal(serr)
	}
	return uint32(st.Ino)
}

// A handler that holds an answer past the application's FIN waits for the
// application to read on or reset, as a half-closed one may. Once the
// application closed its socket whole (an SDK cancelling a long poll, a
// process that exits), no process holds that socket and nothing will: the
// session's last read (PairedBytesDrain) found the handler stuck, gave its
// close up as app_bytes_untold, and the run had no verdict. The drain now
// tells such a handler that the application's socket is closed, and counts
// its close. A half-closed application, whose process still holds its
// socket, gets no word until it closes it.
func TestADrainTellsAHandlerHeldPastTheAppsFINThatItsSocketIsClosed(t *testing.T) {
	t.Run("the application closed its socket whole", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		served := proxyServed(t, p)
		ended := holdPastFIN(t, p, tracked, served)
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		awaitState(t, served.(*net.TCPConn), peerEnded, "the application's FIN")
		if err := drainAnswers(t, drainReturns(context.Background(), p), "once no process held the application's socket"); err != nil {
			t.Fatalf("the drain: %v", err)
		}
		awaitEnded(t, ended, "after the drain told it the application's socket is closed")
		if stuck := stuckLosses(t, p); len(stuck) != 0 {
			t.Fatalf("the drain gave the close up as stuck: %+v; want it counted", stuck)
		}
	})
	t.Run("a half-closed application gets no word until it closes", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		served := proxyServed(t, p)
		ended := holdPastFIN(t, p, tracked, served)
		if err := app.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		awaitState(t, served.(*net.TCPConn), peerEnded, "the application's FIN")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		got := drainReturns(ctx, p)
		drainHolds(t, got, "while the half-closed application's process still held its socket")
		if pairedConnOf(t, p, 1).appClosedSaid() {
			t.Fatal("the drain marked the application's socket closed while its process still held it")
		}
		select {
		case <-ended:
			t.Fatal("the handler was told the application's socket is closed while its process held it")
		default:
		}
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		if err := drainAnswers(t, got, "once the application closed its socket"); err != nil {
			t.Fatalf("the drain: %v", err)
		}
		awaitEnded(t, ended, "after the half-closed application closed its socket")
		if stuck := stuckLosses(t, p); len(stuck) != 0 {
			t.Fatalf("the drain gave the close up as stuck: %+v; want it counted", stuck)
		}
	})
	t.Run("a FIN_WAIT1 orphan with tcp_info is decided by inode == 0", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		pc := pairedConnOf(t, p, 1)
		served := pc.conn
		wantIno := socketInode(t, app)
		if wantIno == 0 {
			t.Fatal("premise: a live socket has a nonzero sockfs inode")
		}
		ds, err := openDiag()
		if err != nil {
			t.Fatal(err)
		}
		defer ds.close()
		live, err := lookupWritten(ds, pc, false)
		if err != nil {
			t.Fatalf("lookup live socket: %v", err)
		}
		if live.inode != wantIno {
			t.Fatalf("diagTCP.inode = %d, fstat(fd).Ino = %d; sock_diag inode offset mismatch", live.inode, wantIno)
		}
		if p.appSocketClosed(pc) {
			t.Fatal("live socket reported as closed")
		}
		// Fill the buffers so app.Close() leaves a full FIN_WAIT1 orphan with
		// tcp_info present (neither TIME_WAIT nor CLOSE): only inode == 0 tells.
		_ = served.SetReadBuffer(4096)
		_ = app.SetWriteBuffer(4096)
		_ = app.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		big := bytes.Repeat([]byte("x"), 8<<20)
		if n, err := app.Write(big); err == nil || n == 0 {
			t.Fatalf("premise: the write fills the buffers and times out, wrote %d, %v", n, err)
		}
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		orphan, err := lookupWritten(ds, pc, false)
		if err != nil {
			t.Fatalf("lookup FIN_WAIT1 orphan: %v", err)
		}
		if orphan.info == nil {
			t.Fatalf("premise: FIN_WAIT1 orphan carries tcp_info (state=%d, inode=%d)", orphan.state, orphan.inode)
		}
		if orphan.state != tcpFinWait1 {
			t.Fatalf("premise: expected FIN_WAIT1 (%d), got state %d", tcpFinWait1, orphan.state)
		}
		if orphan.inode != 0 {
			t.Fatalf("FIN_WAIT1 orphan inode = %d, want 0", orphan.inode)
		}
		if !p.appSocketClosed(pc) {
			t.Fatal("appSocketClosed returned false for FIN_WAIT1 orphan with inode == 0")
		}
	})
	t.Run("socket not found counts as closed only when app sockets are visible", func(t *testing.T) {
		p, _, _ := untoldPair(t)
		pc := pairedConnOf(t, p, 1)
		orig := lookupWritten
		t.Cleanup(func() { lookupWritten = orig })
		lookupWritten = func(*diagSock, *pairedConn, bool) (diagTCP, error) {
			return diagTCP{}, errNoSocket
		}
		p.paired.visible.Store(false)
		if p.appSocketClosed(pc) {
			t.Fatal("errNoSocket counted as closed while app sockets have never been seen (may be hidden in another netns)")
		}
		p.paired.visible.Store(true)
		if !p.appSocketClosed(pc) {
			t.Fatal("errNoSocket did not count as closed once app sockets are known to be visible")
		}
	})
	t.Run("sayAppClosed skips a connection whose close has already begun", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		pc := pairedConnOf(t, p, 1)
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		awaitState(t, pc.conn, peerEnded, "the application's FIN")
		if !p.appSocketClosed(pc) {
			t.Fatal("premise: appSocketClosed must be true for the closed app socket before setting atClose")
		}
		ps := readProxySide(pc.conn)
		pc.atClose.Store(&ps)
		if p.sayAppClosed([]*pairedConn{pc}) {
			t.Fatal("sayAppClosed signalled a connection whose close had already begun (atClose != nil)")
		}
		if pc.appClosedSaid() {
			t.Fatal("markAppClosed ran on a connection whose close had already begun")
		}
	})
}

// End-to-end: the real HTTP parser holding a timeout-before-head fault marked
// AppEndedFirst on a paired trackedConn ends once the app closes its socket
// whole and PairedBytesDrain runs, with zero stuck losses.
func TestADrainEndsARealHeldHTTPFaultOnceTheAppsSocketIsClosed(t *testing.T) {
	const request = "GET /charge HTTP/1.1\r\nHost: pay.example\r\n\r\n"
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			app, src := acceptedConn(t)
			m := &models.Mock{Name: "mock-held-timeout", Kind: models.HTTP, Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://pay.example/charge", ProtoMajor: 1, ProtoMinor: 1,
					Header: map[string]string{"Host": "pay.example"}},
				Fault: &models.Fault{Kind: models.FaultTimeout, AppEndedFirst: true,
					Observed: "the application closed the connection with no answer"},
			}}
			p := replayProxyTo(t, "10.96.0.9:80", models.OutgoingOptions{}, m)
			p.settleReads = true
			p.errChannel = make(chan error, 8)
			p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: httpint.New(zap.NewNop())}
			p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
			raw := src.Conn.(*net.TCPConn)
			p.paired.add(raw, 1, 0)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			clientConn, tracked := p.acceptedConn(ctx, raw)
			if tracked == nil || tracked.appClosed == nil {
				t.Fatal("premise: acceptedConn wires appClosed for a paired connection")
			}
			done := make(chan error, 1)
			go func() {
				defer p.paired.remove(raw)
				defer p.inflight.forget(tracked)
				done <- p.handleConnection(ctx, clientConn)
			}()
			if _, err := io.WriteString(app, request); err != nil {
				t.Fatal(err)
			}
			// App closes its socket whole (e.g. SDK cancelling a long poll):
			// the hold outlasts its FIN, then PairedBytesDrain sees no process
			// holds the app socket and ends it.
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			awaitState(t, raw, peerEnded, "the application's FIN")
			// Deterministically wait until holdUntilClosed has consumed the
			// app's FIN and entered MarkIdle, then confirm the hold did not
			// exit at the FIN before the drain runs.
			deadline := time.Now().Add(10 * time.Second)
			for tracked.idle.Load() == 0 {
				select {
				case err := <-done:
					t.Fatalf("handleConnection ended (%v) at the app's FIN before the drain ran", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("holdUntilClosed did not mark the connection idle after the app's FIN")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case err := <-done:
				t.Fatalf("handleConnection ended (%v) at the app's FIN before the drain ran", err)
			default:
			}
			pc := pairedConnOf(t, p, 1)
			if err := drainAnswers(t, drainReturns(context.Background(), p), "once the real HTTP fault hold ended"); err != nil {
				t.Fatalf("the drain: %v", err)
			}
			if !pc.appClosedSaid() {
				t.Fatal("the drain did not signal appClosed to end the held fault")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("handleConnection: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("handleConnection did not exit after the drain ended the hold")
			}
			if stuck := stuckLosses(t, p); len(stuck) != 0 {
				t.Fatalf("the drain gave the close up as stuck: %+v; want it counted", stuck)
			}
		})
	}
}
