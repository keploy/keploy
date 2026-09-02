package recorder

import (
	"bytes"
	"context"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// These tests pin the post-TLS stitch resilience added for the
// e2e-mysql-tls-lowlatency-cycle "(2a) capture shortfall — served but NOT
// recorded" failure: the raw (pre-TLS) leg's greeting push shares the capture
// ringbuf with bulk traffic and, under CPU pressure, lands seconds late or is
// dropped outright. The old code popped the stash with a fixed 5s timeout and,
// on a miss, dialed opts.DstCfg.Addr — which on the degraded proxyless path is
// a fabricated 127.0.0.1:3306 — so a merely-late stash became a total capture
// loss for the connection (COM_QUERY never decoded although the app was
// served).

// postTLSCtxWithStore wires the context the way the SSL/GoTLS reader callback
// does for a decrypted tls-* stream, but with a caller-owned store so tests
// control exactly what (and when) the raw leg stashed.
func postTLSCtxWithStore(store *models.TLSHandshakeStore) context.Context {
	ctx := context.WithValue(context.Background(), models.PostTLSModeKey, true)
	return context.WithValue(ctx, models.TLSHandshakeStoreKey, store)
}

// collectPostTLSMocks drives RecordV2 under ctx and collects want mocks, with
// caller-controlled patience (the late-stash test needs >6s).
func collectPostTLSMocks(t *testing.T, h *v2Harness, ctx context.Context, want int, patience time.Duration) []*models.Mock {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		cctx, cancel := context.WithTimeout(ctx, patience)
		defer cancel()
		done <- RecordV2(cctx, h.logger, h.sess)
	}()
	var got []*models.Mock
	for len(got) < want {
		select {
		case m, ok := <-h.mocks:
			if !ok {
				t.Fatalf("mocks channel closed early (got %d, want %d)", len(got), want)
			}
			got = append(got, m)
			if len(got) == want {
				h.closeStreams()
			}
		case <-time.After(patience):
			t.Fatalf("timed out waiting for mocks (got %d, want %d)", len(got), want)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("RecordV2 (post-TLS) returned error: %v", err)
	}
	return got
}

// TestRecordV2_PostTLS_LateStash_StillRecords is the permanent regression test
// for the 5s cliff: the raw leg's greeting+SSLRequest push lands 6s after the
// decrypted stream started — past the old PopWait(5s)+fallback(2s) budget that
// aborted the connection ("no greeting in store" → ECONNREFUSED on the
// fabricated dial → command phase never reached). With the ctx/TTL-bounded
// wait, lateness within the store TTL must yield a complete recording.
//
// Deliberately not parallel: it must observe the DEFAULT stash-wait bound
// (other tests in this file shorten it via t.Setenv).
func TestRecordV2_PostTLS_LateStash_StillRecords(t *testing.T) {
	h := newV2Harness(t)
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}
	sslReq := cannedSSLRequest(t, 1)

	// Decrypted stream: HandshakeResponse41 (seq>=1), auth OK, one query.
	h.pushClient(cannedHandshakeResponse41(t, 2, false), base.Add(5*time.Millisecond))
	h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), base.Add(10*time.Millisecond))
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), base.Add(20*time.Millisecond))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(25*time.Millisecond))

	// Raw leg: the stash arrives 6 seconds late — after the old cliff.
	store := models.NewTLSHandshakeStore()
	const lateBy = 6 * time.Second
	go func() {
		time.Sleep(lateBy)
		store.Push(models.HandshakeStoreKey("", 3306), models.TLSHandshakeEntry{
			RespPackets:  [][]byte{handshakeBuf},
			ReqPackets:   [][]byte{sslReq},
			ReqTimestamp: base,
		})
	}()

	start := time.Now()
	got := collectPostTLSMocks(t, h, postTLSCtxWithStore(store), 2, 25*time.Second)
	if elapsed := time.Since(start); elapsed < lateBy {
		t.Fatalf("mocks arrived after %v — before the stash was even pushed at %v; the test is not exercising the late path", elapsed, lateBy)
	}

	cfg := got[0]
	if cfg.Name != "config" {
		t.Fatalf("first mock = %q, want config", cfg.Name)
	}
	if len(cfg.Spec.MySQLResponses) < 1 {
		t.Fatal("config mock has no responses — greeting was not seeded from the late stash")
	}
	if len(cfg.Spec.MySQLRequests) < 2 {
		t.Errorf("config mock requests = %d, want >=2 (SSLRequest + HandshakeResponse41)", len(cfg.Spec.MySQLRequests))
	}
	// The entry is THIS connection's own raw leg, so its stamped ReqTimestamp
	// must be preserved (no stale-cache zeroing on this path).
	if !cfg.Spec.ReqTimestampMock.Equal(base) {
		t.Errorf("config ReqTimestampMock = %v, want %v (the raw-leg stash timestamp)", cfg.Spec.ReqTimestampMock, base)
	}
	// The command phase must have been recorded — this is exactly what the old
	// cliff lost.
	assertQueryMock(t, got[1])
}

// TestRecordV2_PostTLS_LostStash_GreetingCacheFallback covers the raw leg
// being genuinely LOST (its capture event dropped by a full ringbuf): the
// consumable queue never gets this connection's entry, but an earlier
// connection to the same port populated the store's last-greeting cache. The
// stitch must reuse that greeting — greetings are per-server except the salt,
// which nothing on the record/replay path verifies — instead of dialing the
// (fabricated) destination or aborting.
func TestRecordV2_PostTLS_LostStash_GreetingCacheFallback(t *testing.T) {
	// Shorten the bounded wait so the test exercises the post-wait fallback
	// quickly. t.Setenv also forbids t.Parallel, which keeps the env change
	// isolated from the default-bound test above.
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "150")

	h := newV2Harness(t)
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}
	sslReq := cannedSSLRequest(t, 1)

	// An EARLIER connection pushed and consumed its entry; only the
	// last-greeting cache retains it. Its (stale) timestamp must NOT leak
	// into this connection's mock.
	store := models.NewTLSHandshakeStore()
	staleTs := base.Add(-time.Hour)

	// The degraded proxyless dest is fabricated — the fallback must succeed
	// WITHOUT dialing it (a dial would fail: nothing listens on this port).
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: "127.0.0.1:1", Port: 3306, AddrFabricated: true}

	// The earlier connection was this SAME app reaching the SAME destination, so
	// it shares the scope+address key and its greeting is reusable here. Only the
	// queue entry was consumed; the cache retains it.
	store.RememberLast(models.HandshakeLastKey(h.sess.Opts.PassThroughScope, h.sess.Opts.DstCfg),
		models.TLSHandshakeEntry{
			RespPackets:  [][]byte{handshakeBuf},
			ReqPackets:   [][]byte{sslReq},
			ReqTimestamp: staleTs,
		})

	clientTs := base.Add(5 * time.Millisecond)
	h.pushClient(cannedHandshakeResponse41(t, 2, false), clientTs)
	h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), base.Add(10*time.Millisecond))
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), base.Add(20*time.Millisecond))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(25*time.Millisecond))

	got := collectPostTLSMocks(t, h, postTLSCtxWithStore(store), 2, 10*time.Second)

	cfg := got[0]
	if cfg.Name != "config" {
		t.Fatalf("first mock = %q, want config", cfg.Name)
	}
	if len(cfg.Spec.MySQLResponses) < 1 {
		t.Fatal("config mock has no responses — cached greeting was not used")
	}
	if len(cfg.Spec.MySQLRequests) < 2 {
		t.Errorf("config mock requests = %d, want >=2 (cached SSLRequest + HandshakeResponse41)", len(cfg.Spec.MySQLRequests))
	}
	if cfg.Spec.ReqTimestampMock.Equal(staleTs) {
		t.Errorf("config ReqTimestampMock = %v — the stale cached timestamp leaked; it must be re-sampled from this connection's first client read", cfg.Spec.ReqTimestampMock)
	}
	if !cfg.Spec.ReqTimestampMock.Equal(clientTs) {
		t.Errorf("config ReqTimestampMock = %v, want %v (sampled from the first client read)", cfg.Spec.ReqTimestampMock, clientTs)
	}
	assertQueryMock(t, got[1])
}

// TestRecordV2_PostTLS_LostStash_FabricatedDest_NoDialCleanAbort covers the
// worst case: the stash never arrives AND nothing has populated the greeting
// cache. With a fabricated destination the recorder must abort cleanly —
// bounded time, an explicit error, no mock — and it must NEVER dial the
// fabricated address (a live listener stands in for the "unrelated
// co-resident server" a real dial could reach).
func TestRecordV2_PostTLS_LostStash_FabricatedDest_NoDialCleanAbort(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "150")

	h := newV2Harness(t)

	// A real listener on the fabricated address: if the recorder dials it, the
	// accept below proves the regression.
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
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: ln.Addr().String(), Port: 3306, AddrFabricated: true}

	store := models.NewTLSHandshakeStore() // stays empty: raw leg lost, no cache

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(postTLSCtxWithStore(store), 10*time.Second)
		defer cancel()
		done <- RecordV2(ctx, h.logger, h.sess)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RecordV2 must return an error when the stash is lost, the cache is empty, and the dest is fabricated")
		}
		if !strings.Contains(err.Error(), "no greeting in store") {
			t.Errorf("error = %v, want it to name the missing greeting", err)
		}
		if !strings.Contains(err.Error(), "refusing to dial") {
			t.Errorf("error = %v, want the fabricated-address dial refusal as the cause", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RecordV2 hung — the lost-stash abort must be bounded")
	}

	select {
	case <-accepted:
		t.Fatal("the recorder dialed the fabricated address — it must never dial a capture-layer stand-in")
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case m := <-h.mocks:
		t.Fatalf("unexpected mock emitted on the clean-abort path: %+v", m)
	default:
	}
}

// TestResolvePreTLSGreeting_CtxCancelUnblocks pins one half of the wait bound:
// teardown (ctx cancellation) must unblock the wait promptly even though the
// overall budget is 30s and no greeting is cached (so the loop is in its
// extended no-cache wait).
func TestResolvePreTLSGreeting_CtxCancelUnblocks(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, source := resolvePreTLSGreeting(ctx, store, "", 3306, "", testDst("10.0.0.5:3306"))
	elapsed := time.Since(start)
	if source != greetingNone {
		t.Fatalf("resolvePreTLSGreeting must return greetingNone on an empty store, got %v", source)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("returned after %v — ctx cancellation must cut the 30s bound short (poll slice is %v)", elapsed, mysqlPostTLSStashPollSlice)
	}
}

// TestResolvePreTLSGreeting_DroppedLegHitsCacheAtPrimaryBound is the regression
// test for finding C: a genuinely-DROPPED raw leg whose port already has a
// cached greeting must fall back to that cache at the SHORT primary bound, NOT
// after the full (large) overall bound — otherwise the stall pushes the
// COM_QUERY decode past the CI (2a) 30s gate. Here the overall bound stays at
// its 30s default while the primary bound is shortened to 200ms; the cache hit
// must land near 200ms, far below both the overall bound and any plausible
// gate.
func TestResolvePreTLSGreeting_DroppedLegHitsCacheAtPrimaryBound(t *testing.T) {
	// Shorten only the PRIMARY bound; leave the overall (30s default) alone so
	// the test proves the fast path returns at primary, not overall. t.Setenv
	// also forbids t.Parallel.
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_PRIMARY_MS", "200")

	store := models.NewTLSHandshakeStore()
	// A sibling connection populated the port's last-greeting cache; THIS
	// connection's own raw leg was dropped (never pushed).
	store.RememberLast("dst:|10.0.0.5:3306", models.TLSHandshakeEntry{
		RespPackets:  [][]byte{[]byte("greeting")},
		ReqPackets:   [][]byte{[]byte("sslreq")},
		ReqTimestamp: time.Now(),
	})

	start := time.Now()
	entry, source := resolvePreTLSGreeting(context.Background(), store, "", 3306, "", testDst("10.0.0.5:3306"))
	elapsed := time.Since(start)
	if source != greetingCached {
		t.Fatalf("source = %v, want greetingCached (the dropped-leg cache fallback)", source)
	}
	if len(entry.RespPackets) != 1 {
		t.Fatalf("cached entry not returned: %+v", entry)
	}
	// Must land at ~the 200ms primary bound, decisively below the 30s overall
	// bound. 5s is a generous ceiling that still proves it did not wait overall.
	if elapsed > 5*time.Second {
		t.Fatalf("cache fallback took %v — it must fire at the primary bound (200ms), not the 30s overall bound", elapsed)
	}
}

// TestResolvePreTLSGreeting_OwnLatePreferredOverCache pins that a merely-late
// own leg arriving within the primary bound is preferred over the cache (it
// carries this connection's real salt+timestamp): with the cache populated but
// the own entry pushed shortly after start, resolve must return greetingOwn.
func TestResolvePreTLSGreeting_OwnLatePreferredOverCache(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_PRIMARY_MS", "3000")

	store := models.NewTLSHandshakeStore()
	// Cache is populated (a sibling), but the own leg is merely late.
	store.RememberLast("dst:|10.0.0.5:3306", models.TLSHandshakeEntry{
		RespPackets: [][]byte{[]byte("sibling-greeting")},
	})
	ownTs := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	go func() {
		time.Sleep(150 * time.Millisecond)
		store.Push(models.HandshakeStoreKey("", 3306), models.TLSHandshakeEntry{
			RespPackets:  [][]byte{[]byte("own-greeting")},
			ReqTimestamp: ownTs,
		})
	}()

	entry, source := resolvePreTLSGreeting(context.Background(), store, "", 3306, "", testDst("10.0.0.5:3306"))
	if source != greetingOwn {
		t.Fatalf("source = %v, want greetingOwn (a late own leg within the primary bound beats the cache)", source)
	}
	if !entry.ReqTimestamp.Equal(ownTs) {
		t.Errorf("ReqTimestamp = %v, want the own leg's %v", entry.ReqTimestamp, ownTs)
	}
}

// testDst builds a resolved (non-fabricated) destination.
func testDst(addr string) *models.ConditionalDstCfg {
	return &models.ConditionalDstCfg{Addr: addr, Port: 3306}
}

// The cross-connection greeting fallback exists because a MySQL server's
// capability flags, protocol version and auth plugin are per-SERVER. That is
// precisely why it must never be served across servers. Under a DaemonSet one
// agent serves every pod on the node, so two apps talking to different MySQL
// servers both see destination port 3306; keyed by port alone, the second would
// stitch the first's greeting into its own config mock.
func TestResolvePreTLSGreeting_DoesNotServeOneServersGreetingToAnother(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	store.RememberLast("dst:|10.0.0.5:3306", models.TLSHandshakeEntry{
		RespPackets: [][]byte{{0x0a, 'A'}},
	})

	// A different server, same port. Past the primary bound the fallback would
	// fire if the cache were port-keyed.
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "0")
	_, source := resolvePreTLSGreeting(context.Background(), store, "", 3306, "", testDst("10.0.0.9:3306"))
	if source == greetingCached {
		t.Fatal("served server A's greeting to a connection destined for server B; the cache must be " +
			"scoped to a server, not to a port")
	}
}

// The DaemonSet hazard: one store shared across every app on a node. Two apps
// whose destinations are BOTH unresolvable present the same placeholder address,
// so only the app/session scope keeps them apart. Without it, app B stitches app
// A's server greeting into its own config mock.
func TestResolvePreTLSGreeting_DoesNotServeOneAppsGreetingToAnother(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	fabricated := func() *models.ConditionalDstCfg {
		return &models.ConditionalDstCfg{Addr: "127.0.0.1:3306", Port: 3306, AddrFabricated: true}
	}
	store.RememberLast(models.HandshakeLastKey("ns/app-a/test-set-0", fabricated()),
		models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})

	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "0")

	// App B, same placeholder address, different scope: must NOT borrow.
	if _, source := resolvePreTLSGreeting(context.Background(), store, "", 3306,
		"ns/app-b/test-set-0", fabricated()); source == greetingCached {
		t.Fatal("served app A's greeting to app B; a store shared across a node must isolate by scope")
	}

	// App A itself must still get the fallback — this is the case the feature
	// exists for, and a fabricated address must not disable it.
	if _, source := resolvePreTLSGreeting(context.Background(), store, "", 3306,
		"ns/app-a/test-set-0", fabricated()); source != greetingCached {
		t.Fatalf("app A lost its own cached fallback (source=%v); the fabricated proxyless "+
			"destination is exactly the case this fallback was built for", source)
	}
}

// HandshakeLastKey is the whole guard, so pin its contract directly.
func TestHandshakeLastKey_OnlyNamesResolvedDestinations(t *testing.T) {
	if got := models.HandshakeLastKey("", nil); got != "" {
		t.Fatalf("nil DstCfg = %q, want empty", got)
	}
	if got := models.HandshakeLastKey("", &models.ConditionalDstCfg{Port: 3306}); got != "" {
		t.Fatalf("no address = %q, want empty", got)
	}
	// A fabricated address is still keyable: the scope is what isolates apps,
	// and refusing here would disable the fallback in the proxyless case it
	// exists to serve.
	if got := models.HandshakeLastKey("s", &models.ConditionalDstCfg{Addr: "127.0.0.1:3306", AddrFabricated: true}); got != "dst:s|127.0.0.1:3306" {
		t.Fatalf("fabricated address = %q, want dst:s|127.0.0.1:3306", got)
	}
	// Different scopes must never collide on one address.
	a := models.HandshakeLastKey("app-a", &models.ConditionalDstCfg{Addr: "127.0.0.1:3306"})
	b := models.HandshakeLastKey("app-b", &models.ConditionalDstCfg{Addr: "127.0.0.1:3306"})
	if a == b {
		t.Fatalf("two apps collided on one key (%q)", a)
	}
}

// TestStorePreTLSHandshakeV2_SeedsLastGreetingCache pins that the V2 writer
// populates the last-greeting cache, under BOTH the address key and the
// port-scoped key.
//
// Regression: it previously seeded only the two CONSUMABLE keys. The only
// RememberLast caller in the package was the legacy handleInitialHandshake,
// which the proxyless V2 flow never runs — so resolvePreTLSGreeting's
// cachedGreeting fallback was dead code on every proxyless MySQL-over-TLS
// recording, and a connection whose own entry had been consumed lost its whole
// command phase.
func TestStorePreTLSHandshakeV2_SeedsLastGreetingCache(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	h := newV2Harness(t)
	const scope = "ns/app/test-set-0"
	h.sess.Opts.PassThroughScope = scope
	h.sess.Opts.ConnKey = "conn-abc"
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: "10.244.0.24:3306", Port: 3306}

	greeting := cannedHandshakeV10(t)
	sslReq := cannedSSLRequest(t, 1)
	ts := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	if err := storePreTLSHandshakeV2(postTLSCtxWithStore(store), zap.NewNop(), h.sess, greeting, sslReq, ts, "server-A"); err != nil {
		t.Fatalf("storePreTLSHandshakeV2: %v", err)
	}

	if _, ok := store.Last(models.HandshakeLastKey(scope, h.sess.Opts.DstCfg)); !ok {
		t.Error("address-keyed last-greeting cache not seeded by the V2 writer")
	}
	if _, ok := store.Last(models.HandshakeLastPortKey(scope, 3306)); !ok {
		t.Error("port-keyed last-greeting cache not seeded by the V2 writer — " +
			"a decrypted leg with an unresolved destination can never find the greeting")
	}
}

// TestHandshakeLastPortKey_ScopeIsolatedAndPortNamed pins the key contract: it
// isolates by scope (so a shared DaemonSet store cannot serve app A's greeting
// to app B) and returns "" when there is no port to key on.
func TestHandshakeLastPortKey_ScopeIsolatedAndPortNamed(t *testing.T) {
	if got := models.HandshakeLastPortKey("s", 0); got != "" {
		t.Errorf("HandshakeLastPortKey(_, 0) = %q, want \"\" (no port to key on)", got)
	}
	a := models.HandshakeLastPortKey("ns/app-a/ts0", 3306)
	b := models.HandshakeLastPortKey("ns/app-b/ts0", 3306)
	if a == b {
		t.Errorf("two apps on the same port share key %q — cross-app greeting contamination", a)
	}
	if a == "" {
		t.Error("HandshakeLastPortKey returned empty for a valid scope+port")
	}
	if same := models.HandshakeLastPortKey("ns/app-a/ts0", 3306); same != a {
		t.Errorf("key is not stable: %q vs %q", same, a)
	}
}

// TestRecordV2_PostTLS_PooledConn_BridgesRealWriterToFabricatedReader is the
// end-to-end regression for the proxyless "served but NOT recorded" capture
// shortfall.
//
// Shape, exactly as it occurs in production: an earlier connection's raw
// plaintext leg recorded the greeting against the REAL destination and its
// consumable entries were then popped (PopWait consumes). A pooled connection
// is now reused; its decrypted leg is fd-less, so the capture layer could not
// resolve a destination and presents a synthesized stand-in. The greeting must
// still be found — via the port, the one thing both legs agree on.
//
// Before the fix this produced no mock at all: nothing had seeded the cache,
// and the address keys could not have matched even if it had.
func TestRecordV2_PostTLS_PooledConn_BridgesRealWriterToFabricatedReader(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "150")

	store := models.NewTLSHandshakeStore()
	const scope = "ns/app/test-set-0"
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}
	sslReq := cannedSSLRequest(t, 1)

	// The EARLIER connection: raw leg seen against the real server address.
	writer := newV2Harness(t)
	writer.sess.Opts.PassThroughScope = scope
	writer.sess.Opts.ConnKey = "earlier-conn"
	writer.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: "10.244.0.24:3306", Port: 3306}
	if err := storePreTLSHandshakeV2(postTLSCtxWithStore(store), zap.NewNop(), writer.sess,
		handshakeBuf, sslReq, base.Add(-time.Hour), "server-A"); err != nil {
		t.Fatalf("seed via storePreTLSHandshakeV2: %v", err)
	}
	// ...and that connection consumed both of its queue entries, leaving only
	// the last-greeting cache behind.
	for _, k := range []string{
		models.HandshakeStoreKey("earlier-conn", 3306),
		models.HandshakeStoreKey("", 3306),
	} {
		for {
			if _, ok := store.PopWait(k, 0); !ok {
				break
			}
		}
	}

	// The pooled connection's decrypted leg: destination unresolved, so the
	// address is a stand-in that matches nothing the writer stored.
	h := newV2Harness(t)
	h.sess.Opts.PassThroughScope = scope
	h.sess.Opts.ConnKey = "pooled-conn"
	// The realistic proxyless stand-in, not a contrived one: an unresolved
	// destination is reported as loopback on the content-matched well-known
	// port. It differs from the writer's REAL address, which is the whole
	// reason an address-keyed cache cannot bridge the two legs.
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: "127.0.0.1:3306", Port: 3306, AddrFabricated: true}

	// A pooled connection joins MID-STREAM: it handshook cycles ago, so there is
	// no HandshakeResponse41 on this leg — the first client packet is a command
	// at seq==0. (The earlier version of this test pushed an HR41 at seq 2 and
	// so exercised the fresh-auth branch, not the pooled shape it claims.)
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), base.Add(20*time.Millisecond))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(25*time.Millisecond))

	got := collectPostTLSMocks(t, h, postTLSCtxWithStore(store), 2, 10*time.Second)

	cfg := got[0]
	if cfg.Name != "config" {
		t.Fatalf("first mock = %q, want config", cfg.Name)
	}
	if len(cfg.Spec.MySQLResponses) < 1 {
		t.Fatal("config mock has no responses — the greeting was not recovered across the address mismatch")
	}
	// A seq==0 config mock must carry a synthesized HandshakeResponse41 or the
	// replayer cannot match the connection — recovering the greeting is only
	// useful if the mock it produces is actually replayable.
	if !configMockHasHR41(cfg) {
		t.Errorf("seq==0 config mock has no HandshakeResponse41; it would fail replay handshake matching (reqs=%d)",
			len(cfg.Spec.MySQLRequests))
	}
	assertQueryMock(t, got[1])
}

// TestResolvePreTLSGreeting_PortKeyRefusesCrossServer covers the cross-server
// case AT THE RESOLVER, with a scope set. The existing sibling test passes
// scope="" and so never exercised the port key at all — which is how a
// cross-server hole reached review. Two servers on 3306 in one scope must latch
// the port key unusable rather than let the second borrow the first's greeting.
func TestResolvePreTLSGreeting_PortKeyRefusesCrossServer(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "0")
	const scope = "ns/app/ts0"
	store := models.NewTLSHandshakeStore()
	portKey := models.HandshakeLastPortKey(scope, 3306)

	store.RememberLastForPort(portKey, "v10|8.4.0|caps=1|cs=255|caching_sha2_password",
		models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	// A genuinely DIFFERENT server on the same port — different version, caps
	// and auth plugin. (Note an identical-version replica would legitimately
	// share a fingerprint and must NOT latch: the reused fields are exactly the
	// fingerprinted ones, so reuse between them is safe.)
	store.RememberLastForPort(portKey, "v10|5.7.44|caps=2|cs=33|mysql_native_password",
		models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'B'}}})

	fabricated := &models.ConditionalDstCfg{Addr: "127.0.0.1:3306", Port: 3306, AddrFabricated: true}
	if _, source := resolvePreTLSGreeting(context.Background(), store, "", 3306, scope, fabricated); source == greetingCached {
		t.Error("resolver served a greeting from a port key that has seen two servers — " +
			"the borrower would stitch the wrong capability flags into its config mock")
	}
}

// TestResolvePreTLSGreeting_UnscopedSidecarStillRecovers pins that the fallback
// works for a caller that was never given a scope — the classic sidecar, and
// every OSS deployment, since nothing in this repo sets PassThroughScope. An
// earlier revision disabled the port key on an empty scope, which silently made
// the whole fix enterprise-only while its tests still passed by hand-setting the
// field.
func TestResolvePreTLSGreeting_UnscopedSidecarStillRecovers(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "0")
	store := models.NewTLSHandshakeStore()
	// Writer: the raw leg, real resolved destination, no scope.
	store.RememberLastForPort(models.HandshakeLastPortKey("", 3306), "10.244.0.24:3306",
		models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})

	// Reader: the decrypted leg, destination unresolved, so its address is a
	// stand-in that cannot match the writer's address key.
	fabricated := &models.ConditionalDstCfg{Addr: "127.0.0.1:3306", Port: 3306, AddrFabricated: true}
	entry, source := resolvePreTLSGreeting(context.Background(), store, "", 3306, "", fabricated)
	if source != greetingCached {
		t.Fatalf("unscoped caller did not recover the greeting (source=%v); the fix would be inert "+
			"in every OSS deployment", source)
	}
	if len(entry.RespPackets) == 0 || entry.RespPackets[0][1] != 'A' {
		t.Errorf("recovered the wrong entry: %q", entry.RespPackets)
	}
}

// TestResolvePreTLSGreeting_ResolvedReaderIgnoresPortKey pins that a connection
// which KNOWS its own destination never accepts an answer from a key that names
// no server.
//
// Regression: the port key was consulted first, unconditionally. A resolved leg
// talking to server B, whose own entry had been consumed, was handed server A's
// greeting — and the ambiguity latch could not help, because it only trips once
// B's own raw leg writes the key, which by construction had not happened (that
// is why the fallback was reached at all). The result was a config mock carrying
// another server's capability flags and auth plugin: a WRONG mock in place of a
// missing one. Before the port key existed this case correctly fell through and
// the recorder fetched B's real greeting.
func TestResolvePreTLSGreeting_ResolvedReaderIgnoresPortKey(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "0")
	for _, scope := range []string{"", "ns/app/ts0"} {
		t.Run("scope="+scope, func(t *testing.T) {
			store := models.NewTLSHandshakeStore()
			// Server A's raw leg populated the port key.
			store.RememberLastForPort(models.HandshakeLastPortKey(scope, 3306), "10.0.0.5:3306",
				models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})

			// A different server, whose destination this leg resolved perfectly.
			resolved := &models.ConditionalDstCfg{Addr: "10.0.0.9:3306", Port: 3306}
			if _, source := resolvePreTLSGreeting(context.Background(), store, "", 3306, scope, resolved); source == greetingCached {
				t.Error("a leg that knows its own destination was served another server's greeting " +
					"from the port key; it must fall through and fetch its own")
			}
		})
	}
}

// TestGreetingServerIdentity pins the fingerprint that the whole cross-server
// guard rests on. It had no direct test: setting it to "" left every test green
// while making the fix INERT (RememberLastForPort drops an empty identity, so
// the port key is never written), and adding a per-connection field left every
// test green while making the fallback PERMANENTLY DEAD (every connection to one
// server looks like a new server, latching the key forever).
func TestGreetingServerIdentity(t *testing.T) {
	base := func() *mysql.HandshakeV10Packet {
		return &mysql.HandshakeV10Packet{
			ProtocolVersion: 10,
			ServerVersion:   "8.4.0",
			ConnectionID:    1234,
			AuthPluginData:  []byte("per-connection-salt-aaaaaaaa"),
			CapabilityFlags: 0xC00FFFFF,
			CharacterSet:    255,
			StatusFlags:     0x0002,
			AuthPluginName:  "caching_sha2_password",
		}
	}
	if got := greetingServerIdentity(base()); got == "" {
		t.Fatal("empty identity for a valid greeting — RememberLastForPort drops it and the fix is inert")
	}
	if got := greetingServerIdentity(nil); got != "" {
		t.Errorf("nil greeting = %q, want empty", got)
	}

	// PER-CONNECTION fields must NOT appear: the same server answers every
	// connection with a fresh connection id, salt and status flags.
	t.Run("stable across connections to one server", func(t *testing.T) {
		a := base()
		b := base()
		b.ConnectionID = 99999
		b.AuthPluginData = []byte("a-completely-different-salt-b")
		b.StatusFlags = 0x0022
		if greetingServerIdentity(a) != greetingServerIdentity(b) {
			t.Errorf("identity changed across two connections to the SAME server:\n a=%s\n b=%s\n"+
				"every connection would latch the key and the fallback dies permanently",
				greetingServerIdentity(a), greetingServerIdentity(b))
		}
	})

	// SERVER-STABLE fields MUST appear: these are exactly what a borrowed
	// greeting contributes to a config mock.
	for _, tc := range []struct {
		name string
		mut  func(*mysql.HandshakeV10Packet)
	}{
		{"server version", func(p *mysql.HandshakeV10Packet) { p.ServerVersion = "5.7.44" }},
		{"capability flags", func(p *mysql.HandshakeV10Packet) { p.CapabilityFlags = 0x000FFFFF }},
		{"auth plugin", func(p *mysql.HandshakeV10Packet) { p.AuthPluginName = "mysql_native_password" }},
		{"protocol version", func(p *mysql.HandshakeV10Packet) { p.ProtocolVersion = 9 }},
		{"charset", func(p *mysql.HandshakeV10Packet) { p.CharacterSet = 33 }},
	} {
		t.Run("distinguishes "+tc.name, func(t *testing.T) {
			a := base()
			b := base()
			tc.mut(b)
			if greetingServerIdentity(a) == greetingServerIdentity(b) {
				t.Errorf("%s does not affect the identity; two genuinely different servers would "+
					"share a port key and cross-serve greetings", tc.name)
			}
		})
	}
}

// TestResolvePreTLSGreeting_LatchedPortKeyDeclinesAddressBucket covers the case
// the sibling cross-server test does not: a latched port key AND a populated
// shared address bucket. Without the IsAmbiguous decline the resolver falls
// through to that bucket — which carries no server identity at all — moments
// after the guarded key established that reuse here is unsafe.
func TestResolvePreTLSGreeting_LatchedPortKeyDeclinesAddressBucket(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "0")
	const scope = "ns/app/ts0"
	store := models.NewTLSHandshakeStore()
	fab := &models.ConditionalDstCfg{Addr: "127.0.0.1:3306", Port: 3306, AddrFabricated: true}

	// Two servers seen on this scope+port -> latched.
	pk := models.HandshakeLastPortKey(scope, 3306)
	store.RememberLastForPort(pk, "v10|8.4.0|caps=1|cs=255|caching_sha2_password",
		models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	store.RememberLastForPort(pk, "v10|5.7.44|caps=2|cs=33|mysql_native_password",
		models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'B'}}})
	if !store.IsAmbiguous(pk) {
		t.Fatal("precondition: the port key should be latched")
	}
	// ...and the shared fabricated-address bucket is populated, exactly as
	// storePreTLSHandshakeV2 does for a fabricated destination.
	store.RememberLast(models.HandshakeLastKey(scope, fab),
		models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'C'}}})

	if _, source := resolvePreTLSGreeting(context.Background(), store, "", 3306, scope, fab); source == greetingCached {
		t.Error("served from the identity-less address bucket after the port key was latched; " +
			"a latch is proof that this scope+port has more than one server")
	}
}

// TestRecordV2_RawLegSeedsPortKeyFromGreeting drives the REAL raw plaintext leg
// end to end and asserts the port key gets seeded.
//
// This is the gap a unit test of greetingServerIdentity cannot close: replacing
// the call site with `serverID = ""` leaves the fingerprint function perfect and
// every other test green, while RememberLastForPort silently drops the empty
// identity — so the port key is never written and the whole fix is inert on the
// exact path it targets. That is the failure mode of three previous rounds,
// reachable by deleting one line.
func TestRecordV2_RawLegSeedsPortKeyFromGreeting(t *testing.T) {
	const scope = "ns/app/test-set-0"
	store := models.NewTLSHandshakeStore()

	h := newV2Harness(t)
	h.sess.Opts.SkipTLSMITM = true // observe-only: stash the handshake and stop
	h.sess.Opts.PassThroughScope = scope
	h.sess.Opts.ConnKey = "raw-leg-conn"
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: "10.244.0.24:3306", Port: 3306}

	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	h.pushDest(cannedHandshakeV10(t), base)
	h.pushClient(cannedSSLRequest(t, 1), base.Add(2*time.Millisecond))

	// The RAW leg, so the store must be in ctx WITHOUT PostTLSModeKey — that key
	// selects the decrypted-leg path instead.
	rawCtx := context.WithValue(context.Background(), models.TLSHandshakeStoreKey, store)
	ctx, cancel := context.WithTimeout(rawCtx, 10*time.Second)
	defer cancel()
	// The raw leg stops after stashing; RecordV2 returning is the success path.
	done := make(chan error, 1)
	go func() { done <- RecordV2(ctx, h.logger, h.sess) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RecordV2 did not return on the raw leg")
	}

	if _, ok := store.Last(models.HandshakeLastPortKey(scope, 3306)); !ok {
		t.Error("the raw leg did not seed the PORT key — greetingServerIdentity produced no " +
			"identity, so RememberLastForPort dropped it and the fallback is inert")
	}
	if _, ok := store.Last(models.HandshakeLastKey(scope, h.sess.Opts.DstCfg)); !ok {
		t.Error("the raw leg did not seed the address key")
	}
}

// greetingWithVersion builds a HandshakeV10 packet identical to cannedHandshakeV10
// except for the server version, so a test can present two genuinely DIFFERENT
// servers (or the same one twice) to the raw leg.
func greetingWithVersion(t *testing.T, version string) []byte {
	t.Helper()
	caps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH |
		mysql.CLIENT_SSL | mysql.CLIENT_SECURE_CONNECTION)
	hs := &mysql.HandshakeV10Packet{
		ProtocolVersion: 0x0a,
		ServerVersion:   version,
		ConnectionID:    42,
		AuthPluginData:  bytes.Repeat([]byte{0x11}, 20),
		CapabilityFlags: caps,
		CharacterSet:    255,
		StatusFlags:     2,
		AuthPluginName:  "caching_sha2_password",
	}
	buf, err := connphase.EncodeHandshakeV10(context.Background(), zap.NewNop(), hs)
	if err != nil {
		t.Fatalf("encode handshake: %v", err)
	}
	return wrapPacket(buf, 0)
}

// rawLeg drives one raw plaintext leg through RecordV2 with the given greeting.
func rawLeg(t *testing.T, store *models.TLSHandshakeStore, scope, connKey, addr, greeting string) {
	t.Helper()
	h := newV2Harness(t)
	h.sess.Opts.SkipTLSMITM = true
	h.sess.Opts.PassThroughScope = scope
	h.sess.Opts.ConnKey = connKey
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: addr, Port: 3306}
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	h.pushDest(greetingWithVersion(t, greeting), base)
	h.pushClient(cannedSSLRequest(t, 1), base.Add(2*time.Millisecond))
	ctx, cancel := context.WithTimeout(
		context.WithValue(context.Background(), models.TLSHandshakeStoreKey, store), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RecordV2(ctx, h.logger, h.sess) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RecordV2 did not return on the raw leg")
	}
}

// TestRawLegIdentityDrivesTheLatch pins the whole chain end to end: call site ->
// fingerprint -> latch. Testing greetingServerIdentity in isolation is not
// enough — replacing the call site with a CONSTANT identity leaves that unit
// test perfect and the suite green, while the latch can then never fire and two
// different servers cross-serve greetings, which is the corruption the latch
// exists to prevent.
func TestRawLegIdentityDrivesTheLatch(t *testing.T) {
	const scope = "ns/app/test-set-0"
	pk := models.HandshakeLastPortKey(scope, 3306)

	t.Run("two different servers latch the port key", func(t *testing.T) {
		store := models.NewTLSHandshakeStore()
		rawLeg(t, store, scope, "conn-a", "10.0.0.5:3306", "8.4.0")
		rawLeg(t, store, scope, "conn-b", "10.0.0.9:3306", "5.7.44")
		if !store.IsAmbiguous(pk) {
			t.Error("two servers with different versions did not latch the port key; the identity " +
				"reaching the store is not derived from the greeting")
		}
	})

	t.Run("the same server twice does NOT latch", func(t *testing.T) {
		store := models.NewTLSHandshakeStore()
		// Same server, two connections — in k8s these routinely arrive from
		// different pod IPs after a rollout.
		rawLeg(t, store, scope, "conn-a", "10.244.0.24:3306", "8.4.0")
		rawLeg(t, store, scope, "conn-b", "10.244.0.31:3306", "8.4.0")
		if store.IsAmbiguous(pk) {
			t.Error("the same server on two addresses latched the key — the identity is " +
				"address-derived, which kills the fallback after any rollout")
		}
		if _, ok := store.Last(pk); !ok {
			t.Error("the port key holds nothing after two writes from one server")
		}
	})
}

// TestResolvePreTLSGreeting_PortKeyWinsOverSharedAddressBucket pins the LOOKUP
// ORDER. The address key for a fabricated reader is the shared
// dst:<scope>|127.0.0.1:3306 placeholder, which RememberLast writes UNTAGGED, so
// it can never latch and can hold any server's greeting. Consulting it first
// bypasses the identity latch entirely.
func TestResolvePreTLSGreeting_PortKeyWinsOverSharedAddressBucket(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "0")
	const scope = "ns/app/ts0"
	store := models.NewTLSHandshakeStore()
	fab := &models.ConditionalDstCfg{Addr: "127.0.0.1:3306", Port: 3306, AddrFabricated: true}

	// Port key: server A, tagged and unlatched (the guarded answer).
	store.RememberLastForPort(models.HandshakeLastPortKey(scope, 3306),
		"v10|8.4.0|caps=1|cs=255|caching_sha2_password",
		models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
	// Shared placeholder bucket: a DIFFERENT server, untagged.
	store.RememberLast(models.HandshakeLastKey(scope, fab),
		models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'B'}}})

	entry, source := resolvePreTLSGreeting(context.Background(), store, "", 3306, scope, fab)
	if source != greetingCached {
		t.Fatalf("no cached greeting served (source=%v)", source)
	}
	if len(entry.RespPackets) == 0 || entry.RespPackets[0][1] != 'A' {
		t.Errorf("served %q from the untagged shared bucket; the identity-guarded port key must "+
			"be consulted first or the latch is bypassed", entry.RespPackets)
	}
}

// TestResolvePreTLSGreeting_GateHandlesNilAndEmptyAddr pins the two conjuncts of
// the resolved-reader gate that no test covered. dst==nil is reachable:
// handlePostTLSHandshakeV2 guards DstCfg != nil separately and then passes the
// possibly-nil pointer down, so removing the check turns a reorder into a panic.
func TestResolvePreTLSGreeting_GateHandlesNilAndEmptyAddr(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "0")
	const scope = "ns/app/ts0"
	seed := func() *models.TLSHandshakeStore {
		s := models.NewTLSHandshakeStore()
		s.RememberLastForPort(models.HandshakeLastPortKey(scope, 3306), "srv-A",
			models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'A'}}})
		return s
	}
	// A nil DstCfg must not panic, and cannot be a "resolved" reader.
	if _, source := resolvePreTLSGreeting(context.Background(), seed(), "", 3306, scope, nil); source != greetingCached {
		t.Errorf("nil DstCfg: source = %v, want the port key to answer", source)
	}
	// A DstCfg with no address is likewise unresolved.
	noAddr := &models.ConditionalDstCfg{Port: 3306}
	if _, source := resolvePreTLSGreeting(context.Background(), seed(), "", 3306, scope, noAddr); source != greetingCached {
		t.Errorf("empty Addr: source = %v, want the port key to answer", source)
	}
}

// TestLegacyPostTLSReadsLastGreetingCache pins that the LEGACY path
// (KEPLOY_NEW_RELAY=off) also consults the last-greeting cache.
//
// That path is the documented escape hatch our own error messages tell users to
// pull when a V2 parser misbehaves — proxy_v2.go, util.go and directive_proc.go
// all name it — so it cannot simply be deleted. But handleInitialHandshake
// SEEDS the cache there while the post-TLS reader only ever did PopWait, so the
// seeding was a write with no reader and a pooled connection lost its command
// phase exactly as it did on V2. A write with no reader is the defect class
// this whole change exists to fix; leaving a second instance of it in place
// would be careless.
func TestLegacyPostTLSReadsLastGreetingCache(t *testing.T) {
	src, err := os.ReadFile("conn.go")
	if err != nil {
		t.Fatalf("read conn.go: %v", err)
	}
	s := string(src)

	// The seeding must still be there...
	if !strings.Contains(s, "hsStore.RememberLast(models.HandshakeLastKey(opts.PassThroughScope, opts.DstCfg)") {
		t.Error("conn.go no longer seeds the last-greeting cache")
	}
	// ...and the post-TLS reader must actually consult it.
	i := strings.Index(s, "PopWait(portKey")
	if i < 0 {
		t.Fatal("the legacy post-TLS port-key PopWait moved; update this guard with it")
	}
	rest := s[i:]
	if j := strings.Index(rest, "serverGreetingBuf = entry.RespPackets[0]"); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "hsStore.Last(lastKey)") {
		t.Error("the legacy post-TLS reader does not consult the last-greeting cache between its " +
			"PopWait misses and the direct dial; the seeding above is a write with no reader and " +
			"a pooled connection still loses its command phase on this path")
	}
}
