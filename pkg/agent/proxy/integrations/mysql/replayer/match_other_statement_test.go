package replayer_test

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// A statement the recording never ran, and one of poolQuery's shape that
// reads another table: the same parse tree, other identifiers.
const (
	otherQuery     = "SELECT code FROM other WHERE ref = ? AND live = 1"
	otherShapeTwin = "SELECT id, name FROM big WHERE id = ?"
)

// stageOwn stages mocks as keploy test does (exact false: test 0's window
// holds own) or as the proven switch serves them (exact: test T's mapping
// is own).
func stageOwn(t *testing.T, all, own []*models.Mock, exact bool) *proxy.MockManager {
	t.Helper()
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	if exact {
		mm.SetMocksWithWindow(nil, all, models.BaseTime, time.Now().Add(time.Hour))
		mm.ServeExactTest("T", own)
		return mm
	}
	start, end := diffWindow(0)
	mm.SetMocksWithWindow(nil, all, start, end.Add(time.Hour))
	return mm
}

// synthesized is the answer to a PREPARE no recording answers: the
// PREPARE_OK the matcher makes for it, which names no recording.
const synthesized = "<synthesized PREPARE_OK>"

// otherStatementCase is one command a test's recording holds no recording
// of, and what answers it: want "" is a miss.
type otherStatementCase struct {
	name     string
	stmts    map[uint32]string // the connection's prepared statements
	req      mysql.Request
	exact    string // the answer under exact serving
	notExact string // the answer under keploy test
	nearest  string // the recording a miss names as the nearest
}

func runOtherStatementCases(t *testing.T, all, own []*models.Mock, cases []otherStatementCase) {
	t.Helper()
	for _, c := range cases {
		for _, exact := range []bool{false, true} {
			want := c.notExact
			if exact {
				want = c.exact
			}
			t.Run(fmt.Sprintf("%s/exact=%v", c.name, exact), func(t *testing.T) {
				mm := stageOwn(t, all, own, exact)
				ctx, dctx := poolConn("c1")
				if c.stmts != nil {
					dctx.StmtIDToQuery = c.stmts
				}
				resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), c.req, mm, dctx)
				if err != nil {
					t.Fatalf("match: %v", err)
				}
				switch {
				case want == synthesized:
					if !ok {
						t.Fatalf("a miss (%s); want the PREPARE_OK the matcher makes", miss)
					}
					if _, prepOK := resp.Message.(*mysql.StmtPrepareOkPacket); !prepOK || resp.Payload != "" {
						t.Fatalf("answered with %q (%T); want the PREPARE_OK the matcher makes", resp.Payload, resp.Message)
					}
				case want == "" && ok:
					t.Fatalf("answered with %s; want a miss: no recording ran this statement", resp.Payload)
				case want == "" && miss == "":
					t.Fatalf("a miss that says nothing; want the miss report")
				case want == "" && c.nearest != "" && !strings.Contains(miss, "closestMock:"+c.nearest+" "):
					t.Fatalf("the miss (%s) does not name %s, the nearest recording", miss, c.nearest)
				case want != "" && !ok:
					t.Fatalf("a miss (%s); want %s", miss, want)
				case want != "" && resp.Payload != want:
					t.Fatalf("answered with %s; want %s", resp.Payload, want)
				}
			})
		}
	}
}

// An EXECUTE of a statement the test's recording never ran was answered by
// the score fallback with another statement's recording, under exact
// serving as under keploy test: header, status, flags and parameter count
// alone scored, so any recorded EXECUTE won. "SELECT code FROM other WHERE
// ref = ? AND live = 1" with 5 got the rows of "SELECT id, name FROM small
// WHERE id = ?" with 7, and the app read rows its statement never returned.
//
// The rule: an EXECUTE is answered only by a recording of its own statement
// (the same prepared text: sqlStatementIdentity, case-insensitive), or, when
// the recording lost its statement, by one with every parameter equal.
// Under exact serving that is all, and tighter: a recording of its own
// statement with other parameters answers only when every parameter that
// differs is configured or learned noise (it is another call's rows: a read
// of 5 got 7's), and a recording that lost its statement answers only a
// command whose statement is unknown too (against a known one it may be
// another statement's rows). Without exact serving, a recording of another
// statement of the same shape (the same parse tree, other identifiers), and
// any recording when the live statement is unknown, still scores, as before:
// that is keploy test's lenient tier, kept; a recording of another statement
// of another shape never answers, in either mode.
func TestAnExecuteOfAStatementNoRecordingRanIsAMiss(t *testing.T) {
	prep := diffPrepareMock("prep-c1", "c1", poolQuery, 1, diffBase.Add(time.Millisecond))
	own := diffMock("exec-small-7", "c1", []mysql.Request{executeReq(1, "7")}, diffBase.Add(2*time.Millisecond), models.LifetimePerTest, "mocks")
	// lost: an EXECUTE whose PREPARE the recording does not hold (statement
	// 9 on c2): its statement is unknown.
	lost := diffMock("exec-lost-8", "c2", []mysql.Request{executeReq(9, "8")}, diffBase.Add(3*time.Millisecond), models.LifetimePerTest, "mocks")
	all := []*models.Mock{prep, own}
	stmts := func(q string) map[uint32]string { return map[uint32]string{1: poolQuery, 2: q} }
	runOtherStatementCases(t, all, []*models.Mock{own}, []otherStatementCase{
		{name: "another statement", stmts: stmts(otherQuery), req: executeReq(2, "5"), nearest: "exec-small-7"},
		{name: "another statement with the recording's parameters", stmts: stmts(otherQuery), req: executeReq(2, "7"), nearest: "exec-small-7"},
		{name: "another statement of the same shape", stmts: stmts(otherShapeTwin), req: executeReq(2, "5"), notExact: "exec-small-7", nearest: "exec-small-7"},
		{name: "a statement the connection did not prepare", stmts: stmts(otherQuery), req: executeReq(3, "5"), notExact: "exec-small-7"},
		// The rule's other side: the statement's own recording answers.
		{name: "its own statement", stmts: stmts(otherQuery), req: executeReq(1, "7"), exact: "exec-small-7", notExact: "exec-small-7"},
		{name: "its own statement with other parameters", stmts: stmts(otherQuery), req: executeReq(1, "5"), notExact: "exec-small-7", nearest: "exec-small-7"},
		{name: "its own statement in another case", stmts: map[uint32]string{1: strings.ToLower(poolQuery)}, req: executeReq(1, "7"), exact: "exec-small-7", notExact: "exec-small-7"},
		{name: "its own statement in another case with other parameters", stmts: map[uint32]string{1: strings.ToLower(poolQuery)}, req: executeReq(1, "5"), notExact: "exec-small-7", nearest: "exec-small-7"},
	})
	t.Run("a recording whose statement is lost", func(t *testing.T) {
		runOtherStatementCases(t, []*models.Mock{lost}, []*models.Mock{lost}, []otherStatementCase{
			{name: "other parameters", stmts: stmts(otherQuery), req: executeReq(2, "5"), notExact: "exec-lost-8"},
			{name: "its parameters", stmts: stmts(otherQuery), req: executeReq(2, "8"), notExact: "exec-lost-8", nearest: "exec-lost-8"},
			{name: "its parameters, the command's statement unknown too", stmts: stmts(otherQuery), req: executeReq(3, "8"), exact: "exec-lost-8", notExact: "exec-lost-8"},
		})
	})
	// A parameter that differs is noise: configured, or learned on the
	// recording (req_body_noise). Under exact serving that recording of the
	// statement answers; a parameter the noise does not name still misses.
	t.Run("a recording whose parameter is learned noise", func(t *testing.T) {
		noisy := diffMock("exec-small-noisy", "c1", []mysql.Request{executeReq(1, "7")}, diffBase.Add(2*time.Millisecond), models.LifetimePerTest, "mocks")
		noisy.Spec.ReqBodyNoise = map[string][]string{"body.parameters.0.value": {}}
		runOtherStatementCases(t, []*models.Mock{prep, noisy}, []*models.Mock{noisy}, []otherStatementCase{
			{name: "its own statement with another value there", stmts: stmts(otherQuery), req: executeReq(1, "5"), exact: "exec-small-noisy", notExact: "exec-small-noisy"},
		})
		other := diffMock("exec-small-other-noise", "c1", []mysql.Request{executeReq(1, "7")}, diffBase.Add(2*time.Millisecond), models.LifetimePerTest, "mocks")
		other.Spec.ReqBodyNoise = map[string][]string{"body.parameters.1.value": {}}
		runOtherStatementCases(t, []*models.Mock{prep, other}, []*models.Mock{other}, []otherStatementCase{
			{name: "its own statement with another value elsewhere", stmts: stmts(otherQuery), req: executeReq(1, "5"), notExact: "exec-small-other-noise", nearest: "exec-small-other-noise"},
		})
	})
}

// Under exact serving, a miss on a recording of the command's statement made
// with other parameters names the parameter that drifted and says it is not
// noise, instead of asking to re-record a statement that did not change.
func TestAnExecuteWithOtherParametersSaysWhichDrifted(t *testing.T) {
	prep := diffPrepareMock("prep-c1", "c1", poolQuery, 1, diffBase.Add(time.Millisecond))
	own := diffMock("exec-small-7", "c1", []mysql.Request{executeReq(1, "7")}, diffBase.Add(2*time.Millisecond), models.LifetimePerTest, "mocks")
	mm := stageOwn(t, []*models.Mock{prep, own}, []*models.Mock{own}, true)
	ctx, dctx := poolConn("c1")
	dctx.StmtIDToQuery = map[uint32]string{1: poolQuery}
	_, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), executeReq(1, "5"), mm, dctx)
	if err != nil || ok {
		t.Fatalf("match: ok=%v err=%v; want a miss", ok, err)
	}
	for _, want := range []string{"closestMock:exec-small-7 ", "body.parameters.0.value", "not noise"} {
		if !strings.Contains(miss, want) {
			t.Errorf("the miss (%s) does not say %q", miss, want)
		}
	}
}

// Datadog DBM's prologues of two requests: the traceparent changes on every
// run (match_dbmcomment_test.go).
const (
	traceWriter = "/*dddb='appdb',traceparent='00-1111111111111111aaaaaaaaaaaaaaaa-1111111111111111-01'*/ "
	traceReader = "/*dddb='appdb',traceparent='00-2222222222222222bbbbbbbbbbbbbbbb-2222222222222222-01'*/ "
)

// A COM_QUERY, and a PREPARE, is answered by a recording of its own
// statement: the same text but for its inline literals (a COM_QUERY's
// parameters). Under exact serving that is all, and tighter: the
// identifier-blind DML tier (the same parse tree: another table, other
// columns) answered an UPDATE of kv with the recording of an UPDATE of
// users; and a recording of a COM_QUERY's own statement made with other
// literals answers only when every literal that differs, by position, is
// configured or learned noise (body.query.literals.N): a read of id 5 got
// id 7's row. Comments are left out (a traceparent changes on every run).
// A PREPARE's recording still answers with other literals: its answer is
// the statement's metadata, and each EXECUTE is matched on the live text.
// Without exact serving both tiers are kept, as before.
func TestAQueryOfAStatementNoRecordingRanIsAMiss(t *testing.T) {
	ts := diffBase.Add(time.Millisecond)
	update := diffMock("query-update-users", "c1", []mysql.Request{queryReq("UPDATE users SET name = 'a' WHERE id = 3")}, ts, models.LifetimePerTest, "mocks")
	read := diffMock("query-read-small", "c1", []mysql.Request{queryReq("SELECT id, name FROM small WHERE id = 7")}, ts.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	prepUpdate := diffPrepareMock("prep-update-users", "c1", "UPDATE users SET name = ? WHERE id = ?", 4, ts.Add(2*time.Millisecond))
	prepRead := diffPrepareMock("prep-read-live", "c1", "SELECT id FROM small WHERE id = ? AND live = 1", 5, ts.Add(3*time.Millisecond))
	all := []*models.Mock{update, read, prepUpdate, prepRead}
	runOtherStatementCases(t, all, all, []otherStatementCase{
		{name: "an UPDATE of another table", req: queryReq("UPDATE kv SET v = 'b' WHERE id = 4"), notExact: "query-update-users", nearest: "query-update-users"},
		{name: "its own UPDATE with other literals", req: queryReq("UPDATE users SET name = 'b' WHERE id = 4"), notExact: "query-update-users", nearest: "query-update-users"},
		{name: "its own UPDATE with one other literal", req: queryReq("UPDATE users SET name = 'a' WHERE id = 4"), notExact: "query-update-users", nearest: "query-update-users"},
		{name: "a SELECT of another statement", req: queryReq("SELECT code FROM other WHERE ref = 5 AND live = 1")},
		{name: "its own SELECT with another literal", req: queryReq("SELECT id, name FROM small WHERE id = 5"), notExact: "query-read-small", nearest: "query-read-small"},
		{name: "its own SELECT with a comment", req: queryReq(traceReader + "SELECT id, name FROM small WHERE id = 7"), exact: "query-read-small", notExact: "query-read-small"},
		{name: "its own SELECT with a comment and another literal", req: queryReq(traceReader + "SELECT id, name FROM small WHERE id = 5"), notExact: "query-read-small", nearest: "query-read-small"},
		{name: "a PREPARE of an UPDATE of another table", req: prepareReq("UPDATE kv SET v = ? WHERE id = ?"), exact: synthesized, notExact: "prep-update-users"},
		{name: "a PREPARE of its own SELECT with another literal", req: prepareReq("SELECT id FROM small WHERE id = ? AND live = 0"), exact: "prep-read-live", notExact: "prep-read-live"},
	})
	// A literal that differs is noise: configured, or learned on the
	// recording (req_body_noise). Under exact serving that recording of the
	// statement answers, whatever its comments; a literal the noise does not
	// name still misses.
	t.Run("a recording whose literal is learned noise", func(t *testing.T) {
		// The comment after SELECT is the one the body keeps
		// (body.query.comments); vitess drops the others.
		noisy := diffMock("query-read-noisy", "c1", []mysql.Request{queryReq("SELECT /* traceparent=00-1111-01 */ id, name FROM small WHERE id = 7")}, ts, models.LifetimePerTest, "mocks")
		noisy.Spec.ReqBodyNoise = map[string][]string{"body.query.literals.0": {}}
		runOtherStatementCases(t, []*models.Mock{noisy}, []*models.Mock{noisy}, []otherStatementCase{
			{name: "its own SELECT with another value there", req: queryReq("SELECT id, name FROM small WHERE id = 5"), exact: "query-read-noisy", notExact: "query-read-noisy"},
			{name: "its own SELECT with another value there and another comment", req: queryReq(traceReader + "SELECT id, name FROM small WHERE id = 5"), exact: "query-read-noisy", notExact: "query-read-noisy"},
			{name: "its own SELECT with another value there and a comment inside", req: queryReq("SELECT /* traceparent=00-3333-01 */ id, name FROM small WHERE id = 5"), exact: "query-read-noisy", notExact: "query-read-noisy"},
		})
		// A query attribute the statement reads is compared as a literal
		// is: another value of it is another call.
		const readsAttr = "SELECT mysql_query_attribute_string('tenant') AS t, name FROM small WHERE id = "
		attr := diffMock("query-read-attr", "c1", []mysql.Request{withTenant(queryReq(readsAttr+"7"), "a")}, ts, models.LifetimePerTest, "mocks")
		attr.Spec.ReqBodyNoise = map[string][]string{"body.query.literals.1": {}}
		runOtherStatementCases(t, []*models.Mock{attr}, []*models.Mock{attr}, []otherStatementCase{
			{name: "a statement reading its attribute, another value of the noise literal", req: withTenant(queryReq(readsAttr+"5"), "a"), exact: "query-read-attr", notExact: "query-read-attr"},
			{name: "a statement reading its attribute, another value of it too", req: withTenant(queryReq(readsAttr+"5"), "b"), notExact: "query-read-attr", nearest: "query-read-attr"},
		})
		other := diffMock("query-update-other-noise", "c1", []mysql.Request{queryReq("UPDATE users SET name = 'a' WHERE id = 3")}, ts, models.LifetimePerTest, "mocks")
		other.Spec.ReqBodyNoise = map[string][]string{"body.query.literals.1": {}}
		runOtherStatementCases(t, []*models.Mock{other}, []*models.Mock{other}, []otherStatementCase{
			{name: "its own UPDATE with another value there", req: queryReq("UPDATE users SET name = 'a' WHERE id = 4"), exact: "query-update-other-noise", notExact: "query-update-other-noise"},
			{name: "its own UPDATE with another value elsewhere too", req: queryReq("UPDATE users SET name = 'b' WHERE id = 4"), notExact: "query-update-other-noise", nearest: "query-update-other-noise"},
		})
	})
}

// withTenant is r, a COM_QUERY, sent with the query attribute tenant.
func withTenant(r mysql.Request, tenant string) mysql.Request {
	qp := r.Message.(*mysql.QueryPacket)
	qp.ParameterCount, qp.NewParamsBindFlag, qp.NullBitmap = 1, 1, []byte{0}
	qp.Parameters = []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Name: "tenant", Value: tenant}}
	return r
}

// Under exact serving, a miss on a recording of the COM_QUERY's statement
// made with other inline literals names the literal that drifted, by
// position, and says it is not noise, instead of asking to re-record a
// statement whose text did not change.
func TestAQueryWithOtherLiteralsSaysWhichDrifted(t *testing.T) {
	read := diffMock("query-read-small", "c1", []mysql.Request{queryReq("SELECT /* traceparent=00-1111-01 */ id, name FROM small WHERE id = 7 AND live = 1")}, diffBase.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	mm := stageOwn(t, []*models.Mock{read}, []*models.Mock{read}, true)
	ctx, dctx := poolConn("c1")
	_, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), queryReq(traceReader+"SELECT /* traceparent=00-2222-01 */ id, name FROM small WHERE id = 5 AND live = 1"), mm, dctx)
	if err != nil || ok {
		t.Fatalf("match: ok=%v err=%v; want a miss", ok, err)
	}
	for _, want := range []string{"closestMock:query-read-small ", "body.query.literals.0", "inline literals", "not noise"} {
		if !strings.Contains(miss, want) {
			t.Errorf("the miss (%s) does not say %q", miss, want)
		}
	}
	for _, unwanted := range []string{"body.query.literals.1", "body.query.comments"} {
		if strings.Contains(miss, unwanted) {
			t.Errorf("the miss (%s) names %q, which did not drift or is left out", miss, unwanted)
		}
	}
}

// A PREPARE no recording answers gets the PREPARE_OK the matcher makes,
// framed as the connection negotiated. A client without
// CLIENT_DEPRECATE_EOF (go-sql-driver) reads the parameter definitions up
// to an EOF packet; the made answer carried none, so such a client waited
// for the rest of it forever: an app whose prepared statement changed since
// it was recorded hung in PREPARE, and never sent the EXECUTE whose miss
// fails its test. A client that negotiated it reads no EOF there.
func TestAPrepareNoRecordingAnswersIsFramedAsTheConnectionNegotiated(t *testing.T) {
	ts := diffBase.Add(time.Millisecond)
	all := []*models.Mock{diffPrepareMock("prep-update-users", "c1", "UPDATE users SET name = ? WHERE id = ?", 4, ts)}
	const serverCaps = uint32(mysql.CLIENT_PROTOCOL_41|mysql.CLIENT_PLUGIN_AUTH|mysql.CLIENT_SECURE_CONNECTION) | wire.CLIENT_DEPRECATE_EOF
	for _, deprecateEOF := range []bool{false, true} {
		t.Run(fmt.Sprintf("deprecateEOF=%v", deprecateEOF), func(t *testing.T) {
			mm := stageOwn(t, all, all, true)
			ctx, dctx := poolConn("c1")
			clientCaps := serverCaps
			if !deprecateEOF {
				clientCaps &^= wire.CLIENT_DEPRECATE_EOF
			}
			dctx.ServerCaps, dctx.ClientCaps = serverCaps, clientCaps
			resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), prepareReq("UPDATE kv SET v = ? WHERE id = ?"), mm, dctx)
			if err != nil || !ok {
				t.Fatalf("match: ok %v, miss %q, err %v; want the PREPARE_OK the matcher makes", ok, miss, err)
			}
			conn, peer := net.Pipe()
			t.Cleanup(func() { _ = conn.Close(); _ = peer.Close() })
			dctx.LastOp, dctx.ServerGreetings = wire.NewLastOpMap(), wire.NewGreetings()
			dctx.ServerGreetings.Store(conn, &mysql.HandshakeV10Packet{CapabilityFlags: serverCaps})
			b, err := wire.EncodeToBinary(ctx, zap.NewNop(), &resp.PacketBundle, conn, dctx)
			if err != nil {
				t.Fatalf("encode the PREPARE_OK: %v", err)
			}
			// The packets a client reads: the PREPARE_OK, a definition per
			// parameter, and, without CLIENT_DEPRECATE_EOF, the EOF.
			var payloads [][]byte
			for seq := byte(1); len(b) > 0; seq++ {
				if len(b) < 4 {
					t.Fatalf("%d bytes after %d packets: not a packet", len(b), len(payloads))
				}
				n := int(b[0]) | int(b[1])<<8 | int(b[2])<<16
				if len(b) < 4+n || b[3] != seq {
					t.Fatalf("packet %d: length %d of %d bytes left, sequence %d; want sequence %d", len(payloads), n, len(b)-4, b[3], seq)
				}
				payloads = append(payloads, b[4:4+n])
				b = b[4+n:]
			}
			want := 3
			if !deprecateEOF {
				want = 4
			}
			if len(payloads) != want {
				t.Fatalf("the PREPARE_OK is %d packets; want %d (PREPARE_OK, 2 parameter definitions%s)", len(payloads), want,
					map[bool]string{false: ", EOF", true: ""}[deprecateEOF])
			}
			if p := payloads[0]; len(p) < 9 || p[0] != 0x00 || p[7] != 2 || p[8] != 0 {
				t.Fatalf("the first packet %x is not a PREPARE_OK of 2 parameters", p)
			}
			for i, p := range payloads[1:3] {
				if len(p) == 0 || p[0] == mysql.EOF {
					t.Fatalf("parameter definition %d is %x", i, p)
				}
			}
			if !deprecateEOF {
				if p := payloads[3]; len(p) != 5 || p[0] != mysql.EOF {
					t.Fatalf("the last packet %x is not the EOF that ends the parameter definitions", p)
				}
			}
		})
	}
}
