//go:build linux

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
)

// tlsHeldPair is heldPair served over TLS as the proxy serves it: its side's
// handshake by pTls.HandleTLSConnection, which hands the layer to the
// connection the proxy closes with care. The application has its answer to
// one call read: its client is past any session ticket.
func tlsHeldPair(t *testing.T, version uint16) (p *Proxy, app, proxySide *net.TCPConn, tracked *trackedConn, srv net.Conn, cli *tls.Conn) {
	t.Helper()
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	tracked = reader.(*trackedConn)
	cli = tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example", MinVersion: version, MaxVersion: version})
	errc := make(chan error, 1)
	go func() { errc <- cli.Handshake() }()
	srv, _, err := pTls.HandleTLSConnection(context.Background(), zap.NewNop(), reader, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if tracked.tlsLayer.Load() == nil {
		t.Fatal("the proxy's TLS handshake did not hand its layer to the connection it closes with care")
	}
	if _, err := srv.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(cli, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	return p, app, proxySide, tracked, srv, cli
}

// unreadArrived returns once the proxy's socket holds n bytes unread or more.
func unreadArrived(t *testing.T, c *net.TCPConn, n int) {
	t.Helper()
	for {
		got, err := readUnread(c)
		if err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		runtime.Gosched()
	}
}

var tlsVersions = []struct {
	name    string
	version uint16
}{{"TLS 1.2", tls.VersionTLS12}, {"TLS 1.3", tls.VersionTLS13}}

// A TLS client's goodbye -- its quit, then its close_notify and FIN, as
// go-sql-driver/mysql's Close sends them (COM_QUIT; pgx's Terminate and
// gRPC's GOAWAY end with one too) -- that the proxy's handler takes only the
// quit of (the MySQL replayer answers COM_QUIT with EOF and closes) leaves
// the close_notify unread. It carries no call: no loss. Data behind the quit
// still is one. The close_notify was charged a loss, for most closes of a
// TLS client under KEPLOY_PROVEN: a false red.
func TestATLSClientsCloseNotifyLeftUnreadIsNoLoss(t *testing.T) {
	for _, v := range tlsVersions {
		for _, finIn := range []bool{true, false} {
			for _, data := range []bool{false, true} {
				name := v.name + ", its FIN in"
				if !finIn {
					name = v.name + ", its FIN not yet in"
				}
				if data {
					name += ", data behind the quit"
				}
				t.Run(name, func(t *testing.T) {
					p, _, proxySide, _, srv, cli := tlsHeldPair(t, v.version)
					if _, err := cli.Write([]byte{1, 0, 0, 0, 1}); err != nil { // COM_QUIT
						t.Fatal(err)
					}
					if _, err := io.ReadFull(srv, make([]byte, 5)); err != nil {
						t.Fatal(err)
					}
					if data {
						if _, err := cli.Write([]byte("publish 2\n")); err != nil {
							t.Fatal(err)
						}
					}
					if finIn {
						_ = cli.Close() // close_notify, then FIN
						for readProxySide(proxySide).state != tcpCloseWait {
							runtime.Gosched()
						}
					} else {
						if !data {
							if err := cli.CloseWrite(); err != nil { // close_notify, its FIN still to come
								t.Fatal(err)
							}
						}
						unreadArrived(t, proxySide, 1)
					}
					_ = srv.Close()
					want := uint64(0)
					if data {
						want = 1
					}
					if l := lossesOf(p, models.CaptureLossAppBytesLost); l != want {
						t.Fatalf("app_bytes_lost = %d at the proxy's close, want %d", l, want)
					}
				})
			}
		}
	}
}

// A record that is not all in as the proxy closes holds the close while the
// layer reads on: the application, told the proxy is done (its FIN, sent
// before the read), ends its side, and its FIN ends the read (what it cut
// short in a record's middle it dropped itself: no loss); one that never
// ends its side is ended by keploy's stop, which charges nothing. Neither is
// a clock.
func TestAPartTLSRecordAtTheCloseWaitsForTheFINOrTheStop(t *testing.T) {
	for _, end := range []string{"the application's FIN", "keploy's stop"} {
		t.Run(end, func(t *testing.T) {
			p, app, proxySide, tracked, srv, _ := tlsHeldPair(t, tls.VersionTLS12)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			tracked.beforeClose = func() { p.pairClosing(ctx, proxySide, tracked) }
			// An application-data record's header for 100 bytes, and 10 of
			// them: written to the socket under the client's layer.
			if _, err := app.Write(append([]byte{23, 3, 3, 0, 100}, make([]byte, 10)...)); err != nil {
				t.Fatal(err)
			}
			unreadArrived(t, proxySide, 15)
			// The application ends its side once it has seen the proxy's
			// end, as a client does (nothing else would), and once the test
			// has seen the close wait.
			sawEnd, waited := make(chan struct{}), make(chan struct{})
			letGo := sync.OnceFunc(func() { close(waited) })
			t.Cleanup(letGo)
			if end == "the application's FIN" {
				go func() {
					_, _ = io.Copy(io.Discard, app)
					close(sawEnd)
					<-waited
					_ = app.Close()
				}()
			}
			closed := make(chan struct{})
			go func() { _ = srv.Close(); close(closed) }()
			for tracked.reading.Load() == 0 { // the close reads on through the layer
				select {
				case <-closed:
					t.Fatal("the close did not wait for the rest of a record not all in")
				default:
				}
				runtime.Gosched()
			}
			if end == "keploy's stop" {
				stop()
			} else {
				select {
				case <-sawEnd:
				case <-time.After(time.Minute): // only turns a hang into a failure
					t.Fatal("the application never saw the proxy's end of the connection while the close waited for it")
				}
				letGo()
			}
			select {
			case <-closed:
			case <-time.After(time.Minute): // only turns a hang into a failure
				t.Fatalf("%s did not end the close's read", end)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
				t.Fatalf("app_bytes_lost = %d, want none", l)
			}
		})
	}
}

// keploy's own stop closes the proxy's connections whatever they hold: what
// the application wrote that the handler never read is no loss then, and no
// WARN says it is. It was charged, and said, at every stop with a connection
// mid-call.
func TestKeploysOwnStopChargesNoLoss(t *testing.T) {
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	core, logs := observer.New(zapcore.WarnLevel)
	p.logger = zap.New(core)
	tracked := reader.(*trackedConn)
	ctx, stop := context.WithCancel(context.Background())
	tracked.beforeClose = func() { p.pairClosing(ctx, proxySide, tracked) }
	if _, err := app.Write([]byte("publish 1\npublish 2\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	for received(t, proxySide) < 20 {
		runtime.Gosched()
	}
	stop() // keploy's stop
	_ = reader.Close()
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("app_bytes_lost = %d at keploy's own stop, want none", l)
	}
	if n := logs.Len(); n != 0 {
		t.Fatalf("keploy's own stop said %d WARNs: %v", n, logs.All())
	}
}

// A close whose unread bytes cannot be counted is a loss of that connection's,
// whether or not a wait holds it, as well as leaving every later wait
// unknown. Only a wait holding it was told.
func TestAnUncountableCloseIsALossWithNoWaitHoldingIt(t *testing.T) {
	p, _, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	_ = proxySide.Close() // its socket gone: nothing can be read of it
	p.pairClosing(context.Background(), proxySide, reader.(*trackedConn))
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
		t.Fatalf("app_bytes_lost = %d for a close whose unread bytes could not be counted, want 1", l)
	}
	if err := p.paired.failure(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("later waits: %v, want unknown (%v)", err, models.ErrProxyConnsUnsupported)
	}
}

// The proxy's handshake takes the connection wrapped (util.Conn, as the main
// path wraps it to give back what it peeked): its layer still reaches the
// connection the proxy closes with care, through the wrapper's NetConn.
func TestTheLayerReachesTheTrackedConnThroughAWrapper(t *testing.T) {
	_, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	tracked := reader.(*trackedConn)
	wrapped := &util.Conn{Conn: tracked, Reader: tracked, Logger: zap.NewNop()}
	cli := tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example"})
	errc := make(chan error, 1)
	go func() { errc <- cli.Handshake() }()
	if _, _, err := pTls.HandleTLSConnection(context.Background(), zap.NewNop(), wrapped, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if tracked.tlsLayer.Load() == nil {
		t.Fatal("the layer did not reach the tracked connection through util.Conn")
	}
}

// A read deadline the handler left set -- a poll's, or a nudge's in the past
// (relay's nudgeDeadline) -- is no end of the close's read: the close_notify
// behind it is still no loss. The read failed at once on it, a loss: the very
// false red the layer's read fixes.
func TestACloseNotifyBehindALeftoverDeadlineIsNoLoss(t *testing.T) {
	for _, v := range tlsVersions {
		t.Run(v.name, func(t *testing.T) {
			p, _, proxySide, _, srv, cli := tlsHeldPair(t, v.version)
			if _, err := cli.Write([]byte{1, 0, 0, 0, 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(srv, make([]byte, 5)); err != nil {
				t.Fatal(err)
			}
			_ = srv.SetReadDeadline(time.Unix(1, 0)) // left by the handler
			_ = cli.Close()
			for readProxySide(proxySide).state != tcpCloseWait {
				runtime.Gosched()
			}
			_ = srv.Close()
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
				t.Fatalf("app_bytes_lost = %d for a close_notify behind a deadline the handler left, want 0", l)
			}
		})
	}
}

// A connection whose close is held reading what its socket holds still holds
// work for a replay's test end (pending): its handler is not done, and a miss
// it files once the close returns would land after an end that had passed.
// The close's read looked like its handler waiting for the application.
func TestAConnectionInItsCloseHoldsWork(t *testing.T) {
	p, app, proxySide, tracked, srv, _ := tlsHeldPair(t, tls.VersionTLS12)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	tracked.beforeClose = func() { p.pairClosing(ctx, proxySide, tracked) }
	// An application-data record's header for 100 bytes, and 10 of them.
	if _, err := app.Write(append([]byte{23, 3, 3, 0, 100}, make([]byte, 10)...)); err != nil {
		t.Fatal(err)
	}
	unreadArrived(t, proxySide, 15)
	closed := make(chan struct{})
	go func() { _ = srv.Close(); close(closed) }()
	for inq(tracked) != 0 || tracked.reading.Load() == 0 { // the close reads on through the layer
		select {
		case <-closed:
			t.Fatal("the close did not wait for the rest of a record not all in")
		default:
		}
		runtime.Gosched()
	}
	if pending, _ := p.inflight.State(true); pending == 0 {
		t.Fatal("a connection in its close reads as holding no work to a replay's test end")
	}
	stop()
	<-closed
}

// Another reader of the connection re-arms a deadline while the close's read
// waits for the rest of a record -- a parser's poll does, before its next
// read -- and that is no end of the read either: the application ends in the
// record's middle, which is no loss. The read ended on the deadline, and the
// bytes were charged a loss.
func TestADeadlineSetDuringTheClosesReadIsNoEndOfIt(t *testing.T) {
	p, app, proxySide, tracked, srv, _ := tlsHeldPair(t, tls.VersionTLS12)
	// An application-data record's header for 100 bytes, and 10 of them.
	if _, err := app.Write(append([]byte{23, 3, 3, 0, 100}, make([]byte, 10)...)); err != nil {
		t.Fatal(err)
	}
	unreadArrived(t, proxySide, 15)
	sawEnd := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, app); close(sawEnd) }()
	closed := make(chan struct{})
	go func() { _ = srv.Close(); close(closed) }()
	for inq(tracked) != 0 || tracked.reading.Load() == 0 { // the close reads on through the layer
		select {
		case <-closed:
			t.Fatal("the close did not wait for the rest of a record not all in")
		default:
		}
		runtime.Gosched()
	}
	_ = proxySide.SetReadDeadline(time.Unix(1, 0)) // another reader's
	select {
	case <-sawEnd: // the proxy's end reached the application
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the application never saw the proxy's end of the connection")
	}
	_ = app.Close()
	select {
	case <-closed:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the application's FIN did not end the close's read")
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("app_bytes_lost = %d, want 0: a deadline another reader set ended the close's read", l)
	}
}

// A TLS client that meets keploy's close -- it reads the answer keploy closed
// after (a 502 with Connection: close), then closes, its close_notify first --
// writes that close_notify after keploy's close: no capture loss. The proxy's
// orphan answers it with a reset, as the client's own close does with
// keploy's close_notify unread, so a wait finds the client's socket gone, or,
// read before that, the close_notify counted as written. Neither is charged,
// and neither is unknown. Both were, for HTTPS dependencies, with no load
// needed.
func TestATLSClientsCloseNotifyAfterTheProxysCloseIsNoLoss(t *testing.T) {
	for _, v := range tlsVersions {
		t.Run(v.name, func(t *testing.T) {
			p, app, _, _, srv, cli := tlsHeldPair(t, v.version)
			pc := pairedConnOf(t, p, 1)
			_ = srv.Close() // keploy's close_notify, then its close
			w := pc.wroteAtClose.Load()
			at := pc.atClose.Load()
			if w == nil || at == nil || w.want != at.received {
				t.Fatalf("the close's reads: written %+v, received %+v; want all the client wrote received", w, at)
			}
			_ = cli.Close() // the client's close_notify, then its close
			// What a wait reads of the client's socket before the reset:
			// the close_notify record and the FIN.
			target := &pairTarget{pc: pc, want: w.want + 25, finCounted: true}
			if reached, err := p.pairReached(target); err != nil || !reached {
				t.Fatalf("pairReached = %v, %v, want settled", reached, err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
				t.Fatalf("%d app_bytes_lost charged for the client's close_notify, want none", l)
			}
			a, pr := mustAddrPort(t, app.LocalAddr()), mustAddrPort(t, app.RemoteAddr())
			for {
				if _, err := lookupTCP(a, pr); errors.Is(err, errNoSocket) {
					break
				}
				runtime.Gosched()
			}
			if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
				t.Fatalf("WaitPairedBytes with the client's socket reset: %v, want settled", err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
				t.Fatalf("%d app_bytes_lost charged, want none", l)
			}
		})
	}
}

// Bytes a TLS client wrote before keploy's close that are still on their way
// as it begins -- the close's read of the client's socket counts them, the
// proxy's side has not received them -- are read through the TLS layer as
// unread ones are: a close_notify is no loss, and the read waits for it; data
// is a loss. A fixture on loopback cannot hold bytes on their way on demand,
// so the close's read is made to count 24 more than were written, and the
// client writes them once the close has begun.
func TestATLSClientsBytesOnTheirWayAtTheProxysCloseAreReadThrough(t *testing.T) {
	for _, v := range tlsVersions {
		for _, data := range []bool{false, true} {
			name := v.name + ", its close_notify"
			if data {
				name = v.name + ", data"
			}
			t.Run(name, func(t *testing.T) {
				p, _, _, _, srv, cli := tlsHeldPair(t, v.version)
				pc := pairedConnOf(t, p, 1)
				was := lookupWritten
				lookupWritten = func(ds *diagSock, a, b netip.AddrPort) (diagTCP, error) {
					d, err := was(ds, a, b)
					if err == nil && d.info != nil {
						d.wqueue += 24
					}
					return d, err
				}
				t.Cleanup(func() { lookupWritten = was })
				closed := make(chan struct{})
				go func() {
					defer close(closed)
					_ = srv.Close()
				}()
				for pc.atClose.Load() == nil {
					runtime.Gosched()
				}
				lookupWritten = was
				if data {
					if _, err := cli.Write([]byte("publish 2\n")); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := io.ReadAll(cli); err != nil { // keploy's close_notify
						t.Fatal(err)
					}
					_ = cli.Close() // its own close_notify, then its FIN
				}
				<-closed
				want := uint64(0)
				if data {
					want = 1
				}
				if l := lossesOf(p, models.CaptureLossAppBytesLost); l != want {
					t.Fatalf("app_bytes_lost = %d at the proxy's close, want %d", l, want)
				}
				if data {
					return
				}
				if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
					t.Fatalf("WaitPairedBytes: %v, want settled: the bytes on their way were the client's close_notify", err)
				}
				if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
					t.Fatalf("%d app_bytes_lost charged, want none", l)
				}
			})
		}
	}
}
