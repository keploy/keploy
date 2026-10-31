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
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
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

// readCounting is a client's connection that counts what it has read: none
// of keploy's server flight while it is 0.
type readCounting struct {
	net.Conn
	n *atomic.Int64
}

func (c readCounting) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.n.Add(int64(n))
	return n, err
}

// gateSpy is a build's TLS gate (OnClientTLS) that notes each process it was
// asked to make ready, with what the client had read of keploy's answer by
// then, and gives why for any failed handshake.
type gateSpy struct {
	read  *atomic.Int64
	mu    sync.Mutex
	ready []Process
	seen  []int64
	real  [][]string
	why   string
}

func (g *gateSpy) Ready(_ context.Context, p Process, realOrPassthrough []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ready = append(g.ready, p)
	g.seen = append(g.seen, g.read.Load())
	g.real = append(g.real, realOrPassthrough)
}

func (g *gateSpy) Why(Process) string { return g.why }

// keploy's proxy consults a build's TLS gate (OnClientTLS) on each handshake
// it makes with a process it knows: Ready before it answers the client, so
// what the gate does (keploy's TLS pin bypass) is in place before the client
// checks keploy's certificate; and Why in the capture loss a failed handshake
// is charged as, so the test's reason names the process and the cause. The
// gate is told the process by its kernel PID and by its PID in the agent's
// own namespace (0 outside it), so an agent in a container's PID namespace
// can read it in its /proc. A connection whose process the proxy does not
// know consults neither.
func TestAClientTLSGateIsReadyBeforeKeployAnswersAndSaysWhyAHandshakeFailed(t *testing.T) {
	const why = "process 4242 (probe): keploy's TLS pin bypass could not attach to it: operation not permitted"
	for _, tc := range []struct {
		name  string
		pid   uint32
		nsPid uint32
		ready []Process
		what  string
	}{
		{"a process keploy knows", 4242, 4242, []Process{{Pid: 4242, KernelPid: 4242}}, "to 127.0.0.1:8443: " + why},
		{"a process in the agent's container's PID namespace", 4242, 17, []Process{{Pid: 17, KernelPid: 4242}}, "to 127.0.0.1:8443: " + why},
		{"a process outside the agent's PID namespace", 4242, 0, []Process{{KernelPid: 4242}}, "to 127.0.0.1:8443: " + why},
		{"a process keploy does not know", 0, 0, nil, "to 127.0.0.1:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := &atomic.Int64{}
			g := &gateSpy{read: read, why: why}
			prev := OnClientTLS
			OnClientTLS = g
			t.Cleanup(func() { OnClientTLS = prev })
			dest := ipv4Dest(t, "127.0.0.1:8443")
			dest.KernelPid = tc.pid
			dest.AgentNsPid = tc.nsPid
			app, src := acceptedConn(t)
			p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: dest}, mysqlPorts: newMysqlPortRegistry()}
			p.BeginTestErrorCapture()
			p.setSession(&agent.Session{
				Mode:            models.MODE_TEST,
				OutgoingOptions: models.OutgoingOptions{Mocking: true, DisableMysqlAutoDetect: true},
			})
			done := make(chan struct{})
			go func() {
				defer close(done)
				// A client that trusts only its own roots rejects keploy's
				// certificate: a bad_certificate alert.
				_ = tls.Client(readCounting{Conn: app, n: read}, &tls.Config{ServerName: "dep.example"}).Handshake()
				_ = app.Close()
			}()
			if err := p.handleConnection(context.Background(), src); err == nil {
				t.Fatal("handleConnection returned no error for a failed handshake")
			}
			<-done
			g.mu.Lock()
			ready, seen, real := g.ready, g.seen, g.real
			g.mu.Unlock()
			if fmt.Sprint(ready) != fmt.Sprint(tc.ready) {
				t.Fatalf("the gate was asked to make ready %v, want %v", ready, tc.ready)
			}
			for i, n := range seen {
				if n != 0 {
					t.Fatalf("the gate's Ready for process %d came after the client had read %d byte(s) of keploy's answer, want before it", ready[i], n)
				}
			}
			// This run reaches only keploy's mocks, so the gate is free to
			// ready the process: it is told no real destination.
			for i, r := range real {
				if len(r) != 0 {
					t.Fatalf("the gate's Ready for process %d was told real destinations %v, want none", ready[i], r)
				}
			}
			l := p.losses.Snapshot()
			if len(l) != 1 || l[0].Kind != models.CaptureLossTLSAbort || l[0].What != tc.what {
				t.Fatalf("charged %+v, want one tls_handshake_failed %q", l, tc.what)
			}
		})
	}
}

// blockingGate is a build's TLS gate whose Ready holds until release is
// closed, closing entered as it begins.
type blockingGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *blockingGate) Ready(context.Context, Process, []string) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
}

func (g *blockingGate) Why(Process) string { return "" }

// keploy's proxy answers a client's handshake only once the gate's Ready has
// returned: the pin bypass it readies must be in place before the client
// checks keploy's certificate. A proxy that ran Ready beside the handshake
// (on a goroutine of its own) would race the attach, and its first answer
// would reach the client while Ready still held.
//
// An absence is seen only over a window: such a proxy's server flight is on
// the client's socket within microseconds of the hello it reads; a loaded box
// may make the window miss it, and never fails a proxy that waits.
func TestTheProxyAnswersAClientOnlyOnceTheGatesReadyReturned(t *testing.T) {
	g := &blockingGate{entered: make(chan struct{}), release: make(chan struct{})}
	prev := OnClientTLS
	OnClientTLS = g
	t.Cleanup(func() { OnClientTLS = prev })
	released := false
	t.Cleanup(func() {
		if !released {
			close(g.release)
		}
	})
	p := replayProxyTo(t, "127.0.0.1:8443", models.OutgoingOptions{})
	dest := ipv4Dest(t, "127.0.0.1:8443")
	dest.KernelPid = 4242
	p.DestInfo = fakeDestInfo{dest: dest}
	p.BeginTestErrorCapture()
	app, src := acceptedConn(t)
	read := &atomic.Int64{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// It rejects keploy's certificate (its own roots only), once it
		// gets it.
		_ = tls.Client(readCounting{Conn: app, n: read}, &tls.Config{ServerName: "dep.example"}).Handshake()
		_ = app.Close()
	}()
	hc := make(chan error, 1)
	go func() { hc <- p.handleConnection(context.Background(), src) }()
	awaitEvent(t, g.entered, "the gate's Ready for the client's handshake")
	time.Sleep(time.Second)
	if n := read.Load(); n != 0 {
		t.Fatalf("the client read %d byte(s) of keploy's answer while the gate's Ready still held, want none before it returns", n)
	}
	released = true
	close(g.release)
	awaitEvent(t, done, "the client's handshake ending once Ready returned")
	if read.Load() == 0 {
		t.Fatal("the client read nothing of keploy's answer once Ready returned")
	}
	select {
	case <-hc:
	case <-time.After(30 * time.Second):
		t.Fatal("handleConnection did not return after the client left")
	}
}

// pgSSLReplayer is a replayer that makes the client's TLS handshake itself,
// in band, as integrations' Postgres v3 replayer does on an SSLRequest: it
// answers 'S' and calls pTls.HandleTLSConnection with the context
// MockOutgoing was given.
type pgSSLReplayer struct{ integrations.Integrations }

func (pgSSLReplayer) MatchType(_ context.Context, b []byte) bool {
	return isPostgresSSLRequestPrefix(b)
}

func (pgSSLReplayer) MockOutgoing(ctx context.Context, src net.Conn, _ *models.ConditionalDstCfg, _ integrations.MockMemDb, _ models.OutgoingOptions) error {
	req := make([]byte, 8)
	if _, err := io.ReadFull(src, req); err != nil {
		return err
	}
	if _, err := src.Write([]byte{'S'}); err != nil {
		return err
	}
	_, _, err := pTls.HandleTLSConnection(ctx, zap.NewNop(), src, time.Time{})
	return err
}

// A replayer's own in-band TLS handshake (Postgres's, on an SSLRequest) is
// one keploy's proxy makes as the client's server like any other: the gate
// readies the client's process before it, and a failed one is charged as a
// capture loss that says why, not left as the client's opaque TLS error.
func TestAReplayersInBandTLSHandshakeIsReadiedAndChargedAsAnyOther(t *testing.T) {
	const why = "process 4242 (app): keploy's TLS pin bypass could not attach to it: operation not permitted"
	const dst = "127.0.0.1:5432"
	read := &atomic.Int64{}
	g := &gateSpy{read: read, why: why}
	prev := OnClientTLS
	OnClientTLS = g
	t.Cleanup(func() { OnClientTLS = prev })
	p := replayProxyTo(t, dst, models.OutgoingOptions{})
	dest := ipv4Dest(t, dst)
	dest.KernelPid = 4242
	dest.AgentNsPid = 17
	p.DestInfo = fakeDestInfo{dest: dest}
	p.BeginTestErrorCapture()
	p.errChannel = make(chan error, 8)
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.POSTGRES_V3: pgSSLReplayer{}}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.POSTGRES_V3}}
	app, src := acceptedConn(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer app.Close()
		if _, err := app.Write([]byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}); err != nil {
			t.Errorf("SSLRequest: %v", err)
			return
		}
		s := make([]byte, 1)
		if _, err := io.ReadFull(app, s); err != nil || s[0] != 'S' {
			t.Errorf("the replayer answered the SSLRequest %q (%v), want 'S'", s, err)
			return
		}
		// A client that pins its own roots rejects keploy's certificate.
		_ = tls.Client(readCounting{Conn: app, n: read}, &tls.Config{ServerName: "db.example"}).Handshake()
	}()
	hc := make(chan error, 1)
	go func() { hc <- p.handleConnection(context.Background(), src) }()
	awaitEvent(t, done, "the client's in-band handshake ending")
	select {
	case <-hc:
	case <-time.After(30 * time.Second):
		t.Fatal("handleConnection did not return after the client left")
	}
	g.mu.Lock()
	ready, seen := g.ready, g.seen
	g.mu.Unlock()
	if want := fmt.Sprint([]Process{{Pid: 17, KernelPid: 4242}}); fmt.Sprint(ready) != want {
		t.Fatalf("the gate was asked to make ready %v for a replayer's in-band handshake, want %s (the process by its PID in the agent's namespace too)", ready, want)
	}
	if seen[0] != 0 {
		t.Fatalf("the gate's Ready came after the client had read %d byte(s) of keploy's TLS answer, want before it", seen[0])
	}
	l := p.losses.Snapshot()
	if want := "to " + dst + ": " + why; len(l) != 1 || l[0].Kind != models.CaptureLossTLSAbort || l[0].What != want {
		t.Fatalf("charged %+v, want one tls_handshake_failed %q", l, want)
	}
}

// Under the proven switch, the only mode the native gate runs in, a client
// that hangs up silent on keploy's certificate (Node.js, curl, libcurl:
// caTrust) is held, and charged at the next read of the counts. That charge
// says why, as a failed handshake's does: the gate's why for the process,
// read as it hung up.
func TestAHeldHangUpIsChargedWithTheGatesWhy(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const why = "process 4242 (node): not a Go program, so keploy's TLS pin bypass cannot ready it"
	g := &gateSpy{read: &atomic.Int64{}, why: why}
	prev := OnClientTLS
	OnClientTLS = g
	t.Cleanup(func() { OnClientTLS = prev })
	p := &Proxy{logger: zap.NewNop()}
	c := &clientTLS{p: p, ctx: context.Background(), dst: "10.0.0.1:443", tgid: 4242}
	c.HandshakeBegin()
	c.HandshakeFailed(&pTls.HandshakeError{Err: io.EOF, CertificateSent: true})
	if l := p.losses.Snapshot(); len(l) != 0 {
		t.Fatalf("charged %+v at the hang-up, want it held until the counts are read", l)
	}
	l := p.CaptureLosses()
	if want := "to 10.0.0.1:443: " + why; len(l) != 1 || l[0].Kind != models.CaptureLossTLSAbandoned || l[0].What != want {
		t.Fatalf("charged %+v, want one held tls hang-up %q", l, want)
	}
}

// The labels a client TLS gate reads to refuse turning a process's
// certificate checks off while it still talks to a real server: a marker for
// global pass-through; else the run's mock.real list, the bypassRules of the
// process's session and a replay session's options that relay calls live; and
// none when the run reaches only keploy's mocks (realOrPassthroughLabels).
func TestRealOrPassthroughLabelsTellsAGateWhatTheRunReachesLive(t *testing.T) {
	replay := func(o models.OutgoingOptions) *agent.Session {
		return &agent.Session{Mode: models.MODE_TEST, OutgoingOptions: o}
	}
	for _, tc := range []struct {
		name    string
		global  bool
		real    []string
		session *agent.Session
		want    []string
	}{
		{name: "only keploy's mocks, no session yet"},
		{name: "only keploy's mocks", session: replay(models.OutgoingOptions{Mocking: true})},
		{name: "global pass-through", global: true, session: replay(models.OutgoingOptions{Mocking: true, Rules: []models.BypassRule{{Port: 443}}}),
			want: []string{"every destination (global pass-through)"}},
		{name: "mock.real", real: []string{"localhost:5432"}, session: replay(models.OutgoingOptions{Mocking: true}),
			want: []string{"localhost:5432"}},
		{name: "a bypassRule", session: replay(models.OutgoingOptions{Mocking: true, Rules: []models.BypassRule{{Host: "api.example.com", Port: 443}}}),
			want: []string{`bypassRule {host: "api.example.com", path: "", port: 443}`}},
		{name: "a replay with mocking off", session: replay(models.OutgoingOptions{}),
			want: []string{"test.mocking: false (--mocking=false): " + allDestinations}},
		{name: "a replay that passes a miss through", session: replay(models.OutgoingOptions{Mocking: true, OnMiss: models.MissPassthrough}),
			want: []string{"onMiss passthrough (--on-miss passthrough): an HTTP call no mock answers goes to its real destination"}},
		{name: "a record session's options are no replay's", session: &agent.Session{Mode: models.MODE_RECORD}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop(), GlobalPassthrough: tc.global}
			if tc.real != nil {
				t.Setenv(provenswitch.Env, "1")
				if err := p.SetRealDeps(context.Background(), tc.real); err != nil {
					t.Fatal(err)
				}
			}
			if tc.session != nil {
				p.setSession(tc.session)
			}
			if got := p.realOrPassthroughLabels(4242); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("the gate was told %q, want %q", got, tc.want)
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
	_, _, err := p.clientTLSHandler("10.0.0.5:3306", Process{})(context.Background(), zap.NewNop(), src, time.Time{})
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
	conn, _, err := p.clientTLSHandler("10.0.0.5:5432", Process{})(context.Background(), zap.NewNop(), src, time.Time{})
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
			upgrade := newProxyTLSUpgradeFn(zap.NewNop(), false, nil)
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
			ctx, cancel := context.WithCancel(p.withClientTLS(context.Background(), remoteOf(dst), Process{}))
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

// A client keeps a connection it set up for a call another connection served
// first: a spare, its TLS handshake done, nothing sent (Go's http.Transport
// keeps the one it dialled for a request an idle connection served before the
// dial finished). It goes when the application stops, closed by the kernel
// with no close_notify -- after the test it was set up in. A hang-up like a
// rejection's, but nothing was lost: it is charged only within the scope its
// handshake began in, and this one crossed the test's end. It was charged a
// tls_handshake_abandoned, leaving a test not judged for every Go application
// that leaves a spare.
//
// The test sets the spare up itself, beside a call made on a connection of
// its own: left to the Transport's racing a second dial against the first
// connection's return, a loaded runner sometimes had the first connection
// serve both calls, and no spare was ever dialled.
func TestASpareIdleConnectionClosedOutsideItsTestIsNoLoss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	dial := func() (net.Conn, error) {
		c, err := net.Dial("tcp4", ln.Addr().String())
		if err == nil {
			mu.Lock()
			raw = append(raw, c)
			mu.Unlock()
		}
		return c, err
	}
	// The proxy dials the name the client sent (SNI).
	clientTLS := &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"}

	ck := containerkey.Default
	ck.Reset()
	t.Cleanup(ck.Reset)
	ck.BeginTest() // the test the application's calls are made in
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: clientTLS,
		DialContext:     func(context.Context, string, string) (net.Conn, error) { return dial() },
	}}
	res, err := client.Get("https://localhost/call")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	// The spare: set up in the test, then left idle.
	rawSpare, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	spare := tls.Client(rawSpare, clientTLS)
	if err := spare.HandshakeContext(context.Background()); err != nil {
		t.Fatalf("the spare's handshake: %v", err)
	}
	ck.EndTest()

	// The application stops: the kernel closes its sockets, no close_notify.
	mu.Lock()
	for _, c := range raw {
		_ = c.Close()
	}
	n := len(raw)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("%d connections dialled, want the call's and the spare", n)
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
