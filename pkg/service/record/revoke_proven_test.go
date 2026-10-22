package record

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// unsavedAt is a mock's span from ms to ms+1 milliseconds after a fixed
// instant.
func unsavedAt(ms int) *models.Mock {
	at := time.Date(2004, 5, 6, 7, 8, 9, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond)
	return &models.Mock{Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}
}

// windowAt is a test case's window from ms to ms+d milliseconds after the
// same instant.
func windowAt(ms, d int) streamedWindow {
	at := time.Date(2004, 5, 6, 7, 8, 9, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond)
	return streamedWindow{req: at.UnixNano(), resp: at.Add(time.Duration(d) * time.Millisecond).UnixNano()}
}

// A test case window overlaps the unsaved mocks' spans it touches, bounds
// included, and no other; past maxUnsavedMockSpans the rest are one span over
// them all, which charges more test cases, never fewer. In a recording whose
// mappings named the owners nothing is charged by window.
func TestTheUnsavedMocksChargeTheTestCasesTheirSpansOverlap(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	u := newUnsavedMocks()
	u.add(unsavedAt(10))  // [10, 11]
	u.add(unsavedAt(100)) // [100, 101]
	u.add(&models.Mock{}) // no request time: in no window
	saved := savedTestCases{}
	saved.add(windowAt(0, 10), "touches-the-first")  // [0, 10]
	saved.add(windowAt(11, 50), "from-its-end")      // [11, 61]
	saved.add(windowAt(20, 10), "between")           // [20, 30]
	saved.add(windowAt(90, 20), "over-the-second")   // [90, 110]
	saved.add(windowAt(90, 20), "same-window-again") // the very same window
	w := watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil}})
	r := &Recorder{logger: zap.NewNop()}
	r.chargeUnsaved(w, u, saved, true)
	if w.chargedAny() {
		t.Fatalf("charged by window in a recording whose mappings named the owners: %v", w.byTestCase)
	}
	r.chargeUnsaved(w, u, saved, false)
	one := fmt.Sprint(models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: 1, What: mockNotSavedWhileItRan}})
	for _, name := range []string{"touches-the-first", "from-its-end", "over-the-second", "same-window-again"} {
		if got := fmt.Sprint(w.byTestCase[name]); got != one {
			t.Fatalf("%s charged %s, want %s", name, got, one)
		}
	}
	if _, ok := w.byTestCase["between"]; ok {
		t.Fatalf("a test case between the unsaved mocks was charged: %v", w.byTestCase["between"])
	}

	// Past the bound: one span over the rest.
	u = newUnsavedMocks()
	for i := 0; i < maxUnsavedMockSpans; i++ {
		u.add(unsavedAt(0))
	}
	u.add(unsavedAt(1000))
	u.add(unsavedAt(2000))
	far := savedTestCases{}
	far.add(windowAt(1500, 1), "in-the-joined-span")
	far.add(windowAt(5000, 1), "after-them-all")
	w = watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil}})
	r.chargeUnsaved(w, u, far, false)
	if _, ok := w.byTestCase["in-the-joined-span"]; !ok {
		t.Fatal("past the bound, a test case within the span over the rest was not charged")
	}
	if _, ok := w.byTestCase["after-them-all"]; ok {
		t.Fatal("past the bound, a test case after every unsaved mock was charged")
	}

	// With the switch off nothing is collected.
	t.Setenv(provenswitch.Env, "")
	if newUnsavedMocks() != nil {
		t.Fatal("unsaved mocks are collected with the switch off")
	}
}

// Under the proven switch a test case whose mapping named mocks that never
// reached keploy record (a short mock pool) is charged with them, unless it
// was deleted; with the switch off it is only counted.
func TestAShortMockPoolChargesItsTestCase(t *testing.T) {
	for _, proven := range []bool{true, false} {
		if proven {
			t.Setenv(provenswitch.Env, "1")
		} else {
			t.Setenv(provenswitch.Env, "")
		}
		var short sync.Map
		short.Store("test-1", int64(2))
		short.Store("test-gone", int64(1))
		w := watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil}})
		chargeShortPools(w, &short, map[string]struct{}{"test-gone": {}})
		if !proven {
			if w.chargedAny() {
				t.Fatalf("charged with the switch off: %v", w.byTestCase)
			}
			continue
		}
		want := fmt.Sprint(models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: 2, What: mocksUncorrelated}})
		if got := fmt.Sprint(w.byTestCase["test-1"]); got != want || len(w.byTestCase) != 1 {
			t.Fatalf("charged %v, want test-1 alone with %s", w.byTestCase, want)
		}
	}
}

// Without a pass that judges the test cases, what the agent left out of the
// recording unnamed fails it, said at ERROR with the reason; with one, it is
// handed over as the set's (RecordedLosses.LeftOut). Nothing with the
// switch off, and nothing for a marker that left none out.
func TestTestCasesTheAgentLeftOutUnnamedFailTheRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	if got := leftOutUnsaid(models.StreamEndSummary{TestCasesSent: 4}); got != "" {
		t.Fatalf("a marker that left none out said %q", got)
	}
	why := leftOutUnsaid(models.StreamEndSummary{TestCasesSuppressed: 1})
	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), &countingInstr{answers: []models.CaptureLosses{nil}})
	w.leftOutOfSet(why)
	if !w.finish("test-set-0", false) {
		t.Fatal("test cases the agent left out unnamed did not fail the recording")
	}
	var said bool
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "left test cases out of it that it could not name") && strings.Contains(fmt.Sprint(e.ContextMap()["reason"]), "tcs_suppressed_total") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the failure did not say why: %v", logs.All())
	}

	// An agent that cannot count: the watch is off, and still says it.
	off := watchCaptureLosses(context.Background(), zap.NewNop(), &blockingInstr{})
	off.leftOutOfSet(why)
	if !off.finish("test-set-0", false) {
		t.Fatal("with an agent that cannot count, the left-out test cases did not fail the recording")
	}

	var handed []RecordedLosses
	t.Cleanup(JudgeLossesLater(func(l RecordedLosses) { handed = append(handed, l) }))
	w = watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil}})
	w.leftOutOfSet(why)
	if fail, incomplete := w.finishRun("test-set-0", false, ""); fail || !incomplete {
		t.Fatalf("judged later: fail %v, incomplete %v; want the recording kept and incomplete", fail, incomplete)
	}
	if len(handed) != 1 || handed[0].LeftOut != why || !handed[0].Failing() {
		t.Fatalf("handed %+v, want the set's left-out test cases", handed)
	}

	t.Setenv(provenswitch.Env, "")
	w = watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil}})
	w.leftOutOfSet(why)
	if w.chargedAny() {
		t.Fatal("noted with the switch off")
	}
}

// What the agent left out unnamed says where each came from: test cases it
// suppressed (an agent that does not stream them with their loss), and test
// cases left out in flight, the agent letting go of the mocks it held for
// them, past its bound on them or under memory pressure.
func TestTheTestCasesTheAgentLeftOutUnnamedSayWhereEachCameFrom(t *testing.T) {
	got := leftOutUnsaid(models.StreamEndSummary{TestCasesSuppressed: 2, LeftOutInFlight: 3})
	for _, want := range []string{
		"left 2 test case(s) out of the recording, unnamed, for a call of theirs it could not record (tcs_suppressed_total",
		"left 3 test case(s) out of the recording, unnamed: each stayed in flight",
		"the agent let go of the mocks it held for the requests in flight, past its bound on them or under memory pressure (tcs_left_out_in_flight)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q lacks %q", got, want)
		}
	}
	if LeftOutUnsaid(models.StreamEndSummary{TestCasesSuppressed: 2, LeftOutInFlight: 3}) != got {
		t.Fatal("LeftOutUnsaid is not leftOutUnsaid")
	}
}

// What the agent's incoming streams could not tell (a stream that charges
// losses that ended without its marker, test cases past the most the agent
// watches for a later span) leaves the set's recording not verified: without
// a pass that judges the test cases it fails, said at ERROR with the reason;
// with one it is handed over as the set's NotVerified, which fails a pass
// that judges no test case of the set. A stream that told all says nothing;
// nothing is noted with the switch off.
//
// Not parallel: it sets the switch and the judging pass, both process-wide.
func TestWhatTheAgentsStreamsCouldNotTellLeavesTheSetNotVerified(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	if got := streamsUntold(models.StreamEndSummary{TestCasesSent: 3, LateLosses: []models.LateLoss{{}}}); got != "" {
		t.Fatalf("streams that told all said %q", got)
	}
	why := streamsUntold(models.StreamEndSummary{Unmarked: 1, NotRechecked: 2})
	for _, want := range []string{"1 of the keploy agent's incoming streams ended without their end marker", "streamed 2 test case(s) past the most it watches", "tcs_not_rechecked"} {
		if !strings.Contains(why, want) {
			t.Fatalf("%q lacks %q", why, want)
		}
	}
	if StreamsUntold(models.StreamEndSummary{Unmarked: 1, NotRechecked: 2}) != why {
		t.Fatal("StreamsUntold is not streamsUntold")
	}

	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), &countingInstr{answers: []models.CaptureLosses{nil}})
	w.untoldOfSet(why)
	if !w.finish("test-set-0", false) {
		t.Fatal("what the agent's streams could not tell did not fail the recording")
	}
	var said bool
	for _, e := range logs.All() {
		if strings.HasPrefix(e.Message, "the recording is not verified: keploy's agent could not tell it everything its test cases lost") && e.ContextMap()["reason"] == why {
			said = true
		}
	}
	if !said {
		t.Fatalf("the failure did not say why: %v", logs.All())
	}

	// An agent that cannot count: the watch is off, and still says it.
	off := watchCaptureLosses(context.Background(), zap.NewNop(), &blockingInstr{})
	off.untoldOfSet(why)
	if !off.finish("test-set-0", false) {
		t.Fatal("with an agent that cannot count, what its streams could not tell did not fail the recording")
	}

	var handed []RecordedLosses
	t.Cleanup(JudgeLossesLater(func(l RecordedLosses) { handed = append(handed, l) }))
	w = watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil}})
	w.untoldOfSet(why)
	if fail, incomplete := w.finishRun("test-set-0", false, ""); fail || !incomplete {
		t.Fatalf("judged later: fail %v, incomplete %v; want the recording kept and incomplete", fail, incomplete)
	}
	if len(handed) != 1 || !strings.Contains(handed[0].NotVerified, why) || !handed[0].Failing() || !strings.Contains(handed[0].Text(), why) {
		t.Fatalf("handed %+v, want the set not verified for it", handed)
	}

	t.Setenv(provenswitch.Env, "")
	w = watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil}})
	w.untoldOfSet(why)
	if w.chargedAny() {
		t.Fatal("noted with the switch off")
	}
}

// The ERROR a recording judged on its own fails with names what its test
// cases lost: what the agent could not capture, what keploy could not record
// as mocks (an exchange a parser left out, mocks let go), or both; and its
// reason names every source of each kind.
//
// Not parallel: it sets the switch.
func TestTheRecordingsErrorSaysWhetherTheAgentOrKeployLostTheCall(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	if m := taintedMsg(true, false); !strings.Contains(m, "keploy's agent could not capture every call while these test cases were recorded") {
		t.Fatalf("agent only: %q", m)
	}
	if m := taintedMsg(false, true); strings.Contains(m, "agent could not capture") || !strings.Contains(m, "keploy could not record every call it captured as a mock") {
		t.Fatalf("keploy's own only: %q", m)
	}
	if m := taintedMsg(true, true); !strings.Contains(m, "agent could not capture every call") || !strings.Contains(m, "could not record every call it captured as a mock") {
		t.Fatalf("both: %q", m)
	}
	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), &countingInstr{answers: []models.CaptureLosses{nil}})
	w.charge("test-1", models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "MySQL: a"}})
	w.charge("test-2", models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "Http: b"}})
	if !w.finish("test-set-0", false) {
		t.Fatal("test cases charged with what keploy could not record did not fail the recording")
	}
	var said bool
	for _, e := range logs.All() {
		if e.Message == taintedMsg(false, true) && strings.Contains(fmt.Sprint(e.ContextMap()["reason"]), "(MySQL: a; Http: b)") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the failure did not name both sources: %v", logs.All())
	}
}
