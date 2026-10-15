package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/models"
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

// A parser's TLS upgrader (the record path's) gets the handshake's own error,
// as before keploy told abandoned handshakes apart: a client's EOF is io.EOF
// itself, which the parsers above it compare by identity.
func TestARecordUpgradersHandshakeErrorIsTheHandshakesOwn(t *testing.T) {
	app, src := acceptedConn(t)
	go func() {
		_, _ = app.Write(rawHello(t, "db.example"))
		_, _ = app.Read(make([]byte, 1<<14))
		_ = app.Close()
	}()
	_, _, err := handleClientTLS(context.Background(), zap.NewNop(), src, time.Time{})
	if err != io.EOF {
		t.Fatalf("handleClientTLS returned %v (%T), want io.EOF itself", err, err)
	}
}
