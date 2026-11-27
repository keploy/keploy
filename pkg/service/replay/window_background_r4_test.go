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
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
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

// Indexed YAML mocks (LeavePayloadOnDisk) must preserve both BackgroundShapeKey
// and SpecificBackgroundShapeKey so per-shape capping and unconsumed-shape
// exclusion behave identically on the real YAML store and on whole mocks.
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

// Fail-closed classification: removed middle test, unreadable recorded start,
// removed prefix without RecordedStartReader, EdgeDecision owned zone, strict
// startup boundary, and descriptive names with/without RecordedWindowsReader.
func TestWindowBg_FailClosedRemovedTestsEdgeSkewAndSchedule(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

	t.Run("middle removed test is never promoted", func(t *testing.T) {
		held := []*models.TestCase{
			makeTestCaseWindow("test-1", at(10000), at(12000)),
			makeTestCaseWindow("test-3", at(30000), at(32000)),
		}
		mocks := []*models.Mock{
			makeMongoPollMock("own-test-1", "users", at(11000), at(11005)),
			makeMongoPollMock("removed-test-2-uitestruns", "uiTestRuns", at(21000), at(21005)),
			makeMongoPollMock("own-test-3", "users", at(31000), at(31005)),
		}
		r := &Replayer{
			logger: zap.NewNop(),
			mockDB: &windowBgMockDB{mocks: mocks},
			testDB: &recordedStartDB{at: at(10000)},
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

	t.Run("unreadable recorded start promotes nothing in prefix", func(t *testing.T) {
		held := []*models.TestCase{
			makeTestCaseWindow("test-2", at(20000), at(22000)),
		}
		mocks := []*models.Mock{
			makeMongoPollMock("removed-test-1-uitestruns", "uiTestRuns", at(11000), at(11005)),
		}
		r := &Replayer{
			logger: zap.NewNop(),
			mockDB: &windowBgMockDB{mocks: mocks},
			testDB: &recordedStartDB{err: errors.New("test store unreachable")},
		}
		doc := &models.Mapping{TestSetID: "ts-0"}
		if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", held, doc, doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Startup) != 0 || len(doc.Worker) != 0 {
			t.Fatalf("unreadable recorded start promoted startup=%v worker=%v; want empty", bgEntryNames(doc.Startup), bgEntryNames(doc.Worker))
		}
	})

	t.Run("removed prefix classifies alike with and without RecordedStartReader", func(t *testing.T) {
		held := []*models.TestCase{
			makeTestCaseWindow("test-2", at(20000), at(22000)),
			makeTestCaseWindow("test-3", at(30000), at(32000)),
		}
		mocks := []*models.Mock{
			makeMongoPollMock("boot", "ptw_endpoints", at(2000), at(2005)),
			makeMongoPollMock("removed-test-1-uitestruns", "uiTestRuns", at(11000), at(11005)),
			// Boundary exactly at effectiveFirst (10s) must NEVER be startup (kills E1 mutant).
			makeMongoPollMock("at-recorded-first", "uiTestRuns", at(10000), at(10005)),
		}
		runWith := func(db TestDB) map[string]string {
			r := &Replayer{logger: zap.NewNop(), mockDB: &windowBgMockDB{mocks: mocks}, testDB: db}
			doc := &models.Mapping{TestSetID: "ts-0"}
			if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", held, doc, doc); err != nil {
				t.Fatal(err)
			}
			where := map[string]string{}
			for _, e := range doc.Startup {
				where[e.Name] = "startup"
			}
			for _, e := range doc.Worker {
				where[e.Name] = "worker"
			}
			return where
		}
		withReader := runWith(&recordedStartDB{at: at(10000)})
		without := runWith(&prTestDB{cases: held})
		if withReader["removed-test-1-uitestruns"] != "" || without["removed-test-1-uitestruns"] != "" {
			t.Fatalf("removed-test-1 promoted: withReader=%q without=%q", withReader["removed-test-1-uitestruns"], without["removed-test-1-uitestruns"])
		}
		if withReader["at-recorded-first"] != "" {
			t.Fatalf("mock at exact effectiveFirst was promoted to %q; want empty", withReader["at-recorded-first"])
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
		r := &Replayer{logger: zap.NewNop(), mockDB: &windowBgMockDB{mocks: mocks}, testDB: &prTestDB{cases: tcs}}
		doc := &models.Mapping{TestSetID: "ts-0"}
		if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", tcs, doc, doc); err != nil {
			t.Fatal(err)
		}
		got := bgEntryNames(doc.Worker)
		if len(got) != 1 || got[0] != "true-gap" {
			t.Fatalf("Worker = %v, want [true-gap] (edge-owned mocks must not be promoted)", got)
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
			// Removed middle test between post-usage-metrics-1 and get-apps-1:
			{TestCase: "delete-item-1", Start: at(30000), End: at(32000)},
			{TestCase: "get-apps-1", Start: at(40000), End: at(42000)},
		}
		mocks := []*models.Mock{
			makeMongoPollMock("boot-poll", "ptw_endpoints", at(2000), at(2005)),
			makeMongoPollMock("safe-gap-poll", "ptw_endpoints", at(15000), at(15005)),
			makeMongoPollMock("removed-mid-own", "uiTestRuns", at(31000), at(31005)),
			makeMongoPollMock("adjacent-to-removed", "ptw_endpoints", at(25000), at(25005)),
		}
		// Without RecordedWindowsReader: fail closed on descriptive non-sequential names.
		rNoSched := &Replayer{logger: zap.NewNop(), mockDB: &windowBgMockDB{mocks: mocks}, testDB: &prTestDB{cases: held}}
		docNoSched := &models.Mapping{TestSetID: "ts-0"}
		_ = rNoSched.populateWindowDerivedBackground(context.Background(), "ts-0", held, docNoSched, docNoSched)
		if len(docNoSched.Startup) != 0 || len(docNoSched.Worker) != 0 {
			t.Fatalf("without RecordedWindowsReader descriptive names must fail closed; got startup=%v worker=%v",
				bgEntryNames(docNoSched.Startup), bgEntryNames(docNoSched.Worker))
		}
		// With RecordedWindowsReader error: fail closed.
		rErr := &Replayer{logger: zap.NewNop(), mockDB: &windowBgMockDB{mocks: mocks}, testDB: &recordedWindowsTestDB{winErr: errors.New("boom")}}
		docErr := &models.Mapping{TestSetID: "ts-0"}
		_ = rErr.populateWindowDerivedBackground(context.Background(), "ts-0", held, docErr, docErr)
		if len(docErr.Startup) != 0 || len(docErr.Worker) != 0 {
			t.Fatalf("RecordedWindowsReader error must fail closed; got startup=%v worker=%v",
				bgEntryNames(docErr.Startup), bgEntryNames(docErr.Worker))
		}
		// With full RecordedWindowsReader: boot + safe adjacent gap promoted; removed test and its adjacent gap refused.
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
	})
}

// End-to-end single derivation parity (Pass 1 in RAM == persisted mappings.yaml),
// middle-removed test never causing false pass in Pass 2, and heal preserving
// user edits, comments, ordering, and non-empty worker lists.
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
		for _, k := range []models.Kind{models.Mongo, models.HTTP, models.HTTP2} {
			for _, rec := range a.mm.BackgroundRecordings(k) {
				inRAM[rec.Mock.Name] = true
			}
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
		held := []*models.TestCase{h1.cases[0], h1.cases[2]}
		h1.replayer.testDB = &prTestDB{cases: held}
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
		if p1Map.last == nil {
			t.Fatal("Pass 1 wrote no mappings.yaml")
		}
		if len(p1Map.last.Worker) != 0 {
			t.Fatalf("Pass 1 promoted removed test-2's call to worker: %v", bgEntryNames(p1Map.last.Worker))
		}
		h2 := newPartialRunHarness(t, 3, 0)
		pinWindows(h2)
		held2 := []*models.TestCase{h2.cases[0], h2.cases[2]}
		h2.replayer.testDB = &prTestDB{cases: held2}
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

	t.Run("user clearing worker on keploy-written file is never re-healed or re-derived", func(t *testing.T) {
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
		if len(m.Worker) != 0 {
			t.Fatalf("user removal of worker: was undone: %v", bgEntryNames(m.Worker))
		}
		after, _ := os.ReadFile(path)
		if !strings.Contains(string(after), "OPS: removed worker on purpose") {
			t.Fatalf("user comment was lost:\n%s", after)
		}

		// Stamped file with NO startup: entries (only BackgroundDerived=true)
		// must also never re-heal or re-derive after user empties worker:.
		stampedNoStartup := &models.Mapping{
			TestSetID:         "test-set-0",
			Source:            models.MappingSourceKeployTest,
			BackgroundDerived: true,
			TestCases: []models.MappedTestCase{
				{ID: "test-1", Mocks: []models.MockEntry{{Name: "in-test-1"}}},
				{ID: "test-2", Mocks: []models.MockEntry{{Name: "in-test-2"}}},
			},
		}
		hStamp := newPartialRunHarness(t, 2, 0)
		pinWindows(hStamp)
		hStamp.replayer.config.Test.StrictMockWindow = true
		pmStamp := &shMappingDB{prMappingDB: &prMappingDB{exists: true}, mapping: stampedNoStartup}
		hStamp.replayer.mappingDB = pmStamp
		hStamp.replayer.mockDB = &allMocksDB{recording: rec}
		hStamp.replayer.hookImpl = prHooks{consumedFor: consumedFor(hStamp)}
		if _, _, derive := hStamp.replayer.backgroundMocksOf(context.Background(), "test-set-0"); derive {
			t.Fatal("stamped file with empty startup: and worker: unexpectedly set derive=true")
		}
		if status := hStamp.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("stamped status = %q", status)
		}
		if pmStamp.last != nil {
			t.Fatalf("stamped file with empty startup: and worker: was unexpectedly re-healed: %+v", pmStamp.last)
		}
	})

	t.Run("heal respects source variants, non-empty worker, and preserves order and comments", func(t *testing.T) {
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

		// Trailing comment on source: line must NOT be healed.
		hComment := newPartialRunHarness(t, 2, 0)
		pinWindows(hComment)
		hComment.replayer.config.Test.StrictMockWindow = true
		pComment := filepath.Join(hComment.replayer.config.Path, "test-set-0", "mappings.yaml")
		bodyComment := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: test-set-0\nsource: keploy test # ops: hand-curated\ntests:\n  - id: test-1\n    mocks:\n      - name: in-test-1\n        kind: Mongo\n"
		if err := os.WriteFile(pComment, []byte(bodyComment), 0o644); err != nil {
			t.Fatal(err)
		}
		dbComment := mapdb.New(zap.NewNop(), hComment.replayer.config.Path, "")
		hComment.replayer.mappingDB = dbComment
		hComment.replayer.mockDB = &allMocksDB{recording: rec}
		hComment.replayer.hookImpl = prHooks{consumedFor: consumedFor(hComment)}
		if status := hComment.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("status = %q", status)
		}
		mComment, err := dbComment.GetMapping(context.Background(), "test-set-0")
		if err != nil {
			t.Fatal(err)
		}
		if len(mComment.Worker) != 0 {
			t.Fatalf("commented source line was healed: %v", bgEntryNames(mComment.Worker))
		}

		// Unstamped pre-PR file heals in place keeping comments and test-2 before test-1 order.
		hOrder := newPartialRunHarness(t, 2, 0)
		pinWindows(hOrder)
		hOrder.replayer.config.Test.StrictMockWindow = true
		pOrder := filepath.Join(hOrder.replayer.config.Path, "test-set-0", "mappings.yaml")
		bodyOrder := "# team note: tests in run order\nsource: keploy test\ntests:\n  # test-2 runs first\n  - id: test-2\n    mocks:\n      - name: in-test-2\n        kind: Mongo\n  - id: test-1\n    mocks:\n      - name: in-test-1\n        kind: Mongo\nversion: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: test-set-0\n"
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

		// Non-empty hand-written worker list on source: keploy test is NEVER overwritten (kills N8).
		userWorker := []models.MockEntry{{Name: "between-ptw-1", Kind: "Mongo", Reason: "ops: our poller"}}
		existing := &models.Mapping{
			TestSetID: "test-set-0",
			Source:    models.MappingSourceKeployTest,
			Worker:    userWorker,
			TestCases: []models.MappedTestCase{
				{ID: "test-1", Mocks: []models.MockEntry{{Name: "in-test-1"}}},
				{ID: "test-2", Mocks: []models.MockEntry{{Name: "in-test-2"}}},
			},
		}
		hNonEmpty := newPartialRunHarness(t, 2, 0)
		pinWindows(hNonEmpty)
		hNonEmpty.replayer.config.Test.StrictMockWindow = true
		pmNonEmpty := &shMappingDB{prMappingDB: &prMappingDB{exists: true}, mapping: existing}
		hNonEmpty.replayer.mappingDB = pmNonEmpty
		hNonEmpty.replayer.mockDB = &allMocksDB{recording: rec}
		hNonEmpty.replayer.hookImpl = prHooks{consumedFor: consumedFor(hNonEmpty)}
		if _, _, derive := hNonEmpty.replayer.backgroundMocksOf(context.Background(), "test-set-0"); derive {
			t.Fatal("non-empty worker: unexpectedly set derive=true")
		}
		if status := hNonEmpty.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("status = %q", status)
		}
		if pmNonEmpty.last != nil {
			t.Fatalf("file with non-empty worker: was unexpectedly rewritten: %+v", pmNonEmpty.last)
		}
	})

	t.Run("heal never writes a mock pruned in the same run", func(t *testing.T) {
		timeout := prClientTimeoutErr(t)
		for _, exists := range []bool{true, false} {
			t.Run(fmt.Sprintf("exists=%v", exists), func(t *testing.T) {
				h, set, _, _ := noAnswerHoldRun(t, "yaml")
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

// Collector eviction guarantees:
//   - burst of >256 one-off shapes evicts sparse shapes first, keeping active
//     poller >= MinEpochReads+1;
//   - head-reserve samples are counted in whole-shape drops so no shape is cut
//     below MinPerShape;
//   - multi-sample eviction drops whole stale shapes by recency (kills N1, N2, N6).
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
		r := &Replayer{logger: zap.NewNop(), mockDB: &windowBgMockDB{mocks: mocks}, testDB: &prTestDB{cases: tcs}}
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
		// Flood with 80 multi-sample shapes (4 samples each = 320 > 256) so
		// eviction reaches atMinLRU and exercises hybrid head+tail eviction too.
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
		// Shape A arrives FIRST (4 samples), then 65 other 4-sample shapes (260
		// samples > 256), with Shape A refreshed AFTER shape-00..shape-10 so A
		// is MORE recent than shape-00..shape-10 despite being first-seen!
		for k := 0; k < 4; k++ {
			mocks = append(mocks, makeMongoPollMock(fmt.Sprintf("shapeA-%d", k), "coll_A", nextTS(), ts.Add(time.Millisecond)))
		}
		for s := 0; s < 20; s++ {
			for k := 0; k < 4; k++ {
				mocks = append(mocks, makeMongoPollMock(fmt.Sprintf("stale-%02d-%d", s, k), fmt.Sprintf("coll_stale_%02d", s), nextTS(), ts.Add(time.Millisecond)))
			}
		}
		// Refresh Shape A so its recency jumps ahead of stale-00..stale-19.
		mocks = append(mocks, makeMongoPollMock("shapeA-refresh", "coll_A", nextTS(), ts.Add(time.Millisecond)))
		for s := 0; s < 60; s++ {
			for k := 0; k < 4; k++ {
				mocks = append(mocks, makeMongoPollMock(fmt.Sprintf("newer-%02d-%d", s, k), fmt.Sprintf("coll_newer_%02d", s), nextTS(), ts.Add(time.Millisecond)))
			}
		}
		r := &Replayer{logger: zap.NewNop(), mockDB: &windowBgMockDB{mocks: mocks}, testDB: &prTestDB{cases: tcs}}
		doc := &models.Mapping{TestSetID: "ts-0"}
		if err := r.populateWindowDerivedBackground(context.Background(), "ts-0", tcs, doc, doc); err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, e := range doc.Worker {
			prefix := e.Name[:strings.LastIndexByte(e.Name, '-')]
			counts[prefix]++
		}
		// Kills N2 (first-seen eviction would evict shapeA instead of stale-00):
		if counts["shapeA"] < learn.MinEpochReads+1 {
			t.Fatalf("refreshed shapeA kept %d samples, want >= %d (recency LRU must keep refreshed shape)", counts["shapeA"], learn.MinEpochReads+1)
		}
		// Kills N1 and N6 (no kept 4-sample shape may be trimmed below MinPerShape=4):
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
