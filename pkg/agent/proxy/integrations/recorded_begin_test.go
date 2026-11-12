package integrations

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The store tells where the recorded test a recording lies in began: the
// start of the named recorded window it lies in. A time inside no window, a
// window named by no test (windows that overlapped, read as one), a zero
// time and a staging with no windows tell none.
func TestRoundSnapshotTellsWhereTheRecordedTestARecordingLiesInBegan(t *testing.T) {
	a := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	b := a.Add(13 * time.Second)
	merged := a.Add(time.Minute)
	snap := RoundSnapshot{}.WithParts(RoundParts{Windows: models.NewWindowSchedule([]models.TestWindow{
		{TestCase: "test-1", Start: a, End: a.Add(4 * time.Second)},
		{TestCase: "test-2", Start: b, End: b.Add(10 * time.Second)},
		{Start: merged, End: merged.Add(10 * time.Second)},
	})})
	for _, tc := range []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"inside test-1's window", a.Add(155 * time.Millisecond), a},
		{"at test-2's begin", b, b},
		{"at test-2's end", b.Add(10 * time.Second), b},
		{"between the windows", a.Add(8 * time.Second), time.Time{}},
		{"before every window", a.Add(-time.Second), time.Time{}},
		{"inside a window named by no test", merged.Add(time.Second), time.Time{}},
		{"zero", time.Time{}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := snap.RecordedTestBegan(tc.at)
			if ok != !tc.want.IsZero() || !got.Equal(tc.want) {
				t.Fatalf("RecordedTestBegan = %v, %v; want %v", got, ok, tc.want)
			}
		})
	}
	if got, ok := (RoundSnapshot{}).RecordedTestBegan(a); ok || !got.IsZero() {
		t.Fatalf("no windows: RecordedTestBegan = %v, %v; want none", got, ok)
	}
}
