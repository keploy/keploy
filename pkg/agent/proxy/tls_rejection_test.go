package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
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
	p.chargeClientHandshake(ctx, &pTls.HandshakeError{Err: io.ErrUnexpectedEOF, CertificateSent: true}, "10.0.0.1:443")
	p.chargeClientHandshake(ctx, &net.OpError{Op: "remote error", Err: io.ErrClosedPipe}, "10.0.0.1:443")
	if l := p.losses.Snapshot(); len(l) != 0 {
		t.Fatalf("a stopping agent's handshakes were charged: %+v", l)
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
	_, _, err := p.clientTLSHandler("10.0.0.5:3306")(context.Background(), zap.NewNop(), src, time.Time{})
	if err != io.EOF {
		t.Fatalf("the upgrader's handshake returned %v (%T), want io.EOF itself", err, err)
	}
	if got := p.losses.Snapshot(); len(got) != 1 || got[0].Kind != models.CaptureLossTLSAbandoned || got[0].What != "to 10.0.0.5:3306" {
		t.Fatalf("charged %+v, want one tls_handshake_abandoned to 10.0.0.5:3306", got)
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
	for _, tc := range []struct {
		name                        string
		client                      func(t *testing.T, app net.Conn)
		failed, abandoned, starttls uint64
	}{
		{"a client that hangs up before keploy answered", func(t *testing.T, app net.Conn) {
			_, _ = app.Write(rawHello(t, "db.example")[:20])
		}, 0, 0, 0},
		{"a client that closes once keploy's certificate is in, without an alert", func(t *testing.T, app net.Conn) {
			_, _ = app.Write(rawHello(t, "db.example"))
			_, _ = app.Read(make([]byte, 1<<14))
		}, 0, 1, 0},
		{"a Go client that rejects keploy's certificate", func(t *testing.T, app net.Conn) {
			_ = tls.Client(app, &tls.Config{ServerName: "db.example"}).Handshake()
		}, 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop()}
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
			ctx, cancel := context.WithCancel(context.Background())
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
			if ack := <-r.Acks(); ack.OK {
				t.Fatal("the upgrade succeeded")
			}
			<-done
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
	t.Setenv(provenswitch.Env, "1")
	app, src := acceptedConn(t)
	go func() {
		_ = tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example"}).Handshake()
	}()
	conn, _, err := pTls.HandleTLSConnection(context.Background(), zap.NewNop(), src, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{logger: zap.NewNop()}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	p.chargeSilentClient(context.Background(), context.Canceled, conn, "10.0.0.1:443")
	p.chargeSilentClient(context.Background(), fmt.Errorf("read: %w", net.ErrClosed), conn, "10.0.0.1:443")
	p.chargeSilentClient(stopped, io.EOF, conn, "10.0.0.1:443")
	if l := p.losses.Snapshot(); len(l) != 0 {
		t.Fatalf("charged %+v for reads the client did not end", l)
	}
	p.chargeSilentClient(context.Background(), io.EOF, conn, "10.0.0.1:443")
	if l := p.losses.Snapshot(); len(l) != 1 || l[0].Kind != models.CaptureLossTLSAbandoned || l[0].What != "to 10.0.0.1:443" {
		t.Fatalf("charged %+v, want one tls_handshake_abandoned to 10.0.0.1:443 for the client's own hang-up", l)
	}
}
