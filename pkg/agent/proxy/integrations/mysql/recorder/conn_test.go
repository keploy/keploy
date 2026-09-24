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

	connPhase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
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

	// Remembered for the next connection: packets only — no SSLRequest was
	// captured, and timing belongs to the connection that uses it.
	key := models.HandshakeLastKey(scope, opts.DstCfg)
	cached, ok := store.Last(key)
	if !ok || len(cached.RespPackets) != 1 || !bytes.Equal(cached.RespPackets[0], greeting) {
		t.Fatalf("fetched greeting not remembered under %q: %+v (found=%v)", key, cached, ok)
	}
	if len(cached.ReqPackets) != 0 || !cached.ReqTimestamp.IsZero() {
		t.Errorf("remembered entry carries per-connection data (req packets %d, timestamp %v); only the greeting may be cached",
			len(cached.ReqPackets), cached.ReqTimestamp)
	}
	if _, found := store.Last(models.HandshakeLastPortKey(scope, 3306)); found {
		t.Error("a fetched greeting was remembered under a port-only key, which names no server")
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
// them still records its command.
//
// Takes ~5s: the fetch is reached only after handlePostTLSRecord's hardcoded
// PopWait on the (empty) port-only key expires.
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
		// Empty: the proxyless decrypted leg cannot know the raw leg's identity.
		ConnKey: "",
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
	wg.Wait()

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

// A raw leg that records its greeting while a fetch is in flight keeps its
// entry: it carries the SSLRequest the seq==0 path needs for a replay-matchable
// config mock, and the fetched one does not. The server withholds its greeting
// until the raw leg's write has landed, so the write is always mid-dial.
func TestFetchServerGreetingShared_KeepsARawLegEntryWrittenDuringTheDial(t *testing.T) {
	greeting := cannedHandshakeV10(t)
	gate := make(chan struct{})
	srv := startGatedFakeMySQLGreeter(t, greeting, 0, gate)
	store := models.NewTLSHandshakeStore()
	opts := models.OutgoingOptions{
		DstCfg:           &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306},
		PassThroughScope: "ns/app/ts0",
	}
	key := models.HandshakeLastKey(opts.PassThroughScope, opts.DstCfg)
	rawLeg := models.TLSHandshakeEntry{
		RespPackets: [][]byte{greeting},
		ReqPackets:  [][]byte{cannedSSLRequest(t, 1)},
	}
	fetched := make(chan error, 1)
	go func() {
		_, errs := fetchConcurrently(1, store, opts)
		fetched <- errs[0]
	}()
	waitAccepted(t, srv, 1)
	store.RememberLast(key, rawLeg) // lands mid-dial
	close(gate)
	if err := <-fetched; err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, ok := store.Last(key)
	if !ok || len(got.ReqPackets) != 1 {
		t.Fatalf("the raw leg's entry (with its SSLRequest) was replaced by the fetched greeting: %+v (found=%v)", got, ok)
	}
}
