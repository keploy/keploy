package proxy

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A connection that scopedFor wraps (a per-start table installed, or a worker
// scope registered, with the proven switch off) tells which gap between the
// recorded tests a time lies in as the manager does: the MySQL matcher reads
// it through integrations.RecordedWindowIndex to answer a call made between
// tests with the recording of its own gap. A view that lost it would leave
// the gaps unknown on such connections, and every call of a request
// recorded in several gaps would take the first gap's recording again.
func TestAScopedViewReportsTheManagersRecordedWindowIndex(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	idx, ok := view.(integrations.RecordedWindowIndex)
	if !ok {
		t.Fatal("a scoped view does not offer integrations.RecordedWindowIndex")
	}
	if i, known := idx.RecordedWindowIndex(pms(1500)); known || i != -1 {
		t.Fatalf("with no recorded windows seeded, the view reports index %d, known %v; want -1, false", i, known)
	}

	// windowsOf: test-1 records over [1000, 1100] ms, test-2 over [2000, 2100] ms.
	mm.SeedRecordedWindows(windowsOf("test-1", "test-2"))
	for _, c := range []struct {
		name string
		at   time.Time
		want int
	}{
		{"start-up", pms(500), -1},
		{"test-1's start", pms(1000), 0},
		{"inside test-1", pms(1050), 0},
		{"the gap after test-1", pms(1500), 0},
		{"inside test-2", pms(2050), 1},
		{"after the last test", pms(3000), 1},
	} {
		wantIdx, wantKnown := mm.RecordedWindowIndex(c.at)
		if wantIdx != c.want || !wantKnown {
			t.Fatalf("%s: the manager reports index %d, known %v; want %d, true", c.name, wantIdx, wantKnown, c.want)
		}
		if i, known := idx.RecordedWindowIndex(c.at); i != wantIdx || known != wantKnown {
			t.Errorf("%s: the scoped view reports index %d, known %v; the manager %d, %v", c.name, i, known, wantIdx, wantKnown)
		}
	}
}

// A learn step that reads the round in one snapshot (the Postgres learn
// step's learnView) tells which gap between the recorded tests a recording
// made outside every test lies in as the manager does: two of a statement's
// recordings made at the app's start-up on two connections are of one gap,
// and one made in the gap after the first test is of another. A snapshot
// that did not say would leave the gaps unknown to every learn step that
// reads one.
func TestARoundSnapshotReportsTheManagersRecordedWindowIndex(t *testing.T) {
	s := newRoundStore(t)
	s.mm.ServeExactPool(models.PoolKeyNone, nil)
	var windows integrations.TestWindows = s.snapshot(t)
	idx, ok := windows.(integrations.RecordedWindowIndex)
	if !ok {
		t.Fatal("a round snapshot does not offer integrations.RecordedWindowIndex")
	}
	if i, known := idx.RecordedWindowIndex(pms(1500)); known || i != -1 {
		t.Fatalf("with no recorded windows seeded, the snapshot reports index %d, known %v; want -1, false", i, known)
	}

	// windowsOf: test-1 records over [1000, 1100] ms, test-2 over [2000, 2100] ms.
	s.mm.SeedRecordedWindows(windowsOf("test-1", "test-2"))
	idx = s.snapshot(t)
	for _, c := range []struct {
		name string
		at   time.Time
		want int
	}{
		{"start-up", pms(500), -1},
		{"test-1's start", pms(1000), 0},
		{"inside test-1", pms(1050), 0},
		{"the gap after test-1", pms(1500), 0},
		{"inside test-2", pms(2050), 1},
		{"after the last test", pms(3000), 1},
	} {
		wantIdx, wantKnown := s.mm.RecordedWindowIndex(c.at)
		if wantIdx != c.want || !wantKnown {
			t.Fatalf("%s: the manager reports index %d, known %v; want %d, true", c.name, wantIdx, wantKnown, c.want)
		}
		if i, known := idx.RecordedWindowIndex(c.at); i != wantIdx || known != wantKnown {
			t.Errorf("%s: the snapshot reports index %d, known %v; the manager %d, %v", c.name, i, known, wantIdx, wantKnown)
		}
	}
}

func TestScopedViewAndRoundSnapshotReportWindowOwnership(t *testing.T) {
	s := newRoundStore(t)
	s.mm.ServeExactPool(models.PoolKeyNone, nil)
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: s.mm}
	viewOwn, ok := view.(integrations.WindowOwnership)
	if !ok {
		t.Fatal("scopedMockDb does not offer integrations.WindowOwnership")
	}
	snapOwn, ok := any(s.snapshot(t)).(integrations.WindowOwnership)
	if !ok {
		t.Fatal("RoundSnapshot does not offer integrations.WindowOwnership")
	}
	if _, _, owned := viewOwn.OwnerTestWindow(pms(1050)); owned {
		t.Fatal("scopedMockDb reported an owner with no windows seeded")
	}
	if _, _, owned := snapOwn.OwnerTestWindow(pms(1050)); owned {
		t.Fatal("RoundSnapshot reported an owner with no windows seeded")
	}

	s.mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "outer", Start: pms(1000), End: pms(1030)},
		{TestCase: "inner", Start: pms(1010), End: pms(1020)},
		{TestCase: "peer", Start: pms(1025), End: pms(1050)},
	})
	snapOwn = s.snapshot(t)
	for _, c := range []struct {
		name       string
		at         time.Time
		wantCase   string
		wantInside bool
		wantOwned  bool
	}{
		{"outer-only", pms(1005), "outer", true, true},
		{"nested-inner", pms(1015), "inner", true, true},
		{"partly-overlapping-peer", pms(1027), "peer", true, true},
		{"edge-skew-after-peer", pms(1050).Add(50 * time.Microsecond), "peer", false, true},
		{"far-outside", pms(2000), "", false, false},
	} {
		wMM, inMM, okMM := s.mm.OwnerTestWindow(c.at)
		if okMM != c.wantOwned || inMM != c.wantInside || wMM.TestCase != c.wantCase {
			t.Fatalf("%s: manager got (%q, %v, %v), want (%q, %v, %v)", c.name, wMM.TestCase, inMM, okMM, c.wantCase, c.wantInside, c.wantOwned)
		}
		if w, in, ok := viewOwn.OwnerTestWindow(c.at); ok != okMM || in != inMM || w.TestCase != wMM.TestCase {
			t.Errorf("%s: scopedMockDb got (%q, %v, %v), manager got (%q, %v, %v)", c.name, w.TestCase, in, ok, wMM.TestCase, inMM, okMM)
		}
		if w, in, ok := snapOwn.OwnerTestWindow(c.at); ok != okMM || in != inMM || w.TestCase != wMM.TestCase {
			t.Errorf("%s: RoundSnapshot got (%q, %v, %v), manager got (%q, %v, %v)", c.name, w.TestCase, in, ok, wMM.TestCase, inMM, okMM)
		}
	}
	// OwnerTestWindowSpan agrees across manager, scoped view and snapshot.
	wMM, okMM := s.mm.OwnerTestWindowSpan(pms(1012), pms(1028))
	if !okMM || wMM.TestCase != "outer" {
		t.Fatalf("OwnerTestWindowSpan(1012,1028): got (%q, %v), want (outer, true)", wMM.TestCase, okMM)
	}
	if w, ok := viewOwn.OwnerTestWindowSpan(pms(1012), pms(1028)); ok != okMM || w.TestCase != wMM.TestCase {
		t.Errorf("scopedMockDb OwnerTestWindowSpan got (%q, %v), want (%q, %v)", w.TestCase, ok, wMM.TestCase, okMM)
	}
	if w, ok := snapOwn.OwnerTestWindowSpan(pms(1012), pms(1028)); ok != okMM || w.TestCase != wMM.TestCase {
		t.Errorf("RoundSnapshot OwnerTestWindowSpan got (%q, %v), want (%q, %v)", w.TestCase, ok, wMM.TestCase, okMM)
	}
	// WindowReclaim is offered by scopedMockDb.
	if _, ok := view.(integrations.WindowReclaim); !ok {
		t.Fatal("scopedMockDb does not offer integrations.WindowReclaim")
	}
}

func TestMockManagerStagesBoundedEdgeSkewedPerTestMock(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(mm.Close)

	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "t1", Start: pms(1000), End: pms(1010)},
		{TestCase: "t2", Start: pms(1020), End: pms(1030)},
	})
	// Recorded per-test MySQL data mocks, as the agent hands them to the
	// manager: tagged "mocks", their lifetime derived (an untagged MySQL mock
	// derives to the session tier, which the edge rule never takes).
	mkEdge := &models.Mock{
		Name: "edge-30us",
		Kind: models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: pms(1010).Add(30 * time.Microsecond),
			ResTimestampMock: pms(1010).Add(40 * time.Microsecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true},
	}
	mkFar := &models.Mock{
		Name: "gap-500us",
		Kind: models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: pms(1010).Add(500 * time.Microsecond),
			ResTimestampMock: pms(1010).Add(510 * time.Microsecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true},
	}
	mm.SetMocksWithWindow([]*models.Mock{mkEdge, mkFar}, nil, pms(1000), pms(1010))
	got, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow: %v", err)
	}
	if len(got) != 1 || got[0].Name != "edge-30us" {
		names := make([]string, len(got))
		for i, m := range got {
			names[i] = m.Name
		}
		t.Fatalf("staged per-test mocks = %v, want [edge-30us]", names)
	}
}

// The edge rule takes only a plain per-test timed mock
// (models.WindowSchedule.EdgeOwner): every other shape placed just outside a
// window is routed as it was before the rule, at the staging and at the
// windowed read alike. In flight into the next test (sent in the gap,
// answered in it), placed at its answer (a poll answered in the next window),
// carry-over (a receipt released by the gap's test, kept reachable by the
// carry-over tier), a carry-over kind's mock its predicate rejects,
// window-bound (a connection failure) and staged with the set's startup (a
// push the set's staging files): none is taken into the
// window whose edge it is near. Before, the rule took each of them by its
// request time, whatever its shape, ahead of the carry-over tier.
func TestMockManagerEdgeRuleLeavesOtherShapesWhereTheyWere(t *testing.T) {
	carryKind, pollKind, pushKind := models.Kind("TestEdgeCarry"), models.Kind("TestEdgePoll"), models.Kind("TestEdgePush")
	t.Cleanup(models.RegisterCarryOver(carryKind, func(m *models.Mock) bool { return m.Name == "carry" }))
	t.Cleanup(models.RegisterAnswerPlaced(pollKind, func(*models.Mock) bool { return true }))
	t.Cleanup(models.RegisterStagingNeed(pushKind, models.StagingNeed{Use: models.StagingWithStartup, Match: func(*models.Mock) bool { return true }}))
	perTest := func(name string, kind models.Kind, req, res time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: kind,
			Spec:         models.MockSpec{Metadata: map[string]string{"type": "mocks"}, ReqTimestampMock: req, ResTimestampMock: res},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true}}
	}
	past := pms(1010).Add(30 * time.Microsecond) // 30 µs past t1's end, in the gap before t2
	before := pms(1020).Add(-30 * time.Microsecond)
	mocks := func() []*models.Mock {
		return []*models.Mock{
			perTest("plain", models.MySQL, past, past.Add(10*time.Microsecond)),
			perTest("in-flight", models.MySQL, past, pms(1025)),
			perTest("poll", pollKind, past, pms(1025)),
			perTest("carry", carryKind, before, before.Add(10*time.Microsecond)),
			// Of the carry-over kind, which its predicate rejects: still
			// flagged when consumed out of its window (MarksOutOfWindow).
			perTest("carry-kind", carryKind, past, past.Add(10*time.Microsecond)),
			// 30 us before t2's start, answered before t2 began: t2's by the
			// edge rule, never t1's.
			perTest("edge-t2", models.MySQL, before, before.Add(10*time.Microsecond)),
			perTest("refused", models.ConnectionFailure, past, past.Add(10*time.Microsecond)),
			perTest("push", pushKind, past, past.Add(10*time.Microsecond)),
		}
	}
	windows := []models.TestWindow{
		{TestCase: "t1", Start: pms(1000), End: pms(1010)},
		{TestCase: "t2", Start: pms(1020), End: pms(1030)},
	}
	names := func(ms []*models.Mock) []string {
		out := make([]string, 0, len(ms))
		for _, m := range ms {
			out = append(out, m.Name)
		}
		sort.Strings(out)
		return out
	}
	newManager := func() *MockManager {
		mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
		t.Cleanup(mm.Close)
		mm.SeedRecordedWindows(windows)
		return mm
	}

	t.Run("staging", func(t *testing.T) {
		// One replay stages t1, then t2, as keploy test does (t1's start is
		// then the set's first window: what precedes it is startup traffic).
		core, logs := observer.New(zap.InfoLevel)
		mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.New(core))
		t.Cleanup(mm.Close)
		mm.SeedRecordedWindows(windows)
		for _, c := range []struct {
			window  int
			perTest []string
			carried []string
		}{
			// t1: only the plain mock; the carry-over receipt, released by t1's
			// gap, goes to the carry-over tier, as on main.
			{0, []string{"plain"}, []string{"carry"}},
			// t2: the call in flight into it and the poll answered in it, as on
			// main, and edge-t2 by the edge rule; not the plain mock (t1's by
			// the edge rule), nor the receipt.
			{1, []string{"edge-t2", "in-flight", "poll"}, nil},
		} {
			w := windows[c.window]
			mm.SetMocksWithWindow(mocks(), nil, w.Start, w.End)
			// What the staging filed in the per-test tree, not the windowed
			// read, which re-checks the tree (that is the subtest below).
			got, err := mm.GetFilteredMocks()
			if err != nil {
				t.Fatalf("GetFilteredMocks: %v", err)
			}
			if g := names(got); !reflect.DeepEqual(g, c.perTest) {
				t.Errorf("%s's staging holds %v in its per-test tier; want %v", w.TestCase, g, c.perTest)
			}
			if c.carried == nil {
				continue
			}
			carried, err := mm.GetCarryOverMocks()
			if err != nil {
				t.Fatalf("GetCarryOverMocks: %v", err)
			}
			if g := names(carried); !reflect.DeepEqual(g, c.carried) {
				t.Errorf("%s's staging carries %v over; want %v", w.TestCase, g, c.carried)
			}
		}
		// The set's summary: t1's staging took the plain mock by the edge
		// rule and left t2 the call in flight into it; t2's took edge-t2.
		mm.logSetOwnership()
		said := logs.FilterMessageSnippet("window ownership summary").All()
		if len(said) != 1 {
			t.Fatalf("the summary was said %d times; want once", len(said))
		}
		f := said[0].ContextMap()
		if f["edge_rule_staged"] != uint64(2) || f["edge_rule_left_to_in_flight_rule"] != uint64(1) {
			t.Errorf("summary edge_rule_staged=%v edge_rule_left_to_in_flight_rule=%v; want 2 and 1", f["edge_rule_staged"], f["edge_rule_left_to_in_flight_rule"])
		}
	})
	t.Run("windowed read", func(t *testing.T) {
		mm := newManager()
		// A zero window stages the per-test tier as it is handed; the read then
		// filters it by the window that opens.
		mm.SetMocksWithWindow(mocks(), nil, time.Time{}, time.Time{})
		mm.SetCurrentTestWindow(windows[0].Start, windows[0].End)
		got, err := mm.GetPerTestMocksInWindow()
		if err != nil {
			t.Fatalf("GetPerTestMocksInWindow: %v", err)
		}
		if g := names(got); !reflect.DeepEqual(g, []string{"plain"}) {
			t.Errorf("t1's windowed read holds %v; want [plain] (not edge-t2, t2's by the edge rule)", g)
		}
		// The reads that mirror it agree: the open test owns the plain mock
		// (OwnedByOpenTest, as repeat claims and declared background ask
		// it), not edge-t2, and the keyed walk hands what the read hands.
		all, err := mm.GetFilteredMocks()
		if err != nil {
			t.Fatalf("GetFilteredMocks: %v", err)
		}
		for _, mk := range all {
			if want := mk.Name == "plain"; mm.OwnedByOpenTest(mk) != want {
				t.Errorf("OwnedByOpenTest(%s) = %v; want %v, as t1's windowed read holds it", mk.Name, !want, want)
			}
		}
		for _, key := range []string{"all", "name:plain", "name:edge-t2"} {
			walked, read := names(keyedPerTestWalk(t, mm, byKindAndName, key, -1)), names(perTestUnder(t, mm, byKindAndName, key))
			if !reflect.DeepEqual(walked, read) {
				t.Errorf("the keyed walk under %q hands %v; the windowed read under it %v", key, walked, read)
			}
		}
	})
}

// A consume of a mock the open window's staging took by the edge rule says so
// (MockState.EdgeOwned), whichever door it is consumed through, so the replay
// lists it under the open test; a consume of the window's own does not.
func TestMockManagerSaysAnEdgeConsumeIsTheOpenTests(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "t1", Start: pms(1000), End: pms(1010)},
		{TestCase: "t2", Start: pms(1020), End: pms(1030)},
	})
	mk := func(name string, at time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.MySQL,
			Spec:         models.MockSpec{Metadata: map[string]string{"type": "mocks"}, ReqTimestampMock: at, ResTimestampMock: at.Add(10 * time.Microsecond)},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true}}
	}
	mm.SetMocksWithWindow([]*models.Mock{mk("edge", pms(1010).Add(30*time.Microsecond)), mk("own", pms(1005))}, nil, pms(1000), pms(1010))
	staged, err := mm.GetPerTestMocksInWindow()
	if err != nil || len(staged) != 2 {
		t.Fatalf("staged %d mocks (%v); want edge and own", len(staged), err)
	}
	for _, m := range staged {
		if !mm.DeleteFilteredMock(*m) {
			t.Fatalf("could not consume %s", m.Name)
		}
	}
	edge := map[string]bool{}
	for _, st := range mm.GetConsumedMocks() {
		edge[st.Name] = st.EdgeOwned
	}
	if !edge["edge"] || edge["own"] || len(edge) != 2 {
		t.Errorf("consumed with EdgeOwned %v; want edge true, own false", edge)
	}
	// A later state for a name in a new drain, after a staging that no
	// longer holds it, says nothing: EdgeOwned is not kept across drains.
	mm.SetMocksWithWindow(nil, nil, pms(1000), pms(1010))
	if err := mm.recordConsume(mockStateDeleted(staged[0]), 0, false); err != nil {
		t.Fatal(err)
	}
	for _, m := range staged[1:] {
		_ = mm.recordConsume(mockStateDeleted(m), 0, false)
	}
	again := map[string]bool{}
	for _, st := range mm.GetConsumedMocks() {
		again[st.Name] = st.EdgeOwned
	}
	// recordConsume's states of edge and own in a fresh drain: only edge's
	// first state said it, and this drain holds none, so neither says it.
	if again["edge"] || again["own"] {
		t.Errorf("a fresh drain says EdgeOwned %v; want neither: no staging took them by the edge rule", again)
	}
	// A later state for a name in the same drain, after a staging of the
	// window that no longer holds it, keeps it said to be the open test's.
	mm.SetMocksWithWindow([]*models.Mock{mk("edge2", pms(1010).Add(30*time.Microsecond))}, nil, pms(1000), pms(1010))
	staged2, _ := mm.GetPerTestMocksInWindow()
	if len(staged2) != 1 || !mm.DeleteFilteredMock(*staged2[0]) {
		t.Fatalf("could not stage and consume edge2 (%d staged)", len(staged2))
	}
	mm.SetMocksWithWindow(nil, nil, pms(1000), pms(1010))
	_ = mm.recordConsume(mockStateDeleted(staged2[0]), 0, false)
	if st := mm.GetConsumedMocks(); len(st) != 1 || !st[0].EdgeOwned {
		t.Errorf("edge2's drain %+v; want one state, still EdgeOwned", st)
	}
	// The next staging takes nothing by the edge rule: a late consume of the
	// earlier window's mock is not said to be the next test's.
	mm.SetMocksWithWindow([]*models.Mock{mk("late", pms(1005))}, nil, pms(1020), pms(1030))
	if mm.edgeStagedNow("edge") {
		t.Error("the next staging still names edge as taken by the edge rule")
	}
}

// The set's ownership summary (logSetOwnership, said at keploy test's read of
// the set's end) counts what the window ownership rules did, and how many
// per-test recordings answered calls with how many serves: after one reclaim
// along one step, one reclaim and one re-credit, two recordings served once
// each (none twice), and nothing taken by the edge rule. It is said once a
// set.
func TestMockManagerSaysTheSetsOwnershipSummaryOnce(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.New(core))
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "outer", Start: pms(1000), End: pms(1100)},
		{TestCase: "inner", Start: pms(1010), End: pms(1020)},
		{TestCase: "unrelated", Start: pms(1080), End: pms(1090)},
	})
	mk := func(name string, at time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.MySQL,
			Spec:         models.MockSpec{Metadata: map[string]string{"type": "mocks"}, ReqTimestampMock: at, ResTimestampMock: at.Add(100 * time.Microsecond)},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true}}
	}
	mm.SetMocksWithWindow([]*models.Mock{mk("m-in", pms(1015)), mk("m-out", pms(1085))}, nil, pms(1000), pms(1100))
	staged, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*models.Mock{}
	for _, m := range staged {
		byName[m.Name] = m
	}
	in, out := byName["m-in"], byName["m-out"]
	if in == nil || out == nil {
		t.Fatalf("outer's staging holds %v; want m-in and m-out", byName)
	}
	// Outer's call takes m-in, recorded in inner's window, with m-out its
	// window's identical spare.
	if !mm.DeleteFilteredMock(*in) {
		t.Fatal("could not consume m-in")
	}
	mm.RecordWindowAlternatives(in, pms(1000), pms(1100), []*models.Mock{out})
	_ = mm.GetConsumedMocks()
	// Inner's call reclaims it.
	mm.SetMocksWithWindow(nil, nil, pms(1010), pms(1020))
	if got, ok := mm.CommitWindowReclaim(in, pms(1010), pms(1020)); !ok || got.Name != "m-out" {
		t.Fatalf("CommitWindowReclaim(m-in) = (%v, %v); want m-out taken in its place", got, ok)
	}
	credit := map[string]string{}
	for _, st := range mm.GetConsumedMocks() {
		credit[st.Name] = st.CreditTo + "/" + st.CreditReplaces
	}
	if want := map[string]string{"m-in": "/", "m-out": "outer/m-in"}; !reflect.DeepEqual(credit, want) {
		t.Errorf("inner's drain credits %v; want %v", credit, want)
	}
	mm.logSetOwnership()
	mm.logSetOwnership()
	said := logs.FilterMessageSnippet("window ownership summary").All()
	if len(said) != 1 {
		t.Fatalf("the summary was said %d times; want once a set", len(said))
	}
	f := said[0].ContextMap()
	for k, want := range map[string]int64{
		"reclaims": 1, "recredited_to_earlier_test": 1, "edge_rule_staged": 0,
		"per_test_recordings_served": 2, "per_test_serves": 2, "per_test_recordings_served_twice_or_more": 0,
	} {
		var got int64
		switch v := f[k].(type) {
		case uint64:
			got = int64(v)
		case int64:
			got = v
		default:
			t.Errorf("summary field %s = %v (%T); want %d", k, f[k], f[k], want)
			continue
		}
		if got != want {
			t.Errorf("summary field %s = %d; want %d", k, got, want)
		}
	}
}

// Under exact serving (a test opened by ServeExactTest) the test's mapping
// says which recordings it owns: a staging takes nothing by the edge rule.
func TestMockManagerEdgeRuleIsOffUnderExactServing(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "t1", Start: pms(1000), End: pms(1010)},
		{TestCase: "t2", Start: pms(1020), End: pms(1030)},
	})
	edge := &models.Mock{Name: "edge", Kind: models.MySQL,
		Spec:         models.MockSpec{Metadata: map[string]string{"type": "mocks"}, ReqTimestampMock: pms(1010).Add(30 * time.Microsecond), ResTimestampMock: pms(1010).Add(40 * time.Microsecond)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true}}
	// Sent 30 us past t1's end and answered in t2: the call the edge rule,
	// were it on, would leave to the in-flight rule.
	inFlight := &models.Mock{Name: "in-flight", Kind: models.MySQL,
		Spec:         models.MockSpec{Metadata: map[string]string{"type": "mocks"}, ReqTimestampMock: pms(1010).Add(30 * time.Microsecond), ResTimestampMock: pms(1025)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true}}
	mm.ServeExactTest("t1", nil)
	if mm.exactView() == nil {
		t.Fatal("ServeExactTest installed no exact view")
	}
	mm.SetMocksWithWindow([]*models.Mock{edge, inFlight}, nil, pms(1000), pms(1010))
	if mm.edgeStagedNow("edge") {
		t.Error("under exact serving the staging took a mock by the edge rule")
	}
	// Nor counts what the rule would have done.
	if n := mm.ownership.edgeStaged.Load() + mm.ownership.edgeLeftInFlight.Load(); n != 0 {
		t.Errorf("under exact serving the staging counted %d edge rule decisions; want none", n)
	}
	// Nor does the agent's window filter ask it there.
	if mm.EdgeWindows() != nil {
		t.Error("under exact serving the manager names recorded windows for the agent's edge rule")
	}
}

// A reclaim's states (each credit along its path and the reclaimed
// recording's own consume) are recorded as one: a drain between two of them
// would split a credit composed with an earlier one of the same drain from
// the state that overwrites that one. With a drain after every recorded
// section (the test's hook), five windows: H takes u0 (spares v0, a1, a2), G
// takes v0 (spare a1), and X reclaims u0, then v0 through the credit its
// first reclaim gave H; each reclaim lands whole in one drain. The first's
// drain was read before the second, so the second's credit to H is not
// composed: a2 in place of a1, which H's entry lists by then.
func TestMockManagerRecordsAReclaimInOneDrain(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "H", Start: pms(4000), End: pms(4100)},
		{TestCase: "G", Start: pms(4005), End: pms(4060)},
		{TestCase: "X", Start: pms(4010), End: pms(4020)},
		{TestCase: "W", Start: pms(4045), End: pms(4055)},
		{TestCase: "U", Start: pms(4080), End: pms(4090)},
	})
	mk := func(name string, at time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.MySQL,
			Spec:         models.MockSpec{Metadata: map[string]string{"type": "mocks"}, ReqTimestampMock: at, ResTimestampMock: at.Add(100 * time.Microsecond)},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true}}
	}
	staged := func() map[string]*models.Mock {
		ms, err := mm.GetPerTestMocksInWindow()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*models.Mock{}
		for _, m := range ms {
			out[m.Name] = m
		}
		return out
	}
	mm.SetMocksWithWindow([]*models.Mock{mk("u0", pms(4012)), mk("v0", pms(4018)), mk("a1", pms(4050)), mk("a2", pms(4085))}, nil, pms(4000), pms(4100))
	h := staged()
	if !mm.DeleteFilteredMock(*h["u0"]) {
		t.Fatal("H could not consume u0")
	}
	mm.RecordWindowAlternatives(h["u0"], pms(4000), pms(4100), []*models.Mock{h["v0"], h["a1"], h["a2"]})
	mm.GetConsumedMocks()
	mm.SetMocksWithWindow([]*models.Mock{mk("v0", pms(4018)), mk("a1", pms(4050))}, nil, pms(4005), pms(4060))
	g := staged()
	if !mm.DeleteFilteredMock(*g["v0"]) {
		t.Fatal("G could not consume v0")
	}
	mm.RecordWindowAlternatives(g["v0"], pms(4005), pms(4060), []*models.Mock{h["a1"]})
	mm.GetConsumedMocks()
	mm.SetMocksWithWindow(nil, nil, pms(4010), pms(4020))
	type entry struct{ name, creditTo, replaces string }
	var drains [][]entry
	mm.consumeRecorded = func() {
		var d []entry
		for _, st := range mm.GetConsumedMocks() {
			d = append(d, entry{st.Name, st.CreditTo, st.CreditReplaces})
		}
		if len(d) > 0 {
			drains = append(drains, d)
		}
	}
	if _, ok := mm.CommitWindowReclaim(h["u0"], pms(4010), pms(4020)); !ok {
		t.Fatal("X could not reclaim u0")
	}
	if _, ok := mm.CommitWindowReclaim(g["v0"], pms(4010), pms(4020)); !ok {
		t.Fatal("X could not reclaim v0")
	}
	mm.consumeRecorded = nil
	byName := func(d []entry) []entry {
		out := slices.Clone(d)
		slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.name, b.name) })
		return out
	}
	want := [][]entry{
		{{"a1", "H", "u0"}, {"u0", "", ""}},
		{{"a1", "G", "v0"}, {"a2", "H", "a1"}, {"v0", "", ""}},
	}
	if len(drains) != len(want) {
		t.Fatalf("X's reclaims reached %d drains %v; want each whole in one: %v", len(drains), drains, want)
	}
	for i := range want {
		if got := byName(drains[i]); !reflect.DeepEqual(got, want[i]) {
			t.Errorf("drain %d holds %v; want %v", i, got, want[i])
		}
	}
}

// A reclaim whose path a holder of which is the open test is refused: its
// credit would move inside the open test's own entry, which only one drain
// can carry. The matcher can offer such a candidate, and then tries its next
// one (commitReclaim). Nothing is taken or credited.
func TestMockManagerRefusesAReclaimTheOpenTestHolds(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "outer", Start: pms(1000), End: pms(1100)},
		{TestCase: "inner", Start: pms(1010), End: pms(1020)},
	})
	mk := func(name string, at time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.MySQL,
			Spec:         models.MockSpec{Metadata: map[string]string{"type": "mocks"}, ReqTimestampMock: at, ResTimestampMock: at.Add(100 * time.Microsecond)},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true}}
	}
	mm.SetMocksWithWindow([]*models.Mock{mk("m-in", pms(1015)), mk("m-x", pms(1016))}, nil, pms(1010), pms(1020))
	staged, err := mm.GetPerTestMocksInWindow()
	if err != nil || len(staged) != 2 {
		t.Fatalf("inner's staging holds %d (%v); want m-in and m-x", len(staged), err)
	}
	in, x := staged[0], staged[1]
	if in.Name != "m-in" {
		in, x = x, in
	}
	if !mm.DeleteFilteredMock(*in) {
		t.Fatal("could not consume m-in")
	}
	// Inner itself holds m-in, with m-x its spare.
	mm.RecordWindowAlternatives(in, pms(1010), pms(1020), []*models.Mock{x})
	_ = mm.GetConsumedMocks()
	if got, ok := mm.CommitWindowReclaim(in, pms(1010), pms(1020)); ok {
		t.Fatalf("a reclaim along a path inner holds was committed (took %s)", got.Name)
	}
	if drained := mm.GetConsumedMocks(); len(drained) != 0 {
		t.Errorf("the refused reclaim recorded %v", drained)
	}
	if served := mm.ServedCounts(); served["m-x"] != 0 {
		t.Errorf("m-x served %d times; want none", served["m-x"])
	}
}
