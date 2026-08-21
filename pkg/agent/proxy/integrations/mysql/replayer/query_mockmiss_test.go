package replayer

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// TestHandleCommandMockMiss_ServesErrPacketAndKeepsConnection is the regression
// guard for the outage this fix targets.
//
// Previously a command-phase mock-miss returned models.ErrNoMockMatched, which
// unwound to `errCh <- err` in replay.go and dropped the client's TCP
// connection — turning one drifted/unrecorded query into a multi-second app
// outage (and, with pooled drivers, a connection-pool blacklist cascade).
//
// The fix mirrors the HTTP replayer's serve-without-mock intent: when a
// mismatch reporter is installed (the normal test path), the miss is reported
// out-of-band, a single MySQL ERR packet is written for that one query, and the
// command loop keeps serving the connection. This test asserts exactly that
// outcome at the replayer level — the miss does NOT propagate the fatal
// teardown error, a well-formed ERR packet reaches the client, and the miss is
// still surfaced to the test report.
func TestHandleCommandMockMiss_ServesErrPacketAndKeepsConnection(t *testing.T) {
	const caps = mysql.CLIENT_PROTOCOL_41

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	decodeCtx := newTestDecodeCtx(serverConn, caps)

	// A reporter records the miss out-of-band, exactly like the proxy's
	// sendMockNotFoundError sink in MODE_TEST.
	var reported error
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) {
		reported = err
	})

	req := comQueryRequest("SELECT * FROM widgets WHERE id = 7", 0)
	report := &models.MockMismatchReport{Protocol: "MySQL", ActualSummary: "COM_QUERY"}

	// handleCommandMockMiss writes to serverConn; net.Pipe is unbuffered, so the
	// write blocks until the client reads. Run the handler in a goroutine and
	// read the ERR packet from the other end.
	outCh := make(chan mockMissOutcome, 1)
	go func() {
		outCh <- handleCommandMockMiss(ctx, zap.NewNop(), serverConn, req, report, decodeCtx, 1)
	}()

	errPkt := readErrPacket(t, clientConn, caps)

	var outcome mockMissOutcome
	select {
	case outcome = <-outCh:
	case <-time.After(2 * time.Second):
		t.Fatal("handleCommandMockMiss did not return")
	}

	// 1. The connection is NOT torn down: no fatal error propagates.
	if outcome.err != nil {
		t.Fatalf("expected no fatal error (connection kept open), got: %v", outcome.err)
	}

	// 2. A well-formed ERR packet was served for the one query.
	if errPkt.Header != mysql.ERR {
		t.Fatalf("expected ERR header 0x%x, got 0x%x", mysql.ERR, errPkt.Header)
	}
	if errPkt.ErrorCode != mockMissErrorCode {
		t.Fatalf("expected error code %d (ER_UNKNOWN_ERROR), got %d", mockMissErrorCode, errPkt.ErrorCode)
	}
	if errPkt.SQLState != mockMissSQLState {
		t.Fatalf("expected SQLSTATE %q, got %q", mockMissSQLState, errPkt.SQLState)
	}
	if !strings.Contains(errPkt.ErrorMessage, "keploy") {
		t.Fatalf("expected error message to mention keploy, got %q", errPkt.ErrorMessage)
	}

	// 3. The miss is still surfaced to the test report (test still fails).
	if reported == nil {
		t.Fatal("expected the miss to be reported out-of-band, reporter was not called")
	}
	if !errors.Is(reported, models.ErrNoMockMatched) {
		t.Fatalf("reported error should wrap ErrNoMockMatched, got: %v", reported)
	}
}

// TestHandleCommandMockMiss_NoReporterPreservesFatalTeardown pins the
// conservative fallback: when no mismatch reporter is installed (older agent,
// or a non-test path) the miss cannot be surfaced out-of-band, so the handler
// preserves the historical behaviour and returns the wrapped ErrNoMockMatched
// rather than silently swallowing the miss.
func TestHandleCommandMockMiss_NoReporterPreservesFatalTeardown(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	decodeCtx := newTestDecodeCtx(serverConn, mysql.CLIENT_PROTOCOL_41)
	req := comQueryRequest("SELECT 1", 0)
	report := &models.MockMismatchReport{Protocol: "MySQL"}

	// No reporter installed on the context.
	outcome := handleCommandMockMiss(context.Background(), zap.NewNop(), serverConn, req, report, decodeCtx, 1)

	if outcome.err == nil {
		t.Fatal("expected the fatal ErrNoMockMatched to be returned when no reporter is installed")
	}
	if !errors.Is(outcome.err, models.ErrNoMockMatched) {
		t.Fatalf("expected wrapped ErrNoMockMatched, got: %v", outcome.err)
	}
}

// TestHandleCommandMockMiss_NoResponseCommandKeepsConnectionWithoutWrite guards
// the protocol edge: no-response commands (COM_STMT_CLOSE / COM_QUIT /
// COM_STMT_SEND_LONG_DATA) leave the client not reading a reply, so writing an
// ERR packet would desync the stream. The miss must still be reported and the
// connection kept open, but no packet is written.
func TestHandleCommandMockMiss_NoResponseCommandKeepsConnectionWithoutWrite(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	decodeCtx := newTestDecodeCtx(serverConn, mysql.CLIENT_PROTOCOL_41)

	var reported error
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { reported = err })

	req := mysql.Request{PacketBundle: mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Type:   mysql.CommandStatusToString(mysql.COM_QUIT),
			Header: &mysql.Header{SequenceID: 0},
		},
		Message: &mysql.QueryPacket{},
	}}
	report := &models.MockMismatchReport{Protocol: "MySQL"}

	// Fail the test if anything is written to the client (would block forever
	// on net.Pipe if a write were attempted, so read in the background).
	wrote := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 1)
		_ = clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		if n, _ := clientConn.Read(buf); n > 0 {
			wrote <- struct{}{}
		}
	}()

	outcome := handleCommandMockMiss(ctx, zap.NewNop(), serverConn, req, report, decodeCtx, 1)
	if outcome.err != nil {
		t.Fatalf("expected connection kept open for no-response command, got: %v", outcome.err)
	}
	if reported == nil {
		t.Fatal("expected the miss to still be reported for a no-response command")
	}
	select {
	case <-wrote:
		t.Fatal("no ERR packet should be written for a no-response command")
	case <-time.After(600 * time.Millisecond):
		// good: nothing written
	}
}

// --- test helpers ---

func newTestDecodeCtx(conn net.Conn, caps uint32) *wire.DecodeContext {
	decodeCtx := &wire.DecodeContext{
		Mode:            models.MODE_TEST,
		LastOp:          wire.NewLastOpMap(),
		ServerGreetings: wire.NewGreetings(),
	}
	decodeCtx.ServerGreetings.Store(conn, &mysql.HandshakeV10Packet{CapabilityFlags: caps})
	return decodeCtx
}

func comQueryRequest(sql string, seqID uint8) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Type:   mysql.CommandStatusToString(mysql.COM_QUERY),
			Header: &mysql.Header{SequenceID: seqID},
		},
		Message: &mysql.QueryPacket{Query: sql},
	}}
}

// readErrPacket reads one MySQL packet off conn and decodes it as an ERR packet.
func readErrPacket(t *testing.T, conn net.Conn, caps uint32) *mysql.ERRPacket {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("failed to read packet header: %v", err)
	}
	payloadLen := uint32(header[0]) | uint32(header[1])<<8 | uint32(header[2])<<16
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("failed to read packet payload: %v", err)
	}

	pkt, err := phase.DecodeERR(context.Background(), payload, caps)
	if err != nil {
		t.Fatalf("failed to decode ERR packet: %v", err)
	}
	return pkt
}
