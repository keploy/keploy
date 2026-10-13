package utils

import (
	"context"
	"os"
	"path/filepath"
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
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	reportTeardownProgress(ctx, zap.NewNop(), pathSink(path), stop)
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

// An empty path reports nothing.
func TestReportTeardownProgressWithoutAPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ReportTeardownProgress(ctx, zap.NewNop(), "") // returns, starts nothing
	if err := ReportTeardownProgressTo(ctx, zap.NewNop(), ""); err != nil {
		t.Fatalf("an empty command-line path: %v", err)
	}
}

// Work in progress is reported as such until it ends, and its start and end
// are each a step.
func TestTeardownWorkIsReportedBusy(t *testing.T) {
	defer func(b time.Duration) { teardownBeat = b }(teardownBeat)
	teardownBeat = 5 * time.Millisecond
	path := filepath.Join(t.TempDir(), "teardown.json")
	ctx, begin := context.WithCancel(context.Background())
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	reportTeardownProgress(ctx, zap.NewNop(), pathSink(path), stop)
	begin()
	before := awaitReport(t, path, func(TeardownProgress) bool { return true }).Seq
	end := BeginTeardownWork()
	awaitReport(t, path, func(p TeardownProgress) bool { return p.Busy && p.Seq == before+1 })
	end()
	end() // once
	awaitReport(t, path, func(p TeardownProgress) bool { return !p.Busy && p.Seq == before+2 })
}
