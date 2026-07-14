package recorder

import (
	"context"
	"testing"
	"time"

	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// These tests reproduce the customer failure where non-TLS MySQL recording
// captured 0 mocks: the DS-mode proxyless eBPF path joins a connection that
// was opened before recording started (a pre-warmed JDBC pool), so the first
// server packet it sees is a query response, NOT the HandshakeV10 greeting.
// The parser needs the greeting (server capability flags) to decode anything,
// so it returned "server Greetings not found" and the whole connection's
// recording was aborted with three ERROR logs.
//
// A connection we joined mid-stream is genuinely un-recordable (we can never
// reconstruct the greeting after the fact), so the correct behavior is to skip
// it gracefully — return nil, emit no mock, no ERROR — while surfacing a single
// WARN so the "0 mocks" symptom is explained. Connections captured from their
// first byte are unaffected: their greeting is present and recording proceeds.

// midStreamServerPacket returns a server-side OK packet (payload[0] == 0x00),
// standing in for whatever response first arrives on a connection Keploy joined
// after the greeting was already exchanged. Crucially payload[0] != 0x0a, so
// the decoder does not mistake it for a HandshakeV10 greeting.
func midStreamServerPacket(t *testing.T) []byte {
	t.Helper()
	// Any valid server capability set works — we only need well-formed OK
	// bytes whose first byte is not the HandshakeV10 protocol version.
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), cannedHandshakeV10(t)[4:])
	if err != nil {
		t.Fatalf("decode handshake for caps: %v", err)
	}
	return cannedOK(t, 1, greeting.CapabilityFlags)
}

// observedLogger returns a logger whose entries can be inspected, so a test can
// assert the graceful skip produced no ERROR logs (the customer-visible symptom
// was three ERRORs per connection) and did surface the WARN that explains it.
func observedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

func assertGracefulSkipLogs(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()
	if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
		for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
			t.Logf("unexpected ERROR log: %s", e.Message)
		}
		t.Fatalf("a graceful mid-stream skip must emit no ERROR logs, got %d", n)
	}
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n == 0 {
		t.Fatalf("a mid-stream skip must surface exactly one WARN so missing mocks are explained, got 0")
	}
}

// TestRecord_MidStreamNoGreeting_SkipsGracefully drives the legacy Record path
// (the one the customer hit — mysql.go recordLegacy) with a connection whose
// first server packet is not the greeting.
func TestRecord_MidStreamNoGreeting_SkipsGracefully(t *testing.T) {
	// Not parallel: Record routes emitted mocks through the process-global
	// syncMock singleton, which wireSyncMockOutput binds.
	logger, logs := observedLogger()
	mocks := make(chan *models.Mock, 8)
	wireSyncMockOutput(t, mocks)

	clientConn := newPipeConn()
	destConn := newPipeConn()
	// First byte we ever see from the server is a query OK, not the greeting.
	destConn.push(midStreamServerPacket(t))

	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithTimeout(gctx, 2*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "midstream-conn")

	err := Record(ctx, logger, clientConn, destConn, mocks, models.OutgoingOptions{
		DstCfg: &models.ConditionalDstCfg{Addr: "127.0.0.1:3306", Port: 3306},
	}, nil)
	if err != nil {
		t.Fatalf("Record must skip a mid-stream (greeting-less) connection gracefully, got error: %v", err)
	}

	select {
	case m := <-mocks:
		t.Fatalf("no mock should be emitted for an un-decodable mid-stream connection, got %+v", m)
	default:
	}
	assertGracefulSkipLogs(t, logs)
}

// TestRecordV2_MidStreamNoGreeting_SkipsGracefully drives the V2 relay path
// with the same mid-stream scenario, so both record entry points are covered.
func TestRecordV2_MidStreamNoGreeting_SkipsGracefully(t *testing.T) {
	t.Parallel()
	h := newV2Harness(t)
	logger, logs := observedLogger()
	h.sess.Logger = logger

	base := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	// First DestStream packet is a query OK, not the HandshakeV10 greeting.
	h.pushDest(midStreamServerPacket(t), base)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- RecordV2(ctx, logger, h.sess)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RecordV2 must skip a mid-stream (greeting-less) connection gracefully, got error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RecordV2 did not return; mid-stream skip should be immediate")
	}

	select {
	case m := <-h.mocks:
		t.Fatalf("no mock should be emitted for an un-decodable mid-stream connection, got %+v", m)
	default:
	}
	assertGracefulSkipLogs(t, logs)
}
