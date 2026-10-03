package recorder

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// A response the recorder cannot decode must not be recorded, whole or in
// part. Before, a column definition with a cut-short length prefix, or with an
// 8-byte length past the packet, ended RecordV2 as a clean close (the decoder's
// bare io.EOF passed for the connection closing), so the supervisor saw a
// healthy parser and nothing on the rest of the connection was recorded or
// reported. A row that did not decode was skipped at Debug and the mock was
// emitted with the rows that did: a mock that replays a different answer than
// the server gave.

// undecodableColumns are the two columns every result set below declares.
var undecodableColumns = []*mysql.ColumnDefinition41{
	{Catalog: "def", Schema: "app", Table: "users", OrgTable: "users", Name: "id", OrgName: "id",
		FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)},
	{Catalog: "def", Schema: "app", Table: "users", OrgTable: "users", Name: "email", OrgName: "email",
		FixedLength: 0x0c, CharacterSet: 0x21, ColumnLength: 1020, Type: byte(mysql.FieldTypeVarString)},
}

const undecodableStmtID = uint32(5)

// columnPayload is a column definition's payload, without its header.
func columnPayload(t *testing.T, col *mysql.ColumnDefinition41) []byte {
	t.Helper()
	b, err := rowscols.EncodeColumn(context.Background(), zap.NewNop(), col)
	if err != nil {
		t.Fatal(err)
	}
	return b[4:]
}

// lenencStr is a length-encoded string of up to 250 bytes.
func lenencStr(s string) []byte { return append([]byte{byte(len(s))}, s...) }

func textRowPayload(values ...string) []byte {
	var p []byte
	for _, v := range values {
		p = append(p, lenencStr(v)...)
	}
	return p
}

// binaryRowPayload is a row of (id BIGINT, email VARCHAR); a nil email leaves
// its bytes out, though the null bitmap says it is there.
func binaryRowPayload(id int64, email *string) []byte {
	p := []byte{0x00, 0x00} // OK header, null bitmap (2 columns + 2 reserved bits: 1 byte)
	p = binary.LittleEndian.AppendUint64(p, uint64(id))
	if email != nil {
		p = append(p, lenencStr(*email)...)
	}
	return p
}

func str(s string) *string { return &s }

// undecodableDateColumn replaces the email column in the DATE cases.
var undecodableDateColumn = &mysql.ColumnDefinition41{Catalog: "def", Schema: "app", Table: "users", OrgTable: "users",
	Name: "born", OrgName: "born", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 10, Type: byte(mysql.FieldTypeDate)}

// binaryDateRowPayload is a row of (id BIGINT, born DATE); a nil born leaves
// its bytes out, though the null bitmap says it is there.
func binaryDateRowPayload(id int64, born []byte) []byte {
	p := []byte{0x00, 0x00} // OK header, null bitmap
	p = binary.LittleEndian.AppendUint64(p, uint64(id))
	return append(p, born...)
}

// dateValue is a DATE in the binary protocol: length 4, year, month, day.
func dateValue(y uint16, m, d byte) []byte {
	return append(binary.LittleEndian.AppendUint16([]byte{4}, y), m, d)
}

var (
	eofPayload = []byte{0xfe, 0x00, 0x00, 0x02, 0x00}
	// okEOFPayload is the OK packet that ends a result set under
	// CLIENT_DEPRECATE_EOF.
	okEOFPayload = []byte{0xfe, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
	// Column definitions the decoder must refuse: the schema's 0xfc length
	// prefix cut short, and an 8-byte length that overflows int.
	truncatedPrefixColumn = append(lenencStr("def"), 0xfc)
	overflowingColumn     = append(append(lenencStr("def"), 0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff), 'x')
)

// frame wraps a response's payloads in packet headers, sequence ids from 1.
func frame(payloads [][]byte) [][]byte {
	out := make([][]byte, len(payloads))
	for i, p := range payloads {
		out[i] = wrapPacket(p, byte(i+1))
	}
	return out
}

// resultSet frames a result set: column count, columns, rows, and the EOF
// packets the framing calls for.
func resultSet(deprecateEOF bool, cols, rows [][]byte) [][]byte {
	p := append([][]byte{{byte(len(cols))}}, cols...)
	if !deprecateEOF {
		p = append(p, eofPayload)
	}
	p = append(p, rows...)
	if deprecateEOF {
		p = append(p, okEOFPayload)
	} else {
		p = append(p, eofPayload)
	}
	return frame(p)
}

// prepareOK frames a COM_STMT_PREPARE reply: PREPARE_OK, parameter and
// column definitions, and the EOF packets the framing calls for.
func prepareOK(deprecateEOF bool, params, cols [][]byte) [][]byte {
	ok := []byte{0x00, 0, 0, 0, 0, byte(len(cols)), 0, byte(len(params)), 0, 0x00, 0, 0}
	binary.LittleEndian.PutUint32(ok[1:], undecodableStmtID)
	p := append([][]byte{ok}, params...)
	if len(params) > 0 && !deprecateEOF {
		p = append(p, eofPayload)
	}
	p = append(p, cols...)
	if len(cols) > 0 && !deprecateEOF {
		p = append(p, eofPayload)
	}
	return frame(p)
}

type undecodableCase struct {
	name string
	op   string // the command's requestOperation
	// command is the client's packet, and reply the server's.
	command []byte
	reply   func(t *testing.T, deprecateEOF bool) [][]byte
	// parts counts what a recorded response holds (rows, or a prepared
	// statement's definitions); a whole reply has 3.
	parts func(*models.Mock) int
	// valid marks the control cases: the same shape, every packet whole.
	valid bool
}

func undecodableCases() []undecodableCase {
	col0 := func(t *testing.T) []byte { return columnPayload(t, undecodableColumns[0]) }
	col1 := func(t *testing.T) []byte { return columnPayload(t, undecodableColumns[1]) }

	query := queuedQuery(0, "SELECT id, email FROM users")
	textRows := func(m *models.Mock) int {
		if rs, _ := m.Spec.MySQLResponses[0].Message.(*mysql.TextResultSet); rs != nil {
			return len(rs.Rows)
		}
		return -1
	}
	text := func(name string, valid bool, col func(*testing.T) []byte, row []byte) undecodableCase {
		return undecodableCase{name: "text/" + name, op: "COM_QUERY", command: query, parts: textRows, valid: valid,
			reply: func(t *testing.T, deprecateEOF bool) [][]byte {
				if row == nil {
					row = textRowPayload("2", "b@example.com")
				}
				return resultSet(deprecateEOF, [][]byte{col0(t), col(t)},
					[][]byte{textRowPayload("1", "a@example.com"), row, textRowPayload("3", "c@example.com")})
			}}
	}

	execute := queuedExecute(0, undecodableStmtID)
	binaryRows := func(m *models.Mock) int {
		if rs, _ := m.Spec.MySQLResponses[0].Message.(*mysql.BinaryProtocolResultSet); rs != nil {
			return len(rs.Rows)
		}
		return -1
	}
	bin := func(name string, valid bool, col func(*testing.T) []byte, row []byte) undecodableCase {
		return undecodableCase{name: "binary/" + name, op: "COM_STMT_EXECUTE", command: execute, parts: binaryRows, valid: valid,
			reply: func(t *testing.T, deprecateEOF bool) [][]byte {
				if row == nil {
					row = binaryRowPayload(2, str("b@example.com"))
				}
				return resultSet(deprecateEOF, [][]byte{col0(t), col(t)},
					[][]byte{binaryRowPayload(1, str("a@example.com")), row, binaryRowPayload(3, str("c@example.com"))})
			}}
	}
	cutString := binaryRowPayload(2, str("b@example.com"))
	cutString = cutString[:len(cutString)-4]
	binDate := func(name string, valid bool, row []byte) undecodableCase {
		return undecodableCase{name: "binary/" + name, op: "COM_STMT_EXECUTE", command: execute, parts: binaryRows, valid: valid,
			reply: func(t *testing.T, deprecateEOF bool) [][]byte {
				if row == nil {
					row = binaryDateRowPayload(2, dateValue(1991, 2, 3))
				}
				return resultSet(deprecateEOF, [][]byte{col0(t), columnPayload(t, undecodableDateColumn)},
					[][]byte{binaryDateRowPayload(1, dateValue(1990, 1, 2)), row, binaryDateRowPayload(3, dateValue(1992, 3, 4))})
			}}
	}

	prepare := wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id, email FROM users WHERE id > ?"...), 0)
	prepareParts := func(m *models.Mock) int {
		if sp, _ := m.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket); sp != nil {
			return len(sp.ParamDefs) + len(sp.ColumnDefs)
		}
		return -1
	}
	prep := func(name string, valid bool, param, col func(*testing.T) []byte) undecodableCase {
		return undecodableCase{name: "prepare/" + name, op: "COM_STMT_PREPARE", command: prepare, parts: prepareParts, valid: valid,
			reply: func(t *testing.T, deprecateEOF bool) [][]byte {
				return prepareOK(deprecateEOF, [][]byte{param(t)}, [][]byte{col0(t), col(t)})
			}}
	}
	bad := func(b []byte) func(*testing.T) []byte { return func(*testing.T) []byte { return b } }

	return []undecodableCase{
		text("whole", true, col1, nil),
		text("column length prefix cut short", false, bad(truncatedPrefixColumn), nil),
		text("column length past the packet", false, bad(overflowingColumn), nil),
		text("row with fewer values than columns", false, col1, textRowPayload("2")),
		bin("whole", true, col1, nil),
		bin("column length prefix cut short", false, bad(truncatedPrefixColumn), nil),
		// The string's length byte is missing: an empty buffer where a
		// length-encoded string must start, read as an empty value before.
		bin("row cut before its last value", false, col1, binaryRowPayload(2, nil)),
		bin("row with a string value cut short", false, col1, cutString),
		// A row that is only its OK byte sliced past the packet for its null
		// bitmap, and the panic took the recorder down with it.
		bin("row that is only its OK byte", false, col1, []byte{0x00}),
		// A DATE value cut off after the null bitmap: the date parsers read an
		// empty slice as "nothing to parse" and returned (nil, 0, nil), so the
		// row was recorded with a NULL the server never sent.
		binDate("DATE whole", true, nil),
		binDate("row cut before its DATE value", false, binaryDateRowPayload(2, nil)),
		// A DATE whose length byte is not a protocol value was trusted as the
		// bytes it took, misaligning (or running past) the rest of the row.
		binDate("DATE with a length that is not a protocol value", false,
			binaryDateRowPayload(2, append([]byte{5}, dateValue(1991, 2, 3)[1:]...))),
		prep("whole", true, col0, col1),
		prep("parameter length prefix cut short", false, bad(truncatedPrefixColumn), col1),
		prep("last column length prefix cut short", false, col0, bad(truncatedPrefixColumn)),
	}
}

// preparedFirst is a COM_STMT_PREPARE of undecodableStmtID, so that RecordV2
// can decode a COM_STMT_EXECUTE of it.
func preparedFirst(t *testing.T, h *v2Harness, at func() time.Time) {
	h.pushClient(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id, email FROM users WHERE id > 0"...), 0), at())
	for _, p := range prepareOK(false, nil, [][]byte{columnPayload(t, undecodableColumns[0]), columnPayload(t, undecodableColumns[1])}) {
		h.pushDest(p, at())
	}
}

// runRecordV2 records one connection: the handshake, then exchanges pushed by
// push, then a close. It returns RecordV2's error and every mock emitted.
func runRecordV2(t *testing.T, push func(h *v2Harness, at func() time.Time)) ([]*models.Mock, error) {
	t.Helper()
	h := newV2Harness(t)
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	tick := 0
	at := func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Millisecond) }

	greetingBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), greetingBuf[4:])
	if err != nil {
		t.Fatal(err)
	}
	// The canned greeting does not offer CLIENT_DEPRECATE_EOF: EOF framing.
	h.pushDest(greetingBuf, at())
	h.pushClient(cannedHandshakeResponse41(t, 1, false), at())
	h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), at())
	push(h, at)
	h.closeStreams()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = RecordV2(ctx, h.logger, h.sess)
	var got []*models.Mock
	for {
		select {
		case m := <-h.mocks:
			got = append(got, m)
		default:
			return got, err
		}
	}
}

func mocksOf(mocks []*models.Mock, op string) []*models.Mock {
	var out []*models.Mock
	for _, m := range mocks {
		if m.Spec.Metadata["requestOperation"] == op {
			out = append(out, m)
		}
	}
	return out
}

func TestRecordV2_UndecodableResponseIsAnError(t *testing.T) {
	t.Parallel()
	for _, c := range undecodableCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := runRecordV2(t, func(h *v2Harness, at func() time.Time) {
				if c.op == "COM_STMT_EXECUTE" {
					preparedFirst(t, h, at)
				}
				h.pushClient(c.command, at())
				for _, p := range c.reply(t, false) {
					h.pushDest(p, at())
				}
			})
			recorded := mocksOf(got, c.op)
			if c.op == "COM_STMT_EXECUTE" && len(mocksOf(got, "COM_STMT_PREPARE")) != 1 {
				t.Fatalf("the COM_STMT_PREPARE before the execute was not recorded: %d mocks", len(got))
			}
			if c.valid {
				if err != nil {
					t.Fatalf("RecordV2 = %v on a whole response", err)
				}
				if len(recorded) != 1 || c.parts(recorded[0]) != 3 {
					t.Fatalf("a whole response recorded %d %s mocks (want 1 with 3 parts)", len(recorded), c.op)
				}
				return
			}
			if err == nil {
				t.Fatal("RecordV2 ended cleanly: the supervisor takes the parser for healthy, and nothing says the response was lost")
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("RecordV2 = %v: an io.EOF passes for the connection closing", err)
			}
			// A definition or row that does not decode, behind a header whose
			// sequence id checked out, is lost framing: a misread header that
			// passed the check, or a packet short of bytes. It is reported as
			// lost framing is: once, at WARN, rate-limited across connections,
			// not again at ERROR for every connection it stops.
			if !errors.Is(err, ErrFramingLost) {
				t.Fatalf("RecordV2 = %v, want ErrFramingLost", err)
			}
			if len(recorded) != 0 {
				t.Fatalf("recorded %d %s mocks for a response that did not decode (the first has %d of 3 parts)", len(recorded), c.op, c.parts(recorded[0]))
			}
		})
	}
}

// The legacy async decoder has no supervisor to hand the connection to: it
// drops the one exchange whose response did not decode, leaves out the test
// cases recorded over it, and goes on recording the exchanges after it.
// Before, a row that did not decode was skipped and the mock emitted short; a
// column or parameter definition that did not decode reset the exchange
// mid-response, and its remaining packets were held and replayed as the NEXT
// command's response.
func TestAsyncMySQLDecode_UndecodableResponseIsDroppedNotRecordedShort(t *testing.T) {
	t.Parallel()
	for _, deprecateEOF := range []bool{false, true} {
		for _, c := range undecodableCases() {
			t.Run(fmt.Sprintf("deprecateEOF=%v/%s", deprecateEOF, c.name), func(t *testing.T) {
				clientConn := newPipeConn()
				decodeCtx := buildPostHandshakeDecodeCtx(clientConn)
				if !deprecateEOF {
					decodeCtx.ClientCaps &^= wire.CLIENT_DEPRECATE_EOF
					decodeCtx.ClientCapabilities = decodeCtx.ClientCaps
					decodeCtx.ServerCaps = decodeCtx.ClientCaps
				}
				if decodeCtx.DeprecateEOF() != deprecateEOF {
					t.Fatal("the decode context does not negotiate the framing under test")
				}
				decodeCtx.LastOp.Store(clientConn, wire.RESET)
				decodeCtx.PreparedStatements[undecodableStmtID] = &mysql.StmtPrepareOkPacket{StatementID: undecodableStmtID, NumColumns: 2}
				mocks := make(chan *models.Mock, 16)
				mgr := syncMock.New(zaptest.NewLogger(t))
				mgr.SetOutputChannel(mocks)
				ctx := syncMock.NewContext(context.WithValue(context.Background(), models.ClientConnectionIDKey, "undecodable-"+c.name), mgr)

				base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
				items := make(chan mysqlDecodeItem, 32)
				tick := 0
				send := func(fromClient bool, b []byte) {
					tick++
					items <- mysqlDecodeItem{fromClient: fromClient, data: b, ts: base.Add(time.Duration(tick) * time.Millisecond)}
				}
				send(true, c.command)
				exchange := base.Add(time.Duration(tick) * time.Millisecond)
				for _, p := range c.reply(t, deprecateEOF) {
					send(false, p)
				}
				// The next exchange must pair with its own OK (7 affected rows).
				send(true, queuedQuery(0, "UPDATE users SET seen = 1"))
				next := base.Add(time.Duration(tick) * time.Millisecond)
				send(false, queuedOK(1, 7))
				close(items)

				asyncMySQLDecode(ctx, zaptest.NewLogger(t), items, mocks, decodeCtx, clientConn, models.OutgoingOptions{})

				var recorded, updates []*models.Mock
				for _, m := range drainMocks(t, mocks) {
					if q, _ := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket); q != nil && q.Query == "UPDATE users SET seen = 1" {
						updates = append(updates, m)
					} else if m.Spec.Metadata["requestOperation"] == c.op {
						recorded = append(recorded, m)
					} else {
						t.Fatalf("unexpected mock for %s", m.Spec.Metadata["requestOperation"])
					}
				}
				if len(updates) != 1 {
					t.Fatalf("the command after the response recorded %d mocks, want 1", len(updates))
				}
				if ok, _ := updates[0].Spec.MySQLResponses[0].Message.(*mysql.OKPacket); ok == nil || ok.AffectedRows != 7 {
					t.Fatalf("the command after the response was paired with %T %+v, not its own OK", updates[0].Spec.MySQLResponses[0].Message, updates[0].Spec.MySQLResponses[0].Message)
				}
				if orphaned, _ := mgr.WasMockOrphanedInWindow(next, next); orphaned {
					t.Fatal("the test cases over the next exchange are left out")
				}
				orphaned, _ := mgr.WasMockOrphanedInWindow(exchange, exchange)
				if c.valid {
					if len(recorded) != 1 || c.parts(recorded[0]) != 3 {
						t.Fatalf("a whole response recorded %d mocks (want 1 with 3 parts)", len(recorded))
					}
					if orphaned {
						t.Fatal("the test cases over a whole response are left out")
					}
					return
				}
				if len(recorded) != 0 {
					t.Fatalf("recorded a mock for a response that did not decode (%d of 3 parts)", c.parts(recorded[0]))
				}
				if !orphaned {
					t.Fatal("the test cases recorded over the dropped exchange are kept, and will replay without its mock")
				}
			})
		}
	}
}

// A COM_STMT_PREPARE the legacy decoder drops takes the statement's executes
// with it: an execute cannot replay without its prepare, so it is dropped as
// well and the test cases over it are left out, rather than saved to fail.
func TestAsyncMySQLDecode_ExecuteOfADroppedPrepareIsDropped(t *testing.T) {
	t.Parallel()
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprintf("prepare whole=%v", valid), func(t *testing.T) {
			clientConn := newPipeConn()
			decodeCtx := buildPostHandshakeDecodeCtx(clientConn)
			decodeCtx.LastOp.Store(clientConn, wire.RESET)
			mocks := make(chan *models.Mock, 16)
			mgr := syncMock.New(zaptest.NewLogger(t))
			mgr.SetOutputChannel(mocks)
			ctx := syncMock.NewContext(context.WithValue(context.Background(), models.ClientConnectionIDKey, "lost-prepare"), mgr)

			base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			items := make(chan mysqlDecodeItem, 32)
			tick := 0
			send := func(fromClient bool, b []byte) time.Time {
				tick++
				at := base.Add(time.Duration(tick) * time.Millisecond)
				items <- mysqlDecodeItem{fromClient: fromClient, data: b, ts: at}
				return at
			}
			col1 := columnPayload(t, undecodableColumns[1])
			if !valid {
				col1 = truncatedPrefixColumn
			}
			send(true, wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id, email FROM users"...), 0))
			for _, p := range prepareOK(true, nil, [][]byte{columnPayload(t, undecodableColumns[0]), col1}) {
				send(false, p)
			}
			execute := send(true, queuedExecute(0, undecodableStmtID))
			for _, p := range resultSet(true, [][]byte{columnPayload(t, undecodableColumns[0]), columnPayload(t, undecodableColumns[1])},
				[][]byte{binaryRowPayload(1, str("a@example.com"))}) {
				send(false, p)
			}
			next := send(true, queuedQuery(0, "UPDATE users SET seen = 1"))
			send(false, queuedOK(1, 7))
			close(items)

			asyncMySQLDecode(ctx, zaptest.NewLogger(t), items, mocks, decodeCtx, clientConn, models.OutgoingOptions{})

			got := map[string]int{}
			for _, m := range drainMocks(t, mocks) {
				got[m.Spec.Metadata["requestOperation"]]++
			}
			if got["COM_QUERY"] != 1 {
				t.Fatalf("the command after them recorded %d mocks, want 1", got["COM_QUERY"])
			}
			if orphaned, _ := mgr.WasMockOrphanedInWindow(next, next); orphaned {
				t.Fatal("the test cases over the next command are left out")
			}
			orphaned, _ := mgr.WasMockOrphanedInWindow(execute, execute)
			if valid {
				if got["COM_STMT_PREPARE"] != 1 || got["COM_STMT_EXECUTE"] != 1 || orphaned {
					t.Fatalf("a whole prepare and its execute recorded %v, execute left out: %v", got, orphaned)
				}
				return
			}
			if got["COM_STMT_PREPARE"] != 0 || got["COM_STMT_EXECUTE"] != 0 {
				t.Fatalf("recorded %v: an execute of a prepare that was not recorded cannot replay", got)
			}
			if !orphaned {
				t.Fatal("the test cases over the execute are kept, and will fail at replay without its prepare")
			}
		})
	}
}
