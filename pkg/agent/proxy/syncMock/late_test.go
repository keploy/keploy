package manager

import (
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// lateText is a watch's Late, one line per test case, for comparing.
func lateText(late []models.LateLoss) []string {
	var out []string
	for _, l := range late {
		out = append(out, fmt.Sprintf("%q %v", l.Name, l.Lost))
	}
	return out
}

// A span recorded after a test case went charges it, exactly: the test cases
// the span overlaps, and none other. Past the spans' cap, with every test
// case checked (CheckedBefore past them all), the spans before that are
// joined into one, over the gaps between them: a test case that sat in a gap
// and overlapped no span was charged with the joined span when the stream's
// end checked it again against the spans. The watch is told each span as it
// was recorded, and reads none of the joined ones.
func TestALateSpanChargesTheTestCasesItOverlapsAndNoneInAJoinedGap(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	w := m.WatchLate()
	defer w.Close()
	// Streamed with nothing lost: "gap" between two of the spans recorded
	// before, "late" where a span comes later.
	w.Watch("gap", at(10*maxPressureRanges+5), at(10*maxPressureRanges+6))
	w.Watch("late", at(20*maxPressureRanges), at(20*maxPressureRanges+1))
	for i := 0; i < maxPressureRanges; i++ {
		m.RecordOrphanWindowWhy(at(i*20), at(i*20+1), "MySQL: early")
	}
	// Every test case still to be checked starts after them all: the cap
	// joins everything before it into one span, the gap included.
	m.CheckedBefore(at(30 * maxPressureRanges))
	m.RecordOrphanWindowWhy(at(20*maxPressureRanges), at(20*maxPressureRanges), "MySQL: late")
	if ok, _ := m.WasMockOrphanedInWindow(at(10*maxPressureRanges+5), at(10*maxPressureRanges+6)); !ok {
		t.Fatal("precondition: the spans before the checked point were not joined over the gap")
	}
	// The spans recorded after "gap" went were early ones; those that
	// overlap it are the ones it overlaps, as recorded: none does.
	got := lateText(w.Late())
	want := []string{fmt.Sprintf("%q %v", "late", models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "MySQL: late"}})}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("late: %v, want %v", got, want)
	}
}

// What a watch finds: an open span charges every test case watched that ends
// at or after it opened; a closed one the test cases it overlaps, its bounds
// included; a span that overlaps none charges none. A test case forgotten (not
// streamed: Forget) is not named, nor one never watched. Each is named as it
// was watched, in the order it was, its What naming every source.
func TestALateWatchNamesWhatEachTestCaseLostAfterItWent(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	w := m.WatchLate()
	defer w.Close()
	w.Watch("a", at(100), at(110))
	w.Watch("", at(50), at(60)) // unnamed, watched after a
	w.Watch("c", at(200), at(300))
	forgotten := w.Watch("forgotten", at(100), at(110))
	if w.Watch("zero", time.Time{}, at(1)) != nil {
		t.Fatal("a test case with no request time is watched")
	}
	m.RecordOrphanWindowWhy(at(110), at(120), "MySQL: touches a's end")
	m.RecordOrphanWindow(at(61), at(99)) // between b and a: none
	m.RecordOrphanWindowWhy(at(55), at(55), "Http: inside b")
	m.OpenOrphanWindowWhy(at(250), "mysql: stopped")()
	m.RecordOrphanWindow(at(290), at(295)) // no why: inside c
	w.Forget(forgotten)
	m.RecordOrphanWindowWhy(at(105), at(105), "MySQL: after the forget")
	exchange := func(n uint64, what string) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: n, What: what}}
	}
	want := []string{
		fmt.Sprintf("%q %v", "a", exchange(2, "MySQL: touches a's end; MySQL: after the forget")),
		fmt.Sprintf("%q %v", "", exchange(1, "Http: inside b")),
		fmt.Sprintf("%q %v", "c", exchange(2, "mysql: stopped")),
	}
	got := lateText(w.Late())
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("late:\n%v\nwant\n%v", got, want)
	}
	late := w.Late()
	if !late[1].Request.Equal(at(50)) || !late[1].Response.Equal(at(60)) {
		t.Fatalf("the unnamed test case is named %v..%v, not by its window", late[1].Request, late[1].Response)
	}
	// Closed: no span is told to it any more.
	w.Close()
	m.RecordOrphanWindowWhy(at(0), at(1000), "after the close")
	if fmt.Sprint(lateText(w.Late())) != fmt.Sprint(want) {
		t.Fatalf("a closed watch was charged: %v", lateText(w.Late()))
	}
}

// A test case the agent named whose own mock was dropped after it went is
// named with that, by its name; the memory guard's span charges what it
// overlaps; a mock dropped with no owner named charges the test cases it was
// recorded over.
func TestALateWatchNamesTheMocksLetGoAfterATestCaseWent(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	w := m.WatchLate()
	defer w.Close()
	w.Watch("test-1", at(0), at(10))
	w.Watch("test-2", at(20), at(30))
	m.recordDroppedTC("test-1")
	m.send(&models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at(25), ResTimestampMock: at(26)}}, "", nil)
	dropped := func(what string) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: 1, What: what}}
	}
	want := []string{
		fmt.Sprintf("%q %v", "test-1", dropped(MockDroppedOnItsWay)),
		fmt.Sprintf("%q %v", "test-2", dropped("Http: "+MockDroppedWhileItRan)),
	}
	if got := lateText(w.Late()); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("late:\n%v\nwant\n%v", got, want)
	}
}

// Past MaxLateWatched a test case is not watched, and counted.
func TestATestCasePastTheWatchsBoundIsCounted(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	w := m.WatchLate()
	defer w.Close()
	for i := 0; i < MaxLateWatched; i++ {
		if w.Watch("", at(i), at(i)) == nil {
			t.Fatalf("test case %d was not watched", i)
		}
	}
	if w.Watch("one-too-many", at(0), at(1)) != nil || w.Unwatched() != 1 {
		t.Fatalf("past the bound: watched, or not counted (%d)", w.Unwatched())
	}
	var nilWatch *LateWatch
	if nilWatch.Watch("x", at(0), at(1)) != nil || nilWatch.Late() != nil || nilWatch.Unwatched() != 0 {
		t.Fatal("a nil watch watched something")
	}
	nilWatch.Forget(nil)
	nilWatch.Close()
}

// Under the proven switch a mock dropped on its way to keploy record that
// names no owner (a recording without --sync), with no OnDrop hook to take
// it, charges the test cases recorded over it (TestCaseLosses); with a hook
// the hook takes it, and with the switch off nothing is kept, as before.
func TestADroppedMockWithNoOwnerChargesTheTestCasesItWasRecordedOver(t *testing.T) {
	mock := func() *models.Mock {
		return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at(10), ResTimestampMock: at(12)}}
	}
	for _, c := range []struct {
		name          string
		proven, hook  bool
		owner         string
		wantCharged   bool
		wantHookTaken bool
	}{
		{name: "proven, no hook, no owner", proven: true, wantCharged: true},
		{name: "proven, a hook", proven: true, hook: true, wantHookTaken: true},
		{name: "proven, an owner", proven: true, owner: "test-9"},
		{name: "off", proven: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.proven {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			m := New(zap.NewNop())
			taken := 0
			if c.hook {
				m.OnDrop(func(*models.Mock) { taken++ })
			}
			m.send(mock(), c.owner, nil) // no channel bound: dropped
			got := m.TestCaseLosses("test-1", at(11), at(20))
			want := models.CaptureLosses(nil)
			if c.wantCharged {
				want = models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: 1, What: "Http: " + MockDroppedWhileItRan}}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("charged %+v, want %+v", got, want)
			}
			if (taken == 1) != c.wantHookTaken {
				t.Fatalf("the hook took %d", taken)
			}
			if got := m.TestCaseLosses("test-1", at(13), at(20)); len(got) != 0 {
				t.Fatalf("a test case after the mock charged %+v", got)
			}
		})
	}
}

// A test case streamed with what its check found it lost (Streamed) stays
// watched: a span recorded after that names it for a source the check did
// not see, that source alone, counted once; one of a source the check named
// (the span the check read, told to the watch after it, among them) names it
// with nothing. A kind the check did not see is named whole.
func TestALateWatchNamesOnlyTheSourcesATestCaseStreamedWithItsLossLacked(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	w := m.WatchLate()
	defer w.Close()
	lossy := w.Watch("lossy", at(100), at(110))
	m.RecordOrphanWindowWhy(at(100), at(101), "MySQL: early") // read by the check, told to the watch too
	checked := m.TestCaseLosses("lossy", at(100), at(110))
	w.Streamed(lossy, checked)
	m.RecordOrphanWindowWhy(at(102), at(103), "MySQL: early") // the same source again
	m.RecordOrphanWindowWhy(at(104), at(105), "Http: late")
	m.RecordOrphanWindowWhy(at(106), at(107), "Http: late")
	m.SpanDroppedMock(&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: at(108), ResTimestampMock: at(109)}})
	want := []string{fmt.Sprintf("%q %v", "lossy", models.CaptureLosses{
		{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "Http: late"},
		{Kind: models.CaptureLossMockDropped, Count: 1, What: "Mongo: " + MockDroppedWhileItRan},
	})}
	if got := lateText(w.Late()); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("late:\n%v\nwant\n%v", got, want)
	}
	// Only the source its check named: not named.
	only := w.Watch("only", at(200), at(210))
	m.RecordOrphanWindowWhy(at(200), at(201), "MySQL: early")
	w.Streamed(only, m.TestCaseLosses("only", at(200), at(210)))
	m.RecordOrphanWindowWhy(at(205), at(206), "MySQL: early")
	if got := lateText(w.Late()); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("a test case whose later spans its check named is named: %v", got)
	}
	var nilWatch *LateWatch
	nilWatch.Streamed(nil, checked)
	w.Streamed(nil, checked)
}
