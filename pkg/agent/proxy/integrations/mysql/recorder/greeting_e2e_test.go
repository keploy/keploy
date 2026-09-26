package recorder

// End-to-end guard for the post-TLS greeting against a REAL MySQL server.
//
// It runs only when KEPLOY_E2E_MYSQL_ADDR names a MySQL 8 server by a ROUTABLE
// address (a container IP, as a pod IP or ClusterIP would be in a cluster) and
// KEPLOY_E2E_MYSQL_PASSWORD is its root password. The lane that runs it is
// .github/workflows/mysql-tls-greeting-stitch-linux.yml, through
// .github/workflows/test_workflow_scripts/mysql-tls-greeting-e2e.sh.
//
// The application is go-sql-driver/mysql with tls=skip-verify, pooling its
// connections. It reaches the server through a relay that plays the capture
// layer's part: it terminates TLS on both sides, and while a recording is on it
// feeds the recorder what the proxyless capture would. That is each decrypted
// stream from its next packet on (the uprobe leg, a tls-* stream), and, for a
// connection opened DURING a recording, its plaintext greeting and SSLRequest
// (the raw leg), delivered late on purpose. One TLSHandshakeStore serves every
// recording, as the agent's does.
//
// What it asserts, and what the server itself counts:
//   - The pool is opened BEFORE the first recording, so every pooled
//     connection joins it mid-stream: no greeting will ever be captured for
//     it. Its commands must still be recorded promptly, not after the 30s
//     stash wait.
//   - Two consecutive recording sessions against the same server cost exactly
//     ONE greeting dial between them, and a third costs none. Every dial reads
//     the greeting and hangs up, which MySQL counts in Aborted_connects (and
//     against the dialling host's max_connect_errors).
//   - Three fresh connections opened during the third session are each
//     stitched with ITS OWN greeting (its own connection id, its SSLRequest,
//     its greeting's timestamp): not another's, and not the one remembered for
//     the server. The first one's decrypted stream is already waiting when its
//     raw leg lands late, and another connection's leg lands before its own.
//     The other two's raw legs land in the opposite order to their decrypted
//     streams. By arrival order alone, every one of them would take another's.
//     The relay names each connection on both of its legs (ConnKey), as the
//     capture layer does with the socket cookie.
//   - A raw leg's capture stitches ONE config mock, its own connection's: its
//     timestamp is in no other. treedb.sameMock identifies a mock by
//     Name+Kind+ReqTimestampMock, so a capture stitched twice is one mock to
//     replay.
//
// It uses only recorder APIs that predate the per-server greeting memo, so the
// same file runs against an older tree to show what it catches there.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

const (
	e2eMySQLAddrEnv     = "KEPLOY_E2E_MYSQL_ADDR"
	e2eMySQLPasswordEnv = "KEPLOY_E2E_MYSQL_PASSWORD"
	// e2ePromptBound is how long a pooled connection's command may take to be
	// recorded. The stash wait it must not sit out is 30s.
	e2ePromptBound = 5 * time.Second
	// e2eRawLegDelay is how late the raw leg of a fresh connection lands.
	e2eRawLegDelay = time.Second
	e2ePoolSize    = 4
	// e2eFreshConns are opened in the third session, in this order.
	e2eFreshConns = 3
)

func TestE2E_MySQLTLSGreeting_PooledConnsAcrossRecordingSessions(t *testing.T) {
	addr := os.Getenv(e2eMySQLAddrEnv)
	password := os.Getenv(e2eMySQLPasswordEnv)
	if addr == "" {
		t.Skipf("%s is not set; this end-to-end test needs a real MySQL 8 server", e2eMySQLAddrEnv)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("%s=%q: %v", e2eMySQLAddrEnv, addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsLoopback() {
		t.Fatalf("%s=%q must be a ROUTABLE IP (a container IP): the DaemonSet case this guards "+
			"reaches its server by pod IP, ClusterIP or external IP", e2eMySQLAddrEnv, addr)
	}
	port, _ := strconv.Atoi(portStr)

	// The server's own count of aborted handshakes, read on one plaintext admin
	// connection opened before the baseline so it is not counted itself.
	admin, err := sql.Open("mysql", fmt.Sprintf("root:%s@tcp(%s)/?timeout=5s", password, addr))
	if err != nil {
		t.Fatalf("admin open: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	admin.SetMaxOpenConns(1)
	admin.SetConnMaxLifetime(0)
	abortedConnects := func() int {
		t.Helper()
		var name string
		var v int
		if err := admin.QueryRow("SHOW GLOBAL STATUS LIKE 'Aborted_connects'").Scan(&name, &v); err != nil {
			t.Fatalf("read Aborted_connects: %v", err)
		}
		return v
	}
	abortedBefore := abortedConnects()

	relay := startE2ERelay(t, addr, port)

	// The application's pool, opened and authenticated BEFORE any recording.
	dsn := fmt.Sprintf("root:%s@tcp(%s)/?tls=skip-verify&timeout=5s&readTimeout=30s", password, relay.addr)
	app, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("app open: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	app.SetMaxOpenConns(e2ePoolSize + e2eFreshConns)
	app.SetMaxIdleConns(e2ePoolSize + e2eFreshConns)
	app.SetConnMaxLifetime(0)
	pooled := make([]*sql.Conn, e2ePoolSize)
	for i := range pooled {
		c, err := app.Conn(context.Background())
		if err != nil {
			t.Fatalf("pooled connection %d: %v", i, err)
		}
		if err := c.PingContext(context.Background()); err != nil {
			t.Fatalf("ping pooled connection %d: %v", i, err)
		}
		pooled[i] = c
	}
	t.Cleanup(func() {
		for _, c := range pooled {
			_ = c.Close()
		}
	})
	if n := relay.connections(); n != e2ePoolSize {
		t.Fatalf("relay carries %d connections, want the %d pooled ones", n, e2ePoolSize)
	}
	if got := abortedConnects() - abortedBefore; got != 0 {
		t.Fatalf("opening the pool aborted %d connects; the baseline is not clean", got)
	}

	// One store for every recording, as the agent keeps one. Sessions 0 and 1
	// have only the pooled connections, so any greeting has to be fetched: they
	// are the "one dial across sessions" check. Session 2 adds a fresh
	// connection whose raw leg lands late.
	store := models.NewTLSHandshakeStore()
	for session := 0; session < 3; session++ {
		scope := fmt.Sprintf("e2e/app/test-set-%d", session)
		rec := relay.startRecording(store, addr, port, scope)

		var wg sync.WaitGroup
		sent := make([]time.Time, e2ePoolSize)
		for i, c := range pooled {
			wg.Add(1)
			go func(i int, c *sql.Conn) {
				defer wg.Done()
				sent[i] = time.Now()
				var v int
				if err := c.QueryRowContext(context.Background(), fmt.Sprintf("SELECT %d", 100+i)).Scan(&v); err != nil {
					t.Errorf("session %d, pooled connection %d: query: %v", session, i, err)
				}
			}(i, c)
		}
		wg.Wait()

		var fresh []*e2eFresh
		if session == 2 {
			// Raw legs land in the order F2, F0, F1. F0's decrypted stream
			// starts at once and waits; F1's and F2's start after every raw
			// leg has landed, F1's first.
			relay.setLegDelays(
				[]time.Duration{e2eRawLegDelay, 2 * e2eRawLegDelay, e2eRawLegDelay / 2},
				[]time.Duration{0, 5 * e2eRawLegDelay / 2, 3 * e2eRawLegDelay})
			fresh = runFreshConnections(t, app, e2eFreshConns)
		}

		legs := rec.waitForQueries(t, e2ePoolSize, 45*time.Second)
		for _, l := range legs {
			took := l.firstQueryAt.Sub(l.startedAt).Truncate(time.Millisecond)
			if !l.joinedMidStream {
				continue
			}
			t.Logf("session %d: pooled connection's first command recorded after %v", session, took)
			if took > e2ePromptBound {
				t.Errorf("session %d: a pooled connection's first command was recorded %v after it was sent; "+
					"it waited for a greeting that cannot come", session, took)
			}
		}
		if fresh != nil {
			rec.assertFreshConnsStitchedWithTheirOwnGreetings(t, fresh)
		}
		rec.assertEachCaptureStitchesOnlyItsOwnConnection(t)
		rec.stop(t)
		dials := abortedConnects() - abortedBefore
		t.Logf("session %d (scope %s): %d decrypted legs recorded their commands; Aborted_connects +%d so far",
			session, scope, len(legs), dials)
		if session == 1 && dials != 1 {
			t.Errorf("two consecutive recording sessions against one server made %d greeting dials "+
				"(Aborted_connects delta), want exactly 1: each is an aborted handshake counted against "+
				"the dialling host", dials)
		}
	}

	if got := abortedConnects() - abortedBefore; got != 1 {
		t.Fatalf("three recording sessions against one server made %d greeting dials (Aborted_connects "+
			"delta), want exactly 1: each is an aborted handshake counted against the dialling host", got)
	}
}

// e2eFresh is a connection opened during a recording.
type e2eFresh struct {
	mysqlConnID uint32
}

// runFreshConnections opens n new connections one after the other (the pooled
// ones are all held), runs a query on each and returns their server connection
// ids. Each is held until all are open, so none is reused for the next.
func runFreshConnections(t *testing.T, app *sql.DB, n int) []*e2eFresh {
	t.Helper()
	var out []*e2eFresh
	var held []*sql.Conn
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	for i := 0; i < n; i++ {
		c, err := app.Conn(context.Background())
		if err != nil {
			t.Fatalf("fresh connection %d: %v", i, err)
		}
		held = append(held, c)
		var id uint32
		if err := c.QueryRowContext(context.Background(), "SELECT CONNECTION_ID()").Scan(&id); err != nil {
			t.Fatalf("fresh connection %d query: %v", i, err)
		}
		out = append(out, &e2eFresh{mysqlConnID: id})
	}
	return out
}

// e2eRelay is the application's path to the server: TCP in, greeting and
// SSLRequest relayed in plaintext, then TLS terminated on both sides and every
// packet relayed in the clear, where the current recording (if any) sees it.
type e2eRelay struct {
	addr string
	ln   net.Listener

	mu    sync.Mutex
	conns int
	rec   *e2eRecording
	// rawDelays and decDelays are how late the raw and the decrypted legs of
	// the next connections reach the recorder, in order; e2eRawLegDelay and
	// no delay once they run out.
	rawDelays, decDelays []time.Duration
}

func (r *e2eRelay) setLegDelays(raw, decrypted []time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rawDelays, r.decDelays = raw, decrypted
}

// nextLegDelays returns the delays of the next connection's two legs.
func (r *e2eRelay) nextLegDelays() (raw, decrypted time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw = e2eRawLegDelay
	if len(r.rawDelays) > 0 {
		raw, r.rawDelays = r.rawDelays[0], r.rawDelays[1:]
	}
	if len(r.decDelays) > 0 {
		decrypted, r.decDelays = r.decDelays[0], r.decDelays[1:]
	}
	return raw, decrypted
}

func startE2ERelay(t *testing.T, serverAddr string, _ int) *e2eRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}
	r := &e2eRelay{addr: ln.Addr().String(), ln: ln}
	cert := e2eSelfSignedCert(t)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serve(t, c, serverAddr, cert)
		}
	}()
	return r
}

func (r *e2eRelay) connections() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns
}

func (r *e2eRelay) current() *e2eRecording {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec
}

func (r *e2eRelay) serve(t *testing.T, client net.Conn, serverAddr string, cert tls.Certificate) {
	defer func() { _ = client.Close() }()
	server, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		t.Errorf("relay dial: %v", err)
		return
	}
	defer func() { _ = server.Close() }()
	r.mu.Lock()
	r.conns++
	r.mu.Unlock()
	// The connection's name on both of its legs, as the capture layer gives
	// each of a proxyless connection's legs its socket cookie.
	connKey := fmt.Sprintf("sk:%d", client.RemoteAddr().(*net.TCPAddr).Port)

	// Plaintext phase: the greeting, then the client's SSLRequest.
	greeting, err := e2eReadPacket(server)
	if err != nil {
		t.Errorf("relay: read greeting: %v", err)
		return
	}
	greetingAt := time.Now()
	if _, err := client.Write(greeting); err != nil {
		return
	}
	sslReq, err := e2eReadPacket(client)
	if err != nil {
		return
	}
	sslReqAt := time.Now()
	if len(sslReq) < 8 || binary.LittleEndian.Uint32(sslReq[4:8])&mysql.CLIENT_SSL == 0 {
		t.Errorf("relay: the client did not ask for TLS")
		return
	}
	if _, err := server.Write(sslReq); err != nil {
		return
	}
	// A connection opened during a recording has a raw leg too.
	rec := r.current()
	var decDelay time.Duration
	if rec != nil {
		var rawDelay time.Duration
		rawDelay, decDelay = r.nextLegDelays()
		rec.rawLeg(connKey, rawDelay, greeting, greetingAt, sslReq, sslReqAt)
	}

	tlsServer := tls.Client(server, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) // #nosec G402 -- test relay to a throwaway server
	tlsClient := tls.Server(client, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	errs := make(chan error, 2)
	go func() { errs <- tlsServer.Handshake() }()
	go func() { errs <- tlsClient.Handshake() }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("relay: TLS handshake: %v", err)
			return
		}
	}

	c := &e2eConn{relay: r, connKey: connKey, decDelay: decDelay}
	done := make(chan struct{}, 2)
	go func() { c.pump(tlsClient, tlsServer, true); done <- struct{}{} }()
	go func() { c.pump(tlsServer, tlsClient, false); done <- struct{}{} }()
	<-done
}

// e2eConn is one relayed connection's decrypted stream as the capture layer
// would deliver it: bound to the recording that was on when its next packet
// was seen, starting at that packet.
type e2eConn struct {
	relay   *e2eRelay
	connKey string
	// decDelay is how late its decrypted stream reaches the recorder.
	decDelay time.Duration
	mu       sync.Mutex
	leg      *e2eLeg
}

func (c *e2eConn) pump(from, to net.Conn, fromClient bool) {
	for {
		pkt, err := e2eReadPacket(from)
		if err != nil {
			_ = to.Close()
			return
		}
		at := time.Now()
		c.mu.Lock()
		rec := c.relay.current()
		if c.leg != nil && (rec == nil || c.leg.rec != rec) {
			c.leg = nil // that recording ended
		}
		if c.leg == nil && fromClient && rec != nil {
			c.leg = rec.decryptedLeg(c.connKey, pkt[3] == 0, c.decDelay)
		}
		if c.leg != nil {
			c.leg.push(fromClient, pkt, at)
		}
		c.mu.Unlock()
		if _, err := to.Write(pkt); err != nil {
			return
		}
	}
}

// e2eRecording is one recording session: a scope, the agent's store, and the
// recorder sessions it started.
type e2eRecording struct {
	relay *e2eRelay
	store *models.TLSHandshakeStore
	dst   *models.ConditionalDstCfg
	scope string
	ctx   context.Context
	stopC context.CancelFunc

	mu   sync.Mutex
	legs []*e2eLeg
	raw  []*e2eRawLeg
	wg   sync.WaitGroup
}

func (r *e2eRelay) startRecording(store *models.TLSHandshakeStore, addr string, port int, scope string) *e2eRecording {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &e2eRecording{
		relay: r,
		store: store,
		dst:   &models.ConditionalDstCfg{Addr: addr, Port: uint(port)},
		scope: scope,
		ctx:   ctx,
		stopC: cancel,
	}
	r.mu.Lock()
	r.rec = rec
	r.mu.Unlock()
	return rec
}

func (rec *e2eRecording) opts(connKey string) models.OutgoingOptions {
	return models.OutgoingOptions{
		DstCfg:           rec.dst,
		PassThroughScope: rec.scope,
		SkipTLSMITM:      true,
		ConnKey:          connKey,
	}
}

// e2eLeg is one recorder session over a decrypted stream.
type e2eLeg struct {
	rec             *e2eRecording
	connKey         string
	joinedMidStream bool
	startedAt       time.Time

	mu       sync.Mutex
	closed   bool
	clientCh chan fakeconn.Chunk
	destCh   chan fakeconn.Chunk
	mocks    chan *models.Mock

	firstQueryAt time.Time
	got          []*models.Mock
	err          error
}

func newE2ELegSession(logger *zap.Logger, opts models.OutgoingOptions, clientCh, destCh chan fakeconn.Chunk, mocks chan *models.Mock, id string) *supervisor.Session {
	return &supervisor.Session{
		ClientStream: fakeconn.New(clientCh, nil, nil),
		DestStream:   fakeconn.New(destCh, nil, nil),
		Directives:   make(chan directive.Directive, 4),
		Acks:         make(chan directive.Ack, 4),
		Mocks:        mocks,
		Logger:       logger,
		Ctx:          context.Background(),
		ClientConnID: id + "-client",
		DestConnID:   id + "-dest",
		Opts:         opts,
	}
}

// decryptedLeg starts a recorder on a connection's decrypted stream, delay
// late; its packets are buffered meanwhile.
func (rec *e2eRecording) decryptedLeg(connKey string, joinedMidStream bool, delay time.Duration) *e2eLeg {
	l := &e2eLeg{
		rec:             rec,
		connKey:         connKey,
		joinedMidStream: joinedMidStream,
		startedAt:       time.Now(),
		clientCh:        make(chan fakeconn.Chunk, 1024),
		destCh:          make(chan fakeconn.Chunk, 1024),
		mocks:           make(chan *models.Mock, 256),
	}
	rec.mu.Lock()
	rec.legs = append(rec.legs, l)
	id := fmt.Sprintf("tls-e2e-%d", len(rec.legs))
	rec.mu.Unlock()
	sess := newE2ELegSession(zap.NewNop(), rec.opts(connKey), l.clientCh, l.destCh, l.mocks, id)
	ctx := context.WithValue(rec.ctx, models.PostTLSModeKey, true)
	ctx = context.WithValue(ctx, models.TLSHandshakeStoreKey, rec.store)
	rec.wg.Add(2)
	go func() {
		defer rec.wg.Done()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-rec.ctx.Done():
			}
		}
		err := RecordV2(ctx, zap.NewNop(), sess)
		l.mu.Lock()
		if l.err == nil {
			l.err = err
		}
		l.mu.Unlock()
		close(l.mocks) // RecordV2 has returned: nothing sends on it any more
	}()
	go func() {
		defer rec.wg.Done()
		for m := range l.mocks {
			l.mu.Lock()
			l.got = append(l.got, m)
			if l.firstQueryAt.IsZero() && e2eIsQuery(m) {
				l.firstQueryAt = time.Now()
			}
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *e2eLeg) push(fromClient bool, pkt []byte, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	ch, dir := l.destCh, fakeconn.FromDest
	if fromClient {
		ch, dir = l.clientCh, fakeconn.FromClient
	}
	select {
	case ch <- fakeconn.Chunk{Dir: dir, Bytes: append([]byte(nil), pkt...), ReadAt: at, WrittenAt: at}:
	default:
		l.err = errors.New("e2e leg buffer full: a packet was dropped")
	}
}

func (l *e2eLeg) closeStreams() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.clientCh)
		close(l.destCh)
	}
}

// e2eRawLeg is one recorder session over a raw (pre-TLS) leg.
type e2eRawLeg struct {
	connKey    string
	greeting   []byte
	greetingAt time.Time
}

// rawLeg delivers a new connection's plaintext greeting and SSLRequest to a raw
// leg recorder, delay late, the way a loaded capture ringbuf does.
func (rec *e2eRecording) rawLeg(connKey string, delay time.Duration, greeting []byte, greetingAt time.Time, sslReq []byte, sslReqAt time.Time) {
	clientCh := make(chan fakeconn.Chunk, 4)
	destCh := make(chan fakeconn.Chunk, 4)
	mocks := make(chan *models.Mock, 4)
	rec.mu.Lock()
	rec.raw = append(rec.raw, &e2eRawLeg{connKey: connKey, greeting: append([]byte(nil), greeting...), greetingAt: greetingAt})
	id := fmt.Sprintf("proxyless-e2e-%d", len(rec.raw))
	rec.mu.Unlock()
	sess := newE2ELegSession(zap.NewNop(), rec.opts(connKey), clientCh, destCh, mocks, id)
	ctx := context.WithValue(rec.ctx, models.TLSHandshakeStoreKey, rec.store)
	rec.wg.Add(1)
	go func() {
		defer rec.wg.Done()
		time.Sleep(delay)
		destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: append([]byte(nil), greeting...), ReadAt: greetingAt, WrittenAt: greetingAt}
		clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: append([]byte(nil), sslReq...), ReadAt: sslReqAt, WrittenAt: sslReqAt}
		_ = RecordV2(ctx, zap.NewNop(), sess)
		close(clientCh)
		close(destCh)
	}()
}

// waitForQueries waits until at least n decrypted legs recorded a query, and
// returns every leg that did.
func (rec *e2eRecording) waitForQueries(t *testing.T, n int, patience time.Duration) []*e2eLeg {
	t.Helper()
	deadline := time.Now().Add(patience)
	for {
		var done []*e2eLeg
		rec.mu.Lock()
		for _, l := range rec.legs {
			l.mu.Lock()
			if !l.firstQueryAt.IsZero() {
				done = append(done, l)
			}
			if l.err != nil && !errors.Is(l.err, io.EOF) {
				t.Errorf("a decrypted leg failed: %v", l.err)
			}
			l.mu.Unlock()
		}
		total := len(rec.legs)
		rec.mu.Unlock()
		if len(done) >= n {
			return done
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %v only %d of %d decrypted legs (of %d started) recorded a query", patience, len(done), n, total)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertFreshConnsStitchedWithTheirOwnGreetings checks each fresh connection's
// decrypted leg, found by the connection it relays, produced a config mock that
// is the connection's own: its greeting (the server's connection id for that
// connection), its SSLRequest ahead of its HandshakeResponse41, and its
// greeting's timestamp.
func (rec *e2eRecording) assertFreshConnsStitchedWithTheirOwnGreetings(t *testing.T, fresh []*e2eFresh) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		rec.mu.Lock()
		legs := append([]*e2eLeg(nil), rec.legs...)
		raws := append([]*e2eRawLeg(nil), rec.raw...)
		rec.mu.Unlock()
		// The connection each fresh leg relays, by its server connection id.
		rawOf := map[string]*e2eRawLeg{}
		for _, r := range raws {
			rawOf[r.connKey] = r
		}
		cfgOf := map[uint32]*models.Mock{}
		for _, l := range legs {
			r := rawOf[l.connKey]
			if l.joinedMidStream || r == nil {
				continue
			}
			l.mu.Lock()
			for _, m := range l.got {
				if m.Name == "config" {
					cfgOf[e2eGreetingConnID(t, r.greeting)] = m
				}
			}
			l.mu.Unlock()
		}
		if len(cfgOf) >= len(fresh) {
			for _, f := range fresh {
				cfg := cfgOf[f.mysqlConnID]
				var own *e2eRawLeg
				for _, r := range raws {
					if e2eGreetingConnID(t, r.greeting) == f.mysqlConnID {
						own = r
					}
				}
				if cfg == nil || own == nil {
					t.Errorf("fresh connection %d: no config mock from its own decrypted leg", f.mysqlConnID)
					continue
				}
				got := e2eConfigGreetingConnID(cfg)
				t.Logf("fresh connection %d: config mock stitched with the greeting of connection %d, %d requests, "+
					"ReqTimestampMock %s", f.mysqlConnID, got, len(cfg.Spec.MySQLRequests),
					cfg.Spec.ReqTimestampMock.Format(time.RFC3339Nano))
				if got != f.mysqlConnID {
					t.Errorf("fresh connection %d was stitched with the greeting of connection %d, not its own",
						f.mysqlConnID, got)
				}
				reqs := cfg.Spec.MySQLRequests
				if len(reqs) < 2 || reqs[0].Header == nil || reqs[0].Header.Type != mysql.SSLRequest ||
					reqs[1].Header == nil || reqs[1].Header.Type != mysql.HandshakeResponse41 {
					t.Errorf("fresh connection %d's config mock does not open with its SSLRequest and "+
						"HandshakeResponse41", f.mysqlConnID)
				}
				if !cfg.Spec.ReqTimestampMock.Equal(own.greetingAt) {
					t.Errorf("fresh connection %d: ReqTimestampMock = %v, want its own greeting's %v",
						f.mysqlConnID, cfg.Spec.ReqTimestampMock, own.greetingAt)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d fresh connections produced a config mock", len(cfgOf), len(fresh))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertEachCaptureStitchesOnlyItsOwnConnection: a raw leg's greeting
// timestamp may appear in the config mock of its own connection's decrypted leg
// and nowhere else. treedb.sameMock identifies a mock by
// Name+Kind+ReqTimestampMock, so one capture stitched into two streams is one
// mock to replay, and the other stream's connection lost its own.
func (rec *e2eRecording) assertEachCaptureStitchesOnlyItsOwnConnection(t *testing.T) {
	t.Helper()
	rec.mu.Lock()
	legs := append([]*e2eLeg(nil), rec.legs...)
	raws := append([]*e2eRawLeg(nil), rec.raw...)
	rec.mu.Unlock()
	captureOf := map[time.Time]string{}
	for _, r := range raws {
		captureOf[r.greetingAt] = r.connKey
	}
	for _, l := range legs {
		l.mu.Lock()
		for _, m := range l.got {
			if m.Name != "config" {
				continue
			}
			if owner, isCapture := captureOf[m.Spec.ReqTimestampMock]; isCapture && owner != l.connKey {
				t.Errorf("connection %s's config mock carries the timestamp of connection %s's captured greeting "+
					"(joined mid-stream: %v): one capture stitched into another connection's mock",
					l.connKey, owner, l.joinedMidStream)
			}
		}
		l.mu.Unlock()
	}
}

// e2eConfigGreetingConnID is the server connection id in a config mock's
// greeting.
func e2eConfigGreetingConnID(m *models.Mock) uint32 {
	for _, r := range m.Spec.MySQLResponses {
		if g, ok := r.Message.(*mysql.HandshakeV10Packet); ok {
			return g.ConnectionID
		}
	}
	return 0
}

// e2eGreetingConnID is the server connection id in a raw greeting packet:
// protocol version, NUL-terminated server version, then the id.
func e2eGreetingConnID(t *testing.T, pkt []byte) uint32 {
	t.Helper()
	p := pkt[4:]
	for i := 1; i < len(p); i++ {
		if p[i] == 0 && i+5 <= len(p) {
			return binary.LittleEndian.Uint32(p[i+1 : i+5])
		}
	}
	t.Fatalf("not a greeting: %x", pkt)
	return 0
}

// stop ends the recording: every leg's streams close, as the capture layer
// closes them at session end, and every recorder returns.
func (rec *e2eRecording) stop(t *testing.T) {
	t.Helper()
	rec.relay.mu.Lock()
	rec.relay.rec = nil
	rec.relay.mu.Unlock()
	rec.mu.Lock()
	legs := append([]*e2eLeg(nil), rec.legs...)
	rec.mu.Unlock()
	for _, l := range legs {
		l.closeStreams()
	}
	finished := make(chan struct{})
	go func() {
		rec.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		rec.stopC()
		t.Fatal("recorders did not return after the recording stopped")
	}
	rec.stopC()
	for _, l := range legs {
		l.mu.Lock()
		err := l.err
		l.mu.Unlock()
		if err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("a decrypted leg ended with %v", err)
		}
	}
}

func e2eIsQuery(m *models.Mock) bool {
	return m != nil && len(m.Spec.MySQLRequests) > 0 && m.Spec.MySQLRequests[0].Header != nil &&
		m.Spec.MySQLRequests[0].Header.Type == "COM_QUERY"
}

// e2eReadPacket reads one MySQL packet (header included).
func e2eReadPacket(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	pkt := make([]byte, 4+n)
	copy(pkt, hdr)
	if _, err := io.ReadFull(r, pkt[4:]); err != nil {
		return nil, err
	}
	return pkt, nil
}

func e2eSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "keploy-e2e-relay"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

var _ = gomysql.ErrInvalidConn // the driver registers itself as "mysql"
