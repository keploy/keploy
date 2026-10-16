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
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// A scoped worker's test run again in the same agent (a retry, -count=2, a
// watch-mode rerun) is served its own MySQL recordings again, those its first
// run consumed included; within one run each is served once.
func TestAScopedTestRunAgainIsServedItsMySQLRecordingsAgain(t *testing.T) {
	const sql = "SELECT v FROM kv WHERE id = 1"
	read := diffMock("read-in-a", "c1", []mysql.Request{queryReq(sql)}, diffBase.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	read.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
	read.TestModeInfo = models.TestModeInfo{}
	read.DeriveLifetime()
	if read.TestModeInfo.Lifetime != models.LifetimePerTest {
		t.Fatalf("the recording derived %v, want per-test", read.TestModeInfo.Lifetime)
	}
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.BeginTestSet(time.Time{}, nil, []*models.Mock{read}, nil)
	p := &proxy.Proxy{}
	p.SetMappedUniverse([]string{"read-in-a"})
	const worker = 4101
	conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
	serve := func(db integrations.MockMemDb) string {
		t.Helper()
		dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
		resp, ok, _, err := replayer.MatchCommandForTest(conn, zap.NewNop(), queryReq(sql), db, dctx, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || resp == nil {
			return "MISS"
		}
		return resp.Payload
	}
	for run := 1; run <= 2; run++ {
		p.SetWorkerScope(worker, []string{"read-in-a"})
		view := p.WorkerViewForTest(worker, mm)
		got := serve(view) + "," + serve(view)
		if !strings.HasPrefix(got, "read-in-a,") || strings.HasSuffix(got, ",read-in-a") {
			t.Fatalf("run %d was served %s, want its recording once", run, got)
		}
		p.ClearWorkerScope(worker)
		// Between the runs, on the connection the test opened: no answer.
		if got := serve(view); got != "MISS" {
			t.Fatalf("a call after run %d was served %s, want a miss", run, got)
		}
	}
}
