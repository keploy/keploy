package async

import (
	"context"
	"regexp"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func observedEngine(t *testing.T, p AsyncParser) (*Engine, *observer.ObservedLogs, *zap.Logger) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	return newTestEngine(p), logs, zap.New(core)
}

func verdictCount(logs *observer.ObservedLogs) int {
	return len(logs.FilterMessage("async egress verdict").All())
}

func driftCount(logs *observer.ObservedLogs) int {
	n := 0
	for _, e := range logs.All() {
		if len(e.Message) >= 24 && e.Message[:24] == "async egress shape drift" {
			n++
		}
	}
	return n
}

func fieldInt(t *testing.T, e observer.LoggedEntry, key string) int64 {
	t.Helper()
	for _, f := range e.Context {
		if f.Key == key {
			return f.Integer
		}
	}
	t.Fatalf("verdict line has no %q field", key)
	return 0
}

// LogReport used to be sync.Once-guarded, so the verdict was printed at the end
// of TEST-SET 1 and the accumulation for sets 2..N was silently discarded.
//
// The counts stay CUMULATIVE across flushes rather than becoming per-set
// deltas. CI greps the line with `tail -n1`, so the last line must describe the
// whole run; deltas would narrow that gate to "the last chunk" and let a
// straggler poll — woken by the test-set context being cancelled — report
// served:0 on a run that served dozens.
func TestLogReportEmitsPerFlushWithCumulativeCounts(t *testing.T) {
	e, logs, lg := observedEngine(t, &fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10}
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "V0")})

	e.Decide(context.Background(), lane, &models.Mock{})
	e.LogReport(lg)
	if got := verdictCount(logs); got != 1 {
		t.Fatalf("first flush emitted %d verdict lines, want 1", got)
	}
	if got := fieldInt(t, logs.All()[0], "served"); got != 1 {
		t.Fatalf("first verdict served=%d, want 1", got)
	}

	e.Decide(context.Background(), lane, &models.Mock{})
	e.LogReport(lg)
	all := logs.FilterMessage("async egress verdict").All()
	if len(all) != 2 {
		t.Fatalf("second flush emitted %d verdict lines total, want 2: the sync.Once "+
			"guard is back, so every test-set after the first is invisible", len(all))
	}
	if got := fieldInt(t, all[1], "served"); got != 2 {
		t.Fatalf("second verdict served=%d, want 2 (CUMULATIVE): per-set deltas make "+
			"the last line — the one CI reads with tail -n1 — describe only the final "+
			"chunk instead of the run", got)
	}
}

// Drift DETAILS are drained on emit, so each is logged exactly once. Retaining
// them while removing the Once guard would re-log the whole backlog on every
// flush, making output quadratic in (test-sets x drifts).
func TestLogReportDrainsDriftDetails(t *testing.T) {
	e, logs, lg := observedEngine(t, &fakeParser{matches: true, shapeOK: false, empty: []byte("KA")})
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10}
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "V0")})

	e.Decide(context.Background(), lane, &models.Mock{})
	e.LogReport(lg)
	if got := driftCount(logs); got != 1 {
		t.Fatalf("first flush logged %d drift details, want 1", got)
	}

	e.Decide(context.Background(), lane, &models.Mock{})
	e.LogReport(lg)
	if got := driftCount(logs); got != 2 {
		t.Fatalf("logged %d drift details in total, want 2: the backlog is re-logged on "+
			"every flush, so output grows quadratically with test-sets", got)
	}
	if got := fieldInt(t, logs.FilterMessage("async egress verdict").All()[1], "shape_flags"); got != 2 {
		t.Fatalf("second verdict shape_flags=%d, want the cumulative 2", got)
	}
}

// A flush with nothing new since the last line must stay silent — otherwise the
// run-level flush that follows a per-set flush prints a duplicate.
func TestLogReportQuietWhenNothingNew(t *testing.T) {
	e, logs, lg := observedEngine(t, &fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10}
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "V0")})

	e.Decide(context.Background(), lane, &models.Mock{})
	e.LogReport(lg)
	before := verdictCount(logs)

	e.LogReport(lg) // no traffic in between
	if got := verdictCount(logs); got != before {
		t.Fatalf("a repeat flush with no new activity emitted another line (%d -> %d)",
			before, got)
	}
}

// An engine that never served anything must stay silent: record mode never
// increments, and LoadAsyncMocks now flushes at every test-set boundary
// including sets with no async traffic at all.
func TestLogReportQuietOnVirginEngine(t *testing.T) {
	e, logs, lg := observedEngine(t, &fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.LogReport(lg)
	if got := len(logs.All()); got != 0 {
		t.Fatalf("a virgin engine emitted %d log entries, want 0: record mode and "+
			"async-free test-sets would print served:0 noise", got)
	}
}

// The verdict line is machine-read. This pins the field names AND their order
// against the verbatim regex from
// .github/workflows/test_workflow_scripts/java/async_config_poll/java-linux.sh:
//
//	'"served": [0-9]+, "shape_flags": [0-9]+, "not_exercised": [0-9]+'
//
// Reordering the zap.Int calls or renaming a field breaks that gate silently —
// the lane would report "async lane was not evaluated" on a healthy run.
func TestVerdictLineMatchesTheCIRegex(t *testing.T) {
	e, logs, lg := observedEngine(t, &fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10}
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "V0")})
	e.Decide(context.Background(), lane, &models.Mock{})
	e.LogReport(lg)

	entries := logs.FilterMessage("async egress verdict").All()
	if len(entries) != 1 {
		t.Fatalf("want exactly one verdict line, got %d", len(entries))
	}
	// The CONSOLE encoder is what keploy runs (utils/log: NewDevelopmentConfig
	// with a custom "ansiConsole" encoding), and it renders context fields as
	// {"served": 2, "shape_flags": 0, ...} — byte-for-byte what the CI script
	// greps. No normalisation: matching the real encoder's real output is the
	// whole point.
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	buf, err := enc.EncodeEntry(entries[0].Entry, entries[0].Context)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	line := buf.String()

	ciRegex := regexp.MustCompile(`"served": [0-9]+, "shape_flags": [0-9]+, "not_exercised": [0-9]+`)
	if !ciRegex.MatchString(line) {
		t.Fatalf("verdict line does not match the CI regex.\n  encoded: %s\n  regex: %s\n"+
			"Field names and order are load-bearing — see java-linux.sh in "+
			".github/workflows/test_workflow_scripts/java/async_config_poll/",
			line, ciRegex)
	}
}

// drainReport must do snapshot + drain + cursor advance in ONE critical
// section: MatchRequestShape runs unlocked and takes e.mu only for its
// increment, so a snapshot-then-separate-drain drops increments that land in
// the gap AND advances the cursor past them, losing them permanently.
//
// This drives that window DETERMINISTICALLY via drainTestHook rather than by
// racing goroutines. Racing does not work: Decide holds e.mu throughout unless
// the lane is a POLL lane (AsyncLane.IsPoll needs a "Poll" type suffix), and
// even with one the gap is a few instructions — a purely concurrent version of
// this test scored ZERO detections under CI's `go test ./...`, which passes
// neither -race nor -count>1. A test that cannot fail is worse than none.
func TestDrainReportLosesNoIncrementInTheSnapshotWindow(t *testing.T) {
	e, logs, lg := observedEngine(t, &fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10}
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "V0")})

	e.Decide(context.Background(), lane, &models.Mock{}) // pass = 1

	// Land a second increment exactly inside the snapshot->advance window.
	// Under a split drain the snapshot says 1, the cursor advances to 2, and
	// that second serve is never reported by any line, ever.
	var once sync.Once
	drainTestHook = func() {
		// NO locking here: the hook runs with e.mu already held by drainReport,
		// and sync.Mutex is not reentrant — taking it would deadlock, which is
		// the same trap that keeps drainReport from delegating to Report().
		once.Do(func() { e.pass++ }) // as decideServe's increment would
	}
	t.Cleanup(func() { drainTestHook = nil })

	e.LogReport(lg)
	drainTestHook = nil
	e.LogReport(lg) // whatever the first flush missed must surface here

	entries := logs.FilterMessage("async egress verdict").All()
	if len(entries) == 0 {
		t.Fatal("no verdict emitted")
	}
	last := entries[len(entries)-1]
	if got := fieldInt(t, last, "served"); got != 2 {
		t.Fatalf("final cumulative served=%d, want 2: an increment landed between the "+
			"snapshot and the cursor advance and was discarded — the cursor moved past "+
			"it, so no later flush reports it either", got)
	}
}

// drainReport must do snapshot + drain + cursor advance in ONE critical
// section: MatchRequestShape runs unlocked and takes e.mu only for its
// increment, so a snapshot-then-separate-drain drops increments that land in
// the gap. Run under -race.
