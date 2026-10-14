package recorder

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	connPhase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
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
	// The connection below joined mid-stream (its first packet has seq 0), so
	// handlePostTLSRecord does not wait on either PopWait before reaching the
	// cache: nothing it waited for could serve it better.
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

// The legacy post-TLS reader draws from the same port queue the V2 path does,
// and a stream that cannot name its connection may take another connection's
// entry from it — an entry every other post-TLS stream to this port may be
// competing for. Popping is destructive: a stream that takes a
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
	// Another connection's capture, which this stream (it names no connection)
	// may take.
	owner := models.HandshakeOwner{Conn: "sk:5", Proc: "p1"}
	store.PushFor(portKey, owner, models.TLSHandshakeEntry{
		RespPackets:  [][]byte{cannedHandshakeV10(t)},
		ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
		ReqTimestamp: origTs,
	})

	// A fresh connection's HandshakeResponse41 arrives, so the greeting is
	// popped and decoded, and then the connection is gone before its auth
	// exchange reaches the server.
	clientConn, peer := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	destConn, destPeer := net.Pipe()
	_ = destPeer.Close()
	defer func() { _ = destConn.Close() }()
	go func() {
		_, _ = peer.Write(cannedHandshakeResponse41(t, 2, false))
		_ = peer.Close()
	}()

	// ConnKey empty ⇒ storeKey == portKey ⇒ the pop reads the SHARED FIFO.
	opts := models.OutgoingOptions{
		DstCfg:  &models.ConditionalDstCfg{Addr: "127.0.0.1:1", Port: 3306, AddrFabricated: true},
		ConnKey: "",
	}
	err := handlePostTLSRecord(ctx, zap.NewNop(), clientConn, destConn, mocks,
		buildPostHandshakeDecodeCtx(clientConn), opts)
	if err == nil {
		t.Fatal("expected the legacy post-TLS record to fail when the server leg is gone")
	}
	if strings.Contains(err.Error(), "first post-TLS client packet") {
		t.Fatalf("failed before adopting the greeting (%v); the restore assertion below would be vacuous", err)
	}

	restored, _, ok := store.PopWaitFor(portKey, owner, true, 0)
	if !ok {
		t.Fatal("a greeting consumed by a FAILED legacy post-TLS attempt was never returned to its " +
			"owner; the connection it belonged to is starved")
	}
	if !restored.ReqTimestamp.IsZero() {
		t.Fatalf("restored entry kept ReqTimestamp %v; it would backdate the next connection's config mock",
			restored.ReqTimestamp)
	}
}

// A greeting this connection USED must never go back. handlePostTLSRecord emits
// its own mocks and then returns handleClientQueries, which returns non-nil at
// ordinary teardown (ctx cancellation) — so "err != nil" does NOT mean "no mock
// was produced". Restoring on that basis republishes a spent greeting and lets
// another stream stitch a duplicate config mock from it.
func TestLegacyPostTLS_DoesNotRestoreAGreetingItAlreadyUsed(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	ctx := context.WithValue(postTLSCtxWithStore(store),
		models.ClientConnectionIDKey, "used-greeting-conn")
	mocks := make(chan *models.Mock, 16)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(mocks)
	ctx = syncMock.NewContext(ctx, mgr)
	// Teardown while the command phase is live — the normal end of a recorded
	// connection, and the case that makes err != nil despite a mock existing.
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	handshakeBuf := cannedServerGreeting(t, 41)
	portKey := models.HandshakeStoreKey("", 3306)
	store.Push(portKey, models.TLSHandshakeEntry{
		RespPackets:  [][]byte{handshakeBuf},
		ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
		ReqTimestamp: time.Now(),
	})

	clientConn, clientPeer := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	destConn, destPeer := net.Pipe()
	defer func() { _ = destConn.Close() }()

	// A fresh connection: its HandshakeResponse41 and the server's OK, each as
	// its own side of the observed connection delivers it. The config mock is
	// built FROM the popped greeting, so the greeting is spent, and the
	// connection then idles in its command phase until teardown, which closes
	// both sides as the capture layer does.
	go func() {
		_, _ = clientPeer.Write(cannedHandshakeResponse41(t, 2, false))
		_, _ = destPeer.Write(cannedOK(t, 3, decodeGreeting(t, handshakeBuf).CapabilityFlags))
		<-ctx.Done()
		_ = clientPeer.Close()
		_ = destPeer.Close()
	}()

	err := handlePostTLSRecord(ctx, zap.NewNop(), clientConn, destConn, mocks,
		buildPostHandshakeDecodeCtx(clientConn), models.OutgoingOptions{
			DstCfg:  &models.ConditionalDstCfg{Addr: "127.0.0.1:1", Port: 3306, AddrFabricated: true},
			ConnKey: "",
		})
	if err == nil {
		t.Fatal("expected the teardown to end the command phase with an error")
	}
	var cfg bool
	close(mocks)
	for m := range mocks {
		cfg = cfg || (m != nil && m.Name == "config")
	}
	if !cfg {
		t.Fatalf("no config mock was recorded (err %v); the greeting was never used, so this test proves nothing", err)
	}
	if _, ok := store.PopWait(portKey, 0); ok {
		t.Fatal("a greeting that was already used to build a config mock was pushed back into the " +
			"shared FIFO; another stream would stitch a duplicate config mock from it")
	}
}

// A greeting that DECODES but is not a HandshakeV10 must not be recycled either.
// GetPluginName rejects it before the HandshakeV10 assertion, so that error path
// needs its own guard; Push re-stamps the expiry, so a recycled entry would
// never age out and would trip every later stream to this port.
func TestLegacyPostTLS_DoesNotRecycleDecodableNonHandshakeGreeting(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	ctx := context.WithValue(postTLSCtxWithStore(store),
		models.ClientConnectionIDKey, "non-handshake-conn")
	mocks := make(chan *models.Mock, 16)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(mocks)
	ctx = syncMock.NewContext(ctx, mgr)

	portKey := models.HandshakeStoreKey("", 3306)
	// Decodes cleanly, but it is an OK packet, not a handshake. The capability
	// flags must be REAL: with 0 the packet is too short to decode and the test
	// would stop at the decode guard instead of reaching GetPluginName, which is
	// the path under test.
	store.Push(portKey, models.TLSHandshakeEntry{
		RespPackets: [][]byte{cannedOK(t, 1, 0x000FA68D)},
	})

	clientConn, peer := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	destConn, destPeer := net.Pipe()
	_ = destPeer.Close()
	defer func() { _ = destConn.Close() }()
	// The stream's own first packet, so it goes on to adopt the entry.
	go func() {
		_, _ = peer.Write(cannedHandshakeResponse41(t, 2, false))
		_ = peer.Close()
	}()

	err := handlePostTLSRecord(ctx, zap.NewNop(), clientConn, destConn, mocks,
		buildPostHandshakeDecodeCtx(clientConn), models.OutgoingOptions{
			DstCfg:  &models.ConditionalDstCfg{Addr: "127.0.0.1:1", Port: 3306, AddrFabricated: true},
			ConnKey: "",
		})
	if err == nil {
		t.Fatal("expected a non-handshake greeting to fail the legacy post-TLS record")
	}
	// Pin the path: it must fail at GetPluginName, which runs BEFORE the
	// HandshakeV10 assertion and therefore needs its own guard.
	if !strings.Contains(err.Error(), "get plugin name") {
		t.Fatalf("failed on a different path (%v); this test must exercise GetPluginName", err)
	}
	if _, ok := store.PopWait(portKey, 0); ok {
		t.Fatal("a decodable non-handshake greeting was recycled into the shared FIFO; Push re-stamps " +
			"its expiry so it would never age out and would trip every later stream to this port")
	}
}

// A legacy post-TLS stream whose own bytes never arrive must not touch the
// shared port FIFO: it reads its first packet before it looks for a greeting,
// so the entry stays exactly as captured, own timestamp included, for the
// stream that needs it.
func TestLegacyPostTLS_StreamWithoutBytesLeavesTheSharedGreetingUntouched(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	ctx := context.WithValue(postTLSCtxWithStore(store),
		models.ClientConnectionIDKey, "no-bytes-conn")
	portKey := models.HandshakeStoreKey("", 3306)
	origTs := time.Now().Add(-time.Minute)
	store.Push(portKey, models.TLSHandshakeEntry{
		RespPackets:  [][]byte{cannedHandshakeV10(t)},
		ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
		ReqTimestamp: origTs,
	})

	clientConn, peer := net.Pipe()
	_ = peer.Close()
	defer func() { _ = clientConn.Close() }()
	destConn, destPeer := net.Pipe()
	_ = destPeer.Close()
	defer func() { _ = destConn.Close() }()

	err := handlePostTLSRecord(ctx, zap.NewNop(), clientConn, destConn, make(chan *models.Mock, 1),
		buildPostHandshakeDecodeCtx(clientConn), models.OutgoingOptions{
			DstCfg: &models.ConditionalDstCfg{Addr: "127.0.0.1:1", Port: 3306, AddrFabricated: true},
		})
	if err == nil || !strings.Contains(err.Error(), "first post-TLS client packet") {
		t.Fatalf("err = %v, want the first-client-packet read to fail", err)
	}
	got, ok := store.PopWait(portKey, 0)
	if !ok {
		t.Fatal("a stream with no bytes of its own consumed the shared greeting")
	}
	if !got.ReqTimestamp.Equal(origTs) {
		t.Fatalf("ReqTimestamp = %v, want the entry's own %v: it was popped and pushed back", got.ReqTimestamp, origTs)
	}
}

// The legacy reader keeps V2's rules for a connection that joined mid-stream: it
// takes from the port's queue only an entry that is provably its own (same
// ConnKey on both legs), and otherwise borrows the app-scoped cache. Anything
// else on the queue may be a fresh connection's own leg that has not reached
// its decrypted stream yet, the normal ordering: the raw leg's greeting and
// SSLRequest precede the TLS handshake, which precedes the first decrypted
// packet. Taking it robbed that connection of its SSLRequest, salt and
// timestamp.
func TestLegacyPostTLS_JoinedConnTakesOnlyItsOwnEntry(t *testing.T) {
	portKey := models.HandshakeStoreKey("", 3306)
	dst := &models.ConditionalDstCfg{Addr: "10.0.0.5:3306", Port: 3306}
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
	ownLeg := entryOf("8.0.36", 23, time.Now().Add(-2*time.Second))
	run := func(t *testing.T, store *models.TLSHandshakeStore, owner models.HandshakeOwner) *models.Mock {
		t.Helper()
		ctx := context.WithValue(postTLSCtxWithStore(store), models.ClientConnectionIDKey, "joined-conn")
		mocks := make(chan *models.Mock, 16)
		mgr := syncMock.New(zap.NewNop())
		mgr.SetOutputChannel(mocks)
		ctx = syncMock.NewContext(ctx, mgr)
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		clientConn, clientPeer := net.Pipe()
		defer func() { _ = clientConn.Close() }()
		destConn, destPeer := net.Pipe()
		defer func() { _ = destConn.Close() }()
		go func() {
			buf := make([]byte, 4096)
			_, _ = clientPeer.Write(cannedCOMQuery(t, 0, "SELECT 1"))
			_, _ = destPeer.Read(buf)
			_, _ = destPeer.Write(cannedOK(t, 1, decodeGreeting(t, cached.RespPackets[0]).CapabilityFlags))
			_, _ = clientPeer.Read(buf)
			_ = clientPeer.Close()
			_ = destPeer.Close()
		}()
		start := time.Now()
		_ = handlePostTLSRecord(ctx, zap.NewNop(), clientConn, destConn, mocks,
			buildPostHandshakeDecodeCtx(clientConn), models.OutgoingOptions{
				DstCfg: dst, PassThroughScope: scope, ConnKey: owner.Conn, ConnProc: owner.Proc,
			})
		if d := time.Since(start); d > time.Second {
			t.Fatalf("the joined connection took %v; it waited", d)
		}
		close(mocks)
		for m := range mocks {
			if m != nil && m.Name == "config" {
				return m
			}
		}
		t.Fatal("no config mock")
		return nil
	}
	greetingID := func(m *models.Mock) uint32 {
		for _, r := range m.Spec.MySQLResponses {
			if g, ok := r.Message.(*mysql.HandshakeV10Packet); ok {
				return g.ConnectionID
			}
		}
		return 0
	}
	queued := func(store *models.TLSHandshakeStore) int {
		n := 0
		for {
			if _, ok := store.PopWait(portKey, 0); !ok {
				return n
			}
			n++
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
		t.Run("a fresh connection's leg is left to it: "+tc.name, func(t *testing.T) {
			store := models.NewTLSHandshakeStore()
			store.RememberLast(models.HandshakeLastKey(scope, dst), cached) // an earlier raw leg
			store.PushFor(portKey, tc.fresh, freshLeg)
			cfg := run(t, store, tc.owner)
			if id := greetingID(cfg); id != 21 {
				t.Fatalf("stitched greeting %d, want the cached 21: it took a fresh connection's leg", id)
			}
			if cfg.Spec.ReqTimestampMock.Equal(cached.ReqTimestamp) {
				t.Fatal("the joined connection's config mock carries the borrowed entry's timestamp")
			}
			if n := queued(store); n != 1 {
				t.Fatalf("%d entries left on the port's queue, want the fresh connection's 1", n)
			}
		})
	}

	t.Run("its own entry is taken, and only that", func(t *testing.T) {
		store := models.NewTLSHandshakeStore()
		store.RememberLast(models.HandshakeLastKey(scope, dst), cached)
		store.PushFor(portKey, models.HandshakeOwner{Conn: "sk:6", Proc: "p1"}, freshLeg)
		store.PushFor(portKey, models.HandshakeOwner{Conn: "sk:5", Proc: "p1"}, ownLeg)
		cfg := run(t, store, models.HandshakeOwner{Conn: "sk:5", Proc: "p1"})
		if id := greetingID(cfg); id != 23 {
			t.Fatalf("stitched greeting %d, want its own 23", id)
		}
		if !cfg.Spec.ReqTimestampMock.Equal(ownLeg.ReqTimestamp) {
			t.Fatalf("ReqTimestampMock = %v, want its own greeting's %v", cfg.Spec.ReqTimestampMock, ownLeg.ReqTimestamp)
		}
		e, taker, ok := store.PopWaitFor(portKey, models.HandshakeOwner{}, false, 0)
		if !ok || taker.Conn != "sk:6" || !e.ReqTimestamp.Equal(freshLeg.ReqTimestamp) {
			t.Fatalf("queue head = %v (owner %v, %v), want the other connection's entry untouched", e, taker, ok)
		}
	})
}

// cannedServerGreeting is a greeting as a server sends it: a 20-byte salt plus
// its NUL, so the legacy reader decodes its auth plugin and a fresh
// connection's auth exchange completes.
func cannedServerGreeting(t *testing.T, connID uint32) []byte {
	t.Helper()
	raw, err := connPhase.EncodeHandshakeV10(context.Background(), zap.NewNop(), &mysql.HandshakeV10Packet{
		ProtocolVersion: 0x0a,
		ServerVersion:   "8.0.36",
		ConnectionID:    connID,
		AuthPluginData:  append(bytes.Repeat([]byte{0x22}, 20), 0),
		CapabilityFlags: uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SSL | mysql.CLIENT_SECURE_CONNECTION),
		CharacterSet:    0x21,
		StatusFlags:     0x02,
		AuthPluginName:  string(mysql.Native),
	})
	if err != nil {
		t.Fatal(err)
	}
	return wrapPacket(raw, 0)
}

// The legacy reader pairs a fresh connection with its OWN raw leg, as V2 does:
// two connections to one port whose raw legs were queued in one order and whose
// decrypted streams arrive in the other. Arrival order alone crossed them.
func TestLegacyPostTLS_EachStreamGetsItsOwnConnectionsGreeting(t *testing.T) {
	store := models.NewTLSHandshakeStore()
	portKey := models.HandshakeStoreKey("", 3306)
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	type conn struct {
		owner models.HandshakeOwner
		id    uint32
		ts    time.Time
	}
	conns := []conn{
		{models.HandshakeOwner{Conn: "sk:1", Proc: "p1"}, 71, base},
		{models.HandshakeOwner{Conn: "sk:2", Proc: "p1"}, 72, base.Add(time.Millisecond)},
	}
	for _, c := range conns {
		store.PushFor(portKey, c.owner, models.TLSHandshakeEntry{
			RespPackets:  [][]byte{cannedServerGreeting(t, c.id)},
			ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
			ReqTimestamp: c.ts,
		})
	}
	for i := len(conns) - 1; i >= 0; i-- {
		c := conns[i]
		mocks := make(chan *models.Mock, 16)
		mgr := syncMock.New(zap.NewNop())
		mgr.SetOutputChannel(mocks)
		ctx := syncMock.NewContext(context.WithValue(postTLSCtxWithStore(store),
			models.ClientConnectionIDKey, c.owner.Conn), mgr)
		ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		clientConn, clientPeer := net.Pipe()
		destConn, destPeer := net.Pipe()
		caps := decodeGreeting(t, cannedServerGreeting(t, c.id)).CapabilityFlags
		go func() {
			_, _ = clientPeer.Write(cannedHandshakeResponse41(t, 2, false))
			_, _ = destPeer.Write(cannedOK(t, 3, caps))
			<-ctx.Done()
			_ = clientPeer.Close()
			_ = destPeer.Close()
		}()
		_ = handlePostTLSRecord(ctx, zap.NewNop(), clientConn, destConn, mocks,
			buildPostHandshakeDecodeCtx(clientConn), models.OutgoingOptions{
				DstCfg:  &models.ConditionalDstCfg{Addr: "10.0.0.5:3306", Port: 3306},
				ConnKey: c.owner.Conn, ConnProc: c.owner.Proc,
			})
		cancel()
		_ = clientConn.Close()
		_ = destConn.Close()
		close(mocks)
		var cfg *models.Mock
		for m := range mocks {
			if m != nil && m.Name == "config" {
				cfg = m
			}
		}
		if cfg == nil {
			t.Fatalf("connection %s recorded no config mock", c.owner.Conn)
		}
		var got uint32
		for _, r := range cfg.Spec.MySQLResponses {
			if g, ok := r.Message.(*mysql.HandshakeV10Packet); ok {
				got = g.ConnectionID
			}
		}
		if got != c.id || !cfg.Spec.ReqTimestampMock.Equal(c.ts) {
			t.Errorf("connection %s: stitched with greeting %d at %v, want its own %d at %v",
				c.owner.Conn, got, cfg.Spec.ReqTimestampMock, c.id, c.ts)
		}
	}
}
