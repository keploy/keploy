package recorder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	connPhase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// TestFetchServerGreeting_RefusesFabricatedAddr pins the dial guard: when the
// capture layer marked the destination as a stand-in (AddrFabricated — the
// proxyless SSL-uprobe path's loopback substitution + content-matched port),
// fetchServerGreeting must fail WITHOUT dialing. A live listener on the
// address proves no connection is attempted — dialing a fabricated loopback
// address can reach an unrelated co-resident server and stitch a foreign
// greeting into the mock.
func TestFetchServerGreeting_RefusesFabricatedAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		accepted <- struct{}{}
		_ = conn.Close()
	}()

	opts := models.OutgoingOptions{
		DstCfg: &models.ConditionalDstCfg{Addr: ln.Addr().String(), Port: 3306, AddrFabricated: true},
	}
	buf, err := fetchServerGreeting(context.Background(), zap.NewNop(), opts)
	if err == nil {
		t.Fatal("fetchServerGreeting must refuse a fabricated destination")
	}
	if !strings.Contains(err.Error(), "refusing to dial") {
		t.Errorf("error = %v, want the explicit dial refusal", err)
	}
	if buf != nil {
		t.Errorf("buf = %v, want nil on refusal", buf)
	}
	select {
	case <-accepted:
		t.Fatal("fetchServerGreeting dialed a fabricated address")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestFetchServerGreeting_ReadsRealGreeting proves the guard does not break
// the legitimate fallback (pre-warmed connections whose handshake predates
// interception): a non-fabricated destination is dialed and the greeting
// packet is returned verbatim.
func TestFetchServerGreeting_ReadsRealGreeting(t *testing.T) {
	greeting := cannedHandshakeV10(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		_, _ = conn.Write(greeting)
		_ = conn.Close()
	}()

	opts := models.OutgoingOptions{
		DstCfg: &models.ConditionalDstCfg{Addr: ln.Addr().String(), Port: 3306},
	}
	buf, err := fetchServerGreeting(context.Background(), zap.NewNop(), opts)
	if err != nil {
		t.Fatalf("fetchServerGreeting: %v", err)
	}
	if !bytes.Equal(buf, greeting) {
		t.Fatalf("greeting bytes mismatch: got %d bytes, want %d", len(buf), len(greeting))
	}
}

// fakeMySQLGreeter is a MySQL server as fetchServerGreeting sees it: it greets
// every connection and the dialler hangs up. That is an aborted handshake, and
// a real server counts it against the dialling host (max_connect_errors). The
// accept count is therefore what these tests measure.
type fakeMySQLGreeter struct {
	addr     string
	accepted atomic.Int32
}

// startFakeMySQLGreeter answers each connection with reply after delay (an
// empty reply hangs up without greeting). delay holds the first dial open long
// enough for concurrent callers to pile up on it, which is what distinguishes
// coalescing from "the first finished before the rest looked".
func startFakeMySQLGreeter(t *testing.T, reply []byte, delay time.Duration) *fakeMySQLGreeter {
	t.Helper()
	gate := make(chan struct{})
	close(gate)
	return startGatedFakeMySQLGreeter(t, reply, delay, gate)
}

// startGatedFakeMySQLGreeter is startFakeMySQLGreeter that also withholds the
// reply until gate is closed, so a test can act while a dial is in flight.
func startGatedFakeMySQLGreeter(t *testing.T, reply []byte, delay time.Duration, gate <-chan struct{}) *fakeMySQLGreeter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	g := &fakeMySQLGreeter{addr: ln.Addr().String()}
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			g.accepted.Add(1)
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				time.Sleep(delay)
				<-gate
				if len(reply) == 0 {
					return // hang up without greeting
				}
				_, _ = c.Write(reply)
				// Hold the socket until the dialler hangs up, as a server does.
				_, _ = io.Copy(io.Discard, c)
			}(conn)
		}
	}()
	return g
}

// waitAccepted waits (bounded) until g has accepted at least n connections.
func waitAccepted(t *testing.T, g *fakeMySQLGreeter, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.accepted.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("server accepted %d connections within 5s, want %d", g.accepted.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// shortenGreetingFetchFailureBackoff sets the failure backoff for one test.
func shortenGreetingFetchFailureBackoff(t *testing.T, d time.Duration) {
	t.Helper()
	orig := greetingFetchFailureBackoff
	greetingFetchFailureBackoff = d
	t.Cleanup(func() { greetingFetchFailureBackoff = orig })
}

// cannedBlockedHostERR is the ERR packet MySQL sends in place of a greeting to
// a host that tripped max_connect_errors (ER_HOST_IS_BLOCKED, 1129).
func cannedBlockedHostERR() []byte {
	msg := "Host '10.0.0.1' is blocked because of many connection errors; unblock with 'mysqladmin flush-hosts'"
	payload := append([]byte{0xff, 0x69, 0x04}, []byte(msg)...)
	return append([]byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), 0}, payload...)
}

// fetchConcurrently runs n fetchServerGreetingShared calls at once and returns
// their results.
func fetchConcurrently(n int, store *models.TLSHandshakeStore, opts models.OutgoingOptions) ([][]byte, []error) {
	bufs := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			bufs[i], errs[i] = fetchServerGreetingShared(ctx, zap.NewNop(), store, opts)
		}(i)
	}
	close(start)
	wg.Wait()
	return bufs, errs
}

// Every direct greeting fetch is an aborted handshake the server counts against
// the dialling host. A DaemonSet agent dials from the node, which SNATed pods
// share, so a pool of pre-recording connections dialling once each could block
// the whole node from its database. N concurrent callers for one server must
// cost ONE dial, a later caller none (it reuses the remembered greeting), and a
// second server its own single dial.
func TestFetchServerGreetingShared_OneDialPerServer(t *testing.T) {
	greeting := cannedHandshakeV10(t)
	srv := startFakeMySQLGreeter(t, greeting, 500*time.Millisecond)
	store := models.NewTLSHandshakeStore()
	scope := "ns/app/ts0"
	opts := models.OutgoingOptions{
		DstCfg:           &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306},
		PassThroughScope: scope,
		// The fake server is on loopback, which names a server only within one
		// network namespace; this test process is one.
		NetNS: testNetNS,
	}

	const n = 8
	bufs, errs := fetchConcurrently(n, store, opts)
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if !bytes.Equal(bufs[i], greeting) {
			t.Fatalf("caller %d got %d bytes, want the server's %d-byte greeting", i, len(bufs[i]), len(greeting))
		}
	}
	if got := srv.accepted.Load(); got != 1 {
		t.Fatalf("%d concurrent callers dialled the server %d times, want 1: each dial is an aborted "+
			"handshake counted against the dialling host", n, got)
	}

	// Remembered for the next connection, per SERVER: the greeting bytes only,
	// no SSLRequest (none was captured) and no timing (it belongs to the
	// connection that uses it).
	key := models.HandshakeServerKey(opts.NetNS, opts.DstCfg)
	cached, ok := store.ServerGreeting(key)
	if !ok || !bytes.Equal(cached, greeting) {
		t.Fatalf("fetched greeting not remembered under %q: %x (found=%v)", key, cached, ok)
	}
	if e, _ := store.Last(key); len(e.ReqPackets) != 0 || !e.ReqTimestamp.IsZero() {
		t.Errorf("remembered entry carries per-connection data (req packets %d, timestamp %v); only the greeting may be cached",
			len(e.ReqPackets), e.ReqTimestamp)
	}
	if _, found := store.Last(models.HandshakeLastPortKey(scope, 3306)); found {
		t.Error("a fetched greeting was remembered under a port-only key, which names no server")
	}
	if _, found := store.Last(models.HandshakeLastKey(scope, opts.DstCfg)); found {
		t.Error("a fetched greeting was remembered under the app/session-scoped destination key, " +
			"where only CAPTURED entries (with their SSLRequest) belong")
	}

	// A later caller reuses it.
	if _, errs := fetchConcurrently(1, store, opts); errs[0] != nil {
		t.Fatalf("later caller: %v", errs[0])
	}
	if got := srv.accepted.Load(); got != 1 {
		t.Fatalf("a later caller dialled again (%d dials) instead of reusing the remembered greeting", got)
	}

	// Another server is another destination: its own single dial.
	srv2 := startFakeMySQLGreeter(t, greeting, 200*time.Millisecond)
	opts2 := opts
	opts2.DstCfg = &models.ConditionalDstCfg{Addr: srv2.addr, Port: 3306}
	if _, errs := fetchConcurrently(3, store, opts2); errs[0] != nil || errs[1] != nil || errs[2] != nil {
		t.Fatalf("second server: %v", errs)
	}
	if a, b := srv.accepted.Load(), srv2.accepted.Load(); a != 1 || b != 1 {
		t.Fatalf("dials: first server %d, second server %d; want 1 each", a, b)
	}
}

// A failed dial must not become a cached success. Callers already waiting on
// it share the failure. Callers arriving during the backoff get that failure
// without dialling, since each dial of a firewalled server costs seconds.
// Nothing is remembered, and once the backoff passes the next caller dials
// again. A reply that is not a greeting is a failure too: the ERR packet a
// blocked host gets, or a truncated greeting (which used to panic the decoder,
// and a panic inside the shared fetch cannot be recovered by any caller).
func TestFetchServerGreetingShared_FailureIsNotRemembered(t *testing.T) {
	shortenGreetingFetchFailureBackoff(t, 400*time.Millisecond)
	// A greeting cut inside the 18 fixed bytes after the filler, keeping 5 of
	// them: protocol version, NUL-terminated server version, connection id (4),
	// auth-plugin-data part 1 + filler (9), then 5 bytes.
	payload := cannedHandshakeV10(t)[4:]
	versionEnd := bytes.IndexByte(payload[1:], 0x00) + 1
	truncatedPayload := payload[:versionEnd+1+4+9+5]
	truncated := append([]byte{byte(len(truncatedPayload)), 0, 0, 0}, truncatedPayload...)
	for _, tc := range []struct {
		name  string
		reply []byte
	}{
		{"hang-up without a greeting", nil},
		{"blocked-host ERR packet", cannedBlockedHostERR()},
		{"truncated greeting", truncated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFakeMySQLGreeter(t, tc.reply, 300*time.Millisecond)
			store := models.NewTLSHandshakeStore()
			opts := models.OutgoingOptions{
				DstCfg:           &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306},
				PassThroughScope: "ns/app/ts0",
				NetNS:            testNetNS,
			}
			_, errs := fetchConcurrently(4, store, opts)
			for i, err := range errs {
				if err == nil {
					t.Fatalf("caller %d succeeded against a server that sent no greeting", i)
				}
			}
			if got := srv.accepted.Load(); got != 1 {
				t.Fatalf("concurrent callers of a failing dial dialled %d times, want 1", got)
			}
			if c, ok := store.Last(models.HandshakeLastKey(opts.PassThroughScope, opts.DstCfg)); ok {
				t.Fatalf("a failed fetch was remembered: %+v", c)
			}
			// Within the backoff: the failure again, no dial.
			if _, errs := fetchConcurrently(1, store, opts); errs[0] == nil || !strings.Contains(errs[0].Error(), "not retrying") {
				t.Fatalf("within the backoff: err = %v, want the remembered failure", errs[0])
			}
			if got := srv.accepted.Load(); got != 1 {
				t.Fatalf("a caller within the failure backoff dialled again (%d dials)", got)
			}
			// After it: dial again (and fail again).
			time.Sleep(450 * time.Millisecond)
			if _, errs := fetchConcurrently(1, store, opts); errs[0] == nil || strings.Contains(errs[0].Error(), "not retrying") {
				t.Fatalf("after the backoff: err = %v, want a fresh failed dial", errs[0])
			}
			if got := srv.accepted.Load(); got != 2 {
				t.Fatalf("after the backoff the next caller must dial again: %d dials, want 2", got)
			}
		})
	}
}

// A caller whose connection is already torn down never starts a dial: recording
// stop is not a reason to reach the server.
func TestFetchServerGreetingShared_CancelledCallerNeverDials(t *testing.T) {
	srv := startFakeMySQLGreeter(t, cannedHandshakeV10(t), 0)
	store := models.NewTLSHandshakeStore()
	opts := models.OutgoingOptions{
		DstCfg:           &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306},
		PassThroughScope: "ns/app/ts0",
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchServerGreetingShared(ctx, zap.NewNop(), store, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := srv.accepted.Load(); got != 0 {
		t.Fatalf("a torn-down caller dialled the server %d times", got)
	}
}

// A fabricated destination is never dialled, however many callers ask, and
// nothing is remembered under its (stand-in) address.
func TestFetchServerGreetingShared_FabricatedDestNeverDials(t *testing.T) {
	srv := startFakeMySQLGreeter(t, cannedHandshakeV10(t), 0)
	store := models.NewTLSHandshakeStore()
	opts := models.OutgoingOptions{
		DstCfg:           &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306, AddrFabricated: true},
		PassThroughScope: "ns/app/ts0",
	}
	_, errs := fetchConcurrently(4, store, opts)
	for i, err := range errs {
		if err == nil || !strings.Contains(err.Error(), "refusing to dial") {
			t.Fatalf("caller %d: err = %v, want the fabricated-address refusal", i, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := srv.accepted.Load(); got != 0 {
		t.Fatalf("a fabricated destination was dialled %d times", got)
	}
	if c, ok := store.Last(models.HandshakeLastKey(opts.PassThroughScope, opts.DstCfg)); ok {
		t.Fatalf("something was remembered for a fabricated destination: %+v", c)
	}
}

// One caller's teardown must not fail the others sharing its dial.
func TestFetchServerGreetingShared_LeaderCancelDoesNotFailWaiters(t *testing.T) {
	greeting := cannedHandshakeV10(t)
	srv := startFakeMySQLGreeter(t, greeting, 500*time.Millisecond)
	store := models.NewTLSHandshakeStore()
	opts := models.OutgoingOptions{
		DstCfg:           &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306},
		PassThroughScope: "ns/app/ts0",
		NetNS:            testNetNS,
	}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := fetchServerGreetingShared(leaderCtx, zap.NewNop(), store, opts)
		leaderErr <- err
	}()
	// Let the leader's dial start, then join it and tear the leader down.
	waitAccepted(t, srv, 1)
	waiter := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		buf, err := fetchServerGreetingShared(ctx, zap.NewNop(), store, opts)
		if err == nil && !bytes.Equal(buf, greeting) {
			err = errors.New("wrong greeting bytes")
		}
		waiter <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled leader returned %v, want context.Canceled", err)
	}
	if err := <-waiter; err != nil {
		t.Fatalf("a waiter failed because the connection that started the dial was torn down: %v", err)
	}
	if got := srv.accepted.Load(); got != 1 {
		t.Fatalf("%d dials, want 1", got)
	}
}

// The legacy post-TLS reader shares the same fetch: N pre-recording pooled
// connections with nothing captured cost the server ONE dial, and every one of
// them still records its command, promptly. Each used to wait out
// handlePostTLSRecord's 5s PopWait on the (empty) port-only key first, for a
// greeting that a connection joined mid-stream never gets.
func TestLegacyPostTLS_ConcurrentPooledConnsDialTheServerOnce(t *testing.T) {
	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connPhase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}
	srv := startFakeMySQLGreeter(t, handshakeBuf, time.Second)
	store := models.NewTLSHandshakeStore()
	mocks := make(chan *models.Mock, 64)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(mocks)

	opts := models.OutgoingOptions{
		DstCfg:           &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306},
		PassThroughScope: "ns/app/ts0",
		NetNS:            testNetNS,
	}

	query := cannedCOMQuery(t, 0, "SELECT 1")
	okResp := cannedOK(t, 1, greeting.CapabilityFlags)
	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		clientConn, clientPeer := net.Pipe()
		destConn, destPeer := net.Pipe()
		t.Cleanup(func() {
			_ = clientConn.Close()
			_ = destConn.Close()
		})
		// A pooled connection joined mid-stream: one command, then it idles out.
		go func() {
			_, _ = clientPeer.Write(query)
			buf := make([]byte, 4096)
			_, _ = destPeer.Read(buf) // the recorder forwards the command
			_, _ = destPeer.Write(okResp)
			_, _ = clientPeer.Read(buf) // and the response back
			_ = clientPeer.Close()
			_ = destPeer.Close()
		}()
		ctx := context.WithValue(postTLSCtxWithStore(store), models.ClientConnectionIDKey, fmt.Sprintf("pooled-%d", i))
		ctx = syncMock.NewContext(ctx, mgr)
		wg.Add(1)
		go func(i int, ctx context.Context) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			errs[i] = handlePostTLSRecord(cctx, zap.NewNop(), clientConn, destConn, mocks,
				buildPostHandshakeDecodeCtx(clientConn), opts)
		}(i, ctx)
	}
	start := time.Now()
	wg.Wait()
	// The fake server holds its greeting for 1s so the callers pile up on one
	// dial; anything near 5s is the PopWait again.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("pooled connections took %v to record; they waited for a greeting that cannot come", elapsed)
	}

	for i, err := range errs {
		if err != nil && strings.Contains(err.Error(), "direct fetch failed") {
			t.Fatalf("connection %d could not get a greeting: %v", i, err)
		}
	}
	if got := srv.accepted.Load(); got != 1 {
		t.Fatalf("%d pooled connections dialled the server %d times, want 1", n, got)
	}
	close(mocks)
	queries := 0
	for m := range mocks {
		if m != nil && len(m.Spec.MySQLRequests) > 0 && m.Spec.MySQLRequests[0].Header != nil &&
			m.Spec.MySQLRequests[0].Header.Type == "COM_QUERY" {
			queries++
		}
	}
	if queries != n {
		t.Fatalf("recorded %d COM_QUERY mocks, want %d — every connection sharing the one fetched greeting must still decode its command", queries, n)
	}
}

// A greeting a raw leg captures while a fetch is in flight is newer evidence of
// what the server is, and the fetch must not overwrite it. The server withholds
// its greeting until the raw leg's write has landed, so the write is always
// mid-dial.
func TestFetchServerGreetingShared_KeepsARawLegGreetingWrittenDuringTheDial(t *testing.T) {
	fetched := cannedHandshakeV10Variant(t, "8.0.36-fetched", 7)
	live := cannedHandshakeV10Variant(t, "8.0.36-live", 8)
	gate := make(chan struct{})
	srv := startGatedFakeMySQLGreeter(t, fetched, 0, gate)
	store := models.NewTLSHandshakeStore()
	opts := models.OutgoingOptions{
		DstCfg:           &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306},
		PassThroughScope: "ns/app/ts0",
		NetNS:            testNetNS,
	}
	fetchedCh := make(chan error, 1)
	go func() {
		_, errs := fetchConcurrently(1, store, opts)
		fetchedCh <- errs[0]
	}()
	waitAccepted(t, srv, 1)
	ctx := context.WithValue(context.Background(), models.TLSHandshakeStoreKey, store)
	rememberServerGreeting(ctx, opts, live, greetingServerIdentity(decodeGreeting(t, live))) // lands mid-dial
	close(gate)
	if err := <-fetchedCh; err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got, ok := store.ServerGreeting(greetingMemoKey(opts)); !ok || !bytes.Equal(got, live) {
		t.Fatalf("the raw leg's live greeting was replaced by the fetched one: %x (found=%v)", got, ok)
	}
}

// testNetNS is the NetNS token for connections made by this test process: the
// fake servers listen on loopback, which names a server only within one network
// namespace, and every connection here is made from the same one.
const testNetNS = "netns:test-process"

// cannedHandshakeV10Variant is cannedHandshakeV10 with a chosen server version
// and connection id, so a test can tell which greeting a connection was given.
func cannedHandshakeV10Variant(t *testing.T, serverVersion string, connID uint32) []byte {
	t.Helper()
	caps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SSL | mysql.CLIENT_SECURE_CONNECTION)
	hs := &mysql.HandshakeV10Packet{
		ProtocolVersion: 0x0a,
		ServerVersion:   serverVersion,
		ConnectionID:    connID,
		AuthPluginData:  bytes.Repeat([]byte{0x22}, 20),
		CapabilityFlags: caps,
		CharacterSet:    0x21,
		StatusFlags:     0x02,
		AuthPluginName:  string(mysql.Native),
	}
	buf, err := connPhase.EncodeHandshakeV10(context.Background(), zap.NewNop(), hs)
	if err != nil {
		t.Fatalf("encode handshake v10: %v", err)
	}
	return wrapPacket(buf, 0)
}

// redirectDialer is a models.DstDialer that reaches srv whatever address it is
// asked for, counting the calls. It lets a test give a connection a ROUTABLE
// destination (10.0.0.5:3306, a pod IP) that no test can listen on, and still
// have the greeting fetch reach a fake server.
func redirectDialer(srv *fakeMySQLGreeter, calls *atomic.Int32) models.DstDialer {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		calls.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.addr)
	}
}

// One greeting dial per SERVER, not per recording session. The enterprise
// DaemonSet scope is "<ns>/<deployment>/<test-set>", so a key that includes it
// re-learned the same server's greeting, with an aborted handshake counted
// against the node, at every recording session. The handshake store outlives
// sessions (one per agent), so the memo can too, for exactly the addresses
// that name one server everywhere. A namespace-local address names a different
// server in every pod, and stays per namespace.
func TestFetchServerGreetingShared_OneDialPerServerAcrossSessions(t *testing.T) {
	type conn struct {
		addr  string
		scope string
		netNS string
	}
	for _, tc := range []struct {
		name  string
		conns []conn
		dials int32
	}{
		{
			name: "routable server, two recording sessions of one app",
			conns: []conn{
				{"10.0.0.5:3306", "ns/app/test-set-0", ""},
				{"10.0.0.5:3306", "ns/app/test-set-1", ""},
			},
			dials: 1,
		},
		{
			name: "routable server, two apps in two pods",
			conns: []conn{
				{"10.0.0.5:3306", "ns-a/app-a/test-set-0", "netns:pod-a"},
				{"10.0.0.5:3306", "ns-b/app-b/test-set-3", "netns:pod-b"},
			},
			dials: 1,
		},
		{
			name: "loopback server, two sessions in one pod",
			conns: []conn{
				{"127.0.0.1:3306", "ns/app/test-set-0", "netns:pod-a"},
				{"127.0.0.1:3306", "ns/app/test-set-1", "netns:pod-a"},
			},
			dials: 1,
		},
		{
			name: "loopback servers in two pods are two servers",
			conns: []conn{
				{"127.0.0.1:3306", "ns/app/test-set-0", "netns:pod-a"},
				{"127.0.0.1:3306", "ns/app/test-set-0", "netns:pod-b"},
			},
			dials: 2,
		},
		{
			// No namespace to tell pods apart, and the app/session scope does
			// not: every replica of a deployment shares it, each with its own
			// loopback server. Nothing is shared between connections at all.
			name: "loopback server, namespace unknown, two replicas of one app",
			conns: []conn{
				{"127.0.0.1:3306", "ns/app/test-set-0", ""},
				{"127.0.0.1:3306", "ns/app/test-set-0", ""},
			},
			dials: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFakeMySQLGreeter(t, cannedHandshakeV10(t), 0)
			var calls atomic.Int32
			ctx := context.WithValue(context.Background(), models.DstDialerKey, redirectDialer(srv, &calls))
			store := models.NewTLSHandshakeStore() // one per agent, shared by every session
			for i, c := range tc.conns {
				opts := models.OutgoingOptions{
					DstCfg:           &models.ConditionalDstCfg{Addr: c.addr, Port: 3306},
					PassThroughScope: c.scope,
					NetNS:            c.netNS,
				}
				cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				buf, err := fetchServerGreetingShared(cctx, zap.NewNop(), store, opts)
				cancel()
				if err != nil || len(buf) == 0 {
					t.Fatalf("connection %d: %v", i, err)
				}
			}
			if got := srv.accepted.Load(); got != tc.dials {
				t.Fatalf("%d greeting dials, want %d", got, tc.dials)
			}
			if got := calls.Load(); got != tc.dials {
				t.Fatalf("%d dials went through the connection's dialer, want %d", got, tc.dials)
			}
		})
	}
}

// A greeting a raw leg captures from a live connection is the server as it is
// NOW. It replaces a fetched one, so the next pooled connection is served the
// live greeting with no dial. A raw greeting that does not decode as a
// HandshakeV10 is never remembered.
func TestRememberServerGreeting_LiveReplacesFetchedAndUndecodableIsIgnored(t *testing.T) {
	fetched := cannedHandshakeV10Variant(t, "8.0.36-fetched", 7)
	live := cannedHandshakeV10Variant(t, "8.0.36-live", 8)
	srv := startFakeMySQLGreeter(t, fetched, 0)
	store := models.NewTLSHandshakeStore()
	opts := models.OutgoingOptions{
		DstCfg:           &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306},
		PassThroughScope: "ns/app/test-set-0",
		NetNS:            testNetNS,
	}
	ctx := context.WithValue(context.Background(), models.TLSHandshakeStoreKey, store)
	if _, err := fetchServerGreetingShared(ctx, zap.NewNop(), store, opts); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	key := greetingMemoKey(opts)
	if g, _ := store.ServerGreeting(key); !bytes.Equal(g, fetched) {
		t.Fatalf("memo after the fetch = %x, want the fetched greeting", g)
	}

	// A raw leg of ANOTHER session of another app captures the live greeting.
	liveOpts := opts
	liveOpts.PassThroughScope = "ns/other-app/test-set-4"
	rememberServerGreeting(ctx, liveOpts, live, greetingServerIdentity(decodeGreeting(t, live)))
	if g, _ := store.ServerGreeting(key); !bytes.Equal(g, live) {
		t.Fatalf("memo after a live capture = %x, want the live greeting", g)
	}
	g, err := fetchServerGreetingShared(ctx, zap.NewNop(), store, opts)
	if err != nil || !bytes.Equal(g, live) {
		t.Fatalf("next pooled connection got %x (%v), want the live greeting", g, err)
	}
	if got := srv.accepted.Load(); got != 1 {
		t.Fatalf("%d dials, want the 1 initial fetch", got)
	}

	// Only a greeting its caller decoded as a HandshakeV10 is remembered: a
	// failed decode (no packet) and bytes that are not a greeting are ignored.
	payload := live[4:]
	liveHS := decodeGreeting(t, live)
	for name, tc := range map[string]struct {
		buf []byte
		hs  *mysql.HandshakeV10Packet
	}{
		"blocked-host ERR, decode failed": {cannedBlockedHostERR(), nil},
		"truncated, decode failed":        {append([]byte{byte(len(payload) - 30), 0, 0, 0}, payload[:len(payload)-30]...), nil},
		"ERR bytes with a stray packet":   {cannedBlockedHostERR(), liveHS},
		"header only with a stray packet": {[]byte{0x00, 0x00, 0x00, 0x00}, liveHS},
		"a greeting whose decode failed":  {cannedHandshakeV10Variant(t, "8.0.36-other", 12), nil},
	} {
		rememberServerGreeting(ctx, opts, tc.buf, greetingServerIdentity(tc.hs))
		if g, _ := store.ServerGreeting(key); !bytes.Equal(g, live) {
			t.Fatalf("%s: replaced the memo: %x", name, g)
		}
	}
	// Nor anything for a stand-in address.
	fab := opts
	fab.DstCfg = &models.ConditionalDstCfg{Addr: "127.0.0.1:3306", Port: 3306, AddrFabricated: true}
	rememberServerGreeting(ctx, fab, live, greetingServerIdentity(liveHS))
	if k := greetingMemoKey(fab); k != "" {
		t.Fatalf("a fabricated destination has a memo key %q", k)
	}
}

// decodeGreeting decodes a greeting packet the way the raw paths do.
func decodeGreeting(t *testing.T, buf []byte) *mysql.HandshakeV10Packet {
	t.Helper()
	hs, err := connPhase.DecodeHandshakeV10(context.Background(), zap.NewNop(), buf[4:])
	if err != nil {
		t.Fatalf("decode greeting: %v", err)
	}
	return hs
}

// The greeting fetch for a connection made from another network namespace goes
// through the dialer its capture layer supplied. The enterprise DaemonSet agent
// is hostNetwork, so a pod's 127.0.0.1:3306 dialled from the agent is the NODE's
// loopback: a different server or nothing. Without a dialer it dials as before.
func TestFetchServerGreeting_DialsThroughTheConnectionsDialer(t *testing.T) {
	greeting := cannedHandshakeV10(t)
	srv := startFakeMySQLGreeter(t, greeting, 0)
	var calls atomic.Int32
	opts := models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: "127.0.0.1:1", Port: 3306}}
	ctx := context.WithValue(context.Background(), models.DstDialerKey, redirectDialer(srv, &calls))
	buf, err := fetchServerGreeting(ctx, zap.NewNop(), opts)
	if err != nil || !bytes.Equal(buf, greeting) {
		t.Fatalf("fetch through the dialer: %x, %v", buf, err)
	}
	if calls.Load() != 1 || srv.accepted.Load() != 1 {
		t.Fatalf("dialer calls %d, server accepts %d; want 1 and 1", calls.Load(), srv.accepted.Load())
	}
	// No dialer: 127.0.0.1:1 is dialled from here, and refused.
	if _, err := fetchServerGreeting(context.Background(), zap.NewNop(), opts); err == nil {
		t.Fatal("without a dialer the fetch reached something other than the requested address")
	}
	if calls.Load() != 1 {
		t.Fatal("a fetch with no dialer in its context used one")
	}
}

// The legacy raw path records the greeting it captured as the server's too, so
// the V2 and legacy post-TLS readers learn the server from either path. And it
// queues the capture ONCE, under its connection's identity, for that
// connection's decrypted stream.
func TestLegacyRawLegRemembersTheServerGreeting(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	live := cannedHandshakeV10Variant(t, "8.0.36-live", 11)
	opts := models.OutgoingOptions{
		DstCfg:           &models.ConditionalDstCfg{Addr: "10.244.0.24:3306", Port: 3306},
		PassThroughScope: "ns/app/test-set-0",
		SkipTLSMITM:      true,
		ConnKey:          "sk:11",
		ConnProc:         "pid:100",
	}
	clientConn, clientPeer := net.Pipe()
	destConn, destPeer := net.Pipe()
	defer func() { _ = clientConn.Close(); _ = destConn.Close() }()
	go func() {
		_, _ = destPeer.Write(live)
		buf := make([]byte, 256)
		_, _ = clientPeer.Read(buf) // the greeting, relayed to the client
		_, _ = clientPeer.Write(cannedSSLRequest(t, 1))
		_, _ = destPeer.Read(buf) // the SSLRequest, relayed to the server
	}()
	ctx := context.WithValue(context.Background(), models.TLSHandshakeStoreKey, store)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := handleInitialHandshake(ctx, zap.NewNop(), clientConn, destConn, &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
	}, opts, nil)
	if err != nil || !res.skipConfigMock {
		t.Fatalf("legacy raw leg: skipConfigMock=%v err=%v", res.skipConfigMock, err)
	}
	other := opts
	other.PassThroughScope = "ns/other/test-set-5"
	if g, ok := store.ServerGreeting(greetingMemoKey(other)); !ok || !bytes.Equal(g, live) {
		t.Fatalf("server greeting after the legacy raw leg = %x (found=%v), want the captured one", g, ok)
	}
	// Its greeting is queued once, for its own connection's decrypted stream.
	portKey := models.HandshakeStoreKey("", 3306)
	e, owner, ok := store.PopWaitFor(portKey, models.HandshakeOwnerOf(opts), true, 0)
	if !ok || owner != models.HandshakeOwnerOf(opts) || !bytes.Equal(e.RespPackets[0], live) {
		t.Fatalf("queued entry for the connection = %v (owner %v, %v), want its greeting under its identity", e, owner, ok)
	}
	if extra, ok := store.PopWait(portKey, 0); ok {
		t.Fatalf("the capture was queued twice: %v is left for another stream to take", extra)
	}
}

// Every recorded MySQL handshake reports its server's greeting to the memo
// (rememberServerGreeting), so that report sits on the hot path of every
// connection. It takes the caller's decoding instead of decoding again, and a
// server already known is not rewritten.
func BenchmarkRememberServerGreeting(b *testing.B) {
	g := cannedHandshakeV10(&testing.T{})
	hs, err := connPhase.DecodeHandshakeV10(context.Background(), zap.NewNop(), g[4:])
	if err != nil {
		b.Fatal(err)
	}
	id := greetingServerIdentity(hs) // as the caller has it
	store := models.NewTLSHandshakeStore()
	ctx := context.WithValue(context.Background(), models.TLSHandshakeStoreKey, store)
	opts := models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: "10.0.0.5:3306", Port: 3306}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rememberServerGreeting(ctx, opts, g, id)
	}
}

// The handshake queue's Push/PopWait, on which post-TLS streams wait, while
// eight goroutines report greetings as fast as they can: the memo has its own
// lock, so the reports do not slow the queue.
func BenchmarkQueueUnderHandshakes(b *testing.B) {
	g := cannedHandshakeV10(&testing.T{})
	hs, err := connPhase.DecodeHandshakeV10(context.Background(), zap.NewNop(), g[4:])
	if err != nil {
		b.Fatal(err)
	}
	id := greetingServerIdentity(hs) // as the caller has it
	store := models.NewTLSHandshakeStore()
	ctx := context.WithValue(context.Background(), models.TLSHandshakeStoreKey, store)
	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			opts := models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: fmt.Sprintf("10.0.0.%d:3306", w+1), Port: 3306}}
			for !stop.Load() {
				rememberServerGreeting(ctx, opts, g, id)
			}
		}(w)
	}
	e := models.TLSHandshakeEntry{RespPackets: [][]byte{g}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Push("port:3306", e)
		store.PopWait("port:3306", 0)
	}
	b.StopTimer()
	stop.Store(true)
	wg.Wait()
}

// The app-scoped last-greeting cache key carries the connection's network
// namespace for an observed namespace-local address: the scope is shared by
// every replica of a deployment, and each replica's 127.0.0.1:3306 is its own
// sidecar. Without a namespace there is no key. A routable address and a
// capture-layer stand-in keep their per-scope keys.
func TestLastGreetingKey(t *testing.T) {
	const scope = "ns/app/test-set-0"
	dst := func(addr string, fab bool) *models.ConditionalDstCfg {
		return &models.ConditionalDstCfg{Addr: addr, Port: 3306, AddrFabricated: fab}
	}
	loop := dst("127.0.0.1:3306", false)
	if a, b := lastGreetingKey(scope, "netns:A", loop), lastGreetingKey(scope, "netns:B", loop); a == "" || a == b {
		t.Fatalf("two replicas' loopback servers: keys %q and %q, want two distinct keys", a, b)
	}
	for _, addr := range []string{"127.0.0.1:3306", "[::1]:3306", "169.254.1.1:3306", "mysql:3306"} {
		if k := lastGreetingKey(scope, "", dst(addr, false)); k != "" {
			t.Errorf("%s with no namespace: key %q, want none", addr, k)
		}
	}
	routable := dst("10.0.0.5:3306", false)
	if got, want := lastGreetingKey(scope, "netns:A", routable), models.HandshakeLastKey(scope, routable); got != want {
		t.Errorf("routable: key %q, want the per-scope %q", got, want)
	}
	stand := dst("127.0.0.1:3306", true)
	if got, want := lastGreetingKey(scope, "", stand), models.HandshakeLastKey(scope, stand); got != want {
		t.Errorf("stand-in: key %q, want the per-scope bucket %q", got, want)
	}
}

// A dialer that cannot reach its own connection's namespace (its process is
// gone, the agent may not enter it) says nothing about the server. The flight
// for a server is shared by every pod that reaches it, so that failure must
// neither be remembered for the server nor end the fetch for a caller that
// only shared the flight: they dial with their own dialers.
func TestFetchServerGreetingShared_CallerSpecificDialFailureIsNotAServerFailure(t *testing.T) {
	greeting := cannedHandshakeV10(t)
	opts := models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: "10.0.0.5:3306", Port: 3306}}
	unavailable := func(delay time.Duration) models.DstDialer {
		return func(context.Context, string) (net.Conn, error) {
			time.Sleep(delay)
			return nil, fmt.Errorf("pod A's namespace: %w", models.ErrDstDialerUnavailable)
		}
	}
	fetch := func(dial models.DstDialer, store *models.TLSHandshakeStore) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), models.DstDialerKey, dial), 5*time.Second)
		defer cancel()
		return fetchServerGreetingShared(ctx, zap.NewNop(), store, opts)
	}

	t.Run("the next caller is not refused", func(t *testing.T) {
		srv := startFakeMySQLGreeter(t, greeting, 0)
		var calls atomic.Int32
		store := models.NewTLSHandshakeStore()
		if _, err := fetch(unavailable(0), store); !errors.Is(err, models.ErrDstDialerUnavailable) {
			t.Fatalf("pod A: err = %v, want its dialer's", err)
		}
		g, err := fetch(redirectDialer(srv, &calls), store)
		if err != nil || !bytes.Equal(g, greeting) {
			t.Fatalf("pod B was refused after pod A's dialer failed: %v", err)
		}
		if srv.accepted.Load() != 1 {
			t.Fatalf("%d dials, want pod B's 1", srv.accepted.Load())
		}
	})

	t.Run("a caller that shared the failed flight dials with its own dialer", func(t *testing.T) {
		srv := startFakeMySQLGreeter(t, greeting, 0)
		var calls atomic.Int32
		store := models.NewTLSHandshakeStore()
		leaderErr := make(chan error, 1)
		go func() {
			_, err := fetch(unavailable(300*time.Millisecond), store)
			leaderErr <- err
		}()
		time.Sleep(50 * time.Millisecond) // pod B joins pod A's flight
		g, err := fetch(redirectDialer(srv, &calls), store)
		if err != nil || !bytes.Equal(g, greeting) {
			t.Fatalf("pod B shared pod A's flight and was failed with pod A's error: %v", err)
		}
		if err := <-leaderErr; !errors.Is(err, models.ErrDstDialerUnavailable) {
			t.Fatalf("pod A: err = %v, want its dialer's", err)
		}
		if srv.accepted.Load() != 1 || calls.Load() != 1 {
			t.Fatalf("%d dials through %d dialer calls, want pod B's 1", srv.accepted.Load(), calls.Load())
		}
	})
}
