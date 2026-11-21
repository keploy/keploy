package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The mock manager keeps a worker-scope mock (models.ServeAsSession's marked
// copy) in each test's per-test pool, whatever the window, and a reusable
// serve of it leaves it there: Mongo matches a data call from that pool
// alone, so a timer's call between the tests is answered in every test.
func TestAWorkerScopeMockIsInEveryTestsPerTestPool(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t0 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	timer := &models.Mock{Kind: models.Mongo, Name: "timer"}
	timer.Spec.ReqTimestampMock, timer.Spec.ResTimestampMock = t0.Add(5*time.Second), t0.Add(5*time.Second+time.Millisecond)
	timer.TestModeInfo.Lifetime, timer.TestModeInfo.LifetimeDerived, timer.TestModeInfo.Background = models.LifetimeSession, true, true
	own := func(name string, at time.Time) *models.Mock {
		m := &models.Mock{Kind: models.Mongo, Name: name}
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		return m
	}
	has := func(name string) bool {
		got, err := mm.GetFilteredMocks()
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range got {
			if m.Name == name {
				return true
			}
		}
		return false
	}
	// Two tests, neither of whose windows holds the timer's call.
	for i, start := range []time.Time{t0, t0.Add(10 * time.Second)} {
		mm.SetMocksWithWindow([]*models.Mock{own("own", start.Add(time.Millisecond)), timer}, []*models.Mock{timer}, start, start.Add(time.Second))
		if !has("timer") {
			t.Fatalf("test %d: the per-test pool has no worker-scope mock", i+1)
		}
		if !has("own") {
			t.Fatalf("test %d: the test's own mock is missing", i+1)
		}
		if !mm.MarkMockAsUsed(*timer) {
			t.Fatalf("test %d: a serve of the worker-scope mock was refused", i+1)
		}
		if !has("timer") {
			t.Fatalf("test %d: a serve took the worker-scope mock out of the per-test pool", i+1)
		}
	}
}
