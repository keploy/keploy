package utils

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// readReport returns the teardown progress at path, once there is one.
func readReport(path string) (TeardownProgress, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return TeardownProgress{}, false
	}
	p, err := ParseTeardownProgress(b)
	return p, err == nil
}

// awaitReport returns the first report at path that satisfies ok, failing the
// test if none comes within a bound that only turns a hang into a failure.
func awaitReport(t *testing.T, path string, ok func(TeardownProgress) bool) TeardownProgress {
	t.Helper()
	hung := time.After(time.Minute)
	for {
		if p, read := readReport(path); read && ok(p) {
			return p
		}
		select {
		case <-hung:
			p, _ := readReport(path)
			t.Fatalf("no such report came; the last was %+v", p)
		case <-time.After(time.Millisecond):
		}
	}
}

// startReporter starts a reporter (reportTeardownProgress) writing to sink,
// stopped when the test ends -- and waited for, before the test's temporary
// directory is removed: a reporter still writing there would leave a file
// behind, and the removal would fail.
func startReporter(t *testing.T, ctx context.Context, sink progressSink, between func()) {
	t.Helper()
	stop := make(chan struct{})
	exited := reportTeardownProgress(ctx, zap.NewNop(), sink, stop, between)
	t.Cleanup(func() {
		close(stop)
		select {
		case <-exited:
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Error("the reporter did not return once stopped")
		}
	})
}

// recordingSink keeps the reports a reporter writes, in order, up to its
// capacity; it drops the rest rather than block the reporter.
type recordingSink chan []byte

func (recordingSink) clear() error { return nil }

func (r recordingSink) write(b []byte) error {
	select {
	case r <- append([]byte(nil), b...):
	default: // the test reads only the first few
	}
	return nil
}

// The reporter writes nothing until the teardown begins (the root context's
// cancel), and removes what an earlier agent left; from then on it reports at
// once, on every step and on the end of the detach, and on every beat.
func TestReportTeardownProgress(t *testing.T) {
	defer func(b time.Duration) { teardownBeat = b }(teardownBeat)
	teardownBeat = 5 * time.Millisecond
	teardownSeq.Store(0)
	teardownDone.Store(false)
	t.Cleanup(func() { teardownSeq.Store(0); teardownDone.Store(false) })

	path := filepath.Join(t.TempDir(), "teardown.json")
	if err := os.WriteFile(path, []byte(`{"seq":7,"beat":9,"done":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, begin := context.WithCancel(context.Background())
	startReporter(t, ctx, pathSink(path), nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an earlier agent's report is still there (%v): it would pass for this agent's", err)
	}
	TeardownStep() // before the teardown: recorded, not yet written
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the reporter wrote before the teardown began (%v)", err)
	}

	begin()
	first := awaitReport(t, path, func(TeardownProgress) bool { return true })
	if first.Seq != 1 || first.Done {
		t.Fatalf("first report %+v, want the step taken so far and not done", first)
	}
	awaitReport(t, path, func(p TeardownProgress) bool { return p.Beat >= first.Beat+2 })
	TeardownStep()
	awaitReport(t, path, func(p TeardownProgress) bool { return p.Seq == 2 })
	MarkDetachDone()
	awaitReport(t, path, func(p TeardownProgress) bool { return p.Done && p.Seq == 2 })
}

// StopTeardownReporting ends every reporter the agent's entry points started,
// writing or not yet, and returns once none writes any more; a reporter
// started after it runs, and the next call ends that one too.
func TestStopTeardownReportingEndsEveryReporter(t *testing.T) {
	defer func(b time.Duration) { teardownBeat = b }(teardownBeat)
	teardownBeat = time.Millisecond
	for round := 0; round < 2; round++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "teardown.json")
		ctx, begin := context.WithCancel(context.Background())
		ReportTeardownProgress(ctx, zap.NewNop(), path)
		idle, cancelIdle := context.WithCancel(context.Background()) // its teardown never begins
		ReportTeardownProgress(idle, zap.NewNop(), filepath.Join(t.TempDir(), "teardown.json"))
		begin()
		first := awaitReport(t, path, func(TeardownProgress) bool { return true })
		awaitReport(t, path, func(p TeardownProgress) bool { return p.Beat > first.Beat })

		stopped := make(chan struct{})
		go func() { StopTeardownReporting(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("round %d: StopTeardownReporting never returned: a reporter it should have ended still runs", round)
		}
		cancelIdle()
		if n := reporting.live.Load(); n != 0 {
			t.Fatalf("round %d: %d reporters still running after StopTeardownReporting returned", round, n)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("round %d: a stopped reporter's directory does not come away: %v", round, err)
		}
	}
}

// An empty path reports nothing.
func TestReportTeardownProgressWithoutAPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ReportTeardownProgress(ctx, zap.NewNop(), "") // returns, starts nothing
	if err := ReportTeardownProgressTo(ctx, zap.NewNop(), ""); err != nil {
		t.Fatalf("an empty command-line path: %v", err)
	}
}

// withoutWorkChecks has the stages of teardown work a test begins go
// unchecked (newWorkProbe), as where their progress cannot be seen: they then
// count as making progress throughout, and take no steps but their start and
// end, which a test that counts steps relies on.
func withoutWorkChecks(t *testing.T) {
	t.Helper()
	old := newWorkProbe
	newWorkProbe = func() workProbe { return nil }
	t.Cleanup(func() { newWorkProbe = old })
}

// Work in progress is reported as such until it ends, and its start and end
// are each a step.
func TestTeardownWorkIsReportedBusy(t *testing.T) {
	withoutWorkChecks(t)
	defer func(b time.Duration) { teardownBeat = b }(teardownBeat)
	teardownBeat = 5 * time.Millisecond
	path := filepath.Join(t.TempDir(), "teardown.json")
	ctx, begin := context.WithCancel(context.Background())
	startReporter(t, ctx, pathSink(path), nil)
	begin()
	before := awaitReport(t, path, func(TeardownProgress) bool { return true }).Seq
	end := BeginTeardownWork()
	awaitReport(t, path, func(p TeardownProgress) bool { return p.Busy && p.Seq == before+1 })
	end()
	end() // once
	awaitReport(t, path, func(p TeardownProgress) bool { return !p.Busy && p.Seq == before+2 })
}

// A report never pairs a step from before a stage of teardown work ended with
// an agent no longer busy: the CLI would count every beat since the last
// report it read, up to the whole stage, as checks without progress. It rests
// on two orders, and each case guards one: the work ends between the report's
// loads (Busy, then Seq), and the report is taken as the work ends (its step,
// then the end of the work). Either way the report reads the work in progress
// or the step that ended it.
func TestATeardownReportIsNeverTorn(t *testing.T) {
	withoutWorkChecks(t)
	if TeardownBusy() {
		t.Fatal("teardown work an earlier test began is still in progress: every report reads busy, and this test would prove nothing")
	}
	// Either case's report is the same: taken with the work in progress, it
	// still reads it so, and the end's step too.
	assertNotTorn := func(t *testing.T, before uint64, p TeardownProgress) {
		t.Helper()
		if p.Seq == before && !p.Busy {
			t.Fatalf("a torn report: no step since %d and not busy, across a stage of work (%+v)", before, p)
		}
		if !p.Busy || p.Seq != before+1 {
			t.Fatalf("the report read Busy=%v and %d steps since %d, want the work in progress and the one step that ended it: its hook ran out of place, or the order this case guards is broken (%+v)", p.Busy, p.Seq-before, before, p)
		}
	}
	t.Run("the work ends between the report's loads", func(t *testing.T) {
		// The reporter the agent runs, its teardown begun, taking its first
		// report as the work ends.
		end := BeginTeardownWork()
		defer end() // once: a no-op if the report ended it
		before := TeardownSteps()
		var ended atomic.Bool
		reports := make(recordingSink, 8)
		ctx, begin := context.WithCancel(context.Background())
		begin()
		startReporter(t, ctx, reports, func() {
			if ended.CompareAndSwap(false, true) {
				end()
			}
		})
		var p TeardownProgress
		select {
		case b := <-reports:
			var err error
			if p, err = ParseTeardownProgress(b); err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("the reporter wrote no report")
		}
		if !ended.Load() {
			t.Fatal("the report never ran its hook between its loads: this case proves nothing")
		}
		assertNotTorn(t, before, p)
	})
	t.Run("the report is taken as the work ends", func(t *testing.T) {
		var p TeardownProgress
		taken := false
		end := beginTeardownWork(func() { p = snapshotTeardown(7, nil); taken = true })
		before := TeardownSteps()
		end()
		if !taken {
			t.Fatal("the end of the work never ran its hook between its step and the end: this case proves nothing")
		}
		assertNotTorn(t, before, p)
	})
}
