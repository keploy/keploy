package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A background series (a liveness probe the application makes every 10 s,
// a declared background rule's: one request, one answer) whose recordings
// are data-plane HTTP client calls, as a recorder writes them
// (ConsumeCursorSaturate), placed in the worker scope in record order. Once
// each answered in the windows, its last-placed recording answers every
// later call in a window: the second and third of one window too, not only
// the first call of each window; a replay slower than the record makes more
// probes than were recorded, in any window.
func TestSeriesLastMemberAnswersEveryRepeatOfOneWindowThroughTheHTTPParser(t *testing.T) {
	const probePath = "/v1/sys/health"
	mks := []*models.Mock{
		workerProbe("p1", time.Second, probePath, `{"ok":1}`),
		workerProbe("p2", 2*time.Second, probePath, `{"ok":2}`),
	}
	mm := stagedManager(t, nil, mks)
	mm.ServeExactWorker([]string{"p1", "p2"})
	mm.SetPoolSeries(map[string][]string{"probe/-/1": {"p1", "p2"}})
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("TestB", nil)
	var got []string
	for i := 0; i < 4; i++ {
		body, _ := httpMissCall(t, mm, probePath)
		got = append(got, body)
	}
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("TestC", nil)
	for i := 0; i < 3; i++ {
		body, _ := httpMissCall(t, mm, probePath)
		got = append(got, body)
	}
	want := []string{`{"ok":1}`, `{"ok":2}`, `{"ok":2}`, `{"ok":2}`, `{"ok":2}`, `{"ok":2}`, `{"ok":2}`}
	if !slices.Equal(got, want) {
		t.Fatalf("the probes were answered %q, want %q", got, want)
	}
	if got := mm.SeriesRepeats()["probe/-/1"]; got != 5 {
		t.Fatalf("repeats %d, want 5", got)
	}
}
