package recorder

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
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
	return cannedHandshakeV10WithCaps(t, 0)
}

func cannedHandshakeV10WithCaps(t *testing.T, additionalCaps uint32) []byte {
	t.Helper()
	caps := uint32(mysql.CLIENT_PROTOCOL_41 |
		mysql.CLIENT_PLUGIN_AUTH |
		mysql.CLIENT_SSL |
		mysql.CLIENT_SECURE_CONNECTION)
	caps |= additionalCaps
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
	return cannedHandshakeResponse41WithCaps(t, seq, sslBit, 0)
}

func cannedHandshakeResponse41WithCaps(t *testing.T, seq byte, sslBit bool, additionalCaps uint32) []byte {
	t.Helper()
	caps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION)
	if sslBit {
		caps |= mysql.CLIENT_SSL
	}
	caps |= additionalCaps
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

func cannedStmtPrepare(seq byte, query string) []byte {
	payload := append([]byte{mysql.COM_STMT_PREPARE}, []byte(query)...)
	return wrapPacket(payload, seq)
}

func cannedStmtPrepareOK(seq byte, statementID uint32, numColumns uint16) []byte {
	payload := make([]byte, 12)
	payload[0] = mysql.OK
	binary.LittleEndian.PutUint32(payload[1:5], statementID)
	binary.LittleEndian.PutUint16(payload[5:7], numColumns)
	return wrapPacket(payload, seq)
}

func cannedLongColumn(t *testing.T, seq byte, name string) []byte {
	t.Helper()
	column := &mysql.ColumnDefinition41{
		Header:       mysql.Header{SequenceID: seq},
		Catalog:      "def",
		Schema:       "test",
		Table:        "cursor_rows",
		OrgTable:     "cursor_rows",
		Name:         name,
		OrgName:      name,
		FixedLength:  0x0c,
		CharacterSet: 0x21,
		ColumnLength: 11,
		Type:         byte(mysql.FieldTypeLong),
		Filler:       []byte{0x00, 0x00},
	}
	buf, err := rowscols.EncodeColumn(context.Background(), zap.NewNop(), column)
	if err != nil {
		t.Fatalf("encode column: %v", err)
	}
	return buf
}

func cannedEOF(seq byte, statusFlags uint16) []byte {
	payload := []byte{mysql.EOF, 0x00, 0x00, 0x00, 0x00}
	binary.LittleEndian.PutUint16(payload[3:5], statusFlags)
	return wrapPacket(payload, seq)
}

func cannedOKReplacingEOF(seq byte, statusFlags uint16) []byte {
	payload := []byte{mysql.EOF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	binary.LittleEndian.PutUint16(payload[3:5], statusFlags)
	return wrapPacket(payload, seq)
}

func cannedStmtExecute(seq byte, statementID uint32, flags byte) []byte {
	payload := make([]byte, 10)
	payload[0] = mysql.COM_STMT_EXECUTE
	binary.LittleEndian.PutUint32(payload[1:5], statementID)
	payload[5] = flags
	binary.LittleEndian.PutUint32(payload[6:10], 1)
	return wrapPacket(payload, seq)
}

func cannedStmtFetch(seq byte, statementID, numRows uint32) []byte {
	payload := make([]byte, 9)
	payload[0] = mysql.COM_STMT_FETCH
	binary.LittleEndian.PutUint32(payload[1:5], statementID)
	binary.LittleEndian.PutUint32(payload[5:9], numRows)
	return wrapPacket(payload, seq)
}

func cannedStmtClose(seq byte, statementID uint32) []byte {
	payload := make([]byte, 5)
	payload[0] = mysql.COM_STMT_CLOSE
	binary.LittleEndian.PutUint32(payload[1:5], statementID)
	return wrapPacket(payload, seq)
}

func cannedBinaryLongRow(t *testing.T, seq byte, value int32) []byte {
	t.Helper()
	row := &mysql.BinaryRow{
		Header:        mysql.Header{SequenceID: seq},
		RowNullBuffer: []byte{0x00},
		Values: []mysql.ColumnEntry{{
			Type:  mysql.FieldTypeLong,
			Name:  "id",
			Value: value,
		}},
	}
	column := &mysql.ColumnDefinition41{Name: "id", Type: byte(mysql.FieldTypeLong)}
	buf, err := rowscols.EncodeBinaryRow(context.Background(), zap.NewNop(), row, []*mysql.ColumnDefinition41{column})
	if err != nil {
		t.Fatalf("encode binary row: %v", err)
	}
	return buf
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

func TestRecordV2_StmtFetchRecordsEveryCursorChunk(t *testing.T) {
	t.Parallel()
	h := newV2Harness(t)

	const (
		statementID       = uint32(4)
		serverAutocommit  = uint16(0x0002)
		serverCursorOpen  = uint16(0x0040)
		serverLastRowSent = uint16(0x0080)
	)
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	handshakeBuf := cannedHandshakeV10WithCaps(t, wire.CLIENT_DEPRECATE_EOF)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}

	// Handshake.
	h.pushDest(handshakeBuf, base)
	h.pushClient(cannedHandshakeResponse41WithCaps(t, 1, false, wire.CLIENT_DEPRECATE_EOF), base.Add(time.Millisecond))
	h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), base.Add(2*time.Millisecond))

	// PREPARE supplies the result-column metadata later needed to decode the
	// row-only FETCH responses.
	h.pushClient(cannedStmtPrepare(0, "SELECT id FROM cursor_rows ORDER BY id"), base.Add(10*time.Millisecond))
	h.pushDest(cannedStmtPrepareOK(1, statementID, 1), base.Add(11*time.Millisecond))
	h.pushDest(cannedLongColumn(t, 2, "id"), base.Add(12*time.Millisecond))

	// EXECUTE opens a read-only cursor. It returns metadata and a cursor-open
	// terminator, but no rows; rows arrive only after COM_STMT_FETCH.
	h.pushClient(cannedStmtExecute(0, statementID, mysql.CURSOR_TYPE_READ_ONLY), base.Add(20*time.Millisecond))
	h.pushDest(wrapPacket([]byte{0x01}, 1), base.Add(21*time.Millisecond))
	h.pushDest(cannedLongColumn(t, 2, "id"), base.Add(22*time.Millisecond))
	h.pushDest(cannedOKReplacingEOF(3, serverAutocommit|serverCursorOpen), base.Add(23*time.Millisecond))

	// Two fetches prove repeated identical commands remain separate FIFO mocks.
	h.pushClient(cannedStmtFetch(0, statementID, 2), base.Add(30*time.Millisecond))
	h.pushDest(cannedBinaryLongRow(t, 1, 11), base.Add(31*time.Millisecond))
	h.pushDest(cannedBinaryLongRow(t, 2, 12), base.Add(32*time.Millisecond))
	h.pushDest(cannedOKReplacingEOF(3, serverAutocommit|serverCursorOpen), base.Add(33*time.Millisecond))

	h.pushClient(cannedStmtFetch(0, statementID, 2), base.Add(40*time.Millisecond))
	h.pushDest(cannedBinaryLongRow(t, 1, 13), base.Add(41*time.Millisecond))
	h.pushDest(cannedOKReplacingEOF(2, serverAutocommit|serverLastRowSent), base.Add(42*time.Millisecond))

	// CLOSE is a no-response command and must still be recorded separately.
	h.pushClient(cannedStmtClose(0, statementID), base.Add(50*time.Millisecond))

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- RecordV2(ctx, h.logger, h.sess)
	}()

	got := make([]*models.Mock, 0, 6)
	for len(got) < 6 {
		select {
		case mock := <-h.mocks:
			got = append(got, mock)
			if len(got) == 6 {
				h.closeStreams()
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for cursor mocks (got %d)", len(got))
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("RecordV2 returned error: %v", err)
	}

	wantOps := []string{
		mysql.AuthStatusToString(mysql.HandshakeV10),
		mysql.CommandStatusToString(mysql.COM_STMT_PREPARE),
		mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE),
		mysql.CommandStatusToString(mysql.COM_STMT_FETCH),
		mysql.CommandStatusToString(mysql.COM_STMT_FETCH),
		mysql.CommandStatusToString(mysql.COM_STMT_CLOSE),
	}
	for i, want := range wantOps {
		if gotOp := got[i].Spec.Metadata["requestOperation"]; gotOp != want {
			t.Errorf("mock %d requestOperation = %q, want %q", i, gotOp, want)
		}
	}

	assertFetch := func(mock *models.Mock, wantValues []int32, wantStatus uint16) {
		t.Helper()
		if len(mock.Spec.MySQLRequests) != 1 {
			t.Fatalf("fetch requests = %d, want 1", len(mock.Spec.MySQLRequests))
		}
		request, ok := mock.Spec.MySQLRequests[0].Message.(*mysql.StmtFetchPacket)
		if !ok {
			t.Fatalf("fetch request message = %T", mock.Spec.MySQLRequests[0].Message)
		}
		if request.StatementID != statementID || request.NumRows != 2 {
			t.Errorf("fetch request = %+v, want stmt=%d rows=2", request, statementID)
		}

		if len(mock.Spec.MySQLResponses) != 1 {
			t.Fatalf("fetch responses = %d, want 1", len(mock.Spec.MySQLResponses))
		}
		response, ok := mock.Spec.MySQLResponses[0].Message.(*mysql.StmtFetchResponse)
		if !ok {
			t.Fatalf("fetch response message = %T", mock.Spec.MySQLResponses[0].Message)
		}
		if len(response.Rows) != len(wantValues) {
			t.Fatalf("fetch rows = %d, want %d", len(response.Rows), len(wantValues))
		}
		for i, want := range wantValues {
			if gotValue, ok := response.Rows[i].Values[0].Value.(int32); !ok || gotValue != want {
				t.Errorf("fetch row %d value = %v (%T), want int32(%d)", i, response.Rows[i].Values[0].Value, response.Rows[i].Values[0].Value, want)
			}
		}
		if response.FinalResponse == nil || len(response.FinalResponse.Data) < 9 {
			t.Fatalf("fetch final response = %+v", response.FinalResponse)
		}
		if response.FinalResponse.Type != mysql.StatusToString(mysql.OK) {
			t.Errorf("fetch final response type = %q, want OK", response.FinalResponse.Type)
		}
		if gotStatus := binary.LittleEndian.Uint16(response.FinalResponse.Data[7:9]); gotStatus != wantStatus {
			t.Errorf("fetch final status = %#x, want %#x", gotStatus, wantStatus)
		}
	}

	assertFetch(got[3], []int32{11, 12}, serverAutocommit|serverCursorOpen)
	assertFetch(got[4], []int32{13}, serverAutocommit|serverLastRowSent)
	if len(got[5].Spec.MySQLResponses) != 0 {
		t.Errorf("COM_STMT_CLOSE responses = %d, want 0", len(got[5].Spec.MySQLResponses))
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
	// The seq==0 config mock MUST carry a synthesized HandshakeResponse41 at
	// requests[0] or [1] — the replayer matches a connection on it and errors
	// otherwise (replayer/conn.go). Without the synthesis this config mock would
	// only have [SSLRequest] and fail replay handshake matching.
	if !configMockHasHR41(got[0]) {
		t.Errorf("seq==0 config mock has no HandshakeResponse41 in requests[0]/[1] — would fail replay handshake matching; reqs=%d", len(got[0].Spec.MySQLRequests))
	}
	// The query mock proves the seq==0 firstCmd was decoded as a COMMAND (not
	// mis-decoded as a handshake response because LastOp stayed HandshakeV10).
	assertQueryMock(t, got[1])
}

// configMockHasHR41 reports whether the config mock carries a
// HandshakeResponse41 at requests[0] or [1] (the replayer's match requirement).
func configMockHasHR41(m *models.Mock) bool {
	r := m.Spec.MySQLRequests
	if len(r) > 0 && r[0].Header != nil && r[0].Header.Type == mysql.HandshakeResponse41 {
		return true
	}
	if len(r) > 1 && r[1].Header != nil && r[1].Header.Type == mysql.HandshakeResponse41 {
		return true
	}
	return false
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

// TestEmitMockV2ReportsAWindowInvertedByMonotonicClamping pins the diagnostic.
//
// enforceReqMonotonic raises ReqTimestampMock to lastReq+1ns to keep the
// matcher's ordering invariant, and does not touch ResTimestampMock. On a mock
// that arrived well-ordered that can push req PAST res, and filterByTimeStamp
// then drops any mock with res < req (pkg/util.go) -- so the mock is orphaned by
// this step, not by the recorder.
//
// The timestamps are deliberately NOT repaired here; see the comment at the call
// site for the two attempts that regressed go-memory-load-mongo, the second of
// which produced recordings the RELEASED replayer could not consume. What this
// pins is that the loss is announced rather than silent: the first instance of it
// was found only by diffing recorded YAML by hand.
//
// The inverted values are the real ones from k8s-proxy pipeline 6303, mock-61.
func TestEmitMockV2ReportsAWindowInvertedByMonotonicClamping(t *testing.T) {
	base := time.Date(2026, 9, 3, 0, 18, 17, 0, time.UTC)
	req := func(op string) []mysql.Request {
		return []mysql.Request{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: op}}}}
	}
	resp := func(op string) []mysql.Response {
		return []mysql.Response{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: op}}}}
	}

	newHarnessWithLogs := func(t *testing.T) (*v2Harness, *observer.ObservedLogs) {
		t.Helper()
		h := newV2Harness(t)
		core, logs := observer.New(zapcore.DebugLevel)
		h.sess.Logger = zap.New(core)
		return h, logs
	}
	emit := func(h *v2Harness, rq, rs time.Time) *models.Mock {
		t.Helper()
		emitMockV2(context.Background(), h.sess, req("COM_QUERY"), resp("OK"), "mocks", "COM_QUERY", "OK", rq, rs)
		select {
		case m := <-h.mocks:
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("emitMockV2 produced no mock within 5s")
			return nil
		}
	}

	t.Run("an inversion this step creates is reported", func(t *testing.T) {
		t.Parallel()
		h, logs := newHarnessWithLogs(t)
		// A later request FIRST, so enforceReqMonotonic has a baseline to raise
		// to. Without it the monotonic pass returns early on the session's first
		// mock, the branch is never reached, and this subtest would pass no
		// matter what the branch does.
		_ = emit(h, base.Add(900*time.Millisecond), base.Add(900*time.Millisecond))
		// Well-ordered on arrival (req < res); only the monotonic raise inverts it.
		m := emit(h, base.Add(700*time.Millisecond), base.Add(750*time.Millisecond))

		if !m.Spec.ResTimestampMock.Before(m.Spec.ReqTimestampMock) {
			t.Fatalf("expected the monotonic raise to invert this window (req=%v res=%v); "+
				"the fixture no longer exercises the branch",
				m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock)
		}
		warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
		if len(warns) != 1 {
			t.Fatalf("got %d WARN entries, want 1 — a mock that replay will silently DROP "+
				"was orphaned with no diagnostic at all", len(warns))
		}
		if got := warns[0].ContextMap()["inversion"]; got == nil {
			t.Error("the warning does not carry the inversion magnitude")
		}
	})

	t.Run("a window that stays well-ordered is not reported", func(t *testing.T) {
		t.Parallel()
		h, logs := newHarnessWithLogs(t)
		_ = emit(h, base.Add(900*time.Millisecond), base.Add(900*time.Millisecond))
		// Raised to lastReq+1ns, which is still before this response stamp.
		m := emit(h, base.Add(700*time.Millisecond), base.Add(950*time.Millisecond))
		if m.Spec.ResTimestampMock.Before(m.Spec.ReqTimestampMock) {
			t.Fatalf("fixture inverted unexpectedly: req=%v res=%v", m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock)
		}
		if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
			t.Errorf("got %d WARN entries for a well-ordered window, want 0", n)
		}
	})

	t.Run("timestamps are not rewritten", func(t *testing.T) {
		t.Parallel()
		h, _ := newHarnessWithLogs(t)
		rq, rs := base.Add(100*time.Millisecond), base.Add(150*time.Millisecond)
		m := emit(h, rq, rs)
		if !m.Spec.ReqTimestampMock.Equal(rq) || !m.Spec.ResTimestampMock.Equal(rs) {
			t.Errorf("a well-ordered window was altered: got req=%v res=%v, want req=%v res=%v",
				m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock, rq, rs)
		}
	})
}

// tlsClientHelloBytes is a stand-in for the record the client's TLS
// stack writes immediately after its SSLRequest. Only the leading
// `16 03 01` matters: read as a MySQL header it declares a payload
// length of 66326, which is what makes the failure a hang rather than a
// decode error.
func tlsClientHelloBytes() []byte {
	p := make([]byte, 120)
	p[0], p[1], p[2] = 0x16, 0x03, 0x01
	p[3], p[4] = 0x00, 0x73
	return p
}

// TestRecordV2_TLSUpgrade_ClientHelloDiscardedFromParserStream is the
// MySQL end of the client write hold's contract, and the reason the
// hold is not finished when the ClientHello stops reaching the wire.
//
// The relay's forwarder is parked in Read on the client socket, so it
// holds the SSLRequest and the ClientHello — here in ONE chunk, which is
// what a coalesced client write produces — before this parser has been
// scheduled to look at either. Both are teed. The parser reads exactly
// the 36-byte SSLRequest and asks for the upgrade; the relay then
// consumes the ClientHello for its own client-side handshake and, at the
// pause barrier, tells the parser's stream to discard everything below
// the position it consumed to.
//
// Without that last step the parser's next read returns `16 03 01 00`
// where the post-TLS HandshakeResponse41 should be, ReadRequiredBytes
// blocks on a 66326-byte payload, the hang watchdog retires the parser
// and the connection falls through to passthrough with zero mocks. The
// wire leak would be fixed and every MySQL TLS recording lost.
func TestRecordV2_TLSUpgrade_ClientHelloDiscardedFromParserStream(t *testing.T) {
	t.Parallel()
	h := newV2Harness(t)
	h.sess.ClientWritesHeld = true

	base := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)

	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake: %v", err)
	}
	h.pushDest(handshakeBuf, base)

	sslReq := cannedSSLRequest(t, 1)
	clientHello := tlsClientHelloBytes()
	// One chunk, both messages: the forwarder read them together and
	// teed what it read.
	h.pushClient(append(append([]byte(nil), sslReq...), clientHello...), base.Add(1*time.Millisecond))

	// Post-TLS traffic, exactly as the relay would deliver it once the
	// handshake is up.
	h.pushClient(cannedHandshakeResponse41(t, 2, true), base.Add(20*time.Millisecond))
	h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), base.Add(25*time.Millisecond))

	dirSeen := make(chan directive.Directive, 1)
	go func() {
		select {
		case d := <-h.dirs:
			dirSeen <- d
			// The relay's half of the contract, at the point it performs
			// it: the barrier is up, the parser is blocked on this ack,
			// and everything teed so far that the parser did not read is
			// handshake material.
			h.sess.ClientStream.DiscardBefore(int64(len(sslReq) + len(clientHello)))
			h.acks <- directive.Ack{
				Kind:              d.Kind,
				OK:                true,
				BoundaryReadAt:    base.Add(10 * time.Millisecond),
				BoundaryWrittenAt: base.Add(15 * time.Millisecond),
			}
		case <-time.After(2 * time.Second):
			t.Errorf("parser never sent a directive")
		}
	}()

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- RecordV2(ctx, h.logger, h.sess)
	}()

	select {
	case m := <-h.mocks:
		if m.Name != "config" {
			t.Errorf("mock name = %q, want config", m.Name)
		}
		if len(m.Spec.MySQLRequests) < 2 {
			t.Errorf("TLS config mock has %d requests, want >=2 (SSLRequest + post-TLS "+
				"HandshakeResponse41). A short count means the parser never got past the "+
				"ClientHello left in its stream.", len(m.Spec.MySQLRequests))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the TLS config mock: the parser is blocked reading a " +
			"packet whose header it took from the TLS ClientHello")
	}

	select {
	case d := <-dirSeen:
		if d.TLS == nil {
			t.Fatalf("directive carried no TLS params: %+v", d)
		}
		if d.TLS.ClientFlushBytes != len(sslReq) {
			t.Errorf("ClientFlushBytes = %d, want %d — the relay forwards exactly the "+
				"SSLRequest upstream and keeps the ClientHello, so the count has to be the "+
				"measured width of the packet this parser consumed",
				d.TLS.ClientFlushBytes, len(sslReq))
		}
	default:
		t.Fatal("directive never observed")
	}

	h.closeStreams()
	if err := <-done; err != nil {
		t.Errorf("RecordV2 returned error: %v", err)
	}
}
