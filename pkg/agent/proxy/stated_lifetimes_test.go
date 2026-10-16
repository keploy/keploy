package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// StatesLifetimes is a fact of the set's recording: a mock of the kind staged
// since the set's boundary states its lifetime. It holds through the set's
// later stagings, whatever they hand over, and a test's own mocks being
// consumed; the next set's recording decides it afresh.
func TestStatesLifetimesIsTheSetsRecording(t *testing.T) {
	at := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	mock := func(name string, kind models.Kind, req time.Time, stated bool) *models.Mock {
		m := &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    name,
			Kind:    kind,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "mocks", "connID": "c1"},
				ReqTimestampMock: req,
				ResTimestampMock: req.Add(time.Millisecond),
			},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
		}
		if stated {
			m.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
		}
		return m
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	check := func(step string, kind models.Kind, want bool) {
		t.Helper()
		if got := mm.StatesLifetimes(kind); got != want {
			t.Fatalf("%s: StatesLifetimes(%s) = %v, want %v", step, kind, got, want)
		}
	}

	call := mock("token", models.HTTP, at.Add(time.Second), true)
	query := mock("query", models.MySQL, at.Add(3*time.Second), false)
	mm.ResetForReplaySession()
	mm.BeginTestSet(time.Time{}, nil, []*models.Mock{call, query}, nil)
	check("the set's staging", models.HTTP, true)
	check("the set's staging", models.MySQL, false)

	// The test that recorded the call consumes it; a later test is staged
	// without an HTTP mock.
	mm.SetMocksWithWindow([]*models.Mock{call}, nil, at, at.Add(2*time.Second))
	if !mm.DeleteFilteredMock(*call) {
		t.Fatal("the test's call was not consumed")
	}
	mm.SetMocksWithWindow([]*models.Mock{query}, nil, at.Add(2*time.Second), at.Add(4*time.Second))
	check("a later test's staging", models.HTTP, true)

	// The next set was recorded before lifetimes were stated.
	mm.ResetForReplaySession()
	mm.BeginTestSet(time.Time{}, nil, []*models.Mock{mock("legacy", models.HTTP, at.Add(time.Second), false)}, nil)
	check("the next set's staging", models.HTTP, false)
}
