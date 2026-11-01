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
