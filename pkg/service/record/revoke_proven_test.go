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
