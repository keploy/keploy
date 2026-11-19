package pkg

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A worker-scope mock the replay serves to every test (models.ServeAsSession
// marks its copy) is every window's per-test mock, wherever its call was made:
// a parser that reads only the per-test pool (Mongo's data calls) finds it in
// every test. In the session pool it stays a session mock. An unmarked mock
// placed alike is out of the window, as before.
func TestAWorkerScopeMockIsEveryWindowsOwn(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	between := func(marked bool) *models.Mock {
		m := &models.Mock{Kind: models.Mongo, Name: "timer"}
		// Made between two tests, after the first window, outside this one.
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = t0.Add(30*time.Second), t0.Add(30*time.Second+time.Millisecond)
		m.TestModeInfo.Lifetime = models.LifetimeSession
		m.TestModeInfo.LifetimeDerived = true
		m.TestModeInfo.Background = marked
		return m
	}
	for _, strict := range []bool{true, false} {
		w := ServeWindow{Start: t0.Add(time.Minute), End: t0.Add(time.Minute + time.Second), FirstStart: t0, StrictPerCall: strict, StrictWindow: strict, Resolved: true}
		if got := ServeInWindow(between(true), false, w); got != ServedPerTest {
			t.Errorf("strict %v: a worker-scope mock in the per-test pool is served %v, want per-test", strict, got)
		}
		if got := ServeInWindow(between(true), true, w); got != ServedSession {
			t.Errorf("strict %v: a worker-scope mock in the session pool is served %v, want session", strict, got)
		}
		if got, _ := StageInWindow(nil, between(true), w); got != ServedPerTest {
			t.Errorf("strict %v: the mock manager stages a worker-scope mock %v, want per-test", strict, got)
		}
		unmarked := between(false)
		unmarked.TestModeInfo.Lifetime = models.LifetimePerTest
		if got, _ := StageInWindow(nil, unmarked, w); got == ServedPerTest {
			t.Errorf("strict %v: an unmarked mock outside the window is staged per-test", strict)
		}
	}
}
