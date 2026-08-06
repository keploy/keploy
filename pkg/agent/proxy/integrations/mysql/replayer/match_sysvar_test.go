package replayer

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	mysqlutils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

func TestParseSingleSystemVarRead(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		wantVar string
		wantOK  bool
	}{
		{"bare", "SELECT @@transaction_isolation", "transaction_isolation", true},
		{"session prefix", "SELECT @@session.transaction_isolation", "transaction_isolation", true},
		{"global prefix", "SELECT @@GLOBAL.max_allowed_packet", "max_allowed_packet", true},
		{"local prefix", "SELECT @@local.autocommit", "autocommit", true},
		{"tab after select", "SELECT\t@@transaction_isolation", "transaction_isolation", true},
		{"trailing semicolon", "SELECT @@sql_mode;", "sql_mode", true},
		{"leading comment", "/* mysql-connector-java */ SELECT @@transaction_isolation", "transaction_isolation", true},
		{"lowercase select", "select @@autocommit", "autocommit", true},

		// \r must be treated as whitespace everywhere, not only after the keyword.
		// Trimming that handled "; " but not "\r" used to return the variable name
		// with the carriage return still attached, which then failed every lookup.
		{"trailing CR", "SELECT @@sql_mode\r", "sql_mode", true},
		{"trailing CRLF", "SELECT @@sql_mode\r\n", "sql_mode", true},
		{"trailing tab", "SELECT @@sql_mode\t", "sql_mode", true},
		{"semicolon then CRLF", "SELECT @@sql_mode;\r\n", "sql_mode", true},
		{"CR after select", "SELECT\r@@autocommit", "autocommit", true},

		// Space before the terminator. TrimSpace only strips whitespace at the very
		// end of the string, and there the last character is ';', so this space is
		// interior and used to trip the multi-column check — rejecting a legal
		// single-variable read. The terminator is now trimmed before that check.
		{"space before semicolon", "SELECT @@sql_mode ;", "sql_mode", true},
		{"tab before semicolon", "SELECT @@sql_mode\t;", "sql_mode", true},
		{"space before semicolon then CRLF", "SELECT @@sql_mode ;\r\n", "sql_mode", true},
		{"prefixed with space before semicolon", "SELECT @@session.transaction_isolation ;", "transaction_isolation", true},

		{"multi column", "SELECT @@a, @@b", "", false},
		{"aliased", "SELECT @@transaction_isolation AS ti", "", false},
		{"projects var in dml", "SELECT @@version, u.name FROM users u WHERE u.id = 1", "", false},
		{"with limit", "SELECT @@version_comment LIMIT 1", "", false},
		{"function call", "SELECT @@GLOBAL.foo()", "", false},
		{"not a var", "SELECT 1", "", false},
		{"not a select", "SET autocommit=1", "", false},
		{"empty", "", "", false},
		{"just select", "SELECT", "", false},
		{"var only whitespace", "SELECT @@;", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotVar, _, gotOK := parseSingleSystemVarRead(tc.query)
			assert.Equal(t, tc.wantOK, gotOK)
			if tc.wantOK {
				assert.Equal(t, tc.wantVar, gotVar)
			}
		})
	}
}

// probeMockPool builds a mock pool containing one connection-setup probe result
// set. It aliases transaction_isolation (bare column name) and leaves sql_mode
// as the raw "@@sql_mode" label to exercise both resolution forms. The
// terminator carries a distinctive status flag (0x03) so tests can prove it is
// reused rather than fabricated.
func probeMockPool() []*models.Mock {
	col := func(name string) *mysql.ColumnDefinition41 {
		return &mysql.ColumnDefinition41{
			Header:       mysql.Header{SequenceID: 2},
			Catalog:      "def",
			Name:         name,
			OrgName:      name,
			FixedLength:  0x0c,
			CharacterSet: 0x21,
			ColumnLength: 256,
			Type:         0xfd, // MYSQL_TYPE_VAR_STRING
			Filler:       []byte{0x00, 0x00},
		}
	}
	trs := &mysql.TextResultSet{
		ColumnCount: 2,
		Columns:     []*mysql.ColumnDefinition41{col("transaction_isolation"), col("@@sql_mode")},
		Rows: []*mysql.TextRow{{
			Header: mysql.Header{SequenceID: 3},
			Values: []mysql.ColumnEntry{
				{Type: mysql.FieldTypeVarString, Value: "REPEATABLE-READ"},
				{Type: mysql.FieldTypeVarString, Value: "STRICT_TRANS_TABLES"},
			},
		}},
		// OK-replacing-EOF terminator, seq 5, status flags 0x03 (distinctive).
		FinalResponse: &mysql.GenericResponse{
			Type: "OK",
			Data: []byte{0x07, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00},
		},
	}
	return []*models.Mock{{
		Kind: models.MySQL,
		Spec: models.MockSpec{
			// The probe's REQUEST, which is what marks this mock as a legitimate
			// source for a synthesized variable read. Column labels alone cannot
			// distinguish a probe from an application query that happens to
			// project a same-named column, so resolveVarFromProbe keys off this.
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{
				Message: &mysql.QueryPacket{
					Command: 0x03,
					Query:   "SELECT @@transaction_isolation AS transaction_isolation, @@sql_mode",
				},
			}}},
			MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: trs}}},
		},
	}}
}

// appQueryMockPool is a NON-probe mock whose result set carries a column named
// "version" — the conventional optimistic-locking column, and an ordinary
// application query. resolveVarFromProbe must not answer SELECT @@version from
// it: doing so would report an application row value (here 3) as the MySQL
// server version.
func appQueryMockPool() []*models.Mock {
	trs := &mysql.TextResultSet{
		ColumnCount: 1,
		Columns: []*mysql.ColumnDefinition41{{
			Header: mysql.Header{SequenceID: 2}, Catalog: "def",
			Name: "version", OrgName: "version", CharacterSet: 0x21,
			ColumnLength: 11, Type: 0x03,
		}},
		Rows: []*mysql.TextRow{{
			Header: mysql.Header{SequenceID: 3},
			Values: []mysql.ColumnEntry{{Type: mysql.FieldTypeLong, Value: "3"}},
		}},
		FinalResponse: &mysql.GenericResponse{
			Type: "OK",
			Data: []byte{0x07, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00},
		},
	}
	return []*models.Mock{{
		Kind: models.MySQL,
		Spec: models.MockSpec{
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{
				Message: &mysql.QueryPacket{
					Command: 0x03,
					Query:   "SELECT version FROM orders WHERE id = 42",
				},
			}}},
			MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: trs}}},
		},
	}}
}

func TestResolveVarFromProbe(t *testing.T) {
	pool := probeMockPool()

	t.Run("aliased column resolves", func(t *testing.T) {
		col, val, term, ok := resolveVarFromProbe(pool, "transaction_isolation")
		require.True(t, ok)
		require.NotNil(t, col)
		require.NotNil(t, term)
		assert.Equal(t, "REPEATABLE-READ", fmt.Sprintf("%v", val.Value))
	})

	t.Run("raw @@-prefixed column resolves via fallback", func(t *testing.T) {
		col, val, _, ok := resolveVarFromProbe(pool, "sql_mode")
		require.True(t, ok)
		require.NotNil(t, col)
		assert.Equal(t, "STRICT_TRANS_TABLES", fmt.Sprintf("%v", val.Value))
	})

	t.Run("missing variable not found", func(t *testing.T) {
		_, _, _, ok := resolveVarFromProbe(pool, "nonexistent_var")
		assert.False(t, ok)
	})
}

func TestTerminatorForSingleColumn(t *testing.T) {
	t.Run("reuses recorded status flags and rewrites sequence id", func(t *testing.T) {
		probe := &mysql.GenericResponse{Type: "OK", Data: []byte{0x07, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00}}
		got := terminatorForSingleColumn(probe)
		require.NotNil(t, got)
		assert.Equal(t, byte(0x04), got.Data[3], "sequence id rewritten to 4")
		assert.Equal(t, byte(0x03), got.Data[7], "status flags preserved from probe")
		// Ensure the source probe bytes were not mutated in place.
		assert.Equal(t, byte(0x05), probe.Data[3], "source probe terminator must be left untouched")
	})

	t.Run("falls back to autocommit terminator when probe unusable", func(t *testing.T) {
		got := terminatorForSingleColumn(nil)
		require.NotNil(t, got)
		assert.True(t, mysqlutils.IsOKReplacingEOF(got.Data))
		assert.Equal(t, byte(0x04), got.Data[3])
		assert.Equal(t, byte(0x02), got.Data[7], "fallback uses SERVER_STATUS_AUTOCOMMIT")
	})
}

func deprecateEOFContext() *wire.DecodeContext {
	return &wire.DecodeContext{
		ServerCaps: wire.CLIENT_DEPRECATE_EOF,
		ClientCaps: wire.CLIENT_DEPRECATE_EOF,
	}
}

// TestBuildSessionVarResponse_RoundTrip proves the synthesized single-column
// result set encodes to valid wire bytes that decode back to the recorded
// value, with the terminator framed at sequence 4 and its status flags reused
// from the probe. This guards the hand-built framing against a future change to
// the encoder or TextResultSet layout.
func TestBuildSessionVarResponse_RoundTrip(t *testing.T) {
	ctx := context.Background()
	log := zap.NewNop()
	pool := probeMockPool()

	resp := buildSessionVarResponse(log, pool, "transaction_isolation", "@@transaction_isolation", deprecateEOFContext())
	require.NotNil(t, resp)
	trs, ok := resp.PacketBundle.Message.(*mysql.TextResultSet)
	require.True(t, ok)

	enc, err := query.EncodeTextResultSet(ctx, log, trs)
	require.NoError(t, err)

	pos := 0
	count, _, n := mysqlutils.ReadLengthEncodedInteger(enc[pos:])
	require.Positive(t, n)
	require.Equal(t, uint64(1), count, "single-column result set")
	pos += n

	col, cn, err := rowscols.DecodeColumn(ctx, log, enc[pos:])
	require.NoError(t, err)
	require.NotNil(t, col)
	// The column is labelled with the EXPRESSION TEXT, matching what a real
	// server returns for "SELECT @@session.transaction_isolation". A client that
	// reads its result by label (rather than by index, as Connector/J does) would
	// not find the column under the bare variable name.
	assert.Equal(t, "@@transaction_isolation", col.Name)
	pos += cn

	row, rn, err := rowscols.DecodeTextRow(ctx, log, enc[pos:], []*mysql.ColumnDefinition41{col})
	require.NoError(t, err)
	require.Len(t, row.Values, 1)
	assert.Equal(t, "REPEATABLE-READ", fmt.Sprintf("%v", row.Values[0].Value))
	pos += rn

	term := enc[pos:]
	require.True(t, mysqlutils.IsOKReplacingEOF(term), "terminator must be a valid OK-replacing-EOF packet")
	assert.Equal(t, byte(0x04), term[3], "terminator sequence id must be 4")
	assert.Equal(t, byte(0x03), term[7], "terminator status flags must be reused from the recorded probe")
}

func TestBuildSessionVarResponse_NonDeprecateEOF_ReturnsNil(t *testing.T) {
	pool := probeMockPool()
	// No CLIENT_DEPRECATE_EOF negotiated: must decline rather than emit
	// mis-sequenced framing.
	resp := buildSessionVarResponse(zap.NewNop(), pool, "transaction_isolation", "@@transaction_isolation", &wire.DecodeContext{})
	assert.Nil(t, resp)
}

func TestBuildSessionVarResponse_UnknownVar_ReturnsNil(t *testing.T) {
	pool := probeMockPool()
	resp := buildSessionVarResponse(zap.NewNop(), pool, "nonexistent_var", "@@nonexistent_var", deprecateEOFContext())
	assert.Nil(t, resp)
}

// TestRejectsCrossVariableRead pins the blast radius of the pure-system-variable
// rejection in matchQuery.
//
// matchQuery is the SHARED MySQL replay path: proxy (MITM) and DaemonSet recordings
// run through it too, not only the proxyless capture this work targets. The
// rejection is subtractive, so it must fire ONLY when the candidate reads a
// DIFFERENT variable. Rejecting on any textual difference would make a recorded
// read of the same variable depend on buildSessionVarResponse succeeding, and that
// helper bails out when the connection did not negotiate CLIENT_DEPRECATE_EOF or
// when no recorded result set carries the column, i.e. it could fail a recording
// that passes today.
func TestRejectsCrossVariableRead(t *testing.T) {
	t.Run("different variable is rejected", func(t *testing.T) {
		// The original defect: an unrecorded @@transaction_isolation read was
		// cross-served the recorded @@transaction_read_only mock because both are
		// equal-length non-DML SELECTs.
		assert.True(t, rejectsCrossVariableRead(
			"SELECT @@session.transaction_read_only",
			"SELECT @@session.transaction_isolation"))
		assert.True(t, rejectsCrossVariableRead(
			"SELECT @@autocommit", "SELECT @@sql_mode"))
	})

	t.Run("same variable with cosmetic difference is NOT rejected", func(t *testing.T) {
		for _, c := range []struct{ expected, actual string }{
			{"SELECT @@transaction_isolation", "SELECT @@session.transaction_isolation"},
			{"SELECT @@session.transaction_isolation", "SELECT @@transaction_isolation"},
			{"SELECT @@global.transaction_isolation", "SELECT @@session.transaction_isolation"},
			{"SELECT  @@autocommit", "SELECT @@autocommit"},
			{"SELECT @@sql_mode", "SELECT @@sql_mode;"},
			{"SELECT @@sql_mode", "/* mysql-connector-java */ SELECT @@sql_mode"},
			{"SELECT @@AUTOCOMMIT", "SELECT @@autocommit"},
			{"SELECT @@sql_mode", "SELECT @@sql_mode\r\n"},
		} {
			assert.False(t, rejectsCrossVariableRead(c.expected, c.actual),
				"same variable must stay eligible: expected=%q actual=%q", c.expected, c.actual)
		}
	})

	t.Run("a var read may not be answered by an unrelated query", func(t *testing.T) {
		assert.True(t, rejectsCrossVariableRead(
			"SELECT id FROM users WHERE id = 1", "SELECT @@transaction_isolation"))
	})

	t.Run("non-var actual is never rejected here", func(t *testing.T) {
		// Ordinary DML keeps its normal fuzzy matching; this guard must not touch it.
		assert.False(t, rejectsCrossVariableRead(
			"SELECT id FROM users WHERE id = 1", "SELECT id FROM users WHERE id = 2"))
		assert.False(t, rejectsCrossVariableRead(
			"SELECT @@version, u.name FROM users u", "SELECT @@version, u.email FROM users u"))
	})

	t.Run("identical text is never rejected", func(t *testing.T) {
		assert.False(t, rejectsCrossVariableRead(
			"SELECT @@transaction_isolation", "SELECT @@transaction_isolation"))
	})
}

// TestResolveVarFromProbe_IgnoresNonProbeMocks pins the source check. Matching on
// column label alone let an ordinary application result set answer a system
// variable read: `version` is the conventional optimistic-locking column, and
// `autocommit`, `sql_mode` and `timezone` are all plausible application names. An
// unrecorded SELECT @@version would then be served an application row value as
// the MySQL server version.
func TestResolveVarFromProbe_IgnoresNonProbeMocks(t *testing.T) {
	_, _, _, ok := resolveVarFromProbe(appQueryMockPool(), "version")
	assert.False(t, ok, "an application query must never answer a system-variable read")

	// The same label IS answerable from a real probe, so the guard is about the
	// source and not about the variable name.
	pool := probeMockPool()
	_, _, _, ok = resolveVarFromProbe(pool, "transaction_isolation")
	assert.True(t, ok, "a genuine probe must still resolve")
}

// TestTerminatorForSingleColumn_SanitisesStatusFlags covers the two recorded
// flags that describe the PROBE's result set and are harmful when replayed onto
// a synthesized single-column one.
func TestTerminatorForSingleColumn_SanitisesStatusFlags(t *testing.T) {
	t.Run("SERVER_MORE_RESULTS_EXISTS is cleared", func(t *testing.T) {
		// 0x0b = AUTOCOMMIT|IN_TRANS|MORE_RESULTS. Leaving MORE_RESULTS set makes
		// the driver wait for a second result set that never arrives.
		probe := &mysql.GenericResponse{Type: "OK", Data: []byte{0x07, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x0b, 0x00, 0x00, 0x00}}
		got := terminatorForSingleColumn(probe)
		require.NotNil(t, got)
		assert.Equal(t, byte(0x03), got.Data[7], "MORE_RESULTS cleared, other flags kept")
		assert.Equal(t, byte(0x0b), probe.Data[7], "source probe must not be mutated")
	})

	t.Run("SERVER_SESSION_STATE_CHANGED falls back", func(t *testing.T) {
		// 0x4000 (d[8]=0x40) means a session-state trailer follows. That trailer
		// belongs to the probe, so replaying it would apply state changes that did
		// not happen for this query. There is no safe rewrite; use the fallback.
		probe := &mysql.GenericResponse{Type: "OK", Data: []byte{0x07, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x40, 0x00, 0x00}}
		got := terminatorForSingleColumn(probe)
		require.NotNil(t, got)
		assert.Equal(t, fallbackOKReplacingEOFTerminator, got.Data, "must use the deterministic fallback")
	})
}

// TestMatchQuery_CrossVarRejectionIsComQueryOnly pins the rejection's scope.
// matchQuery is shared with COM_STMT_PREPARE, but the compensating synthesis in
// matchCommand runs only for COM_QUERY. Rejecting on the prepare path would drop
// a candidate that used to fuzzy-match and put nothing in its place, so a
// server-side-prepared variable read (useServerPrepStmts=true) would hard-fail.
func TestMatchQuery_CrossVarRejectionIsComQueryOnly(t *testing.T) {
	ctx := context.Background()
	log := zap.NewNop()

	mk := func(q string) mysql.PacketBundle {
		return mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{}},
			Message: &mysql.QueryPacket{Command: 0x03, Query: q},
		}
	}
	getQuery := func(p mysql.PacketBundle) string {
		msg, _ := p.Message.(*mysql.QueryPacket)
		return msg.Query
	}
	expected := mk("SELECT @@session.transaction_read_only")
	actual := mk("SELECT @@session.transaction_isolation")

	okQuery, scoreQuery := matchQuery(ctx, log, expected, actual, getQuery, true)
	assert.False(t, okQuery)
	assert.Equal(t, 0, scoreQuery, "COM_QUERY: a different @@var must score 0")

	_, scorePrepare := matchQuery(ctx, log, expected, actual, getQuery, false)
	assert.NotEqual(t, 0, scorePrepare, "COM_STMT_PREPARE: matching must be left as it was")
}

// TestParseSingleSystemVarRead_Label pins the expression text returned alongside
// the normalised variable name. The name drives the probe lookup; the label is
// what the synthesized column is called, and it must match what a real server
// would return for that exact query — prefix and all.
func TestParseSingleSystemVarRead_Label(t *testing.T) {
	tests := []struct {
		query     string
		wantVar   string
		wantLabel string
	}{
		{"SELECT @@transaction_isolation", "transaction_isolation", "@@transaction_isolation"},
		{"SELECT @@session.transaction_isolation", "transaction_isolation", "@@session.transaction_isolation"},
		{"SELECT @@GLOBAL.max_allowed_packet", "max_allowed_packet", "@@GLOBAL.max_allowed_packet"},
		{"SELECT @@sql_mode;", "sql_mode", "@@sql_mode"},
		{"SELECT @@sql_mode ;", "sql_mode", "@@sql_mode"},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			gotVar, gotLabel, ok := parseSingleSystemVarRead(tc.query)
			require.True(t, ok)
			assert.Equal(t, tc.wantVar, gotVar, "normalised name drives the probe lookup")
			assert.Equal(t, tc.wantLabel, gotLabel, "label must be the expression as written")
		})
	}
}

// TestBuildSessionVarResponse_LabelFallback covers the caller that has no
// expression to give: the column keeps the bare variable name rather than being
// left unlabelled.
func TestBuildSessionVarResponse_LabelFallback(t *testing.T) {
	resp := buildSessionVarResponse(zap.NewNop(), probeMockPool(), "transaction_isolation", "", deprecateEOFContext())
	require.NotNil(t, resp)
	trs, ok := resp.PacketBundle.Message.(*mysql.TextResultSet)
	require.True(t, ok)
	require.Len(t, trs.Columns, 1)
	assert.Equal(t, "transaction_isolation", trs.Columns[0].Name)
}
