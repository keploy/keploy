package proxy

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// mysqlGreeting is a real MySQL 8.0 Initial Handshake Packet (HandshakeV10):
// 3-byte length + seq 0 + 0x0a protocol version + "8.0.46\0" + salt/caps.
func mysqlGreeting() []byte {
	return []byte{
		0x4a, 0x00, 0x00, 0x00, 0x0a, '8', '.', '0', '.', '4', '6', 0x00,
		0x0a, 0x00, 0x00, 0x00, 0x38, 0x0a, 0x6e, 0x21, 0x50, 0x54, 0x57, 0x65,
		0x00, 0xff, 0xff, 0xff, 0x02, 0x00, 0xff, 0xdf, 0x15, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x59, 0x3a, 0x3e, 0x0f,
		0x69, 0x2b, 0x1e, 0x51, 0x77, 0x1a, 0x2f, 0x59, 0x00, 'c', 'a', 'c',
		'h', 'i', 'n', 'g', '_', 's', 'h', 'a', '2', '_', 'p', 'a', 's', 's',
		'w', 'o', 'r', 'd', 0x00,
	}
}

func testProxy(t *testing.T) *Proxy {
	t.Helper()
	return &Proxy{
		logger:     zap.NewNop(),
		mysqlPorts: newMysqlPortRegistry(),
		Integrations: map[integrations.IntegrationType]integrations.Integrations{
			integrations.MYSQL: mysql.New(zap.NewNop()),
		},
	}
}

// serverThatGreets starts a listener that writes greeting to every
// accepted connection, mimicking a server-speaks-first upstream.
func serverThatGreets(t *testing.T, greeting []byte) (addr string, port uint32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if len(greeting) > 0 {
				_, _ = c.Write(greeting)
			}
			// Hold the connection open so the probe's read does not
			// race a close.
			go func(c net.Conn) {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}(c)
		}
	}()

	tcpAddr := ln.Addr().(*net.TCPAddr)
	return ln.Addr().String(), uint32(tcpAddr.Port)
}

// clientPair returns the two ends of an in-memory-ish TCP connection.
// The returned "app" end is what a client app would hold; "srv" is what
// the proxy sees as srcConn.
func clientPair(t *testing.T) (app net.Conn, srv net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv, _ = ln.Accept()
	}()

	app, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	wg.Wait()
	if srv == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = app.Close(); _ = srv.Close() })
	return app, srv
}

// The headline case: a real MySQL server on a port nobody configured.
// Before auto-detection this deadlocked in ReadInitialBuf and the app
// died with "Lost connection ... reading initial communication packet".
func TestProbeDetectsMysqlOnUnconfiguredPort(t *testing.T) {
	p := testProxy(t)
	greeting := mysqlGreeting()
	dstAddr, port := serverThatGreets(t, greeting)

	_, srcConn := clientPair(t) // app end stays silent, like a MySQL client

	probe, err := p.probeMysql(context.Background(), srcConn, dstAddr, port,
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !probe.IsMySQL {
		t.Fatalf("expected MySQL detection, got reason=%q", probe.Reason)
	}
	if probe.Reason != "detected-handshake" {
		t.Errorf("reason = %q, want detected-handshake", probe.Reason)
	}

	// The greeting must survive the probe — the parser has to see the
	// handshake it consumed, byte for byte.
	if probe.DstConn == nil {
		t.Fatal("expected a pre-dialed upstream conn")
	}
	got := make([]byte, len(greeting))
	if _, err := io.ReadFull(probe.DstConn, got); err != nil {
		t.Fatalf("replay greeting: %v", err)
	}
	if string(got) != string(greeting) {
		t.Errorf("greeting was not replayed intact")
	}

	// And the port is learned, so the next connection skips the probe.
	if !p.mysqlPorts.Has(port) {
		t.Errorf("port %d was not learned", port)
	}
}

// Second connection to a learned port must take the fast path — no
// probe, no latency, no upstream dial.
func TestProbeFastPathAfterLearning(t *testing.T) {
	p := testProxy(t)
	p.mysqlPorts.Learn(7777)

	_, srcConn := clientPair(t)
	start := time.Now()
	probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:7777", 7777,
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !probe.IsMySQL || probe.Reason != "known-port" {
		t.Fatalf("IsMySQL=%v reason=%q, want true/known-port", probe.IsMySQL, probe.Reason)
	}
	if probe.DstConn != nil {
		t.Error("fast path must not dial upstream")
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("fast path took %v, expected it to be immediate", elapsed)
	}
}

// Configured / default ports keep their exact pre-existing behaviour.
func TestProbeConfiguredPortsUnchanged(t *testing.T) {
	p := testProxy(t)
	_, srcConn := clientPair(t)

	probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:3306", 3306,
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !probe.IsMySQL || probe.Reason != "configured-port" {
		t.Fatalf("IsMySQL=%v reason=%q, want true/configured-port", probe.IsMySQL, probe.Reason)
	}
}

// A client that speaks first is never MySQL, and its bytes must not be
// swallowed by the probe.
func TestProbeClientSpeaksFirstPreservesBytes(t *testing.T) {
	p := testProxy(t)
	app, srcConn := clientPair(t)

	payload := []byte("GET /health HTTP/1.1\r\nHost: x\r\n\r\n")
	if _, err := app.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:9999", 9999,
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL {
		t.Fatal("HTTP request classified as MySQL")
	}
	if probe.Reason != "client-spoke-first" {
		t.Errorf("reason = %q, want client-spoke-first", probe.Reason)
	}
	if probe.DstConn != nil {
		t.Error("must not dial upstream once the client has spoken")
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(probe.SrcConn, got); err != nil {
		t.Fatalf("replay client bytes: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("client bytes lost: got %q want %q", got, payload)
	}
}

// A silent client in front of a non-MySQL server must fall through to
// generic dispatch with the upstream conn and its bytes intact.
func TestProbeSilentClientNonMysqlServer(t *testing.T) {
	p := testProxy(t)
	banner := []byte("220 smtp.example.com ESMTP Postfix\r\n")
	dstAddr, port := serverThatGreets(t, banner)

	_, srcConn := clientPair(t)

	probe, err := p.probeMysql(context.Background(), srcConn, dstAddr, port,
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL {
		t.Fatal("SMTP banner classified as MySQL")
	}
	if probe.Reason != "greeting-not-mysql" {
		t.Errorf("reason = %q, want greeting-not-mysql", probe.Reason)
	}
	if p.mysqlPorts.Has(port) {
		t.Error("non-MySQL port must not be learned as MySQL")
	}
	// The probe connection must NOT be handed to the caller on a
	// negative verdict. Generic dispatch reassigns dstConn on its TLS /
	// CONNECT / Postgres-SSL branches without closing what was there, so
	// a handed-over socket would leak; and it dials just-in-time, so a
	// socket opened while waiting on a slow client can go stale before
	// the first byte is written. The upstream re-greets on the fresh
	// dial, so nothing is lost by dropping this one.
	if probe.DstConn != nil {
		t.Error("probe must not hand its upstream conn to the caller on a negative verdict")
	}
	// And the verdict is cached, so the next connection to this port
	// skips the probe entirely rather than re-paying the wait + dial.
	if !p.mysqlPorts.IsKnownNotMysql(port) {
		t.Error("negative verdict was not cached")
	}
}

// Regression test for the leak the negative-verdict path used to have:
// probeMysql dials upstream, and on a non-MySQL verdict that socket
// must be closed by the probe itself.
func TestProbeClosesUpstreamOnNegativeVerdict(t *testing.T) {
	p := testProxy(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	closed := make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("220 smtp.example.com ESMTP\r\n"))
		// Block until the peer closes; that read returns EOF exactly
		// when the probe has released its end.
		_, _ = io.Copy(io.Discard, c)
		closed <- struct{}{}
	}()

	tcpAddr := ln.Addr().(*net.TCPAddr)
	_, srcConn := clientPair(t)

	probe, err := p.probeMysql(context.Background(), srcConn, ln.Addr().String(),
		uint32(tcpAddr.Port), models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL {
		t.Fatal("SMTP classified as MySQL")
	}

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("probe leaked its upstream connection on a negative verdict")
	}
}

// Once a port is known not to speak MySQL, later connections must skip
// the probe entirely — no client wait, no upstream dial. Without this
// the probe is a per-connection tax on every non-MySQL port.
func TestProbeSkipsKnownNegativePort(t *testing.T) {
	p := testProxy(t)
	p.mysqlPorts.LearnNotMysql(8443)

	_, srcConn := clientPair(t)
	start := time.Now()
	probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:8443", 8443,
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL || probe.Reason != "known-not-mysql" {
		t.Fatalf("IsMySQL=%v reason=%q, want false/known-not-mysql", probe.IsMySQL, probe.Reason)
	}
	if probe.DstConn != nil {
		t.Error("cached negative must not dial upstream")
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("cached negative took %v; the probe was not skipped", elapsed)
	}
}

// A HandshakeV10 split across TCP segments must still be detected.
// MatchType needs the complete first packet, so a single Read on a
// fragmented greeting would return "not MySQL" and re-introduce the
// very deadlock this feature removes.
func TestProbeDetectsFragmentedGreeting(t *testing.T) {
	p := testProxy(t)
	greeting := mysqlGreeting()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		// Split mid-packet: the first write carries the 4-byte header
		// and a few body bytes, so MatchType cannot decide from it.
		_, _ = c.Write(greeting[:10])
		time.Sleep(120 * time.Millisecond)
		_, _ = c.Write(greeting[10:])
		_, _ = io.Copy(io.Discard, c)
	}()

	tcpAddr := ln.Addr().(*net.TCPAddr)
	_, srcConn := clientPair(t)

	probe, err := p.probeMysql(context.Background(), srcConn, ln.Addr().String(),
		uint32(tcpAddr.Port), models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !probe.IsMySQL {
		t.Fatalf("fragmented handshake not detected, reason=%q", probe.Reason)
	}

	// Every byte of the split greeting must still reach the parser.
	got := make([]byte, len(greeting))
	if _, err := io.ReadFull(probe.DstConn, got); err != nil {
		t.Fatalf("replay greeting: %v", err)
	}
	if string(got) != string(greeting) {
		t.Error("fragmented greeting was not reassembled intact")
	}
}

// A client that speaks first proves the port is not MySQL, so that
// verdict is cached too — otherwise every HTTP connection on a
// non-standard port keeps paying the client-silence window.
func TestProbeCachesClientSpokeFirstAsNegative(t *testing.T) {
	p := testProxy(t)
	app, srcConn := clientPair(t)
	if _, err := app.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:9443", 9443,
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL {
		t.Fatal("HTTP classified as MySQL")
	}
	if !p.mysqlPorts.IsKnownNotMysql(9443) {
		t.Error("client-spoke-first verdict was not cached")
	}
}

// An explicit positive must never be overridden by a cached negative —
// config and recorded mocks are stronger evidence than a probe timeout.
func TestRegistryPositiveBeatsNegative(t *testing.T) {
	r := newMysqlPortRegistry()

	r.LearnNotMysql(3307)
	if !r.IsKnownNotMysql(3307) {
		t.Fatal("negative not recorded")
	}

	// A real handshake later on the same port clears the negative.
	r.Learn(3307)
	if !r.Has(3307) {
		t.Error("positive not recorded")
	}
	if r.IsKnownNotMysql(3307) {
		t.Error("negative must be cleared by a positive")
	}

	// And a stale negative can no longer demote a known positive.
	r.LearnNotMysql(3307)
	if r.IsKnownNotMysql(3307) {
		t.Error("known positive was demoted by a negative")
	}
	if !r.Has(3307) {
		t.Error("known positive was lost")
	}
}

// Opting out restores the strict port-list behaviour exactly.
func TestProbeAutoDetectDisabled(t *testing.T) {
	p := testProxy(t)
	dstAddr, port := serverThatGreets(t, mysqlGreeting())
	_, srcConn := clientPair(t)

	probe, err := p.probeMysql(context.Background(), srcConn, dstAddr, port,
		models.MODE_RECORD, models.OutgoingOptions{DisableMysqlAutoDetect: true}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL {
		t.Fatal("auto-detection ran despite being disabled")
	}
	if probe.Reason != "auto-detect-disabled" {
		t.Errorf("reason = %q, want auto-detect-disabled", probe.Reason)
	}
}

// Replay has no upstream to greet anyone, so the port must come from
// the recorded mocks' destAddr.
func TestReplayRecallsPortFromMocks(t *testing.T) {
	p := testProxy(t)
	mocks := []*models.Mock{
		{Kind: models.MySQL, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "172.19.0.3:6033"}}},
		{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "1.2.3.4:80"}}},
	}
	p.deriveMysqlPorts(mocks)

	if !p.mysqlPorts.Has(6033) {
		t.Fatal("MySQL mock port 6033 not derived")
	}
	if p.mysqlPorts.Has(80) {
		t.Error("HTTP mock port must not be derived as MySQL")
	}

	_, srcConn := clientPair(t)
	probe, err := p.probeMysql(context.Background(), srcConn, "172.19.0.9:6033", 6033,
		models.MODE_TEST, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	// Note the host differs from the recorded one (container IPs change
	// between runs) — matching is by port for exactly this reason.
	if !probe.IsMySQL || probe.Reason != "known-port" {
		t.Fatalf("IsMySQL=%v reason=%q, want true/known-port", probe.IsMySQL, probe.Reason)
	}
}

// Replay must never dial upstream while probing — there is nothing to
// dial, and doing so would leak a connection to a real server.
func TestReplayNeverDials(t *testing.T) {
	p := testProxy(t)
	p.deriveMysqlPorts(nil) // signal derivation with no MySQL mocks

	_, srcConn := clientPair(t)
	probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:1", 1,
		models.MODE_TEST, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.DstConn != nil {
		t.Fatal("replay probe dialed upstream")
	}
	if probe.Reason != "no-mysql-mock-for-port" {
		t.Errorf("reason = %q, want no-mysql-mock-for-port", probe.Reason)
	}
}

// The compose replay path starts the app before mocks are stored. A
// connection arriving in that gap must park until derivation lands
// rather than guess wrong.
func TestReplayWaitsForMockDerivation(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_MOCK_DERIVE_WAIT", "5s")
	p := testProxy(t)

	go func() {
		time.Sleep(200 * time.Millisecond)
		p.deriveMysqlPorts([]*models.Mock{
			{Kind: models.MySQL, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "db:15306"}}},
		})
	}()

	_, srcConn := clientPair(t)
	start := time.Now()
	probe, err := p.probeMysql(context.Background(), srcConn, "db:15306", 15306,
		models.MODE_TEST, models.OutgoingOptions{}, zap.NewNop())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !probe.IsMySQL {
		t.Fatalf("expected MySQL after derivation landed, reason=%q", probe.Reason)
	}
	if probe.Reason != "derived-from-mocks" {
		t.Errorf("reason = %q, want derived-from-mocks", probe.Reason)
	}
	if elapsed > 3*time.Second {
		t.Errorf("waited %v, expected to wake as soon as mocks landed", elapsed)
	}
}

// The wait is bounded — a run with no mocks at all must not hang.
func TestReplayDeriveWaitIsBounded(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_MOCK_DERIVE_WAIT", "300ms")
	p := testProxy(t)

	_, srcConn := clientPair(t)
	start := time.Now()
	probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:9", 9,
		models.MODE_TEST, models.OutgoingOptions{}, zap.NewNop())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL {
		t.Fatal("classified as MySQL with no mocks at all")
	}
	if probe.Reason != "mocks-not-derived" {
		t.Errorf("reason = %q, want mocks-not-derived", probe.Reason)
	}
	if elapsed > 2*time.Second {
		t.Errorf("wait was not bounded: %v", elapsed)
	}
}

func TestPortFromAddr(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
		ok   bool
	}{
		{"127.0.0.1:3306", 3306, true},
		{"db:6033", 6033, true},
		{"[::1]:4000", 4000, true},
		{"[fd00::2]:15306", 15306, true},
		{"", 0, false},
		{"127.0.0.1", 0, false},
		{"127.0.0.1:0", 0, false},
		{"127.0.0.1:notaport", 0, false},
	}
	for _, c := range cases {
		got, ok := portFromAddr(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("portFromAddr(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := newMysqlPortRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Learn(uint32(3000 + i%8))
			r.Has(uint32(3000 + i%8))
			r.Ports()
			r.DeriveFromMocks([]*models.Mock{
				{Kind: models.MySQL, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "h:3306"}}},
			})
		}(i)
	}
	wg.Wait()
	if !r.Has(3306) {
		t.Error("expected 3306 after concurrent derivation")
	}
}
