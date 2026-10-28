package replayer

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// traceparentAttr is the query attribute Connector/J 8.1+ sends with every
// EXECUTE of a traced call: named, after the statement's own parameters.
func traceparentAttr(tp string) mysql.Parameter {
	return mysql.Parameter{Type: uint16(mysql.FieldTypeString), Name: "traceparent", Value: tp}
}

// withAttrs is p with attrs sent after its statement's own parameters.
func withAttrs(p *mysql.StmtExecutePacket, attrs ...mysql.Parameter) *mysql.StmtExecutePacket {
	p.Parameters = append(p.Parameters, attrs...)
	p.ParameterCount = len(p.Parameters)
	return p
}

func executeOf(p *mysql.StmtExecutePacket) mysql.PacketBundle {
	return mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 40}, Type: "COM_STMT_EXECUTE"}, Message: p}
}

func executePacket(flag byte, params ...mysql.Parameter) *mysql.StmtExecutePacket {
	return &mysql.StmtExecutePacket{Status: 0x17, StatementID: 33, IterationCount: 1, NewParamsBindFlag: flag,
		ParameterCount: len(params), Parameters: params}
}

// An EXECUTE's query attributes are not part of what it is matched on unless
// its statement reads one (mysql_query_attribute_string()): a per-call
// traceparent made no EXECUTE of a traced statement match its recording.
// The statement's own parameters still are.
func TestMatchStmtExecute_AQueryAttributeCountsOnlyForAStatementThatReadsIt(t *testing.T) {
	const sql = "SELECT ? * 10 AS v"
	const reads = "SELECT ? * 10 AS v, mysql_query_attribute_string('traceparent') AS t"
	id := func(v int32) mysql.Parameter { return mysql.Parameter{Type: uint16(mysql.FieldTypeLong), Value: v} }
	for name, tc := range map[string]struct {
		query      string
		exp, act   *mysql.StmtExecutePacket
		liveParams int // the live statement's own parameter count, -1 not known
		want       bool
	}{
		"another traceparent": {sql, withAttrs(executePacket(1, id(1)), traceparentAttr(recordedTraceparent)),
			withAttrs(executePacket(1, id(1)), traceparentAttr(replayTraceparent)), 1, true},
		"another traceparent, the statement's count not known": {sql, withAttrs(executePacket(1, id(1)), traceparentAttr(recordedTraceparent)),
			withAttrs(executePacket(1, id(1)), traceparentAttr(replayTraceparent)), -1, true},
		"another value": {sql, withAttrs(executePacket(1, id(1)), traceparentAttr(recordedTraceparent)),
			withAttrs(executePacket(1, id(2)), traceparentAttr(recordedTraceparent)), 1, false},
		"no attribute live": {sql, withAttrs(executePacket(1, id(1)), traceparentAttr(recordedTraceparent)),
			executePacket(1, id(1)), 1, true},
		"another traceparent, read by the statement": {reads, withAttrs(executePacket(1, id(1)), traceparentAttr(recordedTraceparent)),
			withAttrs(executePacket(1, id(1)), traceparentAttr(replayTraceparent)), 1, false},
		"the same traceparent, read by the statement": {reads, withAttrs(executePacket(1, id(1)), traceparentAttr(recordedTraceparent)),
			withAttrs(executePacket(1, id(1)), traceparentAttr(recordedTraceparent)), 1, true},
	} {
		got, _, _ := matchStmtExecutePacketQueryAware(zap.NewNop(), executeOf(tc.exp), executeOf(tc.act), tc.query, tc.query, tc.liveParams, "m", util.NewNoiseChecker(nil))
		if got != tc.want {
			t.Errorf("%s: definitive %v, want %v", name, got, tc.want)
		}
	}

	// A client that names its statement's own parameters, matched on its
	// parameters alone (no recorded PREPARE): the recording, split at its
	// named ones, holds none of the statement's, the live one, split by its
	// statement's count, one. They differ, even for the same value: never a
	// match on fewer parameters.
	named := func(v int32) mysql.Parameter {
		return mysql.Parameter{Type: uint16(mysql.FieldTypeLong), Name: "id", Value: v}
	}
	for _, v := range []int32{1, 2} {
		exp := withAttrs(executePacket(1, named(1)), traceparentAttr(recordedTraceparent))
		act := withAttrs(executePacket(1, named(v)), traceparentAttr(replayTraceparent))
		if got, _, _ := matchStmtExecutePacketQueryAware(zap.NewNop(), executeOf(exp), executeOf(act), "", sql, 1, "m", util.NewNoiseChecker(nil)); got {
			t.Errorf("the statement's own named, no recorded PREPARE, live value %d: definitive", v)
		}
	}
}

// Where an EXECUTE's query attributes begin: at its statement's own count when
// known, else after its unnamed parameters; a packet sent without types
// (and so without names) is not split.
func TestExecuteAttributesFrom(t *testing.T) {
	str := func(name string) mysql.Parameter {
		return mysql.Parameter{Type: uint16(mysql.FieldTypeString), Name: name, Value: "v"}
	}
	for name, tc := range map[string]struct {
		p         *mysql.StmtExecutePacket
		statement int
		want      int
	}{
		"one of each, count known":     {executePacket(1, str(""), str("traceparent")), 1, 1},
		"one of each, count not known": {executePacket(1, str(""), str("traceparent")), -1, 1},
		"attributes only":              {executePacket(1, str("a"), str("b")), 0, 0},
		"no attribute":                 {executePacket(1, str(""), str("")), -1, 2},
		"a count past the parameters":  {executePacket(1, str("")), 3, 1},
		"sent without types":           {executePacket(0, str(""), str("")), 1, 2},
	} {
		if got := executeAttributesFrom(tc.p, tc.statement); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}

// Through matchCommand: the recording of an EXECUTE with a traceparent is
// answered for the same EXECUTE with another, plainly, under strict mock
// noise, and by an auto-replay that learns no noise from it; the recording of
// another value is not.
func TestMatchCommand_AnExecuteWithAnotherTraceparentMatchesItsRecording(t *testing.T) {
	at := stmtT0.Add(time.Second)
	recorded := func(name, marker, key string) *models.Mock {
		m := executedMock(name, 1, at, marker, ordersInsert, key, "t1")
		withAttrs(m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket), traceparentAttr(recordedTraceparent))
		return m
	}
	for name, eng := range map[string]func() *schemanoise.Engine{
		"lenient":   func() *schemanoise.Engine { return nil },
		"strict":    func() *schemanoise.Engine { return schemanoise.New(mysqlNoiseAdapter{}, false, true) },
		"detection": func() *schemanoise.Engine { return schemanoise.New(mysqlNoiseAdapter{}, true, false) },
	} {
		t.Run(name, func(t *testing.T) {
			db := &learnedNoiseDb{fakeMockDb: &fakeMockDb{
				session:  []*models.Mock{recorded("mock-k2", "row-k2", "k2"), recorded("mock-k1", "row-k1", "k1")},
				winStart: at.Add(-time.Second), winEnd: at.Add(time.Second),
			}}
			req := liveExecute("k1", "t1")
			withAttrs(req.Message.(*mysql.StmtExecutePacket), traceparentAttr(replayTraceparent))
			dctx := newDecodeCtx()
			dctx.StmtIDToQuery[33] = ordersInsert
			dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 2}
			ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
			resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, eng(), nil)
			if err != nil || !ok || resp.Payload != "row-k1" {
				t.Fatalf("served %v (ok %v, %v, miss %+v), want the recording of k1", resp, ok, err, miss)
			}
			if len(db.learned) != 0 {
				t.Errorf("learned %v as noise from a traceparent", db.learned)
			}
		})
	}
}

// A client that names its statement's own parameters too: matchCommand splits
// the live EXECUTE, and a recording of the same statement, by the statement's
// count (its PREPARE_OK's), so the recording of the value asked for is the one
// served. Split at their named parameters, both would hold none of the
// statement's, and every recording would match.
func TestMatchCommand_AnExecuteNamingItsOwnParametersIsMatchedOnThem(t *testing.T) {
	at := stmtT0.Add(time.Second)
	named := func(params ...mysql.Parameter) []mysql.Parameter {
		for i := range params {
			params[i].Name = "p" + string(rune('0'+i))
		}
		return params
	}
	recorded := func(name, marker, key string) *models.Mock {
		m := executedMock(name, 1, at, marker, ordersInsert, key, "t1")
		p := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
		p.Parameters = named(p.Parameters...)
		withAttrs(p, traceparentAttr(recordedTraceparent))
		return m
	}
	db := &learnedNoiseDb{fakeMockDb: &fakeMockDb{
		session:  []*models.Mock{recorded("mock-k2", "row-k2", "k2"), recorded("mock-k1", "row-k1", "k1")},
		winStart: at.Add(-time.Second), winEnd: at.Add(time.Second),
	}}
	req := liveExecute("k1", "t1")
	p := req.Message.(*mysql.StmtExecutePacket)
	p.Parameters = named(p.Parameters...)
	withAttrs(p, traceparentAttr(replayTraceparent))
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 2}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, nil, nil)
	if err != nil || !ok || resp.Payload != "row-k1" {
		t.Fatalf("served %v (ok %v, %v, miss %+v), want the recording of k1", resp, ok, err, miss)
	}
}

// An EXECUTE whose PREPARE was pruned is still known as one of the live
// statement's when it sent an attribute the live one does not: its shape is
// its statement's own parameters (sameParameterShape), so the command is a
// miss that names it, not another test's answer.
func TestMatchCommand_AnExecuteWithAnAttributeWhosePrepareWasPrunedIsNotAnsweredByAnotherTest(t *testing.T) {
	db, _, execB := orderTests("")
	withAttrs(execB.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket), traceparentAttr(recordedTraceparent))
	resp, ok, miss := matchLiveOrder(t, db, "k1", "2026-10-15 09:00:00")
	if ok {
		t.Fatalf("served %q: test B's own EXECUTE lost its statement, and another test's mock answered for it", resp.Payload)
	}
	if miss == nil || miss.closestMock != execB.Name {
		t.Fatalf("the miss %+v does not name the EXECUTE that lost its statement, %q", miss, execB.Name)
	}
}

// Strict compares a recording of the live statement on its own parameters
// even when the client names them (the server takes the first num_params as
// the statement's, named or not): split at its named parameters it would
// hold none, and strict would serve another value's answer.
func TestMatchCommand_StrictComparesAnExecuteNamingItsOwnParameters(t *testing.T) {
	for _, traced := range []bool{false, true} {
		at := stmtT0.Add(time.Second)
		recorded := func(name, marker, key string) *models.Mock {
			m := executedMock(name, 1, at, marker, ordersInsert, key, "t1")
			p := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
			p.Parameters[0].Name, p.Parameters[1].Name = "key", "at"
			if traced {
				withAttrs(p, traceparentAttr(recordedTraceparent))
			}
			return m
		}
		db := &fakeMockDb{session: []*models.Mock{recorded("mock-k2", "row-k2", "k2"), recorded("mock-k1", "row-k1", "k1")},
			winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
		req := liveExecute("k3", "t1")
		p := req.Message.(*mysql.StmtExecutePacket)
		p.Parameters[0].Name, p.Parameters[1].Name = "key", "at"
		if traced {
			withAttrs(p, traceparentAttr(replayTraceparent))
		}
		dctx := newDecodeCtx()
		dctx.StmtIDToQuery[33] = ordersInsert
		dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 2}
		ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
		resp, ok, _, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
		if err != nil || ok {
			t.Errorf("traced %v: strict served %v (%v) for a value recorded nowhere", traced, resp, err)
		}
	}
}

// Strict compares the attributes of a prepared statement that reads them.
func TestMatchCommand_StrictComparesTheAttributesAnExecuteReads(t *testing.T) {
	const reads = "SELECT ? AS k, mysql_query_attribute_string('traceparent') AS t"
	at := stmtT0.Add(time.Second)
	m := executedMock("mock-reads", 1, at, "row", reads, "k1")
	withAttrs(m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket), traceparentAttr(recordedTraceparent))
	db := &fakeMockDb{session: []*models.Mock{m}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
	req := liveExecute("k1")
	withAttrs(req.Message.(*mysql.StmtExecutePacket), traceparentAttr(replayTraceparent))
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = reads
	dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 1}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
	if err != nil || ok {
		t.Fatalf("strict served %v (%v): the statement reads a traceparent the replay changed", resp, err)
	}
	if miss == nil || len(miss.fieldDiffs) != 1 || miss.fieldDiffs[0].Path != "body.parameters.1.value" {
		t.Errorf("the miss %+v does not name the attribute's drift", miss)
	}
}

// Strict compares, of a prepared statement's attributes, only the ones it
// reads, by name: a traceparent sent with it changes on every call, and the
// client may send it before the read one.
func TestMatchCommand_StrictComparesOnlyTheAttributesAnExecuteReadsByName(t *testing.T) {
	const reads = "SELECT ? AS k, mysql_query_attribute_string('tenant') AS t"
	tenant := func(v string) mysql.Parameter {
		return mysql.Parameter{Type: uint16(mysql.FieldTypeString), Name: "tenant", Value: v}
	}
	cases := []struct {
		name   string
		attrs  []mysql.Parameter
		served bool
		drift  string
	}{
		{name: "the same tenant, another traceparent", attrs: []mysql.Parameter{tenant("a"), traceparentAttr(replayTraceparent)}, served: true},
		{name: "the same tenant after another traceparent", attrs: []mysql.Parameter{traceparentAttr(replayTraceparent), tenant("a")}, served: true},
		{name: "the same tenant, no traceparent", attrs: []mysql.Parameter{tenant("a")}, served: true},
		{name: "another tenant, the same traceparent", attrs: []mysql.Parameter{tenant("b"), traceparentAttr(recordedTraceparent)}, drift: "body.parameters.1.value"},
		{name: "another tenant after the same traceparent", attrs: []mysql.Parameter{traceparentAttr(recordedTraceparent), tenant("b")}, drift: "body.parameters.1.value"},
		{name: "no tenant", attrs: []mysql.Parameter{traceparentAttr(recordedTraceparent)}, drift: "body.parameters.1.value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at := stmtT0.Add(time.Second)
			m := executedMock("mock-reads", 1, at, "row", reads, "k1")
			withAttrs(m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket), tenant("a"), traceparentAttr(recordedTraceparent))
			db := &fakeMockDb{session: []*models.Mock{m}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
			req := liveExecute("k1")
			withAttrs(req.Message.(*mysql.StmtExecutePacket), c.attrs...)
			dctx := newDecodeCtx()
			dctx.StmtIDToQuery[33] = reads
			dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 1}
			ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
			resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, mocknoise.New(mysqlNoiseAdapter{}, false, true), nil)
			if err != nil || ok != c.served {
				t.Fatalf("served=%v (%v, %v, miss %+v); want served=%v", ok, resp, err, miss, c.served)
			}
			if c.served {
				return
			}
			// Only the read attribute's fields: its value, and its type
			// when it was not sent (a NULL).
			named := false
			for _, d := range miss.fieldDiffs {
				named = named || d.Path == c.drift
				if !strings.HasPrefix(d.Path, "body.parameters.1.") {
					t.Errorf("the miss names %s: only the read attribute is compared", d.Path)
				}
			}
			if !named {
				t.Errorf("the miss %+v does not name the read attribute's drift (%s)", miss, c.drift)
			}
		})
	}
}

// The names a statement reads, in the order it first names them; not known
// when it reads one it does not name with a string literal.
func TestAttributesRead(t *testing.T) {
	for _, c := range []struct {
		query string
		names []string
		ok    bool
	}{
		{query: "SELECT 1", ok: true},
		{query: "SELECT mysql_query_attribute_string('tenant')", names: []string{"tenant"}, ok: true},
		{query: `SELECT MYSQL_QUERY_ATTRIBUTE_STRING ( "Tenant" ), mysql_query_attribute_string('a''b'), mysql_query_attribute_string('tenant')`, names: []string{"Tenant", "a'b", "tenant"}, ok: true},
		{query: "SELECT mysql_query_attribute_string('x'), mysql_query_attribute_string('x')", names: []string{"x"}, ok: true},
		{query: `SELECT mysql_query_attribute_string("a""b")`, names: []string{`a"b`}, ok: true},
		{query: `SELECT mysql_query_attribute_string('a\'b')`, names: []string{"a'b"}, ok: true},
		{query: "SELECT mysql_query_attribute_string('tenant'), mysql_query_attribute_string(@name)", ok: false},
		{query: "SELECT mysql_query_attribute_string(CONCAT('ten', 'ant'))", ok: false},
	} {
		names, ok := attributesRead(c.query)
		if ok != c.ok || (ok && !reflect.DeepEqual(names, c.names)) {
			t.Errorf("attributesRead(%q) = %q, %v; want %q, %v", c.query, names, ok, c.names, c.ok)
		}
	}
}

// keploy #4810's guard holds for a client that names its statement's own
// parameters: test B's EXECUTE, its PREPARE pruned, has the live one's shape.
func TestMatchCommand_AnExecuteNamingItsOwnParametersWhosePrepareWasPrunedIsNotAnsweredByAnotherTest(t *testing.T) {
	db, _, execB := orderTests("")
	for _, m := range db.session {
		if p, ok := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket); ok {
			p.Parameters[0].Name, p.Parameters[1].Name = "key", "at"
		}
	}
	req := liveExecute("k1", "2026-10-15 09:00:00")
	p := req.Message.(*mysql.StmtExecutePacket)
	p.Parameters[0].Name, p.Parameters[1].Name = "key", "at"
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 2}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, nil, nil)
	if err != nil || ok {
		t.Fatalf("served %v (%v): test B's own EXECUTE lost its statement, and another test's mock answered for it", resp, err)
	}
	if miss == nil || miss.closestMock != execB.Name {
		t.Fatalf("the miss %+v does not name the EXECUTE that lost its statement, %q", miss, execB.Name)
	}
}

// How a recorded EXECUTE is split against a live one whose statement has
// liveParams parameters of its own: by that count when it recorded the same
// statement; else at its trailing named parameters only when that leaves as
// many and they are named as the live one's attributes are, in order (not
// split at all, then): never compared on fewer.
func TestRecordedAttributesFrom(t *testing.T) {
	p := func(names ...string) *mysql.StmtExecutePacket {
		params := make([]mysql.Parameter, len(names))
		for i, n := range names {
			params[i] = mysql.Parameter{Type: uint16(mysql.FieldTypeString), Name: n, Value: "v"}
		}
		return executePacket(1, params...)
	}
	for name, tc := range map[string]struct {
		p, live    *mysql.StmtExecutePacket
		same       bool
		liveParams int
		want       int
	}{
		"the same statement":                               {p("key", "at", "traceparent"), p("key", "at"), true, 2, 2},
		"another, its own unnamed":                         {p("", "", "traceparent"), p("", "", "traceparent"), false, 2, 2},
		"another, its own named too":                       {p("key", "at", "traceparent"), p("", "", "traceparent"), false, 2, 3},
		"another, one of its own named":                    {p("", "at", "traceparent"), p("", "", "traceparent"), false, 2, 3},
		"another, attributes only, live has none":          {p("traceparent"), p("traceparent"), false, 0, 0},
		"another, its own named, live has none":            {p("key", "at"), p(), false, 0, 2},
		"another, its own named and traced, live has none": {p("key", "at", "traceparent"), p("traceparent"), false, 0, 3},
		"another, one of its own named, live has one":      {p("", "at", "traceparent"), p("", "traceparent"), false, 1, 3},
		"another, attributes the live one did not send":    {p("", "traceparent"), p(""), false, 1, 2},
		"another, its own named, live has none, traced":    {p("key"), p("traceparent"), false, 0, 1},
		"another, attributes named otherwise":              {p("", "traceparent"), p("", "tracestate"), false, 1, 2},
		"another, attributes in another order":             {p("", "traceparent", "tracestate"), p("", "tracestate", "traceparent"), false, 1, 3},
		"the live count not known":                         {p("", "traceparent"), nil, false, -1, 1},
		"sent without types":                               {executePacket(0, mysql.Parameter{Value: "v"}, mysql.Parameter{Value: "w"}), p(""), false, 1, 2},
	} {
		if got := recordedAttributesFrom(tc.p, tc.live, tc.same, tc.liveParams); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}

// Strict splits a recording whose statement the matcher found by its PREPARE
// in the pool (it kept no query of its own) as the matcher does: a client that
// names its statement's own parameters, traced, is answered with the
// recording of its value, and refused for a value recorded nowhere.
func TestMatchCommand_StrictSplitsARecordingByItsPreparedStatement(t *testing.T) {
	at := stmtT0.Add(time.Second)
	recorded := func(name, marker, key string) *models.Mock {
		m := executedMock(name, 1, at, marker, "", key, "t1")
		p := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
		p.Parameters[0].Name, p.Parameters[1].Name = "key", "at"
		withAttrs(p, traceparentAttr(recordedTraceparent))
		return m
	}
	for key, want := range map[string]string{"k1": "row-k1", "k3": ""} {
		db := &fakeMockDb{session: []*models.Mock{
			preparedMock("mock-prepare", "0", 1, ordersInsert, at.Add(-time.Millisecond)),
			recorded("mock-k2", "row-k2", "k2"), recorded("mock-k1", "row-k1", "k1"),
		}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
		req := liveExecute(key, "t1")
		p := req.Message.(*mysql.StmtExecutePacket)
		p.Parameters[0].Name, p.Parameters[1].Name = "key", "at"
		withAttrs(p, traceparentAttr(replayTraceparent))
		dctx := newDecodeCtx()
		dctx.StmtIDToQuery[33] = ordersInsert
		dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 2}
		ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
		resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
		switch {
		case err != nil:
			t.Fatalf("%s: %v", key, err)
		case want == "" && ok:
			t.Errorf("%s: strict served %q for a value recorded nowhere", key, resp.Payload)
		case want != "" && (!ok || resp.Payload != want):
			t.Errorf("%s: served %v (ok %v, miss %+v), want %q", key, resp, ok, miss, want)
		}
	}
}

// Strict compares the attributes of a live statement that reads them with a
// recording whose statement the matcher could not name (no query kept, no
// PREPARE in the pool): it is not split, though its named run would leave
// the live statement's count.
func TestMatchCommand_StrictComparesTheAttributesTheLiveStatementReads(t *testing.T) {
	const reads = "SELECT ? AS k, mysql_query_attribute_string('traceparent') AS t"
	at := stmtT0.Add(time.Second)
	m := executedMock("mock-reads", 1, at, "row", "", "k1")
	withAttrs(m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket), traceparentAttr(recordedTraceparent))
	db := &fakeMockDb{session: []*models.Mock{m}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
	req := liveExecute("k1")
	withAttrs(req.Message.(*mysql.StmtExecutePacket), traceparentAttr(replayTraceparent))
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = reads
	dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 1}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	resp, ok, _, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
	if err != nil || ok {
		t.Fatalf("strict served %v (%v): the live statement reads a traceparent the replay changed", resp, err)
	}
}

// keploy #4810's guard, for a traced client that names its statement's own
// parameters: test B's EXECUTE, its PREPARE pruned, carries an attribute
// after them, and still has the live one's shape.
func TestMatchCommand_ATracedExecuteNamingItsOwnParametersWhosePrepareWasPrunedIsNotAnsweredByAnotherTest(t *testing.T) {
	db, _, execB := orderTests("")
	for _, m := range db.session {
		if p, ok := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket); ok {
			p.Parameters[0].Name, p.Parameters[1].Name = "key", "at"
		}
	}
	withAttrs(execB.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket), traceparentAttr(recordedTraceparent))
	req := liveExecute("k1", "2026-10-15 09:00:00")
	p := req.Message.(*mysql.StmtExecutePacket)
	p.Parameters[0].Name, p.Parameters[1].Name = "key", "at"
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 2}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, nil, nil)
	if err != nil || ok {
		t.Fatalf("served %v (%v): test B's own EXECUTE lost its statement, and another test's mock answered for it", resp, err)
	}
	if miss == nil || miss.closestMock != execB.Name {
		t.Fatalf("the miss %+v does not name the EXECUTE that lost its statement, %q", miss, execB.Name)
	}
}

// An auto-replay learns the drift of an attribute a prepared statement reads,
// as of a COM_QUERY's: it is a request field, and the recording's body holds
// it.
func TestMatchCommand_LearnsTheDriftOfAnAttributeAnExecuteReads(t *testing.T) {
	const reads = "SELECT ? AS k, mysql_query_attribute_string('traceparent') AS t"
	at := stmtT0.Add(time.Second)
	m := executedMock("mock-reads", 1, at, "row", reads, "k1")
	withAttrs(m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket), traceparentAttr(recordedTraceparent))
	db := &learnedNoiseDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{m}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}}
	req := liveExecute("k1")
	withAttrs(req.Message.(*mysql.StmtExecutePacket), traceparentAttr(replayTraceparent))
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = reads
	dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 1}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	if _, _, _, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, schemanoise.New(mysqlNoiseAdapter{}, true, false), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.learned["body.parameters.1.value"]; !ok {
		t.Errorf("learned %v, want the read attribute's drift (body.parameters.1.value)", db.learned)
	}
}

// An auto-replay learns the drift strict then compares (strictGate's
// recorded body): of a parameter of a client that names its statement's own,
// and of an attribute a statement reads when the recording kept no query (its
// PREPARE in the pool). The strict replay after it is answered.
func TestMatchCommand_AnAutoReplayLearnsTheDriftStrictCompares(t *testing.T) {
	const reads = "SELECT ? AS k, mysql_query_attribute_string('traceparent') AS t"
	at := stmtT0.Add(time.Second)
	for name, tc := range map[string]struct {
		recorded func() *models.Mock
		pool     []*models.Mock
		live     func() mysql.Request
		query    string
		params   uint16
		want     string
	}{
		"its own named, one drifting": {
			recorded: func() *models.Mock {
				m := executedMock("mock-k1", 1, at, "row-k1", ordersInsert, "k1", "t1")
				p := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
				p.Parameters[0].Name, p.Parameters[1].Name = "key", "at"
				withAttrs(p, traceparentAttr(recordedTraceparent))
				return m
			},
			live: func() mysql.Request {
				r := liveExecute("k1", "t2")
				p := r.Message.(*mysql.StmtExecutePacket)
				p.Parameters[0].Name, p.Parameters[1].Name = "key", "at"
				withAttrs(p, traceparentAttr(replayTraceparent))
				return r
			},
			query: ordersInsert, params: 2, want: "body.parameters.1.value",
		},
		"a read attribute, no query kept": {
			recorded: func() *models.Mock {
				m := executedMock("mock-reads", 1, at, "row", "", "k1")
				withAttrs(m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket), traceparentAttr(recordedTraceparent))
				return m
			},
			pool: []*models.Mock{preparedMock("mock-prepare", "0", 1, reads, at.Add(-time.Millisecond))},
			live: func() mysql.Request {
				r := liveExecute("k1")
				withAttrs(r.Message.(*mysql.StmtExecutePacket), traceparentAttr(replayTraceparent))
				return r
			},
			query: reads, params: 1, want: "body.parameters.1.value",
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := tc.recorded()
			session := append(append([]*models.Mock{}, tc.pool...), m)
			dctx := newDecodeCtx()
			dctx.StmtIDToQuery[33] = tc.query
			dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: tc.params}
			ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
			db := &learnedNoiseDb{fakeMockDb: &fakeMockDb{session: session, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}}
			if _, ok, _, err := matchCommand(ctx, zap.NewNop(), tc.live(), db, dctx, schemanoise.New(mysqlNoiseAdapter{}, true, false), nil); err != nil || !ok {
				t.Fatalf("the auto-replay served nothing (%v)", err)
			}
			if _, ok := db.learned[tc.want]; !ok || len(db.learned) != 1 {
				t.Fatalf("learned %v, want only %s", db.learned, tc.want)
			}
			m.Spec.ReqBodyNoise = db.learned
			strictDb := &fakeMockDb{session: session, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
			if resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), tc.live(), strictDb, dctx, schemanoise.New(mysqlNoiseAdapter{}, false, true), nil); err != nil || !ok {
				t.Errorf("strict, after the auto-replay, served %v (%v, miss %+v)", resp, err, miss)
			}
		})
	}
}

// A recording whose statement is not known, by a client that names its
// statement's own parameters, is not split at them for a live statement with
// none: named as no attribute the live EXECUTE sent, they are its statement's,
// and the recording is of another statement. Strict does not serve it, and
// it does not outrank the live statement's own recording.
func TestMatchCommand_ANamedRecordingOfAnotherStatementIsNotAnExecuteWithNoParameters(t *testing.T) {
	const count = "SELECT COUNT(*) FROM orders"
	at := stmtT0.Add(20 * time.Second)
	insert := func(traced bool) *models.Mock {
		m := executedMock("mock-insert", 1, at, "insert-ok", "", "k1", "t1")
		p := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
		p.Parameters[0].Name, p.Parameters[1].Name = "key", "at"
		if traced {
			withAttrs(p, traceparentAttr(recordedTraceparent))
		}
		return m
	}
	live := func(traced bool) mysql.Request {
		r := liveExecute()
		if traced {
			withAttrs(r.Message.(*mysql.StmtExecutePacket), traceparentAttr(replayTraceparent))
		}
		return r
	}
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = count
	dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 0}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	for _, traced := range []bool{false, true} {
		db := &fakeMockDb{session: []*models.Mock{insert(traced)}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
		if resp, ok, _, err := matchCommand(ctx, zap.NewNop(), live(traced), db, dctx, schemanoise.New(mysqlNoiseAdapter{}, false, true), nil); err != nil || ok {
			t.Errorf("traced %v: strict served %v (%v) for %q", traced, resp, err, count)
		}
		own := executedMock("mock-count", 5, stmtT0, "count-row", count)
		db = &fakeMockDb{session: []*models.Mock{own, insert(traced)}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
		if resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), live(traced), db, dctx, nil, nil); err != nil || !ok || resp.Payload != "count-row" {
			t.Errorf("traced %v: served %v (ok %v, %v, miss %+v), want the statement's own recording", traced, resp, ok, err, miss)
		}
	}
}

// Strict keeps every parameter of a recording whose statement reads its
// attributes, though the live statement does not, and of the live EXECUTE
// only its statement's own: the recording's attributes are missing from the
// live body, so a recording of that other statement is not served for it,
// whatever traceparent the live EXECUTE sent.
func TestMatchCommand_StrictComparesTheAttributesTheRecordedStatementReads(t *testing.T) {
	const reads = "SELECT ? AS k, mysql_query_attribute_string('traceparent') AS t"
	at := stmtT0.Add(time.Second)
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = "SELECT ? AS k"
	dctx.PreparedStatements[33] = &mysql.StmtPrepareOkPacket{StatementID: 33, NumParams: 1}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	for _, tp := range []string{replayTraceparent, recordedTraceparent} {
		m := executedMock("mock-reads", 1, at, "row", reads, "k1")
		withAttrs(m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket), traceparentAttr(recordedTraceparent))
		db := &fakeMockDb{session: []*models.Mock{m}, winStart: at.Add(-time.Second), winEnd: at.Add(time.Second)}
		req := liveExecute("k1")
		withAttrs(req.Message.(*mysql.StmtExecutePacket), traceparentAttr(tp))
		resp, ok, _, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, schemanoise.New(mysqlNoiseAdapter{}, false, true), nil)
		if err != nil || ok {
			t.Errorf("live traceparent %s: strict served %v (%v), a recording of a statement that reads its attributes", tp, resp, err)
		}
	}
}
