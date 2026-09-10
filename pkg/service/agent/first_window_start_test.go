package agent

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func TestEffectiveFirstWindowStart(t *testing.T) {
	t1 := time.Date(2026, 9, 1, 12, 0, 10, 0, time.UTC)
	t7 := t1.Add(60 * time.Second)
	var zero time.Time

	for _, tc := range []struct {
		name      string
		windowed  bool
		readerVal time.Time
		afterTime time.Time
		want      time.Time
		why       string
	}{
		{
			name:     "first window on a windowed proxy derives from this call",
			windowed: true, readerVal: zero, afterTime: t1, want: t1,
			why: "the manager adopts this same start later in this call; without it the startup " +
				"band is switched off for test #1, the test a slow bootstrap lands in",
		},
		{
			name:     "later window keeps the reader's value",
			windowed: true, readerVal: t1, afterTime: t7, want: t1,
			why: "the cutoff is the FIRST window, not the current one",
		},
		{
			// This is the case that made an earlier version of this fix reintroduce
			// cross-test bleed: no reader means zero on EVERY window, so an
			// unguarded fallback would substitute the current window's start and
			// collapse the startup predicate into "req < afterTime".
			name:     "no windowed proxy never derives, even with a real window",
			windowed: false, readerVal: zero, afterTime: t7, want: zero,
			why: "strict is live on this path; deriving here preserves every stale mock",
		},
		{
			name:     "BaseTime means no window",
			windowed: true, readerVal: zero, afterTime: models.BaseTime, want: zero,
			why: "matches the manager's own guard",
		},
		{
			name:     "zero AfterTime means no window",
			windowed: true, readerVal: zero, afterTime: zero, want: zero,
			why: "nothing to derive from",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveFirstWindowStart(tc.windowed, tc.readerVal, tc.afterTime)
			if !got.Equal(tc.want) {
				t.Fatalf("got %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}
