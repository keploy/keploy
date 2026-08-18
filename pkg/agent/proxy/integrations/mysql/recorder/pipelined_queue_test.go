package recorder

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap/zaptest"
)

func queuedExecute(seq byte, id uint32) []byte {
	p := make([]byte, 10)
	p[0] = mysql.COM_STMT_EXECUTE
	binary.LittleEndian.PutUint32(p[1:], id)
	p[5] = 0
	binary.LittleEndian.PutUint32(p[6:], 1)
	return wrapPacket(p, seq)
}

func queuedOK(seq byte, affected byte) []byte {
	return wrapPacket([]byte{mysql.OK, affected, 0x00, 0x02, 0x00, 0x00, 0x00}, seq)
}

func newQueueHarness(t *testing.T, name string) (*pipeConn, *wire.DecodeContext, chan *models.Mock, context.Context, uint32) {
	t.Helper()
	const stmtID = uint32(11)
	clientConn := newPipeConn()
	decodeCtx := buildPostHandshakeDecodeCtx(clientConn)
	decodeCtx.LastOp.Store(clientConn, wire.RESET)
	decodeCtx.PreparedStatements[stmtID] = &mysql.StmtPrepareOkPacket{StatementID: stmtID}
	mocks := make(chan *models.Mock, 512)
	wireSyncMockOutput(t, mocks)
	return clientConn, decodeCtx, mocks, context.WithValue(context.Background(), models.ClientConnectionIDKey, name), stmtID
}

// The command FIFO holds commands the server has not answered yet. On a
// healthy connection it is a handful deep, but a connection whose responses
// stop arriving would otherwise accumulate one entry per command for the life
// of the process — the same unbounded-growth shape the heldResp guard beside
// it is explicitly capped against.
func TestAsyncMySQLDecode_PipelinedQueueIsBounded(t *testing.T) {
	clientConn, decodeCtx, mocks, ctx, stmtID := newQueueHarness(t, "queue-bound")

	// Far more commands than the cap, and not a single response.
	const commands = 5000
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	items := make(chan mysqlDecodeItem, commands+4)
	for i := 0; i < commands; i++ {
		items <- mysqlDecodeItem{fromClient: true, data: queuedExecute(0, stmtID), ts: base.Add(time.Duration(i) * time.Millisecond)}
	}
	// One response at the end: it must still pair with a queued command
	// rather than being dropped, i.e. the queue is bounded, not disabled.
	items <- mysqlDecodeItem{fromClient: false, data: queuedOK(1, 7), ts: base.Add(time.Hour)}
	close(items)

	asyncMySQLDecode(ctx, zaptest.NewLogger(t), items, mocks, decodeCtx, clientConn, models.OutgoingOptions{})
	close(mocks)

	// The trailing response pairs with whatever sits at the head of the FIFO,
	// and that is what makes the bound observable: trimmed, the head is one of
	// the LAST few commands; unbounded, it is still command #0 from 5000
	// commands ago. Asserting only "one response paired" passes either way —
	// the timestamp is what distinguishes them.
	paired := 0
	var pairedAt time.Time
	for m := range mocks {
		if len(m.Spec.MySQLResponses) > 0 {
			paired++
			pairedAt = m.Spec.ReqTimestampMock
		}
	}
	if paired != 1 {
		t.Fatalf("got %d responses paired, want 1 — the queue should still serve the trailing "+
			"response after being trimmed", paired)
	}

	oldestKept := base.Add(time.Duration(commands-maxPipelinedCommandsForTest) * time.Millisecond)
	if pairedAt.Before(oldestKept) {
		t.Errorf("the response paired with a command from %s, but only the last %d commands "+
			"should still be queued (oldest kept: %s). The queue is not being trimmed, so a "+
			"connection whose responses stop arriving accumulates one entry per command for "+
			"the life of the process",
			pairedAt.Format("15:04:05.000"), maxPipelinedCommandsForTest, oldestKept.Format("15:04:05.000"))
	}
}

// Mirrors the cap inside asyncMySQLDecode. Kept local so the test fails loudly
// if the production bound is raised without revisiting this assertion.
const maxPipelinedCommandsForTest = 32

func queuedQuery(seq byte, sql string) []byte {
	p := append([]byte{mysql.COM_QUERY}, []byte(sql)...)
	return wrapPacket(p, seq)
}

// Drains without closing: the mock channel is still registered with the
// process-global sync-mock manager, which tracks closure with its own flag.
func drainMocks(t *testing.T, mocks chan *models.Mock) []*models.Mock {
	t.Helper()
	var out []*models.Mock
	for {
		select {
		case m := <-mocks:
			out = append(out, m)
		default:
			return out
		}
	}
}

// A response that fails to decode used to cost one exchange and no more: the
// next client command reassigned pendingCommand and the machine carried on.
// With the FIFO nothing on the client path clears pendingCommand, so an
// undecodable response can leave state==stateExpectCommand with a command
// still active — a pair activateNextCommand reports as "already active"
// without setting stateExpectResponse, after which the switch matches nothing
// and every later packet on the connection is silently dropped.
func TestAsyncMySQLDecode_UndecodableResponseDoesNotKillTheConnection(t *testing.T) {
	clientConn, decodeCtx, mocks, ctx, stmtID := newQueueHarness(t, "bad-response")

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	items := make(chan mysqlDecodeItem, 16)
	// Exchange 1: its response is a LOCAL INFILE packet the decoder rejects.
	items <- mysqlDecodeItem{fromClient: true, data: queuedExecute(0, stmtID), ts: base}
	items <- mysqlDecodeItem{fromClient: false, data: wrapPacket([]byte{0xfb, 'x'}, 1), ts: base.Add(time.Millisecond)}
	// Exchanges 2 and 3 are perfectly ordinary and must still be recorded.
	items <- mysqlDecodeItem{fromClient: true, data: queuedExecute(0, stmtID), ts: base.Add(2 * time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: false, data: queuedOK(1, 2), ts: base.Add(3 * time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: true, data: queuedExecute(0, stmtID), ts: base.Add(4 * time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: false, data: queuedOK(1, 3), ts: base.Add(5 * time.Millisecond)}
	close(items)

	asyncMySQLDecode(ctx, zaptest.NewLogger(t), items, mocks, decodeCtx, clientConn, models.OutgoingOptions{})

	paired := 0
	for _, m := range drainMocks(t, mocks) {
		if len(m.Spec.MySQLResponses) > 0 {
			paired++
		}
	}
	if paired < 2 {
		t.Errorf("only %d exchanges recorded after one undecodable response; the two clean "+
			"exchanges that followed were lost, so the decoder is wedged for the life of the "+
			"connection", paired)
	}
}

// A result set whose terminator never arrives parks the decoder in a
// column/row state. activateNextCommand is only reachable from the
// stateExpectCommand guard, so without a force-flush nothing recovers: later
// commands queue and later RESPONSES are appended as rows of the stuck set.
func TestAsyncMySQLDecode_UnterminatedResultSetDoesNotSwallowTheConnection(t *testing.T) {
	clientConn, decodeCtx, mocks, ctx, _ := newQueueHarness(t, "stuck-resultset")

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	items := make(chan mysqlDecodeItem, 16)
	// A SELECT answered with "1 column" then a column def — and then nothing.
	items <- mysqlDecodeItem{fromClient: true, data: queuedQuery(0, "SELECT 1"), ts: base}
	items <- mysqlDecodeItem{fromClient: false, data: wrapPacket([]byte{0x01}, 1), ts: base.Add(time.Millisecond)}
	// Two ordinary exchanges follow; both must still be recorded.
	items <- mysqlDecodeItem{fromClient: true, data: queuedQuery(0, "INSERT 1"), ts: base.Add(2 * time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: false, data: queuedOK(1, 2), ts: base.Add(3 * time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: true, data: queuedQuery(0, "INSERT 2"), ts: base.Add(4 * time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: false, data: queuedOK(1, 3), ts: base.Add(5 * time.Millisecond)}
	close(items)

	asyncMySQLDecode(ctx, zaptest.NewLogger(t), items, mocks, decodeCtx, clientConn, models.OutgoingOptions{})

	if got := len(drainMocks(t, mocks)); got < 3 {
		t.Errorf("recorded %d mocks, want at least 3 — the unterminated result set never "+
			"released the decoder, so the two exchanges after it were swallowed", got)
	}
}

// The general pipelining case, not just Connector/J's RESET+SLD+EXECUTE.
// wire.DecodePayload dispatches on LastOp, so with COM_QUERY still set the
// second pipelined command decodes as that query's RESPONSE and is queued as a
// phantom TextResultSet "command" that eats a real response slot.
func TestAsyncMySQLDecode_PipelinedSameOpCommandsAreNotDecodedAsResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  func(byte) []byte
		want string
	}{
		{"COM_QUERY", func(s byte) []byte { return queuedQuery(s, "INSERT INTO t VALUES (1)") }, "COM_QUERY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, decodeCtx, mocks, ctx, _ := newQueueHarness(t, "pipelined-"+tc.name)

			base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
			items := make(chan mysqlDecodeItem, 16)
			items <- mysqlDecodeItem{fromClient: true, data: tc.cmd(0), ts: base}
			items <- mysqlDecodeItem{fromClient: true, data: tc.cmd(0), ts: base.Add(time.Millisecond)}
			items <- mysqlDecodeItem{fromClient: false, data: queuedOK(1, 1), ts: base.Add(2 * time.Millisecond)}
			items <- mysqlDecodeItem{fromClient: false, data: queuedOK(1, 2), ts: base.Add(3 * time.Millisecond)}
			close(items)

			asyncMySQLDecode(ctx, zaptest.NewLogger(t), items, mocks, decodeCtx, clientConn, models.OutgoingOptions{})

			for _, m := range drainMocks(t, mocks) {
				op := ""
				if len(m.Spec.MySQLRequests) > 0 {
					op = m.Spec.MySQLRequests[0].Header.Type
				}
				if op != tc.want {
					t.Errorf("recorded a mock whose REQUEST is %q; a client packet was decoded "+
						"as a response because LastOp still named the previous command", op)
				}
			}
		})
	}
}

func rResetCmd(seq byte, id uint32) []byte {
	p := make([]byte, 5)
	p[0] = mysql.COM_STMT_RESET
	binary.LittleEndian.PutUint32(p[1:], id)
	return wrapPacket(p, seq)
}

func queuedPrepare(seq byte, sql string) []byte {
	return wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, []byte(sql)...), seq)
}

func queuedPrepareOK(seq byte, stmtID uint32) []byte {
	p := make([]byte, 12)
	p[0] = mysql.OK
	binary.LittleEndian.PutUint32(p[1:], stmtID)
	binary.LittleEndian.PutUint16(p[5:], 0) // num columns
	binary.LittleEndian.PutUint16(p[7:], 0) // num params
	p[9] = 0                                // filler
	binary.LittleEndian.PutUint16(p[10:], 0)
	return wrapPacket(p, seq)
}

// activateNextCommand restores decodeCtx.LastOp for the command it activates.
// Without that, the response decoder uses whatever op the LAST pipelined
// command left behind, and a COM_STMT_PREPARE's response degrades from a
// StmtPrepareOkPacket to a plain OK — losing the statement id, so every
// COM_STMT_EXECUTE against it fails at replay.
func TestAsyncMySQLDecode_ActivateRestoresTheOpForItsOwnResponse(t *testing.T) {
	clientConn, decodeCtx, mocks, ctx, stmtID := newQueueHarness(t, "restore-lastop")

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	items := make(chan mysqlDecodeItem, 8)
	// Pipelined: PREPARE then RESET, so LastOp ends up on RESET before any
	// response is read. PREPARE's response must still decode as a prepare OK.
	items <- mysqlDecodeItem{fromClient: true, data: queuedPrepare(0, "SELECT 1"), ts: base}
	items <- mysqlDecodeItem{fromClient: true, data: rResetCmd(0, stmtID), ts: base.Add(time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: false, data: queuedPrepareOK(1, stmtID), ts: base.Add(2 * time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: false, data: queuedOK(1, 0), ts: base.Add(3 * time.Millisecond)}
	close(items)

	asyncMySQLDecode(ctx, zaptest.NewLogger(t), items, mocks, decodeCtx, clientConn, models.OutgoingOptions{})

	for _, m := range drainMocks(t, mocks) {
		if len(m.Spec.MySQLRequests) == 0 || m.Spec.MySQLRequests[0].Header.Type != "COM_STMT_PREPARE" {
			continue
		}
		if len(m.Spec.MySQLResponses) == 0 {
			t.Fatal("COM_STMT_PREPARE recorded with no response")
		}
		if _, ok := m.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket); !ok {
			t.Errorf("COM_STMT_PREPARE's response decoded as %T, want *mysql.StmtPrepareOkPacket — "+
				"the activated command's operation was not restored, so the response was decoded "+
				"as the next pipelined command's", m.Spec.MySQLResponses[0].Message)
		}
		return
	}
	t.Fatal("no COM_STMT_PREPARE mock was recorded")
}

// The inverse race to pipelining: on loopback a response can reach the decoder
// before its own command. It is parked in heldResp and must be replayed once a
// command arrives, or the exchange is recorded with no response at all — which
// is what silently lost mocks for fast connection-pool validation queries.
func TestAsyncMySQLDecode_ResponseArrivingBeforeItsCommandIsReplayed(t *testing.T) {
	clientConn, decodeCtx, mocks, ctx, stmtID := newQueueHarness(t, "held-response")

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	items := make(chan mysqlDecodeItem, 8)
	// Response first, command second.
	items <- mysqlDecodeItem{fromClient: false, data: queuedOK(1, 4), ts: base.Add(time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: true, data: queuedExecute(0, stmtID), ts: base}
	close(items)

	asyncMySQLDecode(ctx, zaptest.NewLogger(t), items, mocks, decodeCtx, clientConn, models.OutgoingOptions{})

	for _, m := range drainMocks(t, mocks) {
		if len(m.Spec.MySQLRequests) > 0 && m.Spec.MySQLRequests[0].Header.Type == "COM_STMT_EXECUTE" {
			if len(m.Spec.MySQLResponses) == 0 {
				t.Fatal("the out-of-order response was never replayed, so the command was " +
					"recorded with no response and replay has nothing to answer with")
			}
			return
		}
	}
	t.Fatal("no COM_STMT_EXECUTE mock was recorded")
}

// The full streamed-BLOB sequence, end to end through the recorder — the one
// issue #4262 actually reports. Connector/J sends the BLOB out of band, so the
// EXECUTE payload carries no value for that parameter while the null bitmap
// still marks it NOT NULL. Unless the decoder is told the value arrived via
// COM_STMT_SEND_LONG_DATA, it reads past the end, rejects the command, and the
// EXECUTE never becomes a mock at all — replay then has nothing to answer the
// live EXECUTE with and Connector/J reports
// "Can not read response from server".
func TestAsyncMySQLDecode_StreamedBlobExecuteIsRecorded(t *testing.T) {
	const stmtID = uint32(21)

	clientConn := newPipeConn()
	decodeCtx := buildPostHandshakeDecodeCtx(clientConn)
	decodeCtx.LastOp.Store(clientConn, wire.RESET)
	// One BLOB parameter, supplied by SEND_LONG_DATA rather than by EXECUTE.
	decodeCtx.PreparedStatements[stmtID] = &mysql.StmtPrepareOkPacket{
		StatementID: stmtID,
		NumParams:   1,
	}

	mocks := make(chan *models.Mock, 32)
	wireSyncMockOutput(t, mocks)
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "streamed-blob")

	// EXECUTE with the parameter bound but its value absent.
	execute := wrapPacket([]byte{
		mysql.COM_STMT_EXECUTE,
		byte(stmtID), 0x00, 0x00, 0x00,
		0x00,                   // flags
		0x01, 0x00, 0x00, 0x00, // iteration count
		0x00,       // NULL bitmap: parameter 0 is NOT null
		0x01,       // new params bind flag
		0xfc, 0x00, // MYSQL_TYPE_BLOB
	}, 0)

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	items := make(chan mysqlDecodeItem, 16)
	items <- mysqlDecodeItem{fromClient: true, data: rResetCmd(0, stmtID), ts: base}
	items <- mysqlDecodeItem{fromClient: true, data: stmtSendLongDataCommand(0, stmtID, 0, []byte("blob-bytes")), ts: base.Add(time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: true, data: execute, ts: base.Add(2 * time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: false, data: queuedOK(1, 0), ts: base.Add(3 * time.Millisecond)}
	items <- mysqlDecodeItem{fromClient: false, data: queuedOK(1, 1), ts: base.Add(4 * time.Millisecond)}
	close(items)

	asyncMySQLDecode(ctx, zaptest.NewLogger(t), items, mocks, decodeCtx, clientConn, models.OutgoingOptions{})

	seen := map[string]bool{}
	for _, m := range drainMocks(t, mocks) {
		if len(m.Spec.MySQLRequests) > 0 {
			seen[m.Spec.MySQLRequests[0].Header.Type] = true
		}
	}
	for _, want := range []string{"COM_STMT_RESET", "COM_STMT_SEND_LONG_DATA", "COM_STMT_EXECUTE"} {
		if !seen[want] {
			t.Errorf("no %s mock recorded; got %v", want, seen)
		}
	}
}
