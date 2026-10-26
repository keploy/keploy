package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A built-in default MySQL port (3306, 4000) or a learned one is a hint, not a
// verdict: these tests pin what each end of a connection to one decides.

type probeResult struct {
	probe *mysqlProbe
	err   error
}

// probeAsync runs probeMysql on a goroutine and hands back its verdict.
func probeAsync(p *Proxy, src net.Conn, dstAddr string, port uint32, mode models.Mode, opts models.OutgoingOptions) <-chan probeResult {
	done := make(chan probeResult, 1)
	go func() {
		probe, err := p.probeMysql(context.Background(), src, dstAddr, nil, port, mode, opts, zap.NewNop())
		done <- probeResult{probe, err}
	}()
	return done
}

func requireUndecided(t *testing.T, done <-chan probeResult, while string) {
	t.Helper()
	select {
	case r := <-done:
		t.Fatalf("the probe decided (IsMySQL=%v, reason %q, err %v) %s", r.probe.IsMySQL, r.probe.Reason, r.err, while)
	default:
	}
}

// An HTTP client on 3306 or 4000 speaks first; a MySQL client never does. The
// port used to decide alone, so the connection went to the MySQL path, which
// waited for a greeting the HTTP service never sends: the request was never
// relayed (an HTTP service on :4000 got no answer under record).
func TestRecordAClientThatSpeaksFirstOnADefaultMysqlPortIsNotMysql(t *testing.T) {
	for _, port := range []uint32{3306, 4000} {
		t.Run("port"+itoa(port), func(t *testing.T) {
			addr, conns, accepted := acceptingServer(t, silentUntilAsked)
			p := testProxy(t)
			app, srcConn := clientPair(t)

			// The destination is dialled at addr; the port the probe judges
			// is the default one the application dialled.
			done := probeAsync(p, srcConn, addr, port, models.MODE_RECORD, models.OutgoingOptions{})
			awaitEvent(t, accepted, "the probe reaching the destination")
			requireUndecided(t, done, "while neither end had said anything")

			const request = "GET /auth/register HTTP/1.1\n"
			if _, err := io.WriteString(app, request); err != nil {
				t.Fatal(err)
			}
			r := awaitEvent(t, done, "the probe's verdict once the client spoke")
			if r.err != nil {
				t.Fatalf("probe: %v", r.err)
			}
			if r.probe.IsMySQL || r.probe.Reason != "client-spoke-first" {
				t.Fatalf("IsMySQL=%v reason=%q; the client spoke first, so it is not MySQL", r.probe.IsMySQL, r.probe.Reason)
			}
			if p.mysqlPorts.IsKnownNotMysql(port) || p.mysqlPorts.Has(port) {
				t.Errorf("a verdict on default port %d was cached; the next connection must be judged by what it does", port)
			}
			got := make([]byte, len(request))
			if _, err := io.ReadFull(r.probe.SrcConn, got); err != nil || string(got) != request {
				t.Fatalf("the client's first bytes read back as %q, %v; want %q", got, err, request)
			}
			if r.probe.Predialed == nil {
				t.Fatal("the probe dropped the upstream it dialled")
			}
			up, err := util.DialRaw(context.Background(), nil, "tcp", addr, r.probe.Predialed)
			if err != nil {
				t.Fatal(err)
			}
			defer up.Close()
			if _, err := io.WriteString(up, request); err != nil {
				t.Fatal(err)
			}
			answer, err := bufio.NewReader(up).ReadString('\n')
			if err != nil || answer != "ok "+request {
				t.Fatalf("the destination answered %q, %v; want %q", answer, err, "ok "+request)
			}
			if n := conns.Load(); n != 1 {
				t.Fatalf("the destination saw %d connections for the application's one", n)
			}
		})
	}
}

// A port learned as MySQL is a hint too: a client that speaks first there is
// dispatched on its bytes, and the port stays learned.
func TestRecordAClientThatSpeaksFirstOnALearnedMysqlPortIsNotMysql(t *testing.T) {
	addr, _, accepted := acceptingServer(t, silentUntilAsked)
	port := uint32(mustPort(t, addr))
	p := testProxy(t)
	p.mysqlPorts.Learn(port)
	app, srcConn := clientPair(t)

	done := probeAsync(p, srcConn, addr, port, models.MODE_RECORD, models.OutgoingOptions{})
	awaitEvent(t, accepted, "the probe reaching the destination")
	requireUndecided(t, done, "while neither end had said anything")
	if _, err := io.WriteString(app, "PING\n"); err != nil {
		t.Fatal(err)
	}
	r := awaitEvent(t, done, "the probe's verdict once the client spoke")
	if r.err != nil || r.probe.IsMySQL || r.probe.Reason != "client-spoke-first" {
		t.Fatalf("IsMySQL=%v reason=%q err=%v, want client-spoke-first", r.probe.IsMySQL, r.probe.Reason, r.err)
	}
	if !p.mysqlPorts.Has(port) {
		t.Error("the learned port was forgotten")
	}
	if r.probe.Predialed != nil {
		r.probe.Predialed.CloseIfUnused()
	}
}

// MySQL on a default port (TiDB on 4000) still takes the MySQL path, with the
// greeting the probe read handed on intact; the port is not learned, so a
// later replay in this agent still decides 4000 by its own recording.
func TestRecordMysqlOnADefaultPortStillTakesTheMysqlPath(t *testing.T) {
	for _, port := range []uint32{3306, 4000} {
		t.Run("port"+itoa(port), func(t *testing.T) {
			greeting := mysqlGreeting()
			addr, _ := serverThatGreets(t, greeting)
			p := testProxy(t)
			_, srcConn := clientPair(t)

			r := awaitEvent(t, probeAsync(p, srcConn, addr, port, models.MODE_RECORD, models.OutgoingOptions{}), "the probe's verdict")
			if r.err != nil || !r.probe.IsMySQL || r.probe.Reason != "default-port" {
				t.Fatalf("IsMySQL=%v reason=%q err=%v, want true/default-port", r.probe.IsMySQL, r.probe.Reason, r.err)
			}
			if r.probe.DstConn == nil {
				t.Fatal("the MySQL verdict dropped the upstream it read the greeting from")
			}
			defer r.probe.DstConn.Close()
			got := make([]byte, len(greeting))
			if _, err := io.ReadFull(r.probe.DstConn, got); err != nil || string(got) != string(greeting) {
				t.Fatalf("the greeting read back as %x, %v", got, err)
			}
			if p.mysqlPorts.Has(port) {
				t.Errorf("default port %d was learned", port)
			}
		})
	}
}

// A MySQL server that is slow to greet is waited for, however long: its
// client stays silent, so no clock may hand the connection to generic
// dispatch, where it would wait forever for the client.
func TestRecordASlowGreetingOnADefaultPortIsStillMysql(t *testing.T) {
	release := make(chan struct{})
	addr, _, accepted := acceptingServer(t, func(c net.Conn) {
		defer c.Close()
		<-release
		if _, err := c.Write(mysqlGreeting()); err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, c)
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	p := testProxy(t)
	_, srcConn := clientPair(t)

	done := probeAsync(p, srcConn, addr, 3306, models.MODE_RECORD, models.OutgoingOptions{})
	awaitEvent(t, accepted, "the probe reaching the destination")
	requireUndecided(t, done, "before the server greeted")
	close(release)
	r := awaitEvent(t, done, "the probe's verdict once the server greeted")
	if r.err != nil || !r.probe.IsMySQL || r.probe.Reason != "default-port" {
		t.Fatalf("IsMySQL=%v reason=%q err=%v, want true/default-port", r.probe.IsMySQL, r.probe.Reason, r.err)
	}
	_ = r.probe.DstConn.Close()
}

// Whatever else the destination does first on a default port takes the MySQL
// path, as the port alone took it before: an ERR packet in place of the
// handshake (MySQL refusing: Too many connections) is relayed to the client by
// the MySQL parser, and a close ends the client's connection there. An HTTP/2
// server's SETTINGS at accept is a client-first protocol's: not MySQL.
func TestRecordWhatADefaultPortsDestinationDoesFirstDecides(t *testing.T) {
	errPacket := []byte{0x17, 0x00, 0x00, 0x00, 0xff, 0x10, 0x04, 'T', 'o', 'o', ' ', 'm', 'a', 'n', 'y', ' ', 'c', 'o', 'n', 'n', 'e', 'c', 't', 'i', 'o', 'n', 's'}
	for _, tc := range []struct {
		name    string
		serve   func(c net.Conn)
		isMySQL bool
		reason  string
	}{
		{"an ERR packet", func(c net.Conn) { _, _ = c.Write(errPacket); _, _ = io.Copy(io.Discard, c); _ = c.Close() }, true, "default-port"},
		{"a close", func(c net.Conn) { _ = c.Close() }, true, "default-port"},
		{"an HTTP/2 SETTINGS", func(c net.Conn) { _, _ = c.Write(h2Settings); _, _ = io.Copy(io.Discard, c); _ = c.Close() }, false, "greeting-not-mysql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _, _ := acceptingServer(t, tc.serve)
			p := testProxy(t)
			_, srcConn := clientPair(t)

			r := awaitEvent(t, probeAsync(p, srcConn, addr, 4000, models.MODE_RECORD, models.OutgoingOptions{}), "the probe's verdict")
			if r.err != nil || r.probe.IsMySQL != tc.isMySQL || r.probe.Reason != tc.reason {
				t.Fatalf("IsMySQL=%v reason=%q err=%v, want %v/%s", r.probe.IsMySQL, r.probe.Reason, r.err, tc.isMySQL, tc.reason)
			}
			if tc.isMySQL && r.probe.DstConn == nil {
				t.Fatal("the MySQL verdict dropped the upstream")
			}
			if r.probe.DstConn != nil {
				_ = r.probe.DstConn.Close()
			}
			if r.probe.Predialed != nil {
				r.probe.Predialed.CloseIfUnused()
			}
			if p.mysqlPorts.IsKnownNotMysql(4000) || p.mysqlPorts.Has(4000) {
				t.Error("a verdict on default port 4000 was cached")
			}
		})
	}
}

// A port listed in mysqlPorts stays pinned to MySQL, as does a built-in
// default while detection is turned off: no probe, no dial.
func TestListedAndDetectionOffPortsArePinned(t *testing.T) {
	for _, tc := range []struct {
		name string
		port uint32
		opts models.OutgoingOptions
	}{
		{"listed", 3307, models.OutgoingOptions{MysqlPorts: []uint32{3307}}},
		{"default with detection off", 4000, models.OutgoingOptions{DisableMysqlAutoDetect: true}},
	} {
		for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
			t.Run(tc.name+"/"+string(mode), func(t *testing.T) {
				p := testProxy(t)
				p.deriveMysqlPorts(nil)
				_, srcConn := clientPair(t)
				// Nothing listens at 127.0.0.1:1: a dial would fail the probe.
				probe, err := p.probeMysql(context.Background(), srcConn, "127.0.0.1:1", nil, tc.port, mode, tc.opts, zap.NewNop())
				if err != nil || !probe.IsMySQL || probe.Reason != "configured-port" || probe.DstConn != nil {
					t.Fatalf("IsMySQL=%v reason=%q err=%v dialled=%v, want a pinned configured-port verdict", probe.IsMySQL, probe.Reason, err, probe.DstConn != nil)
				}
			})
		}
	}
}

// End to end through handleConnection: on a port known as MySQL, an
// application's request to a client-speaks-first service is relayed and
// answered. It used to go to the MySQL path and get no answer.
func TestHandleConnectionRelaysAClientFirstServiceOnAMysqlPort(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	addr, conns, accepted := acceptingServer(t, silentUntilAsked)

	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, addr)}, cfg)
	parser := relayParser{relayed: make(chan string, 1)}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{
		integrations.MYSQL: mysql.New(zap.NewNop()),
		"relay":            parser,
	}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: "relay"}}
	if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	// The destination's port stands for 4000: known as MySQL.
	p.mysqlPorts.Learn(uint32(mustPort(t, addr)))

	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	awaitEvent(t, accepted, "the probe reaching the destination")

	// A line protocol rather than HTTP: the relay parser, not the HTTP
	// path, is what tells the test the request went through.
	const request = "register 1\n"
	if _, err := io.WriteString(app, request); err != nil {
		t.Fatal(err)
	}
	if got := awaitEvent(t, parser.relayed, "the request being relayed"); got != request {
		t.Fatalf("relayed %q, want %q", got, request)
	}
	answer, err := bufio.NewReader(app).ReadString('\n')
	if err != nil || answer != "ok "+request {
		t.Fatalf("the application read %q, %v; want %q", answer, err, "ok "+request)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("the destination saw %d connections for the application's one", n)
	}
	_ = app.Close()
	awaitEvent(t, done, "handleConnection returning")
}

func httpMockTo(host string) *models.Mock {
	return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq: &models.HTTPReq{Method: "POST", URL: "/auth/register", Header: map[string]string{"Host": host}},
	}}
}

func mysqlMockAt(destAddr string) *models.Mock {
	m := &models.Mock{Kind: models.MySQL, Spec: models.MockSpec{Metadata: map[string]string{}}}
	if destAddr != "" {
		m.Spec.Metadata["destAddr"] = destAddr
	}
	return m
}

// A replay has no destination to speak first, so a default port is decided by
// the recording: an HTTP exchange recorded on 4000 replays as HTTP (the MySQL
// replayer would greet the HTTP client), a MySQL one recorded there as MySQL,
// and a port the recording says nothing of stays MySQL, so a MySQL connection
// in a set that recorded none still gets the MySQL replayer's miss.
func TestReplayDecidesADefaultPortByTheRecording(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mocks   []*models.Mock
		port    uint32
		isMySQL bool
		reason  string
	}{
		{"HTTP recorded on 4000", []*models.Mock{httpMockTo("localhost:4000"), mysqlMockAt("db:3306")}, 4000, false, "recorded-not-mysql"},
		{"HTTP recorded on 3306", []*models.Mock{httpMockTo("api:3306")}, 3306, false, "recorded-not-mysql"},
		{"MySQL recorded on 4000", []*models.Mock{mysqlMockAt("tidb:4000"), httpMockTo("localhost:8080")}, 4000, true, "known-port"},
		{"MySQL recorded on 3306", []*models.Mock{mysqlMockAt("db:3306")}, 3306, true, "known-port"},
		{"HTTP and MySQL both recorded on 4000", []*models.Mock{httpMockTo("localhost:4000"), mysqlMockAt("tidb:4000")}, 4000, true, "known-port"},
		{"nothing recorded on 3306", []*models.Mock{httpMockTo("localhost:4000")}, 3306, true, "default-port"},
		{"no mocks at all", nil, 3306, true, "default-port"},
		{"MySQL recorded without an address", []*models.Mock{httpMockTo("localhost:4000"), mysqlMockAt("")}, 4000, true, "default-port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProxy(t)
			p.deriveMysqlPorts(tc.mocks)
			app, srcConn := clientPair(t)

			const request = "POST /auth/register HTTP/1.1\r\nHost: localhost\r\n\r\n"
			if !tc.isMySQL {
				// An HTTP client speaks at once; the verdict must not
				// depend on that, but its bytes must survive it.
				if _, err := io.WriteString(app, request); err != nil {
					t.Fatal(err)
				}
			}
			r := awaitEvent(t, probeAsync(p, srcConn, "127.0.0.1:"+itoa(tc.port), tc.port, models.MODE_TEST, models.OutgoingOptions{}), "the probe's verdict")
			if r.err != nil || r.probe.IsMySQL != tc.isMySQL || r.probe.Reason != tc.reason {
				t.Fatalf("IsMySQL=%v reason=%q err=%v, want %v/%s", r.probe.IsMySQL, r.probe.Reason, r.err, tc.isMySQL, tc.reason)
			}
			if r.probe.DstConn != nil {
				t.Fatal("a replay probe dialled")
			}
			if !tc.isMySQL {
				got := make([]byte, len(request))
				if _, err := io.ReadFull(r.probe.SrcConn, got); err != nil || string(got) != request {
					t.Fatalf("the client's bytes read back as %q, %v", got, err)
				}
			}
			if p.mysqlPorts.IsKnownNotMysql(tc.port) {
				t.Error("a verdict on a default port was cached")
			}
		})
	}
}

// A replay connection to a default port that arrives before its test set's
// mocks are stored waits for them, and the recording then decides it.
func TestReplayADefaultPortWaitsForTheRecording(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mocks   []*models.Mock
		isMySQL bool
	}{
		{"MySQL recorded", []*models.Mock{mysqlMockAt("tidb:4000")}, true},
		{"HTTP recorded", []*models.Mock{httpMockTo("drogon:4000")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProxy(t)
			_, srcConn := clientPair(t)
			done := probeAsync(p, srcConn, "127.0.0.1:4000", 4000, models.MODE_TEST, models.OutgoingOptions{})
			requireUndecided(t, done, "before the mocks were stored")
			p.deriveMysqlPorts(tc.mocks)
			r := awaitEvent(t, done, "the probe's verdict once the mocks were stored")
			if r.err != nil || r.probe.IsMySQL != tc.isMySQL {
				t.Fatalf("IsMySQL=%v reason=%q err=%v, want %v", r.probe.IsMySQL, r.probe.Reason, r.err, tc.isMySQL)
			}
		})
	}
}

// What one recording said of a default port does not outlive it: the next
// session's recording decides.
func TestReplayADefaultPortsVerdictFollowsTheSession(t *testing.T) {
	p := testProxy(t)
	p.deriveMysqlPorts([]*models.Mock{httpMockTo("localhost:4000")})
	p.mysqlPorts.MarkSessionStale()
	p.deriveMysqlPorts([]*models.Mock{mysqlMockAt("")})
	if p.mysqlPorts.recordedNotMysql(4000) {
		t.Fatal("the previous recording's HTTP on 4000 outlived its session")
	}
	p.mysqlPorts.MarkSessionStale()
	p.deriveMysqlPorts([]*models.Mock{httpMockTo("localhost:4000")})
	if !p.mysqlPorts.recordedNotMysql(4000) {
		t.Fatal("the previous recording's address-less MySQL outlived its session")
	}
}

func itoa(n uint32) string { return strconv.FormatUint(uint64(n), 10) }
