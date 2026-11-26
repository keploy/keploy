package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The store says a recording is the open test's own or the start-up's from
// its view in force: the test's own recordings and the set's start-up ones
// are; a reached worker: recording, another test's recording, the
// background's under proven routing, and every recording while no test is
// open (a pool view, even one holding the next test's recording ahead) are
// not. The initial staging, which files the whole pool in the startup tier,
// names none as the start-up's.
func TestTheStoreSaysARecordingIsTheOpenTestsOwnOrTheStartupsFromItsView(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	other := exactMock("other", time.Second, models.LifetimePerTest)
	worker := httpCall("w-tick", 2*time.Second, "/tick")
	mm := stagedManager(t, []*models.Mock{own, other}, []*models.Mock{worker})
	says := func(what string, mk *models.Mock, want bool) {
		t.Helper()
		if got := mm.ServedAsOwnOrStartup(mk); got != want {
			t.Fatalf("%s: the store says %s is the open test's own or the start-up's: %v, want %v", what, mk.Name, got, want)
		}
	}
	says("the initial staging", own, false)
	says("the initial staging", worker, false)

	mm.ServeExactWorker([]string{"w-tick"})
	// A pool view holding the next test's recording ahead (to answer its
	// calls in the gap) is no test's view.
	mm.ServeExactPool("k", nil, own)
	says("no test open, own ahead", own, false)

	mm.ServeExactWorker([]string{"w-tick"})
	mm.ServeExactTest("T1", []*models.Mock{own})
	says("T1 open", own, true)
	says("T1 open", worker, false)
	says("T1 open", other, false)

	// A view that holds the reached worker: recording too (in flight into
	// the test) still says it is not the test's own.
	mm.ServeExactWorker([]string{"w-tick"})
	mm.ServeExactTest("T1", []*models.Mock{own, worker})
	says("T1 holding the worker: recording", worker, false)
	says("T1 holding the worker: recording", own, true)

	// Under proven routing the background's recordings (its pool, and its
	// whole worker: scope, reached or not) are never the test's own, even
	// where its view holds them.
	t.Setenv(provenswitch.Env, "1")
	pool := httpCall("pool-1", 3*time.Second, "/pool")
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool-1"}, []string{"w-tick"})
	mm.ServeExactTest("T1", []*models.Mock{own, pool})
	says("under proven routing", pool, false)
	says("under proven routing", own, true)

	start := exactMock("boot", -time.Second, models.LifetimePerTest)
	mm.SetMocksWithWindowThreeTier([]*models.Mock{own, other}, []*models.Mock{worker}, []*models.Mock{start}, exactT0, exactT0.Add(time.Hour))
	mm.ServeExactWorker([]string{"w-tick"})
	mm.ServeExactTest("T1", []*models.Mock{own})
	says("a staging with a start-up slice", start, true)
	says("a staging with a start-up slice", worker, false)
}

// The store tells what it cannot say of a recording apart from what it says
// no to (ServeOwnerOf): with no test's view in force (the initial staging, a
// pool view holding the next test's recording ahead) it cannot say whose any
// recording is, as of a recording with no name, except the start-up's,
// which are the test's whatever the view; with a test's view in force the
// test's own recordings and test-owned start-up recordings are the test's,
// and a worker: recording, a non-startup-owned background recording (even
// when recorded before the first test), and another test's are another's.
// Its yes is ServedAsOwnOrStartup's.
func TestTheStoreSaysWhoseARecordingIsOnlyWhereATestsViewIsInForce(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	other := exactMock("other", time.Second, models.LifetimePerTest)
	worker := httpCall("w-tick", 2*time.Second, "/tick")
	start := exactMock("boot", -time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{own, other}, []*models.Mock{worker})
	says := func(what string, mk *models.Mock, want learn.ServeOwner) {
		t.Helper()
		got := mm.ServeOwnerOf(mk)
		if got != want {
			t.Fatalf("%s: the store says %s is %v, want %v", what, mk.Name, got, want)
		}
		if yes := mm.ServedAsOwnOrStartup(mk); yes != (got == learn.OwnerTest) {
			t.Fatalf("%s: ServedAsOwnOrStartup(%s) = %v beside ServeOwnerOf's %v", what, mk.Name, yes, got)
		}
	}
	says("the initial staging", own, learn.OwnerUnknown)
	says("the initial staging", worker, learn.OwnerUnknown)
	mm.ServeExactWorker([]string{"w-tick"})
	mm.ServeExactPool("k", nil, own)
	says("no test open, own ahead", own, learn.OwnerUnknown)
	says("no test open", worker, learn.OwnerUnknown)
	mm.ServeExactWorker([]string{"w-tick"})
	mm.ServeExactTest("T1", []*models.Mock{own})
	says("T1 open", own, learn.OwnerTest)
	says("T1 open", worker, learn.OwnerOther)
	says("T1 open", other, learn.OwnerOther)
	says("T1 open, a recording with no name", &models.Mock{}, learn.OwnerUnknown)
	mm.SetMocksWithWindowThreeTier([]*models.Mock{own, other}, []*models.Mock{worker}, []*models.Mock{start}, exactT0, exactT0.Add(time.Hour))
	mm.ServeExactWorker([]string{"w-tick"})
	mm.ServeExactPool("k", nil, own)
	says("a staging with a start-up slice, a pool view in force", start, learn.OwnerTest)
	says("a staging with a start-up slice, a pool view in force", worker, learn.OwnerUnknown)
	mm.ServeExactWorker([]string{"w-tick"})
	mm.ServeExactTest("T1", []*models.Mock{own})
	says("a staging with a start-up slice", start, learn.OwnerTest)
	says("a staging with a start-up slice", worker, learn.OwnerOther)
}

// Under the proven switch the proxy wires its store's view into the learn
// tracker (setMockManager): a worker: serve that lands between the open
// test's own serve and a call's arrival does not start the span of the
// call's check against the test's own recording, while LiveSpan and a check
// against the worker: recording start at it. With the switch off nothing is
// filed: every span is LiveSpan's.
func TestTheProxyFilesAWorkerServeAsNotTheOpenTestsOwn(t *testing.T) {
	for _, on := range []bool{true, false} {
		name := "switch off"
		if on {
			name = "switch on"
		}
		t.Run(name, func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "0")
			}
			own := exactMock("own", 0, models.LifetimePerTest)
			own2 := exactMock("own-2", time.Second, models.LifetimePerTest)
			worker := httpCall("w-tick", 2*time.Second, "/tick")
			mm := stagedManager(t, []*models.Mock{own, own2}, []*models.Mock{worker})
			p := startedDrainProxy(t, 10)
			tr := learn.NewTracker()
			tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
			p.learnTracker = tr
			p.setMockManager(mm)
			t.Cleanup(func() { p.setMockManager(nil) })
			if !on {
				// The switch off wires no observer: tell the tracker of the
				// serves as the store would, so only the owner differs.
				mm.SetServedObserver(tr.ServedMock)
			}

			tr.NewSet()
			tr.Begin()
			mm.ServeExactWorker([]string{"w-tick"})
			mm.ServeExactTest("T1", []*models.Mock{own, own2})
			if !mm.DeleteFilteredMock(*own) {
				t.Fatal("the open test's own recording was refused")
			}
			// Distinct instants for the two serves; the order is what counts.
			time.Sleep(2 * time.Millisecond)
			if got := pollFirst(t, mm, "w-tick"); got != "w-tick" {
				t.Fatalf("the reached worker: recording did not answer in the test: %s", got)
			}
			time.Sleep(2 * time.Millisecond)
			arrival := time.Now()

			anyFlow, ok := tr.LiveSpan(arrival)
			if !ok {
				t.Fatalf("no span to %v", arrival)
			}
			ofOwn, ok := tr.LiveSpanOf(arrival, own2)
			if !ok {
				t.Fatalf("no span of the test's own recording to %v", arrival)
			}
			ofWorker, _ := tr.LiveSpanOf(arrival, worker)
			if !ofWorker.From.Equal(anyFlow.From) {
				t.Fatalf("a check against the worker: recording reads its span from %v, want LiveSpan's %v", ofWorker.From, anyFlow.From)
			}
			if !on {
				if !ofOwn.From.Equal(anyFlow.From) {
					t.Fatalf("switch off: a check against the test's own recording reads its span from %v, want LiveSpan's %v", ofOwn.From, anyFlow.From)
				}
				return
			}
			if !ofOwn.From.Before(anyFlow.From) {
				t.Fatalf("a check against the test's own recording reads its span from %v, the worker: serve's (LiveSpan from %v): the worker: serve started it", ofOwn.From, anyFlow.From)
			}
			if !droppedStoreFilesNothing(t, p, tr, arrival, own2) {
				t.Fatal("with the store dropped, the tracker still files by its view")
			}
		})
	}
}

// droppedStoreFilesNothing drops p's store and reports whether a check
// against rec then reads LiveSpan's span: the tracker keeps no hold on the
// store it no longer serves from.
func droppedStoreFilesNothing(t *testing.T, p *Proxy, tr *learn.Tracker, arrival time.Time, rec *models.Mock) bool {
	t.Helper()
	p.setMockManager(nil)
	tr.ServedMockAt(&models.Mock{Name: "after-drop", Kind: models.HTTP}, arrival.Add(-time.Millisecond))
	anyFlow, _ := tr.LiveSpan(arrival)
	of, _ := tr.LiveSpanOf(arrival, rec)
	return of.From.Equal(anyFlow.From)
}

// The proxy wires what its store cannot say of a recording apart from what
// it says no to (setMockManager, ServeOwnerOf): on a clock whose run the
// hooks tell, a check against the test's own recording carries where the
// test began (learn.Span.TestFrom) once a serve made after that begin starts
// its span, and one against the reached worker: recording carries none.
// Once a pool view replaces the test's while the tracker's test is still
// open, the store cannot say whose the worker: recording is, and its span
// carries the test's begin as LiveSpan's does.
func TestTheProxyTellsTheTrackerWhatItsStoreCannotSayOfARecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own := exactMock("own", 0, models.LifetimePerTest)
	own2 := exactMock("own-2", time.Second, models.LifetimePerTest)
	worker := httpCall("w-tick", 2*time.Second, "/tick")
	mm := stagedManager(t, []*models.Mock{own, own2}, []*models.Mock{worker})
	p := startedDrainProxy(t, 10)
	tr := learn.NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, true })
	p.learnTracker = tr
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })

	tr.NewSet()
	tr.Begin()
	time.Sleep(2 * time.Millisecond)
	mm.ServeExactWorker([]string{"w-tick"})
	mm.ServeExactTest("T1", []*models.Mock{own, own2})
	if !mm.DeleteFilteredMock(*own) {
		t.Fatal("the open test's own recording was refused")
	}
	time.Sleep(2 * time.Millisecond)
	if got := pollFirst(t, mm, "w-tick"); got != "w-tick" {
		t.Fatalf("the reached worker: recording did not answer in the test: %s", got)
	}
	time.Sleep(2 * time.Millisecond)
	arrival := time.Now()
	ofOwn, ok := tr.LiveSpanOf(arrival, own2)
	if !ok || ofOwn.TestFrom.IsZero() {
		t.Fatalf("a check against the test's own recording: span %+v,%v; want it carrying the test's begin", ofOwn, ok)
	}
	if ofWorker, ok := tr.LiveSpanOf(arrival, worker); !ok || !ofWorker.TestFrom.IsZero() {
		t.Fatalf("a check against the worker: recording: span %+v,%v; want none of the test's begin", ofWorker, ok)
	}
	mm.ServeExactPool("k", nil, own2)
	if ofWorker, ok := tr.LiveSpanOf(arrival, worker); !ok || !ofWorker.TestFrom.Equal(ofOwn.TestFrom) {
		t.Fatalf("a pool view in force, the test still open: span of the worker: recording %+v,%v; want the test's begin %v", ofWorker, ok, ofOwn.TestFrom)
	}
}

// A worker: or background recording made before the first test window (such as
// a queue consumer's delivering poll recorded at boot) is placed in the startup
// tier by SetMocksWithWindow, yet belongs to worker:/background rather than to
// any open test. While a test's view is in force, ServeOwnerOf must return
// OwnerOther for it so a concurrent serve of that recording during the test
// does not narrow the test's OwnCall clock window past the handler's clock
// read, while genuine non-worker startup recordings still return OwnerTest.
func TestServeOwnerOfTreatsPreFirstTestWorkerAndBackgroundAsOwnerOther(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	boot := exactMock("boot-init", -3*time.Second, models.LifetimePerTest)
	bootInBg := httpCall("boot-startup-bg", -2500*time.Millisecond, "/startup/config")
	poll := httpCall("mock-2", -2*time.Second, "/sqs/receive")
	bgPool := httpCall("mock-3", -time.Second, "/mongo/read")
	own := exactMock("post-url-3-update", 0, models.LifetimePerTest)
	own.Spec.ReqTimestampMock = exactT0.Add(15 * time.Millisecond)

	mm := NewMockManager(nil, nil, nil)
	// Pre-first-test perTest mocks (like S12 (a)'s delivering SQS poll mock-2
	// and consumer read mock-3) land in startupInit -> m.startupNames when
	// SetMocksWithWindow partitions filtered mocks placed before firstStart,
	// even though mappings.yaml files them under worker:/background. A plain
	// startup: recording marked Startup: true in the background pool stays
	// startup-owned (OwnerTest) during a test window.
	mm.SetMocksWithWindow(
		[]*models.Mock{boot, bootInBg, poll, bgPool, own},
		nil,
		exactT0,
		exactT0.Add(time.Hour),
	)
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{
		{Mock: bootInBg, Startup: true},
		{Mock: bgPool},
		{Mock: poll},
	})
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"boot-startup-bg", "mock-3"}, []string{"mock-2"})
	mm.ServeExactWorker([]string{"mock-2"})
	mm.ServeExactTest("post-url-3", []*models.Mock{own})

	for _, tc := range []struct {
		what string
		mk   *models.Mock
		want learn.ServeOwner
	}{
		{"pre-first-test worker poll", poll, learn.OwnerOther},
		{"pre-first-test background pool mock", bgPool, learn.OwnerOther},
		{"plain startup recording in background pool", bootInBg, learn.OwnerTest},
		{"non-worker startup mock", boot, learn.OwnerTest},
		{"open test own mock", own, learn.OwnerTest},
	} {
		if got := mm.ServeOwnerOf(tc.mk); got != tc.want {
			t.Errorf("%s (%s): ServeOwnerOf = %v, want %v", tc.what, tc.mk.Name, got, tc.want)
		}
	}

	// End-to-end window check: a concurrent serve of mock-2 between the
	// handler's time.Now() read and its dependency call arrival must not advance
	// OwnCall.Window's F.From past the read.
	base := time.Date(2026, 11, 26, 15, 6, 0, 0, time.UTC)
	moveTo := base.Add(100 * time.Microsecond)
	recBegin := exactT0.Add(10 * time.Millisecond)
	lead := learn.FullRoundLead
	off := recBegin.Add(lead).Sub(moveTo)

	tr := learn.NewTracker()
	tr.SetServeOwnerOf(mm.ServeOwnerOf)
	tr.SetAppClock(func(r time.Time) (time.Time, bool) {
		if r.Before(moveTo) {
			return r, true
		}
		return r.Add(off), true
	})
	tr.SetAppClockRun(func(r time.Time) (time.Time, bool) {
		if r.Before(moveTo) {
			return time.Time{}, true
		}
		return moveTo, true
	})
	tr.NewSetAt(base.Add(-time.Second))
	tr.ServedMockAt(&models.Mock{Name: "get-lhr4bwai-1", Kind: models.Mongo}, base.Add(-10*time.Millisecond))
	tr.RecordedBegin(recBegin)
	tr.BeginAt(base)

	handlerRead := moveTo.Add(400 * time.Microsecond).Add(off)
	workerServe := moveTo.Add(1500 * time.Microsecond)
	callArrival := moveTo.Add(2200 * time.Microsecond)

	tr.ServedMockAt(poll, workerServe)

	prev := exactMock("get-lhr4bwai-1", 0, models.LifetimePerTest)
	prev.Spec.ReqTimestampMock = exactT0.Add(5 * time.Millisecond)
	tests := recordedWindow{start: recBegin, end: exactT0.Add(20 * time.Millisecond)}
	oc := tr.NewOwnCall(own, nil, tests)
	if oc == nil {
		t.Fatal("NewOwnCall returned nil")
	}
	oc.See(prev)
	w := oc.Window(callArrival)
	if w == nil {
		t.Fatal("OwnCall.Window returned nil")
	}
	if w.F.From.After(handlerRead) {
		t.Fatalf("OwnCall.Window F.From = %v advanced past handlerRead %v (worker serve at %v leaked into own-test span)", w.F.From, handlerRead, workerServe.Add(off))
	}
}

type recordedWindow struct{ start, end time.Time }

func (rw recordedWindow) InRecordedTestWindow(at time.Time) (bool, bool) {
	return !at.Before(rw.start) && !at.After(rw.end), true
}

func (rw recordedWindow) SameRecordedTestWindow(a, b time.Time) (bool, bool) {
	inA, _ := rw.InRecordedTestWindow(a)
	inB, _ := rw.InRecordedTestWindow(b)
	return inA && inB, true
}
