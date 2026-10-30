package replayer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// servedOn serves one command, live, through the MySQL matcher, as the
// command phase matches it, from a store that holds recs and tells tracker
// of each consume as the agent's does; dctx maps the connection's
// statement ids. It returns the recording read with the view the serve
// stated (learn.Order.ServedView), in the next test of the set, as a later
// call reads it.
func servedOn(t *testing.T, tracker *learn.Tracker, recs []*models.Mock, served *models.Mock, live mysql.Request, dctx *wire.DecodeContext) (learn.Raw, *mysql.Response) {
	t.Helper()
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetServedObserver(tracker.ServedMock)
	mm.SetServedInObserver(tracker.ServedMockIn)
	mm.SetMocksWithWindow(nil, recs, models.BaseTime, time.Now().Add(time.Hour))
	tracker.Begin()
	in := tracker.Lift()
	ctx := learn.WithTracker(context.Background(), tracker)
	ctx = integrations.WithRequestHolder(context.WithValue(ctx, models.ClientConnectionIDKey, "c1"), aloft{in})
	resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), live, mm, dctx)
	in.Land()
	// A command the server does not answer gets nothing back.
	answer := served.Name
	if len(served.Spec.MySQLResponses) == 0 {
		answer = ""
	}
	if err != nil || !ok || resp.Payload != answer {
		t.Fatalf("the command was not answered with %s: ok=%v err=%v miss=%s", served.Name, ok, err, miss)
	}
	tracker.End()
	tracker.Begin()
	order := tracker.Placed().Order
	// A protocol phase (a connection's PREPARE) is numbered with no place
	// in F's order (learn.Tracker.ServedMockIn): read as served all the same.
	first, last := order.Of(served)
	if (first == 0 || last == 0) && !served.IsProtocolPhase() {
		t.Fatalf("%s is at %d,%d: its serve was not numbered", served.Name, first, last)
	}
	return order.ServedView(served, learn.RawOf(served, true).ServedIn(first, last)), resp
}

// statementCtx is a connection that prepared query as statement id.
func statementCtx(id uint32, query string) *wire.DecodeContext {
	return &wire.DecodeContext{
		PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{},
		StmtIDToQuery:      map[uint32]string{id: query},
		NextStmtID:         id + 1,
	}
}

// Every serve the MySQL command phase makes of a recording under the proven
// switch states what it gave the application (learn.Tracker.
// ServedMockAsRecorded where the command was its recording's and F wrote
// the recorded answer, else learn.Tracker.ServedMockLive with the command
// as the application sent it and the answer F writes): a query sent as
// recorded, an EXECUTE of a statement this connection prepared under
// another id than record's, and a PREPARE F answers with this connection's
// next id. A later call of the set then reads the recording with that view:
// a changed value that holds no word of it is learned; one that holds a
// word only the live command or F's answer held there is refused, naming
// the recording at that place. Before, the matcher's consume numbered the
// serve and no statement followed, so the later call refused every changed
// value of the set ("its adapter did not say what it gave the application
// there"): one MySQL query anywhere earlier in a set refused every learned
// value after it.
func TestEveryMySQLCommandServeStatesTheViewItGave(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	const (
		unstated = "where its adapter did not say what it gave the application there"
		path     = "body.ref"
		recorded = "82"
		fresh    = "9f3e"
		query    = "SELECT v FROM kv WHERE id = ?"
	)
	at := diffBase.Add(time.Millisecond)
	for _, tc := range []struct {
		name       string
		serve      string // "query", "execute", "prepare"
		value      string // the later call's changed value
		asRecorded bool
		why        string // "" learns; else a part of the refusal
	}{
		{"a query sent as recorded, a fresh value", "query", fresh, true, ""},
		{"an execute under this connection's statement id, a fresh value", "execute", fresh, false, ""},
		{"an execute under this connection's statement id, the id only it sent", "execute", "4242", false, "as record's full round gave it"},
		{"a close under this connection's statement id, a fresh value", "close", fresh, false, ""},
		{"a close under this connection's statement id, the id only it sent", "close", "4242", false, "as record's full round gave it"},
		{"a long data chunk under this connection's statement id, a fresh value", "longdata", fresh, false, ""},
		{"a long data chunk under this connection's statement id, the id only it sent", "longdata", "4242", false, "as record's full round gave it"},
		{"a prepare answered with this connection's statement id, a fresh value", "prepare", fresh, false, ""},
		{"a prepare answered with this connection's statement id, the id only F's answer held", "prepare", "4242", false, "as record's full round gave it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				served *models.Mock
				recs   []*models.Mock
				live   mysql.Request
				dctx   *wire.DecodeContext
			)
			switch tc.serve {
			case "query":
				served = diffMock("kv", "c1", []mysql.Request{queryReq("SELECT v FROM kv WHERE id = 1")}, at, models.LifetimeSession, "mocks")
				recs, live, dctx = []*models.Mock{served}, queryReq("SELECT v FROM kv WHERE id = 1"), statementCtx(1, query)
			case "execute":
				prep := diffPrepareMock("prep", "c1", query, 1, at)
				served = diffMock("exec", "c1", []mysql.Request{executeReq(1, "7")}, at.Add(time.Millisecond), models.LifetimeSession, "mocks")
				recs, live, dctx = []*models.Mock{prep, served}, executeReq(4242, "7"), statementCtx(4242, query)
			case "close":
				prep := diffPrepareMock("prep", "c1", query, 1, at)
				served = diffMock("close", "c1", []mysql.Request{closeReq(1)}, at.Add(time.Millisecond), models.LifetimeSession, "mocks")
				served.Spec.MySQLResponses = nil
				recs, live, dctx = []*models.Mock{prep, served}, closeReq(4242), statementCtx(4242, query)
			case "longdata":
				prep := diffPrepareMock("prep", "c1", query, 1, at)
				served = diffMock("sld", "c1", []mysql.Request{longDataReq(1, "chunk")}, at.Add(time.Millisecond), models.LifetimeSession, "mocks")
				served.Spec.MySQLResponses = nil
				recs, live, dctx = []*models.Mock{prep, served}, longDataReq(4242, "chunk"), statementCtx(4242, query)
			case "prepare":
				served = diffPrepareMock("prep", "c1", query, 1, at)
				recs, live, dctx = []*models.Mock{served}, prepareReq(query), statementCtx(4241, query)
			}
			tracker := learn.NewTracker()
			r, _ := servedOn(t, tracker, recs, served, live, dctx)
			rec := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: recorded}}}
			now := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: tc.value}}}
			got := learn.Classify(learn.NewDriftCache(), []string{path}, now, rec, time.Now(), nil, []learn.Raw{r}, nil)
			if len(got) != 1 {
				t.Fatalf("%d verdicts; want one", len(got))
			}
			switch {
			case tc.why == "" && got[0].Kind == learn.KindUnaccounted:
				t.Fatalf("FALSE RED: %s refused (%q); want learned: no word of it stands in what F gave at %s", tc.value, got[0].Why, served.Name)
			case tc.why != "" && got[0].Kind != learn.KindUnaccounted:
				t.Fatalf("FALSE GREEN: %s %s (%q); want refused, naming %s", tc.value, got[0].Kind, got[0].Why, tc.why)
			case tc.why != "" && (!strings.Contains(got[0].Why, served.Name) || !strings.Contains(got[0].Why, tc.why) || strings.Contains(got[0].Why, unstated)):
				t.Fatalf("FALSE REASON: %s refused (%q); want it to name %s by what F gave there: %s", tc.value, got[0].Why, served.Name, tc.why)
			}
			if r.AsRecorded() != tc.asRecorded || r.Live() == tc.asRecorded {
				t.Fatalf("%s read as recorded %v, with a view %v; want as recorded %v", served.Name, r.AsRecorded(), r.Live(), tc.asRecorded)
			}
		})
	}
}

// longDataReq is a COM_STMT_SEND_LONG_DATA of statement id's first
// parameter, carrying data.
func longDataReq(id uint32, data string) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(data) + 7)}, Type: "COM_STMT_SEND_LONG_DATA"},
		Message: &mysql.StmtSendLongDataPacket{Status: 0x18, StatementID: id, ParameterID: 0, Data: []byte(data)},
	}}
}

// With the proven switch off the command phase states nothing, and serves
// as it did: the same answer, and the recording read as a serve no adapter
// stated (which no learn step reads with the switch off).
func TestAMySQLServeStatesNothingWithTheSwitchOff(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "0")
	const query = "SELECT v FROM kv WHERE id = ?"
	at := diffBase.Add(time.Millisecond)
	served := diffPrepareMock("prep", "c1", query, 1, at)
	r, resp := servedOn(t, learn.NewTracker(), []*models.Mock{served}, served, prepareReq(query), statementCtx(4241, query))
	if r.AsRecorded() || r.Live() {
		t.Fatalf("%s read as recorded %v, with a view %v; want nothing stated with the switch off", served.Name, r.AsRecorded(), r.Live())
	}
	if ok, _ := resp.Message.(*mysql.StmtPrepareOkPacket); ok == nil || ok.StatementID != 4242 {
		t.Fatalf("the PREPARE was answered with %+v; want this connection's next statement id, 4242", resp.Message)
	}
}
