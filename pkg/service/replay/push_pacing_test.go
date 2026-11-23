package replay

// RunTestSet-level tests for server-push pacing: the staging call's recorded
// windows (O1) and the prune/mapping attribution of mocks consumed outside
// their own window (§8).

import (
	"reflect"
	"sort"
	"testing"
	"time"

	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestRunTestSetStagesEveryRecordedWindow: the staging call carries the window of
// EVERY recorded test — the deselected and ignored ones too, since the traffic a
// deselected test's handler produced is still released at that test's place in
// the recording — and the per-test calls carry none.
func TestRunTestSetStagesEveryRecordedWindow(t *testing.T) {
	h := newPartialRunHarness(t, 4, 0)
	h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {"test-2", "test-3"}}
	h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-3"}}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %q, want PASSED", status)
	}

	h.instr.mu.Lock()
	params := append([]models.MockFilterParams(nil), h.instr.allParams...)
	h.instr.mu.Unlock()

	var staging []models.MockFilterParams
	perTest := 0
	for _, p := range params {
		if p.AfterTime.Equal(models.BaseTime) {
			staging = append(staging, p)
			continue
		}
		perTest++
		if len(p.RecordedWindows) != 0 {
			t.Fatalf("a per-test call carried %d recorded windows; they belong to the staging call only",
				len(p.RecordedWindows))
		}
	}
	if len(staging) != 1 {
		t.Fatalf("%d staging calls, want 1", len(staging))
	}
	if perTest != 1 {
		t.Fatalf("%d per-test calls, want 1 (test-2 only)", perTest)
	}
	got := staging[0].RecordedWindows
	if len(got) != len(h.cases) {
		t.Fatalf("staging carried %d recorded windows, want %d (every recorded test, selected or not)",
			len(got), len(h.cases))
	}
	for i, tc := range h.cases {
		w := got[i]
		if w.TestCase != tc.Name || !w.Start.Equal(tc.HTTPReq.Timestamp) || !w.End.Equal(tc.HTTPResp.Timestamp) {
			t.Fatalf("window %d = %+v, want %s [%v, %v]", i, w, tc.Name, tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp)
		}
	}
	if !staging[0].FirstRecordedTestStart.Equal(h.cases[0].HTTPReq.Timestamp) {
		t.Fatal("the startup cutoff seed moved")
	}
}

func TestRecordedTestWindowsSkipsCasesWithoutAWindow(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	cases := []*models.TestCase{
		{Name: "http", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: t0}, HTTPResp: models.HTTPResp{Timestamp: t0.Add(time.Second)}},
		{Name: "grpc", Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Timestamp: t0.Add(2 * time.Second)}, GrpcResp: models.GrpcResp{Timestamp: t0.Add(3 * time.Second)}},
		{Name: "no-ts", Kind: models.HTTP},
		nil,
	}
	got := recordedTestWindows(cases)
	if len(got) != 2 || got[0].TestCase != "http" || got[1].TestCase != "grpc" || !got[1].End.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("recordedTestWindows = %+v", got)
	}
}

// ---------------------------------------------------------------------------
// §8: prune and mappings of mocks consumed outside their own window
// ---------------------------------------------------------------------------

var attr0 = time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)

func attrAt(ms int) time.Time { return attr0.Add(time.Duration(ms) * time.Millisecond) }

func consumedAt(name string, ms int, usage models.MockUsage, carryOver bool) models.MockState {
	return models.MockState{
		Name: name, Kind: "TestPulsar", Usage: usage, CarryOver: carryOver,
		Lifetime:         models.LifetimePerTest,
		ReqTimestampMock: models.FormatMockTimestamp(attrAt(ms)),
		ResTimestampMock: models.FormatMockTimestamp(attrAt(ms + 1)),
	}
}

// pinWindows gives the harness's cases fixed windows: test-i at i s, 100 ms.
func pinWindows(h *prRun) {
	for i, tc := range h.cases {
		tc.HTTPReq.Timestamp = attrAt(1000 * (i + 1))
		tc.HTTPResp.Timestamp = attrAt(1000*(i+1) + 100)
	}
}

func mappedNames(m *models.Mapping, test string) map[string]bool {
	out := map[string]bool{}
	if m == nil {
		return out
	}
	for _, tc := range m.TestCases {
		if tc.ID == test {
			for _, e := range tc.Mocks {
				out[e.Name] = true
			}
		}
	}
	return out
}

// A push or carry-over SEND consumed during a failing test, outside its own
// window, is kept by the prune and mapped to its own window's test. The failing
// test's own mocks are pruned as before, and a flagged mock consumed in its own
// (gap) window follows that test's verdict. The failing test itself is not
// mapped: it keeps its recorded mapping (mapping_verdicts.go).
func TestRunTestSetKeepsAndMapsMocksConsumedOutsideTheirWindow(t *testing.T) {
	h := newPartialRunHarness(t, 3, 0)
	pinWindows(h)
	h.replayer.config.Test.RemoveUnusedMocks = true
	h.replayer.config.Test.PreserveFailedMocks = false

	byTest := map[string][]models.MockState{
		"test-1": {
			consumedAt("own-1", 1050, models.Deleted, false),
		},
		// test-2 fails. It consumes, besides its own mock:
		//   send-1: a carry-over SEND recorded in test-1's window;
		//   msg-1:  a push recorded in the gap after test-1, delivered late;
		//   msg-2:  a push recorded in the gap after test-2, i.e. test-2's own.
		"test-2": {
			consumedAt("own-2", 2050, models.Deleted, false),
			consumedAt("send-1", 1060, models.Deleted, true),
			consumedAt("msg-1", 1500, models.Updated, true),
			consumedAt("msg-2", 2500, models.Updated, true),
		},
		// test-2 also consumes, early, a carry-over SEND recorded in test-3's
		// window (the lookahead), and a startup-band push delivered late.
		"test-3": {
			consumedAt("own-3", 3050, models.Deleted, false),
		},
	}
	byTest["test-2"] = append(byTest["test-2"],
		consumedAt("early-send-3", 3060, models.Deleted, true),
		consumedAt("boot-msg", 500, models.Updated, true),
	)
	byStart := map[time.Time]string{}
	for _, tc := range h.cases {
		byStart[tc.HTTPReq.Timestamp] = tc.Name
	}
	h.replayer.hookImpl = prHooks{
		wrongBody: map[string]bool{"test-2": true},
		consumedFor: func() []models.MockState {
			h.instr.mu.Lock()
			after := h.instr.windowAfter
			h.instr.mu.Unlock()
			return byTest[byStart[after]]
		},
	}

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %q, want FAILED (test-2 fails)", status)
	}
	if h.mocks.pruneCalls() != 1 {
		t.Fatalf("prune ran %d times, want 1", h.mocks.pruneCalls())
	}
	kept := h.mocks.kept
	for _, n := range []string{"own-1", "own-3", "send-1", "msg-1", "early-send-3", "boot-msg"} {
		if _, ok := kept[n]; !ok {
			t.Errorf("%s pruned: a mock consumed outside its own window must survive the running test's failure", n)
		}
	}
	for _, n := range []string{"own-2", "msg-2"} {
		if _, ok := kept[n]; ok {
			t.Errorf("%s kept: it belongs to the failing test-2 and follows its verdict, as before", n)
		}
	}

	m := h.mappings.last
	if m == nil {
		t.Fatal("no mapping written")
	}
	t1, t2 := mappedNames(m, "test-1"), mappedNames(m, "test-2")
	if !t1["own-1"] || !t1["send-1"] || !t1["msg-1"] {
		t.Errorf("test-1 maps %v; want own-1, send-1 and msg-1 (its window's mocks, wherever they were consumed)", t1)
	}
	if len(t2) != 0 {
		t.Errorf("test-2 failed and the write maps it to %v; a test that did not pass keeps its recorded mapping", t2)
	}
	if t3 := mappedNames(m, "test-3"); !t3["own-3"] || !t3["early-send-3"] {
		t.Errorf("test-3 maps %v; want own-3, and early-send-3, which test-2 consumed before test-3 ran", t3)
	}
	var boot bool
	for _, e := range m.Startup {
		boot = boot || e.Name == "boot-msg"
	}
	if !boot {
		t.Errorf("startup section %+v; want boot-msg in it", m.Startup)
	}
}

// A carry-over mock the agent flagged CarryOver, consumed before the set's
// first test (a message the app received as it started, a publish it made
// then), is consumed once: the agent is told it is consumed, not re-armed as a
// readiness gate's per-test consume is, which would serve it again in every
// test. The mapping records it in the startup section, which a mapping-mode
// replay names in every test's call, one recorded after the first test began
// too. The readiness gate's consume is still re-armed. (The prune keeps both,
// as it kept them before: a re-armed consume leaves a placeholder.)
func TestRunTestSetConsumesACarryOverMockConsumedBeforeTheFirstTestOnce(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.replayer.config.Test.RemoveUnusedMocks = true
	byTest := map[string][]models.MockState{
		// The setup drain, before every window.
		"": {
			consumedAt("boot-poll", 500, models.Deleted, true),     // recorded before every window
			consumedAt("crossed-send", 1500, models.Deleted, true), // in the gap after test-1
			consumedAt("gate-select", 1050, models.Deleted, false), // test-1's, consumed by the readiness gate
		},
		"test-1": {consumedAt("own-1", 1060, models.Deleted, false)},
		"test-2": {consumedAt("own-2", 2050, models.Deleted, false)},
	}
	byStart := map[time.Time]string{}
	for _, tc := range h.cases {
		byStart[tc.HTTPReq.Timestamp] = tc.Name
	}
	h.replayer.hookImpl = prHooks{consumedFor: func() []models.MockState {
		h.instr.mu.Lock()
		after := h.instr.windowAfter
		h.instr.mu.Unlock()
		return byTest[byStart[after]]
	}}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %q, want PASSED", status)
	}
	h.instr.mu.Lock()
	var firstTest models.MockFilterParams
	for _, p := range h.instr.allParams {
		if p.AfterTime.Equal(h.cases[0].HTTPReq.Timestamp) {
			firstTest = p
			break
		}
	}
	h.instr.mu.Unlock()
	told := firstTest.TotalConsumedMocks
	for _, n := range []string{"boot-poll", "crossed-send"} {
		if st, ok := told[n]; !ok || st.Usage != models.Deleted {
			t.Errorf("%s is not consumed for the agent (%+v): it would be served again in every test", n, st)
		}
	}
	if _, ok := told["gate-select"]; ok {
		t.Error("the readiness gate's consume was not re-armed for test-1, which owns it")
	}
	for _, n := range []string{"boot-poll", "crossed-send"} {
		if _, ok := h.mocks.kept[n]; !ok {
			t.Errorf("%s pruned: the app consumed it as it started", n)
		}
	}
	m := h.mappings.last
	if m == nil {
		t.Fatal("no mapping written")
	}
	startup := map[string]bool{}
	for _, e := range m.Startup {
		startup[e.Name] = true
	}
	if !startup["boot-poll"] || !startup["crossed-send"] {
		t.Errorf("startup section %+v; want boot-poll and crossed-send, both consumed as the app started", m.Startup)
	}
}

// What the running test consumed is attributed by recorded window before the
// write decides whose entries it takes: its own mocks, and a flagged mock
// recorded in the gap after its window, are its; a flagged mock of an earlier
// window (or of its gap) is that window's test's, and one from before every
// window is the startup section's. (The RunTestSet test above sees only the
// passing tests' entries, so the running test's own is pinned here.)
func TestCarryOverAttributionMapsTheRunningTestsOwnAndGapMocks(t *testing.T) {
	cases := []*models.TestCase{
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(1000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(1100)}},
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(2000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(2100)}},
	}
	a := newCarryOverAttribution(cases)
	mapping := &models.Mapping{}
	a.mapConsumed(mapping, "test-1", attrAt(1000), attrAt(1100), []models.MockState{consumedAt("own-1", 1050, models.Deleted, false)})
	a.mapConsumed(mapping, "test-2", attrAt(2000), attrAt(2100), []models.MockState{
		consumedAt("own-2", 2050, models.Deleted, false),
		consumedAt("send-1", 1060, models.Deleted, true),
		consumedAt("msg-1", 1500, models.Updated, true),
		consumedAt("msg-2", 2500, models.Updated, true),
		consumedAt("boot-msg", 500, models.Updated, true),
	})
	if t2 := mappedNames(mapping, "test-2"); len(t2) != 2 || !t2["own-2"] || !t2["msg-2"] {
		t.Fatalf("test-2 maps %v; want only own-2, and msg-2 from its gap", t2)
	}
	if t1 := mappedNames(mapping, "test-1"); !t1["send-1"] || !t1["msg-1"] {
		t.Fatalf("test-1 maps %v; want send-1 and msg-1, its window's mocks consumed while test-2 ran", t1)
	}
	if len(mapping.Startup) != 1 || mapping.Startup[0].Name != "boot-msg" {
		t.Fatalf("startup section = %+v; want boot-msg alone", mapping.Startup)
	}
}

// A flagged mock consumed before its own test ran (the carry-over lookahead:
// up to 3 windows early) is mapped to that test once it is mapped, and to no
// test when its test is not mapped this run, as before.
func TestCarryOverAttributionMapsAnEarlyConsumeToItsOwner(t *testing.T) {
	cases := []*models.TestCase{
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(1000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(1100)}},
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(2000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(2100)}},
		{Name: "test-3", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(3000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(3100)}},
		{Name: "test-4", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(4000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(4100)}},
	}
	a := newCarryOverAttribution(cases)
	mapping := &models.Mapping{}
	a.mapConsumed(mapping, "test-1", attrAt(1000), attrAt(1100), []models.MockState{consumedAt("own-1", 1050, models.Deleted, false)})
	// test-2 consumes test-3's and test-4's SENDs early; test-4 never runs.
	a.mapConsumed(mapping, "test-2", attrAt(2000), attrAt(2100), []models.MockState{
		consumedAt("own-2", 2050, models.Deleted, false),
		consumedAt("early-send-3", 3050, models.Deleted, true),
		consumedAt("early-send-4", 4050, models.Deleted, true),
	})
	if t2 := mappedNames(mapping, "test-2"); len(t2) != 1 || !t2["own-2"] {
		t.Fatalf("test-2 maps %v, want only own-2", t2)
	}
	if len(mappedNames(mapping, "test-3")) != 0 {
		t.Fatal("an entry was written for test-3 before it ran; if it never ran it would replace its recorded mapping")
	}
	a.mapConsumed(mapping, "test-3", attrAt(3000), attrAt(3100), []models.MockState{consumedAt("own-3", 3060, models.Deleted, false)})
	if t3 := mappedNames(mapping, "test-3"); len(t3) != 2 || !t3["own-3"] || !t3["early-send-3"] {
		t.Fatalf("test-3 maps %v, want own-3 and early-send-3", t3)
	}
	// A later pass mapping test-3 again does not list the early SEND twice.
	a.mapConsumed(mapping, "test-3", attrAt(3000), attrAt(3100), []models.MockState{consumedAt("own-3", 3060, models.Deleted, false)})
	n := 0
	for _, tc := range mapping.TestCases {
		for _, e := range tc.Mocks {
			if e.Name == "early-send-3" {
				n++
			}
		}
	}
	if n != 1 {
		t.Fatalf("early-send-3 is mapped %d times, want once", n)
	}
	if len(mappedNames(mapping, "test-4")) != 0 {
		t.Fatal("an entry was written for test-4, which this run did not map")
	}
}

func TestCarryOverAttributionEdges(t *testing.T) {
	cases := []*models.TestCase{
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(1000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(1100)}},
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(2000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(2100)}},
		{Name: "test-3", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(3000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(3100)}},
	}
	a := newCarryOverAttribution(cases)
	mapping := &models.Mapping{}

	// test-1 never ran this run (deselected); test-2 runs and consumes one of
	// test-1's mocks, one from before every window, and one with no timestamp.
	boot := consumedAt("boot", 500, models.Updated, true)
	noTs := consumedAt("no-ts", 0, models.Updated, true)
	noTs.ReqTimestampMock = ""
	orphan := consumedAt("of-test-1", 1050, models.Deleted, true)
	a.mapConsumed(mapping, "test-2", attrAt(2000), attrAt(2100), []models.MockState{boot, noTs, orphan})
	if len(mappedNames(mapping, "test-1")) != 0 {
		t.Fatal("an entry was written for test-1, which this run did not map; it would replace its recorded mapping")
	}
	if len(mapping.Startup) != 1 || mapping.Startup[0].Name != "boot" {
		t.Fatalf("startup section = %+v, want boot", mapping.Startup)
	}
	a.mapConsumed(mapping, "test-3", attrAt(3000), attrAt(3100), []models.MockState{boot})
	if len(mapping.Startup) != 1 {
		t.Fatal("the startup section listed boot twice")
	}

	passing := map[string]models.MockState{}
	a.keepWhateverTheVerdict(passing, "test-2", []models.MockState{boot, noTs, orphan, consumedAt("plain", 2050, models.Deleted, false)})
	for _, n := range []string{"boot", "no-ts", "of-test-1"} {
		if _, ok := passing[n]; !ok {
			t.Errorf("%s not kept", n)
		}
	}
	if _, ok := passing["plain"]; ok {
		t.Error("an unflagged mock was kept regardless of the verdict")
	}

	// No windows at all (an old recording): flagged mocks stay with the running
	// test and are still kept.
	none := newCarryOverAttribution(nil)
	m2 := &models.Mapping{}
	none.mapConsumed(m2, "test-2", attrAt(2000), attrAt(2100), []models.MockState{consumedAt("in", 2050, models.Deleted, true)})
	if !mappedNames(m2, "test-2")["in"] {
		t.Fatal("with no windows a flagged mock must be mapped as before")
	}
}

// A call in flight when a test began (sent in the gap before it) is that
// test's in the mapping, though answered after the test ended, where the
// window filter alone drops it; one answered inside the window the filter
// keeps, once. One sent and answered in the gap is mapped to no test.
func TestCarryOverAttributionMapsACallInFlightWhenItsTestBegan(t *testing.T) {
	cases := []*models.TestCase{
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(1000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(1100)}},
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(2000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(2100)}},
	}
	withRes := func(name string, req, res int) models.MockState {
		st := consumedAt(name, req, models.Deleted, false)
		st.ResTimestampMock = models.FormatMockTimestamp(attrAt(res))
		return st
	}
	// The mapping filter keeps every DNS, session and connection mock
	// whatever its times, so one sent in the gap and answered late is listed
	// once, by it; a connection failure is no test's but its own window's.
	sessionLate := withRes("session-late", 1550, 2500)
	sessionLate.Lifetime = models.LifetimeSession
	connLate := withRes("conn-late", 1560, 2500)
	connLate.Lifetime = models.LifetimeConnection
	dnsLate := withRes("dns-late", 1570, 2500)
	dnsLate.Kind = models.DNS
	failureLate := withRes("failure-late", 1580, 2500)
	failureLate.Kind = models.ConnectionFailure
	a := newCarryOverAttribution(cases)
	mapping := &models.Mapping{}
	a.mapConsumed(mapping, "test-2", attrAt(2000), attrAt(2100), []models.MockState{
		withRes("own-2", 2050, 2051),
		withRes("fetch-late", 1500, 2500), // answered after test-2 ended
		withRes("fetch-in", 1600, 2050),   // answered inside test-2
		withRes("gap-only", 1700, 1800),
		sessionLate, connLate, dnsLate, failureLate,
	})
	var names []string
	for _, tc := range mapping.TestCases {
		if tc.ID == "test-2" {
			for _, e := range tc.Mocks {
				names = append(names, e.Name)
			}
		}
	}
	sort.Strings(names)
	if want := []string{"conn-late", "dns-late", "fetch-in", "fetch-late", "own-2", "session-late"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("test-2 maps %v; want %v: own-2, the two calls in flight when it began, and what the filter always keeps, each once", names, want)
	}
}

// A poll placed at its answer (MockState.AnswerPlaced) is mapped by its
// answer: one sent inside test-1 and answered inside test-2 is test-2's, and
// one sent in the gap before test-2 and answered after test-2 ended is not
// test-2's, as the in-flight rule would have it: its answer is in no test's
// window. Asked as the app booted and answered in a test, it is not start-up
// traffic.
func TestCarryOverAttributionMapsAPollPlacedAtItsAnswerByItsAnswer(t *testing.T) {
	cases := []*models.TestCase{
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(1000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(1100)}},
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(2000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(2100)}},
	}
	poll := func(name string, req, res int) models.MockState {
		st := consumedAt(name, req, models.Deleted, false)
		st.ResTimestampMock = models.FormatMockTimestamp(attrAt(res))
		st.AnswerPlaced = true
		return st
	}
	a := newCarryOverAttribution(cases)
	mapping := &models.Mapping{}
	a.mapConsumed(mapping, "test-2", attrAt(2000), attrAt(2100), []models.MockState{
		poll("poll-b", 1050, 2050),
		poll("poll-late", 1500, 2500),
	})
	var names []string
	for _, tc := range mapping.TestCases {
		if tc.ID == "test-2" {
			for _, e := range tc.Mocks {
				names = append(names, e.Name)
			}
		}
	}
	if !reflect.DeepEqual(names, []string{"poll-b"}) {
		t.Fatalf("test-2 maps %v; want the poll answered inside it alone", names)
	}
	// One flagged CarryOver (consumed out of its window, its kind also
	// registering carry-over) is mapped by its answer's window: asked in
	// test-2, answered in test-3, consumed in test-4, it is test-3's.
	cases4 := append(cases[:2:2],
		&models.TestCase{Name: "test-3", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(3000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(3100)}},
		&models.TestCase{Name: "test-4", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(4000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(4100)}})
	a4 := newCarryOverAttribution(cases4)
	m4 := &models.Mapping{}
	for i, tc := range cases4[:3] {
		a4.mapConsumed(m4, tc.Name, attrAt(1000*(i+1)), attrAt(1000*(i+1)+100), []models.MockState{consumedAt("own-"+tc.Name, 1000*(i+1)+50, models.Deleted, false)})
	}
	flagged := consumedAt("poll-3", 2020, models.Updated, true)
	flagged.ResTimestampMock = models.FormatMockTimestamp(attrAt(3050))
	flagged.AnswerPlaced = true
	a4.mapConsumed(m4, "test-4", attrAt(4000), attrAt(4100), []models.MockState{flagged})
	if !mappedNames(m4, "test-3")["poll-3"] || mappedNames(m4, "test-2")["poll-3"] {
		t.Fatalf("the flagged poll is mapped to test-2 %v, test-3 %v; want test-3 alone, its answer's", mappedNames(m4, "test-2"), mappedNames(m4, "test-3"))
	}
	boot := poll("boot-poll", 500, 1050)
	if bootAsked(boot, attrAt(1000)) {
		t.Fatal("a poll asked as the app booted and answered in test-1 is start-up traffic")
	}
	boot.AnswerPlaced = false
	if !bootAsked(boot, attrAt(1000)) {
		t.Fatal("a call asked as the app booted is no longer start-up traffic")
	}
}

// creditedAt is a consume a window reclaim credits to test's call in place of
// replaces (MockState.CreditTo, CreditReplaces).
func creditedAt(name string, ms int, test, replaces string) models.MockState {
	st := consumedAt(name, ms, models.Deleted, false)
	st.CreditTo, st.CreditReplaces = test, replaces
	return st
}

// mappedList is test's entry of m, in order.
func mappedList(m *models.Mapping, test string) []string {
	var out []string
	for _, tc := range m.TestCases {
		if tc.ID == test {
			for _, e := range tc.Mocks {
				out = append(out, e.Name)
			}
		}
	}
	return out
}

// A window reclaim's credits (the agent's CommitWindowReclaim) move each
// earlier test's entry to the recording its call is credited with now, in the
// place of the one it was: the outer test lists m-out, the inner test m-in,
// each recording once; along a two-hop path each holder moves one step. Only
// the running test's own consumes are judged by its verdict: a credit is kept
// whatever it is. Before, the inner test's drain was mapped as its own (m-out
// dropped by its window filter, m-in kept), and the outer test kept m-in: m-in
// was listed for two tests and m-out for none, so the next replay missed.
func TestCarryOverAttributionMovesAReclaimsCredits(t *testing.T) {
	win := func(name string, from, to int) *models.TestCase {
		return &models.TestCase{Name: name, Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(from)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(to)}}
	}
	t.Run("one hop", func(t *testing.T) {
		cases := []*models.TestCase{win("outer", 1000, 1100), win("inner", 1010, 1020), win("unrelated", 1080, 1090)}
		a := newCarryOverAttribution(cases)
		mapping := &models.Mapping{}
		a.mapConsumed(mapping, "outer", attrAt(1000), attrAt(1100), []models.MockState{consumedAt("m-in", 1015, models.Deleted, false)})
		inner := []models.MockState{creditedAt("m-out", 1085, "outer", "m-in"), consumedAt("m-in", 1015, models.Deleted, false)}
		a.mapConsumed(mapping, "inner", attrAt(1010), attrAt(1020), inner)
		if got := mappedList(mapping, "outer"); !reflect.DeepEqual(got, []string{"m-out"}) {
			t.Errorf("outer lists %v; want [m-out], the copy its call is credited with now", got)
		}
		if got := mappedList(mapping, "inner"); !reflect.DeepEqual(got, []string{"m-in"}) {
			t.Errorf("inner lists %v; want [m-in], its own", got)
		}
		passing := map[string]models.MockState{}
		a.keepWhateverTheVerdict(passing, "inner", inner)
		if _, ok := passing["m-out"]; !ok {
			t.Error("m-out, credited to outer, is not kept whatever inner's verdict")
		}
		if _, ok := passing["m-in"]; ok {
			t.Error("m-in, inner's own consume, is kept whatever inner's verdict")
		}
	})
	t.Run("two hops", func(t *testing.T) {
		cases := []*models.TestCase{win("A", 2000, 2100), win("B", 2010, 2060), win("C", 2045, 2055)}
		a := newCarryOverAttribution(cases)
		mapping := &models.Mapping{}
		a.mapConsumed(mapping, "A", attrAt(2000), attrAt(2100), []models.MockState{consumedAt("m1", 2015, models.Deleted, false)})
		a.mapConsumed(mapping, "B", attrAt(2010), attrAt(2060), []models.MockState{consumedAt("m3", 2050, models.Deleted, false)})
		a.mapConsumed(mapping, "C", attrAt(2045), attrAt(2055), []models.MockState{
			creditedAt("m2", 2085, "A", "m1"), creditedAt("m1", 2015, "B", "m3"), consumedAt("m3", 2050, models.Deleted, false),
		})
		for test, want := range map[string][]string{"A": {"m2"}, "B": {"m1"}, "C": {"m3"}} {
			if got := mappedList(mapping, test); !reflect.DeepEqual(got, want) {
				t.Errorf("%s lists %v; want %v", test, got, want)
			}
		}
	})
	t.Run("a credit applied again lists the spare once", func(t *testing.T) {
		cases := []*models.TestCase{win("outer", 1000, 1100), win("inner", 1010, 1020)}
		a := newCarryOverAttribution(cases)
		mapping := &models.Mapping{}
		a.mapConsumed(mapping, "outer", attrAt(1000), attrAt(1100), []models.MockState{consumedAt("m-in", 1015, models.Deleted, false), consumedAt("own", 1050, models.Deleted, false)})
		inner := []models.MockState{creditedAt("m-out", 1085, "outer", "m-in"), consumedAt("m-in", 1015, models.Deleted, false)}
		a.mapConsumed(mapping, "inner", attrAt(1010), attrAt(1020), inner)
		a.mapDrained(mapping, nil, inner[:1]) // a retry cycle's drain hands the credit over again
		if got := mappedList(mapping, "outer"); !reflect.DeepEqual(got, []string{"m-out", "own"}) {
			t.Errorf("outer lists %v; want [m-out own]: the spare once, in m-in's place", got)
		}
		// The entry lists the spare and, again, the recording it replaced (a
		// retry re-mapped it): the credit leaves the spare's one listing.
		mapping.TestCases[0].Mocks = append(mapping.TestCases[0].Mocks, models.MockEntry{Name: "m-in"})
		a.mapDrained(mapping, nil, inner[:1])
		if got := mappedList(mapping, "outer"); !reflect.DeepEqual(got, []string{"m-out", "own"}) {
			t.Errorf("outer lists %v; want [m-out own]", got)
		}
		// A credit in place of a recording the entry does not list adds it.
		a.mapDrained(mapping, nil, []models.MockState{creditedAt("m-y", 1090, "outer", "m-z")})
		if got := mappedList(mapping, "outer"); !reflect.DeepEqual(got, []string{"m-out", "own", "m-y"}) {
			t.Errorf("outer lists %v; want [m-out own m-y]", got)
		}
	})
	t.Run("a credit in a drain no test's mapping reads", func(t *testing.T) {
		cases := []*models.TestCase{win("outer", 1000, 1100), win("inner", 1010, 1020)}
		a := newCarryOverAttribution(cases)
		mapping := &models.Mapping{}
		a.mapConsumed(mapping, "outer", attrAt(1000), attrAt(1100), []models.MockState{consumedAt("m-in", 1015, models.Deleted, false)})
		// A connection reset's drain (the request is not retried, its test
		// fails): the spare credited to outer is listed for outer and kept by
		// the prune; the drain's other mocks are left to the prune as before.
		passing := map[string]models.MockState{}
		a.mapDrained(mapping, passing, []models.MockState{creditedAt("m-out", 1085, "outer", "m-in"), consumedAt("pushed", 1090, models.Deleted, false)})
		if got := mappedList(mapping, "outer"); !reflect.DeepEqual(got, []string{"m-out"}) {
			t.Errorf("outer lists %v; want [m-out]", got)
		}
		if _, ok := passing["m-out"]; !ok {
			t.Error("m-out, listed for outer, is not kept by the prune")
		}
		if _, ok := passing["pushed"]; ok {
			t.Error("the drain's own mock is kept by mapDrained; the prune decides it, as before")
		}
	})
	t.Run("a credit flagged carry-over goes to its credited test alone", func(t *testing.T) {
		// The release rule would list it for the test whose window it was
		// recorded in; the credit names the test whose call it answers.
		cases := []*models.TestCase{win("outer", 1000, 1100), win("inner", 1010, 1020), win("later", 1200, 1300)}
		a := newCarryOverAttribution(cases)
		mapping := &models.Mapping{}
		a.mapConsumed(mapping, "outer", attrAt(1000), attrAt(1100), []models.MockState{consumedAt("m-in", 1015, models.Deleted, false)})
		a.mapConsumed(mapping, "later", attrAt(1200), attrAt(1300), nil)
		credit := creditedAt("m-out", 1250, "outer", "m-in")
		credit.CarryOver = true
		a.mapDrained(mapping, map[string]models.MockState{}, []models.MockState{credit})
		if got := mappedList(mapping, "outer"); !reflect.DeepEqual(got, []string{"m-out"}) {
			t.Errorf("outer lists %v; want [m-out]", got)
		}
		if got := mappedList(mapping, "later"); len(got) != 0 {
			t.Errorf("later lists %v; want none: m-out is credited to outer", got)
		}
	})
	t.Run("a credit held for a test mapped later replaces what its drain lists", func(t *testing.T) {
		cases := []*models.TestCase{win("outer", 1000, 1100), win("inner", 1010, 1020)}
		a := newCarryOverAttribution(cases)
		mapping := &models.Mapping{}
		a.mapConsumed(mapping, "inner", attrAt(1010), attrAt(1020), []models.MockState{creditedAt("m-out", 1085, "outer", "m-in"), consumedAt("m-in", 1015, models.Deleted, false)})
		a.mapConsumed(mapping, "outer", attrAt(1000), attrAt(1100), []models.MockState{consumedAt("m-in", 1015, models.Deleted, false)})
		if got := mappedList(mapping, "outer"); !reflect.DeepEqual(got, []string{"m-out"}) {
			t.Errorf("outer lists %v; want [m-out], in m-in's place", got)
		}
		if got := mappedList(mapping, "inner"); !reflect.DeepEqual(got, []string{"m-in"}) {
			t.Errorf("inner lists %v; want [m-in]", got)
		}
	})
	t.Run("a credit to a test not mapped yet waits for it", func(t *testing.T) {
		cases := []*models.TestCase{win("outer", 1000, 1100), win("inner", 1010, 1020)}
		a := newCarryOverAttribution(cases)
		mapping := &models.Mapping{}
		a.mapConsumed(mapping, "inner", attrAt(1010), attrAt(1020), []models.MockState{creditedAt("m-out", 1085, "outer", "m-in"), consumedAt("m-in", 1015, models.Deleted, false)})
		if got := mappedList(mapping, "inner"); !reflect.DeepEqual(got, []string{"m-in"}) {
			t.Errorf("inner lists %v; want [m-in]", got)
		}
		a.mapConsumed(mapping, "outer", attrAt(1000), attrAt(1100), nil)
		if got := mappedList(mapping, "outer"); !reflect.DeepEqual(got, []string{"m-out"}) {
			t.Errorf("outer lists %v once mapped; want [m-out]", got)
		}
	})
}

// A consume the edge rule gave the running test's window (MockState.EdgeOwned:
// placed 30 µs past its end, outside every recorded window) is listed under
// the running test, as any of its own: its recorded window does not hold it,
// so the window filter used to leave it in no entry, and the next replay,
// which loads each test's mocks by name, never served it. One the agent did
// not give it so is still left out.
func TestCarryOverAttributionMapsAnEdgeConsumeToItsTest(t *testing.T) {
	cases := []*models.TestCase{
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(1000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(1100)}},
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(2000)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(2100)}},
	}
	a := newCarryOverAttribution(cases)
	mapping := &models.Mapping{}
	past := func(name string, us int, edge bool) models.MockState {
		at := attrAt(1100).Add(time.Duration(us) * time.Microsecond)
		return models.MockState{
			Name: name, Kind: models.MySQL, Usage: models.Deleted, Lifetime: models.LifetimePerTest, EdgeOwned: edge,
			ReqTimestampMock: models.FormatMockTimestamp(at),
			ResTimestampMock: models.FormatMockTimestamp(at.Add(time.Millisecond)),
		}
	}
	a.mapConsumed(mapping, "test-1", attrAt(1000), attrAt(1100), []models.MockState{
		consumedAt("own-1", 1050, models.Deleted, false), past("edge-1", 30, true), past("stray-1", 50, false),
	})
	got := mappedNames(mapping, "test-1")
	if !got["own-1"] || !got["edge-1"] || got["stray-1"] || len(got) != 2 {
		t.Errorf("test-1 maps %v; want own-1 and edge-1 (the edge rule's), not stray-1", got)
	}
}

// The real mock manager's drains, mapped as RunTestSet maps them
// (mapConsumed, then passingEntriesOf): after a reclaim each recording is
// listed for one test and each test lists the recordings its calls are
// credited with. One hop: outer m-out, inner m-in. Two hops: A m2, B m1, C m3.
// One holder holding two
// steps of a path (H consumed u0 and u1, each with the next as its spare; I
// reclaims u0): H's entry moves off u1 first, then off u0, and lists u1 and
// u2; I lists u0. Two reclaims of one test in one drain, the second through
// the credit the first gave: each holder lists its last copy. Each recording
// is served once.
func TestTheManagersReclaimDrainsMapEachRecordingOnce(t *testing.T) {
	mk := func(name string, at time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.MySQL,
			Spec:         models.MockSpec{Metadata: map[string]string{"type": "mocks"}, ReqTimestampMock: at, ResTimestampMock: at.Add(100 * time.Microsecond)},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true, IsFiltered: true}}
	}
	win := func(name string, from, to int) *models.TestCase {
		return &models.TestCase{Name: name, Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: attrAt(from)}, HTTPResp: models.HTTPResp{Timestamp: attrAt(to)}}
	}
	staged := func(t *testing.T, mm *proxyPkg.MockManager) map[string]*models.Mock {
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
	consume := func(t *testing.T, mm *proxyPkg.MockManager, m *models.Mock) {
		if m == nil || !mm.DeleteFilteredMock(*m) {
			t.Fatalf("could not consume %v", m)
		}
	}
	check := func(t *testing.T, cases []*models.TestCase, drains map[string][]models.MockState, order []string, want map[string][]string, served map[string]uint64) {
		a := newCarryOverAttribution(cases)
		mapping := &models.Mapping{}
		passing := map[string]bool{}
		for _, name := range order {
			for _, tc := range cases {
				if tc.Name == name {
					a.mapConsumed(mapping, name, tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp, drains[name])
				}
			}
			passing[name] = true
		}
		out, _ := passingEntriesOf(mapping, passing)
		for test, w := range want {
			got := mappedList(out, test)
			sort.Strings(got)
			if !reflect.DeepEqual(got, w) {
				t.Errorf("%s lists %v; want %v", test, got, w)
			}
		}
		for name, n := range served {
			if n != 1 {
				t.Errorf("%s served %d times; want once", name, n)
			}
		}
		// And every recording a test lists was served: once, above.
		for test, w := range want {
			for _, name := range w {
				if served[name] == 0 {
					t.Errorf("%s, listed for %s, was never served", name, test)
				}
			}
		}
	}
	t.Run("one hop", func(t *testing.T) {
		cases := []*models.TestCase{win("outer", 1000, 1100), win("inner", 1010, 1020), win("unrelated", 1080, 1090)}
		mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.SeedRecordedWindows(recordedTestWindows(cases))
		drains := map[string][]models.MockState{}
		mm.SetMocksWithWindow([]*models.Mock{mk("m-in", attrAt(1015)), mk("m-out", attrAt(1085))}, nil, attrAt(1000), attrAt(1100))
		s := staged(t, mm)
		consume(t, mm, s["m-in"])
		mm.RecordWindowAlternatives(s["m-in"], attrAt(1000), attrAt(1100), []*models.Mock{s["m-out"]})
		drains["outer"] = mm.GetConsumedMocks()
		mm.SetMocksWithWindow(nil, nil, attrAt(1010), attrAt(1020))
		if _, ok := mm.CommitWindowReclaim(s["m-in"], attrAt(1010), attrAt(1020)); !ok {
			t.Fatal("inner could not reclaim m-in")
		}
		drains["inner"] = mm.GetConsumedMocks()
		check(t, cases, drains, []string{"outer", "inner"}, map[string][]string{"outer": {"m-out"}, "inner": {"m-in"}}, mm.ServedCounts())
	})
	t.Run("two hops", func(t *testing.T) {
		// A takes m1 (m3 and m2 its spares), B takes m3 (m1 linked), C
		// reclaims m3 along B -> m1 -> A -> m2.
		cases := []*models.TestCase{win("A", 3000, 3100), win("B", 3010, 3060), win("C", 3045, 3055)}
		mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.SeedRecordedWindows(recordedTestWindows(cases))
		drains := map[string][]models.MockState{}
		all := func() []*models.Mock {
			return []*models.Mock{mk("m1", attrAt(3015)), mk("m3", attrAt(3050)), mk("m2", attrAt(3085))}
		}
		mm.SetMocksWithWindow(all(), nil, attrAt(3000), attrAt(3100))
		a := staged(t, mm)
		consume(t, mm, a["m1"])
		mm.RecordWindowAlternatives(a["m1"], attrAt(3000), attrAt(3100), []*models.Mock{a["m3"], a["m2"]})
		drains["A"] = mm.GetConsumedMocks()
		mm.SetMocksWithWindow(all()[1:], nil, attrAt(3010), attrAt(3060))
		b := staged(t, mm)
		consume(t, mm, b["m3"])
		mm.RecordWindowAlternatives(b["m3"], attrAt(3010), attrAt(3060), []*models.Mock{a["m1"]})
		drains["B"] = mm.GetConsumedMocks()
		mm.SetMocksWithWindow(nil, nil, attrAt(3045), attrAt(3055))
		if _, ok := mm.CommitWindowReclaim(b["m3"], attrAt(3045), attrAt(3055)); !ok {
			t.Fatal("C could not reclaim m3")
		}
		drains["C"] = mm.GetConsumedMocks()
		check(t, cases, drains, []string{"A", "B", "C"}, map[string][]string{"A": {"m2"}, "B": {"m1"}, "C": {"m3"}}, mm.ServedCounts())
	})
	t.Run("one holder, two steps", func(t *testing.T) {
		cases := []*models.TestCase{win("H", 2000, 2100), win("I", 2010, 2020)}
		mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.SeedRecordedWindows(recordedTestWindows(cases))
		drains := map[string][]models.MockState{}
		mm.SetMocksWithWindow([]*models.Mock{mk("u0", attrAt(2015)), mk("u1", attrAt(2050)), mk("u2", attrAt(2060))}, nil, attrAt(2000), attrAt(2100))
		s := staged(t, mm)
		consume(t, mm, s["u0"])
		mm.RecordWindowAlternatives(s["u0"], attrAt(2000), attrAt(2100), []*models.Mock{s["u1"]})
		consume(t, mm, s["u1"])
		mm.RecordWindowAlternatives(s["u1"], attrAt(2000), attrAt(2100), []*models.Mock{s["u2"]})
		drains["H"] = mm.GetConsumedMocks()
		mm.SetMocksWithWindow(nil, nil, attrAt(2010), attrAt(2020))
		if _, ok := mm.CommitWindowReclaim(s["u0"], attrAt(2010), attrAt(2020)); !ok {
			t.Fatal("I could not reclaim u0")
		}
		drains["I"] = mm.GetConsumedMocks()
		check(t, cases, drains, []string{"H", "I"}, map[string][]string{"H": {"u1", "u2"}, "I": {"u0"}}, mm.ServedCounts())
	})
	twoReclaims := func(t *testing.T, split bool) {
		// H takes u0 (v0, a1, a2 its spares), G takes v0 (a1 its spare);
		// X reclaims u0 along H -> a1, then v0 along G -> a1 -> H -> a2:
		// the second reclaim moves H's call off a1, the credit the first one
		// gave it in this same drain, so H's entry, which lists u0, moves
		// to a2; G's to a1.
		cases := []*models.TestCase{win("H", 4000, 4100), win("G", 4005, 4060), win("X", 4010, 4020), win("W", 4045, 4055), win("U", 4080, 4090)}
		mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.SeedRecordedWindows(recordedTestWindows(cases))
		drains := map[string][]models.MockState{}
		mm.SetMocksWithWindow([]*models.Mock{mk("u0", attrAt(4012)), mk("v0", attrAt(4018)), mk("a1", attrAt(4050)), mk("a2", attrAt(4085))}, nil, attrAt(4000), attrAt(4100))
		h := staged(t, mm)
		consume(t, mm, h["u0"])
		mm.RecordWindowAlternatives(h["u0"], attrAt(4000), attrAt(4100), []*models.Mock{h["v0"], h["a1"], h["a2"]})
		drains["H"] = mm.GetConsumedMocks()
		mm.SetMocksWithWindow([]*models.Mock{mk("v0", attrAt(4018)), mk("a1", attrAt(4050))}, nil, attrAt(4005), attrAt(4060))
		g := staged(t, mm)
		consume(t, mm, g["v0"])
		mm.RecordWindowAlternatives(g["v0"], attrAt(4005), attrAt(4060), []*models.Mock{h["a1"]})
		drains["G"] = mm.GetConsumedMocks()
		mm.SetMocksWithWindow(nil, nil, attrAt(4010), attrAt(4020))
		if _, ok := mm.CommitWindowReclaim(h["u0"], attrAt(4010), attrAt(4020)); !ok {
			t.Fatal("X could not reclaim u0")
		}
		// split: X streams, and its first drain is read between its two
		// reclaims; the replay joins its drains in order.
		var early []models.MockState
		if split {
			early = mm.GetConsumedMocks()
		}
		if _, ok := mm.CommitWindowReclaim(g["v0"], attrAt(4010), attrAt(4020)); !ok {
			t.Fatal("X could not reclaim v0")
		}
		drains["X"] = append(early, mm.GetConsumedMocks()...)
		check(t, cases, drains, []string{"H", "G", "X"}, map[string][]string{"H": {"a2"}, "G": {"a1"}, "X": {"u0", "v0"}}, mm.ServedCounts())
	}
	t.Run("two reclaims in one drain", func(t *testing.T) { twoReclaims(t, false) })
	t.Run("two reclaims, a drain read between them", func(t *testing.T) { twoReclaims(t, true) })
}
