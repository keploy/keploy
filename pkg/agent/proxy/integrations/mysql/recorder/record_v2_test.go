package recorder

import (
	"context"
	"net"
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
	store.Push(models.HandshakeStoreKey("", 3306), models.TLSHandshakeEntry{
		RespPackets:  [][]byte{handshakeBuf},
		ReqPackets:   [][]byte{sslReq},
		ReqTimestamp: staleTs,
	})
	if _, ok := store.PopWait(models.HandshakeStoreKey("", 3306), 0); !ok {
		t.Fatal("setup: expected to consume the earlier connection's entry")
	}

	// The degraded proxyless dest is fabricated — the fallback must succeed
	// WITHOUT dialing it (a dial would fail: nothing listens on this port).
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: "127.0.0.1:1", Port: 3306, AddrFabricated: true}

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
	_, source := resolvePreTLSGreeting(ctx, store, "", 3306)
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
	store.Push(models.HandshakeStoreKey("", 3306), models.TLSHandshakeEntry{
		RespPackets:  [][]byte{[]byte("greeting")},
		ReqPackets:   [][]byte{[]byte("sslreq")},
		ReqTimestamp: time.Now(),
	})
	if _, ok := store.PopWait(models.HandshakeStoreKey("", 3306), 0); !ok {
		t.Fatal("setup: expected to consume the sibling's queued entry, leaving only the cache")
	}

	start := time.Now()
	entry, source := resolvePreTLSGreeting(context.Background(), store, "", 3306)
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
	store.Push(models.HandshakeStoreKey("", 3306), models.TLSHandshakeEntry{
		RespPackets: [][]byte{[]byte("sibling-greeting")},
	})
	if _, ok := store.PopWait(models.HandshakeStoreKey("", 3306), 0); !ok {
		t.Fatal("setup: expected to consume the sibling's queued entry")
	}
	ownTs := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	go func() {
		time.Sleep(150 * time.Millisecond)
		store.Push(models.HandshakeStoreKey("", 3306), models.TLSHandshakeEntry{
			RespPackets:  [][]byte{[]byte("own-greeting")},
			ReqTimestamp: ownTs,
		})
	}()

	entry, source := resolvePreTLSGreeting(context.Background(), store, "", 3306)
	if source != greetingOwn {
		t.Fatalf("source = %v, want greetingOwn (a late own leg within the primary bound beats the cache)", source)
	}
	if !entry.ReqTimestamp.Equal(ownTs) {
		t.Errorf("ReqTimestamp = %v, want the own leg's %v", entry.ReqTimestamp, ownTs)
	}
}
