package replay

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// tcAt builds a minimal HTTP test case whose request timestamp is `t`.
func tcAt(t time.Time) *models.TestCase {
	return &models.TestCase{
		Kind:    models.HTTP,
		HTTPReq: models.HTTPReq{Timestamp: t},
	}
}

// TestStartupMockCutoff pins the startup-mock exemption boundary used by
// RemoveUnusedMocks: it must be the (models.StartupMockTestCaseWindow+1)-th test case
// by request time once a set has more tests than the window, fall back to
// keepAll when the whole set is startup, and disable cleanly with no usable
// timestamps.
func TestStartupMockCutoff(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	keepAll := base.Add(-time.Hour) // stand-in for pruneBefore (replay start)

	// at(i) is the i-th second after base; ordering is intentionally shuffled
	// in the slices below so we also exercise the internal sort.
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Second) }

	t.Run("more tests than window picks the (window+1)-th by time", func(t *testing.T) {
		// Window is 5, so with 8 tests the cutoff is the 6th-earliest (index 5),
		// i.e. at(5). Feed them out of order to prove sorting decides, not arrival.
		tcs := []*models.TestCase{
			tcAt(at(3)), tcAt(at(0)), tcAt(at(6)), tcAt(at(1)),
			tcAt(at(7)), tcAt(at(2)), tcAt(at(5)), tcAt(at(4)),
		}
		got := startupMockCutoff(tcs, keepAll, time.Time{})
		if !got.Equal(at(models.StartupMockTestCaseWindow)) {
			t.Fatalf("cutoff = %v, want %v (the (window+1)-th test case)", got, at(models.StartupMockTestCaseWindow))
		}
	})

	t.Run("exactly window tests keeps everything", func(t *testing.T) {
		tcs := make([]*models.TestCase, 0, models.StartupMockTestCaseWindow)
		for i := range models.StartupMockTestCaseWindow {
			tcs = append(tcs, tcAt(at(i)))
		}
		got := startupMockCutoff(tcs, keepAll, time.Time{})
		if !got.Equal(keepAll) {
			t.Fatalf("cutoff = %v, want keepAll %v when the whole set is startup", got, keepAll)
		}
	})

	t.Run("fewer than window tests keeps everything", func(t *testing.T) {
		got := startupMockCutoff([]*models.TestCase{tcAt(at(0)), tcAt(at(1))}, keepAll, time.Time{})
		if !got.Equal(keepAll) {
			t.Fatalf("cutoff = %v, want keepAll %v", got, keepAll)
		}
	})

	t.Run("no test cases disables the exemption", func(t *testing.T) {
		got := startupMockCutoff(nil, keepAll, time.Time{})
		if !got.IsZero() {
			t.Fatalf("cutoff = %v, want zero time", got)
		}
	})

	t.Run("timestamp-less tests are ignored and disable the exemption", func(t *testing.T) {
		// Created==0 and no req timestamps → no usable candidate → zero time.
		tcs := []*models.TestCase{{Kind: models.HTTP}, {Kind: models.HTTP}}
		got := startupMockCutoff(tcs, keepAll, time.Time{})
		if !got.IsZero() {
			t.Fatalf("cutoff = %v, want zero time for timestamp-less set", got)
		}
	})

	t.Run("falls back to grpc and Created timestamps", func(t *testing.T) {
		// Six tests so the window is exceeded; mix the timestamp sources to
		// confirm grpc and the coarse Created fallback are honoured. The
		// 6th-earliest (index 5) is at(5), carried on a Created-only test case.
		tcs := []*models.TestCase{
			tcAt(at(0)),
			{GrpcReq: models.GrpcReq{Timestamp: at(1)}},
			tcAt(at(2)),
			{GrpcReq: models.GrpcReq{Timestamp: at(3)}},
			tcAt(at(4)),
			{Created: at(5).Unix()},
		}
		got := startupMockCutoff(tcs, keepAll, time.Time{})
		// Created has second precision; compare on unix seconds.
		if got.Unix() != at(5).Unix() {
			t.Fatalf("cutoff = %v, want %v from the Created fallback", got, at(5))
		}
	})

	t.Run("earlier recordedFirst pins cutoff for > window tests", func(t *testing.T) {
		earlier := base.Add(-2 * time.Hour)
		tcs := make([]*models.TestCase, 0, models.StartupMockTestCaseWindow+3)
		for i := range models.StartupMockTestCaseWindow + 3 {
			tcs = append(tcs, tcAt(at(i)))
		}
		got := startupMockCutoff(tcs, keepAll, earlier)
		if !got.Equal(earlier) {
			t.Fatalf("cutoff = %v, want earlier recordedFirst %v", got, earlier)
		}
	})

	t.Run("earlier recordedFirst pins cutoff for <= window tests instead of keepAll", func(t *testing.T) {
		earlier := base.Add(-2 * time.Hour)
		tcs := []*models.TestCase{tcAt(at(0)), tcAt(at(1)), tcAt(at(2))}
		got := startupMockCutoff(tcs, keepAll, earlier)
		if !got.Equal(earlier) {
			t.Fatalf("cutoff = %v, want earlier recordedFirst %v (not keepAll %v)", got, earlier, keepAll)
		}
	})

	t.Run("earlier recordedFirst pins cutoff when held tests have no timestamps", func(t *testing.T) {
		earlier := base.Add(-2 * time.Hour)
		tcs := []*models.TestCase{nil, {Kind: models.HTTP}}
		got := startupMockCutoff(tcs, keepAll, earlier)
		if !got.Equal(earlier) {
			t.Fatalf("cutoff = %v, want earlier recordedFirst %v", got, earlier)
		}
	})

	t.Run("equal recordedFirst keeps round-1 window semantics", func(t *testing.T) {
		many := make([]*models.TestCase, 0, models.StartupMockTestCaseWindow+2)
		for i := range models.StartupMockTestCaseWindow + 2 {
			many = append(many, tcAt(at(i)))
		}
		if got := startupMockCutoff(many, keepAll, at(0)); !got.Equal(at(models.StartupMockTestCaseWindow)) {
			t.Fatalf("> window round-1 cutoff = %v, want %v", got, at(models.StartupMockTestCaseWindow))
		}
		few := []*models.TestCase{tcAt(at(0)), tcAt(at(1))}
		if got := startupMockCutoff(few, keepAll, at(0)); !got.Equal(keepAll) {
			t.Fatalf("<= window round-1 cutoff = %v, want keepAll %v", got, keepAll)
		}
	})

	t.Run("later recordedFirst is ignored", func(t *testing.T) {
		later := base.Add(time.Hour)
		many := make([]*models.TestCase, 0, models.StartupMockTestCaseWindow+2)
		for i := range models.StartupMockTestCaseWindow + 2 {
			many = append(many, tcAt(at(i)))
		}
		if got := startupMockCutoff(many, keepAll, later); !got.Equal(at(models.StartupMockTestCaseWindow)) {
			t.Fatalf("cutoff = %v, want %v when recordedFirst is after held tests", got, at(models.StartupMockTestCaseWindow))
		}
	})
}
