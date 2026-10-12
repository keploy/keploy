package http

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The barrier must never starve a read pool into a miss. Measured on the atg
// k8 set, filtering MyApps from ~1855 candidates down to 20 drove its miss rate
// from 2% to 93%: the survivors were too few to answer the request, so the
// barrier manufactured the very miss its doc comment promises it cannot.
func mkMock(name string, ts time.Time) *models.Mock {
	m := &models.Mock{Name: name}
	m.Spec.ReqTimestampMock = ts
	return m
}

func TestFilterStaleHonoursSurvivorFloor(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var w writeBarrier
	w.arm("app", base)

	// 1855 candidates, only 20 recorded after the barrier: the shape that
	// produced the 93% miss rate. Filtering must decline.
	mocks := make([]*models.Mock, 0, 1855)
	for i := 0; i < 1835; i++ {
		mocks = append(mocks, mkMock("old", base.Add(-time.Minute)))
	}
	for i := 0; i < 20; i++ {
		mocks = append(mocks, mkMock("new", base.Add(time.Minute)))
	}

	got, dropped := w.filterStale(mocks, "operationName=MyApps")
	if dropped != 0 || len(got) != len(mocks) {
		t.Fatalf("barrier starved the pool: kept %d of %d (dropped %d); want pool returned untouched",
			len(got), len(mocks), dropped)
	}
}

func TestFilterStaleStillFiltersWhenPoolStaysHealthy(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var w writeBarrier
	w.arm("app", base)

	// Plenty of post-barrier candidates: the barrier must still do its job,
	// otherwise the stale-read bug it exists to fix comes back.
	mocks := make([]*models.Mock, 0, 300)
	for i := 0; i < 100; i++ {
		mocks = append(mocks, mkMock("old", base.Add(-time.Minute)))
	}
	for i := 0; i < 200; i++ {
		mocks = append(mocks, mkMock("new", base.Add(time.Minute)))
	}

	got, dropped := w.filterStale(mocks, "operationName=MyApps")
	if dropped != 100 || len(got) != 200 {
		t.Fatalf("barrier stopped filtering: kept %d, dropped %d; want kept 200, dropped 100", len(got), dropped)
	}
}
