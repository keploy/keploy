package replayer_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/pkg/service/record"
	"go.uber.org/zap"
)

// rollFeed is an agent the row hands its captures to, one at a time.
type rollFeed struct {
	incoming chan *models.TestCase
	outgoing chan *models.Mock
	mappings chan models.TestMockMapping
}

func (f *rollFeed) Setup(context.Context, string, models.SetupOptions) error { return nil }
func (f *rollFeed) GetIncoming(context.Context, models.IncomingOptions) (<-chan *models.TestCase, error) {
	return f.incoming, nil
}
func (f *rollFeed) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	return f.outgoing, nil
}
func (f *rollFeed) GetMappings(context.Context, models.IncomingOptions) (<-chan models.TestMockMapping, error) {
	return f.mappings, nil
}
func (f *rollFeed) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}
func (f *rollFeed) MakeAgentReadyForDockerCompose(context.Context) error { return nil }
func (f *rollFeed) NotifyGracefulShutdown(context.Context) error         { return nil }
func (f *rollFeed) StreamPcapArtifacts(context.Context, string) error    { return nil }

type rollTelemetry struct{}

func (rollTelemetry) RecordedTestSuite(string, int, map[string]int, map[string]interface{}) {}
func (rollTelemetry) RecordedTestCaseMock(string)                                           {}
func (rollTelemetry) RecordedMocks(map[string]int)                                          {}
func (rollTelemetry) RecordedTestAndMocks()                                                 {}
func (rollTelemetry) RecordSessionCompleted(int64, int64, int64, string, string)            {}

// rollTo is a Roller with a limit of a few kilobytes.
type rollTo struct {
	mu   sync.Mutex
	sets []string
}

func (r *rollTo) Limit() int64 { return 4096 }
func (r *rollTo) Next(_ context.Context, cur string) (string, error) {
	return cur + "-s1", nil
}
func (r *rollTo) Rolled(prev, next string, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sets = append(r.sets, prev, next)
}
func (r *rollTo) rolled() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sets...)
}

// A prepared statement a connection made before a recording rolled over to a
// new test set, and executed by a test after it, replays from the new set
// alone: the new set starts with the connection's PREPARE (a connection-
// lifetime mock), so the statement's PREPARE and its EXECUTE are both
// answered from it. Without the PREPARE in the new set, the replayed
// application's PREPARE finds nothing to answer it, and the test fails.
func TestARolledOverSetAloneAnswersAPrepareMadeBeforeItsCut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	logger := zap.NewNop()
	feed := &rollFeed{
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
		mappings: make(chan models.TestMockMapping),
	}
	roller := &rollTo{}
	rec := record.New(logger, testdb.New(logger, dir), mockdb.New(logger, dir, "mocks"), mapdb.New(logger, dir, "mappings"),
		rollTelemetry{}, feed, nil, record.BaseRecordHooks{}, &config.Config{}).(*record.Recorder)
	rec.SetRoller(roller)
	recCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- rec.Start(recCtx) }()

	send := func(what string, ch any, v any) {
		t.Helper()
		ok := false
		switch c := ch.(type) {
		case chan *models.Mock:
			select {
			case c <- v.(*models.Mock):
				ok = true
			case <-time.After(10 * time.Second):
			}
		case chan *models.TestCase:
			select {
			case c <- v.(*models.TestCase):
				ok = true
			case <-time.After(10 * time.Second):
			}
		case chan models.TestMockMapping:
			select {
			case c <- v.(models.TestMockMapping):
				ok = true
			case <-time.After(10 * time.Second):
			}
		}
		if !ok {
			t.Fatalf("the recorder never took %s", what)
		}
	}
	tcAt := func(name string, req time.Time) *models.TestCase {
		return &models.TestCase{Version: models.GetVersion(), Name: name, Kind: models.HTTP,
			HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://app/" + name, Timestamp: req},
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: "ok", Timestamp: req.Add(4 * time.Millisecond)}}
	}

	t0 := diffBase
	prep := diffPrepareMock("prep-c1", "c1", diffQueryA, 7, t0)
	send("the PREPARE", feed.outgoing, prep)
	t1 := t0.Add(10 * time.Millisecond)
	send("test-0's EXECUTE", feed.outgoing, diffMock("exec-0", "c1", []mysql.Request{executeReq(7, "0")}, t1.Add(time.Millisecond), models.LifetimePerTest, "mocks"))
	send("test-0", feed.incoming, tcAt("test-0", t1))
	send("test-0's mapping", feed.mappings, models.TestMockMapping{TestName: "test-0", MockIDs: []string{"exec-0"}})
	// A large call of another kind fills the set: the cut is at its time.
	cut := t0.Add(20 * time.Millisecond)
	fill := &models.Mock{Version: models.GetVersion(), Name: "fill", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "mocks"},
		HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://blob/big", Timestamp: cut},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: strings.Repeat("f", 8192), Timestamp: cut.Add(time.Millisecond)},
		ReqTimestampMock: cut, ResTimestampMock: cut.Add(time.Millisecond),
	}}
	send("the filling call", feed.outgoing, fill)
	deadline := time.Now().Add(10 * time.Second)
	for len(roller.rolled()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the recording did not roll over")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// After the cut, test-1 executes the statement prepared before it.
	t2 := cut.Add(10 * time.Millisecond)
	send("test-1's EXECUTE", feed.outgoing, diffMock("exec-1", "c1", []mysql.Request{executeReq(7, "1")}, t2.Add(time.Millisecond), models.LifetimePerTest, "mocks"))
	send("test-1", feed.incoming, tcAt("test-1", t2))
	send("test-1's mapping", feed.mappings, models.TestMockMapping{TestName: "test-1", MockIDs: []string{"exec-1"}})
	time.Sleep(5 * time.Second) // two of the mapping consumer's flushes
	stop()
	close(feed.outgoing)
	close(feed.incoming)
	close(feed.mappings)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return")
	}

	sets := roller.rolled()
	setN := sets[1]
	mocks, err := mockdb.New(logger, dir, "mocks").GetUnFilteredMocks(ctx, setN, time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := mapdb.New(logger, dir, "mappings").Get(ctx, setN)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || len(m["test-1"]) != 1 {
		t.Fatalf("%s maps %v; want test-1 and its EXECUTE", setN, m)
	}
	// Set N holds the connection's PREPARE (carried) and test-1's EXECUTE,
	// and no other MySQL call: test-0's EXECUTE stays before the cut.
	byCommand := map[string]int{}
	for _, mk := range mocks {
		if mk.Kind == models.MySQL && len(mk.Spec.MySQLRequests) > 0 && mk.Spec.MySQLRequests[0].Header != nil {
			byCommand[mk.Spec.MySQLRequests[0].Header.Type]++
		}
	}
	if byCommand["COM_STMT_PREPARE"] != 1 || byCommand["COM_STMT_EXECUTE"] != 1 || len(byCommand) != 2 {
		t.Fatalf("%s holds MySQL calls %v; want the PREPARE and test-1's EXECUTE", setN, byCommand)
	}

	// Replay test-1 from set N alone, as a replay of it stages the test:
	// the per-test mocks of its window, and the reusable ones.
	var perTest, reusable []*models.Mock
	for _, mk := range mocks {
		if mk.TestModeInfo.Lifetime != models.LifetimePerTest || mk.Spec.Metadata["type"] == "config" {
			reusable = append(reusable, mk)
			continue
		}
		perTest = append(perTest, mk)
	}
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(perTest, reusable, t2, t2.Add(4*time.Millisecond))
	connCtx := context.WithValue(ctx, models.ClientConnectionIDKey, "c1")
	dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}, NextStmtID: 100}
	resp, ok, miss, err := replayer.MatchForTest(connCtx, zap.NewNop(), prepareReq(diffQueryA), mm, dctx)
	if err != nil || !ok || resp == nil {
		t.Fatalf("set %s alone does not answer the PREPARE made before its cut: ok=%v err=%v miss=%s", setN, ok, err, miss)
	}
	if resp.Header == nil || resp.Header.Type != mysql.COM_STMT_PREPARE_OK {
		t.Fatalf("the PREPARE was answered with %+v; want the connection's PREPARE OK", resp.Header)
	}
	// The statement is the connection's now, as the PREPARE's answer made it.
	dctx.StmtIDToQuery[7] = diffQueryA
	resp, ok, miss, err = replayer.MatchForTest(connCtx, zap.NewNop(), executeReq(7, "1"), mm, dctx)
	if err != nil || !ok || resp == nil {
		t.Fatalf("set %s alone does not answer test-1's EXECUTE: ok=%v err=%v miss=%s", setN, ok, err, miss)
	}
	if resp.Header == nil || resp.Header.Type != "TextResultSet" {
		t.Fatalf("test-1's EXECUTE was answered with %+v; want its recorded result set", resp.Header)
	}
}
