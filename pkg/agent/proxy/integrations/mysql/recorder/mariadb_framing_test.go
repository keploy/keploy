package recorder

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// mariaDBExtCaps are the extended capabilities both sides send in the rows
// below: MARIADB_CLIENT_EXTENDED_METADATA and MARIADB_CLIENT_CACHE_METADATA.
const mariaDBExtCaps = mysql.MARIADB_CLIENT_EXTENDED_METADATA | mysql.MARIADB_CLIENT_CACHE_METADATA

// mariaDBLoginCaps are the capability flags both sides send in mariaDBLogin:
// CLIENT_MYSQL clear, no CLIENT_DEPRECATE_EOF.
const mariaDBLoginCaps = uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION)

// jsonFormat is a column definition's extended type information for a JSON
// column, as MariaDB 10.6 sends it: 6 bytes, format (1) "json".
var jsonFormat = []byte{0x06, 0x01, 0x04, 'j', 's', 'o', 'n'}

// mariaDBColumn is the payload of a column definition as MariaDB sends it on a
// connection that negotiated MARIADB_CLIENT_EXTENDED_METADATA: ext, its
// extended type information, after org_name. It is written out by hand, not
// by the encoder under test.
func mariaDBColumn(name string, ext []byte, charset uint16, length uint32, typ byte) []byte {
	var p []byte
	for _, s := range []string{"def", "demo", "t", "t", name, name} {
		p = append(p, lenencStr(s)...)
	}
	p = append(p, ext...)
	p = append(p, 0x0c)
	p = binary.LittleEndian.AppendUint16(p, charset)
	p = binary.LittleEndian.AppendUint32(p, length)
	p = append(p, typ, 0x90, 0x00, 0x00, 0x00, 0x00)
	return p
}

// mariaDBGreeting is a MariaDB server's greeting: CLIENT_MYSQL clear, and its
// extended capabilities in the last four reserved bytes.
func mariaDBGreeting(t *testing.T, caps, ext uint32) []byte {
	t.Helper()
	gb, err := connphase.EncodeHandshakeV10(context.Background(), zap.NewNop(), &mysql.HandshakeV10Packet{
		ProtocolVersion: 0x0a, ServerVersion: "5.5.5-10.6.28-MariaDB", ConnectionID: 42,
		AuthPluginData: bytes.Repeat([]byte{0x11}, 20), CapabilityFlags: caps, CharacterSet: 0x2d, StatusFlags: 0x02,
		MariaDBCapabilities: ext, AuthPluginName: string(mysql.Native),
	})
	if err != nil {
		t.Fatal(err)
	}
	if caps&mysql.CLIENT_MYSQL != 0 {
		t.Fatal("a MariaDB greeting has CLIENT_MYSQL clear")
	}
	return wrapPacket(gb, 0)
}

// mariaDBLogin starts a recording of a connection that logs in to MariaDB
// with MariaDB Connector/C's capabilities (EOF framing), and returns it, what
// it leaves out, and its capture clock.
func mariaDBLogin(t *testing.T) (*v2Harness, *orphanSpans, func() time.Time) {
	t.Helper()
	return mariaDBLoginWith(t, mariaDBExtCaps)
}

// mariaDBLoginWith is mariaDBLogin with ext the extended capabilities both
// sides send.
func mariaDBLoginWith(t *testing.T, ext uint32) (*v2Harness, *orphanSpans, func() time.Time) {
	t.Helper()
	h := newV2Harness(t)
	leftOut := &orphanSpans{}
	h.sess.Orphans = leftOut
	tick := 0
	at := func() time.Time { tick++; return wireBase.Add(time.Duration(tick) * time.Millisecond) }
	caps := mariaDBLoginCaps
	h.pushDest(mariaDBGreeting(t, caps, ext), at())
	hr := &mysql.HandshakeResponse41Packet{CapabilityFlags: caps, MaxPacketSize: 1 << 24, CharacterSet: 0x2d,
		Username: "root", AuthResponse: bytes.Repeat([]byte{0xAB}, 20), AuthPluginName: string(mysql.Native)}
	binary.LittleEndian.PutUint32(hr.Filler[19:], ext)
	hb, err := connphase.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), hr)
	if err != nil {
		t.Fatal(err)
	}
	h.pushClient(wrapPacket(hb, 1), at())
	h.pushDest(cannedOK(t, 2, caps), at())
	return h, leftOut, at
}

// recordAll runs RecordV2 on h's streams, closed, and returns what it emitted.
func recordAll(t *testing.T, h *v2Harness) []*models.Mock {
	t.Helper()
	h.closeStreams()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := recordV2NoPanic(t, ctx, h.logger, h.sess); err != nil {
		t.Fatalf("RecordV2: %v", err)
	}
	var got []*models.Mock
	for len(h.mocks) > 0 {
		got = append(got, <-h.mocks)
	}
	return got
}

// A prepared statement on MariaDB with MariaDB Connector/C's capabilities:
// its PREPARE_OK's definitions and its EXECUTE's binary result set carry each
// column's extended type information, and the result set's column count its
// metadata-follows byte. Both are recorded, with the JSON column's format.
func TestRecordV2_AMariaDBPreparedStatementIsRecordedWithItsExtendedMetadata(t *testing.T) {
	t.Parallel()
	h, leftOut, at := mariaDBLogin(t)
	param := mariaDBColumn("?", []byte{0x00}, 0x3f, 0, byte(mysql.FieldTypeVarString))
	data := mariaDBColumn("data", jsonFormat, 0x2d, 0xffffffff, byte(mysql.FieldTypeBLOB))
	h.pushClient(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT data FROM t WHERE id = ? AND n = ?"...), 0), at())
	for _, p := range prepareOK(false, [][]byte{param, param}, [][]byte{data}) {
		h.pushDest(p, at())
	}
	h.pushClient(typedExecuteCommand(undecodableStmtID, true, 1, "a"), at())
	row := append([]byte{0x00, 0x00}, lenencStr(`{"k":1}`)...)
	for _, p := range frame([][]byte{{0x01, 0x01}, data, eofPayload, row, eofPayload}) {
		h.pushDest(p, at())
	}
	got := recordAll(t, h)
	if n := leftOut.count(); n != 0 {
		t.Fatalf("%d exchanges left out, want none", n)
	}
	json := []mysql.ColumnExtendedMetadata{{Kind: 1, Value: "json"}}
	isJSON := func(c *mysql.ColumnDefinition41) bool {
		return c.Name == "data" && c.Type == byte(mysql.FieldTypeBLOB) && c.ColumnLength == 0xffffffff && c.CharacterSet == 0x2d &&
			len(c.ExtendedMetadata) == 1 && c.ExtendedMetadata[0] == json[0]
	}

	prepares := mocksOf(got, "COM_STMT_PREPARE")
	if len(prepares) != 1 {
		t.Fatalf("recorded %d PREPAREs, want 1", len(prepares))
	}
	sp, ok := prepares[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
	if !ok || len(sp.ParamDefs) != 2 || len(sp.ColumnDefs) != 1 {
		t.Fatalf("the PREPARE recorded %+v, want 2 parameters and 1 column", prepares[0].Spec.MySQLResponses[0].Message)
	}
	for i, p := range sp.ParamDefs {
		if p.Name != "?" || p.Type != byte(mysql.FieldTypeVarString) || p.CharacterSet != 0x3f || len(p.ExtendedMetadata) != 0 {
			t.Errorf("parameter %d recorded as %+v", i, p)
		}
	}
	if !isJSON(sp.ColumnDefs[0]) {
		t.Errorf("the PREPARE's column recorded as %+v, want the JSON column", sp.ColumnDefs[0])
	}

	executes := mocksOf(got, "COM_STMT_EXECUTE")
	if len(executes) != 1 {
		t.Fatalf("recorded %d EXECUTEs, want 1", len(executes))
	}
	rs, ok := executes[0].Spec.MySQLResponses[0].Message.(*mysql.BinaryProtocolResultSet)
	if !ok || rs.ColumnCount != 1 || len(rs.Columns) != 1 || len(rs.Rows) != 1 {
		t.Fatalf("the EXECUTE recorded %+v, want a result set of 1 column and 1 row", executes[0].Spec.MySQLResponses[0].Message)
	}
	if !isJSON(rs.Columns[0]) {
		t.Errorf("the EXECUTE's column recorded as %+v, want the JSON column", rs.Columns[0])
	}
	if v := rs.Rows[0].Values; len(v) != 1 || v[0].Value != `{"k":1}` {
		t.Errorf("the EXECUTE's row recorded as %+v", v)
	}
}

// A MariaDB server leaves out an EXECUTE's column definitions when the client
// has them cached (metadata-follows 0). Read as if they followed, its rows
// would be recorded as definitions: the exchange is left out instead, and the
// commands after it are still recorded.
func TestRecordV2_AMariaDBResultSetWithoutItsDefinitionsIsLeftOut(t *testing.T) {
	t.Parallel()
	h, leftOut, at := mariaDBLogin(t)
	core, logs := observer.New(zapcore.DebugLevel)
	h.logger = zap.New(core)
	h.sess.Logger = h.logger
	param := mariaDBColumn("?", []byte{0x00}, 0x3f, 0, byte(mysql.FieldTypeVarString))
	data := mariaDBColumn("data", jsonFormat, 0x2d, 0xffffffff, byte(mysql.FieldTypeBLOB))
	h.pushClient(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT data FROM t WHERE id = ? AND n = ?"...), 0), at())
	for _, p := range prepareOK(false, [][]byte{param, param}, [][]byte{data}) {
		h.pushDest(p, at())
	}
	h.pushClient(typedExecuteCommand(undecodableStmtID, true, 1, "a"), at())
	row := append([]byte{0x00, 0x00}, lenencStr(`{"k":1}`)...)
	for _, p := range frame([][]byte{{0x01, 0x00}, row, eofPayload}) {
		h.pushDest(p, at())
	}
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), at())
	h.pushDest(cannedOK(t, 1, mariaDBLoginCaps), at())
	// The query after SELECT 1 confirms it, taken up after the EXECUTE left
	// out (takenUp): the end of the connection would not.
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 2"), at())
	h.pushDest(cannedOK(t, 1, mariaDBLoginCaps), at())
	got := recordAll(t, h)
	if n := len(mocksOf(got, "COM_STMT_EXECUTE")); n != 0 {
		t.Errorf("recorded %d EXECUTEs, want the one without its definitions left out", n)
	}
	if n := leftOut.count(); n != 1 {
		t.Errorf("%d exchanges left out, want the EXECUTE's", n)
	}
	// Left out for what it is, not for rows misread as definitions.
	// Its reason is said once, at WARN or, past the WARN's rate, at DEBUG.
	if n := logs.FilterFieldKey("reason").Filter(func(e observer.LoggedEntry) bool {
		return strings.Contains(e.ContextMap()["reason"].(string), "leaves out its column definitions (MariaDB's metadata cache)")
	}).Len(); n != 1 {
		t.Errorf("%d exchanges left out for leaving out their definitions, want the EXECUTE", n)
	}
	if n := len(mocksOf(got, "COM_QUERY")); n != 2 {
		t.Errorf("recorded %d COM_QUERYs after it, want 2", n)
	}
}

// A multi-statement query on MariaDB answers with several results. A mock
// holds one, so the exchange is framed whole and left out for that, and the
// next command is recorded. Each further result's column count is framed as
// the first one's, as the connection negotiated: under the metadata cache it
// carries the metadata-follows byte, and with extended type information alone
// (MariaDB 10.5) it does not. A further result without its definitions cannot
// be framed by reading them, so the framing is lost there and the recording
// goes on from the next command.
func TestRecordV2_AMariaDBResponseOfSeveralResultsIsLeftOutForWhatItIs(t *testing.T) {
	t.Parallel()
	more := []byte{0xfe, 0x00, 0x00, 0x0a, 0x00} // EOF: SERVER_STATUS_AUTOCOMMIT | SERVER_MORE_RESULTS_EXISTS
	extCol := mariaDBColumn("n", []byte{0x00}, 0x3f, 1, byte(mysql.FieldTypeLongLong))
	plainCol := mariaDBColumn("n", nil, 0x3f, 1, byte(mysql.FieldTypeLongLong))
	for _, c := range []struct {
		name  string
		ext   uint32
		reply [][]byte
		why   []string // each is in the one reason
	}{
		{"under the metadata cache", mariaDBExtCaps,
			[][]byte{{0x01, 0x01}, extCol, eofPayload, lenencStr("1"), more, {0x01, 0x01}, extCol, eofPayload, lenencStr("2"), eofPayload},
			[]string{"holds more than one result"}},
		// MariaDB 10.5: extended type information without the metadata
		// cache, so no metadata-follows byte after either count.
		{"extended type information without the metadata cache", mysql.MARIADB_CLIENT_EXTENDED_METADATA,
			[][]byte{{0x01}, extCol, eofPayload, lenencStr("1"), more, {0x01}, extCol, eofPayload, lenencStr("2"), eofPayload},
			[]string{"holds more than one result"}},
		{"without MariaDB's metadata extensions", 0,
			[][]byte{{0x01}, plainCol, eofPayload, lenencStr("1"), more, {0x01}, plainCol, eofPayload, lenencStr("2"), eofPayload},
			[]string{"holds more than one result"}},
		// The framing is lost there: its rows are not read as definitions.
		{"a further result without its definitions", mariaDBExtCaps,
			[][]byte{{0x01, 0x01}, extCol, eofPayload, lenencStr("1"), more, {0x01, 0x00}, lenencStr("2"), eofPayload},
			[]string{"cannot be framed", "leaves out its column definitions (MariaDB's metadata cache)"}},
		// A byte past the count and its metadata-follows byte: not a column
		// count packet, whatever its first bytes say.
		{"a further result's head longer than its column count", mariaDBExtCaps,
			[][]byte{{0x01, 0x01}, extCol, eofPayload, lenencStr("1"), more, {0x01, 0x01, 0x00}, extCol, eofPayload, lenencStr("2"), eofPayload},
			[]string{"cannot be framed", "is not a column count"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, leftOut, at := mariaDBLoginWith(t, c.ext)
			core, logs := observer.New(zapcore.DebugLevel)
			h.logger = zap.New(core)
			h.sess.Logger = h.logger
			h.pushClient(cannedCOMQuery(t, 0, "SELECT 1; SELECT 2"), at())
			for _, p := range frame(c.reply) {
				h.pushDest(p, at())
			}
			h.pushClient(cannedCOMQuery(t, 0, "SELECT 3"), at())
			h.pushDest(cannedOK(t, 1, mariaDBLoginCaps), at())
			// The query after SELECT 3 confirms it where it is taken up
			// after the response left out (takenUp): the end of the
			// connection would not.
			h.pushClient(cannedCOMQuery(t, 0, "SELECT 4"), at())
			h.pushDest(cannedOK(t, 1, mariaDBLoginCaps), at())
			got := recordAll(t, h)
			if n := leftOut.count(); n != 1 {
				t.Errorf("%d exchanges left out, want the multi-statement query's", n)
			}
			reasons := logs.FilterFieldKey("reason").All()
			var rs []string
			for _, e := range reasons {
				rs = append(rs, e.ContextMap()["reason"].(string))
			}
			if len(rs) != 1 {
				t.Errorf("left out for %q, want once for %q", rs, c.why)
			} else {
				for _, why := range c.why {
					if !strings.Contains(rs[0], why) {
						t.Errorf("left out for %q, want %q in it", rs[0], why)
					}
				}
			}
			if n := len(mocksOf(got, "COM_QUERY")); n != 2 {
				t.Errorf("recorded %d COM_QUERYs, want the two after the multi-statement query", n)
			}
		})
	}
}

// A connection joined mid-stream on its decrypted TLS stream is framed by its
// own SSLRequest's capabilities, MariaDB's extended ones too: its result set,
// with a JSON column and a metadata-follows byte, is recorded. An SSLRequest
// that is not provably the connection's frames nothing: its extended
// capabilities are not assumed, and the column definition read without them
// is refused (the exchange is left out, not recorded misread).
func TestRecordV2_PostTLS_AJoinedMariaDBConnectionIsFramedByItsOwnSSLRequest(t *testing.T) {
	t.Parallel()
	serverCaps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SSL |
		mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_DEPRECATE_EOF)
	data := mariaDBColumn("data", jsonFormat, 0x2d, 0xffffffff, byte(mysql.FieldTypeBLOB))
	reply := frame([][]byte{{0x01, 0x01}, data, textRowPayload(`{"k":1}`), textRowPayload("[1,2]"), okEOFPayload})
	query := cannedCOMQuery(t, 0, "SELECT data FROM t")
	for _, own := range []bool{true, false} {
		j := newJoinedMidStream(t)
		j.greeting = mariaDBGreeting(t, serverCaps, mariaDBExtCaps)
		store, _ := j.ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
		body := make([]byte, 32)
		binary.LittleEndian.PutUint32(body[0:4], serverCaps)
		binary.LittleEndian.PutUint32(body[4:8], 1<<24)
		body[8] = 0x2d
		binary.LittleEndian.PutUint32(body[28:32], mariaDBExtCaps)
		entry := models.TLSHandshakeEntry{RespPackets: [][]byte{j.greeting}, ReqPackets: [][]byte{wrapPacket(body, 1)}, ReqTimestamp: time.Now()}
		if own {
			owner := models.HandshakeOwner{Conn: "pooled-conn-7", Proc: "app"}
			j.h.sess.Opts.ConnKey, j.h.sess.Opts.ConnProc = owner.Conn, owner.Proc
			store.PushFor(models.HandshakeStoreKey("", 3306), owner, entry)
		} else {
			store.RememberLast(lastGreetingKey(j.h.sess.Opts.PassThroughScope, j.h.sess.Opts.NetNS, j.h.sess.Opts.DstCfg), entry)
		}
		leftOut := &orphanSpans{}
		j.h.sess.Orphans = leftOut
		j.exchange(query, reply)
		mocks, err := j.run(t)
		if err != nil {
			t.Fatalf("own=%v: RecordV2 = %v", own, err)
		}
		if !own {
			if len(mocks) != 0 || leftOut.count() != 1 {
				t.Errorf("not own: %d mock(s), %d exchange(s) left out; want the SELECT left out", len(mocks), leftOut.count())
			}
			continue
		}
		if len(mocks) != 1 || leftOut.count() != 0 {
			t.Fatalf("own: %d mock(s), %d exchange(s) left out; want the SELECT recorded", len(mocks), leftOut.count())
		}
		rs, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.TextResultSet)
		if !ok || len(rs.Columns) != 1 || len(rs.Rows) != 2 {
			t.Fatalf("own: the SELECT recorded %+v, want 1 column and 2 rows", mocks[0].Spec.MySQLResponses[0].Message)
		}
		c := rs.Columns[0]
		if c.Name != "data" || c.Type != byte(mysql.FieldTypeBLOB) || c.ColumnLength != 0xffffffff ||
			len(c.ExtendedMetadata) != 1 || c.ExtendedMetadata[0] != (mysql.ColumnExtendedMetadata{Kind: 1, Value: "json"}) {
			t.Errorf("own: the column recorded as %+v, want the JSON column", c)
		}
		if v := rs.Rows[1].Values; len(v) != 1 || v[0].Value != "[1,2]" {
			t.Errorf("own: the second row recorded as %+v", v)
		}
	}
}
