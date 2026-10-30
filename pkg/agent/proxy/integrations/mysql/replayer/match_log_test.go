package replayer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"vitess.io/vitess/go/vt/sqlparser"
)

// The debug log of an EXECUTE the matcher compares masks every value it
// carries: an attribute named as a secret (a password) holds one whatever it
// looks like, and was logged in the clear, and the statement's own values are
// the app's data (k1 here), logged in the clear too. Its attributes' names
// and its parameters' types are logged as they are.
func TestMatchCommand_TheDebugLogMasksAnExecutesValues(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	at := stmtT0.Add(time.Second)
	db := &fakeMockDb{session: []*models.Mock{executedMock("mock-1", 1, at, "row", ordersInsert, "k1", "t1")},
		winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
	req := liveExecute("k1", "t1")
	p := req.Message.(*mysql.StmtExecutePacket)
	p.Parameters = append(p.Parameters, mysql.Parameter{Type: uint16(mysql.FieldTypeString), Name: "password", Value: secret})
	p.ParameterCount = len(p.Parameters)
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	if _, _, _, err := matchCommand(ctx, zap.New(core), req, db, dctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	entries := logs.FilterMessage("Trying to match the mock with com-stmt-execute request").All()
	if len(entries) == 0 {
		t.Fatal("precondition: the matcher logged no EXECUTE it compared")
	}
	for _, e := range entries {
		logged := fmt.Sprintf("%+v", e.ContextMap()["Req"])
		if strings.Contains(logged, secret) {
			t.Fatalf("the debug log holds the attribute's value: %s", logged)
		}
		if strings.Contains(logged, "k1") || strings.Contains(logged, "t1") {
			t.Fatalf("the debug log holds the statement's own values: %s", logged)
		}
		if !strings.Contains(logged, "password") || !strings.Contains(logged, maskedAttributeValue) {
			t.Fatalf("the debug log does not show the EXECUTE by its names and types, its values masked: %s", logged)
		}
	}
	if p.Parameters[2].Value != secret {
		t.Fatalf("logging the EXECUTE changed it: %+v", p.Parameters[2])
	}
}

// The EXECUTE the debug log writes (executeForLog) masks the values of its
// legacy parameters too: zap's JSON leaves LegacyParameters out (json:"-"),
// but an observed entry printed with %+v, and a logger's other encoders, do
// not. The logged packet is a copy: the live one keeps its values.
func TestExecuteForLog_MasksTheLegacyParametersToo(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	p := &mysql.StmtExecutePacket{
		Parameters:       []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Name: "password", Value: secret}},
		LegacyParameters: []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Name: "password", Value: secret}},
	}
	logged := fmt.Sprintf("%+v", *executeForLog(p))
	if strings.Contains(logged, secret) {
		t.Fatalf("the logged EXECUTE holds a value: %s", logged)
	}
	if strings.Count(logged, maskedAttributeValue) != 2 {
		t.Fatalf("the logged EXECUTE does not mask both its parameters and its legacy parameters: %s", logged)
	}
	if p.Parameters[0].Value != secret || p.LegacyParameters[0].Value != secret {
		t.Fatalf("masking for the log changed the live EXECUTE: %+v", p)
	}
}

// The matcher's debug log writes a statement by its shape, its literals "?":
// a literal is the app's data (an e-mail address, a token in a WHERE). No
// stretch of the recorded or the live literals appears in the log in any
// encoding, on an exact match and on a match with drifted literals. It
// logged both statements whole.
func TestMatchCommand_TheDebugLogHoldsNoSQLLiteral(t *testing.T) {
	const email, recTok, liveTok = "grace.vault@example.net", "sk_rec_Mm3Pq8Zt", "sk_live_Q7wZr9Kf"
	stmt := func(tok string) string {
		return "SELECT id, name FROM users WHERE email = '" + email + "' AND token = '" + tok + "'"
	}
	for _, c := range []struct {
		name, recorded, live, line string
	}{
		{"an exact match", stmt(liveTok), stmt(liveTok), "Query Exact matched"},
		{"a match with drifted literals", stmt(recTok), stmt(liveTok), "query matched with drifted inline literals"},
	} {
		t.Run(c.name, func(t *testing.T) {
			obs, logs := observer.New(zapcore.DebugLevel)
			logger, out := logleak.Logger(obs)
			db := &fakeMockDb{session: []*models.Mock{readbackMock("mock-1", c.recorded, "row=1", stmtT0)}}
			resp, ok, _, err := matchCommand(context.Background(), logger, comQueryReq(c.live), db, newDecodeCtx(), nil, nil)
			if err != nil || !ok || resp == nil || resp.Payload != "row=1" {
				t.Fatalf("want mock-1 served, got ok=%v err=%v resp=%v", ok, err, resp)
			}
			lines := logs.FilterMessage(c.line).All()
			if len(lines) == 0 {
				t.Fatalf("the line %q was not logged: the path that logged SQL text did not run; got %v", c.line, logs.All())
			}
			want := "SELECT id, name FROM users WHERE email = ? AND token = ?"
			if f := lines[0].ContextMap(); f["expected query"] != want || f["actual query"] != want {
				t.Errorf("the line writes both statements by their shape %q; got %v", want, f)
			}
			log := out.String()
			if !strings.Contains(log, c.line) {
				t.Fatalf("the rendered log is not the one checked: %s", log)
			}
			for what, content := range map[string]string{"the e-mail": email, "the recorded token": recTok, "the live token": liveTok} {
				if found := logleak.Find(t, log, []byte(content)); len(found) > 0 {
					t.Errorf("the log holds %s: %v", what, found)
				}
			}
		})
	}
}

// A miss's debug line writes the live statement, and the closest recorded
// one, by its shape (models.SQLShape, cut at 160 bytes): no stretch of either's literals appears
// in the log in any encoding. It logged both statements cut, literals and all.
func TestMatchCommand_AMissLogsNoSQLLiteral(t *testing.T) {
	const email, recTok, liveTok = "grace.vault@example.net", "sk_rec_Mm3Pq8Zt", "sk_live_Q7wZr9Kf"
	obs, logs := observer.New(zapcore.DebugLevel)
	logger, out := logleak.Logger(obs)
	recorded := "SELECT id FROM orders WHERE email = '" + email + "' AND token = '" + recTok + "'"
	live := "DELETE FROM sessions WHERE email = '" + email + "' AND token = '" + liveTok + "'"
	db := &fakeMockDb{session: []*models.Mock{readbackMock("mock-1", recorded, "row=1", stmtT0)}}
	if _, ok, _, _ := matchCommand(context.Background(), logger, comQueryReq(live), db, newDecodeCtx(), nil, nil); ok {
		t.Fatal("a DELETE was answered by a recorded SELECT; want a miss")
	}
	lines := logs.FilterMessage("mock miss").All()
	if len(lines) == 0 {
		t.Fatalf("the line %q was not logged: the path that logged SQL text did not run; got %v", "mock miss", logs.All())
	}
	if got := lines[0].ContextMap()["actual_query"]; got != "DELETE FROM sessions WHERE email = ? AND token = ?" {
		t.Errorf("actual_query = %q, want the live statement's shape", got)
	}
	log := out.String()
	if !strings.Contains(log, "mock miss") {
		t.Fatalf("the rendered log is not the one checked: %s", log)
	}
	for what, content := range map[string]string{"the e-mail": email, "the recorded token": recTok, "the live token": liveTok} {
		if found := logleak.Find(t, log, []byte(content)); len(found) > 0 {
			t.Errorf("the log holds %s: %v", what, found)
		}
	}
}

// The mismatch summary of an EXECUTE masks its query attributes' values: it
// is saved in the test report, which keeps a statement's own values (the
// WARN line of a miss logs the call's shape only, models.CallShape).
func TestFormatExecParams_MasksAttributeValues(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	got := formatExecParams([]mysql.Parameter{
		{Type: 253, Value: "k1"},
		{Type: uint16(mysql.FieldTypeString), Name: "password", Value: secret},
		{Type: uint16(mysql.FieldTypeString), Name: "unset"},
	})
	if strings.Contains(got, secret) || !strings.Contains(got, "k1") || !strings.Contains(got, maskedAttributeValue) {
		t.Fatalf("the summary is %q: want the statement's own value, and the attribute's masked", got)
	}
}

// A strict miss's field diffs mask the recorded and live values of a query
// attribute, of an EXECUTE and of a COM_QUERY; a statement's own value's
// drift is shown as it is.
func TestMatchCommand_AStrictMissMasksAttributeValues(t *testing.T) {
	const secret, old = "correct-horse-battery-staple", "old-password"
	password := func(v string) mysql.Parameter {
		return mysql.Parameter{Type: uint16(mysql.FieldTypeString), Name: "password", Value: v}
	}
	check := func(t *testing.T, miss *mockMiss, ownPath string) {
		t.Helper()
		if miss == nil {
			t.Fatal("no miss")
		}
		var masked, own bool
		for _, d := range miss.fieldDiffs {
			// An enveloped value ($bin) is held base64-encoded.
			for _, v := range []string{secret, old, base64.StdEncoding.EncodeToString([]byte(secret)), base64.StdEncoding.EncodeToString([]byte(old))} {
				if strings.Contains(d.Expected, v) || strings.Contains(d.Actual, v) {
					t.Fatalf("a field diff holds the attribute's value: %+v", miss.fieldDiffs)
				}
			}
			if (d.Kind == models.DiffKindMissingInLive && d.Actual != "") || (d.Kind == models.DiffKindMissingInMock && d.Expected != "") {
				t.Fatalf("a field diff fills the side its field is absent from: %+v", miss.fieldDiffs)
			}
			if strings.Contains(d.Path, ".value") && (d.Expected == maskedAttributeValue || d.Actual == maskedAttributeValue) {
				masked = true
			}
			if d.Path == ownPath && d.Expected == "k0" && d.Actual == "k1" {
				own = true
			}
		}
		if !masked || (ownPath != "" && !own) {
			t.Fatalf("the diffs %+v: want the attribute's masked and the statement's own shown", miss.fieldDiffs)
		}
	}
	t.Run("an EXECUTE", func(t *testing.T) {
		// A statement that reads its attribute: one that does not leaves its
		// attributes out of what strict compares.
		const readsExec = "SELECT ? AS k, ? AS t, mysql_query_attribute_string('password') AS v"
		at := stmtT0.Add(time.Second)
		m := executedMock("mock-1", 1, at, "row", readsExec, "k0", "t1")
		p := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
		p.Parameters = append(p.Parameters, password(old))
		p.ParameterCount = len(p.Parameters)
		db := &fakeMockDb{session: []*models.Mock{m}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
		req := liveExecute("k1", "t1")
		lp := req.Message.(*mysql.StmtExecutePacket)
		lp.Parameters = append(lp.Parameters, password(secret))
		lp.ParameterCount = len(lp.Parameters)
		dctx := newDecodeCtx()
		dctx.StmtIDToQuery[33] = readsExec
		ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
		_, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
		if err != nil || ok {
			t.Fatalf("strict served the EXECUTE (%v)", err)
		}
		check(t, miss, "body.parameters.0.value")
	})
	// A statement that reads the attribute: one that does not leaves its
	// attributes out of what strict compares.
	const reads = "SELECT mysql_query_attribute_string('password') AS v"
	t.Run("a COM_QUERY", func(t *testing.T) {
		const sql = reads
		m := readbackMock("q1", sql, "row", zeroTime())
		m.Spec.MySQLRequests[0].PacketBundle.Message.(*mysql.QueryPacket).Parameters = []mysql.Parameter{password(old)}
		req := comQueryReq(sql)
		req.PacketBundle.Message.(*mysql.QueryPacket).Parameters = []mysql.Parameter{password(secret)}
		db := &fakeMockDb{session: []*models.Mock{m}}
		_, ok, miss, err := matchCommand(context.Background(), zap.NewNop(), req, db, newDecodeCtx(), schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
		if err != nil || ok {
			t.Fatalf("strict served the COM_QUERY (%v)", err)
		}
		check(t, miss, "")
	})
	// Either side's name masks the position, and the side a field is absent
	// from stays empty: a recorded parameter sent without a name, an
	// attribute the call no longer sends, one it sends anew beside one that
	// drifted, and a value in an envelope.
	enveloped := func(v string) mysql.Parameter {
		return mysql.Parameter{Type: uint16(mysql.FieldTypeString), Name: "password", Value: []byte(v)}
	}
	for name, sides := range map[string][2][]mysql.Parameter{
		"a recording without the names": {{{Type: uint16(mysql.FieldTypeString), Value: old}}, {password(secret)}},
		"an attribute no longer sent":   {{password(old)}, nil},
		"an attribute sent anew":        {{password(old)}, {password(secret), {Type: uint16(mysql.FieldTypeString), Name: "token", Value: secret}}},
		"a value in an envelope":        {{enveloped(old)}, {enveloped(secret)}},
	} {
		t.Run(name, func(t *testing.T) {
			const sql = reads
			m := readbackMock("q1", sql, "row", zeroTime())
			m.Spec.MySQLRequests[0].PacketBundle.Message.(*mysql.QueryPacket).Parameters = sides[0]
			req := comQueryReq(sql)
			req.PacketBundle.Message.(*mysql.QueryPacket).Parameters = sides[1]
			db := &fakeMockDb{session: []*models.Mock{m}}
			_, ok, miss, err := matchCommand(context.Background(), zap.NewNop(), req, db, newDecodeCtx(), schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
			if err != nil || ok {
				t.Fatalf("strict served the COM_QUERY (%v)", err)
			}
			check(t, miss, "")
		})
	}
}

// A statement the parser cannot read fails by where the parser stopped, never
// by the parser's text: vitess quotes the token it stopped near, and that
// token is the statement's own (a literal the application sent). The
// matcher logs the error beside the statement's shape. It quoted the token.
func TestGetQueryStructure_AParseFailureNamesNoLiteral(t *testing.T) {
	const email, tok = "grace.vault@example.net", "sk_live_Q7wZr9Kf"
	parser, err := sqlparser.New(sqlparser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"SELECT id FROM users WHERE email '" + email + "'",
		"SELECT id FROM users WHERE email = '" + email + "' " + tok,
		"INSERT INTO users (email) VALUES ('" + email + "') '" + tok + "'",
	} {
		if _, raw := parser.Parse(sql); raw == nil || !strings.Contains(raw.Error(), "near '") {
			t.Fatalf("%q: the parser's own error quotes no token (%v), so the row checks nothing", sql, raw)
		}
		_, err := getQueryStructure(sql)
		if err == nil {
			t.Fatalf("%q parsed: the path that failed did not run", sql)
		}
		for what, v := range map[string]string{"the e-mail": email, "the token": tok} {
			if found := logleak.Find(t, err.Error(), []byte(v)); len(found) > 0 {
				t.Errorf("the parse error holds %s: %v in %q", what, found, err)
			}
		}
		if !strings.HasPrefix(err.Error(), "failed to parse SQL: the parser stopped at byte ") || !strings.HasSuffix(err.Error(), " (INVALID_ARGUMENT)") {
			t.Errorf("the parse error does not say where the parser stopped and its code: %q", err)
		}
	}
	// The parser's own errors that quote nothing of the statement are kept,
	// wrapped: a caller can still tell an empty statement from two.
	for sql, want := range map[string]error{"": sqlparser.ErrEmpty, "SELECT 1; SELECT 2": sqlparser.ErrMultipleStatements} {
		if _, err := getQueryStructure(sql); !errors.Is(err, want) {
			t.Errorf("%q: the parse error %v does not wrap %v", sql, err, want)
		}
	}
}

// The single-column session-variable answer the replayer builds from a
// recorded probe logs the value's type, never the value: it is the
// recording's (a sql_mode, an isolation level, or a variable the
// application set). It logged the value.
func TestBuildSessionVarResponse_LogsNoValue(t *testing.T) {
	const planted = "planted-REPEAT-Q7wZr9Kf"
	pool := probeMockPool()
	trs := pool[0].Spec.MySQLResponses[0].PacketBundle.Message.(*mysql.TextResultSet)
	trs.Rows[0].Values[0].Value = planted
	obs, logs := observer.New(zapcore.DebugLevel)
	logger, out := logleak.Logger(obs)
	resp := buildSessionVarResponse(logger, pool, "transaction_isolation", deprecateEOFContext())
	if resp == nil {
		t.Fatal("no answer was built: the path that logged the value did not run")
	}
	lines := logs.FilterMessage("built single-column session-variable result set").All()
	if len(lines) != 1 {
		t.Fatalf("the line was not logged once: %v", logs.All())
	}
	if f := lines[0].ContextMap(); f["value type"] != "string" {
		t.Errorf("the line names the value's type %q; got %v", "string", f)
	}
	if found := logleak.Find(t, out.String(), []byte(planted)); len(found) > 0 {
		t.Errorf("the log holds the recorded value: %v", found)
	}
}

// The debug line that lists the recorded prepared statements writes each by
// its shape, its literals "?": a statement prepared with a literal (an e-mail
// address in a WHERE) is the app's data. It logged every statement whole.
func TestMatchCommand_TheRecordedPrepEntriesLineHoldsNoSQLLiteral(t *testing.T) {
	const email = "grace.vault@example.net"
	query := "SELECT id FROM users WHERE email = '" + email + "' AND id = ?"
	at := stmtT0.Add(time.Second)
	db := &fakeMockDb{session: []*models.Mock{preparedMock("prep-1", "0", 33, query, stmtT0), executedMock("mock-1", 33, at, "row", query, "k1")},
		winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = query
	obs, logs := observer.New(zapcore.DebugLevel)
	logger, out := logleak.Logger(obs)
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	if _, _, _, err := matchCommand(ctx, logger, liveExecute("k1"), db, dctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	lines := logs.FilterMessage("recorded prepEntries").All()
	if len(lines) == 0 {
		t.Fatalf("the line was not logged: the path that listed the statements did not run; got %v", logs.All())
	}
	if got, want := fmt.Sprint(lines[0].ContextMap()["entries"]), `query="SELECT id FROM users WHERE email = ? AND id = ?"`; !strings.Contains(got, want) {
		t.Errorf("the line does not write the statement by its shape %s: %s", want, got)
	}
	log := out.String()
	if !strings.Contains(log, "recorded prepEntries") {
		t.Fatalf("the rendered log is not the one checked: %s", log)
	}
	if found := logleak.Find(t, log, []byte(email)); len(found) > 0 {
		t.Errorf("the log holds the statement's literal: %v", found)
	}
}

// The schema-noise lines name a drifted field by its shape (models.FieldShapes):
// a JSON key in a SEND_LONG_DATA chunk is the application's (an account ID
// here) and is written {}, and no stretch of the key or of either side's
// value appears in the log in any encoding; so is a query attribute's name
// that may be a value. A command's own places (an
// EXECUTE's parameters.0.value, a statement's query.literals.0) are positions
// keploy counts: they are kept whole, so the line names the field as the
// user marks it as noise. Both the learn (detection) and the strict
// rejection logged the paths raw. (A chunk's drift is learned on a line of
// its own that names no field.)
func TestMatchCommand_TheSchemaNoiseLinesNameDriftedFieldsByTheirShape(t *testing.T) {
	const acct, acctAttr, recTok, liveTok = "acct_7Hq2Lz9Xw4Pm", "acct_9wq4lz7xh2km", "sk_rec_Mm3Pq8Zt", "sk_live_Q7wZr9Kf"
	chunk := func(tok string) []byte { return []byte(`{"` + acct + `":"` + tok + `","pad":"x"}`) }
	tokenQuery := func(tok string) string { return "SELECT id FROM sessions WHERE token = '" + tok + "'" }
	// A query attribute the statement reads, by a name that may be a value
	// (the app's account ID): the body keys it by that name.
	const readsAcct = "SELECT mysql_query_attribute_string('" + acctAttr + "') AS v"
	attr := func(tok string) []mysql.Parameter {
		return []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Name: acctAttr, Value: tok}}
	}
	attrMock := func(tok string) *models.Mock {
		m := readbackMock("q1", readsAcct, "row", zeroTime())
		m.Spec.MySQLRequests[0].PacketBundle.Message.(*mysql.QueryPacket).Parameters = attr(tok)
		return m
	}
	attrReq := func(tok string) mysql.Request {
		req := comQueryReq(readsAcct)
		req.PacketBundle.Message.(*mysql.QueryPacket).Parameters = attr(tok)
		return req
	}
	const learned = "schema-noise detection: learned request-body drift on matched mock"
	const rejected = "schema-noise strict: rejected candidate mock (non-noise request field drifted)"
	for _, c := range []struct {
		name, line, field string
		detect, strict    bool
		mock              *models.Mock
		req               mysql.Request
		want              []string
	}{
		{"a learned literal", learned, "fields", true, false, readbackMock("q1", tokenQuery(recTok), "row", zeroTime()), comQueryReq(tokenQuery(liveTok)), []string{"body.query.literals.0"}},
		{"a learned attribute", learned, "fields", true, false, attrMock(recTok), attrReq(liveTok), []string{"body.attributes.{}.value"}},
		{"a rejected chunk key", rejected, "drifted_fields", false, true, sldMockOf("sld1", chunk(recTok)), sldReq(chunk(liveTok)), []string{"body.data.{}"}},
		{"a learned parameter", learned, "fields", true, false, execMock("m1", recTok), mysql.Request{PacketBundle: execBundle(liveTok)}, []string{"body.parameters.0.value"}},
		{"a rejected parameter", rejected, "drifted_fields", false, true, execMock("m1", recTok), mysql.Request{PacketBundle: execBundle(liveTok)}, []string{"body.parameters.0.value"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			obs, logs := observer.New(zapcore.DebugLevel)
			logger, out := logleak.Logger(obs)
			db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{c.mock}}}
			_, _, _, _ = matchCommand(context.Background(), logger, c.req, db, newDecodeCtx(), schemanoise.New(mysqlNoiseAdapter{}, c.detect, c.strict), nil)
			lines := logs.FilterMessage(c.line).All()
			if len(lines) == 0 {
				t.Fatalf("the line %q was not logged: the path that logged the drifted fields did not run; got %v", c.line, logs.All())
			}
			got := fmt.Sprint(lines[0].ContextMap()[c.field])
			if want := fmt.Sprint(c.want); got != want {
				t.Errorf("%s = %s, want %s", c.field, got, want)
			}
			log := out.String()
			if !strings.Contains(log, c.line) {
				t.Fatalf("the rendered log is not the one checked: %s", log)
			}
			for what, content := range map[string]string{"the chunk's key": acct, "the attribute's name": acctAttr, "the recorded value": recTok, "the live value": liveTok} {
				if found := logleak.Find(t, log, []byte(content)); len(found) > 0 {
					t.Errorf("the log holds %s: %v", what, found)
				}
			}
		})
	}
}
