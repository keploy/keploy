package recorder

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// wrapPacket prepends the 4-byte MySQL packet header (3-byte little-
// endian payload length + 1-byte sequence id) to payload.
func wrapPacket(payload []byte, seq byte) []byte {
	out := make([]byte, 4+len(payload))
	n := uint32(len(payload))
	out[0] = byte(n)
	out[1] = byte(n >> 8)
	out[2] = byte(n >> 16)
	out[3] = seq
	copy(out[4:], payload)
	return out
}

// cannedHandshakeV10 returns an encoded HandshakeV10 packet advertising
// mysql_native_password auth. Using Native avoids the additional auth-
// more-data round trip needed by caching_sha2 in the happy test.
func cannedHandshakeV10(t *testing.T) []byte {
	t.Helper()
	caps := uint32(mysql.CLIENT_PROTOCOL_41 |
		mysql.CLIENT_PLUGIN_AUTH |
		mysql.CLIENT_SSL |
		mysql.CLIENT_SECURE_CONNECTION)
	hs := &mysql.HandshakeV10Packet{
		ProtocolVersion: 0x0a,
		ServerVersion:   "8.0.test-keploy",
		ConnectionID:    42,
		// 20-byte auth plugin data (8 part1 + 1 filler-ignored + 12 part2
		// plus terminator).
		AuthPluginData:  bytes.Repeat([]byte{0x11}, 20),
		Filler:          0x00,
		CapabilityFlags: caps,
		CharacterSet:    0x21, // utf8_general_ci
		StatusFlags:     0x02,
		AuthPluginName:  string(mysql.Native),
	}
	buf, err := connphase.EncodeHandshakeV10(context.Background(), zap.NewNop(), hs)
	if err != nil {
		t.Fatalf("encode handshake v10: %v", err)
	}
	return wrapPacket(buf, 0)
}

// cannedHandshakeResponse41 returns a full HandshakeResponse41 packet
// (Native auth). sslBit toggles whether CLIENT_SSL is advertised.
func cannedHandshakeResponse41(t *testing.T, seq byte, sslBit bool) []byte {
	t.Helper()
	caps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION)
	if sslBit {
		caps |= mysql.CLIENT_SSL
	}
	hr := &mysql.HandshakeResponse41Packet{
		CapabilityFlags: caps,
		MaxPacketSize:   1 << 24,
		CharacterSet:    0x21,
		Username:        "root",
		AuthResponse:    bytes.Repeat([]byte{0xAB}, 20),
		AuthPluginName:  string(mysql.Native),
	}
	buf, err := connphase.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), hr)
	if err != nil {
		t.Fatalf("encode handshake response 41: %v", err)
	}
	return wrapPacket(buf, seq)
}

// cannedSSLRequest returns a short-form SSLRequest (32-byte body).
// This signals the server the client wants TLS before sending
// credentials.
func cannedSSLRequest(t *testing.T, seq byte) []byte {
	t.Helper()
	body := make([]byte, 32)
	caps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_SSL | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_PLUGIN_AUTH)
	binary.LittleEndian.PutUint32(body[0:4], caps)
	binary.LittleEndian.PutUint32(body[4:8], 1<<24)
	body[8] = 0x21 // charset
	// remaining 23 bytes already zero
	return wrapPacket(body, seq)
}

// cannedOK returns an OK packet with the given sequence number.
func cannedOK(t *testing.T, seq byte, serverCaps uint32) []byte {
	t.Helper()
	ok := &mysql.OKPacket{Header: mysql.OK, StatusFlags: 2}
	payload, err := phase.EncodeOk(context.Background(), ok, serverCaps)
	if err != nil {
		t.Fatalf("encode ok: %v", err)
	}
	return wrapPacket(payload, seq)
}

// cannedCOMQuery returns a COM_QUERY command packet.
func cannedCOMQuery(_ *testing.T, seq byte, query string) []byte {
	body := make([]byte, 1+len(query))
	body[0] = mysql.COM_QUERY
	copy(body[1:], query)
	return wrapPacket(body, seq)
}

// v2Harness stitches together a fake supervisor.Session driven by
// explicit chunk channels. Callers push chunks to feed ClientStream
// (client→dest bytes) and DestStream (dest→client bytes).
type v2Harness struct {
	t        *testing.T
	logger   *zap.Logger
	clientCh chan fakeconn.Chunk
	destCh   chan fakeconn.Chunk
	mocks    chan *models.Mock
	dirs     chan directive.Directive
	acks     chan directive.Ack
	sess     *supervisor.Session
}

func newV2Harness(t *testing.T) *v2Harness {
	t.Helper()
	h := &v2Harness{
		t:        t,
		logger:   zaptest.NewLogger(t),
		clientCh: make(chan fakeconn.Chunk, 64),
		destCh:   make(chan fakeconn.Chunk, 64),
		mocks:    make(chan *models.Mock, 32),
		dirs:     make(chan directive.Directive, 4),
		acks:     make(chan directive.Ack, 4),
	}
	clientFC := fakeconn.New(h.clientCh, nil, nil)
	destFC := fakeconn.New(h.destCh, nil, nil)
	h.sess = &supervisor.Session{
		ClientStream: clientFC,
		DestStream:   destFC,
		Directives:   h.dirs,
		Acks:         h.acks,
		Mocks:        h.mocks,
		Logger:       h.logger,
		Ctx:          context.Background(),
		ClientConnID: "test-client-1",
		DestConnID:   "test-dest-1",
		Opts: models.OutgoingOptions{
			DstCfg: &models.ConditionalDstCfg{Addr: "127.0.0.1:3306", Port: 3306},
		},
	}
	return h
}

func (h *v2Harness) pushClient(payload []byte, ts time.Time) {
	h.clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: append([]byte(nil), payload...), ReadAt: ts, WrittenAt: ts}
}

func (h *v2Harness) pushDest(payload []byte, ts time.Time) {
	h.destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: append([]byte(nil), payload...), ReadAt: ts, WrittenAt: ts}
}

func (h *v2Harness) closeStreams() {
	close(h.clientCh)
	close(h.destCh)
}

func TestRecordV2_HappyPath_HandshakeAndOneQuery(t *testing.T) {
	t.Parallel()
	h := newV2Harness(t)

	base := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)

	// Build the server greeting so we can compute its capability flags
	// for the OK encoder. Decode our own canned handshake to get caps.
	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}

	// -- Handshake phase ------------------------------------------------
	h.pushDest(handshakeBuf, base)
	h.pushClient(cannedHandshakeResponse41(t, 1, false), base.Add(5*time.Millisecond))
	// Native auth → server replies with OK.
	h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), base.Add(10*time.Millisecond))

	// -- Command phase: one COM_QUERY → OK -----------------------------
	queryTs := base.Add(20 * time.Millisecond)
	queryRespTs := base.Add(25 * time.Millisecond)
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), queryTs)
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), queryRespTs)

	// Drive the recorder; close streams when we're done pushing so it
	// exits cleanly via io.EOF.
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- RecordV2(ctx, h.logger, h.sess)
	}()

	// Collect mocks; we expect two: one "config" for the handshake and
	// one "mocks" for the query.
	var got []*models.Mock
collect:
	for len(got) < 2 {
		select {
		case m, ok := <-h.mocks:
			if !ok {
				break collect
			}
			got = append(got, m)
			if len(got) == 2 {
				// Signal EOF so the parser exits now that both mocks
				// are in hand. Additional reads after this should
				// return io.EOF.
				h.closeStreams()
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for mocks (got %d)", len(got))
		}
	}

	if err := <-done; err != nil {
		t.Fatalf("RecordV2 returned error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 mocks, got %d", len(got))
	}

	// -- Assertions on the config mock ----------------------------------
	cfg := got[0]
	if cfg.Kind != models.MySQL {
		t.Errorf("config mock kind = %v, want %v", cfg.Kind, models.MySQL)
	}
	if cfg.Name != "config" {
		t.Errorf("config mock name = %q, want %q", cfg.Name, "config")
	}
	if cfg.Spec.Metadata["type"] != "config" {
		t.Errorf("config mock metadata type = %q, want config", cfg.Spec.Metadata["type"])
	}
	if cfg.Spec.Metadata["connID"] != "test-client-1" {
		t.Errorf("config mock connID = %q, want test-client-1", cfg.Spec.Metadata["connID"])
	}
	if cfg.Spec.Metadata["destAddr"] != "127.0.0.1:3306" {
		t.Errorf("config mock destAddr = %q, want 127.0.0.1:3306", cfg.Spec.Metadata["destAddr"])
	}
	if cfg.Spec.ReqTimestampMock.IsZero() || cfg.Spec.ResTimestampMock.IsZero() {
		t.Errorf("config mock timestamps must be non-zero: req=%v res=%v",
			cfg.Spec.ReqTimestampMock, cfg.Spec.ResTimestampMock)
	}
	if cfg.Spec.ResTimestampMock.Before(cfg.Spec.ReqTimestampMock) {
		t.Errorf("config mock res (%v) before req (%v)",
			cfg.Spec.ResTimestampMock, cfg.Spec.ReqTimestampMock)
	}
	if len(cfg.Spec.MySQLRequests) < 1 {
		t.Fatalf("config mock requests = %d, want >=1", len(cfg.Spec.MySQLRequests))
	}
	if len(cfg.Spec.MySQLResponses) < 2 {
		t.Fatalf("config mock responses = %d, want >=2 (HandshakeV10 + OK)", len(cfg.Spec.MySQLResponses))
	}
	if got, want := cfg.Spec.MySQLResponses[0].PacketBundle.Header.Type, "HandshakeV10"; got != want {
		t.Errorf("first config response type = %q, want %q", got, want)
	}

	// -- Assertions on the query mock ----------------------------------
	qm := got[1]
	if qm.Name != "mocks" {
		t.Errorf("query mock name = %q, want %q", qm.Name, "mocks")
	}
	if qm.Spec.Metadata["requestOperation"] != "COM_QUERY" {
		t.Errorf("query mock requestOperation = %q, want COM_QUERY", qm.Spec.Metadata["requestOperation"])
	}
	if qm.Spec.Metadata["responseOperation"] != mysql.StatusToString(mysql.OK) {
		t.Errorf("query mock responseOperation = %q, want OK", qm.Spec.Metadata["responseOperation"])
	}
	if !qm.Spec.ReqTimestampMock.Equal(queryTs) {
		t.Errorf("query mock ReqTimestampMock = %v, want %v", qm.Spec.ReqTimestampMock, queryTs)
	}
	if !qm.Spec.ResTimestampMock.Equal(queryRespTs) {
		t.Errorf("query mock ResTimestampMock = %v, want %v", qm.Spec.ResTimestampMock, queryRespTs)
	}
}

func TestRecordV2_TLSUpgrade_DirectiveAndResume(t *testing.T) {
	t.Parallel()
	h := newV2Harness(t)

	base := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)

	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake: %v", err)
	}

	// Server greeting.
	h.pushDest(handshakeBuf, base)
	// Short-form SSLRequest (CLIENT_SSL bit set, 32-byte body).
	h.pushClient(cannedSSLRequest(t, 1), base.Add(1*time.Millisecond))

	// After the parser gets the ack, it reads the post-TLS
	// HandshakeResponse41 (with credentials) then the server's final OK.
	h.pushClient(cannedHandshakeResponse41(t, 2, true), base.Add(20*time.Millisecond))
	h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), base.Add(25*time.Millisecond))

	// Auto-ack the directive from a side goroutine.
	dirSeen := make(chan directive.Directive, 1)
	go func() {
		select {
		case d := <-h.dirs:
			dirSeen <- d
			h.acks <- directive.Ack{Kind: d.Kind, OK: true, BoundaryReadAt: base.Add(10 * time.Millisecond), BoundaryWrittenAt: base.Add(15 * time.Millisecond)}
		case <-time.After(2 * time.Second):
			t.Errorf("parser never sent a directive")
		}
	}()

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- RecordV2(ctx, h.logger, h.sess)
	}()

	// Expect exactly one mock (the config mock).
	select {
	case m := <-h.mocks:
		if m.Name != "config" {
			t.Errorf("mock name = %q, want config", m.Name)
		}
		// Must include the SSLRequest + post-TLS HandshakeResponse41.
		if len(m.Spec.MySQLRequests) < 2 {
			t.Errorf("TLS config mock requests = %d, want >=2 (SSLRequest + HandshakeResponse41)", len(m.Spec.MySQLRequests))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS config mock")
	}

	// Verify the directive surfaced.
	select {
	case d := <-dirSeen:
		if d.Kind != directive.KindUpgradeTLS {
			t.Errorf("directive kind = %v, want KindUpgradeTLS", d.Kind)
		}
		if d.TLS == nil || d.TLS.DestTLSConfig == nil {
			t.Errorf("expected TLS params with DestTLSConfig, got %+v", d.TLS)
		}
	default:
		t.Fatal("directive never observed")
	}

	// Close streams so the parser returns.
	h.closeStreams()
	if err := <-done; err != nil {
		t.Errorf("RecordV2 returned error: %v", err)
	}
}

func TestRecordV2_TLSUpgrade_FailureMarksIncompleteAndReturnsErr(t *testing.T) {
	t.Parallel()
	h := newV2Harness(t)

	base := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)

	h.pushDest(cannedHandshakeV10(t), base)
	h.pushClient(cannedSSLRequest(t, 1), base.Add(1*time.Millisecond))

	// Reply to the directive with OK=false.
	go func() {
		select {
		case d := <-h.dirs:
			h.acks <- directive.Ack{Kind: d.Kind, OK: false, Err: errFakeTLSFail}
		case <-time.After(2 * time.Second):
			t.Errorf("parser never sent a directive")
		}
	}()

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- RecordV2(ctx, h.logger, h.sess)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected RecordV2 to return an error on TLS upgrade failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RecordV2 to return")
	}

	if !h.sess.IsMockIncomplete() {
		t.Error("expected session to be marked mock incomplete after TLS upgrade failure")
	}

	// Ensure no mocks were emitted (incomplete gate drops them).
	select {
	case m := <-h.mocks:
		t.Errorf("unexpected mock emitted after TLS failure: %+v", m)
	default:
	}
}

// errFakeTLSFail is a sentinel used by the failure test.
var errFakeTLSFail = fakeTLSErr("fake handshake failure")

type fakeTLSErr string

func (e fakeTLSErr) Error() string { return string(e) }

// postTLSCtx builds a context wired the way the enterprise SSL/GoTLS reader
// callback wires it for a decrypted tls-* stream: PostTLSModeKey set, and a
// TLSHandshakeStore seeded with the pre-TLS greeting (+ SSLRequest) under the
// port-only key — exactly what handlePostTLSHandshakeV2 pops.
func postTLSCtx(t *testing.T, greeting, sslRequest []byte, reqTs time.Time, dstPort uint16) context.Context {
	t.Helper()
	store := models.NewTLSHandshakeStore()
	store.Push(models.HandshakeStoreKey("", dstPort), models.TLSHandshakeEntry{
		RespPackets:  [][]byte{greeting},
		ReqPackets:   [][]byte{sslRequest},
		ReqTimestamp: reqTs,
	})
	ctx := context.WithValue(context.Background(), models.PostTLSModeKey, true)
	ctx = context.WithValue(ctx, models.TLSHandshakeStoreKey, store)
	return ctx
}

// TestRecordV2_PostTLS_FreshConn covers the decrypted tls-* stream for a fresh
// connection (HandshakeResponse41, seq>=1): the greeting is popped from the
// store instead of read off DestStream, then auth + one query are recorded via
// the normal V2 command loop.
func TestRecordV2_PostTLS_FreshConn(t *testing.T) {
	t.Parallel()
	h := newV2Harness(t)
	base := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)

	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}
	sslReq := cannedSSLRequest(t, 1)

	// Decrypted stream: HandshakeResponse41 (seq>=1) then a query. No greeting
	// on DestStream — the auth OK is the first dest packet.
	h.pushClient(cannedHandshakeResponse41(t, 2, false), base.Add(5*time.Millisecond))
	h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), base.Add(10*time.Millisecond))
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), base.Add(20*time.Millisecond))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(25*time.Millisecond))

	got := runPostTLS(t, h, postTLSCtx(t, handshakeBuf, sslReq, base, 3306), 2)

	if got[0].Name != "config" {
		t.Errorf("first mock = %q, want config", got[0].Name)
	}
	// Config mock must carry the popped greeting as a response.
	if len(got[0].Spec.MySQLResponses) < 1 {
		t.Fatalf("config mock has no responses (greeting not seeded from store)")
	}
	if got[0].Spec.ResTimestampMock.Before(got[0].Spec.ReqTimestampMock) {
		t.Errorf("config res before req (clamp/order broken): req=%v res=%v", got[0].Spec.ReqTimestampMock, got[0].Spec.ResTimestampMock)
	}
	assertQueryMock(t, got[1])
}

// TestRecordV2_PostTLS_PooledConnSeq0 covers the pre-warmed pool case: the
// decrypted stream is joined mid-command (seq==0, no HandshakeResponse41). The
// first packet is a command, which must be threaded into the command loop via
// firstCmd — and LastOp must be reset so it decodes as a command, not a
// handshake response (the Copilot-flagged bug).
func TestRecordV2_PostTLS_PooledConnSeq0(t *testing.T) {
	t.Parallel()
	h := newV2Harness(t)
	base := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)

	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}
	sslReq := cannedSSLRequest(t, 1)

	// No HandshakeResponse41 — first client packet is a COM_QUERY (seq==0).
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), base.Add(20*time.Millisecond))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(25*time.Millisecond))

	got := runPostTLS(t, h, postTLSCtx(t, handshakeBuf, sslReq, base, 3306), 2)

	if got[0].Name != "config" {
		t.Errorf("first mock = %q, want config", got[0].Name)
	}
	// The query mock proves the seq==0 firstCmd was decoded as a COMMAND (not
	// mis-decoded as a handshake response because LastOp stayed HandshakeV10).
	assertQueryMock(t, got[1])
}

// runPostTLS drives RecordV2 with the given post-TLS ctx and collects want mocks.
func runPostTLS(t *testing.T, h *v2Harness, ctx context.Context, want int) []*models.Mock {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
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
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for mocks (got %d, want %d)", len(got), want)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("RecordV2 (post-TLS) returned error: %v", err)
	}
	return got
}

// assertQueryMock checks a mock is the recorded COM_QUERY exchange.
func assertQueryMock(t *testing.T, m *models.Mock) {
	t.Helper()
	if m.Kind != models.MySQL {
		t.Errorf("query mock kind = %v, want MySQL", m.Kind)
	}
	if len(m.Spec.MySQLRequests) < 1 {
		t.Fatalf("query mock has no requests")
	}
	if got := m.Spec.MySQLRequests[0].Header.Type; got != "COM_QUERY" {
		t.Errorf("query mock request type = %q, want COM_QUERY (seq==0 firstCmd mis-decoded as handshake?)", got)
	}
}
