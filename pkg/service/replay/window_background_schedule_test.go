package replay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	httppkg "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	yamlLib "gopkg.in/yaml.v3"
)

type recordedWindowsTestDB struct {
	prTestDB
	allWindows []models.TestWindow
	winErr     error
	firstStart time.Time
	firstErr   error
	hasFirst   bool
}

func (db *recordedWindowsTestDB) RecordedTestWindows(context.Context, string) ([]models.TestWindow, error) {
	if db.winErr != nil {
		return nil, db.winErr
	}
	return append([]models.TestWindow(nil), db.allWindows...), nil
}

func (db *recordedWindowsTestDB) RecordedFirstTestStart(context.Context, string) (time.Time, error) {
	if db.firstErr != nil {
		return time.Time{}, db.firstErr
	}
	if db.hasFirst {
		return db.firstStart, nil
	}
	return time.Time{}, nil
}

func agentBackgroundNames(a *managedAgent) []string {
	var out []string
	for _, k := range []models.Kind{models.Mongo, models.HTTP, models.HTTP2} {
		for _, rec := range a.mm.BackgroundRecordings(k) {
			out = append(out, rec.Mock.Name)
		}
	}
	sort.Strings(out)
	return out
}

func makeShapeMongoMock(name, query string, req, res time.Time) *models.Mock {
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
			MongoRequests: []models.MongoRequest{{Message: &models.MongoOpMessage{
				Sections: []string{`msg:{"findAndModify":"ptw_endpoints","query":` + query + `}`},
			}}},
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:        models.LifetimePerTest,
			LifetimeDerived: true,
			IsFiltered:      true,
		},
	}
}

// Decision 1 + P1: Real fresh record on default descriptive (slug) test names
// via TestYaml.InsertTestCase persists immutable recorded_windows, implements
// RecordedWindowsReader, and derives + persists the between-tests poller in
// RunTestSet on Pass 1 with background_derived: true.
func TestWindowBg_FreshDefaultSlugRecordDerivesAndPersistsPoller(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	ctx := context.Background()
	rootDir := t.TempDir()
	tdb := testdb.New(zap.NewNop(), rootDir) // default NamingDescriptive!

	// Simulate a real fresh keploy record (leaving tc.Name empty so TestYaml
	// assigns default descriptive slugs: get-v1-clusters-1, post-v1-apps-1, get-v1-users-1).
	h := newPartialRunHarness(t, 3, 0)
	baseURL := strings.TrimSuffix(h.cases[0].HTTPReq.URL, "/test-1")
	if baseURL == h.cases[0].HTTPReq.URL {
		// Fall back to the listener host/port from h.cases[0].HTTPReq.URL.
		if idx := strings.Index(h.cases[0].HTTPReq.URL, "://"); idx >= 0 {
			rest := h.cases[0].HTTPReq.URL[idx+3:]
			if slash := strings.IndexByte(rest, '/'); slash >= 0 {
				baseURL = h.cases[0].HTTPReq.URL[:idx+3+slash]
			} else {
				baseURL = h.cases[0].HTTPReq.URL
			}
		}
	}
	rawCases := []*models.TestCase{
		{
			Version:  models.GetVersion(),
			Kind:     models.HTTP,
			HTTPReq:  models.HTTPReq{Method: "GET", URL: baseURL + "/v1/clusters", Timestamp: attrAt(1000)},
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"ok":1}`, Timestamp: attrAt(1100)},
		},
		{
			Version:  models.GetVersion(),
			Kind:     models.HTTP,
			HTTPReq:  models.HTTPReq{Method: "POST", URL: baseURL + "/v1/apps", Timestamp: attrAt(2000)},
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"ok":2}`, Timestamp: attrAt(2100)},
		},
		{
			Version:  models.GetVersion(),
			Kind:     models.HTTP,
			HTTPReq:  models.HTTPReq{Method: "GET", URL: baseURL + "/v1/users", Timestamp: attrAt(3000)},
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"ok":3}`, Timestamp: attrAt(3100)},
		},
	}
	for _, tc := range rawCases {
		if err := tdb.InsertTestCase(ctx, tc, "test-set-0", false); err != nil {
			t.Fatalf("InsertTestCase: %v", err)
		}
		if strings.HasPrefix(tc.Name, "test-") {
			t.Fatalf("expected default descriptive slug name, got %q", tc.Name)
		}
	}

	held, err := tdb.GetTestCases(ctx, "test-set-0")
	if err != nil || len(held) != 3 {
		t.Fatalf("GetTestCases = (%d, %v), want 3", len(held), err)
	}

	h.replayer.config.Path = rootDir
	h.replayer.testDB = tdb
	h.cases = held
	h.instr.cases = held
	h.replayer.config.Test.StrictMockWindow = true
	h.replayer.config.Test.UpdateTestMapping = true

	var rec []*models.Mock
	consumed := map[string][]models.MockState{}
	for i, tc := range held {
		at := tc.HTTPReq.Timestamp.Add(50 * time.Millisecond)
		m := makeMongoPollMock(fmt.Sprintf("own-%d", i+1), fmt.Sprintf("own_%d", i+1), at, at.Add(5*time.Millisecond))
		rec = append(rec, m)
		consumed[tc.Name] = []models.MockState{{Name: m.Name, Kind: models.Mongo, Timestamp: m.Spec.ReqTimestampMock.Unix()}}
	}
	// 6-sample ptw_endpoints poller between test 1 and test 2.
	for i := 0; i < 6; i++ {
		at := attrAt(1200 + 100*i)
		rec = append(rec, makeMongoPollMock(fmt.Sprintf("poll-%d", i), "ptw_endpoints", at, at.Add(5*time.Millisecond)))
	}
	pm := &shMappingDB{prMappingDB: &prMappingDB{exists: false}}
	h.replayer.mappingDB = pm
	h.replayer.mockDB = &allMocksDB{recording: rec}
	h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, consumed)}
	a := newManagedAgent(t, h.instr)
	h.replayer.instrumentation = a

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status = %q, want PASSED", status)
	}
	inRAM := 0
	for _, n := range agentBackgroundNames(a) {
		if strings.HasPrefix(n, "poll-") {
			inRAM++
		}
	}
	if inRAM != 6 {
		t.Fatalf("Pass 1 agent in-RAM poller samples = %d, want 6", inRAM)
	}
	if pm.last == nil || !pm.last.BackgroundDerived {
		t.Fatalf("expected persisted mappings.yaml stamped background_derived=true, got %+v", pm.last)
	}
	persisted := 0
	for _, e := range pm.last.Worker {
		if strings.HasPrefix(e.Name, "poll-") {
			persisted++
		}
	}
	if persisted != 6 {
		t.Fatalf("persisted worker: holds %d poller samples, want 6 (%v)", persisted, bgEntryNames(pm.last.Worker))
	}
}

// Decision 2 + P1/S-Fresh no-regression: for a pre-existing set with NO
// recorded schedule (neither default descriptive nor sequential names), keep
// main's exact agent-side Pass 1 in-RAM behaviour, persist NOTHING to worker:,
// and NEVER stamp background_derived=true (so a later re-record/heal stays open).
func TestWindowBg_NoScheduleKeepsLegacyPass1AndNeverStamps(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, tc := range []struct {
		label string
		names []string
	}{
		{"descriptive default names", []string{"get-v1-clusters-1", "post-v1-apps-1", "get-v1-users-1"}},
		{"sequential test-N names", []string{"test-1", "test-2", "test-3"}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			h := newPartialRunHarness(t, 3, 0)
			pinWindows(h)
			for i, c := range h.cases {
				c.Name = tc.names[i]
			}
			h.replayer.config.Test.StrictMockWindow = true
			h.replayer.config.Test.UpdateTestMapping = true
			var rec []*models.Mock
			consumed := map[string][]models.MockState{}
			for i := 1; i <= 3; i++ {
				at := 1000*i + 50
				m := makeMongoPollMock(fmt.Sprintf("own-%d", i), fmt.Sprintf("own_%d", i), attrAt(at), attrAt(at+5))
				rec = append(rec, m)
				consumed[tc.names[i-1]] = []models.MockState{{Name: m.Name, Kind: models.Mongo, Timestamp: m.Spec.ReqTimestampMock.Unix()}}
			}
			for i := 0; i < 6; i++ {
				at := 1200 + 100*i
				rec = append(rec, makeMongoPollMock(fmt.Sprintf("poll-%d", i), "ptw_endpoints", attrAt(at), attrAt(at+5)))
			}
			pm := &shMappingDB{prMappingDB: &prMappingDB{exists: false}}
			h.replayer.mappingDB = pm
			h.replayer.mockDB = &allMocksDB{recording: rec}
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, consumed)}
			a := newManagedAgent(t, h.instr)
			h.replayer.instrumentation = a

			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status = %q, want PASSED", status)
			}
			inRAM := 0
			for _, n := range agentBackgroundNames(a) {
				if strings.HasPrefix(n, "poll-") {
					inRAM++
				}
			}
			if inRAM < learn.MinEpochReads+1 {
				t.Fatalf("Pass 1 agent in-RAM regressed: got %d poller samples, want >= %d", inRAM, learn.MinEpochReads+1)
			}
			if pm.last == nil {
				t.Fatal("expected mappings.yaml written")
			}
			if len(pm.last.Worker) != 0 {
				t.Fatalf("without recorded schedule, persisted worker: must stay empty, got %v", bgEntryNames(pm.last.Worker))
			}
			if pm.last.BackgroundDerived {
				t.Fatal("without recorded schedule, background_derived must stay false (never lock out future heal)")
			}
		})
	}
}

// Decision 3 + P2 + D1 + D2:
//   - Distinct queries on the same collection (D1: _id:42 vs dirtyAt:$lte) NEVER
//     match by shape: wrong answer -> OBSOLETE (1 DepResult row), --assert-dependencies -> FAILED (1 DepResult row).
//   - Identical query shape under --assert-dependencies (P2) STILL FAILS (1 DepResult row).
//   - Foreign same-shape mock consumed outside mapping (D2) -> OBSOLETE.
//   - Incidental in-window background poll (same query structure dirtyAt:$lte) left
//     unconsumed on a wrong answer vetoes ONLY the OBSOLETE demotion -> FAILED.
func TestWindowBg_Decision3_ShapeMatchObsoleteOnlyAndQueryStructure(t *testing.T) {
	own1Consumed := []models.MockState{{Name: "mock-own-1", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest}}

	for _, c := range []struct {
		label      string
		seed       string
		inWinQuery string
		wrongBody  bool
		assertDeps bool
		wantSet    models.TestSetStatus
		wantTest   models.TestStatus
		wantRows   int
	}{
		{"D1 wrong answer, distinct query, seed in worker -> OBSOLETE", "worker", `{"_id":42,"owner":null}`, true, false, models.TestSetStatusInternalErr, models.TestStatusObsolete, 1},
		{"D1 wrong answer, distinct query, seed in startup -> OBSOLETE", "startup", `{"_id":42,"owner":null}`, true, false, models.TestSetStatusInternalErr, models.TestStatusObsolete, 1},
		{"D1/P2 right answer, --assert-dependencies, distinct query -> FAILED", "worker", `{"_id":42,"owner":null}`, false, true, models.TestSetStatusFailed, models.TestStatusFailed, 1},
		{"P2 right answer, --assert-dependencies, SAME query shape -> FAILED", "worker", `{"dirtyAt":{"$lte":1795784599}}`, false, true, models.TestSetStatusFailed, models.TestStatusFailed, 1},
		{"Bug #2 wrong answer, incidental in-window poller (SAME query structure) -> FAILED not OBSOLETE", "worker", `{"dirtyAt":{"$lte":1795784599}}`, true, false, models.TestSetStatusFailed, models.TestStatusFailed, 1},
	} {
		t.Run(c.label, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "")
			h := newPartialRunHarness(t, 1, 0)
			h.replayer.config.Test.AssertDependencies = c.assertDeps
			own1 := perTestMongoMock("mock-own-1")
			inWin := makeShapeMongoMock("mock-in-win", c.inWinQuery, attrAt(1020), attrAt(1021))
			bgPoll := makeShapeMongoMock("mock-bg-poll", `{"dirtyAt":{"$lte":1795784500}}`, attrAt(500), attrAt(501))
			h.mocks.filtered = []*models.Mock{own1, inWin, bgPoll}
			testEntries := []models.MockEntry{{Name: "mock-own-1", Kind: string(models.Mongo)}, {Name: "mock-in-win", Kind: string(models.Mongo)}}
			h.mappings.onDisk = map[string][]models.MockEntry{"test-1": testEntries}
			doc := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: testEntries}}}
			seed := []models.MockEntry{{Name: "mock-bg-poll", Kind: string(models.Mongo)}}
			if c.seed == "worker" {
				doc.Worker = seed
			} else {
				doc.Startup = seed
				h.mappings.onDiskStartup = seed
			}
			h.replayer.mappingDB = &shMappingDB{prMappingDB: h.mappings, mapping: doc}
			hooks := prHooks{consumed: own1Consumed}
			if c.wrongBody {
				hooks.wrongBody = map[string]bool{"test-1": true}
			}
			h.replayer.hookImpl = hooks
			setStatus := h.run(t)
			got := resultOf(t, h, "test-1")
			if setStatus != c.wantSet || got.Status != c.wantTest || len(got.Result.DepResult) != c.wantRows {
				t.Fatalf("got (set=%s test=%s rows=%d), want (set=%s test=%s rows=%d)",
					setStatus, got.Status, len(got.Result.DepResult), c.wantSet, c.wantTest, c.wantRows)
			}
		})
	}

	t.Run("D2 foreign same-shape mock consumed outside mapping stays OBSOLETE", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		// Direct check: consuming a foreign mock outside expected NEVER
		// triggers the OBSOLETE veto even if its shape matches worker:.
		if isOnlyObsoleteShapeUnconsumed(
			[]string{"mock-own-1", "mock-foreign-claim"},
			[]string{"mock-own-1", "mock-in-win-poll"},
			map[string]bool{"mock-in-win-poll": true, "mock-foreign-claim": true},
		) {
			t.Fatal("isOnlyObsoleteShapeUnconsumed returned true despite foreign consumed mock outside expected")
		}
		// End-to-end RunTestSet: test-1 expects mock-own-1 + an incidental
		// in-window same-shape poll, consumes mock-own-1 + a foreign mock
		// outside its mapping, and answers wrong -> must stay OBSOLETE.
		h := newPartialRunHarness(t, 1, 0)
		own1 := perTestMongoMock("mock-own-1")
		inWinPoll := makeShapeMongoMock("mock-in-win-poll", `{"dirtyAt":{"$lte":1795784550}}`, attrAt(1015), attrAt(1016))
		foreign := makeShapeMongoMock("mock-foreign-claim", `{"dirtyAt":{"$lte":1795784599}}`, attrAt(1020), attrAt(1021))
		bgPoll := makeShapeMongoMock("mock-bg-poll", `{"dirtyAt":{"$lte":1795784500}}`, attrAt(500), attrAt(501))
		h.mocks.filtered = []*models.Mock{own1, inWinPoll, foreign, bgPoll}
		testEntries := []models.MockEntry{
			{Name: "mock-own-1", Kind: string(models.Mongo)},
			{Name: "mock-in-win-poll", Kind: string(models.Mongo)},
		}
		h.mappings.onDisk = map[string][]models.MockEntry{"test-1": testEntries}
		h.replayer.mappingDB = &shMappingDB{prMappingDB: h.mappings, mapping: &models.Mapping{
			TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: testEntries}},
			Worker:    []models.MockEntry{{Name: "mock-bg-poll", Kind: string(models.Mongo)}},
		}}
		h.replayer.hookImpl = prHooks{
			consumed: []models.MockState{
				{Name: "mock-own-1", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
				{Name: "mock-foreign-claim", Kind: models.Mongo, Type: "read", Lifetime: models.LifetimePerTest},
			},
			wrongBody: map[string]bool{"test-1": true},
		}
		_ = h.run(t)
		got := resultOf(t, h, "test-1")
		if got.Status != models.TestStatusObsolete {
			t.Fatalf("foreign consumed same-shape mock gave %s, want OBSOLETE", got.Status)
		}
	})

	t.Run("stub mocks with empty SpecificBackgroundShapeKey never match (kills R21)", func(t *testing.T) {
		stubA := perTestMongoMock("stub-a")
		stubB := perTestMongoMock("stub-b")
		if matched := backgroundShapeMatchedNames(map[string]bool{"stub-a": true}, []*models.Mock{stubA, stubB}); len(matched) != 0 {
			t.Fatalf("stub mocks matched by empty shape: %v", matched)
		}
	})
}

// Decision 4 + P3 + P5 + A2 + R07/M1/R05/R08/R09/R10/R12/R13/R15/R16b/R24:
//   - Consecutive test-N names NEVER prove an empty gap (P3).
//   - --must-pass deletions ONLY add refusal zones, never making an incomplete
//     schedule complete (P5/A2), and still refuse mocks inside deleted windows
//     when a full schedule is present (kills R07).
//   - In-window, response-overlap, in-flight-into-first, ScheduleIncomplete, and
//     RecordedStartUnreadable all fail closed and never stamp (kills M1, R05, R08, R09, B2).
func TestWindowBg_Decision4_ScheduleOnlyRefusalZonesAndMutantKillers(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

	t.Run("P3 consecutive test-N names without recorded schedule promote nothing", func(t *testing.T) {
		held := []*models.TestCase{
			makeTestCaseWindow("test-3", at(10000), at(11000)),
			makeTestCaseWindow("test-4", at(20000), at(21000)),
		}
		mocks := []*models.Mock{
			makeMongoPollMock("removed-test-5-own", "uiTestRuns", at(16000), at(16005)),
		}
		core, logs := observer.New(zapcore.InfoLevel)
		r := &Replayer{logger: zap.New(core), mockDB: &windowBgMockDB{mocks: mocks}, testDB: &prTestDB{cases: held}}
		doc := &models.Mapping{TestSetID: "ts-0"}
		_ = r.populateWindowDerivedBackground(context.Background(), "ts-0", held, doc, doc)
		if len(doc.Startup) != 0 || len(doc.Worker) != 0 || doc.BackgroundDerived {
			t.Fatalf("consecutive test-N without recorded schedule promoted startup=%v worker=%v stamped=%v",
				bgEntryNames(doc.Startup), bgEntryNames(doc.Worker), doc.BackgroundDerived)
		}
		if logs.Len() == 0 {
			t.Fatal("expected Info log explaining wholesale refusal when no recorded schedule exists")
		}
	})

	t.Run("P5/A2 must-pass deletions never make a partial schedule complete", func(t *testing.T) {
		for _, names := range [][3]string{{"test-2", "test-4", "test-5"}, {"get-b-1", "get-c-1", "get-d-1"}} {
			held := []*models.TestCase{
				makeTestCaseWindow(names[0], at(20000), at(21000)),
				makeTestCaseWindow(names[1], at(40000), at(41000)),
			}
			mocks := []*models.Mock{
				makeMongoPollMock("removed-a-own", "uiTestRuns", at(11000), at(11005)),
				makeMongoPollMock("removed-x-own", "orders", at(31000), at(31005)),
			}
			r := &Replayer{
				logger:             zap.NewNop(),
				mockDB:             &windowBgMockDB{mocks: mocks},
				testDB:             &prTestDB{cases: held},
				deletedTestWindows: map[string][]models.TestWindow{"ts-0": {{TestCase: names[2], Start: at(50000), End: at(51000)}}},
			}
			doc := &models.Mapping{TestSetID: "ts-0"}
			_ = r.populateWindowDerivedBackground(context.Background(), "ts-0", held, doc, doc)
			if len(doc.Startup) != 0 || len(doc.Worker) != 0 || doc.BackgroundDerived {
				t.Fatalf("%s: --must-pass deletion promoted startup=%v worker=%v stamped=%v",
					names[0], bgEntryNames(doc.Startup), bgEntryNames(doc.Worker), doc.BackgroundDerived)
			}
		}
	})

	t.Run("R07 must-pass deleted window still adds refusal zone on a full schedule", func(t *testing.T) {
		held := []*models.TestCase{
			makeTestCaseWindow("get-a-1", at(10000), at(11000)),
			makeTestCaseWindow("get-c-1", at(30000), at(31000)),
		}
		mocks := []*models.Mock{
			makeMongoPollMock("deleted-b-own", "orders", at(20500), at(20505)),
		}
		r := &Replayer{
			logger:             zap.NewNop(),
			mockDB:             &windowBgMockDB{mocks: mocks},
			testDB:             &recordedWindowsTestDB{prTestDB: prTestDB{cases: held}, allWindows: recordedTestWindows(held)},
			deletedTestWindows: map[string][]models.TestWindow{"ts-0": {{TestCase: "get-b-1", Start: at(20000), End: at(21000)}}},
		}
		doc := &models.Mapping{TestSetID: "ts-0"}
		_ = r.populateWindowDerivedBackground(context.Background(), "ts-0", held, doc, doc)
		if len(doc.Worker) != 0 {
			t.Fatalf("mock inside --must-pass deleted window was promoted: %v", bgEntryNames(doc.Worker))
		}
	})

	t.Run("M1 and R05 in-flight into first window and response overlapping next window are refused", func(t *testing.T) {
		held := []*models.TestCase{
			makeTestCaseWindow("get-a-1", at(10000), at(11000)),
			makeTestCaseWindow("get-b-1", at(20000), at(21000)),
		}
		mocks := []*models.Mock{
			makeMongoPollMock("inflight-into-first", "orders", at(9500), at(10500)),
			makeMongoPollMock("resp-overlap-second", "orders", at(19500), at(20500)),
			makeMongoPollMock("safe-gap", "ptw_endpoints", at(15000), at(15005)),
		}
		r := &Replayer{
			logger: zap.NewNop(),
			mockDB: &windowBgMockDB{mocks: mocks},
			testDB: &recordedWindowsTestDB{prTestDB: prTestDB{cases: held}, allWindows: recordedTestWindows(held)},
		}
		doc := &models.Mapping{TestSetID: "ts-0"}
		_ = r.populateWindowDerivedBackground(context.Background(), "ts-0", held, doc, doc)
		if len(doc.Startup) != 0 {
			t.Fatalf("in-flight-into-first leaked to Startup: %v", bgEntryNames(doc.Startup))
		}
		if got := bgEntryNames(doc.Worker); len(got) != 1 || got[0] != "safe-gap" {
			t.Fatalf("Worker = %v, want [safe-gap] (response-overlapping mock must be refused)", got)
		}
	})

	t.Run("B2/R08/R09 ScheduleIncomplete and RecordedStartUnreadable fail closed and never stamp", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		held := []*models.TestCase{
			makeTestCaseWindow("get-a-1", at(10000), at(11000)),
			makeTestCaseWindow("get-b-1", at(20000), at(21000)),
		}
		mocks := []*models.Mock{
			makeMongoPollMock("boot-poll", "ptw_endpoints", at(2000), at(2005)),
			makeMongoPollMock("gap-poll", "ptw_endpoints", at(15000), at(15005)),
		}
		// R08: RecordedWindowsReader error -> nothing promoted, NOT stamped.
		rErr := &Replayer{
			logger: zap.NewNop(),
			mockDB: &windowBgMockDB{mocks: mocks},
			testDB: &recordedWindowsTestDB{prTestDB: prTestDB{cases: held}, winErr: errors.New("connection reset")},
		}
		docErr := &models.Mapping{TestSetID: "ts-0"}
		_ = rErr.populateWindowDerivedBackground(context.Background(), "ts-0", held, docErr, docErr)
		if len(docErr.Startup) != 0 || len(docErr.Worker) != 0 || docErr.BackgroundDerived {
			t.Fatalf("ScheduleIncomplete promoted/stamped: startup=%v worker=%v stamped=%v",
				bgEntryNames(docErr.Startup), bgEntryNames(docErr.Worker), docErr.BackgroundDerived)
		}
		// R09: RecordedStartUnreadable with full schedule -> prefix MUST fail closed.
		rStartErr := &Replayer{
			logger: zap.NewNop(),
			mockDB: &windowBgMockDB{mocks: mocks},
			testDB: &recordedWindowsTestDB{prTestDB: prTestDB{cases: held}, allWindows: recordedTestWindows(held), firstErr: errors.New("start EIO")},
		}
		docStartErr := &models.Mapping{TestSetID: "ts-0"}
		_ = rStartErr.populateWindowDerivedBackground(context.Background(), "ts-0", held, docStartErr, docStartErr)
		if len(docStartErr.Startup) != 0 {
			t.Fatalf("RecordedStartUnreadable promoted prefix to startup: %v", bgEntryNames(docStartErr.Startup))
		}
	})

	t.Run("R12 derived startup reason blocks re-healing", func(t *testing.T) {
		m := &models.Mapping{
			TestSetID: "ts-0",
			Source:    models.MappingSourceKeployTest,
			Startup:   []models.MockEntry{{Name: "boot-1", Reason: WindowDerivedStartupReason}},
		}
		if isHealableKeployTestMapping(m) {
			t.Fatal("mapping with WindowDerivedStartupReason must NOT be heal-eligible")
		}
	})

	t.Run("R16b lookup read error resets windowBg tracker", func(t *testing.T) {
		tr := newWindowBgTracker(
			[]*models.TestCase{makeTestCaseWindow("get-a-1", at(10000), at(11000)), makeTestCaseWindow("get-b-1", at(20000), at(21000))},
			models.BackgroundWindowOptions{RecordedWindows: []models.TestWindow{
				{TestCase: "get-a-1", Start: at(10000), End: at(11000)},
				{TestCase: "get-b-1", Start: at(20000), End: at(21000)},
			}},
			zap.NewNop(), "ts-0",
		)
		tr.observe(makeMongoPollMock("gap-1", "ptw_endpoints", at(15000), at(15005)))
		tr.markPass()
		tr.reset()
		if tr.sawPass || len(tr.keepNames()) != 0 {
			t.Fatalf("reset() left sawPass=%v names=%v", tr.sawPass, tr.keepNames())
		}
	})
}

// A1 + A3 + C1 + C2 + R13:
//   - A1: Pass 1 agent set == persisted mappings.yaml even when test-2 consumes
//     a between-tests poller sample during Pass 1.
//   - A3: recordedSetShapeWith with full recorded schedule sets deriveBackground=false.
//   - C1/C2/R13: mapdb.HealWindowBackground handles empty existing worker:/startup:
//     without duplicate keys and refuses head or trailing comments on source: or worker:.
func TestWindowBg_Pass1PersistedParityAndMapdbInPlaceHealGuards(t *testing.T) {
	t.Setenv(provenswitch.Env, "")

	t.Run("A1 Pass 1 set equals persisted set even when test-2 consumes a poller sample", func(t *testing.T) {
		h := newPartialRunHarness(t, 3, 0)
		pinWindows(h)
		h.replayer.testDB = &recordedWindowsTestDB{prTestDB: prTestDB{cases: h.cases}, allWindows: recordedTestWindows(h.cases)}
		var rec []*models.Mock
		consumed := map[string][]models.MockState{}
		for i := 1; i <= 3; i++ {
			at := attrAt(1000*i + 50)
			m := makeMongoPollMock(fmt.Sprintf("own-%d", i), "orders", at, at.Add(5*time.Millisecond))
			rec = append(rec, m)
			consumed[h.cases[i-1].Name] = []models.MockState{{Name: m.Name, Kind: models.Mongo, Timestamp: m.Spec.ReqTimestampMock.Unix()}}
		}
		for i := 1; i < 3; i++ {
			for k := 0; k < 3; k++ {
				at := attrAt(1000*i + 400 + 100*k)
				rec = append(rec, makeMongoPollMock(fmt.Sprintf("between-ptw-%d-%d", i, k), "ptw_endpoints", at, at.Add(5*time.Millisecond)))
			}
			at := attrAt(1000*i + 450)
			rec = append(rec, makeHTTPPollMock(fmt.Sprintf("between-health-%d", i), "/v1/sys/health", at, at.Add(5*time.Millisecond)))
		}
		// test-2 also consumes between-ptw-1-0 during Pass 1.
		for _, m := range rec {
			if m.Name == "between-ptw-1-0" {
				consumed["test-2"] = append(consumed["test-2"], models.MockState{Name: m.Name, Kind: m.Kind, Timestamp: m.Spec.ReqTimestampMock.Unix()})
			}
		}
		h.replayer.config.Test.StrictMockWindow = true
		pm := &shMappingDB{prMappingDB: &prMappingDB{exists: false}}
		h.replayer.mappingDB = pm
		h.replayer.mockDB = &allMocksDB{recording: rec}
		h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, consumed)}
		a := newManagedAgent(t, h.instr)
		h.replayer.instrumentation = a
		if status := h.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("status = %q", status)
		}
		inRAM := agentBackgroundNames(a)
		var persisted []string
		if pm.last != nil {
			persisted = pm.last.BackgroundMockNames()
			sort.Strings(persisted)
		}
		if strings.Join(inRAM, ",") != strings.Join(persisted, ",") || len(inRAM) != 8 {
			t.Fatalf("Pass 1 vs persisted diverged: inRAM(%d)=%v persisted(%d)=%v", len(inRAM), inRAM, len(persisted), persisted)
		}
		for _, tc := range pm.last.TestCases {
			for _, e := range tc.Mocks {
				if e.Name == "between-ptw-1-0" {
					t.Fatalf("between-ptw-1-0 duplicated in %s.Mocks alongside worker:", tc.ID)
				}
			}
		}
	})

	t.Run("C1/C2/R13 HealWindowBackground handles empty keys cleanly and refuses commented source/worker", func(t *testing.T) {
		entries := []models.MockEntry{{Name: "between-ptw-1", Kind: string(models.Mongo), Reason: WindowDerivedWorkerReason}}
		startup := []models.MockEntry{{Name: "boot-ptw-1", Kind: string(models.Mongo), Reason: WindowDerivedStartupReason}}
		for _, c := range []struct {
			label      string
			extra      string
			sourceLine string
			wantHealed bool
		}{
			{"empty worker: []", "worker: []\n", "source: keploy test\n", true},
			{"empty startup: []", "startup: []\n", "source: keploy test\n", true},
			{"commented worker: block", "worker:\n  # - name: between-ptw-1\n", "source: keploy test\n", false},
			{"trailing comment on source:", "", "source: keploy test # hand-curated\n", false},
			{"head comment on source:", "", "# ops: hand-curated\nsource: keploy test\n", false},
		} {
			t.Run(c.label, func(t *testing.T) {
				root := t.TempDir()
				dir := filepath.Join(root, "test-set-0")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				body := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: test-set-0\n" +
					c.sourceLine + c.extra +
					"tests:\n  - id: test-1\n    mocks:\n      - name: in-test-1\n        kind: Mongo\n"
				path := filepath.Join(dir, "mappings.yaml")
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				db := mapdb.New(zap.NewNop(), root, "")
				healed, err := db.HealWindowBackground(context.Background(), "test-set-0", entries, startup, WindowDerivedStartupReason)
				if err != nil {
					t.Fatalf("HealWindowBackground error: %v", err)
				}
				if healed != c.wantHealed {
					t.Fatalf("healed = %v, want %v", healed, c.wantHealed)
				}
				after, _ := os.ReadFile(path)
				s := string(after)
				if strings.Count(s, "\nworker:") > 1 || strings.Count(s, "\nstartup:") > 1 {
					t.Fatalf("duplicate key written:\n%s", s)
				}
				if _, gerr := mapdb.New(zap.NewNop(), root, "").GetMapping(context.Background(), "test-set-0"); gerr != nil {
					t.Fatalf("file unreadable after heal: %v\n%s", gerr, s)
				}
			})
		}
	})
}

// Indexed YAML mocks (LeavePayloadOnDisk) preserve both BackgroundShapeKey and
// SpecificBackgroundShapeKey across stores.
func TestWindowBg_IndexedYAMLMocksKeepShapeAndParityAcrossStores(t *testing.T) {
	t.Run("indexed shapes stay distinct", func(t *testing.T) {
		h := newPartialRunHarness(t, 1, 0)
		health := makeHTTPPollMock("health", "/v1/sys/health", attrAt(1200), attrAt(1205))
		pat := makeHTTPPollMock("pat", "/v1/secret/data/pat-1", attrAt(1300), attrAt(1305))
		fullHealth := models.BackgroundShapeKey(health)
		fullPat := models.BackgroundShapeKey(pat)
		fullSpecHealth := models.SpecificBackgroundShapeKey(health)
		fullSpecPat := models.SpecificBackgroundShapeKey(pat)
		if fullHealth == fullPat || fullSpecHealth == "" || fullSpecHealth == fullSpecPat {
			t.Fatalf("precondition failed: full shapes %q vs %q, specific %q vs %q", fullHealth, fullPat, fullSpecHealth, fullSpecPat)
		}
		ys := useYAMLMocks(t, h, health, pat)
		set, err := ys.IndexTestSetMocks(context.Background(), "test-set-0", models.BaseTime, time.Now(), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		gotSpec := map[string]string{}
		for _, m := range append(append([]*models.Mock(nil), set.AllPerTest...), set.AllSession...) {
			if m.Kind == models.HTTP {
				got[m.Name] = models.BackgroundShapeKey(m)
				gotSpec[m.Name] = models.SpecificBackgroundShapeKey(m)
			}
		}
		if got[health.Name] != fullHealth || got[pat.Name] != fullPat {
			t.Fatalf("indexed BackgroundShapeKey mismatch: got (%q, %q), want (%q, %q)", got[health.Name], got[pat.Name], fullHealth, fullPat)
		}
		if gotSpec[health.Name] != fullSpecHealth || gotSpec[pat.Name] != fullSpecPat {
			t.Fatalf("indexed SpecificBackgroundShapeKey mismatch: got (%q, %q), want (%q, %q)", gotSpec[health.Name], gotSpec[pat.Name], fullSpecHealth, fullSpecPat)
		}
	})

	t.Run("RunTestSet worker list does not depend on store", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		for _, store := range []string{"fake-whole-mocks", "yaml-index"} {
			t.Run(store, func(t *testing.T) {
				h := newPartialRunHarness(t, 3, 0)
				pinWindows(h)
				h.replayer.testDB = &recordedWindowsTestDB{prTestDB: prTestDB{cases: h.cases}, allWindows: recordedTestWindows(h.cases)}
				h.replayer.config.Test.StrictMockWindow = true
				h.replayer.config.Test.UpdateTestMapping = true
				var rec []*models.Mock
				for i := 1; i <= 3; i++ {
					at := 1000*i + 50
					rec = append(rec, makeHTTPPollMock(fmt.Sprintf("in-test-%d", i), fmt.Sprintf("/v1/own/%d", i), attrAt(at), attrAt(at+5)))
				}
				for i := 0; i < 6; i++ {
					at := 1200 + 100*i
					rec = append(rec, makeHTTPPollMock(fmt.Sprintf("health-%d", i), "/v1/sys/health", attrAt(at), attrAt(at+5)))
				}
				for i := 0; i < 12; i++ {
					at := 2150 + 60*i
					rec = append(rec, makeHTTPPollMock(fmt.Sprintf("pat-%02d", i), fmt.Sprintf("/v1/secret/data/pat-%02d", i), attrAt(at), attrAt(at+5)))
				}
				labels := make([]string, len(rec))
				for i, m := range rec {
					labels[i] = m.Name
				}
				pm := &shMappingDB{prMappingDB: &prMappingDB{exists: false}}
				h.replayer.mappingDB = pm
				if store == "yaml-index" {
					useYAMLMocks(t, h, rec...)
				} else {
					h.replayer.mockDB = &allMocksDB{recording: rec}
				}
				label := map[string]string{}
				for i, m := range rec {
					label[m.Name] = labels[i]
				}
				consumed := map[string][]models.MockState{}
				for i := 1; i <= 3; i++ {
					m := rec[i-1]
					consumed[fmt.Sprintf("test-%d", i)] = []models.MockState{{Name: m.Name, Kind: models.HTTP, Timestamp: m.Spec.ReqTimestampMock.Unix()}}
				}
				h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, consumed)}
				if status := h.run(t); status != models.TestSetStatusPassed {
					t.Fatalf("status = %q, want PASSED", status)
				}
				if pm.last == nil {
					t.Fatal("no mappings.yaml written")
				}
				health := 0
				for _, e := range pm.last.Worker {
					if strings.HasPrefix(label[e.Name], "health-") {
						health++
					}
				}
				if health < learn.MinEpochReads+1 {
					t.Fatalf("%s: worker kept %d health reads, want >= %d", store, health, learn.MinEpochReads+1)
				}
			})
		}
	})
}

func TestWindowBg_FailClosedRemovedTestsEdgeSkewAndSchedule(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

	t.Run("middle removed test is never promoted", func(t *testing.T) {
		held := []*models.TestCase{
			makeTestCaseWindow("test-1", at(10000), at(12000)),
			makeTestCaseWindow("test-3", at(30000), at(32000)),
		}
		all := []models.TestWindow{
			{TestCase: "test-1", Start: at(10000), End: at(12000)},
			{TestCase: "test-2", Start: at(20000), End: at(22000)},
			{TestCase: "test-3", Start: at(30000), End: at(32000)},
		}
		mocks := []*models.Mock{
			makeMongoPollMock("own-test-1", "users", at(11000), at(11005)),
			makeMongoPollMock("removed-test-2-uitestruns", "uiTestRuns", at(21000), at(21005)),
			makeMongoPollMock("own-test-3", "users", at(31000), at(31005)),
		}
		r := &Replayer{
			logger: zap.NewNop(),
			mockDB: &windowBgMockDB{mocks: mocks},
			testDB: &recordedWindowsTestDB{prTestDB: prTestDB{cases: held}, allWindows: all},
		}
		doc := &models.Mapping{TestSetID: "ts-0"}
		if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", held, doc, doc); err != nil {
			t.Fatal(err)
		}
		for _, e := range append(append([]models.MockEntry(nil), doc.Startup...), doc.Worker...) {
			if e.Name == "removed-test-2-uitestruns" {
				t.Fatalf("removed middle test's own call was promoted: %+v", e)
			}
		}
	})

	t.Run("EdgeDecision owned mocks are never promoted", func(t *testing.T) {
		tcs := []*models.TestCase{
			makeTestCaseWindow("test-1", at(10000), at(10100)),
			makeTestCaseWindow("test-2", at(20000), at(20100)),
		}
		afterEnd := at(10100).Add(50 * time.Microsecond)
		beforeStart := at(20000).Add(-60 * time.Microsecond)
		mocks := []*models.Mock{
			makeMongoPollMock("edge-after-test-1", "uiTestRuns", afterEnd, afterEnd.Add(10*time.Microsecond)),
			makeMongoPollMock("edge-before-test-2", "uiTestRuns", beforeStart, beforeStart.Add(10*time.Microsecond)),
			makeMongoPollMock("true-gap", "ptw_endpoints", at(15000), at(15005)),
		}
		r := &Replayer{
			logger: zap.NewNop(),
			mockDB: &windowBgMockDB{mocks: mocks},
			testDB: &recordedWindowsTestDB{prTestDB: prTestDB{cases: tcs}, allWindows: recordedTestWindows(tcs)},
		}
		doc := &models.Mapping{TestSetID: "ts-0"}
		if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", tcs, doc, doc); err != nil {
			t.Fatal(err)
		}
		got := bgEntryNames(doc.Worker)
		if len(got) != 1 || got[0] != "true-gap" {
			t.Fatalf("Worker = %v, want [true-gap]", got)
		}
	})

	t.Run("descriptive names with and without RecordedWindowsReader", func(t *testing.T) {
		held := []*models.TestCase{
			makeTestCaseWindow("get-cluster-list-1", at(10000), at(12000)),
			makeTestCaseWindow("post-usage-metrics-1", at(20000), at(22000)),
			makeTestCaseWindow("get-apps-1", at(40000), at(42000)),
		}
		allRecorded := []models.TestWindow{
			{TestCase: "get-cluster-list-1", Start: at(10000), End: at(12000)},
			{TestCase: "post-usage-metrics-1", Start: at(20000), End: at(22000)},
			{TestCase: "delete-item-1", Start: at(30000), End: at(32000)},
			{TestCase: "get-apps-1", Start: at(40000), End: at(42000)},
		}
		mocks := []*models.Mock{
			makeMongoPollMock("boot-poll", "ptw_endpoints", at(2000), at(2005)),
			makeMongoPollMock("safe-gap-poll", "ptw_endpoints", at(15000), at(15005)),
			makeMongoPollMock("removed-mid-own", "uiTestRuns", at(31000), at(31005)),
			makeMongoPollMock("adjacent-to-removed", "ptw_endpoints", at(25000), at(25005)),
		}
		rNoSched := &Replayer{logger: zap.NewNop(), mockDB: &windowBgMockDB{mocks: mocks}, testDB: &prTestDB{cases: held}}
		docNoSched := &models.Mapping{TestSetID: "ts-0"}
		_ = rNoSched.populateWindowDerivedBackground(context.Background(), "ts-0", held, docNoSched, docNoSched)
		if len(docNoSched.Startup) != 0 || len(docNoSched.Worker) != 0 || docNoSched.BackgroundDerived {
			t.Fatalf("without RecordedWindowsReader must fail closed and not stamp: startup=%v worker=%v stamped=%v",
				bgEntryNames(docNoSched.Startup), bgEntryNames(docNoSched.Worker), docNoSched.BackgroundDerived)
		}
		rFull := &Replayer{
			logger: zap.NewNop(),
			mockDB: &windowBgMockDB{mocks: mocks},
			testDB: &recordedWindowsTestDB{allWindows: allRecorded},
		}
		docFull := &models.Mapping{TestSetID: "ts-0"}
		_ = rFull.populateWindowDerivedBackground(context.Background(), "ts-0", held, docFull, docFull)
		if got := bgEntryNames(docFull.Startup); len(got) != 1 || got[0] != "boot-poll" {
			t.Fatalf("full schedule Startup = %v, want [boot-poll]", got)
		}
		if got := bgEntryNames(docFull.Worker); len(got) != 1 || got[0] != "safe-gap-poll" {
			t.Fatalf("full schedule Worker = %v, want [safe-gap-poll]", got)
		}
		if !docFull.BackgroundDerived {
			t.Fatal("full schedule derivation must stamp BackgroundDerived=true")
		}
	})
}

func TestWindowBg_SingleDerivationParityAndSafeInPlaceHeal(t *testing.T) {
	t.Setenv(provenswitch.Env, "")

	sendHTTPMiss := func(t *testing.T, a *managedAgent, path string) []models.UnmatchedCall {
		t.Helper()
		a.mm.SetFilteredMocks(nil)
		a.mm.SetUnFilteredMocks(nil)
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
					calls = append(calls, models.UnmatchedCall{Protocol: r.Protocol, ActualSummary: r.ActualSummary, Background: r.Background})
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
			if err := httppkg.New(zap.NewNop()).MockOutgoing(ctx, serverConn,
				&models.ConditionalDstCfg{Addr: "upstream.internal:8080", Port: 8080}, a.mm, models.OutgoingOptions{}); err != nil && !models.IsMismatchReported(err) {
				recordErr(err)
			}
		}()
		_, _ = io.WriteString(clientConn, "GET "+path+" HTTP/1.1\r\nHost: upstream.internal:8080\r\nConnection: close\r\n\r\n")
		_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.ReadAll(clientConn)
		_ = clientConn.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		return calls
	}

	t.Run("heal run in-RAM agent set matches persisted set and excludes test-owned mock", func(t *testing.T) {
		recording := []*models.Mock{
			makeMongoPollMock("boot-ptw-1", "ptw_endpoints", attrAt(500), attrAt(505)),
			makeMongoPollMock("in-test-1", "users", attrAt(1050), attrAt(1055)),
			makeMongoPollMock("between-ptw-1", "ptw_endpoints", attrAt(1500), attrAt(1505)),
			makeMongoPollMock("between-ptw-2", "ptw_endpoints", attrAt(1600), attrAt(1605)),
			makeMongoPollMock("in-test-2", "orders", attrAt(2050), attrAt(2055)),
		}
		stale := &models.Mapping{
			TestSetID: "test-set-0",
			Source:    models.MappingSourceKeployTest,
			TestCases: []models.MappedTestCase{
				{ID: "test-1", Mocks: []models.MockEntry{{Name: "in-test-1"}}},
				{ID: "test-2", Mocks: []models.MockEntry{{Name: "in-test-2"}, {Name: "between-ptw-2"}}},
			},
		}
		h := newPartialRunHarness(t, 2, 0)
		pinWindows(h)
		h.replayer.testDB = &recordedWindowsTestDB{prTestDB: prTestDB{cases: h.cases}, allWindows: recordedTestWindows(h.cases)}
		h.replayer.config.Test.StrictMockWindow = true
		pm := &shMappingDB{prMappingDB: &prMappingDB{exists: true}, mapping: stale}
		h.replayer.mappingDB = pm
		h.replayer.mockDB = &allMocksDB{recording: recording}
		h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{
			"test-1": {{Name: "in-test-1", Kind: models.Mongo, Timestamp: attrAt(1050).Unix()}},
			"test-2": {{Name: "in-test-2", Kind: models.Mongo, Timestamp: attrAt(2050).Unix()}},
		})}
		a := newManagedAgent(t, h.instr)
		h.replayer.instrumentation = a
		if status := h.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("status = %q", status)
		}
		persisted := map[string]bool{}
		if pm.last != nil {
			for _, e := range append(append([]models.MockEntry(nil), pm.last.Startup...), pm.last.Worker...) {
				persisted[e.Name] = true
			}
		}
		inRAM := map[string]bool{}
		for _, n := range agentBackgroundNames(a) {
			inRAM[n] = true
		}
		if persisted["between-ptw-2"] || inRAM["between-ptw-2"] {
			t.Fatalf("test-2-owned between-ptw-2 leaked into background: persisted=%v inRAM=%v", persisted, inRAM)
		}
		if !persisted["between-ptw-1"] || !inRAM["between-ptw-1"] {
			t.Fatalf("between-ptw-1 missing from background: persisted=%v inRAM=%v", persisted, inRAM)
		}
	})

	t.Run("middle removed duplicate never swallows kept test miss in Pass 2", func(t *testing.T) {
		mkHTTP := func(name, path string, req, res time.Time) *models.Mock {
			m := makeHTTPPollMock(name, "/", req, res)
			m.Spec.HTTPReq.URL = "http://upstream.internal:8080" + path
			return m
		}
		recording := []*models.Mock{
			makeMongoPollMock("in-test-1", "orders", attrAt(1050), attrAt(1055)),
			mkHTTP("removed-test-2-uitestruns", "/api/uiTestRuns?run=7", attrAt(2050), attrAt(2055)),
			mkHTTP("own-test-3-uitestruns", "/api/uiTestRuns?run=7", attrAt(3050), attrAt(3055)),
		}
		consumed := map[string][]models.MockState{
			"test-1": {{Name: "in-test-1", Kind: models.Mongo, Timestamp: attrAt(1050).Unix()}},
			"test-3": {{Name: "own-test-3-uitestruns", Kind: models.HTTP, Timestamp: attrAt(3050).Unix()}},
		}
		h1 := newPartialRunHarness(t, 3, 0)
		pinWindows(h1)
		allWins := recordedTestWindows(h1.cases)
		held := []*models.TestCase{h1.cases[0], h1.cases[2]}
		h1.replayer.testDB = &recordedWindowsTestDB{prTestDB: prTestDB{cases: held}, allWindows: allWins}
		h1.instr.cases, h1.cases = held, held
		h1.replayer.config.Test.StrictMockWindow = true
		p1Map := &shMappingDB{prMappingDB: &prMappingDB{exists: false}}
		h1.replayer.mappingDB = p1Map
		h1.replayer.mockDB = &allMocksDB{recording: recording}
		h1.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h1, consumed)}
		h1.replayer.instrumentation = newManagedAgent(t, h1.instr)
		if status := h1.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("Pass 1 status = %q", status)
		}
		if p1Map.last == nil || !p1Map.last.BackgroundDerived || len(p1Map.last.Worker) != 0 {
			t.Fatalf("Pass 1 wrote unexpected mapping: %+v", p1Map.last)
		}
		h2 := newPartialRunHarness(t, 3, 0)
		pinWindows(h2)
		held2 := []*models.TestCase{h2.cases[0], h2.cases[2]}
		h2.replayer.testDB = &recordedWindowsTestDB{prTestDB: prTestDB{cases: held2}, allWindows: allWins}
		h2.instr.cases, h2.cases = held2, held2
		h2.replayer.config.Test.StrictMockWindow = true
		h2.replayer.mappingDB = &shMappingDB{prMappingDB: &prMappingDB{exists: true}, mapping: p1Map.last}
		h2.replayer.mockDB = &allMocksDB{recording: recording}
		a2 := newManagedAgent(t, h2.instr)
		h2.replayer.instrumentation = a2
		if status := h2.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("Pass 2 status = %q", status)
		}
		own := sendHTTPMiss(t, a2, "/api/uiTestRuns?run=7")
		if len(own) != 1 || own[0].Background || !own[0].CountsAgainstTest() {
			t.Fatalf("test-3's own miss was swallowed as background: %+v", own)
		}
	})

	t.Run("user clearing worker on stamped keploy-written file is never re-healed (kills R10/R11)", func(t *testing.T) {
		rec := []*models.Mock{
			makeMongoPollMock("boot-ptw-1", "ptw_endpoints", attrAt(500), attrAt(505)),
			makeMongoPollMock("in-test-1", "users", attrAt(1050), attrAt(1055)),
			makeMongoPollMock("between-ptw-1", "ptw_endpoints", attrAt(1500), attrAt(1505)),
			makeMongoPollMock("between-ptw-2", "ptw_endpoints", attrAt(1600), attrAt(1605)),
			makeMongoPollMock("in-test-2", "orders", attrAt(2050), attrAt(2055)),
		}
		consumedFor := func(h *prRun) func() []models.MockState {
			return consumedByWindow(h, map[string][]models.MockState{
				"test-1": {{Name: "in-test-1", Kind: models.Mongo, Timestamp: attrAt(1050).Unix()}},
				"test-2": {{Name: "in-test-2", Kind: models.Mongo, Timestamp: attrAt(2050).Unix()}},
			})
		}
		h1 := newPartialRunHarness(t, 2, 0)
		pinWindows(h1)
		allWins := recordedTestWindows(h1.cases)
		h1.replayer.testDB = &recordedWindowsTestDB{prTestDB: prTestDB{cases: h1.cases}, allWindows: allWins}
		h1.replayer.config.Test.StrictMockWindow = true
		h1.replayer.mappingDB = mapdb.New(zap.NewNop(), h1.replayer.config.Path, "")
		h1.replayer.mockDB = &allMocksDB{recording: rec}
		h1.replayer.hookImpl = prHooks{consumedFor: consumedFor(h1)}
		if status := h1.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("Pass 1 status = %q", status)
		}
		path := filepath.Join(h1.replayer.config.Path, "test-set-0", "mappings.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc yamlLib.Node
		if err := yamlLib.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		root := doc.Content[0]
		kept := root.Content[:0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == "worker" {
				continue
			}
			kept = append(kept, root.Content[i], root.Content[i+1])
		}
		root.Content = kept
		root.Content[0].HeadComment = "# OPS: removed worker on purpose"
		var buf bytes.Buffer
		enc := yamlLib.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(&doc); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}

		h2 := newPartialRunHarness(t, 2, 0)
		pinWindows(h2)
		h2.replayer.testDB = &recordedWindowsTestDB{prTestDB: prTestDB{cases: h2.cases}, allWindows: allWins}
		h2.replayer.config.Path = h1.replayer.config.Path
		h2.replayer.config.Test.StrictMockWindow = true
		db2 := mapdb.New(zap.NewNop(), h1.replayer.config.Path, "")
		h2.replayer.mappingDB = db2
		h2.replayer.mockDB = &allMocksDB{recording: rec}
		h2.replayer.hookImpl = prHooks{consumedFor: consumedFor(h2)}
		if _, _, derive := h2.replayer.backgroundMocksOf(context.Background(), "test-set-0"); derive {
			t.Fatal("Pass 2 unexpectedly set derive=true after user cleared worker:")
		}
		if status := h2.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("Pass 2 status = %q", status)
		}
		m, err := db2.GetMapping(context.Background(), "test-set-0")
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Worker) != 0 || !m.BackgroundDerived {
			t.Fatalf("user removal of worker: was undone: Worker=%v BackgroundDerived=%v", bgEntryNames(m.Worker), m.BackgroundDerived)
		}
	})

	t.Run("heal preserves order and comments and respects non-empty worker", func(t *testing.T) {
		rec := []*models.Mock{
			makeMongoPollMock("boot-ptw-1", "ptw_endpoints", attrAt(500), attrAt(505)),
			makeMongoPollMock("in-test-1", "users", attrAt(1050), attrAt(1055)),
			makeMongoPollMock("between-ptw-1", "ptw_endpoints", attrAt(1500), attrAt(1505)),
			makeMongoPollMock("between-ptw-2", "ptw_endpoints", attrAt(1600), attrAt(1605)),
			makeMongoPollMock("in-test-2", "orders", attrAt(2050), attrAt(2055)),
		}
		consumedFor := func(h *prRun) func() []models.MockState {
			return consumedByWindow(h, map[string][]models.MockState{
				"test-1": {{Name: "in-test-1", Kind: models.Mongo, Timestamp: attrAt(1050).Unix()}},
				"test-2": {{Name: "in-test-2", Kind: models.Mongo, Timestamp: attrAt(2050).Unix()}},
			})
		}
		hOrder := newPartialRunHarness(t, 2, 0)
		pinWindows(hOrder)
		hOrder.replayer.testDB = &recordedWindowsTestDB{prTestDB: prTestDB{cases: hOrder.cases}, allWindows: recordedTestWindows(hOrder.cases)}
		hOrder.replayer.config.Test.StrictMockWindow = true
		pOrder := filepath.Join(hOrder.replayer.config.Path, "test-set-0", "mappings.yaml")
		bodyOrder := "# team note: tests in run order\nversion: api.keploy.io/v1beta1\nsource: keploy test\ntests:\n  # test-2 runs first\n  - id: test-2\n    mocks:\n      - name: in-test-2\n        kind: Mongo\n  - id: test-1\n    mocks:\n      - name: in-test-1\n        kind: Mongo\nkind: TestMocksMapping\ntest_set_id: test-set-0\n"
		if err := os.WriteFile(pOrder, []byte(bodyOrder), 0o644); err != nil {
			t.Fatal(err)
		}
		dbOrder := mapdb.New(zap.NewNop(), hOrder.replayer.config.Path, "")
		hOrder.replayer.mappingDB = dbOrder
		hOrder.replayer.mockDB = &allMocksDB{recording: rec}
		hOrder.replayer.hookImpl = prHooks{consumedFor: consumedFor(hOrder)}
		if status := hOrder.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("status = %q", status)
		}
		afterOrder, _ := os.ReadFile(pOrder)
		s := string(afterOrder)
		if !strings.Contains(s, "team note") || !strings.Contains(s, "test-2 runs first") {
			t.Fatalf("heal dropped comments:\n%s", s)
		}
		if i2, i1 := strings.Index(s, "id: test-2"), strings.Index(s, "id: test-1"); i2 < 0 || i1 < 0 || i2 > i1 {
			t.Fatalf("heal reordered tests: test-2=%d test-1=%d\n%s", i2, i1, s)
		}
		mOrder, err := dbOrder.GetMapping(context.Background(), "test-set-0")
		if err != nil {
			t.Fatal(err)
		}
		if !mOrder.BackgroundDerived || len(mOrder.Worker) != 2 {
			t.Fatalf("healed doc BackgroundDerived=%v Worker=%v, want true and 2 entries", mOrder.BackgroundDerived, bgEntryNames(mOrder.Worker))
		}
	})

	t.Run("heal never writes a mock pruned in the same run (kills R14/R15/R24)", func(t *testing.T) {
		timeout := prClientTimeoutErr(t)
		for _, exists := range []bool{true, false} {
			t.Run(fmt.Sprintf("exists=%v", exists), func(t *testing.T) {
				h, set, _, _ := noAnswerHoldRun(t, "yaml")
				h.replayer.testDB = &recordedWindowsTestDB{prTestDB: prTestDB{cases: h.cases}, allWindows: recordedTestWindows(h.cases)}
				hooks := h.replayer.hookImpl.(prHooks)
				hooks.simErr = map[string]error{"test-7": timeout}
				h.replayer.hookImpl = hooks
				h.mappings.exists = exists
				var onDisk *models.Mapping
				if exists {
					onDisk = &models.Mapping{
						TestSetID: "test-set-0",
						Source:    models.MappingSourceKeployTest,
						TestCases: []models.MappedTestCase{{ID: "test-7", Mocks: []models.MockEntry{{Name: set.mapped.Name, Kind: string(models.HTTP)}}}},
					}
				}
				pm := &shMappingDB{prMappingDB: h.mappings, mapping: onDisk}
				h.replayer.mappingDB = pm
				_ = h.run(t)
				left := storedMockNames(t, h)
				if pm.last == nil {
					return
				}
				for _, e := range pm.last.Worker {
					if !left[e.Name] {
						t.Fatalf("worker: names %s which the same run pruned", e.Name)
					}
				}
			})
		}
	})
}

func TestWindowBg_CollectorEvictionProtectsPollersAndRecency(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

	t.Run("burst of one-offs never cuts active poller below MinEpochReads+1", func(t *testing.T) {
		tcs := []*models.TestCase{
			makeTestCaseWindow("test-1", at(10000), at(11000)),
			makeTestCaseWindow("test-2", at(20000), at(21000)),
			makeTestCaseWindow("test-3", at(30000), at(31000)),
			makeTestCaseWindow("test-4", at(40000), at(41000)),
		}
		var mocks []*models.Mock
		for gap := 0; gap < 3; gap++ {
			for i := 0; i < 7; i++ {
				ts := at(12000 + gap*10000 + i*1000)
				mocks = append(mocks, makeMongoPollMock(fmt.Sprintf("poll-g%d-%d", gap, i), "ptw_endpoints", ts, ts.Add(time.Millisecond)))
			}
		}
		for i := 0; i < 300; i++ {
			ts := at(39000).Add(time.Duration(i) * time.Millisecond)
			m := makeHTTPPollMock(fmt.Sprintf("oneoff-%03d", i), fmt.Sprintf("/items/%d", i), ts, ts.Add(100*time.Microsecond))
			mocks = append(mocks, m)
		}
		r := &Replayer{
			logger: zap.NewNop(),
			mockDB: &windowBgMockDB{mocks: mocks},
			testDB: &recordedWindowsTestDB{prTestDB: prTestDB{cases: tcs}, allWindows: recordedTestWindows(tcs)},
		}
		doc := &models.Mapping{TestSetID: "ts-0"}
		if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", tcs, doc, doc); err != nil {
			t.Fatal(err)
		}
		polls := 0
		for _, e := range doc.Worker {
			if strings.HasPrefix(e.Name, "poll-") {
				polls++
			}
		}
		if polls < learn.MinEpochReads+1 {
			t.Fatalf("active poller kept %d samples, want >= %d", polls, learn.MinEpochReads+1)
		}
	})

	t.Run("head reserve samples never leave a shape below MinPerShape", func(t *testing.T) {
		col := models.NewDerivedBgCollector(models.DerivedBgCollectorConfig{
			MaxEntries:  windowDerivedMaxPerTier,
			MaxPerShape: windowDerivedMaxPerShape,
			MinPerShape: learn.MinEpochReads + 1,
			HeadReserve: windowDerivedStartupReserve,
		})
		seq := 0
		add := func(name, shape string) {
			seq++
			col.Add(models.DerivedBgItem{Entry: models.MockEntry{Name: name}, Shape: shape, At: at(seq)})
		}
		for k := 0; k < 3; k++ {
			add(fmt.Sprintf("boot-poll-head-%d", k), "boot-poll")
		}
		for k := 0; k < windowDerivedStartupReserve-3; k++ {
			add(fmt.Sprintf("head-filler-%d", k), fmt.Sprintf("hf-%d", k))
		}
		for k := 0; k < 2; k++ {
			add(fmt.Sprintf("boot-poll-tail-%d", k), "boot-poll")
		}
		for s := 0; s < 80; s++ {
			for k := 0; k < learn.MinEpochReads+1; k++ {
				add(fmt.Sprintf("multi-%02d-%d", s, k), fmt.Sprintf("ms-%02d", s))
			}
		}
		n := 0
		for _, e := range col.Entries() {
			if strings.HasPrefix(e.Name, "boot-poll-") {
				n++
			}
		}
		if n > 0 && n < learn.MinEpochReads+1 {
			t.Fatalf("boot-poll cut to %d samples (< MinPerShape %d)", n, learn.MinEpochReads+1)
		}
	})

	t.Run("multi-sample eviction drops whole shapes by recency not first-seen (kills N1 N2 N6)", func(t *testing.T) {
		tcs := []*models.TestCase{
			makeTestCaseWindow("test-1", at(1000), at(2000)),
			makeTestCaseWindow("test-2", at(900000), at(901000)),
		}
		var mocks []*models.Mock
		ts := at(3000)
		nextTS := func() time.Time {
			ts = ts.Add(10 * time.Millisecond)
			return ts
		}
		for k := 0; k < 4; k++ {
			mocks = append(mocks, makeMongoPollMock(fmt.Sprintf("shapeA-%d", k), "coll_A", nextTS(), ts.Add(time.Millisecond)))
		}
		for s := 0; s < 20; s++ {
			for k := 0; k < 4; k++ {
				mocks = append(mocks, makeMongoPollMock(fmt.Sprintf("stale-%02d-%d", s, k), fmt.Sprintf("coll_stale_%02d", s), nextTS(), ts.Add(time.Millisecond)))
			}
		}
		mocks = append(mocks, makeMongoPollMock("shapeA-refresh", "coll_A", nextTS(), ts.Add(time.Millisecond)))
		for s := 0; s < 60; s++ {
			for k := 0; k < 4; k++ {
				mocks = append(mocks, makeMongoPollMock(fmt.Sprintf("newer-%02d-%d", s, k), fmt.Sprintf("coll_newer_%02d", s), nextTS(), ts.Add(time.Millisecond)))
			}
		}
		r := &Replayer{
			logger: zap.NewNop(),
			mockDB: &windowBgMockDB{mocks: mocks},
			testDB: &recordedWindowsTestDB{prTestDB: prTestDB{cases: tcs}, allWindows: recordedTestWindows(tcs)},
		}
		doc := &models.Mapping{TestSetID: "ts-0"}
		if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", tcs, doc, doc); err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, e := range doc.Worker {
			prefix := e.Name[:strings.LastIndexByte(e.Name, '-')]
			counts[prefix]++
		}
		if counts["shapeA"] < learn.MinEpochReads+1 {
			t.Fatalf("refreshed shapeA kept %d samples, want >= %d", counts["shapeA"], learn.MinEpochReads+1)
		}
		var crippled []string
		for k, n := range counts {
			if n > 0 && n < learn.MinEpochReads+1 {
				crippled = append(crippled, fmt.Sprintf("%s=%d", k, n))
			}
		}
		sort.Strings(crippled)
		if len(crippled) > 0 {
			t.Fatalf("multi-sample shapes were crippled below MinPerShape=%d: %v", learn.MinEpochReads+1, crippled)
		}
	})
}

type stubCommentCheckerMappingDB struct {
	shMappingDB
	commented bool
	err       error
}

func (db *stubCommentCheckerMappingDB) SourceLineCommented(context.Context, string) (bool, error) {
	return db.commented, db.err
}

// TestWindowBg_ClassifierAndEligibilityEdgeGuards pins every remaining
// fail-closed boundary across WindowClassifier, windowBgEligibility,
// recordedSetShapeWith, and partial no-answer persistence/pruning.
func TestWindowBg_ClassifierAndEligibilityEdgeGuards(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

	t.Run("startup boundary is strictly exclusive and earlier RecordedFirstStart narrows prefix", func(t *testing.T) {
		held := []models.TestWindow{
			{TestCase: "get-b-1", Start: at(20), End: at(21)},
			{TestCase: "get-c-1", Start: at(30), End: at(31)},
		}
		clf := models.NewWindowClassifierForBackground(held, models.BackgroundWindowOptions{
			RecordedStartKnown: true,
			RecordedFirstStart: at(10),
		})
		// Strictly before RecordedFirstStart (10s) -> WindowBgStartup.
		boot := makeMongoPollMock("boot-5s", "ptw_endpoints", at(5), at(5).Add(5*time.Millisecond))
		if k, _ := clf.ClassifyForBackground(boot, at(10)); k != models.WindowBgStartup {
			t.Fatalf("5s mock = %v, want WindowBgStartup", k)
		}
		// Exactly AT RecordedFirstStart (10s) -> exclusive boundary -> WindowBgNone (kills E1).
		atStart := makeMongoPollMock("at-10s", "ptw_endpoints", at(10), at(10).Add(5*time.Millisecond))
		if k, _ := clf.ClassifyForBackground(atStart, at(10)); k != models.WindowBgNone {
			t.Fatalf("exact RecordedFirstStart boundary = %v, want WindowBgNone", k)
		}
		// Between RecordedFirstStart (10s) and heldFirst (20s) -> WindowBgNone (kills N4).
		inEarlier := makeMongoPollMock("in-15s", "ptw_endpoints", at(15), at(15).Add(5*time.Millisecond))
		if k, _ := clf.ClassifyForBackground(inEarlier, at(10)); k != models.WindowBgNone {
			t.Fatalf("mock between RecordedFirstStart and heldFirst = %v, want WindowBgNone", k)
		}
	})

	t.Run("ScheduleIncomplete, RecordedStartUnreadable, and RefusalWindows fail closed even with other fields set", func(t *testing.T) {
		held := []models.TestWindow{
			{TestCase: "get-a-1", Start: at(20), End: at(21)},
			{TestCase: "get-b-1", Start: at(30), End: at(31)},
		}
		between := makeMongoPollMock("between-25s", "ptw_endpoints", at(25), at(25).Add(5*time.Millisecond))
		boot := makeMongoPollMock("boot-5s", "ptw_endpoints", at(5), at(5).Add(5*time.Millisecond))
		inDeletedPrefix := makeMongoPollMock("in-del-11s", "orders", at(11), at(11).Add(5*time.Millisecond))

		// ScheduleIncomplete overrides even a supplied RecordedWindows slice (kills R08).
		clfInc := models.NewWindowClassifierForBackground(held, models.BackgroundWindowOptions{
			RecordedWindows:    held,
			ScheduleIncomplete: true,
		})
		if clfInc.HasFullSchedule() {
			t.Fatal("ScheduleIncomplete=true reported HasFullSchedule=true")
		}
		if k, _ := clfInc.ClassifyForBackground(between, at(20)); k != models.WindowBgNone {
			t.Fatalf("ScheduleIncomplete classified between mock as %v, want WindowBgNone", k)
		}

		// RecordedStartUnreadable overrides RecordedStartKnown (kills R09).
		clfUnr := models.NewWindowClassifierForBackground(held, models.BackgroundWindowOptions{
			RecordedStartKnown:      true,
			RecordedFirstStart:      at(20),
			RecordedStartUnreadable: true,
		})
		if k, _ := clfUnr.ClassifyForBackground(boot, at(20)); k != models.WindowBgNone {
			t.Fatalf("RecordedStartUnreadable classified boot mock as %v, want WindowBgNone", k)
		}

		// RefusalWindows without full RecordedWindows blocks prefix calls inside deleted windows (kills R5_08).
		clfRef := models.NewWindowClassifierForBackground(held, models.BackgroundWindowOptions{
			RecordedStartKnown: true,
			RecordedFirstStart: at(20),
			RefusalWindows:     []models.TestWindow{{TestCase: "del-0", Start: at(10), End: at(12)}},
		})
		if k, _ := clfRef.ClassifyForBackground(inDeletedPrefix, at(20)); k != models.WindowBgNone {
			t.Fatalf("mock inside RefusalWindows classified as %v, want WindowBgNone", k)
		}
	})

	t.Run("windowBgEligibility and healability guards reject errors, non-empty worker, stamp, and source comments", func(t *testing.T) {
		ctx := context.Background()
		r := &Replayer{
			logger:    zap.NewNop(),
			mappingDB: &shMappingDB{prMappingDB: &prMappingDB{exists: false}},
		}
		// Exists error -> fail closed (kills MX5).
		if allow, _, _ := r.windowBgEligibility(ctx, "ts-0", func() (bool, error) {
			return false, errors.New("stat failed")
		}); allow {
			t.Fatal("windowBgEligibility returned allow=true on Exists error")
		}
		// Non-empty Worker -> not healable (kills N8).
		if isHealableKeployTestMapping(&models.Mapping{
			Source: models.MappingSourceKeployTest,
			Worker: []models.MockEntry{{Name: "w-1", Kind: string(models.Mongo)}},
		}) {
			t.Fatal("isHealableKeployTestMapping returned true when Worker is non-empty")
		}
		// BackgroundDerived=true -> not healable (kills R11).
		if isHealableKeployTestMapping(&models.Mapping{
			Source:            models.MappingSourceKeployTest,
			BackgroundDerived: true,
		}) {
			t.Fatal("isHealableKeployTestMapping returned true when BackgroundDerived=true")
		}
		// Commented source: or check error -> not healable (kills R13).
		for _, tc := range []struct {
			name      string
			commented bool
			err       error
		}{
			{"commented", true, nil},
			{"error", false, errors.New("read failed")},
		} {
			rCheck := &Replayer{
				logger: zap.NewNop(),
				mappingDB: &stubCommentCheckerMappingDB{
					commented: tc.commented,
					err:       tc.err,
				},
			}
			if rCheck.isHealableKeployTestMappingDoc(ctx, "ts-0", &models.Mapping{Source: models.MappingSourceKeployTest}) {
				t.Fatalf("isHealableKeployTestMappingDoc(%s) = true, want false", tc.name)
			}
		}
	})

	t.Run("recordedSetShapeWith clears deriveBackground when full proof saw pass even with empty derived set", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		h := newPartialRunHarness(t, 2, 0)
		pinWindows(h)
		h.replayer.mappingDB = &shMappingDB{prMappingDB: &prMappingDB{exists: false}}
		h.replayer.testDB = &recordedWindowsTestDB{prTestDB: prTestDB{cases: h.cases}, allWindows: recordedTestWindows(h.cases)}
		tracker := newWindowBgTracker(h.cases, h.replayer.backgroundWindowOptions(context.Background(), "test-set-0", h.cases), zap.NewNop(), "test-set-0")
		tracker.markPass() // saw pass with 0 outside-window mocks
		shape := h.replayer.recordedSetShapeWith(context.Background(), "test-set-0", h.cases, tracker, nil, nil)
		if shape.deriveBackground {
			t.Fatal("recordedSetShapeWith left deriveBackground=true despite sawPass && hasFullProof (kills R17)")
		}
	})

	t.Run("partial no-answer run with shouldWriteMappings=true neither heals nor populates window background (kills E3/R14/R15)", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		timeout := prClientTimeoutErr(t)
		for _, exists := range []bool{true, false} {
			t.Run(fmt.Sprintf("exists=%v", exists), func(t *testing.T) {
				h := newPartialRunHarness(t, 2, 0)
				pinWindows(h)
				h.replayer.testDB = &recordedWindowsTestDB{prTestDB: prTestDB{cases: h.cases}, allWindows: recordedTestWindows(h.cases)}
				h.replayer.config.Test.UpdateTestMapping = false
				h.replayer.config.Test.StrictMockWindow = true
				own1 := makeMongoPollMock("own-1", "users", attrAt(1050), attrAt(1055))
				between := makeMongoPollMock("between-ptw-1", "ptw_endpoints", attrAt(1500), attrAt(1505))
				own2 := makeMongoPollMock("own-2", "orders", attrAt(2050), attrAt(2055))
				h.replayer.mockDB = &allMocksDB{recording: []*models.Mock{own1, between, own2}}
				h.mappings.exists = exists
				var onDisk *models.Mapping
				if exists {
					onDisk = &models.Mapping{
						TestSetID: "test-set-0",
						Source:    models.MappingSourceKeployTest,
						TestCases: []models.MappedTestCase{
							{ID: "test-1", Mocks: []models.MockEntry{{Name: "own-1", Kind: string(models.Mongo)}}},
							{ID: "test-2", Mocks: []models.MockEntry{{Name: "own-2", Kind: string(models.Mongo)}}},
						},
					}
					h.mappings.onDisk = map[string][]models.MockEntry{
						"test-1": {{Name: "own-1", Kind: string(models.Mongo)}},
						"test-2": {{Name: "own-2", Kind: string(models.Mongo)}},
					}
				}
				pm := &shMappingDB{prMappingDB: h.mappings, mapping: onDisk}
				h.replayer.mappingDB = pm
				h.replayer.hookImpl = prHooks{
					simErr: map[string]error{"test-2": timeout},
					consumedFor: consumedByWindow(h, map[string][]models.MockState{
						"test-1": {{Name: "own-1", Kind: models.Mongo, Timestamp: attrAt(1050).Unix()}},
					}),
				}
				_ = h.run(t)
				if pm.last != nil && (len(pm.last.Worker) > 0 || pm.last.BackgroundDerived) {
					t.Fatalf("exists=%v no-answer run wrote window-derived background (Worker=%v BackgroundDerived=%v), want none",
						exists, bgEntryNames(pm.last.Worker), pm.last.BackgroundDerived)
				}
			})
		}
	})
}
