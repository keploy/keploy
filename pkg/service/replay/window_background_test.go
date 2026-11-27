package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	httppkg "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type windowBgMockDB struct {
	mocks []*models.Mock
	err   error
}

func (db *windowBgMockDB) GetFilteredMocks(_ context.Context, _ string, _, _ time.Time, _, _ map[string]bool) ([]*models.Mock, error) {
	if db.err != nil {
		return nil, db.err
	}
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

type pruningWindowMockDB struct {
	prMockDB
	mu        sync.Mutex
	recording []*models.Mock
	pruned    []string
}

func (m *pruningWindowMockDB) GetFilteredMocks(_ context.Context, _ string, _, _ time.Time, _, _ map[string]bool) ([]*models.Mock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*models.Mock, 0, len(m.recording))
	for _, mk := range m.recording {
		out = append(out, mk.DeepCopy())
	}
	return out, nil
}

func (m *pruningWindowMockDB) UpdateMocks(_ context.Context, _ string, keep map[string]models.MockState, pruneBefore, cutoff time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.recording[:0]
	for _, mk := range m.recording {
		_, named := keep[mk.Name]
		at := mk.Spec.ReqTimestampMock
		if named || at.After(pruneBefore) || (!cutoff.IsZero() && at.Before(cutoff)) {
			kept = append(kept, mk)
			continue
		}
		m.pruned = append(m.pruned, mk.Name)
	}
	m.recording = kept
	return nil
}

type existsErrMappingDB struct {
	*shMappingDB
	existsErr error
}

func (db *existsErrMappingDB) Exists(_ context.Context, _ string) (bool, error) {
	return false, db.existsErr
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

func makeHTTPPollMock(name, urlPath string, req, res time.Time) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":                     "mocks",
				models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
			},
			ReqTimestampMock: req,
			ResTimestampMock: res,
			HTTPReq: &models.HTTPReq{
				Method:     models.Method("GET"),
				ProtoMajor: 1,
				ProtoMinor: 1,
				URL:        "http://upstream.internal:8080" + urlPath,
				Header: map[string]string{
					"Connection": "close",
				},
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header:     map[string]string{"Content-Type": "application/json"},
				Body:       `{"ok":true}`,
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

func bgEntryNames(es []models.MockEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func TestPopulateWindowDerivedBackground_SplitsStartupAndWorkerAndStaysFailClosedInWindow(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	// Two recorded test windows: [t0+10s, t0+12s] and [t0+20s, t0+22s].
	// Note: test-2 is NOT in mappingDoc.TestCases (simulating a failing test
	// and the last window simultaneously — kills MX1 and MX2).
	tcs := []*models.TestCase{
		makeTestCaseWindow("test-1", t0.Add(10*time.Second), t0.Add(12*time.Second)),
		makeTestCaseWindow("test-2", t0.Add(20*time.Second), t0.Add(22*time.Second)),
	}

	mocks := []*models.Mock{
		// Strictly before first test -> startup:
		makeMongoPollMock("boot-ptw-1", "ptw_endpoints", t0.Add(2*time.Second), t0.Add(2*time.Second+5*time.Millisecond)),
		// Strictly inside test-1 window -> MUST NEVER be promoted (fail-closed)
		makeMongoPollMock("in-window-uitestruns", "uiTestRuns", t0.Add(11*time.Second), t0.Add(11*time.Second+5*time.Millisecond)),
		// Strictly inside failing / last test-2 window (unowned in mappingDoc) -> MUST NEVER be promoted (kills MX1 & MX2)
		makeMongoPollMock("in-last-failing-window", "uiTestRuns", t0.Add(21*time.Second), t0.Add(21*time.Second+5*time.Millisecond)),
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

	if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", tcs, mappingDoc, mappingDoc); err != nil {
		t.Fatalf("populateWindowDerivedBackground: %v", err)
	}

	// Verify startup: contains only boot-ptw-1 with legible window-derived reason.
	if len(mappingDoc.Startup) != 1 || mappingDoc.Startup[0].Name != "boot-ptw-1" {
		t.Fatalf("Startup = %+v, want [boot-ptw-1]", mappingDoc.Startup)
	}
	if mappingDoc.Startup[0].Reason != WindowDerivedStartupReason {
		t.Fatalf("Startup[0].Reason = %q, want %q", mappingDoc.Startup[0].Reason, WindowDerivedStartupReason)
	}

	// Verify worker: contains ONLY between-window mocks (between-ptw-1, between-sqs-recv);
	// after-ptw-2 (after the last held window) is NOT promoted (fail-closed B2).
	wantWorker := []string{"between-ptw-1", "between-sqs-recv"}
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
	for _, want := range []string{"boot-ptw-1", "between-ptw-1", "between-sqs-recv"} {
		if !gotSet[want] {
			t.Fatalf("Pass 2 backgroundMocksOf missing %q (got=%v)", want, all)
		}
	}
	for _, forbidden := range []string{"in-window-uitestruns", "in-last-failing-window", "at-boundary-start", "at-boundary-end", "owned-by-test", "after-ptw-2"} {
		if gotSet[forbidden] {
			t.Fatalf("Pass 2 backgroundMocksOf must NOT include %q (got=%v)", forbidden, all)
		}
	}
}

// B2 (R1): when TestDB reports an earlier RecordedFirstTestStart than the first
// held test case (earlier tests were removed/left out), a mock recorded inside
// that earlier window MUST NOT be promoted to startup: or worker:, while a mock
// recorded strictly before RecordedFirstTestStart still lands in startup: and a
// mock strictly between two held windows still lands in worker:.
func TestPopulateWindowDerivedBackground_MockOfRemovedTestWindowIsNotPromoted(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	// test-1 [t0+10s, t0+12s] was removed; only test-2 and test-3 remain.
	tcs := []*models.TestCase{
		makeTestCaseWindow("test-2", t0.Add(20*time.Second), t0.Add(22*time.Second)),
		makeTestCaseWindow("test-3", t0.Add(30*time.Second), t0.Add(32*time.Second)),
	}
	mocks := []*models.Mock{
		makeMongoPollMock("boot-ptw-1", "ptw_endpoints", t0.Add(2*time.Second), t0.Add(2*time.Second+5*time.Millisecond)),
		makeMongoPollMock("removed-test-uitestruns", "uiTestRuns", t0.Add(11*time.Second), t0.Add(11*time.Second+5*time.Millisecond)),
		makeMongoPollMock("between-held-ptw", "ptw_endpoints", t0.Add(25*time.Second), t0.Add(25*time.Second+5*time.Millisecond)),
		makeMongoPollMock("after-last-held-maybe-removed", "uiTestRuns", t0.Add(35*time.Second), t0.Add(35*time.Second+5*time.Millisecond)),
	}
	r := &Replayer{
		logger: zap.NewNop(),
		mockDB: &windowBgMockDB{mocks: mocks},
		testDB: &recordedStartDB{at: t0.Add(10 * time.Second)},
	}
	doc := &models.Mapping{TestSetID: "ts-0"}
	if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", tcs, doc, doc); err != nil {
		t.Fatalf("populateWindowDerivedBackground: %v", err)
	}
	if got := bgEntryNames(doc.Startup); len(got) != 1 || got[0] != "boot-ptw-1" {
		t.Fatalf("Startup = %v, want [boot-ptw-1]", got)
	}
	if got := bgEntryNames(doc.Worker); len(got) != 1 || got[0] != "between-held-ptw" {
		t.Fatalf("Worker = %v, want [between-held-ptw]", got)
	}
}

// S1 (R2): a parser-declared idle answer recorded INSIDE a test window must
// NEVER be given the false "outside every recorded test window" reason; it gets
// its own rule-D4 reason naming the window.
func TestPopulateWindowDerivedBackground_InWindowIdleAnswerGetsRuleD4Reason(t *testing.T) {
	unregister := models.RegisterIdleAnswer(models.HTTP2, func(m *models.Mock) bool { return m.Name == "idle-in-window" })
	t.Cleanup(unregister)

	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	tcs := []*models.TestCase{
		makeTestCaseWindow("test-1", t0.Add(10*time.Second), t0.Add(12*time.Second)),
		makeTestCaseWindow("test-2", t0.Add(20*time.Second), t0.Add(22*time.Second)),
	}
	mocks := []*models.Mock{
		makeHTTP2SQSMock("idle-in-window", "AmazonSQS.ReceiveMessage", t0.Add(11*time.Second), t0.Add(11*time.Second+5*time.Millisecond)),
	}
	r := &Replayer{logger: zap.NewNop(), mockDB: &windowBgMockDB{mocks: mocks}}
	doc := &models.Mapping{TestSetID: "ts-0"}
	if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", tcs, doc, doc); err != nil {
		t.Fatalf("populateWindowDerivedBackground: %v", err)
	}
	if len(doc.Worker) != 1 || doc.Worker[0].Name != "idle-in-window" {
		t.Fatalf("Worker = %+v, want [idle-in-window]", doc.Worker)
	}
	reason := doc.Worker[0].Reason
	if strings.Contains(reason, "outside every recorded test window") {
		t.Fatalf("in-window idle answer got false outside-window reason: %q", reason)
	}
	if !strings.Contains(reason, "rule D4") || !strings.Contains(reason, "test-1") {
		t.Fatalf("in-window idle answer reason = %q, want rule D4 + test-1", reason)
	}
}

// B4 + MX4 (R3): past maxPerTier (256), the shared DerivedBgCollector drops
// stale whole shapes by recency without ever cutting an active poller below
// learn.MinEpochReads+1, caps total entries at maxPerTier (kills MX4), and logs
// the cap drops at Info.
func TestPopulateWindowDerivedBackground_TierCapKeepsPollerEpochSamplesAndLogs(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	tcs := []*models.TestCase{
		makeTestCaseWindow("test-1", t0.Add(10*time.Second), t0.Add(12*time.Second)),
		makeTestCaseWindow("test-2", t0.Add(200*time.Second), t0.Add(202*time.Second)),
	}
	var mocks []*models.Mock
	for i := 0; i < 300; i++ {
		at := t0.Add(13*time.Second + time.Duration(i)*100*time.Millisecond)
		mocks = append(mocks, makeMongoPollMock(fmt.Sprintf("oneoff-%03d", i), fmt.Sprintf("coll_%03d", i), at, at.Add(time.Millisecond)))
	}
	for i := 0; i < 8; i++ {
		at := t0.Add(50*time.Second + time.Duration(i)*10*time.Second)
		mocks = append(mocks, makeMongoPollMock(fmt.Sprintf("poll-%d", i), "ptw_endpoints", at, at.Add(time.Millisecond)))
	}
	core, logs := observer.New(zapcore.InfoLevel)
	r := &Replayer{logger: zap.New(core), mockDB: &windowBgMockDB{mocks: mocks}}
	doc := &models.Mapping{TestSetID: "ts-0"}
	if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", tcs, doc, doc); err != nil {
		t.Fatalf("populateWindowDerivedBackground: %v", err)
	}
	if len(doc.Worker) > windowDerivedMaxPerTier {
		t.Fatalf("len(doc.Worker) = %d exceeded windowDerivedMaxPerTier=%d", len(doc.Worker), windowDerivedMaxPerTier)
	}
	polls := 0
	for _, e := range doc.Worker {
		if strings.HasPrefix(e.Name, "poll-") {
			polls++
		}
	}
	if polls < learn.MinEpochReads+1 {
		t.Fatalf("poller samples kept = %d, want >= %d", polls, learn.MinEpochReads+1)
	}
	if logs.Len() == 0 {
		t.Fatalf("expected Info log when tier cap dropped %d recordings, got 0 logs", 308-len(doc.Worker))
	}
}

// S2: a stream error during fallback populateWindowDerivedBackground logs at
// Warn (not Debug) stating worker: was not derived and returns the error.
func TestPopulateWindowDerivedBackground_StreamErrorWarns(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	tcs := []*models.TestCase{makeTestCaseWindow("test-1", t0.Add(10*time.Second), t0.Add(12*time.Second))}
	core, logs := observer.New(zapcore.WarnLevel)
	r := &Replayer{
		logger: zap.New(core),
		mockDB: &windowBgMockDB{err: errors.New("disk read failed")},
	}
	doc := &models.Mapping{TestSetID: "ts-0"}
	if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", tcs, doc, doc); err == nil {
		t.Fatal("expected error from populateWindowDerivedBackground on stream failure")
	}
	if logs.Len() == 0 {
		t.Fatal("expected Warn log on stream failure, got 0 Warn logs")
	}
}

// B3 (R4): a fresh RunTestSet with RemoveUnusedMocks=true preserves
// between-window pollers past the startup cutoff from pruning and writes them
// into mappings.yaml worker:.
func TestRunTestSet_FreshPruneRunKeepsWindowDerivedWorkers(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	h := newPartialRunHarness(t, 7, 0)
	pinWindows(h) // test-i at i s, 100 ms; startup cutoff = test-6's start
	h.replayer.config.Test.StrictMockWindow = true
	h.replayer.config.Test.RemoveUnusedMocks = true
	h.replayer.config.Test.UpdateTestMapping = true

	var recording []*models.Mock
	consumed := map[string][]models.MockState{}
	for i := 1; i <= 7; i++ {
		name := fmt.Sprintf("in-test-%d", i)
		at := 1000*i + 50
		recording = append(recording, makeMongoPollMock(name, "users", attrAt(at), attrAt(at+5)))
		consumed[fmt.Sprintf("test-%d", i)] = []models.MockState{{Name: name, Kind: models.Mongo, Timestamp: attrAt(at).Unix()}}
	}
	for i := 1; i <= 6; i++ {
		at := 1000*i + 500
		recording = append(recording, makeMongoPollMock(fmt.Sprintf("poll-%d", i), "ptw_endpoints", attrAt(at), attrAt(at+5)))
	}
	db := &pruningWindowMockDB{recording: recording}
	pm := &shMappingDB{prMappingDB: &prMappingDB{exists: false}, mapping: nil}
	h.replayer.mappingDB = pm
	h.replayer.mockDB = db
	h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, consumed)}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status = %q, want PASSED", status)
	}
	if pm.last == nil {
		t.Fatal("no mappings.yaml written")
	}
	got := map[string]bool{}
	for _, e := range pm.last.Worker {
		got[e.Name] = true
	}
	for i := 1; i <= 6; i++ {
		if n := fmt.Sprintf("poll-%d", i); !got[n] {
			t.Errorf("worker: lacks %s (pruned=%v, worker=%v)", n, db.pruned, bgEntryNames(pm.last.Worker))
		}
	}
	for _, pruned := range db.pruned {
		if strings.HasPrefix(pruned, "poll-") {
			t.Errorf("between-window poller %s was pruned before mappings.yaml persistence", pruned)
		}
	}
}

// MX5: when mappingDB.Exists returns an error, RunTestSet MUST fail closed and
// NOT populate or clobber worker: even under UpdateTestMapping=true.
func TestRunTestSet_ExistsErrorDoesNotPopulateWorker(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	recording := []*models.Mock{
		makeMongoPollMock("in-test-1", "users", attrAt(1050), attrAt(1055)),
		makeMongoPollMock("between-ptw-1", "ptw_endpoints", attrAt(1500), attrAt(1505)),
		makeMongoPollMock("in-test-2", "orders", attrAt(2050), attrAt(2055)),
	}
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.replayer.config.Test.StrictMockWindow = true
	h.replayer.config.Test.UpdateTestMapping = true
	baseMap := &shMappingDB{prMappingDB: &prMappingDB{exists: true}, mapping: &models.Mapping{TestSetID: "test-set-0"}}
	h.replayer.mappingDB = &existsErrMappingDB{shMappingDB: baseMap, existsErr: errors.New("stat EIO")}
	h.replayer.mockDB = &allMocksDB{recording: recording}
	h.replayer.hookImpl = prHooks{
		consumedFor: consumedByWindow(h, map[string][]models.MockState{
			"test-1": {{Name: "in-test-1", Kind: models.Mongo, Timestamp: attrAt(1050).Unix()}},
			"test-2": {{Name: "in-test-2", Kind: models.Mongo, Timestamp: attrAt(2050).Unix()}},
		}),
	}
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status = %q, want PASSED", status)
	}
	if baseMap.last != nil && len(baseMap.last.Worker) != 0 {
		t.Fatalf("Exists error still populated Worker: %+v", baseMap.last.Worker)
	}
}

// S3: an existing keploy-written mappings.yaml (Source == "keploy test") with
// an empty worker: list IS healed (both Pass 1 derive=true AND persistence on
// default and --update-test-mapping runs), while a user-edited file (Source ==
// "") or one that already has worker: entries is NEVER modified.
func TestRunTestSet_HealsKeployTestEmptyWorkerAndPreservesUserEdited(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	recording := []*models.Mock{
		makeMongoPollMock("boot-ptw-1", "ptw_endpoints", attrAt(500), attrAt(505)),
		makeMongoPollMock("in-test-1", "users", attrAt(1050), attrAt(1055)),
		makeMongoPollMock("between-ptw-1", "ptw_endpoints", attrAt(1500), attrAt(1505)),
		makeMongoPollMock("in-test-2", "orders", attrAt(2050), attrAt(2055)),
	}

	t.Run("heals keploy-written mappings.yaml with empty worker on default run", func(t *testing.T) {
		stale := &models.Mapping{
			TestSetID: "test-set-0",
			Source:    models.MappingSourceKeployTest,
			Startup:   []models.MockEntry{{Name: "boot-ptw-1"}},
			Worker:    nil,
			TestCases: []models.MappedTestCase{
				{ID: "test-1", Mocks: []models.MockEntry{{Name: "in-test-1"}}},
				{ID: "test-2", Mocks: []models.MockEntry{{Name: "in-test-2"}}},
			},
		}
		h := newPartialRunHarness(t, 2, 0)
		pinWindows(h)
		h.replayer.config.Test.StrictMockWindow = true
		pm := &shMappingDB{prMappingDB: &prMappingDB{exists: true}, mapping: stale}
		h.replayer.mappingDB = pm
		h.replayer.mockDB = &allMocksDB{recording: recording}
		// backgroundMocksOf should enable derive=true while healing.
		if _, _, derive := h.replayer.backgroundMocksOf(context.Background(), "test-set-0"); !derive {
			t.Fatal("keploy-written mapping with empty worker: should set derive=true while healing")
		}
		if status := h.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("status = %q, want PASSED", status)
		}
		if pm.last == nil {
			t.Fatal("expected healing Insert on keploy-written mapping with empty worker:")
		}
		if got := bgEntryNames(pm.last.Worker); len(got) != 1 || got[0] != "between-ptw-1" {
			t.Fatalf("healed Worker = %v, want [between-ptw-1]", got)
		}
	})

	t.Run("never touches user-edited mapping with empty worker", func(t *testing.T) {
		userEdited := &models.Mapping{
			TestSetID: "test-set-0",
			Source:    "", // not written by keploy test
			Startup:   []models.MockEntry{{Name: "boot-ptw-1"}},
			Worker:    nil,
			TestCases: []models.MappedTestCase{
				{ID: "test-1", Mocks: []models.MockEntry{{Name: "in-test-1"}}},
				{ID: "test-2", Mocks: []models.MockEntry{{Name: "in-test-2"}}},
			},
		}
		h := newPartialRunHarness(t, 2, 0)
		pinWindows(h)
		h.replayer.config.Test.StrictMockWindow = true
		h.replayer.config.Test.UpdateTestMapping = true
		pm := &shMappingDB{prMappingDB: &prMappingDB{exists: true}, mapping: userEdited}
		h.replayer.mappingDB = pm
		h.replayer.mockDB = &allMocksDB{recording: recording}
		h.replayer.hookImpl = prHooks{
			consumedFor: consumedByWindow(h, map[string][]models.MockState{
				"test-1": {{Name: "in-test-1", Kind: models.Mongo, Timestamp: attrAt(1050).Unix()}},
				"test-2": {{Name: "in-test-2", Kind: models.Mongo, Timestamp: attrAt(2050).Unix()}},
			}),
		}
		if _, _, derive := h.replayer.backgroundMocksOf(context.Background(), "test-set-0"); derive {
			t.Fatal("user-edited empty worker: must NOT set derive=true")
		}
		if status := h.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("status = %q, want PASSED", status)
		}
		if pm.last == nil {
			t.Fatal("expected Insert under UpdateTestMapping=true")
		}
		if len(pm.last.Worker) != 0 {
			t.Fatalf("user-edited mapping had Worker populated: %+v", pm.last.Worker)
		}
	})
}

// End-to-end Pass 1 -> Pass 2 test through real RunTestSet + real managed agent
// + real HTTP MockOutgoing miss classification (S4):
//   - Pass 1 (no mappings.yaml): creates mappings.yaml with startup: (boot) and
//     worker: (between/after windows), while leaving in-window mocks out.
//   - Pass 2 (loads Pass 1's mappings.yaml): seeds MockManager.BackgroundRecordings
//     from the persisted file.
//   - A between-tests poller's repeat inside a test is marked Background=true
//     (CountsAgainstTest() == false, dataPlaneMisses == 0 so learning proceeds),
//     while the handler's own uiTestRuns miss inside a test is NOT background
//     (CountsAgainstTest() == true, dataPlaneMisses == 1 so it still counts).
func TestRunTestSet_Pass1PersistsWindowBackgroundForPass2AndRespectsExistingMappings(t *testing.T) {
	t.Setenv(provenswitch.Env, "")

	// pinWindows sets test-1=[attrAt(1000), attrAt(1100)], test-2=[attrAt(2000), attrAt(2100)].
	recording := []*models.Mock{
		makeMongoPollMock("boot-ptw-1", "ptw_endpoints", attrAt(500), attrAt(505)),
		makeHTTPPollMock("in-test-1-uitestruns", "/api/uiTestRuns", attrAt(1050), attrAt(1055)),
		makeHTTPPollMock("between-ptw-http", "/internal/ptw_endpoints/poll", attrAt(1500), attrAt(1505)),
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
	h1.replayer.hookImpl = prHooks{
		consumedFor: consumedByWindow(h1, map[string][]models.MockState{
			"test-1": {{Name: "in-test-1-uitestruns", Kind: models.HTTP, Timestamp: attrAt(1050).Unix()}},
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
	wantWorker := []string{"between-ptw-http", "between-sqs-1"}
	if got := bgEntryNames(written.Worker); len(got) != len(wantWorker) {
		t.Fatalf("Pass 1 written Worker = %v, want %v", got, wantWorker)
	} else {
		for i, w := range wantWorker {
			if got[i] != w {
				t.Fatalf("Pass 1 written Worker[%d] = %q, want %q", i, got[i], w)
			}
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
	bgNames := map[string]bool{}
	for _, kind := range []models.Kind{models.Mongo, models.HTTP, models.HTTP2} {
		for _, rec := range agent2.mm.BackgroundRecordings(kind) {
			bgNames[rec.Mock.Name] = true
		}
	}
	for _, want := range []string{"boot-ptw-1", "between-ptw-http", "between-sqs-1"} {
		if !bgNames[want] {
			t.Fatalf("Pass 2 agent missing background recording %q (got=%v)", want, bgNames)
		}
	}
	for _, forbidden := range []string{"in-test-1-uitestruns", "in-test-2", "after-ptw-1"} {
		if bgNames[forbidden] {
			t.Fatalf("Pass 2 agent must NOT promote %q (got=%v)", forbidden, bgNames)
		}
	}

	// --- End-to-end miss classification on Pass 2's seeded MockManager ---
	// Clear filtered mocks so both live outgoing calls miss and go through
	// real HTTP background-miss classification against Pass 2's seeded
	// BackgroundRecordings.
	agent2.mm.SetFilteredMocks(nil)
	agent2.mm.SetUnFilteredMocks(nil)
	sendHTTPMiss := func(rawReq string) []models.UnmatchedCall {
		var mu sync.Mutex
		var calls []models.UnmatchedCall
		recordErr := func(err error) {
			if err == nil {
				return
			}
			var mr interface {
				MismatchReport() *models.MockMismatchReport
			}
			if errors.As(err, &mr) {
				if r := mr.MismatchReport(); r != nil {
					mu.Lock()
					calls = append(calls, models.UnmatchedCall{
						Protocol:      r.Protocol,
						ActualSummary: r.ActualSummary,
						Background:    r.Background,
					})
					mu.Unlock()
				}
			}
		}
		ctx := models.WithMockMismatchReporter(context.Background(), recordErr)
		clientConn, serverConn := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() { _ = serverConn.Close() }()
			if err := httppkg.New(zap.NewNop()).MockOutgoing(
				ctx,
				serverConn,
				&models.ConditionalDstCfg{Addr: "upstream.internal:8080", Port: 8080},
				agent2.mm,
				models.OutgoingOptions{},
			); err != nil && !models.IsMismatchReported(err) {
				recordErr(err)
			}
		}()
		_, _ = io.WriteString(clientConn, rawReq)
		_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.ReadAll(clientConn)
		_ = clientConn.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		return calls
	}

	// 1. Repeat of the between-tests poller inside a test -> marked Background
	// (CountsAgainstTest == false, dataPlaneMisses == 0 -> learning unblocked).
	pollerMisses := sendHTTPMiss("GET /internal/ptw_endpoints/poll HTTP/1.1\r\nHost: upstream.internal:8080\r\nConnection: close\r\n\r\n")
	if len(pollerMisses) != 1 {
		t.Fatalf("expected 1 poller miss, got %+v", pollerMisses)
	}
	if !pollerMisses[0].Background || pollerMisses[0].CountsAgainstTest() {
		t.Fatalf("poller repeat miss must be Background (CountsAgainstTest=false), got %+v", pollerMisses[0])
	}
	if got := dataPlaneMisses(pollerMisses); got != 0 {
		t.Fatalf("dataPlaneMisses(poller repeat) = %d, want 0 (must not block noise learning)", got)
	}

	// 2. Handler's own uiTestRuns miss (recorded inside test-1, NOT in worker:)
	// -> NOT Background (CountsAgainstTest == true, dataPlaneMisses == 1).
	ownMisses := sendHTTPMiss("GET /api/uiTestRuns HTTP/1.1\r\nHost: upstream.internal:8080\r\nConnection: close\r\n\r\n")
	if len(ownMisses) != 1 {
		t.Fatalf("expected 1 own-call miss, got %+v", ownMisses)
	}
	if ownMisses[0].Background || !ownMisses[0].CountsAgainstTest() {
		t.Fatalf("handler's own uiTestRuns miss must NOT be Background (CountsAgainstTest=true), got %+v", ownMisses[0])
	}
	if got := dataPlaneMisses(ownMisses); got != 1 {
		t.Fatalf("dataPlaneMisses(uiTestRuns own miss) = %d, want 1", got)
	}

	// --- Pass 3: existing user-edited mappings.yaml where operator cleared worker: ---
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
			"test-1": {{Name: "in-test-1-uitestruns", Kind: models.HTTP, Timestamp: attrAt(1050).Unix()}},
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
		t.Fatalf("Pass 3 re-populated Worker on an EXISTING user-edited mappings.yaml: %+v", p3Map.last.Worker)
	}
}

func TestWindowBgCollector_BoundsPerShapeAndSeparatesHTTP2AmzTargets(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	col := models.NewDerivedBgCollector(models.DerivedBgCollectorConfig{
		MaxEntries:  windowDerivedMaxPerTier,
		MaxPerShape: windowDerivedMaxPerShape,
		MinPerShape: learn.MinEpochReads + 1,
	})

	add := func(m *models.Mock) {
		col.Add(models.DerivedBgItem{
			Entry: models.MockEntry{Name: m.Name, Kind: string(m.Kind)},
			Shape: models.BackgroundShapeKey(m),
			At:    m.Spec.ReqTimestampMock,
		})
	}

	// Push 12 ReceiveMessage and 2 DeleteMessage calls to the SAME HTTP/2 POST / authority.
	for i := 0; i < 12; i++ {
		at := t0.Add(time.Duration(i) * time.Second)
		add(makeHTTP2SQSMock("recv-"+string(rune('a'+i)), "AmazonSQS.ReceiveMessage", at, at.Add(time.Millisecond)))
	}
	for i := 0; i < 2; i++ {
		at := t0.Add(time.Duration(20+i) * time.Second)
		add(makeHTTP2SQSMock("del-"+string(rune('a'+i)), "AmazonSQS.DeleteMessage", at, at.Add(time.Millisecond)))
	}

	entries := col.Entries()
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
