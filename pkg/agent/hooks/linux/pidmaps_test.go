//go:build linux

package linux

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
)

// fakeCounts stands in for pid_map_insert_failures: the test sets what the
// next read returns.
type fakeCounts struct {
	mu    sync.Mutex
	c     pidMapCounts
	err   error
	reads int
}

func (f *fakeCounts) set(c pidMapCounts, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.c, f.err = c, err
}

func (f *fakeCounts) read() (pidMapCounts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.c, f.err
}

// readsPast waits until the watcher has read at least n more times than it
// had when called, so that what it logs about the current counts is in. It
// waits on the reads, however long a loaded machine takes to make them: the
// test binary's -timeout is the only bound, for a watcher that never reads.
func (f *fakeCounts) readsPast(t *testing.T, n int) {
	t.Helper()
	f.mu.Lock()
	want := f.reads + n
	f.mu.Unlock()
	for {
		f.mu.Lock()
		got := f.reads
		f.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func warnsFor(logs *observer.ObservedLogs, m string) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		if e.ContextMap()["map"] == m {
			out = append(out, e)
		}
	}
	return out
}

// The first refused insert into each map is logged at WARN, once, naming the
// map, how many it refused, the latest TGID and why; a map that refuses more
// later is not logged again, and no refusal is not logged at all.
func TestWatchPIDMapRefusalsWarnsAtTheFirstRefusalOfEachMapOnce(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	counts := &fakeCounts{c: pidMapCounts{targetCap: 10240, excludedCap: 10240}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchPIDMapRefusals(ctx, zap.New(core), counts.read, time.Millisecond)
	}()
	defer func() { cancel(); <-done }()

	counts.readsPast(t, 3)
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Fatalf("%d WARN lines with nothing refused, want none: %v", n, logs.All())
	}

	refused := pidMapInsertFailure{Failures: 1, LastTGID: 4242, LastErr: -int32(unix.E2BIG)}
	counts.set(pidMapCounts{target: refused, targetCap: 10240, excludedCap: 10240}, nil)
	counts.readsPast(t, 3)
	warns := warnsFor(logs, "target_namespace_pids")
	if len(warns) != 1 {
		t.Fatalf("%d WARN lines for target_namespace_pids after its first refusal, want 1: %v", len(warns), logs.All())
	}
	fields := warns[0].ContextMap()
	if fields["refused_inserts"] != uint64(1) || fields["tgid"] != uint32(4242) || fields["error"] != "E2BIG" || fields["capacity"] != uint32(10240) {
		t.Fatalf("the WARN says %v; want refused_inserts 1, tgid 4242, error E2BIG, capacity 10240", fields)
	}
	if fields["next_step"] != models.PIDMapRefusalRemedy(refused.LastErr) {
		t.Fatalf("the WARN's next_step is %q, want the remedy for its error (%q)", fields["next_step"], models.PIDMapRefusalRemedy(refused.LastErr))
	}

	// More refusals, of both maps.
	more := pidMapInsertFailure{Failures: 50, LastTGID: 4343, LastErr: -int32(unix.E2BIG)}
	excl := pidMapInsertFailure{Failures: 2, LastTGID: 77, LastErr: -int32(unix.EBUSY)}
	counts.set(pidMapCounts{target: more, excluded: excl, targetCap: 10240, excludedCap: 10240}, nil)
	counts.readsPast(t, 3)
	if n := len(warnsFor(logs, "target_namespace_pids")); n != 1 {
		t.Fatalf("%d WARN lines for target_namespace_pids after more refusals, want still 1", n)
	}
	exclWarns := warnsFor(logs, "excluded_pids")
	if n := len(exclWarns); n != 1 {
		t.Fatalf("%d WARN lines for excluded_pids after its first refusal, want 1: %v", n, logs.All())
	}
	// Its cause is the bucket lock, not a full map: its own next step.
	if ef := exclWarns[0].ContextMap(); ef["error"] != "EBUSY" || ef["next_step"] != models.PIDMapRefusalRemedy(-int32(unix.EBUSY)) || ef["next_step"] == fields["next_step"] {
		t.Fatalf("excluded_pids' WARN says %v; want error EBUSY and the lock's next step, not the full map's", ef)
	}
}

// A refusal in the last interval, before the agent stops, is still warned:
// the watcher reads once more as its context ends. A test command whose last
// call is the refused one ends within a second of it, and the agent with it.
func TestWatchPIDMapRefusalsWarnsARefusalItOnlySeesAsItStops(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	counts := &fakeCounts{c: pidMapCounts{target: pidMapInsertFailure{Failures: 1, LastTGID: 9, LastErr: -int32(unix.E2BIG)}, targetCap: 10240}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchPIDMapRefusals(ctx, zap.New(core), counts.read, time.Hour) // no tick before the stop
	}()
	cancel()
	<-done
	if n := len(warnsFor(logs, "target_namespace_pids")); n != 1 {
		t.Fatalf("%d WARN lines for a refusal read as the watcher stopped, want 1", n)
	}
}

// A read that fails is not a refusal, and the next one that succeeds is still
// reported.
func TestWatchPIDMapRefusalsKeepsReadingAfterAFailedRead(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	counts := &fakeCounts{err: errors.New("bad file descriptor")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchPIDMapRefusals(ctx, zap.New(core), counts.read, time.Millisecond)
	}()
	defer func() { cancel(); <-done }()

	counts.readsPast(t, 3)
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Fatalf("%d WARN lines for failed reads, want none (they are Debug)", n)
	}
	counts.set(pidMapCounts{target: pidMapInsertFailure{Failures: 3, LastErr: -int32(unix.E2BIG)}}, nil)
	counts.readsPast(t, 3)
	if n := len(warnsFor(logs, "target_namespace_pids")); n != 1 {
		t.Fatalf("%d WARN lines once a read succeeded with a refusal, want 1", n)
	}
}

// What the agent answers over GET /agent/pidmaps: both counts, the size of
// each map as it was loaded, the latest refused TGID only when
// target_namespace_pids refused one, and each map's latest errno only when it
// refused one (so a report names the cause, not "full" for a lock failure).
func TestPIDMapCountsRefusals(t *testing.T) {
	c := pidMapCounts{
		target:    pidMapInsertFailure{Failures: 3, LastTGID: 99, LastErr: -int32(unix.E2BIG)},
		excluded:  pidMapInsertFailure{Failures: 1, LastTGID: 7, LastErr: -int32(unix.ETIMEDOUT)},
		targetCap: 10240, excludedCap: 64,
	}
	want := models.PIDMapRefusals{Target: 3, Excluded: 1, LastTargetTGID: 99, TargetCapacity: 10240, ExcludedCapacity: 64,
		LastTargetErr: -int32(unix.E2BIG), LastExcludedErr: -int32(unix.ETIMEDOUT)}
	if got := c.refusals(); got != want {
		t.Fatalf("refusals() = %+v, want %+v", got, want)
	}
	// A map that refused nothing gives no TGID and no errno, whatever its
	// slot still holds.
	c.target = pidMapInsertFailure{LastTGID: 5, LastErr: -int32(unix.E2BIG)}
	if got := c.refusals(); got.LastTargetTGID != 0 || got.LastTargetErr != 0 || got.Target != 0 || got.Excluded != 1 || got.LastExcludedErr != -int32(unix.ETIMEDOUT) {
		t.Fatalf("refusals() = %+v, want only Excluded 1 with its errno", got)
	}
	c.excluded = pidMapInsertFailure{LastTGID: 7, LastErr: -int32(unix.E2BIG)}
	if got := c.refusals(); got.LastExcludedErr != 0 || got.Excluded != 0 {
		t.Fatalf("refusals() = %+v, want no excluded errno without an excluded refusal", got)
	}
}
