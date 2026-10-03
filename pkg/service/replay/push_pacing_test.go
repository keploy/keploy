package replay

// RunTestSet-level tests for server-push pacing: the staging call's recorded
// windows (O1) and the prune/mapping attribution of mocks consumed outside
// their own window (§8).

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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
// (gap) window follows that test's verdict.
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
			after := h.instr.lastParams.AfterTime
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
	if t2["send-1"] || t2["msg-1"] {
		t.Errorf("test-2 maps %v; a mock of test-1's window must not be mapped to the test that happened to run", t2)
	}
	if !t2["own-2"] || !t2["msg-2"] {
		t.Errorf("test-2 maps %v; want own-2, and msg-2 from its gap", t2)
	}
	if t3 := mappedNames(m, "test-3"); !t3["own-3"] || !t3["early-send-3"] {
		t.Errorf("test-3 maps %v; want own-3, and early-send-3, which test-2 consumed before test-3 ran", t3)
	}
	var boot bool
	for _, e := range m.Startup {
		boot = boot || e.Name == "boot-msg"
	}
	if !boot || t2["boot-msg"] {
		t.Errorf("startup section %+v, test-2 %v; want boot-msg in the startup section only", m.Startup, t2)
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
