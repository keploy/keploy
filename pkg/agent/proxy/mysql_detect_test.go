package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
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
		// No address of the test's machine, nor its gateway, decides a verdict.
		readNetns:      func() (own, gateways []netip.Addr) { return nil, nil },
		dockerHostAddr: func(netip.Addr) bool { return false },
		// Nor does this machine's /etc/hosts.
		readDockerHostNames: func() map[netip.Addr]string { return nil },
		// Built the way proxy.New does, so tests can drive SetMocksWithWindow —
		// the call a stock agent actually makes to publish a test set's mocks.
		dnsCache: newDNSCache(),
		// Preseeded so Mock() skips setupNsswitchConfig, which rewrites
		// /etc/nsswitch.conf and only restores it on proxy shutdown — a unit
		// test never shuts the proxy down, so as root this would permanently
		// replace the host's DNS configuration, and as non-root it would fail
		// the write and abort Mock() early. Either way the test's verdict would
		// depend on privileges rather than on the code under test.
		nsswitchData: []byte("preseeded-by-test"),
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

	probe, err := p.probeMysql(context.Background(), srcConn, dstAddr, nil, port,
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
	probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:7777", nil, 7777,
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

	probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:3306", nil, 3306,
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

	// A client-speaks-first destination the test controls, silent until asked.
	dstAddr, _, _ := acceptingServer(t, silentUntilAsked)
	probe, err := p.probeMysql(context.Background(), srcConn, dstAddr, nil, uint32(mustPort(t, dstAddr)),
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL {
		t.Fatal("HTTP request classified as MySQL")
	}
	defer probe.Predialed.CloseIfUnused()
	if probe.Reason != "client-spoke-first" {
		t.Errorf("reason = %q, want client-spoke-first", probe.Reason)
	}
	if probe.DstConn != nil {
		t.Error("a negative verdict must not hand its upstream over as DstConn")
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

	probe, err := p.probeMysql(context.Background(), srcConn, dstAddr, nil, port,
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
	// The probe's connection is not DstConn on a negative verdict:
	// generic dispatch reassigns dstConn on its TLS / CONNECT /
	// Postgres-SSL branches without closing what was there, so a socket
	// handed over that way would leak. It is handed on as the
	// connection's pre-dialled one instead, which every one of those
	// branches dials through, banner first.
	if probe.DstConn != nil {
		t.Error("probe must not hand its upstream conn to the caller as DstConn on a negative verdict")
	}
	if probe.Predialed == nil {
		t.Fatal("the probe dropped its upstream; the connection's dial would open a second one")
	}
	next, err := util.DialRaw(context.Background(), nil, "tcp", dstAddr, probe.Predialed)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	got := make([]byte, len(banner))
	if _, err := io.ReadFull(next, got); err != nil || string(got) != string(banner) {
		t.Fatalf("the next dial read %q, %v; want the banner the probe already read", got, err)
	}
	// And the verdict is cached, so the next connection to this port
	// skips the probe entirely rather than re-paying the wait + dial.
	if !p.mysqlPorts.IsKnownNotMysql(port) {
		t.Error("negative verdict was not cached")
	}
}

// Regression test for the leak the negative-verdict path used to have:
// probeMysql dials upstream, and on a non-MySQL verdict that socket must
// not be left unreferenced. It is handed on (mysqlProbe.Predialed), and
// closed when no dial takes it (CloseIfUnused, which handleConnection
// defers).
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

	probe, err := p.probeMysql(context.Background(), srcConn, ln.Addr().String(), nil,
		uint32(tcpAddr.Port), models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL {
		t.Fatal("SMTP classified as MySQL")
	}
	if probe.Predialed == nil {
		t.Fatal("the probe's upstream was neither kept nor handed on")
	}
	probe.Predialed.CloseIfUnused()

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
	probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:8443", nil, 8443,
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

	probe, err := p.probeMysql(context.Background(), srcConn, ln.Addr().String(), nil,
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

	dstAddr, _, _ := acceptingServer(t, silentUntilAsked)
	port := uint32(mustPort(t, dstAddr))
	probe, err := p.probeMysql(context.Background(), srcConn, dstAddr, nil, port,
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer probe.Predialed.CloseIfUnused()
	if probe.IsMySQL {
		t.Fatal("HTTP classified as MySQL")
	}
	if !p.mysqlPorts.IsKnownNotMysql(port) {
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

	probe, err := p.probeMysql(context.Background(), srcConn, dstAddr, nil, port,
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
	probe, err := p.probeMysql(context.Background(), srcConn, "172.19.0.9:6033", nil, 6033,
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
	probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:1", nil, 1,
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
	p := testProxy(t)

	go func() {
		time.Sleep(200 * time.Millisecond)
		p.deriveMysqlPorts([]*models.Mock{
			{Kind: models.MySQL, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "db:15306"}}},
		})
	}()

	_, srcConn := clientPair(t)
	start := time.Now()
	probe, err := p.probeMysql(context.Background(), srcConn, "db:15306", nil, 15306,
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

// The inference is gated on the RECORDING holding MySQL mocks. The registry
// also accumulates ports learned by probing during a record session, and the
// agent is one long-lived process that does not reset it between sessions — so
// gating on "any known MySQL port" would arm the inference in a replay whose
// recording contains no MySQL at all, and route a silent connection to a
// replayer with nothing to serve.
func TestReplayDoesNotInventMysqlWithoutMocks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(p *Proxy)
	}{
		{
			name:    "no mocks at all",
			arrange: func(p *Proxy) { p.deriveMysqlPorts(nil) },
		},
		{
			name: "a port learned while recording, then a replay with no MySQL mocks",
			arrange: func(p *Proxy) {
				p.mysqlPorts.Learn(3307) // as a record session would
				p.deriveMysqlPorts([]*models.Mock{
					{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:8080"}}},
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProxy(t)
			tc.arrange(p)

			_, srcConn := clientPair(t)
			probe, err := p.probeMysql(context.Background(), srcConn, "172.20.0.2:283", nil, 283,
				models.MODE_TEST, models.OutgoingOptions{}, zap.NewNop())
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if probe.IsMySQL {
				t.Fatalf("IsMySQL=true reason=%q: with no MySQL mocks in the recording there is nothing to serve", probe.Reason)
			}
		})
	}
}

// The escape hatch has to work without also turning off record-time detection,
// which is what disableMysqlAutoDetect would cost.
func TestEndpointDriftCanBeDisabled(t *testing.T) {
	p := testProxy(t)
	p.deriveMysqlPorts([]*models.Mock{
		{Kind: models.MySQL, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:3306"}}},
	})

	_, srcConn := clientPair(t)
	probe, err := p.probeMysql(context.Background(), srcConn, "172.20.0.2:283", nil, 283,
		models.MODE_TEST, models.OutgoingOptions{DisableMysqlEndpointDrift: true}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL {
		t.Fatalf("IsMySQL=true reason=%q: the inference was disabled", probe.Reason)
	}
	if probe.Reason != "no-mysql-mock-for-port" {
		t.Errorf("reason = %q, want no-mysql-mock-for-port", probe.Reason)
	}
}

// A replay session must be judged on its OWN recording. The agent is one
// long-lived process and the registry is allocated once, so without a per
// session reset the drift gate would mean "any recording this process has ever
// replayed": a test set holding MySQL mocks would arm the inference for a later
// test set holding none, and a silent connection there would be routed to a
// replayer with nothing to serve.
//
// Session 2 publishes its mocks through SetMocksWithWindow because that is the
// call a stock agent makes — Agent.UpdateMockParams takes the WindowedProxy arm
// and *Proxy implements it, so the SetMocks arm exists only for a third-party
// proxy that does not. An earlier version of this test drove SetMocks and was
// green while the production path leaked; TestProxyTakesTheWindowedMocksPath
// below keeps that mistake from being made again.
func TestDriftGateIsScopedToTheCurrentRecording(t *testing.T) {
	p := testProxy(t)

	// Session 1: a recording that does contain MySQL.
	p.deriveMysqlPorts([]*models.Mock{
		{Kind: models.MySQL, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:3306"}}},
	})
	if len(p.mysqlPorts.MockPorts()) != 1 {
		t.Fatalf("session 1 should have derived one port, got %v", p.mysqlPorts.MockPorts())
	}

	// Session 2 begins. Going through Mock() rather than calling the registry
	// directly is the point: it pins that the per-test-set entry point the
	// replayer actually reaches is the one wired up.
	if err := p.Mock(context.Background(), models.OutgoingOptions{}); err != nil {
		t.Fatalf("Mock: %v", err)
	}
	// The previous recording's ports are still usable here, on purpose: the
	// swap waits for the replacement so a pool reconnecting during the gap
	// between Mock() and the first mock delivery keeps its fast path.
	if got := p.mysqlPorts.MockPorts(); len(got) != 1 {
		t.Fatalf("ports were dropped before the replacement arrived, leaving a blind window: %v", got)
	}
	// Now session 2's recording arrives, and it contains no MySQL at all.
	if err := p.SetMocksWithWindow(context.Background(),
		[]*models.Mock{{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:8080"}}}},
		nil, time.Time{}, time.Time{}); err != nil {
		t.Fatalf("SetMocksWithWindow: %v", err)
	}
	if got := p.mysqlPorts.MockPorts(); len(got) != 0 {
		t.Fatalf("session 1's mock-derived ports survived into session 2: %v", got)
	}

	_, srcConn := clientPair(t)
	probe, err := p.probeMysql(context.Background(), srcConn, "10.0.0.9:5671", nil, 5671,
		models.MODE_TEST, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL {
		t.Fatalf("IsMySQL=true reason=%q: this recording has no MySQL mocks to serve", probe.Reason)
	}
}

// The reset above is only worth anything on the code path a real agent takes.
// Agent.UpdateMockParams publishes mocks through SetMocksWithWindow when the
// proxy implements WindowedProxy and through SetMocks when it does not, so if
// this proxy ever stopped implementing it, every per-test-set guarantee in this
// file would quietly move to a branch nothing exercises.
func TestProxyTakesTheWindowedMocksPath(t *testing.T) {
	var p interface{} = &Proxy{}
	if _, ok := p.(agent.WindowedProxy); !ok {
		t.Fatal("*Proxy no longer implements WindowedProxy: a stock agent would fall back to SetMocks, " +
			"and any per-test-set reset wired into Mock() would need revisiting")
	}
}

// Deferring the swap to derive time is what keeps derivedCh close-once. If a
// future change re-armed it at the boundary instead, a connection already
// waiting on derivedSignal would be left on the orphaned channel: the derive
// closes the replacement, and the waiter is never released by the mocks it
// waits for — the connection gets no MySQL and the test reports status_code 0,
// which is the failure this mechanism exists to prevent.
func TestSessionSwapDoesNotStrandDerivedWaiters(t *testing.T) {
	p := testProxy(t)

	parked := make(chan struct{})
	signal := p.mysqlPorts.derivedSignal() // what a connection arriving now waits on
	go func() {
		<-signal
		close(parked)
	}()

	p.mysqlPorts.MarkSessionStale()
	p.deriveMysqlPorts([]*models.Mock{
		{Kind: models.MySQL, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:3306"}}},
	})

	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter stranded across the session boundary: it is still parked on a channel nothing will close")
	}
}

// predialProbeServer accepts upstream connections, counting them, and does
// with each what serve says.
func predialProbeServer(t *testing.T, serve func(c net.Conn)) (addr string, conns *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			go serve(c)
		}
	}()
	return ln.Addr().String(), &n
}

// TestProbeHandsTheApplicationsOwnUpstreamBack: when the probe takes the
// connection dialled while the application's handshake was held, it is the
// application's own connection, not a probe's. A negative verdict hands it
// back — with any greeting it read — for generic dispatch's dial, so the
// destination sees one connection; and a destination that closed it while
// the application was still silent closes the application's end too.
func TestProbeHandsTheApplicationsOwnUpstreamBack(t *testing.T) {
	type outcome struct {
		greeting     string // what the next dial reads first
		appEndClosed bool
	}
	for _, tc := range []struct {
		name      string
		serve     func(c net.Conn)
		clientSay string // what the application sends first, if anything
		want      outcome
	}{
		// Both ends silent decide nothing: the client speaking does.
		{"silent upstream, the client speaks", func(c net.Conn) {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}, "GET / HTTP/1.1\r\n\r\n", outcome{}},
		{"a greeting that is not MySQL", func(c net.Conn) {
			defer c.Close()
			_, _ = c.Write([]byte("220 smtp.example.com ESMTP\r\n"))
			_, _ = io.Copy(io.Discard, c)
		}, "", outcome{greeting: "220 smtp.example.com ESMTP\r\n"}},
		{"upstream closes first", func(c net.Conn) {
			_ = c.Close()
		}, "", outcome{appEndClosed: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, conns := predialProbeServer(t, tc.serve)
			p := testProxy(t)
			upstream, err := util.DialUpstream(context.Background(), nil, "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			pre := util.NewPredialed(addr, upstream)
			defer pre.CloseIfUnused()
			app, srcConn := clientPair(t)
			defer app.Close()
			// As handleConnection arms it.
			pre.WatchUntilTaken(func() {
				if util.ReceivedNothing(srcConn) {
					_ = srcConn.Close()
				}
			})
			port := uint32(upstream.RemoteAddr().(*net.TCPAddr).Port)
			if tc.clientSay != "" {
				if _, err := io.WriteString(app, tc.clientSay); err != nil {
					t.Fatal(err)
				}
			}

			probe, err := p.probeMysql(context.Background(), srcConn, addr, pre, port,
				models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if probe.IsMySQL {
				t.Fatal("classified as MySQL")
			}

			if tc.want.appEndClosed {
				_ = app.SetReadDeadline(time.Now().Add(2 * time.Second))
				if _, err := app.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
					t.Fatalf("read %v; the destination closed the application's connection while it was silent, and its end must close too", err)
				}
				return
			}

			next, err := util.DialRaw(context.Background(), nil, "tcp", addr, pre)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if tc.want.greeting != "" {
				_ = next.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, len(tc.want.greeting))
				if _, err := io.ReadFull(next, buf); err != nil || string(buf) != tc.want.greeting {
					t.Fatalf("the next dial read %q, %v; want the greeting the probe already read", buf, err)
				}
			}
			time.Sleep(50 * time.Millisecond)
			if n := conns.Load(); n != 1 {
				t.Fatalf("the destination saw %d connections for the application's one", n)
			}
		})
	}
}
