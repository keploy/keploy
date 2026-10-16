package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// cancelAfterHello is a client's connection that the client gives up on once
// its ClientHello is out: a cancel mid-handshake, as a request a timeout or
// the application's own stop ends while its connection is still being set
// up.
type cancelAfterHello struct {
	net.Conn
}

func (c cancelAfterHello) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	_ = c.Conn.Close()
	return n, err
}

// rawHello is a Go TLS 1.3 client's first flight (its ClientHello record),
// for a test to send as another client would.
func rawHello(t *testing.T, serverName string) []byte {
	t.Helper()
	cli, srv := net.Pipe()
	defer srv.Close()
	go func() {
		_ = tls.Client(cli, &tls.Config{InsecureSkipVerify: true, ServerName: serverName, MinVersion: tls.VersionTLS13}).Handshake()
		_ = cli.Close()
	}()
	head := make([]byte, 5)
	if _, err := io.ReadFull(srv, head); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(head[3])<<8|int(head[4]))
	if _, err := io.ReadFull(srv, body); err != nil {
		t.Fatal(err)
	}
	return append(head, body...)
}

// A client-side TLS handshake with keploy's proxy that fails is a capture
// loss -- its call neither recorded nor mocked -- unless the client gave it
// up: closed or reset its connection (a cancel, a timeout, its own stop). A
// rejection of keploy's certificate is a loss whether keploy reads the
// client's alert (Go's bad_certificate) or not (OpenSSL sends its TLS 1.3
// alert in plaintext, which reads as a bad record MAC), and so is a hello
// keploy refuses. A client that hangs up once keploy has sent its
// certificate, without a word, is abandoned (tls_handshake_abandoned): Node.js
// rejects a certificate so, and a client that gave up then looks the same,
// so keploy cannot tell whether a call was lost -- the test is not judged,
// never failed, never silent. One that hangs up before keploy answered
// rejected nothing.
func TestOnlyAClientThatGaveUpLostNoCallInATLSHandshake(t *testing.T) {
	for _, tc := range []struct {
		name              string
		client            func(t *testing.T, app net.Conn)
		failed, abandoned uint64
	}{
		{"a client that hangs up before keploy answered (half its ClientHello out)", func(t *testing.T, app net.Conn) {
			_, _ = app.Write(rawHello(t, "dep.example")[:20])
		}, 0, 0},
		{"a client that cancels once its ClientHello is out", func(t *testing.T, app net.Conn) {
			_ = tls.Client(cancelAfterHello{app}, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example"}).Handshake()
		}, 0, 1},
		{"a client that closes once keploy's certificate is in, without an alert (Node.js's rejection under TLS 1.3)", func(t *testing.T, app net.Conn) {
			_, _ = app.Write(rawHello(t, "dep.example"))
			_, _ = app.Read(make([]byte, 1<<14))
		}, 0, 1},
		{"a Go client that rejects keploy's certificate", func(t *testing.T, app net.Conn) {
			// Verified against the system's roots, which keploy's CA is
			// not among: the client sends a bad_certificate alert.
			_ = tls.Client(app, &tls.Config{ServerName: "dep.example"}).Handshake()
		}, 1, 0},
		{"an OpenSSL client that rejects keploy's certificate under TLS 1.3 (its alert in plaintext)", func(t *testing.T, app net.Conn) {
			_, _ = app.Write(rawHello(t, "dep.example"))
			_, _ = app.Read(make([]byte, 1<<14))
			_, _ = app.Write([]byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 48}) // unknown_ca
		}, 1, 0},
		{"a client keploy's server refuses (TLS 1.0 only)", func(t *testing.T, app net.Conn) {
			_ = tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example", MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS10}).Handshake()
		}, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := ipv4Dest(t, "127.0.0.1:8443")
			app, src := acceptedConn(t)
			p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: dest}, mysqlPorts: newMysqlPortRegistry()}
			p.BeginTestErrorCapture() // a test is running: an abandon is charged within it
			p.setSession(&agent.Session{
				Mode:            models.MODE_TEST,
				OutgoingOptions: models.OutgoingOptions{Mocking: true, DisableMysqlAutoDetect: true},
			})
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.client(t, app)
				_ = app.Close()
			}()
			err := p.handleConnection(context.Background(), src)
			if err == nil {
				t.Fatal("handleConnection returned no error for a failed handshake")
			}
			// The handshake's own error, as before: its caller tells a
			// client's EOF apart by identity.
			if errors.As(err, new(*pTls.HandshakeError)) {
				t.Fatalf("handleConnection returned keploy's wrapper %v, not the handshake's own error", err)
			}
			<-done
			got := map[models.CaptureLossKind]uint64{}
			for _, l := range p.losses.Snapshot() {
				got[l.Kind] = l.Count
				if l.What != "to 127.0.0.1:8443" {
					t.Fatalf("%s: a loss names %q, want the destination", l.Kind, l.What)
				}
			}
			if got[models.CaptureLossTLSAbort] != tc.failed || got[models.CaptureLossTLSAbandoned] != tc.abandoned || len(got) > 2 {
				t.Fatalf("%s: charged %v, want %d tls_handshake_failed and %d tls_handshake_abandoned", tc.name, got, tc.failed, tc.abandoned)
			}
		})
	}
}

// keploy's own stop cuts handshakes short: no loss of a test's.
func TestAHandshakeAStoppingAgentCutShortIsNoLoss(t *testing.T) {
	p := &Proxy{logger: zap.NewNop()}
	ctx, stop := context.WithCancel(context.Background())
	stop()
	ev := &clientTLS{p: p, ctx: ctx, dst: "10.0.0.1:443"}
	ev.HandshakeBegin()
	ev.HandshakeFailed(&pTls.HandshakeError{Err: io.ErrUnexpectedEOF, CertificateSent: true})
	ev.HandshakeFailed(&net.OpError{Op: "remote error", Err: io.ErrClosedPipe})
	ev.SilentHangUp(io.EOF)
	if l := p.losses.Snapshot(); len(l) != 0 {
		t.Fatalf("a stopping agent's handshakes were charged: %+v", l)
	}
	// Nor is a handshake keploy cut short by closing its own end, its
	// certificate out or not, in a test that would charge the client's
	// own give-up.
	p.BeginTestErrorCapture()
	live := &clientTLS{p: p, ctx: context.Background(), dst: "10.0.0.1:443"}
	live.HandshakeBegin()
	live.HandshakeFailed(&pTls.HandshakeError{Err: fmt.Errorf("read: %w", net.ErrClosed), CertificateSent: true})
	if l := p.losses.Snapshot(); len(l) != 0 {
		t.Fatalf("a handshake keploy closed was charged: %+v", l)
	}
	live.HandshakeBegin() // the client's own give-up, in the same test, is
	live.HandshakeFailed(&pTls.HandshakeError{Err: io.EOF, CertificateSent: true})
	if l := p.losses.Snapshot(); len(l) != 1 || l[0].Kind != models.CaptureLossTLSAbandoned {
		t.Fatalf("the client's give-up in a test: charged %+v, want one tls_handshake_abandoned", l)
	}
}

// A parser's TLS upgrader on the record path (MySQL's SSLRequest, Postgres's)
// makes a client-side handshake with keploy's proxy: charged as the proxy's
// own are -- here an abandon once keploy's certificate is out -- and its own
// error is what the parser gets, a client's EOF io.EOF itself, which the
// parsers above compare by identity. The upgrader's handshake was charged
// nothing.
func TestARecordUpgradersHandshakeIsChargedAndItsErrorIsItsOwn(t *testing.T) {
	app, src := acceptedConn(t)
	go func() {
		_, _ = app.Write(rawHello(t, "db.example"))
		_, _ = app.Read(make([]byte, 1<<14))
		_ = app.Close()
	}()
	p := &Proxy{logger: zap.NewNop()}
	p.BeginTestErrorCapture() // a test is running: an abandon is charged within it
	_, _, err := p.clientTLSHandler("10.0.0.5:3306")(context.Background(), zap.NewNop(), src, time.Time{})
	if err != io.EOF {
		t.Fatalf("the upgrader's handshake returned %v (%T), want io.EOF itself", err, err)
	}
	if got := p.losses.Snapshot(); len(got) != 1 || got[0].Kind != models.CaptureLossTLSAbandoned || got[0].What != "to 10.0.0.5:3306" {
		t.Fatalf("charged %+v, want one tls_handshake_abandoned to 10.0.0.5:3306", got)
	}
}

// The same for a client that hangs up as soon as the record upgrader's
// handshake is done, having sent nothing: the parser's first read of it ends
// the stream, and that is an abandon, as on every path.
func TestARecordUpgradersClientSilentSinceItsHandshakeAbandonedIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	app, src := acceptedConn(t)
	go func() {
		_ = tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "db.example", MaxVersion: tls.VersionTLS12}).Handshake()
		_ = app.Close() // no close_notify of its own
	}()
	p := &Proxy{logger: zap.NewNop()}
	p.BeginTestErrorCapture() // a test is running: an abandon is charged within it
	conn, _, err := p.clientTLSHandler("10.0.0.5:5432")(context.Background(), zap.NewNop(), src, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the parser's first read: %v, want the end of the stream", err)
	}
	if got := p.losses.Snapshot(); len(got) != 1 || got[0].Kind != models.CaptureLossTLSAbandoned || got[0].What != "to 10.0.0.5:5432" {
		t.Fatalf("charged %+v, want one tls_handshake_abandoned to 10.0.0.5:5432", got)
	}
}

// A client-side TLS upgrade a parser asks the relay for (STARTTLS, Postgres's
// SSLRequest, MySQL's SSL request) is a client-side handshake with keploy's
// proxy like any other: a client that hangs up before keploy answered lost
// nothing, one that abandons it once keploy's certificate is out leaves its
// test not judged, and a rejection is a loss of that test's. Every failure
// was charged starttls_failed -- "keploy could not make the upgrade", a taint
// of every later test -- a client's cancel included: a false red.
func TestAFailedClientTLSUpgradeIsChargedAsAnyClientHandshake(t *testing.T) {
	t.Setenv(provenswitch.Env, "1") // the client is read a record at a time after its handshake
	for _, tc := range []struct {
		name                        string
		client                      func(t *testing.T, app net.Conn)
		failed, abandoned, starttls uint64
		upgraded                    bool
	}{
		{"a client that hangs up before keploy answered", func(t *testing.T, app net.Conn) {
			_, _ = app.Write(rawHello(t, "db.example")[:20])
		}, 0, 0, 0, false},
		{"a client that closes once keploy's certificate is in, without an alert", func(t *testing.T, app net.Conn) {
			_, _ = app.Write(rawHello(t, "db.example"))
			_, _ = app.Read(make([]byte, 1<<14))
		}, 0, 1, 0, false},
		{"a Go client that rejects keploy's certificate", func(t *testing.T, app net.Conn) {
			_ = tls.Client(app, &tls.Config{ServerName: "db.example"}).Handshake()
		}, 1, 0, 0, false},
		{"a client that hangs up once the handshake is done, having sent nothing", func(t *testing.T, app net.Conn) {
			_ = tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "db.example", MaxVersion: tls.VersionTLS12}).Handshake()
		}, 0, 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop()}
			p.BeginTestErrorCapture() // a test is running: an abandon is charged within it
			app, src := acceptedConn(t)
			upstream, dst := acceptedConn(t)
			// The relay holds nothing here (no HoldClientWrites), so the
			// client speaks once the relay terminates its TLS: a
			// ClientHello written before the relay holds its forwarders
			// would be forwarded upstream, and the handshake would wait for
			// it for ever. MySQL, the only sender of this directive in
			// production, writes its ClientHello at once and is protected
			// by the client-write hold instead.
			upgrade := newProxyTLSUpgradeFn(zap.NewNop(), false, nil, nil)
			upgrading := make(chan struct{})
			r := relay.New(relay.Config{
				Logger:                  zap.NewNop(),
				OnCaptureLoss:           p.losses.Add,
				OnClientHandshakeFailed: p.relayClientHandshake(dst),
				TLSUpgradeFn: func(ctx context.Context, conn net.Conn, isClient bool, cfg *tls.Config) (net.Conn, error) {
					if !isClient {
						close(upgrading)
					}
					return upgrade(ctx, conn, isClient, cfg)
				},
			}, src, connseq.NewUpstream(dst))
			// As recordViaSupervisor runs it: what the relay's client-side
			// handshakes lose is charged to the destination.
			ctx, cancel := context.WithCancel(p.withClientTLS(context.Background(), remoteOf(dst)))
			ran := make(chan struct{})
			go func() { defer close(ran); _ = r.Run(ctx) }()
			t.Cleanup(func() { cancel(); _ = app.Close(); _ = upstream.Close(); <-ran })

			r.Directives() <- directive.UpgradeTLS(nil, &tls.Config{}, "the parser asked for TLS")
			done := make(chan struct{})
			go func() {
				defer close(done)
				<-upgrading
				tc.client(t, app)
				_ = app.Close()
			}()
			if ack := <-r.Acks(); ack.OK != tc.upgraded {
				t.Fatalf("the upgrade's ack: %+v, want OK %v", ack, tc.upgraded)
			}
			<-done
			if tc.upgraded {
				// The relay passes the client's end on to the upstream once
				// it has read it, after its hang-up was told.
				_, _ = io.Copy(io.Discard, upstream)
			}
			got := map[models.CaptureLossKind]uint64{}
			for _, l := range p.losses.Snapshot() {
				got[l.Kind] = l.Count
				if l.What != "to "+dst.RemoteAddr().String() {
					t.Fatalf("%s: a loss names %q, want the connection's destination %s", l.Kind, l.What, dst.RemoteAddr())
				}
			}
			if got[models.CaptureLossTLSAbort] != tc.failed || got[models.CaptureLossTLSAbandoned] != tc.abandoned || got[models.CaptureLossSTARTTLS] != tc.starttls || len(got) > 3 {
				t.Fatalf("%s: charged %v, want %d tls_handshake_failed, %d tls_handshake_abandoned and %d starttls_failed", tc.name, got, tc.failed, tc.abandoned, tc.starttls)
			}
		})
	}
}

// A client that hangs up on its TLS connection with keploy's proxy as soon as
// the handshake is done, having sent nothing since -- no call, no
// close_notify -- abandoned it, as a client that rejects keploy's certificate
// once the handshake is done does: curl and libcurl (TLS 1.2 and 1.3) and
// Node.js under TLS 1.2. Its test is not judged (tls_handshake_abandoned). A
// client that sends its close_notify, or a call, and then hangs up, is not
// charged: a pooled connection it used and leaves idle until it closes has
// sent bytes. These were all charged nothing.
func TestAClientSilentSinceItsHandshakeAbandonedIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name      string
		max       uint16
		client    func(t *testing.T, c *tls.Conn, raw net.Conn)
		abandoned uint64
	}{
		{"TLS 1.2, it hangs up once the handshake is done (curl's, libcurl's and Node.js's rejection)", tls.VersionTLS12, func(_ *testing.T, _ *tls.Conn, raw net.Conn) {}, 1},
		{"TLS 1.3, it hangs up once the handshake is done (curl's and libcurl's rejection)", tls.VersionTLS13, func(_ *testing.T, _ *tls.Conn, raw net.Conn) {}, 1},
		{"it sends its close_notify, then hangs up", tls.VersionTLS13, func(_ *testing.T, c *tls.Conn, _ net.Conn) { _ = c.CloseWrite() }, 0},
		{"it sends a call, waits for its answer, then hangs up idle", tls.VersionTLS13, func(t *testing.T, c *tls.Conn, _ net.Conn) {
			if _, err := c.Write([]byte("GET /health HTTP/1.1\r\nHost: dep.example\r\n\r\n")); err != nil {
				t.Error(err)
				return
			}
			_, _ = c.Read(make([]byte, 1<<14)) // an answer, or the proxy's close: the call was read
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := ipv4Dest(t, "127.0.0.1:8443")
			app, src := acceptedConn(t)
			p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: dest}, mysqlPorts: newMysqlPortRegistry()}
			p.BeginTestErrorCapture() // a test is running: an abandon is charged within it
			p.setSession(&agent.Session{
				Mode:            models.MODE_TEST,
				OutgoingOptions: models.OutgoingOptions{Mocking: true, DisableMysqlAutoDetect: true},
			})
			done := make(chan struct{})
			go func() {
				defer close(done)
				c := tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example", MaxVersion: tc.max})
				if err := c.Handshake(); err != nil {
					t.Errorf("the client's handshake: %v", err)
				} else {
					tc.client(t, c, app)
				}
				_ = app.Close() // no close_notify of its own
			}()
			_ = p.handleConnection(context.Background(), src)
			<-done
			got := map[models.CaptureLossKind]uint64{}
			for _, l := range p.losses.Snapshot() {
				got[l.Kind] = l.Count
			}
			if got[models.CaptureLossTLSAbandoned] != tc.abandoned || got[models.CaptureLossTLSAbort] != 0 {
				t.Fatalf("%s: charged %v, want %d tls_handshake_abandoned and no tls_handshake_failed", tc.name, got, tc.abandoned)
			}
		})
	}
}

// A silent client is charged only for its own hang-up: not for a read keploy
// ended itself -- a parser's cancel, keploy's own close of the connection,
// keploy's stop -- however silent the client was since its handshake.
func TestASilentClientIsChargedOnlyForItsOwnHangUp(t *testing.T) {
	p := &Proxy{logger: zap.NewNop()}
	p.BeginTestErrorCapture() // a test is running: an abandon is charged within it
	stopped, stop := context.WithCancel(context.Background())
	stop()
	ev := &clientTLS{p: p, ctx: context.Background(), dst: "10.0.0.1:443"}
	ev.HandshakeBegin()
	ev.SilentHangUp(context.Canceled)
	ev.SilentHangUp(fmt.Errorf("read: %w", net.ErrClosed))
	(&clientTLS{p: p, ctx: stopped, dst: "10.0.0.1:443"}).SilentHangUp(io.EOF)
	if l := p.losses.Snapshot(); len(l) != 0 {
		t.Fatalf("charged %+v for reads the client did not end", l)
	}
	ev.SilentHangUp(io.EOF)
	if l := p.losses.Snapshot(); len(l) != 1 || l[0].Kind != models.CaptureLossTLSAbandoned || l[0].What != "to 10.0.0.1:443" {
		t.Fatalf("charged %+v, want one tls_handshake_abandoned to 10.0.0.1:443 for the client's own hang-up", l)
	}
}

// A Go client's http.Transport keeps the connection it dialled for a request
// another connection served first: a spare, its handshake done, nothing sent.
// It goes when the application stops, closed by the kernel with no
// close_notify -- after the test it was dialled in. A hang-up like a
// rejection's, but nothing was lost: it is charged only within the scope its
// handshake began in, and this one crossed the test's end. It was charged a
// tls_handshake_abandoned, leaving a test not judged for every Go
// application that leaves a spare.
func TestASpareIdleConnectionClosedOutsideItsTestIsNoLoss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	slowIn, slowOut := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(slowIn)
			<-slowOut
		}
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	t.Cleanup(upstream.Close)
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, upstream.Listener.Addr().String())}, cfg)
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	mocks := make(chan *models.Mock, 64)
	go func() {
		for range mocks {
		}
	}()
	if err := p.Record(context.Background(), mocks, models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}

	// The application's connections reach the proxy's handler; each one's
	// raw socket is kept, to close as the kernel does at the app's exit.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var served sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			served.Add(1)
			go func() { defer served.Done(); _ = p.handleConnection(context.Background(), &closeCountingConn{Conn: c}) }()
		}
	}()
	var mu sync.Mutex
	var raw []net.Conn
	secondDial := make(chan struct{})
	dials := 0
	handshakes := make(chan struct{}, 2)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(tls.ConnectionState) error {
			handshakes <- struct{}{}
			return nil
		}},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			mu.Lock()
			dials++
			second := dials == 2
			mu.Unlock()
			if second {
				<-secondDial // dialled while the first connection was busy
			}
			c, err := net.Dial("tcp4", ln.Addr().String())
			if err == nil {
				mu.Lock()
				raw = append(raw, c)
				mu.Unlock()
			}
			return c, err
		},
	}
	client := &http.Client{Transport: tr}
	get := func(path string) <-chan error {
		done := make(chan error, 1)
		go func() {
			res, err := client.Get("https://localhost" + path) // the proxy dials the name the client sent (SNI)
			if err == nil {
				_, err = io.ReadAll(res.Body)
				_ = res.Body.Close()
			}
			done <- err
		}()
		return done
	}

	ck := containerkey.Default
	ck.Reset()
	t.Cleanup(ck.Reset)
	ck.BeginTest() // the test the application's calls are made in
	slow := get("/slow")
	awaitEvent(t, handshakes, "the first connection's handshake")
	awaitEvent(t, slowIn, "the slow call at the dependency")
	fast := get("/fast") // no idle connection: the Transport dials a second
	close(slowOut)
	if err := awaitEvent(t, slow, "the slow call"); err != nil {
		t.Fatal(err)
	}
	if err := awaitEvent(t, fast, "the fast call, on the first connection now idle"); err != nil {
		t.Fatal(err)
	}
	close(secondDial)
	awaitEvent(t, handshakes, "the spare connection's handshake")
	ck.EndTest()

	// The application stops: the kernel closes its sockets, no close_notify.
	mu.Lock()
	for _, c := range raw {
		_ = c.Close()
	}
	n := len(raw)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("%d connections dialled, want the first and the spare", n)
	}
	served.Wait()
	for _, l := range p.losses.Snapshot() {
		if l.Kind == models.CaptureLossTLSAbandoned || l.Kind == models.CaptureLossTLSAbort {
			t.Fatalf("charged %+v for a spare connection the application closed after its test, having lost nothing", l)
		}
	}
}

// An abandon is charged only when the handshake and the hang-up fall within
// one scope -- a test (the open window of a test's misses, keploy test's or a
// replay's; the Universal Mocker's open test) or the time outside tests (the
// run's start, the gap between two tests, after the last one) -- never
// across a boundary, a handshake that straddles one included. A rejection
// keploy read is charged wherever it happens.
func TestAnAbandonIsChargedOnlyWithinTheScopeItBeganIn(t *testing.T) {
	p := &Proxy{logger: zap.NewNop()}
	abandoned := func() uint64 { return lossesOf(p, models.CaptureLossTLSAbandoned) }
	handshake := func() *clientTLS {
		ev := &clientTLS{p: p, ctx: context.Background(), dst: "10.0.0.1:443"}
		ev.HandshakeBegin()
		return ev
	}
	want := func(n uint64, what string) {
		t.Helper()
		if got := abandoned(); got != n {
			t.Fatalf("%s: %d abandoned, want %d", what, got, n)
		}
	}

	handshake().SilentHangUp(io.EOF)
	want(1, "at the application's start, before any test, within one scope")

	ev := handshake()
	p.BeginTestErrorCapture() // a test begins
	ev.SilentHangUp(io.EOF)
	want(1, "a handshake before a test, hung up in it")

	ev = handshake()
	p.EndTestErrorCapture() // the test ends
	ev.SilentHangUp(io.EOF)
	want(1, "after its test ended")

	p.BeginTestErrorCapture()
	ev = handshake()
	if _, err := p.GetMockErrors(context.Background()); err != nil { // keploy test's end of a test
		t.Fatal(err)
	}
	ev.SilentHangUp(io.EOF)
	want(1, "after its test's misses were read")

	handshake().SilentHangUp(io.EOF)
	want(2, "in the gap after a test, within it")

	p.BeginTestErrorCapture()
	ev = handshake()
	p.ContinueTestErrorCapture() // the next test begins
	ev.SilentHangUp(io.EOF)
	want(2, "in the next test")

	handshake().SilentHangUp(io.EOF)
	want(3, "within its test")

	// A handshake that began in one test and ended in the next straddles a
	// boundary, as a spare whose handshake the test's end overtook does:
	// its silent hang-up is no scope's.
	ev = handshake()
	p.ContinueTestErrorCapture()
	ev.SilentHangUp(io.EOF)
	want(3, "a handshake that ended in the next test, hung up in it")

	ev = &clientTLS{p: p, ctx: context.Background(), dst: "10.0.0.1:443"}
	ev.HandshakeBegin()
	p.EndTestErrorCapture()
	ev.HandshakeFailed(&pTls.HandshakeError{Err: io.EOF, CertificateSent: true})
	want(3, "a handshake abandoned after the test it began in")
	ev.HandshakeFailed(&net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")})
	if n := lossesOf(p, models.CaptureLossTLSAbort); n != 1 {
		t.Fatalf("a rejection: %d failed, want 1", n)
	}

	ck := containerkey.Default
	ck.Reset()
	t.Cleanup(ck.Reset)
	ck.BeginTest()
	ev = handshake()
	ck.EndTest()
	ev.SilentHangUp(io.EOF)
	want(3, "the Universal Mocker's test, hung up after it")
	handshake().SilentHangUp(io.EOF)
	want(4, "the Universal Mocker's gap after a test, within it")
	ck.BeginTest()
	handshake().SilentHangUp(io.EOF)
	want(5, "the Universal Mocker's test, within it")
}
