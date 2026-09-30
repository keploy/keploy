package recorder

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models/mysql"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
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
	// A fresh connection (HandshakeResponse41), so the recorder waits out the
	// stash bound for its own raw leg before giving up.
	h.pushClient(cannedHandshakeResponse41(t, 2, false), time.Now())

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
	_, _, source := resolvePreTLSGreeting(ctx, store, models.HandshakeOwner{}, 3306, "", "", testDst("10.0.0.5:3306"), false)
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
	entry, _, source := resolvePreTLSGreeting(context.Background(), store, models.HandshakeOwner{}, 3306, "", "", testDst("10.0.0.5:3306"), false)
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
// the own entry pushed shortly after start, resolve must take the stashed entry
// rather than the cache.
//
// Neither leg names its connection here, so the stream cannot prove the entry
// is its own and takes it as the oldest on the port's queue it may take.
// greetingShared is therefore the correct classification; what this test pins is
// that the stashed entry (with its real timestamp) beats the cache.
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

	entry, _, source := resolvePreTLSGreeting(context.Background(), store, models.HandshakeOwner{}, 3306, "", "", testDst("10.0.0.5:3306"), false)
	if source != greetingShared {
		t.Fatalf("source = %v, want greetingShared (a late own leg within the primary bound beats the cache)", source)
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
	_, _, source := resolvePreTLSGreeting(context.Background(), store, models.HandshakeOwner{}, 3306, "", "", testDst("10.0.0.9:3306"), false)
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
	if _, _, source := resolvePreTLSGreeting(context.Background(), store, models.HandshakeOwner{}, 3306,
		"ns/app-b/test-set-0", "", fabricated(), false); source == greetingCached {
		t.Fatal("served app A's greeting to app B; a store shared across a node must isolate by scope")
	}

	// App A itself must still get the fallback — this is the case the feature
	// exists for, and a fabricated address must not disable it.
	if _, _, source := resolvePreTLSGreeting(context.Background(), store, models.HandshakeOwner{}, 3306,
		"ns/app-a/test-set-0", "", fabricated(), false); source != greetingCached {
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
	// ...and that connection consumed its queue entry, leaving only the
	// last-greeting cache behind.
	for {
		if _, ok := store.PopWait(models.HandshakeStoreKey("", 3306), 0); !ok {
			break
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
	if _, _, source := resolvePreTLSGreeting(context.Background(), store, models.HandshakeOwner{}, 3306, scope, "", fabricated, false); source == greetingCached {
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
	entry, _, source := resolvePreTLSGreeting(context.Background(), store, models.HandshakeOwner{}, 3306, "", "", fabricated, false)
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
			if _, _, source := resolvePreTLSGreeting(context.Background(), store, models.HandshakeOwner{}, 3306, scope, "", resolved, false); source == greetingCached {
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
	// The text is built by appending, not with fmt; it must still read as the
	// fmt version did, "v%d|%s|caps=%d|cs=%d|%s".
	if got, want := greetingServerIdentity(base()), fmt.Sprintf("v%d|%s|caps=%d|cs=%d|%s",
		10, "8.4.0", uint32(0xC00FFFFF), 255, "caching_sha2_password"); got != want {
		t.Errorf("identity = %q, want %q", got, want)
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

	if _, _, source := resolvePreTLSGreeting(context.Background(), store, models.HandshakeOwner{}, 3306, scope, "", fab, false); source == greetingCached {
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

	entry, _, source := resolvePreTLSGreeting(context.Background(), store, models.HandshakeOwner{}, 3306, scope, "", fab, false)
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
	if _, _, source := resolvePreTLSGreeting(context.Background(), seed(), models.HandshakeOwner{}, 3306, scope, "", nil, false); source != greetingCached {
		t.Errorf("nil DstCfg: source = %v, want the port key to answer", source)
	}
	// A DstCfg with no address is likewise unresolved.
	noAddr := &models.ConditionalDstCfg{Port: 3306}
	if _, _, source := resolvePreTLSGreeting(context.Background(), seed(), models.HandshakeOwner{}, 3306, scope, "", noAddr, false); source != greetingCached {
		t.Errorf("empty Addr: source = %v, want the port key to answer", source)
	}
}

// TestLegacyPostTLSUsesTheLastGreetingCacheInsteadOfDialling drives the LEGACY
// post-TLS reader and asserts its BEHAVIOUR.
//
// It replaces a test that asserted on the SOURCE TEXT of conn.go: it read the
// file and grepped for "hsStore.Last(lastKey)". That guard could only detect
// textual deletion. Changing the surrounding condition to `if false` leaves the
// string present, kills the entire fallback, and the whole suite stayed green --
// verified. A write with no reader is the exact defect this fallback exists to
// fix, so the guard for it must exercise the reader rather than look at it.
//
// The destination is a fabricated (capture-layer stand-in) address, which
// separates the two paths cleanly: with the cache consulted the greeting comes
// from the store and the connection's config mock is recorded from it; without
// it, fetchServerGreeting REFUSES the address (it does not dial a stand-in) and
// the call fails with "direct fetch failed".
//
// It asserts the success POSITIVELY as well as the failure negatively. A
// one-sided check passes the moment the flow ends before the store lookup ever
// happens — which is what this test did once the first client packet came to
// be read before the lookup, while its stream sent none.
func TestLegacyPostTLSUsesTheLastGreetingCacheInsteadOfDialling(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	mocks := make(chan *models.Mock, 16)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(mocks)
	ctx := syncMock.NewContext(context.WithValue(postTLSCtxWithStore(store),
		models.ClientConnectionIDKey, "pooled-conn"), mgr)

	scope := "ns/app/ts0"
	// Port 1 is closed, so the direct-dial fallback cannot succeed.
	dst := &models.ConditionalDstCfg{Addr: "127.0.0.1:1", Port: 1, AddrFabricated: true}
	// As a raw leg caches it: the greeting and the client's SSLRequest.
	store.RememberLast(models.HandshakeLastKey(scope, dst), models.TLSHandshakeEntry{
		RespPackets: [][]byte{cannedHandshakeV10(t)},
		ReqPackets:  [][]byte{cannedSSLRequest(t, 1)},
	})

	// A pooled connection: its first decrypted packet is a command, and then
	// the stream ends. The greeting is looked up only once that packet is in,
	// so it must be there; this test is about WHICH SOURCE the greeting came
	// from, not about decoding traffic.
	clientConn, peer := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	destConn, destPeer := net.Pipe()
	defer func() { _ = destConn.Close() }()
	_ = destPeer.Close()
	go func() {
		_, _ = peer.Write(cannedCOMQuery(t, 0, "SELECT 1"))
		_ = peer.Close()
	}()

	opts := models.OutgoingOptions{
		DstCfg:           dst,
		PassThroughScope: scope,
		ConnKey:          "pooled-conn-whose-own-entry-was-consumed",
	}
	err := handlePostTLSRecord(ctx, zap.NewNop(), clientConn, destConn, mocks,
		buildPostHandshakeDecodeCtx(clientConn), opts)

	if err != nil && strings.Contains(err.Error(), "direct fetch failed") {
		t.Fatalf("the legacy post-TLS reader ignored the last-greeting cache and fell through to "+
			"fetchServerGreeting for %s — the seeding in handleInitialHandshake is then a write with "+
			"no reader, and a pooled connection loses its whole command phase here: %v", dst.Addr, err)
	}
	// Positive assertion: the cached greeting was used, for the connection's
	// config mock.
	close(mocks)
	var cfg bool
	for m := range mocks {
		cfg = cfg || (m != nil && m.Name == "config")
	}
	if !cfg {
		t.Fatalf("no config mock from the cached greeting (err %v)", err)
	}
}

// The port's queue ("port:3306") is CROSS-CONNECTION: every decrypted stream to
// that port draws from it, and a stream that cannot name its connection may take
// an entry that is not its own. Popping is destructive, so a stream that takes
// a greeting and then fails has consumed a live connection's greeting and
// produced nothing — silent decode loss, which is what made the
// e2e-mysql-tls-lowlatency-cycle shortfall "served but NOT recorded" rather than
// an error. An unused entry must go back.
func TestPostTLSHandshakeV2_RestoresUnusedSharedGreeting(t *testing.T) {
	h := newV2Harness(t)
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	store := models.NewTLSHandshakeStore()
	portKey := models.HandshakeStoreKey("", 3306)
	store.Push(portKey, models.TLSHandshakeEntry{
		RespPackets:  [][]byte{cannedHandshakeV10(t)},
		ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
		ReqTimestamp: base,
	})

	// A fresh connection's HandshakeResponse41 arrives, so the stream adopts the
	// shared greeting, and then the connection dies before the server's auth
	// reply is captured.
	h.pushClient(cannedHandshakeResponse41(t, 2, false), base.Add(5*time.Millisecond))
	h.closeStreams()

	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
	}
	var clientKey net.Conn = h.sess.ClientStream

	_, err := handlePostTLSHandshakeV2(postTLSCtxWithStore(store), h.logger, h.sess, decodeCtx, &clientKey)
	if err == nil {
		t.Fatal("expected the post-TLS handshake to fail when the server's auth reply never arrives")
	}
	// Pin WHICH failure: the greeting must have been popped and decoded, and the
	// stream must have died after it. Any earlier error would leave the entry
	// unconsumed and make the restore assertion vacuous.
	if !strings.Contains(err.Error(), "read auth data from server") {
		t.Fatalf("failed before consuming the greeting (%v); this test must fail after adopting it "+
			"or it proves nothing", err)
	}

	// The greeting must still be available to the connection that actually needs
	// it. Without the restore it was consumed and gone, and a live MySQL
	// connection lost its command phase with nothing logged.
	if _, ok := store.PopWait(portKey, 0); !ok {
		t.Fatal("a greeting consumed by a FAILED post-TLS attempt was never returned to the shared " +
			"port FIFO; the connection it belonged to is starved and its queries go unrecorded")
	}
}

// A decrypted stream whose own bytes never arrive (the connection completed and
// closed before its parser was wired) must not touch the shared port FIFO at
// all. It used to pop a greeting, fail on its first client read and push the
// entry back, stripped of the timestamp that belonged to the connection it was
// captured for. It now reads first, so the entry stays exactly as captured for
// the stream that needs it.
func TestPostTLSHandshakeV2_StreamWithoutBytesLeavesTheSharedGreetingUntouched(t *testing.T) {
	h := newV2Harness(t)
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	store := models.NewTLSHandshakeStore()
	portKey := models.HandshakeStoreKey("", 3306)
	store.Push(portKey, models.TLSHandshakeEntry{
		RespPackets:  [][]byte{cannedHandshakeV10(t)},
		ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
		ReqTimestamp: base,
	})
	h.closeStreams()

	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
	}
	var clientKey net.Conn = h.sess.ClientStream
	_, err := handlePostTLSHandshakeV2(postTLSCtxWithStore(store), h.logger, h.sess, decodeCtx, &clientKey)
	if err == nil || !strings.Contains(err.Error(), "read first client packet") {
		t.Fatalf("err = %v, want the first-client-packet read to fail", err)
	}
	got, ok := store.PopWait(portKey, 0)
	if !ok {
		t.Fatal("a stream with no bytes of its own consumed the shared greeting")
	}
	if !got.ReqTimestamp.Equal(base) {
		t.Fatalf("the untouched entry's ReqTimestamp = %v, want its own %v: it was popped and pushed back",
			got.ReqTimestamp, base)
	}
}

// The mirror case: a greeting this connection legitimately consumed and USED
// must NOT be pushed back, or a later stream would stitch a duplicate.
func TestPostTLSHandshakeV2_KeepsSharedGreetingOnSuccess(t *testing.T) {
	h := newV2Harness(t)
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}

	store := models.NewTLSHandshakeStore()
	portKey := models.HandshakeStoreKey("", 3306)
	store.Push(portKey, models.TLSHandshakeEntry{
		RespPackets:  [][]byte{handshakeBuf},
		ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
		ReqTimestamp: base,
	})

	h.pushClient(cannedHandshakeResponse41(t, 2, false), base.Add(5*time.Millisecond))
	h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), base.Add(10*time.Millisecond))

	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
	}
	var clientKey net.Conn = h.sess.ClientStream

	if _, err := handlePostTLSHandshakeV2(postTLSCtxWithStore(store), h.logger, h.sess, decodeCtx, &clientKey); err != nil {
		t.Fatalf("post-TLS handshake failed unexpectedly: %v", err)
	}
	if _, ok := store.PopWait(portKey, 0); ok {
		t.Fatal("a greeting that WAS used got pushed back; a later stream would stitch it a second time")
	}
}

// An entry its raw leg pushed under the same connection identity is this
// connection's OWN. It must classify as greetingOwn, be found wherever it sits
// in the port's queue, and never be mistaken for another connection's, whose
// entry stays for it.
func TestResolvePreTLSGreeting_ConnKeyedEntryIsOwnNotShared(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	portKey := models.HandshakeStoreKey("", 3306)
	connA := models.HandshakeOwner{Conn: "conn-A", Proc: "p1"}
	connB := models.HandshakeOwner{Conn: "conn-B", Proc: "p1"}
	store.PushFor(portKey, connB, models.TLSHandshakeEntry{RespPackets: [][]byte{[]byte("B")}})
	store.PushFor(portKey, connA, models.TLSHandshakeEntry{RespPackets: [][]byte{[]byte("A")}})

	entry, taker, source := resolvePreTLSGreeting(context.Background(), store, connA, 3306, "", "", testDst("10.0.0.5:3306"), false)
	if source != greetingOwn || taker != connA {
		t.Fatalf("source = %v (owner %v), want greetingOwn for the entry pushed under its own identity", source, taker)
	}
	if len(entry.RespPackets) != 1 || string(entry.RespPackets[0]) != "A" {
		t.Fatalf("own entry not returned: %+v", entry)
	}
	if e, owner, ok := store.PopWaitFor(portKey, models.HandshakeOwner{}, false, 0); !ok || owner != connB || string(e.RespPackets[0]) != "B" {
		t.Fatalf("the other connection's entry is gone or changed: %v (owner %v, %v)", e, owner, ok)
	}
}

// A restored greeting is, for whatever connection picks it up next, a BORROWED
// entry from a dead connection. Push re-stamps the store's own expiry, so the
// entry does not age out; if it also carried its original ReqTimestamp it would
// backdate the next connection's config mock. Config mocks are identified by
// Name+Kind+ReqTimestampMock and ordered by it, so that is a replay break — the
// same hazard the legacy path documents for borrowed greetings.
func TestPostTLSHandshakeV2_RestoredGreetingCarriesNoTimestamp(t *testing.T) {
	h := newV2Harness(t)
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	store := models.NewTLSHandshakeStore()
	portKey := models.HandshakeStoreKey("", 3306)
	// Another connection's capture, which this stream (its decrypted leg names
	// no connection) may take.
	owner := models.HandshakeOwner{Conn: "sk:5", Proc: "p1"}
	store.PushFor(portKey, owner, models.TLSHandshakeEntry{
		RespPackets:  [][]byte{cannedHandshakeV10(t)},
		ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
		ReqTimestamp: base,
	})
	// Adopted (a HandshakeResponse41 arrives), then the stream dies.
	h.pushClient(cannedHandshakeResponse41(t, 2, false), base.Add(5*time.Millisecond))
	h.closeStreams()

	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
	}
	var clientKey net.Conn = h.sess.ClientStream
	if _, err := handlePostTLSHandshakeV2(postTLSCtxWithStore(store), h.logger, h.sess, decodeCtx, &clientKey); err == nil {
		t.Fatal("expected the post-TLS handshake to fail")
	}

	// Back under its owner, so its own connection still finds it.
	restored, _, ok := store.PopWaitFor(portKey, owner, true, 0)
	if !ok {
		t.Fatal("greeting was not restored to its owner")
	}
	if !restored.ReqTimestamp.IsZero() {
		t.Fatalf("restored entry kept ReqTimestamp %v; it would backdate the next connection's config mock",
			restored.ReqTimestamp)
	}
}

// An entry whose greeting cannot decode is garbage. Restoring it would be worse
// than dropping it: Push re-stamps the expiry, so it would never age out and
// would trip every later decrypted stream to this port.
func TestPostTLSHandshakeV2_DoesNotRecycleUndecodableGreeting(t *testing.T) {
	h := newV2Harness(t)

	store := models.NewTLSHandshakeStore()
	portKey := models.HandshakeStoreKey("", 3306)
	store.Push(portKey, models.TLSHandshakeEntry{
		RespPackets: [][]byte{{0x01, 0x00, 0x00, 0x00, 0xff}}, // not a HandshakeV10
	})
	// The stream's own first packet, so it goes on to adopt the entry.
	h.pushClient(cannedHandshakeResponse41(t, 2, false), time.Now())

	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
	}
	var clientKey net.Conn = h.sess.ClientStream
	if _, err := handlePostTLSHandshakeV2(postTLSCtxWithStore(store), h.logger, h.sess, decodeCtx, &clientKey); err == nil {
		t.Fatal("expected an undecodable greeting to fail the handshake")
	}
	if _, ok := store.PopWait(portKey, 0); ok {
		t.Fatal("an undecodable greeting was put back into the shared FIFO; Push re-stamps its expiry so it " +
			"would never age out and would trip every later stream to this port")
	}
}

// runPostTLSSession drives one RecordV2 session to want mocks without touching
// t, so several can run concurrently.
func runPostTLSSession(ctx context.Context, h *v2Harness, want int, patience time.Duration) ([]*models.Mock, error) {
	done := make(chan error, 1)
	go func() {
		cctx, cancel := context.WithTimeout(ctx, patience)
		defer cancel()
		done <- RecordV2(cctx, h.logger, h.sess)
	}()
	var got []*models.Mock
	deadline := time.After(patience)
	for len(got) < want {
		select {
		case m, ok := <-h.mocks:
			if !ok {
				return got, fmt.Errorf("mocks channel closed after %d of %d", len(got), want)
			}
			got = append(got, m)
		case err := <-done:
			return got, fmt.Errorf("RecordV2 returned after %d of %d mocks: %v", len(got), want, err)
		case <-deadline:
			return got, fmt.Errorf("timed out after %d of %d mocks", len(got), want)
		}
	}
	h.closeStreams()
	if err := <-done; err != nil {
		return got, fmt.Errorf("RecordV2: %w", err)
	}
	return got, nil
}

// Pooled connections opened before the recording started reach the V2
// post-TLS path with nothing captured: no own greeting, and no cached one for
// their (real, resolved) destination. Each used to dial the server for its
// greeting. Every dial is an aborted handshake the server counts against the
// dialling host (max_connect_errors), and a DaemonSet agent dials from the
// node its SNATed pods share. N such connections to one server must cost ONE
// dial, each must still record its command, and a later connection must reuse
// the remembered greeting instead of dialling. A second server gets its own
// dial.
func TestRecordV2_PostTLS_ConcurrentPooledConnsDialTheServerOnce(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "150")

	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}
	srv := startFakeMySQLGreeter(t, handshakeBuf, time.Second)
	store := models.NewTLSHandshakeStore()
	ctx := postTLSCtxWithStore(store)
	const scope = "ns/app/ts0"
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	session := func(addr string) *v2Harness {
		h := newV2Harness(t)
		h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: addr, Port: 3306}
		h.sess.Opts.PassThroughScope = scope
		h.sess.Opts.NetNS = testNetNS
		// Joined mid-stream (seq==0): one command, one response.
		h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), base.Add(20*time.Millisecond))
		h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(25*time.Millisecond))
		return h
	}
	runAll := func(hs []*v2Harness) [][]*models.Mock {
		t.Helper()
		out := make([][]*models.Mock, len(hs))
		errs := make([]error, len(hs))
		var wg sync.WaitGroup
		for i, h := range hs {
			wg.Add(1)
			go func(i int, h *v2Harness) {
				defer wg.Done()
				// config (greeting-only: nothing captured an SSLRequest) + query.
				out[i], errs[i] = runPostTLSSession(ctx, h, 2, 10*time.Second)
			}(i, h)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("session %d: %v", i, err)
			}
		}
		return out
	}

	const n = 6
	batch := make([]*v2Harness, n)
	for i := range batch {
		batch[i] = session(srv.addr)
	}
	for i, got := range runAll(batch) {
		if got[0].Name != "config" || len(got[0].Spec.MySQLResponses) < 1 {
			t.Errorf("session %d: first mock %q has no greeting", i, got[0].Name)
		}
		assertQueryMock(t, got[1])
	}
	if got := srv.accepted.Load(); got != 1 {
		t.Fatalf("%d concurrent pooled connections dialled the server %d times, want 1", n, got)
	}

	// A later connection to the same server: served from the remembered
	// greeting, no dial.
	later := runAll([]*v2Harness{session(srv.addr)})
	assertQueryMock(t, later[0][1])
	if got := srv.accepted.Load(); got != 1 {
		t.Fatalf("a later connection dialled again (%d dials) instead of reusing the remembered greeting", got)
	}

	// A second server is its own destination.
	srv2 := startFakeMySQLGreeter(t, handshakeBuf, 200*time.Millisecond)
	other := runAll([]*v2Harness{session(srv2.addr), session(srv2.addr)})
	assertQueryMock(t, other[0][1])
	assertQueryMock(t, other[1][1])
	if a, b := srv.accepted.Load(), srv2.accepted.Load(); a != 1 || b != 1 {
		t.Fatalf("dials: first server %d, second server %d; want 1 each", a, b)
	}
}

// Recording stop during the stash wait must not dial the server: the wait
// exits on ctx cancellation and the connection is being torn down, so there is
// nothing to fetch a greeting for — only an aborted handshake to cost the host.
// A fresh connection (HandshakeResponse41) is the one that waits.
func TestRecordV2_PostTLS_TeardownDuringStashWaitNeverDials(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "5000")
	srv := startFakeMySQLGreeter(t, cannedHandshakeV10(t), 0)
	h := newV2Harness(t)
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306}
	h.sess.Opts.PassThroughScope = "ns/app/ts0"
	h.pushClient(cannedHandshakeResponse41(t, 2, false), time.Now())

	ctx, cancel := context.WithCancel(postTLSCtxWithStore(models.NewTLSHandshakeStore()))
	done := make(chan error, 1)
	go func() { done <- RecordV2(ctx, h.logger, h.sess) }()
	time.Sleep(300 * time.Millisecond) // inside the 5s stash wait
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RecordV2 did not return after teardown")
	}
	time.Sleep(100 * time.Millisecond)
	if got := srv.accepted.Load(); got != 0 {
		t.Fatalf("teardown during the stash wait dialled the server %d times", got)
	}
}

// A greeting remembered for the server (fetched, or captured on another
// connection) carries no SSLRequest, so it is no better than fetching again,
// and it must not stop a fresh connection from waiting for its OWN raw leg.
// That leg carries the SSLRequest (the config mock's hosted shape) and the
// connection's real timestamp. Only the app-scoped cache of CAPTURED entries is
// good enough at the primary bound; the per-server memo is used where the fetch
// would have happened, after the overall wait.
func TestRecordV2_PostTLS_FetchedCacheDoesNotPreemptOwnLateLeg(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_PRIMARY_MS", "100")
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "5000")

	h := newV2Harness(t)
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: "10.0.0.5:3306", Port: 3306}
	h.sess.Opts.PassThroughScope = "ns/app/ts0"
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}
	store := models.NewTLSHandshakeStore()
	// What fetchServerGreetingShared leaves behind for this server.
	store.RememberServerGreeting(greetingMemoKey(h.sess.Opts), handshakeBuf, "id")

	h.pushClient(cannedHandshakeResponse41(t, 2, false), base.Add(5*time.Millisecond))
	h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), base.Add(10*time.Millisecond))
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), base.Add(20*time.Millisecond))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(25*time.Millisecond))

	// This connection's own raw leg lands well past the primary bound.
	ownLeg := models.TLSHandshakeEntry{
		RespPackets:  [][]byte{handshakeBuf},
		ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
		ReqTimestamp: base,
	}
	go func() {
		time.Sleep(700 * time.Millisecond)
		store.Push(models.HandshakeStoreKey("", 3306), ownLeg)
	}()

	got := collectPostTLSMocks(t, h, postTLSCtxWithStore(store), 2, 10*time.Second)
	cfg := got[0]
	if cfg.Name != "config" {
		t.Fatalf("first mock = %q, want config", cfg.Name)
	}
	if len(cfg.Spec.MySQLRequests) < 2 {
		t.Fatalf("config mock has %d requests, want the own leg's SSLRequest + HandshakeResponse41: "+
			"the fetched cache entry pre-empted this connection's own late raw leg", len(cfg.Spec.MySQLRequests))
	}
	if !cfg.Spec.ReqTimestampMock.Equal(base) {
		t.Errorf("config ReqTimestampMock = %v, want the own raw leg's %v", cfg.Spec.ReqTimestampMock, base)
	}
	assertQueryMock(t, got[1])
}

// configGreetingConnID returns the connection id of the HandshakeV10 greeting in
// a config mock, which tells a test WHICH greeting a connection was stitched
// with.
func configGreetingConnID(t *testing.T, m *models.Mock) uint32 {
	t.Helper()
	for _, r := range m.Spec.MySQLResponses {
		if g, ok := r.Message.(*mysql.HandshakeV10Packet); ok {
			return g.ConnectionID
		}
	}
	t.Fatalf("config mock carries no HandshakeV10 greeting")
	return 0
}

// A pooled connection opened before the recording has no greeting coming: its
// first decrypted packet is a command (seq 0), which proves its TLS session was
// authenticated before capture began. It used to wait out the whole stash bound
// (30s by default) for a raw leg that cannot exist, then fetch. It now decodes
// its first command at once. Runs with the DEFAULT stash bounds: the point is
// that they no longer apply to this connection.
func TestRecordV2_PostTLS_PooledConnRecordsPromptlyWithDefaultStashWait(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "")
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_PRIMARY_MS", "")
	if got := mysqlPostTLSStashWait(); got != mysqlPostTLSStashWaitDefault {
		t.Fatalf("stash wait = %v, want the %v default", got, mysqlPostTLSStashWaitDefault)
	}
	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}
	for _, tc := range []struct {
		name  string
		seed  func(store *models.TLSHandshakeStore, opts models.OutgoingOptions)
		dials int32
	}{
		{"nothing captured, nothing remembered: one fetch", func(*models.TLSHandshakeStore, models.OutgoingOptions) {}, 1},
		{"a greeting remembered for the server: no fetch", func(store *models.TLSHandshakeStore, opts models.OutgoingOptions) {
			store.RememberServerGreeting(greetingMemoKey(opts), handshakeBuf, "id")
		}, 0},
		{"a captured greeting cached for the destination: no fetch", func(store *models.TLSHandshakeStore, opts models.OutgoingOptions) {
			store.RememberLast(lastGreetingKey(opts.PassThroughScope, opts.NetNS, opts.DstCfg), models.TLSHandshakeEntry{
				RespPackets: [][]byte{handshakeBuf}, ReqPackets: [][]byte{cannedSSLRequest(t, 1)},
			})
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFakeMySQLGreeter(t, handshakeBuf, 0)
			h := newV2Harness(t)
			h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306}
			h.sess.Opts.PassThroughScope = "ns/app/test-set-0"
			h.sess.Opts.NetNS = testNetNS
			store := models.NewTLSHandshakeStore()
			tc.seed(store, h.sess.Opts)
			base := time.Now()
			h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), base)
			h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(time.Millisecond))

			start := time.Now()
			got := collectPostTLSMocks(t, h, postTLSCtxWithStore(store), 2, 10*time.Second)
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("the pooled connection's command took %v to record; it waited for a greeting "+
					"that cannot come (stash wait %v)", elapsed, mysqlPostTLSStashWait())
			}
			if got[0].Name != "config" {
				t.Fatalf("first mock = %q, want config", got[0].Name)
			}
			assertQueryMock(t, got[1])
			if n := srv.accepted.Load(); n != tc.dials {
				t.Fatalf("%d greeting dials, want %d", n, tc.dials)
			}
		})
	}
}

// The wait is for a FRESH connection's own raw leg, and it stays. That leg
// carries the SSLRequest that precedes the HandshakeResponse41 in the config
// mock and the connection's real timestamp; a greeting remembered for the
// server carries neither. So a fresh connection whose raw leg is merely late
// must get ITS OWN greeting even when the server's greeting is remembered.
// Default bounds: the own leg lands well inside them.
func TestRecordV2_PostTLS_FreshConnWithALateRawLegGetsItsOwnGreetingNotTheMemo(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "")
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_PRIMARY_MS", "")
	memo := cannedHandshakeV10Variant(t, "8.0.36", 7)
	own := cannedHandshakeV10Variant(t, "8.0.36", 8)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), own[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}
	srv := startFakeMySQLGreeter(t, memo, 0)
	h := newV2Harness(t)
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: srv.addr, Port: 3306}
	h.sess.Opts.PassThroughScope = "ns/app/test-set-1"
	h.sess.Opts.NetNS = testNetNS
	store := models.NewTLSHandshakeStore()
	// A pooled connection of an earlier session had the greeting fetched.
	store.RememberServerGreeting(greetingMemoKey(h.sess.Opts), memo, "id")

	ownTs := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	h.pushClient(cannedHandshakeResponse41(t, 2, false), ownTs.Add(5*time.Millisecond))
	h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), ownTs.Add(10*time.Millisecond))
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), ownTs.Add(20*time.Millisecond))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), ownTs.Add(25*time.Millisecond))
	go func() {
		time.Sleep(600 * time.Millisecond) // the raw leg lands late
		store.Push(models.HandshakeStoreKey("", 3306), models.TLSHandshakeEntry{
			RespPackets:  [][]byte{own},
			ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
			ReqTimestamp: ownTs,
		})
	}()

	got := collectPostTLSMocks(t, h, postTLSCtxWithStore(store), 2, 15*time.Second)
	cfg := got[0]
	if cfg.Name != "config" {
		t.Fatalf("first mock = %q, want config", cfg.Name)
	}
	if id := configGreetingConnID(t, cfg); id != 8 {
		t.Fatalf("config mock stitched greeting connection id %d, want 8 (its own raw leg); 7 is the "+
			"server's remembered greeting", id)
	}
	if len(cfg.Spec.MySQLRequests) < 2 || cfg.Spec.MySQLRequests[0].Header.Type != mysql.SSLRequest {
		t.Fatalf("config mock requests lack the own leg's SSLRequest: %d requests", len(cfg.Spec.MySQLRequests))
	}
	if !cfg.Spec.ReqTimestampMock.Equal(ownTs) {
		t.Fatalf("config ReqTimestampMock = %v, want the own raw leg's %v", cfg.Spec.ReqTimestampMock, ownTs)
	}
	assertQueryMock(t, got[1])
	if n := srv.accepted.Load(); n != 0 {
		t.Fatalf("%d dials; the fresh connection had its own greeting", n)
	}
}

// A connection that joined mid-stream takes from the port's queue only an
// entry that is provably its own: anything else there may be a fresh
// connection's own leg that has not reached its decrypted stream yet, the
// NORMAL ordering (a raw leg's greeting and SSLRequest precede the TLS
// handshake, which precedes the first decrypted packet), or another app's. It
// borrows the app-scoped cache instead, timed from its own first command, and
// then the per-server memo. The queue is left as it was.
func TestRecordV2_PostTLS_JoinedConnTakesOnlyItsOwnEntry(t *testing.T) {
	portKey := models.HandshakeStoreKey("", 3306)
	dst := testDst("10.0.0.5:3306")
	const scope = "ns/app/test-set-0"
	entryOf := func(version string, id uint32, ts time.Time) models.TLSHandshakeEntry {
		return models.TLSHandshakeEntry{
			RespPackets:  [][]byte{cannedHandshakeV10Variant(t, version, id)},
			ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
			ReqTimestamp: ts,
		}
	}
	cached := entryOf("8.0.36", 21, time.Now().Add(-time.Minute))
	freshLeg := entryOf("8.0.36", 22, time.Now().Add(-time.Second))
	otherApp := entryOf("5.7.44-other-server", 24, time.Now())
	ownLeg := entryOf("8.0.36", 23, time.Now().Add(-2*time.Second))
	caps := decodeGreeting(t, cached.RespPackets[0]).CapabilityFlags
	joined := func(t *testing.T, store *models.TLSHandshakeStore, owner models.HandshakeOwner) *models.Mock {
		t.Helper()
		h := newV2Harness(t)
		h.sess.Opts.DstCfg = dst
		h.sess.Opts.PassThroughScope = scope
		h.sess.Opts.ConnKey, h.sess.Opts.ConnProc = owner.Conn, owner.Proc
		base := time.Now()
		h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), base)
		h.pushDest(cannedOK(t, 1, caps), base.Add(time.Millisecond))
		start := time.Now()
		got := collectPostTLSMocks(t, h, postTLSCtxWithStore(store), 2, 5*time.Second)
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("the joined connection took %v; it waited", d)
		}
		if got[0].Name != "config" {
			t.Fatalf("first mock = %q, want config", got[0].Name)
		}
		return got[0]
	}
	remaining := func(store *models.TLSHandshakeStore) []uint32 {
		var ids []uint32
		for {
			e, ok := store.PopWait(portKey, 0)
			if !ok {
				return ids
			}
			g := decodeGreeting(t, e.RespPackets[0])
			ids = append(ids, g.ConnectionID)
		}
	}

	for _, tc := range []struct {
		name  string
		owner models.HandshakeOwner
		fresh models.HandshakeOwner
	}{
		{"neither leg names its connection", models.HandshakeOwner{}, models.HandshakeOwner{}},
		{"same process, connections unnamed", models.HandshakeOwner{Proc: "p1"}, models.HandshakeOwner{Proc: "p1"}},
		{"another connection of the same process", models.HandshakeOwner{Conn: "sk:5", Proc: "p1"}, models.HandshakeOwner{Conn: "sk:6", Proc: "p1"}},
	} {
		t.Run("a fresh connection's leg and another app's are left alone: "+tc.name, func(t *testing.T) {
			store := models.NewTLSHandshakeStore()
			store.RememberLast(models.HandshakeLastKey(scope, dst), cached) // an earlier raw leg
			store.PushFor(portKey, tc.fresh, freshLeg)
			store.PushFor(portKey, models.HandshakeOwner{Conn: "sk:9", Proc: "p2"}, otherApp)
			cfg := joined(t, store, tc.owner)
			if id := configGreetingConnID(t, cfg); id != 21 {
				t.Fatalf("stitched greeting %d, want the cached 21", id)
			}
			if !configMockHasHR41(cfg) {
				t.Fatal("the joined connection's config mock is not replay-matchable")
			}
			if cfg.Spec.ReqTimestampMock.Equal(cached.ReqTimestamp) {
				t.Fatal("the joined connection's config mock carries the borrowed entry's timestamp")
			}
			if ids := remaining(store); len(ids) != 2 || ids[0] != 22 || ids[1] != 24 {
				t.Fatalf("port queue after the joined connection = %v, want [22 24] untouched", ids)
			}
		})
	}

	t.Run("its own entry is taken, and only that", func(t *testing.T) {
		store := models.NewTLSHandshakeStore()
		store.RememberLast(models.HandshakeLastKey(scope, dst), cached)
		store.PushFor(portKey, models.HandshakeOwner{Conn: "sk:6", Proc: "p1"}, freshLeg)
		store.PushFor(portKey, models.HandshakeOwner{Conn: "sk:5", Proc: "p1"}, ownLeg)
		cfg := joined(t, store, models.HandshakeOwner{Conn: "sk:5", Proc: "p1"})
		if id := configGreetingConnID(t, cfg); id != 23 {
			t.Fatalf("stitched greeting %d, want its own 23", id)
		}
		if !cfg.Spec.ReqTimestampMock.Equal(ownLeg.ReqTimestamp) {
			t.Fatalf("ReqTimestampMock = %v, want its own greeting's %v", cfg.Spec.ReqTimestampMock, ownLeg.ReqTimestamp)
		}
		if ids := remaining(store); len(ids) != 1 || ids[0] != 22 {
			t.Fatalf("port queue = %v, want the other connection's [22]", ids)
		}
	})

	t.Run("nothing cached: the per-server memo, and the queue untouched", func(t *testing.T) {
		store := models.NewTLSHandshakeStore()
		store.PushFor(portKey, models.HandshakeOwner{}, freshLeg)
		store.RememberServerGreeting(models.HandshakeServerKey("", dst), cached.RespPackets[0], "id")
		cfg := joined(t, store, models.HandshakeOwner{})
		if id := configGreetingConnID(t, cfg); id != 21 {
			t.Fatalf("stitched greeting %d, want the server's remembered greeting 21", id)
		}
		if ids := remaining(store); len(ids) != 1 || ids[0] != 22 {
			t.Fatalf("port queue = %v, want the fresh connection's [22]", ids)
		}
	})
}

// A raw leg records the greeting it captured as the SERVER's, for the pooled
// connections of any app or session that later need one, and not only on the
// TLS path: a plaintext client's greeting is the same server's greeting.
func TestRecordV2_RawLegRemembersTheServerGreeting(t *testing.T) {
	for _, tls := range []bool{true, false} {
		t.Run(fmt.Sprintf("tls=%v", tls), func(t *testing.T) {
			store := models.NewTLSHandshakeStore()
			h := newV2Harness(t)
			h.sess.Opts.SkipTLSMITM = true
			h.sess.Opts.PassThroughScope = "ns/app/test-set-0"
			h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: "10.244.0.24:3306", Port: 3306}
			live := cannedHandshakeV10Variant(t, "8.0.36-live", 9)
			greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), live[4:])
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			base := time.Now()
			h.pushDest(live, base)
			if tls {
				h.pushClient(cannedSSLRequest(t, 1), base.Add(time.Millisecond))
			} else {
				h.pushClient(cannedHandshakeResponse41(t, 1, false), base.Add(time.Millisecond))
				h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), base.Add(2*time.Millisecond))
			}
			rawCtx := context.WithValue(context.Background(), models.TLSHandshakeStoreKey, store)
			ctx, cancel := context.WithTimeout(rawCtx, 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- RecordV2(ctx, h.logger, h.sess) }()
			if !tls {
				// Plaintext: the connection continues into its command phase.
				select {
				case <-h.mocks:
				case <-time.After(5 * time.Second):
					t.Fatal("no config mock from the plaintext raw leg")
				}
				h.closeStreams()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("RecordV2 did not return on the raw leg")
			}
			// Any other session's pooled connection to this server reads it.
			other := models.OutgoingOptions{DstCfg: h.sess.Opts.DstCfg, PassThroughScope: "ns/other/test-set-9"}
			if g, ok := store.ServerGreeting(greetingMemoKey(other)); !ok || !bytes.Equal(g, live) {
				t.Fatalf("server greeting after the raw leg = %x (found=%v), want the captured one", g, ok)
			}
		})
	}
}

// The race the reviewer found in the port-FIFO design, in its normal ordering:
// a fresh connection's raw leg lands BEFORE its decrypted stream starts, and a
// pooled connection's first command arrives in between. The pooled connection
// must not take the fresh connection's entry, whatever it knows about either
// connection; the fresh connection then gets its own, with its timestamp.
// Repeated with the pooled resolver racing the fresh one's wait.
func TestResolvePreTLSGreeting_JoinedConnLeavesAFreshConnsLeg(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "5000")
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_PRIMARY_MS", "5000")
	portKey := models.HandshakeStoreKey("", 3306)
	dst := testDst("10.0.0.5:3306")
	for _, tc := range []struct {
		name          string
		fresh, pooled models.HandshakeOwner
	}{
		{"no identities", models.HandshakeOwner{}, models.HandshakeOwner{}},
		{"processes known", models.HandshakeOwner{Proc: "p1"}, models.HandshakeOwner{Proc: "p1"}},
		{"connections known", models.HandshakeOwner{Conn: "sk:1", Proc: "p1"}, models.HandshakeOwner{Conn: "sk:2", Proc: "p1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 50; i++ {
				store := models.NewTLSHandshakeStore()
				ts := time.Unix(1_700_000_000+int64(i), 0)
				own := models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, byte(i)}}, ReqPackets: [][]byte{{0x20}}, ReqTimestamp: ts}
				// What the raw leg does: queue the capture, and cache it.
				store.PushFor(portKey, tc.fresh, own)
				store.RememberLast(models.HandshakeLastKey("s", dst), own)
				fresh := make(chan models.TLSHandshakeEntry, 1)
				go func() {
					e, _, _ := resolvePreTLSGreeting(context.Background(), store, tc.fresh, 3306, "s", "", dst, false)
					fresh <- e
				}()
				if e, _, src := resolvePreTLSGreeting(context.Background(), store, tc.pooled, 3306, "s", "", dst, true); src != greetingCached {
					t.Fatalf("iteration %d: the pooled connection got %v from %v, want the cache: the queue "+
						"held only the fresh connection's leg", i, e, src)
				}
				if e := <-fresh; len(e.RespPackets) != 1 || e.RespPackets[0][1] != byte(i) || !e.ReqTimestamp.Equal(ts) {
					t.Fatalf("iteration %d: the fresh connection got %v, want its own leg and timestamp", i, e)
				}
			}
		})
	}
}

// A connection that joined mid-stream still takes its OWN entry (a capture
// layer that can name both legs' connection sets ConnKey on both): it is this
// connection's, and it is preferred over the cache, with no wait.
func TestResolvePreTLSGreeting_JoinedConnTakesItsOwnEntry(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	portKey := models.HandshakeStoreKey("", 3306)
	connA := models.HandshakeOwner{Conn: "conn-A", Proc: "p1"}
	store.PushFor(portKey, models.HandshakeOwner{Conn: "conn-B", Proc: "p1"}, models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 's'}}})
	store.PushFor(portKey, connA, models.TLSHandshakeEntry{RespPackets: [][]byte{{0x0a, 'o'}}})

	start := time.Now()
	entry, _, source := resolvePreTLSGreeting(context.Background(), store, connA, 3306, "", "", testDst("10.0.0.5:3306"), true)
	if source != greetingOwn || len(entry.RespPackets) != 1 || entry.RespPackets[0][1] != 'o' {
		t.Fatalf("got %v from source %v, want the own entry", entry, source)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("a joined connection waited %v", d)
	}
	if e, ok := store.PopWait(portKey, 0); !ok || e.RespPackets[0][1] != 's' {
		t.Fatalf("port queue head = %v (%v), want the other connection's entry untouched", e, ok)
	}
	if _, ok := store.PopWait(portKey, 0); ok {
		t.Fatal("more than the other connection's entry is left on the queue")
	}
}

// Concurrent connections to one port, their raw legs landing in one order and
// their decrypted streams starting in the reverse order: each is stitched with
// its OWN leg when the capture layer names the connection on both legs, and
// with a leg of its own process when it names only the process. Arrival order
// alone, the port FIFO, crossed every pair.
func TestResolvePreTLSGreeting_PairsEachStreamWithItsOwnLeg(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "5000")
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_PRIMARY_MS", "5000")
	portKey := models.HandshakeStoreKey("", 3306)
	dst := testDst("10.0.0.5:3306")
	const n = 6
	for _, tc := range []struct {
		name    string
		ownerOf func(i int) models.HandshakeOwner
	}{
		{"by connection", func(i int) models.HandshakeOwner {
			return models.HandshakeOwner{Conn: fmt.Sprintf("sk:%d", i), Proc: "p1"}
		}},
		{"by process, one connection each", func(i int) models.HandshakeOwner {
			return models.HandshakeOwner{Proc: fmt.Sprintf("p%d", i)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := models.NewTLSHandshakeStore()
			for i := 0; i < n; i++ {
				store.PushFor(portKey, tc.ownerOf(i), models.TLSHandshakeEntry{
					RespPackets: [][]byte{{0x0a, byte(i)}}, ReqTimestamp: time.Unix(int64(i+1), 0),
				})
			}
			var wg sync.WaitGroup
			got := make([]byte, n)
			for i := n - 1; i >= 0; i-- {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					e, _, src := resolvePreTLSGreeting(context.Background(), store, tc.ownerOf(i), 3306, "s", "", dst, false)
					if src == greetingNone {
						got[i] = 0xff
						return
					}
					got[i] = e.RespPackets[0][1]
				}(i)
			}
			wg.Wait()
			for i := 0; i < n; i++ {
				if got[i] != byte(i) {
					t.Errorf("connection %d was stitched with connection %d's leg", i, got[i])
				}
			}
		})
	}
}

// One raw-leg capture is queued once, so it stitches ONE config mock. The raw
// leg used to push the same capture under a conn-specific key and the port
// key; the connection took one copy and the other stayed behind for 30s, for
// the next stream to the port to take as its own. Two config mocks then
// carried one ReqTimestampMock, the Name+Kind+ReqTimestampMock identity
// treedb.sameMock uses, which breaks replay.
func TestRecordV2_PostTLS_OneCaptureStitchesOneConfigMock(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "400")
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_PRIMARY_MS", "200")
	store := models.NewTLSHandshakeStore()
	const scope = "ns/app/test-set-0"
	dst := &models.ConditionalDstCfg{Addr: "10.244.0.24:3306", Port: 3306}
	owner := models.HandshakeOwner{Conn: "sk:77", Proc: "p1"}
	live := cannedHandshakeV10Variant(t, "8.0.36", 31)
	greeting := decodeGreeting(t, live)
	rawTs := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	raw := newV2Harness(t)
	raw.sess.Opts.PassThroughScope = scope
	raw.sess.Opts.DstCfg = dst
	raw.sess.Opts.ConnKey, raw.sess.Opts.ConnProc = owner.Conn, owner.Proc
	if err := storePreTLSHandshakeV2(postTLSCtxWithStore(store), zap.NewNop(), raw.sess,
		live, cannedSSLRequest(t, 1), rawTs, greetingServerIdentity(greeting)); err != nil {
		t.Fatalf("storePreTLSHandshakeV2: %v", err)
	}

	stream := func(t *testing.T, o models.HandshakeOwner, fresh bool) *models.Mock {
		t.Helper()
		h := newV2Harness(t)
		h.sess.Opts.PassThroughScope = scope
		h.sess.Opts.DstCfg = dst
		h.sess.Opts.ConnKey, h.sess.Opts.ConnProc = o.Conn, o.Proc
		at := time.Now()
		if fresh {
			h.pushClient(cannedHandshakeResponse41(t, 2, false), at)
			h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), at.Add(time.Millisecond))
		}
		h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), at.Add(2*time.Millisecond))
		h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), at.Add(3*time.Millisecond))
		return collectPostTLSMocks(t, h, postTLSCtxWithStore(store), 2, 10*time.Second)[0]
	}
	own := stream(t, owner, true)
	if !own.Spec.ReqTimestampMock.Equal(rawTs) || configGreetingConnID(t, own) != 31 {
		t.Fatalf("the connection's config mock is not stitched from its own leg (ts %v)", own.Spec.ReqTimestampMock)
	}
	if e, ok := store.PopWait(models.HandshakeStoreKey("", 3306), 0); ok {
		t.Fatalf("a copy of the consumed capture is still queued: %v", e)
	}
	for _, o := range []models.HandshakeOwner{{}, {Proc: "p1"}, {Conn: "sk:78", Proc: "p1"}} {
		other := stream(t, o, false)
		if other.Spec.ReqTimestampMock.Equal(own.Spec.ReqTimestampMock) {
			t.Fatalf("another connection (%v) carries the same ReqTimestampMock as the capture's own", o)
		}
	}
}

// Through the real raw-leg push: two connections to one port whose raw legs
// land in one order and whose decrypted streams start in the other. Each
// stream is stitched with its OWN connection's greeting and timestamp, which
// the port's arrival order alone gave to the other connection.
func TestRecordV2_PostTLS_EachStreamGetsItsOwnConnectionsGreeting(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	const scope = "ns/app/test-set-0"
	dst := &models.ConditionalDstCfg{Addr: "10.244.0.24:3306", Port: 3306}
	type conn struct {
		owner models.HandshakeOwner
		id    uint32
		ts    time.Time
	}
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	conns := []conn{
		{models.HandshakeOwner{Conn: "sk:1", Proc: "p1"}, 51, base},
		{models.HandshakeOwner{Conn: "sk:2", Proc: "p1"}, 52, base.Add(time.Millisecond)},
	}
	for _, c := range conns {
		raw := newV2Harness(t)
		raw.sess.Opts.PassThroughScope = scope
		raw.sess.Opts.DstCfg = dst
		raw.sess.Opts.ConnKey, raw.sess.Opts.ConnProc = c.owner.Conn, c.owner.Proc
		g := cannedHandshakeV10Variant(t, "8.0.36", c.id)
		if err := storePreTLSHandshakeV2(postTLSCtxWithStore(store), zap.NewNop(), raw.sess,
			g, cannedSSLRequest(t, 1), c.ts, greetingServerIdentity(decodeGreeting(t, g))); err != nil {
			t.Fatalf("storePreTLSHandshakeV2: %v", err)
		}
	}
	caps := decodeGreeting(t, cannedHandshakeV10Variant(t, "8.0.36", 1)).CapabilityFlags
	for i := len(conns) - 1; i >= 0; i-- {
		c := conns[i]
		h := newV2Harness(t)
		h.sess.Opts.PassThroughScope = scope
		h.sess.Opts.DstCfg = dst
		h.sess.Opts.ConnKey, h.sess.Opts.ConnProc = c.owner.Conn, c.owner.Proc
		at := time.Now()
		h.pushClient(cannedHandshakeResponse41(t, 2, false), at)
		h.pushDest(cannedOK(t, 3, caps), at.Add(time.Millisecond))
		h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), at.Add(2*time.Millisecond))
		h.pushDest(cannedOK(t, 1, caps), at.Add(3*time.Millisecond))
		cfg := collectPostTLSMocks(t, h, postTLSCtxWithStore(store), 2, 10*time.Second)[0]
		if id := configGreetingConnID(t, cfg); id != c.id {
			t.Errorf("connection %s was stitched with greeting %d, want its own %d", c.owner.Conn, id, c.id)
		}
		if !cfg.Spec.ReqTimestampMock.Equal(c.ts) {
			t.Errorf("connection %s: ReqTimestampMock %v, want its own greeting's %v", c.owner.Conn, cfg.Spec.ReqTimestampMock, c.ts)
		}
	}
}

// Two replicas of one deployment share an app/session scope, and each talks to
// its own sidecar on 127.0.0.1:3306. Replica A's raw leg caches its sidecar's
// greeting. Replica B's pooled connection must not borrow it: B's greeting
// comes from B's own sidecar (here a fake server).
func TestRecordV2_PostTLS_ReplicasDoNotShareALoopbackGreeting(t *testing.T) {
	const scope = "ns/app/test-set-0"
	replicaA := cannedHandshakeV10Variant(t, "8.0.36-replica-a", 61)
	replicaB := cannedHandshakeV10Variant(t, "8.0.36-replica-b", 62)
	srvB := startFakeMySQLGreeter(t, replicaB, 0)
	store := models.NewTLSHandshakeStore()

	raw := newV2Harness(t)
	raw.sess.Opts.PassThroughScope = scope
	raw.sess.Opts.NetNS = "netns:replica-a"
	raw.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: srvB.addr, Port: 3306}
	if err := storePreTLSHandshakeV2(postTLSCtxWithStore(store), zap.NewNop(), raw.sess,
		replicaA, cannedSSLRequest(t, 1), time.Now(), greetingServerIdentity(decodeGreeting(t, replicaA))); err != nil {
		t.Fatalf("storePreTLSHandshakeV2: %v", err)
	}

	h := newV2Harness(t)
	h.sess.Opts.PassThroughScope = scope
	h.sess.Opts.NetNS = "netns:replica-b"
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: srvB.addr, Port: 3306}
	caps := decodeGreeting(t, replicaB).CapabilityFlags
	at := time.Now()
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), at)
	h.pushDest(cannedOK(t, 1, caps), at.Add(time.Millisecond))
	cfg := collectPostTLSMocks(t, h, postTLSCtxWithStore(store), 2, 10*time.Second)[0]
	if id := configGreetingConnID(t, cfg); id != 62 {
		t.Fatalf("replica B was stitched with greeting %d, want its own sidecar's 62 (61 is replica A's)", id)
	}
}
