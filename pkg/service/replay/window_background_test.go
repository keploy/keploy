package replay

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

type windowBgMockDB struct {
	mocks []*models.Mock
}

func (db *windowBgMockDB) GetFilteredMocks(_ context.Context, _ string, _, _ time.Time, _, _ map[string]bool) ([]*models.Mock, error) {
	out := make([]*models.Mock, 0, len(db.mocks))
	for _, m := range db.mocks {
		out = append(out, m.DeepCopy())
	}
	return out, nil
}

func (db *windowBgMockDB) GetUnFilteredMocks(_ context.Context, _ string, _, _ time.Time, _, _ map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}

func (db *windowBgMockDB) UpdateMocks(_ context.Context, _ string, _ map[string]models.MockState, _, _ time.Time) error {
	return nil
}

func makeTestCaseWindow(name string, start, end time.Time) *models.TestCase {
	return &models.TestCase{
		Name: name,
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method:    "POST",
			URL:       "http://localhost:8080/query",
			Timestamp: start,
		},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Timestamp:  end,
		},
	}
}

func makeMongoPollMock(name, collection string, req, res time.Time) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":                     "mocks",
				models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
				"destAddr":                 "mongo-cluster-rs1-0:27017",
				"operation":                "findAndModify",
			},
			ReqTimestampMock: req,
			ResTimestampMock: res,
			MongoRequests: []models.MongoRequest{
				{
					Message: &models.MongoOpMessage{
						Sections: []string{`msg:{"findAndModify":"` + collection + `","query":{"dirtyAt":{"$lte":1795784500}}}`},
					},
				},
			},
		},
	}
}

func makeHTTP2SQSMock(name, target string, req, res time.Time) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.HTTP2,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":                     "mocks",
				models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
				"destAddr":                 "sqs.us-east-1.amazonaws.com:443",
			},
			ReqTimestampMock: req,
			ResTimestampMock: res,
			HTTP2Req: &models.HTTP2Req{
				Method:    "POST",
				Authority: "sqs.us-east-1.amazonaws.com",
				URL:       "/",
				Headers: map[string]string{
					"x-amz-target": target,
				},
				Body: `{"QueueUrl":"https://sqs.us-east-1.amazonaws.com/123/q"}`,
			},
		},
	}
}

func TestPopulateWindowDerivedBackground_SplitsStartupAndWorkerAndStaysFailClosedInWindow(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	// Two recorded test windows: [t0+10s, t0+12s] and [t0+20s, t0+22s].
	tcs := []*models.TestCase{
		makeTestCaseWindow("test-1", t0.Add(10*time.Second), t0.Add(12*time.Second)),
		makeTestCaseWindow("test-2", t0.Add(20*time.Second), t0.Add(22*time.Second)),
	}

	mocks := []*models.Mock{
		// Strictly before first test -> startup:
		makeMongoPollMock("boot-ptw-1", "ptw_endpoints", t0.Add(2*time.Second), t0.Add(2*time.Second+5*time.Millisecond)),
		// Strictly inside test-1 window -> MUST NEVER be promoted (fail-closed)
		makeMongoPollMock("in-window-uitestruns", "uiTestRuns", t0.Add(11*time.Second), t0.Add(11*time.Second+5*time.Millisecond)),
		// Boundary: starts exactly at test-1 start -> inside window -> MUST NOT be promoted
		makeMongoPollMock("at-boundary-start", "users", t0.Add(10*time.Second), t0.Add(10*time.Second+5*time.Millisecond)),
		// Boundary: starts exactly at test-1 end -> inside window -> MUST NOT be promoted
		makeMongoPollMock("at-boundary-end", "users", t0.Add(12*time.Second), t0.Add(12*time.Second+5*time.Millisecond)),
		// Between test-1 and test-2 -> worker:
		makeMongoPollMock("between-ptw-1", "ptw_endpoints", t0.Add(15*time.Second), t0.Add(15*time.Second+5*time.Millisecond)),
		makeHTTP2SQSMock("between-sqs-recv", "AmazonSQS.ReceiveMessage", t0.Add(16*time.Second), t0.Add(16*time.Second+5*time.Millisecond)),
		// Outside window, BUT already owned by a passing test in mappingDoc -> MUST NOT be promoted
		makeMongoPollMock("owned-by-test", "ptw_endpoints", t0.Add(17*time.Second), t0.Add(17*time.Second+5*time.Millisecond)),
		// After test-2 -> worker:
		makeMongoPollMock("after-ptw-2", "ptw_endpoints", t0.Add(25*time.Second), t0.Add(25*time.Second+5*time.Millisecond)),
	}

	r := &Replayer{
		logger: zap.NewNop(),
		mockDB: &windowBgMockDB{mocks: mocks},
	}

	mappingDoc := &models.Mapping{
		TestSetID: "ts-0",
		TestCases: []models.MappedTestCase{
			{
				ID:    "test-1",
				Mocks: []models.MockEntry{{Name: "owned-by-test", Kind: string(models.Mongo)}},
			},
		},
	}

	r.populateWindowDerivedBackground(context.Background(), "ts-0", tcs, mappingDoc, mappingDoc)

	// Verify startup: contains only boot-ptw-1 with legible window-derived reason.
	if len(mappingDoc.Startup) != 1 || mappingDoc.Startup[0].Name != "boot-ptw-1" {
		t.Fatalf("Startup = %+v, want [boot-ptw-1]", mappingDoc.Startup)
	}
	if mappingDoc.Startup[0].Reason != WindowDerivedStartupReason {
		t.Fatalf("Startup[0].Reason = %q, want %q", mappingDoc.Startup[0].Reason, WindowDerivedStartupReason)
	}

	// Verify worker: contains between-ptw-1, between-sqs-recv, after-ptw-2 and NOTHING else.
	wantWorker := []string{"between-ptw-1", "between-sqs-recv", "after-ptw-2"}
	if len(mappingDoc.Worker) != len(wantWorker) {
		t.Fatalf("Worker = %+v, want %v", mappingDoc.Worker, wantWorker)
	}
	for i, want := range wantWorker {
		got := mappingDoc.Worker[i]
		if got.Name != want {
			t.Fatalf("Worker[%d].Name = %q, want %q (all=%+v)", i, got.Name, want, mappingDoc.Worker)
		}
		if got.Reason != WindowDerivedWorkerReason {
			t.Fatalf("Worker[%d].Reason = %q, want %q", i, got.Reason, WindowDerivedWorkerReason)
		}
		if !strings.Contains(got.Reason, "outside every recorded test window") {
			t.Fatalf("Worker[%d].Reason = %q lacks window evidence", i, got.Reason)
		}
	}

	// Verify Pass 2 backgroundMocksOf reads BOTH persisted startup: and worker: entries.
	r.mappingDB = &shMappingDB{prMappingDB: &prMappingDB{}, mapping: mappingDoc}
	all, _, derive := r.backgroundMocksOf(context.Background(), "ts-0")
	if derive {
		t.Fatal("Pass 2 with persisted mappings.yaml unexpectedly set derive=true")
	}
	gotSet := make(map[string]bool, len(all))
	for _, n := range all {
		gotSet[n] = true
	}
	for _, want := range []string{"boot-ptw-1", "between-ptw-1", "between-sqs-recv", "after-ptw-2"} {
		if !gotSet[want] {
			t.Fatalf("Pass 2 backgroundMocksOf missing %q (got=%v)", want, all)
		}
	}
	for _, forbidden := range []string{"in-window-uitestruns", "at-boundary-start", "at-boundary-end", "owned-by-test"} {
		if gotSet[forbidden] {
			t.Fatalf("Pass 2 backgroundMocksOf must NOT include %q (got=%v)", forbidden, all)
		}
	}
}

// End-to-end Pass 1 -> Pass 2 test through real RunTestSet + real managed agent:
//   - Pass 1 (no mappings.yaml): creates mappings.yaml with startup: (boot) and
//     worker: (between/after windows), while leaving in-window mocks out.
//   - Pass 2 (loads Pass 1's mappings.yaml): DeriveBackgroundFromWindows=false,
//     yet the agent STILL installs both startup: and worker: mocks into
//     MockManager.BackgroundRecordings (no Pass 1 -> Pass 2 amnesia).
//   - Existing mappings.yaml (where operator removed worker:): re-running with
//     UpdateTestMapping=true does NOT re-populate worker: from windows.
func TestRunTestSet_Pass1PersistsWindowBackgroundForPass2AndRespectsExistingMappings(t *testing.T) {
	t.Setenv(provenswitch.Env, "")

	// pinWindows sets test-1=[attrAt(1000), attrAt(1100)], test-2=[attrAt(2000), attrAt(2100)].
	recording := []*models.Mock{
		makeMongoPollMock("boot-ptw-1", "ptw_endpoints", attrAt(500), attrAt(505)),
		makeMongoPollMock("in-test-1", "users", attrAt(1050), attrAt(1055)),
		makeMongoPollMock("between-ptw-1", "ptw_endpoints", attrAt(1500), attrAt(1505)),
		makeHTTP2SQSMock("between-sqs-1", "AmazonSQS.ReceiveMessage", attrAt(1600), attrAt(1605)),
		makeMongoPollMock("in-test-2", "orders", attrAt(2050), attrAt(2055)),
		makeMongoPollMock("after-ptw-1", "ptw_endpoints", attrAt(2500), attrAt(2505)),
	}

	// --- Pass 1: no mappings.yaml yet (default create-if-not-present path) ---
	h1 := newPartialRunHarness(t, 2, 0)
	pinWindows(h1)
	h1.replayer.config.Test.StrictMockWindow = true
	p1Map := &shMappingDB{prMappingDB: &prMappingDB{exists: false}, mapping: nil}
	h1.replayer.mappingDB = p1Map
	h1.replayer.mockDB = &allMocksDB{recording: recording}
	// Simulate test-1 consuming in-test-1 and test-2 consuming in-test-2 so Pass 1
	// creates mappings.yaml via the default create-if-not-present path.
	h1.replayer.hookImpl = prHooks{
		consumedFor: consumedByWindow(h1, map[string][]models.MockState{
			"test-1": {{Name: "in-test-1", Kind: models.Mongo, Timestamp: attrAt(1050).Unix()}},
			"test-2": {{Name: "in-test-2", Kind: models.Mongo, Timestamp: attrAt(2050).Unix()}},
		}),
	}
	agent1 := newManagedAgent(t, h1.instr)
	h1.replayer.instrumentation = agent1
	if status := h1.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("Pass 1 status = %q, want PASSED", status)
	}
	written := p1Map.last
	if written == nil {
		t.Fatal("Pass 1 did not write mappings.yaml")
	}
	if len(written.Startup) != 1 || written.Startup[0].Name != "boot-ptw-1" {
		t.Fatalf("Pass 1 written Startup = %+v, want [boot-ptw-1]", written.Startup)
	}
	wantWorker := []string{"between-ptw-1", "between-sqs-1", "after-ptw-1"}
	if len(written.Worker) != len(wantWorker) {
		t.Fatalf("Pass 1 written Worker = %+v, want %v", written.Worker, wantWorker)
	}
	for i, w := range wantWorker {
		if written.Worker[i].Name != w {
			t.Fatalf("Pass 1 written Worker[%d] = %q, want %q", i, written.Worker[i].Name, w)
		}
	}

	// --- Pass 2: replay with Pass 1's written mappings.yaml ---
	h2 := newPartialRunHarness(t, 2, 0)
	pinWindows(h2)
	h2.replayer.config.Test.StrictMockWindow = true
	p2Map := &shMappingDB{prMappingDB: &prMappingDB{exists: true}, mapping: written}
	h2.replayer.mappingDB = p2Map
	h2.replayer.mockDB = &allMocksDB{recording: recording}
	agent2 := newManagedAgent(t, h2.instr)
	h2.replayer.instrumentation = agent2
	if status := h2.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("Pass 2 status = %q, want PASSED", status)
	}
	gotMongo := agent2.mm.BackgroundRecordings(models.Mongo)
	gotHTTP2 := agent2.mm.BackgroundRecordings(models.HTTP2)
	bgNames := map[string]bool{}
	for _, r := range append(gotMongo, gotHTTP2...) {
		bgNames[r.Mock.Name] = true
	}
	for _, want := range []string{"boot-ptw-1", "between-ptw-1", "between-sqs-1", "after-ptw-1"} {
		if !bgNames[want] {
			t.Fatalf("Pass 2 agent missing background recording %q (got=%v)", want, bgNames)
		}
	}
	for _, forbidden := range []string{"in-test-1", "in-test-2"} {
		if bgNames[forbidden] {
			t.Fatalf("Pass 2 agent must NOT promote in-window mock %q (got=%v)", forbidden, bgNames)
		}
	}

	// --- Pass 3: existing mappings.yaml where operator cleared worker: ---
	edited := &models.Mapping{
		TestSetID: "test-set-0",
		Startup:   []models.MockEntry{{Name: "boot-ptw-1"}},
		Worker:    nil, // operator intentionally removed worker: entries
		TestCases: written.TestCases,
	}
	h3 := newPartialRunHarness(t, 2, 0)
	pinWindows(h3)
	h3.replayer.config.Test.StrictMockWindow = true
	h3.replayer.config.Test.UpdateTestMapping = true
	p3Map := &shMappingDB{prMappingDB: &prMappingDB{exists: true}, mapping: edited}
	h3.replayer.mappingDB = p3Map
	h3.replayer.mockDB = &allMocksDB{recording: recording}
	h3.replayer.hookImpl = prHooks{
		consumedFor: consumedByWindow(h3, map[string][]models.MockState{
			"test-1": {{Name: "in-test-1", Kind: models.Mongo, Timestamp: attrAt(1050).Unix()}},
			"test-2": {{Name: "in-test-2", Kind: models.Mongo, Timestamp: attrAt(2050).Unix()}},
		}),
	}
	if status := h3.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("Pass 3 status = %q, want PASSED", status)
	}
	if p3Map.last == nil {
		t.Fatal("Pass 3 with UpdateTestMapping=true did not call Insert")
	}
	if len(p3Map.last.Worker) != 0 {
		t.Fatalf("Pass 3 re-populated Worker on an EXISTING mappings.yaml: %+v", p3Map.last.Worker)
	}
}

func TestWindowBgCollector_BoundsPerShapeAndSeparatesHTTP2AmzTargets(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	col := newWindowBgCollector(windowDerivedMaxPerTier, windowDerivedMaxPerShape, WindowDerivedWorkerReason)

	// Push 12 ReceiveMessage and 2 DeleteMessage calls to the SAME HTTP/2 POST / authority.
	for i := 0; i < 12; i++ {
		at := t0.Add(time.Duration(i) * time.Second)
		col.add(makeHTTP2SQSMock("recv-"+string(rune('a'+i)), "AmazonSQS.ReceiveMessage", at, at.Add(time.Millisecond)))
	}
	for i := 0; i < 2; i++ {
		at := t0.Add(time.Duration(20+i) * time.Second)
		col.add(makeHTTP2SQSMock("del-"+string(rune('a'+i)), "AmazonSQS.DeleteMessage", at, at.Add(time.Millisecond)))
	}

	entries := col.entries()
	var recvCount, delCount int
	for _, e := range entries {
		if strings.HasPrefix(e.Name, "recv-") {
			recvCount++
		}
		if strings.HasPrefix(e.Name, "del-") {
			delCount++
		}
	}
	if recvCount != windowDerivedMaxPerShape {
		t.Fatalf("ReceiveMessage entries = %d, want capped at %d", recvCount, windowDerivedMaxPerShape)
	}
	if delCount != 2 {
		t.Fatalf("DeleteMessage entries = %d, want 2 (distinct X-Amz-Target must not share ReceiveMessage bucket)", delCount)
	}
}
