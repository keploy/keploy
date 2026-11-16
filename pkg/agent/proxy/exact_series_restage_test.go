package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A background series answered past its recorded count in one staging keeps
// answering after the set is staged again: the new staging starts the
// windows' answers afresh, so the HTTP parser's walk of the call starts over
// at the series' first recording too, and its last answers each repeat once
// every recording answered again. A walk that kept the last staging's served
// marks would offer the last recording alone, which answers once and then
// hides until the others answer, which the walk never offers: every later
// probe would miss.
//
// It holds for a call the parser walks in the view in force (the store's
// sequence view) and for one held to the round it was marked in, as a
// proxied call is (MarkCallRound, HoldToRound): that call walks the windows'
// cursor of the staging its mark read. So does a call marked after the learn
// tracker began the next test set, before the store staged it: it walks the
// windows of the staging still in force, never the last staging's, so after
// the set is staged again its first answer is the series' first recording.
func TestSeriesRepeatsAnswerAfterTheSetIsStagedAgain(t *testing.T) {
	const probePath = "/v1/sys/health"
	mks := func() []*models.Mock {
		return []*models.Mock{
			workerProbe("p1", time.Second, probePath, `{"ok":1}`),
			workerProbe("p2", 2*time.Second, probePath, `{"ok":2}`),
		}
	}
	probe := func(t *testing.T, held bool, mm *MockManager) string {
		t.Helper()
		var (
			body string
			r    *models.MockMismatchReport
		)
		if held {
			db, _ := mm.HoldToRound(mm.MarkCallRound())
			body, r = httpMissSend(t, db, "GET "+probePath+" HTTP/1.1\r\nHost: deps\r\n\r\n")
		} else {
			body, r = httpMissCall(t, mm, probePath)
		}
		if r != nil {
			return "a miss (closest " + r.ClosestMock + ")"
		}
		return body
	}
	for _, held := range []bool{false, true} {
		name := "walked in the view in force"
		if held {
			name = "held to the round it was marked in"
		}
		t.Run(name, func(t *testing.T) {
			mm := stagedManager(t, nil, mks())
			mm.ServeExactWorker([]string{"p1", "p2"})
			mm.SetPoolSeries(map[string][]string{"probe/-/1": {"p1", "p2"}})
			mm.ServeExactPool(models.PoolKeyNone, nil)
			mm.ServeExactTest("TestB", nil)
			var got []string
			for i := 0; i < 3; i++ {
				got = append(got, probe(t, held, mm))
			}
			mm.ServeExactPool(models.PoolKeyNone, nil)
			mm.SetMocksWithWindow(nil, mks(), models.BaseTime, time.Now())
			mm.ServeExactTest("TestC", nil)
			for i := 0; i < 4; i++ {
				got = append(got, probe(t, held, mm))
			}
			want := []string{`{"ok":1}`, `{"ok":2}`, `{"ok":2}`, `{"ok":1}`, `{"ok":2}`, `{"ok":2}`, `{"ok":2}`}
			if !slices.Equal(got, want) {
				t.Fatalf("the probes were answered %q, want %q", got, want)
			}
		})
	}
	t.Run("marked after the tracker began the next set", func(t *testing.T) {
		mm := stagedManager(t, nil, mks())
		mm.ServeExactWorker([]string{"p1", "p2"})
		mm.SetPoolSeries(map[string][]string{"probe/-/1": {"p1", "p2"}})
		mm.ServeExactPool(models.PoolKeyNone, nil)
		tr := learn.NewTracker()
		unstaged := func(t *testing.T, test string) string {
			t.Helper()
			mm.LearnBoundary(tr, func(t *learn.Tracker) { t.NewSet() })
			c := mm.MarkCallRound()
			if !c.Staging() {
				t.Fatal("precondition: a call marked after the tracker began the next set, not staged, is no staging's")
			}
			if s, ok := c.Sequence(); !ok || s.Test != test || s.Between == nil {
				t.Fatalf("precondition: the unstaged set's call walks %s's windows (ok %v): %+v", test, ok, s)
			}
			return probe(t, true, mm)
		}
		// Staged once: the calls after the tracker began the next set walk
		// the series to its end.
		mm.ServeExactTest("TestB", nil)
		got := []string{unstaged(t, "TestB"), unstaged(t, "TestB")}
		// Staged again: the series starts over in its first recording for
		// a call marked before the store staged the set after, as no
		// window of this staging answered it yet, then repeats its last. A
		// walk of the last staging's windows would offer the last alone.
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.SetMocksWithWindow(nil, mks(), models.BaseTime, time.Now())
		mm.ServeExactTest("TestC", nil)
		got = append(got, unstaged(t, "TestC"), unstaged(t, "TestC"), unstaged(t, "TestC"))
		want := []string{`{"ok":1}`, `{"ok":2}`, `{"ok":1}`, `{"ok":2}`, `{"ok":2}`}
		if !slices.Equal(got, want) {
			t.Fatalf("the probes were answered %q, want %q", got, want)
		}
	})
}
