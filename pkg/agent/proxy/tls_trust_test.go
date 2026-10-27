package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// What a client does on one connection to keploy's TLS server.
var (
	// sendsACall completes its handshake, sends a call, reads its answer
	// (or the proxy's close) and hangs up.
	sendsACall = func(t *testing.T, app net.Conn) {
		c := tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example", MinVersion: tls.VersionTLS13})
		if err := c.Handshake(); err != nil {
			t.Errorf("the client's handshake: %v", err)
			return
		}
		if _, err := c.Write([]byte("GET /health HTTP/1.1\r\nHost: dep.example\r\n\r\n")); err != nil {
			t.Error(err)
			return
		}
		_, _ = c.Read(make([]byte, 1<<14))
	}
	// sendsAPlainCall sends a call in plain text, no TLS, and reads its
	// answer (or the proxy's close).
	sendsAPlainCall = func(t *testing.T, app net.Conn) {
		if _, err := app.Write([]byte("GET /health HTTP/1.1\r\nHost: dep.example\r\n\r\n")); err != nil {
			t.Error(err)
			return
		}
		_, _ = app.Read(make([]byte, 1<<14))
	}
	// hangsUpSilent completes its handshake and hangs up having sent
	// nothing: a browser's unused connection, or Node.js's rejection.
	hangsUpSilent = func(t *testing.T, app net.Conn) {
		if err := tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example", MinVersion: tls.VersionTLS13}).Handshake(); err != nil {
			t.Errorf("the client's handshake: %v", err)
		}
	}
	// sendsCloseNotify completes its handshake, sends its close_notify and
	// hangs up: a byte after the handshake, but no call.
	sendsCloseNotify = func(t *testing.T, app net.Conn) {
		c := tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example", MinVersion: tls.VersionTLS13})
		if err := c.Handshake(); err != nil {
			t.Errorf("the client's handshake: %v", err)
			return
		}
		_ = c.CloseWrite()
		_, _ = c.Read(make([]byte, 1)) // keploy's end of it
	}
	// givesUpOnceTheCertificateIsIn hangs up once keploy's certificate is
	// in, without a word: a page closed mid-handshake.
	givesUpOnceTheCertificateIsIn = func(t *testing.T, app net.Conn) {
		_, _ = app.Write(rawHello(t, "dep.example"))
		_, _ = app.Read(make([]byte, 1<<14))
	}
	// rejectsTheCertificate is a Go client verifying against the system's
	// roots, which keploy's CA is not among: a bad_certificate alert.
	rejectsTheCertificate = func(_ *testing.T, app net.Conn) {
		_ = tls.Client(app, &tls.Config{ServerName: "dep.example"}).Handshake()
	}
)

// trustProxy is a replaying proxy with a test running, whose TLS losses a
// row reads after each connection (serveFrom).
func trustProxy() *Proxy {
	p := &Proxy{logger: zap.NewNop(), mysqlPorts: newMysqlPortRegistry()}
	p.BeginTestErrorCapture() // a test is running: an abandon is charged within it
	p.setSession(&agent.Session{
		Mode:            models.MODE_TEST,
		OutgoingOptions: models.OutgoingOptions{Mocking: true, DisableMysqlAutoDetect: true},
	})
	return p
}

// serveFrom has p serve one connection from the process tgid (the eBPF
// redirect's KernelPid; 0: unknown) on which the client does what client
// does, and returns p's TLS losses so far: tls_handshake_abandoned, then
// tls_handshake_failed.
func serveFrom(t *testing.T, p *Proxy, tgid uint32, client func(*testing.T, net.Conn)) (abandoned, failed uint64) {
	t.Helper()
	dest := ipv4Dest(t, "127.0.0.1:8443")
	dest.KernelPid = tgid
	p.DestInfo = fakeDestInfo{dest: dest}
	app, src := acceptedConn(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		client(t, app)
		_ = app.Close() // no close_notify of its own
	}()
	_ = p.handleConnection(context.Background(), src)
	<-done
	for _, l := range p.losses.Snapshot() {
		switch l.Kind {
		case models.CaptureLossTLSAbandoned:
			abandoned = l.Count
		case models.CaptureLossTLSAbort:
			failed = l.Count
		}
	}
	return abandoned, failed
}

// A client process that sent a call through keploy's certificate in this run
// trusts keploy's CA: its later hang-ups -- silent once a handshake is done,
// or once keploy's certificate is in -- are cancels, not capture losses, as a
// browser leaves the connections it opened ahead of a call and those of a
// page it closes. A handshake it fails in another way is still charged, and
// another process's hang-up still is. Before, each hang-up left the test not
// judged.
func TestAProcessThatSentACallThroughKeploysCertificateCancelsItsHangUps(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := trustProxy()
	const browser, other = 4242, 5151
	if a, f := serveFrom(t, p, browser, sendsACall); a != 0 || f != 0 {
		t.Fatalf("a call: charged %d abandoned and %d failed, want none", a, f)
	}
	if a, _ := serveFrom(t, p, browser, hangsUpSilent); a != 0 {
		t.Fatalf("a silent hang-up after the process's call: charged %d tls_handshake_abandoned, want 0 (a cancel)", a)
	}
	if a, _ := serveFrom(t, p, browser, givesUpOnceTheCertificateIsIn); a != 0 {
		t.Fatalf("a handshake given up after the process's call: charged %d tls_handshake_abandoned, want 0 (a cancel)", a)
	}
	if a, f := serveFrom(t, p, browser, rejectsTheCertificate); a != 0 || f != 1 {
		t.Fatalf("a rejection by the process: charged %d abandoned and %d failed, want 1 tls_handshake_failed", a, f)
	}
	if a, _ := serveFrom(t, p, other, hangsUpSilent); a != 1 {
		t.Fatalf("another process's silent hang-up: charged %d tls_handshake_abandoned, want 1", a)
	}
	if a, _ := serveFrom(t, p, other, givesUpOnceTheCertificateIsIn); a != 2 {
		t.Fatalf("another process's handshake given up: charged %d tls_handshake_abandoned in all, want 2", a)
	}
}

// Only a call makes a process trusted, and only from then on: a hang-up
// before its first call is charged; a process whose handshakes all end
// silent -- Node.js rejects a certificate so -- or with a close_notify and no
// call is never trusted, so each of its silent hang-ups is charged, nor is one
// whose only call was in plain text; and a connection whose process is
// unknown (tgid 0) trusts no other.
func TestOnlyACallMakesAProcessTrustedAndOnlyFromThenOn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t.Run("a hang-up before the process's first call", func(t *testing.T) {
		p := trustProxy()
		if a, _ := serveFrom(t, p, 4242, hangsUpSilent); a != 1 {
			t.Fatalf("charged %d tls_handshake_abandoned, want 1", a)
		}
		serveFrom(t, p, 4242, sendsACall)
		if a, _ := serveFrom(t, p, 4242, hangsUpSilent); a != 1 {
			t.Fatalf("after the call: charged %d tls_handshake_abandoned in all, want still 1", a)
		}
	})
	t.Run("a process whose handshakes end silent or with a close_notify", func(t *testing.T) {
		p := trustProxy()
		serveFrom(t, p, 4242, hangsUpSilent)
		serveFrom(t, p, 4242, sendsCloseNotify)
		if a, _ := serveFrom(t, p, 4242, hangsUpSilent); a != 2 {
			t.Fatalf("charged %d tls_handshake_abandoned, want 2: no call came from the process", a)
		}
	})
	t.Run("a process whose only call was in plain text", func(t *testing.T) {
		p := trustProxy()
		serveFrom(t, p, 4242, sendsAPlainCall)
		if a, _ := serveFrom(t, p, 4242, hangsUpSilent); a != 1 {
			t.Fatalf("charged %d tls_handshake_abandoned, want 1: a call in plain text says nothing of keploy's CA", a)
		}
	})
	t.Run("an unknown process", func(t *testing.T) {
		p := trustProxy()
		serveFrom(t, p, 0, sendsACall)
		if a, _ := serveFrom(t, p, 0, hangsUpSilent); a != 1 {
			t.Fatalf("charged %d tls_handshake_abandoned, want 1: an unknown process is no process", a)
		}
	})
}

// Without the proven switch nothing is trusted: a process's handshake given
// up once keploy's certificate is in is charged after its call, as on main.
func TestWithoutTheSwitchAProcessCallMakesNoHangUpACancel(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	p := trustProxy()
	serveFrom(t, p, 4242, sendsACall)
	if a, _ := serveFrom(t, p, 4242, givesUpOnceTheCertificateIsIn); a != 1 {
		t.Fatalf("charged %d tls_handshake_abandoned, want 1", a)
	}
}
