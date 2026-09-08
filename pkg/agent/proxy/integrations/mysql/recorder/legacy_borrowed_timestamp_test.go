package recorder

import (
	"context"
	"net"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestLegacyPostTLS_BorrowedGreetingDoesNotBackdateTheConfigMock pins the
// timing half of the last-greeting fallback.
//
// When this connection's own handshake entry is gone (its capture event
// dropped, or it is a pooled connection whose entry was already consumed),
// the legacy post-TLS reader borrows the last greeting another connection
// recorded for the same destination. Only the PACKETS are reusable that
// way — capabilities, protocol version and auth plugin are per-server. The
// TIMING is not: the borrowed entry's ReqTimestamp belongs to a different
// connection and can be anything up to the store's TTL old.
//
// Adopting it wholesale backdates this connection's config mock. A config
// mock is LifetimeSession so it skips the per-test window, but the
// timestamp is still load-bearing: pkg/util.go sorts the session pool by
// ReqTimestampMock, and treedb.sameMock IDENTIFIES a mock by
// Name+Kind+ReqTimestampMock — so every pooled connection borrowing the
// same cached entry emits mutually indistinguishable config mocks. The V2
// path draws this line explicitly; this test holds the legacy path to it.
func TestLegacyPostTLS_BorrowedGreetingDoesNotBackdateTheConfigMock(t *testing.T) {
	// This test takes ~7s by construction: reaching the cache fallback means
	// missing BOTH PopWait calls in handlePostTLSRecord first (5s on the
	// conn-specific key, then 2s on the port-only key). Those timeouts are
	// hardcoded, so there is no knob to shorten them — and waiting them out
	// is the point, since the fallback is only reachable after they expire.
	store := models.NewTLSHandshakeStore()
	// recordMock reads ClientConnectionIDKey as a string, so the emit path
	// needs it present.
	ctx := context.WithValue(postTLSCtxWithStore(store),
		models.ClientConnectionIDKey, "borrowed-greeting-conn")

	// recordMock delivers through syncMock.FromContextOrGlobal and returns,
	// so a bare mocks channel never sees anything — the mock would go to the
	// package-global manager. Bind a manager of our own with its output
	// channel wired, which is what production does.
	mocks := make(chan *models.Mock, 16)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(mocks)
	ctx = syncMock.NewContext(ctx, mgr)

	scope := "ns/app/ts0"
	// Fabricated, closed port: the direct-dial fallback cannot succeed, so
	// the greeting can only come from the cache.
	dst := &models.ConditionalDstCfg{Addr: "127.0.0.1:1", Port: 3306, AddrFabricated: true}

	// An EARLIER connection's entry, an hour old.
	staleTs := time.Now().Add(-time.Hour)
	store.RememberLast(models.HandshakeLastKey(scope, dst), models.TLSHandshakeEntry{
		RespPackets:  [][]byte{cannedHandshakeV10(t)},
		ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
		ReqTimestamp: staleTs,
	})

	clientConn, peer := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	// A real (immediately closed) destination: the command phase cannot
	// complete, which is fine — the config mock is emitted before that
	// matters — but a nil conn panics rather than failing.
	destConn, destPeer := net.Pipe()
	defer func() { _ = destConn.Close() }()
	_ = destPeer.Close()

	// A seq=0 packet takes the "already authenticated" branch, which emits
	// the synthetic config mock built from the borrowed entry — the shortest
	// route to the timestamp under test.
	go func() {
		_, _ = peer.Write(cannedCOMQuery(t, 0, "SELECT 1"))
		_ = peer.Close()
	}()

	opts := models.OutgoingOptions{
		DstCfg:           dst,
		PassThroughScope: scope,
		ConnKey:          "pooled-conn-whose-own-entry-was-consumed",
	}
	t0 := time.Now()

	// The command phase cannot complete (no destination), but the config
	// mock is emitted before that matters.
	_ = handlePostTLSRecord(ctx, zap.NewNop(), clientConn, destConn, mocks,
		buildPostHandshakeDecodeCtx(clientConn), opts)

	close(mocks)
	var cfg *models.Mock
	for m := range mocks {
		if m != nil && m.Name == "config" {
			cfg = m
			break
		}
	}
	if cfg == nil {
		t.Fatal("no config mock was emitted — the borrowed greeting was not used at all, so " +
			"this test is not exercising the path it claims to")
	}

	if cfg.Spec.ReqTimestampMock.Equal(staleTs) {
		t.Fatalf("the config mock carries the BORROWED connection's request timestamp (%s, %s "+
			"old). Only the greeting PACKETS are reusable across connections; the timing is "+
			"not. It backdates this mock in the session pool's sort order, and because "+
			"treedb.sameMock identifies a mock by Name+Kind+ReqTimestampMock, every pooled "+
			"connection borrowing this same cached entry emits an indistinguishable config mock.",
			staleTs.Format(time.RFC3339), time.Since(staleTs).Truncate(time.Minute))
	}

	// And it must be this connection's own time, not merely "not stale" —
	// bracketed exactly, so a "zero it like V2 does" implementation (which
	// would satisfy the check above) fails here too.
	if got := cfg.Spec.ReqTimestampMock; got.Before(t0) || got.After(time.Now()) {
		t.Errorf("config mock ReqTimestampMock = %s, want a time this connection produced "+
			"(between %s and now)", got.Format(time.RFC3339Nano), t0.Format(time.RFC3339Nano))
	}
}

// The legacy post-TLS reader draws from the same port-only FIFO the V2 path
// does, and an empty ConnKey collapses its conn-specific key onto that shared
// key — so its FIRST PopWait consumes a queue every other post-TLS stream to
// this port is competing for. Popping is destructive: a stream that takes a
// greeting and then fails has starved the connection that pushed it, which
// loses its whole command phase with nothing logged. The entry must go back,
// and without its ReqTimestamp (to the next consumer it is a borrowed entry —
// see TestLegacyPostTLS_BorrowedGreetingDoesNotBackdateTheConfigMock).
func TestLegacyPostTLS_RestoresUnusedSharedGreeting(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	ctx := context.WithValue(postTLSCtxWithStore(store),
		models.ClientConnectionIDKey, "restore-shared-conn")
	mocks := make(chan *models.Mock, 16)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(mocks)
	ctx = syncMock.NewContext(ctx, mgr)

	portKey := models.HandshakeStoreKey("", 3306)
	origTs := time.Now().Add(-time.Minute)
	store.Push(portKey, models.TLSHandshakeEntry{
		RespPackets:  [][]byte{cannedHandshakeV10(t)},
		ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
		ReqTimestamp: origTs,
	})

	// Client closed before any post-TLS packet arrives: the greeting is popped
	// and decoded, then the first-client-packet read fails. That is the measured
	// production shape — a stream whose own bytes never arrive.
	clientConn, peer := net.Pipe()
	_ = peer.Close()
	defer func() { _ = clientConn.Close() }()
	destConn, destPeer := net.Pipe()
	_ = destPeer.Close()
	defer func() { _ = destConn.Close() }()

	// ConnKey empty ⇒ storeKey == portKey ⇒ the pop reads the SHARED FIFO.
	opts := models.OutgoingOptions{
		DstCfg:  &models.ConditionalDstCfg{Addr: "127.0.0.1:1", Port: 3306, AddrFabricated: true},
		ConnKey: "",
	}
	if err := handlePostTLSRecord(ctx, zap.NewNop(), clientConn, destConn, mocks,
		buildPostHandshakeDecodeCtx(clientConn), opts); err == nil {
		t.Fatal("expected the legacy post-TLS record to fail with no client packet")
	}

	restored, ok := store.PopWait(portKey, 0)
	if !ok {
		t.Fatal("a greeting consumed by a FAILED legacy post-TLS attempt was never returned to the " +
			"shared port FIFO; the connection it belonged to is starved")
	}
	if !restored.ReqTimestamp.IsZero() {
		t.Fatalf("restored entry kept ReqTimestamp %v; it would backdate the next connection's config mock",
			restored.ReqTimestamp)
	}
}
