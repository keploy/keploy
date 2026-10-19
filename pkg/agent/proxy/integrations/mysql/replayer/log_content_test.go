package replayer

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is a value of the application's, in a bind value, a statement's
// text and a row.
const canary = "kp-canary-mysql-replay-3b8e"

// The matcher's debug lines of an EXECUTE name the statement and its
// parameters' count, not its bind values or its text, unless
// KEPLOY_LOG_CONTENT is set.
func TestMatchCommand_AnExecutesLinesCarryNoBindValueOrStatement(t *testing.T) {
	const stmt = "INSERT INTO notes (body, at, tag) VALUES (?, ?, '" + canary + "')"
	logtest.Each(t, func(t *testing.T, on bool) {
		at := stmtT0.Add(time.Second)
		// A recording prepared on its connection, and one whose PREPARE was
		// not kept: their EXECUTEs are compared by statement, and by
		// parameters alone.
		prepared := preparedMock("mock-prepare", "0", 1, stmt, at)
		withPrepare := executedMock("mock-exec-1", 1, at, "row", "", canary+"-other", "t1")
		alone := executedMock("mock-exec-2", 2, at, "row", "", canary, "t1")
		db := &fakeMockDb{session: []*models.Mock{prepared, withPrepare, alone},
			winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
		dctx := newDecodeCtx()
		dctx.StmtIDToQuery[33] = stmt
		ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
		logger, logs := logtest.Observe()
		if _, _, _, err := matchCommand(ctx, logger, liveExecute(canary, "t1"), db, dctx, nil, nil); err != nil {
			t.Fatal(err)
		}
		logtest.Check(t, logs, canary, on,
			"recorded prepEntries",
			"Trying to match the mock with com-stmt-execute request",
			"queries comparison",
			"EXECUTE matched on params alone (mock has no recorded PREPARE)")
	})
}

// A COM_QUERY's text is compared with each recording's, and the lines that
// say how carry neither, unless KEPLOY_LOG_CONTENT is set.
func TestMatchCommand_AQuerysComparisonLinesCarryNoStatement(t *testing.T) {
	selectOf := func(v string) string { return "SELECT body FROM notes WHERE tag = '" + v + "'" }
	insertOf := func(v string) string { return "INSERT INTO notes (body) VALUES ('" + v + "')" }
	for name, tc := range map[string]struct {
		live     string
		recorded []string
		want     []string
	}{
		"the same statement": {selectOf(canary), []string{selectOf(canary)}, []string{"Query Exact matched"}},
		"a write recorded for a read": {selectOf(canary), []string{insertOf(canary)},
			[]string{"expected query is dml but actual is not", "mock miss"}},
		"a read recorded for a write": {insertOf(canary), []string{selectOf(canary)},
			[]string{"actual query is dml but expected is not", "mock miss"}},
		"another literal": {selectOf(canary + "-live"), []string{selectOf(canary + "-recorded")},
			[]string{"query matched with drifted inline literals"}},
		"another read": {selectOf(canary), []string{"SELECT tag FROM notes WHERE body = '" + canary + "' LIMIT 1"},
			[]string{"No Query is dml", "mock miss"}},
		"a write that does not parse": {insertOf(canary) + " ,,", []string{insertOf(canary)},
			[]string{"failed to get actual query structure"}},
		"a recorded write that does not parse": {insertOf(canary), []string{insertOf(canary) + " ,,"},
			[]string{"failed to get expected query structure"}},
		// The parser's error quotes the token it stopped at: here, the
		// literal.
		"a write that does not parse at a literal": {"INSERT INTO notes (body) VALUES (5 '" + canary + "')", []string{insertOf("x")},
			[]string{"failed to get actual query structure"}},
		"a recorded write that does not parse at a literal": {insertOf("x"), []string{"INSERT INTO notes (body) VALUES (5 '" + canary + "')"},
			[]string{"failed to get expected query structure"}},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				db := &fakeMockDb{}
				for i, q := range tc.recorded {
					db.session = append(db.session, readbackMock("mock-"+string(rune('a'+i)), q, "row", zeroTime()))
				}
				logger, logs := logtest.Observe()
				if _, _, _, err := matchCommand(context.Background(), logger, comQueryReq(tc.live), db, newDecodeCtx(), nil, nil); err != nil {
					t.Fatal(err)
				}
				logtest.Check(t, logs, canary, on, tc.want...)
			})
		})
	}
}

// A query strict matching refuses the closest recording of is a miss, and
// the line that names that recording carries neither statement, unless
// KEPLOY_LOG_CONTENT is set.
func TestMatchCommand_AStrictMissLineCarriesNoStatement(t *testing.T) {
	selectOf := func(v string) string { return "SELECT body FROM notes WHERE tag = '" + v + "'" }
	logtest.Each(t, func(t *testing.T, on bool) {
		db := &fakeMockDb{session: []*models.Mock{readbackMock("mock-a", selectOf(canary+"-recorded"), "row", zeroTime())}}
		logger, logs := logtest.Observe()
		_, ok, _, err := matchCommand(context.Background(), logger, comQueryReq(selectOf(canary+"-live")), db, newDecodeCtx(),
			schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
		if err != nil || ok {
			t.Fatalf("strict served the query (%v)", err)
		}
		logtest.Check(t, logs, canary, on, "mock miss")
	})
}

// A control statement no recording answers is answered with an OK, and the
// line that says so does not carry it.
func TestMatchCommand_TheSyntheticOKLineCarriesNoStatement(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		db := &fakeMockDb{session: []*models.Mock{readbackMock("unrelated", "SELECT 1", "x", zeroTime())}}
		logger, logs := logtest.Observe()
		if _, _, _, err := matchCommand(context.Background(), logger, comQueryReq("SET @note = '"+canary+"'"), db, newDecodeCtx(), nil, nil); err != nil {
			t.Fatal(err)
		}
		logtest.Check(t, logs, canary, on, "Returning synthetic OK for unmocked control/DDL")
	})
}

// A PREPARE answered from its recording, and one no recording holds, are
// logged without their statement.
func TestMatchCommand_APreparesLinesCarryNoStatement(t *testing.T) {
	const stmt = "SELECT body FROM notes WHERE tag = ? /* " + canary + " */"
	t.Run("recorded", func(t *testing.T) {
		logtest.Each(t, func(t *testing.T, on bool) {
			db := &fakeMockDb{session: []*models.Mock{preparedMock("mock-prepare", "0", 7, stmt, zeroTime())}}
			ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
			logger, logs := logtest.Observe()
			if _, ok, _, err := matchCommand(ctx, logger, stmtPrepareReq(stmt), db, newDecodeCtx(), nil, nil); err != nil || !ok {
				t.Fatalf("the PREPARE was not answered (%v)", err)
			}
			logtest.Check(t, logs, canary, on, "Recorded runtime PREP mapping with new statement ID")
		})
	})
	t.Run("not recorded", func(t *testing.T) {
		logtest.Each(t, func(t *testing.T, on bool) {
			db := &fakeMockDb{session: []*models.Mock{readbackMock("unrelated", "SELECT 1", "x", zeroTime())}}
			logger, logs := logtest.Observe()
			if _, ok, _, err := matchCommand(context.Background(), logger, stmtPrepareReq(stmt), db, newDecodeCtx(), nil, nil); err != nil || !ok {
				t.Fatalf("no PREPARE_OK was made up (%v)", err)
			}
			logtest.Check(t, logs, canary, on, "Synthesized PREPARE_OK for unmocked statement (likely TiDB+JDBC cachePrepStmts caching pre-record stmtIDs)")
		})
	})
}

// A command the matcher answers costs a Debug level that is off nothing for
// the switch: the lines it logs for every command check their level before
// they build a logcontent field. Each case runs a line the canary tests above
// hold: the synthetic OK's and the recorded PREPARE's.
func TestMatchCommand_TheSwitchCostsADisabledLevelNothing(t *testing.T) {
	logger := logtest.Disabled()
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	const stmt = "SELECT body FROM notes WHERE tag = ?"
	for name, tc := range map[string]struct {
		req   func() mysql.Request
		mocks func() []*models.Mock
	}{
		"a synthetic OK": {func() mysql.Request { return comQueryReq("SET @note = 'x'") },
			func() []*models.Mock { return []*models.Mock{readbackMock("unrelated", "SELECT 1", "x", zeroTime())} }},
		"a recorded PREPARE": {func() mysql.Request { return stmtPrepareReq(stmt) },
			func() []*models.Mock { return []*models.Mock{preparedMock("mock-prepare", "0", 7, stmt, zeroTime())} }},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.CheckDisabledCost(t, func() {
				db := &fakeMockDb{session: tc.mocks()}
				if _, ok, _, err := matchCommand(ctx, logger, tc.req(), db, newDecodeCtx(), nil, nil); err != nil || !ok {
					t.Fatalf("the command was not answered (%v)", err)
				}
			})
		})
	}
}

// noteRows is a recorded answer of one row that holds the canary; header nil
// is a recording whose packet has none, which cannot be encoded.
func noteRows(name, sql string, header bool) *models.Mock {
	m := readbackMock(name, sql, "", zeroTime())
	col := &mysql.ColumnDefinition41{Header: mysql.Header{SequenceID: 2}, Catalog: "def", Name: "body", OrgName: "body",
		FixedLength: 0x0c, CharacterSet: 0x21, ColumnLength: 256, Type: 0xfd, Filler: []byte{0, 0}}
	m.Spec.MySQLResponses[0].Message = &mysql.TextResultSet{
		ColumnCount: 1,
		Columns:     []*mysql.ColumnDefinition41{col},
		Rows: []*mysql.TextRow{{Header: mysql.Header{SequenceID: 3},
			Values: []mysql.ColumnEntry{{Type: mysql.FieldTypeVarString, Value: canary}}}},
		FinalResponse: &mysql.GenericResponse{Type: "OK", Data: []byte{0x07, 0x00, 0x00, 0x04, 0xfe, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}},
	}
	m.Spec.MySQLResponses[0].Header = &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}, Type: "TextResultSet"}
	if !header {
		m.Spec.MySQLResponses[0].Header = nil
	}
	return m
}

// The command phase's lines name what was served and sent, not the rows
// served, the bytes a command sent, or the recording it could not encode,
// unless KEPLOY_LOG_CONTENT is set.
func TestReplay_TheCommandPhasesLinesCarryNoRowOrCommand(t *testing.T) {
	const served, broken = "SELECT body FROM notes WHERE id = 1", "SELECT body FROM notes WHERE id = 2"
	logtest.Each(t, func(t *testing.T, on bool) {
		db := &fakeMockDb{session: []*models.Mock{nativeHandshake(), noteRows("mock-rows", served, true), noteRows("mock-broken", broken, false)}}
		logger, logs := logtest.Observe()
		client, agentEnd := net.Pipe()
		t.Cleanup(func() { _ = client.Close(); _ = agentEnd.Close() })
		done := make(chan error, 1)
		go func() {
			done <- Replay(context.Background(), logger, agentEnd, &models.ConditionalDstCfg{Addr: testDBAddr}, db, models.OutgoingOptions{})
			_ = agentEnd.Close()
		}()
		c := mysqlClient{t: t, c: client}
		c.greetAndAnswer("app", mysql.Native)
		if ok := c.read(); len(ok) == 0 || ok[0] != mysql.OK {
			t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
		}
		c.send(0, append([]byte{mysql.COM_QUERY}, served...))
		if n := c.read(); len(n) != 1 || n[0] != 1 {
			t.Fatalf("the query was answered with %x, want a result set of one column", n)
		}
		for {
			if p := c.read(); len(p) > 0 && p[0] == 0xfe {
				break
			}
		}
		longData := binary.LittleEndian.AppendUint32([]byte{mysql.COM_STMT_SEND_LONG_DATA}, 1)
		c.send(0, append(append(longData, 0, 0), canary...))
		c.send(0, append([]byte{mysql.COM_QUERY}, broken...))
		if err := <-done; err == nil {
			t.Fatal("a recording that cannot be encoded was served")
		}
		logtest.Check(t, logs, canary, on,
			"Matched the command with the mock",
			"Encoded Packet",
			"No response for the command",
			"failed to encode the response")
	})
}

// A command the replay serves, and one it sends no answer to, cost a Debug
// level that is off nothing for the switch: the command phase's lines check
// their level before they build a logcontent field.
func TestReplay_TheSwitchCostsADisabledLevelNothing(t *testing.T) {
	const served = "SELECT body FROM notes WHERE id = 1"
	db := &fakeMockDb{session: []*models.Mock{nativeHandshake(), noteRows("mock-rows", served, true)}}
	client, agentEnd := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Replay(context.Background(), logtest.Disabled(), agentEnd, &models.ConditionalDstCfg{Addr: testDBAddr}, db, models.OutgoingOptions{})
	}()
	t.Cleanup(func() { _ = client.Close(); _ = agentEnd.Close(); <-done })
	c := mysqlClient{t: t, c: client}
	c.greetAndAnswer("app", mysql.Native)
	if ok := c.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	longData := append(binary.LittleEndian.AppendUint32([]byte{mysql.COM_STMT_SEND_LONG_DATA}, 1), 0, 0, 'x')
	logtest.CheckDisabledCost(t, func() {
		// The query's answer comes after the long data is taken: the
		// command phase reads one command at a time.
		c.send(0, longData)
		c.send(0, append([]byte{mysql.COM_QUERY}, served...))
		for {
			if p := c.read(); len(p) > 0 && p[0] == 0xfe {
				break
			}
		}
	})
}

// A query no recording answers, in a recording that holds no data mock, is
// named by its statement's shape, not its literals: in the line that says so
// and in the call the miss reports, unless KEPLOY_LOG_CONTENT is set.
func TestReplay_AnOrphanQuerysLinesCarryNoLiteral(t *testing.T) {
	const query = "SELECT body FROM notes WHERE tag = '" + canary + "'"
	logtest.Each(t, func(t *testing.T, on bool) {
		db := &fakeMockDb{session: []*models.Mock{nativeHandshake()}}
		logger, logs := logtest.Observe()
		client, agentEnd := net.Pipe()
		t.Cleanup(func() { _ = client.Close(); _ = agentEnd.Close() })
		done := make(chan error, 1)
		go func() {
			done <- Replay(context.Background(), logger, agentEnd, &models.ConditionalDstCfg{Addr: testDBAddr}, db, models.OutgoingOptions{})
			_ = agentEnd.Close()
		}()
		c := mysqlClient{t: t, c: client}
		c.greetAndAnswer("app", mysql.Native)
		if ok := c.read(); len(ok) == 0 || ok[0] != mysql.OK {
			t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
		}
		c.send(0, append([]byte{mysql.COM_QUERY}, query...))
		err := <-done
		var reporter interface {
			MismatchReport() *models.MockMismatchReport
		}
		if !errors.As(err, &reporter) || reporter.MismatchReport() == nil {
			t.Fatalf("the orphan query was not reported as a miss: %v", err)
		}
		const orphan = "REPLAY-ORPHAN: TC failing — no data mock available for this query, so it was never recorded or was dropped at record time"
		logtest.Check(t, logs, canary, on, orphan)
		if got, want := logs.FilterMessage(orphan).All()[0].ContextMap()["sql_shape"], "SELECT body FROM notes WHERE tag = ?"; got != want {
			t.Fatalf("the line names the query %v, want its shape %q", got, want)
		}
		if got, want := reporter.MismatchReport().ActualSummary, "COM_QUERY "+query; got != want {
			t.Fatalf("the miss reports %q, want the call as it was made, %q, for the report", got, want)
		}
	})
}
