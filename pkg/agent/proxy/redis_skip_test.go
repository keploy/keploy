package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/miekg/dns"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
)

// What go-redis 9.7.1 sends first on every connection, and the PING an
// application then sends on it (captured from the client, see redisskip's
// tests).
const (
	goRedisHello = "*2\r\n$5\r\nhello\r\n$1\r\n3\r\n"
	goRedisPing  = "*1\r\n$4\r\nping\r\n"
)

// freshSaid gives p a record of what the Redis skip said of its own, in
// place of the process's (processRedisSkipSaid), so a destination another
// test was told about is told about again.
func freshSaid(p *Proxy) *Proxy {
	p.redisSaid = &redisSkipSaid{}
	return p
}

// fakeRedis is the real server of these tests: it answers each read with one
// line, "+REAL\r\n" to a RESP frame and "END\r\n" to anything else (a
// memcached-style text request), and keeps every byte it received.
type fakeRedis struct {
	addr string
	mu   sync.Mutex
	got  []string
}

func (f *fakeRedis) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			f.mu.Lock()
			f.got = append(f.got, string(buf[:n]))
			f.mu.Unlock()
			reply := "END\r\n"
			if buf[0] == '*' {
				reply = "+REAL\r\n"
			}
			if _, werr := io.WriteString(c, reply); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *fakeRedis) received() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.got, "")
}

// newFakeRedis listens on loopback, over TLS when cfg is set.
func newFakeRedis(t *testing.T, cfg *tls.Config) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		ln = tls.NewListener(ln, cfg)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeRedis{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

// roundTrip writes req on c and returns the line the other end answers.
func roundTrip(t *testing.T, c io.ReadWriter, br *bufio.Reader, req string) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		if _, err := io.WriteString(c, req); err != nil {
			got <- "write error: " + err.Error()
			return
		}
		line, err := br.ReadString('\n')
		if err != nil {
			got <- "read error: " + err.Error()
			return
		}
		got <- line
	}()
	return awaitEvent(t, got, "the answer to "+req)
}

// redisNotices are the notices logs holds, by destination.
func redisNotices(logs *observer.ObservedLogs) map[string]int {
	out := map[string]int{}
	for _, e := range logs.FilterMessage(integrations.RedisSkipNotice).All() {
		if e.Level != zapcore.WarnLevel {
			continue
		}
		dst, _ := e.ContextMap()["destination"].(string)
		out[dst]++
	}
	return out
}

// fakeRedisParser stands in for Keploy Enterprise's Redis parser.
type fakeRedisParser struct{ ctxProbe }

// The proxy installs the Redis skip only when no integration is registered
// as integrations.REDIS once every init has run, which is when
// InitIntegrations reads the registry: a Redis parser registered after the
// proxy was made but before its parsers were set up (as a package's init
// would be, whatever its order) still keeps the skip out. The skip is the
// proxy's own and never enters the registry.
func TestTheRedisSkipIsInstalledOnlyWithoutARedisParser(t *testing.T) {
	newProxy := func() *Proxy {
		cfg := config.New()
		cfg.Agent.Mode = models.MODE_RECORD
		return New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, "127.0.0.1:6379")}, cfg)
	}
	if _, ok := integrations.Registered[integrations.REDIS]; ok {
		t.Fatal("this build registers a Redis parser; the test assumes it does not")
	}

	p := newProxy()
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Integrations[integrations.REDIS_SKIP]; !ok {
		t.Fatal("without a Redis parser the proxy did not install the Redis skip")
	}
	if last := p.integrationsPriority[len(p.integrationsPriority)-1]; last.ParserType != integrations.REDIS_SKIP {
		t.Fatalf("the Redis skip is not consulted last: the last parser is %s", last.ParserType)
	}
	if _, ok := integrations.Registered[integrations.REDIS_SKIP]; ok {
		t.Fatal("the Redis skip entered the registry")
	}

	p = newProxy()
	integrations.Register(integrations.REDIS, &integrations.Parsers{
		Initializer: func(*zap.Logger) integrations.Integrations { return fakeRedisParser{} },
		Priority:    200,
	})
	t.Cleanup(func() { delete(integrations.Registered, integrations.REDIS) })
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Integrations[integrations.REDIS_SKIP]; ok {
		t.Fatal("with a Redis parser registered the proxy installed the Redis skip")
	}
	if _, ok := p.Integrations[integrations.REDIS].(fakeRedisParser); !ok {
		t.Fatal("the registered Redis parser is not among the proxy's parsers")
	}
	for _, pp := range p.integrationsPriority {
		if pp.ParserType == integrations.REDIS_SKIP {
			t.Fatal("with a Redis parser registered the Redis skip is in the parsers' order")
		}
	}
}

// In a record, a connection whose client opens with a Redis command is
// relayed to the real Redis byte for byte and nothing of it is recorded: no
// mock reaches the recording, while a non-Redis connection to the same
// destination right after it is recorded by the Generic parser as before
// (its mock is the first the recording gets, so no Redis mock came before
// it). The notice is said once for the destination over the pool's three
// connections. (That it is never said for non-Redis traffic is
// TestNonRedisTrafficIsRecordedAndNeverSkipped's: here the destination is
// the same, so the notice said once already would hide a second.)
func TestARecordedRedisConnectionIsRelayedAndRecordsNothing(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	up := newFakeRedis(t, nil)
	p, mocks := recordProxy(t, zap.New(core), up.addr)
	for i := 0; i < 3; i++ {
		serveConn(t, p, false, goRedisHello, goRedisPing)
	}
	if got, want := up.received(), strings.Repeat(goRedisHello+goRedisPing, 3); got != want {
		t.Fatalf("the real Redis received %q, want the client's bytes as sent, %q", got, want)
	}

	serveConn(t, p, false, "get k\r\n")
	m := awaitEvent(t, mocks, "the non-Redis connection's mock")
	if got := firstRequest(t, m); got != "get k\r\n" {
		t.Fatalf("the first mock recorded is of %q, want the non-Redis connection's Generic mock: a Redis connection was recorded", got)
	}
	if got := redisNotices(logs); len(got) != 1 || got[up.addr] != 1 {
		t.Fatalf("over three Redis connections and one other to %s the notice was said %v, want once for it", up.addr, got)
	}
}

// Over TLS the decrypted stream is matched the same way in a record: the
// client's TLS session with keploy's MITM is relayed to the real Redis over
// a TLS session of keploy's own, and nothing of it is recorded (the first
// mock the recording gets is the non-Redis TLS connection's after it).
func TestARecordedRedisConnectionOverTLSIsRelayedAndRecordsNothing(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	up := newFakeRedis(t, newTestServerTLSConfig(t, nil))
	p, mocks := recordProxy(t, zap.New(core), up.addr)
	serveConn(t, p, true, goRedisHello, goRedisPing)
	if got, want := up.received(), goRedisHello+goRedisPing; got != want {
		t.Fatalf("the real Redis received %q over TLS, want %q", got, want)
	}
	serveConn(t, p, true, "get k\r\n")
	m := awaitEvent(t, mocks, "the non-Redis TLS connection's mock")
	if got := firstRequest(t, m); got != "get k\r\n" {
		t.Fatalf("the first mock recorded over TLS is of %q, want the non-Redis connection's: the Redis connection was recorded", got)
	}
	if got := redisNotices(logs); len(got) != 1 || got[up.addr] != 1 {
		t.Fatalf("the record said the notice %v, want once for %s", got, up.addr)
	}
}

// Nothing but a Redis client's first frame is skipped: a memcached client's
// get and set, a binary protocol whose first byte is RESP's array byte, and
// a RESP array that names no Redis command are each recorded by the Generic
// parser, byte for byte, and the notice is never said.
func TestNonRedisTrafficIsRecordedAndNeverSkipped(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	up := newFakeRedis(t, nil)
	p, mocks := recordProxy(t, zap.New(core), up.addr)
	reqs := []string{"get k\r\n", "set k 0 0 5\r\nhello\r\n", "*\x00\x01\x02\xff\r\n", "*1\r\n$4\r\nPONG\r\n"}
	for _, req := range reqs {
		serveConn(t, p, false, req)
		m := awaitEvent(t, mocks, fmt.Sprintf("the mock of %q", req))
		if got := firstRequest(t, m); got != req {
			t.Fatalf("the mock recorded for %q is of %q", req, got)
		}
	}
	if got, want := up.received(), strings.Join(reqs, ""); got != want {
		t.Fatalf("the server received %q, want %q", got, want)
	}
	if got := redisNotices(logs); len(got) != 0 {
		t.Fatalf("the notice was said %v for traffic that is not Redis", got)
	}
}

// recordProxy is a proxy recording with this build's parsers and the Redis
// skip, its connections reaching dst; what it records reaches the channel it
// returns.
func recordProxy(t *testing.T, logger *zap.Logger, dst string) (*Proxy, chan *models.Mock) {
	t.Helper()
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := freshSaid(New(logger, fakeDestInfo{dest: ipv4Dest(t, dst)}, cfg))
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	mocks := make(chan *models.Mock, 64)
	if err := p.Record(context.Background(), mocks, models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	return p, mocks
}

// serveConn has p handle one connection, over TLS when overTLS, on which the
// application sends each of reqs and is answered as fakeRedis answers it
// ("+REAL" to what starts with '*', "END" to anything else). It returns what
// handleConnection returned, once it has.
func serveConn(t *testing.T, p *Proxy, overTLS bool, reqs ...string) error {
	t.Helper()
	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	var c io.ReadWriteCloser = app
	if overTLS {
		// No SNI: keploy dials the address the client dialled.
		c = tls.Client(app, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // keploy's MITM leaf is not under test
	}
	br := bufio.NewReader(c)
	for _, req := range reqs {
		want := "+REAL\r\n"
		if req[0] != '*' {
			want = "END\r\n"
		}
		if got := roundTrip(t, c, br, req); got != want {
			t.Fatalf("%q was answered %q, want the real server's %q", req, got, want)
		}
	}
	_ = c.Close()
	return awaitEvent(t, done, "handleConnection returning")
}

// firstRequest is the bytes of m's first request, as the Generic parser
// stored them (base64 when they are not ASCII).
func firstRequest(t *testing.T, m *models.Mock) string {
	t.Helper()
	if m.Kind != models.GENERIC || len(m.Spec.GenericRequests) == 0 || len(m.Spec.GenericRequests[0].Message) == 0 {
		t.Fatalf("the mock is %s %+v, want a Generic mock with a request", m.Kind, m.Spec.GenericRequests)
	}
	msg := m.Spec.GenericRequests[0].Message[0]
	if msg.Type == models.String {
		return msg.Data
	}
	b, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		t.Fatalf("the mock's %s request %q does not decode: %v", msg.Type, msg.Data, err)
	}
	return string(b)
}

// replayProxy is a proxy in a replay of mocks, with this build's parsers and
// the Redis skip, its connections reaching dst.
func replayProxy(t *testing.T, logger *zap.Logger, dst string, mocks ...*models.Mock) *Proxy {
	t.Helper()
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := freshSaid(New(logger, fakeDestInfo{dest: ipv4Dest(t, dst)}, cfg))
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true, DisableMysqlAutoDetect: true}})
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SetUnFilteredMocks(mocks)
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	return p
}

// oldRedisMock is what an earlier keploy recorded of go-redis's HELLO
// through the Generic parser: a test set recorded before this build carries
// such mocks.
func oldRedisMock() *models.Mock {
	return &models.Mock{
		Name: "mock-0",
		Kind: models.GENERIC,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config"},
			GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: models.String, Data: goRedisHello}}}},
			GenericResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: models.String, Data: "+FROM-MOCK\r\n"}}}},
		},
	}
}

// In a replay a Redis connection reaches the real Redis, which answers it,
// byte for byte, and the Generic mock an earlier keploy recorded of it is
// not served (nor consumed): this build never answers Redis from mocks. The
// notice is said once for the destination.
func TestAReplayedRedisConnectionReachesTheRealRedisNotItsOldMock(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	up := newFakeRedis(t, nil)
	old := oldRedisMock()
	p := replayProxy(t, zap.New(core), up.addr, old)

	for i := 0; i < 2; i++ {
		app, src := acceptedConn(t)
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		br := bufio.NewReader(app)
		for _, req := range []string{goRedisHello, goRedisPing} {
			if got := roundTrip(t, app, br, req); got != "+REAL\r\n" {
				t.Fatalf("%q was answered %q, want the real Redis's answer", req, got)
			}
		}
		_ = app.Close()
		awaitEvent(t, done, "handleConnection returning")
	}
	if got, want := up.received(), strings.Repeat(goRedisHello+goRedisPing, 2); got != want {
		t.Fatalf("the real Redis received %q, want %q", got, want)
	}
	if consumed, _ := p.GetConsumedMocks(context.Background()); len(consumed) != 0 {
		t.Fatalf("the replay consumed %+v; an old Generic Redis mock must not be consulted", consumed)
	}
	if got := redisNotices(logs); len(got) != 1 || got[up.addr] != 1 {
		t.Fatalf("the replay said the notice %v, want once for %s", got, up.addr)
	}
}

// Over TLS the decrypted stream is matched the same way: in a replay the
// client's TLS session with keploy's MITM is relayed to the real Redis over
// a TLS session of keploy's own.
func TestAReplayedRedisConnectionOverTLSReachesTheRealRedis(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	up := newFakeRedis(t, newTestServerTLSConfig(t, nil))
	p := replayProxy(t, zap.New(core), up.addr, oldRedisMock())

	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	// No SNI: keploy dials the address the client dialled.
	client := tls.Client(app, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // keploy's MITM leaf is not under test
	br := bufio.NewReader(client)
	for _, req := range []string{goRedisHello, goRedisPing} {
		if got := roundTrip(t, client, br, req); got != "+REAL\r\n" {
			t.Fatalf("%q over TLS was answered %q, want the real Redis's answer", req, got)
		}
	}
	_ = client.Close()
	awaitEvent(t, done, "handleConnection returning")
	if got, want := up.received(), goRedisHello+goRedisPing; got != want {
		t.Fatalf("the real Redis received %q over TLS, want %q", got, want)
	}
	if got := redisNotices(logs); len(got) != 1 || got[up.addr] != 1 {
		t.Fatalf("the replay said the notice %v, want once for %s", got, up.addr)
	}
}

// A replay whose Redis is not running fails the connection (keploy closes
// the application's end unanswered), says the notice, and says once for the
// destination, at Error, that keploy could not reach it and that it must be
// running: over a pool's retries, once. No old mock answers it.
func TestAReplayedRedisConnectionToADownRedisSaysWhy(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := ln.Addr().String()
	_ = ln.Close()
	p := replayProxy(t, zap.New(core), down, oldRedisMock())

	for i := 0; i < 3; i++ {
		app, src := acceptedConn(t)
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		if _, err := io.WriteString(app, goRedisHello); err != nil {
			t.Fatal(err)
		}
		err := awaitEvent(t, done, "handleConnection returning")
		if err == nil {
			t.Fatal("a connection to a Redis that is down was served")
		}
		if !errors.As(err, new(redisUnreachable)) {
			t.Fatalf("connection %d to a Redis that is down returned %v, want it marked as said (redisUnreachable)", i, err)
		}
		if b, _ := bufio.NewReader(app).ReadString('\n'); b == "+FROM-MOCK\r\n" {
			t.Fatal("a connection to a Redis that is down was answered from the old mock")
		}
	}
	if got := redisNotices(logs); len(got) != 1 || got[down] != 1 {
		t.Fatalf("the notices are %v, want one for %s", got, down)
	}
	unreachable := logs.FilterMessage("keploy could not connect to a Redis the application calls").All()
	if len(unreachable) != 1 || unreachable[0].Level != zapcore.ErrorLevel {
		t.Fatalf("over three refused connections keploy said it could not reach %s %d times (%v), want once at error", down, len(unreachable), unreachable)
	}
	if fields := unreachable[0].ContextMap(); fields["destination"] != down || !strings.Contains(fmt.Sprint(fields["next_step"]), "must be running") {
		t.Fatalf("the error carries %v, want the destination and that Redis must be running", fields)
	}
}

// What handling a connection to a Redis that cannot be reached returns was
// said by serveRedisSkip already, once per destination: the accept loop logs
// it at Debug, wherever the Redis is. The same failure unmarked, to a Redis
// off this machine, would be an error each time a pool reconnects.
func TestAnUnreachableRedisIsNotAnErrorOfEveryConnection(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.7"), Port: 6379}, Err: syscall.ECONNREFUSED}
	core, logs := observer.New(zapcore.DebugLevel)
	p := New(zap.New(core), fakeDestInfo{dest: ipv4Dest(t, "10.0.0.7:6379")}, config.New())
	p.logHandled(redisUnreachable{refused})
	if errs := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errs) != 0 {
		t.Fatalf("a Redis already said to be unreachable was logged as an error again: %v", errs)
	}
	if logs.Len() != 1 {
		t.Fatalf("logHandled logged %d lines, want one at debug: %v", logs.Len(), logs.All())
	}
	p.logHandled(refused)
	if errs := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errs) != 1 {
		t.Fatalf("an unmarked refusal by a host off this machine was logged %v, want one error (the check above would prove nothing)", errs)
	}
}

// In a replay keploy's DNS answers the application's lookup of a name from
// the recording, which may give an address the name no longer has (a
// container recreated since, whose old address may now be another's). A
// Redis connection to an address so answered is relayed to the name's
// address now, as the resolver keploy forwards to gives it: here the
// recording said 127.0.0.2, where nothing listens, and the name is at
// 127.0.0.1 now.
func TestAReplayedRedisConnectionReachesItsNamesAddressNow(t *testing.T) {
	up := newFakeRedis(t, nil)
	_, port, err := net.SplitHostPort(up.addr)
	if err != nil {
		t.Fatal(err)
	}
	stale := net.JoinHostPort("127.0.0.2", port)
	upstream, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if len(r.Question) > 0 && r.Question[0].Name == "redis.test." && r.Question[0].Qtype == dns.TypeA {
			rr, err := dns.NewRR("redis.test. 60 IN A 127.0.0.1")
			if err != nil {
				t.Error(err)
			}
			m.Answer = append(m.Answer, rr)
		}
		_ = w.WriteMsg(m)
	})
	t.Cleanup(stop)
	recordedDNS := &models.Mock{
		Name: "mock-dns",
		Kind: models.DNS,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"},
			DNSReq:   &models.DNSReq{Name: "redis.test.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
			DNSResp: &models.DNSResp{
				Rcode:   dns.RcodeSuccess,
				Answers: []string{"redis.test. 60 IN A 127.0.0.2"},
			},
		},
	}
	p := replayProxy(t, zap.NewNop(), stale, recordedDNS, oldRedisMock())
	host, uport, err := net.SplitHostPort(upstream)
	if err != nil {
		t.Fatal(err)
	}
	p.dnsUpstreamServers, p.dnsUpstreamPort = []string{host}, uport

	if got := lookup(t, p, "redis.test.", false); len(got) != 1 || got[0] != "127.0.0.2" {
		t.Fatalf("the replay's DNS answered redis.test. with %v, want the recording's 127.0.0.2", got)
	}

	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	br := bufio.NewReader(app)
	if got := roundTrip(t, app, br, goRedisHello); got != "+REAL\r\n" {
		t.Fatalf("a Redis connection to %s, the recorded answer for redis.test., was answered %q, want the Redis at redis.test.'s address now", stale, got)
	}
	_ = app.Close()
	if err := awaitEvent(t, done, "handleConnection returning"); err != nil {
		t.Fatalf("handleConnection returned %v", err)
	}
	if got := up.received(); got != goRedisHello {
		t.Fatalf("the Redis at redis.test.'s address now received %q, want %q", got, goRedisHello)
	}
}

// A dial that ends because its connection, or the agent, did (its context)
// is no failure to reach the Redis: it does not use up the one error said
// for the destination. A dial the Redis resets is one, and gets it; the
// refusal after it is said at Debug.
func TestAnEndedDialDoesNotUseUpTheUnreachableError(t *testing.T) {
	const dst = "10.0.0.7:6379"
	core, logs := observer.New(zapcore.InfoLevel)
	p := freshSaid(New(zap.New(core), fakeDestInfo{dest: ipv4Dest(t, dst)}, config.New()))
	opErr := func(errno syscall.Errno) error {
		return &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.7"), Port: 6379}, Err: errno}
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	var none net.Conn
	// Even a reset, once the connection has ended, is the ending's.
	if err := p.serveRedisSkip(ended, p.logger, dst, nil, &none, func() (net.Conn, string, error) { return nil, dst, opErr(syscall.ECONNRESET) }); err == nil {
		t.Fatal("a dial that ended was served")
	}
	if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
		t.Fatalf("a dial that ended with its connection was logged as an error: %v", logs.All())
	}
	for _, errno := range []syscall.Errno{syscall.ECONNRESET, syscall.ECONNREFUSED} {
		err := p.serveRedisSkip(context.Background(), p.logger, dst, nil, &none, func() (net.Conn, string, error) { return nil, dst, opErr(errno) })
		if !errors.As(err, new(redisUnreachable)) {
			t.Fatalf("a dial that failed with %v returned %v, want it marked as said", errno, err)
		}
	}
	said := logs.FilterMessage("keploy could not connect to a Redis the application calls").All()
	if len(said) != 1 || said[0].Level != zapcore.ErrorLevel {
		t.Fatalf("after an ended dial, a reset and a refusal keploy said it could not connect %d times (%v), want once at error", len(said), said)
	}
	if fields := said[0].ContextMap(); !strings.Contains(fmt.Sprint(fields["error"]), "reset") || fields["dialled"] != dst {
		t.Fatalf("the error said is %v, want the reset's, with the address dialled", fields)
	}
}
