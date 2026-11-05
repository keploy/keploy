package proxy

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// watchedSetEndManager is setEndManager whose log the test reads, with a
// set-end filing beat of beat.
func watchedSetEndManager(t *testing.T, beat time.Duration) (*MockManager, *observer.ObservedLogs) {
	t.Helper()
	was := setEndFilingBeat
	setEndFilingBeat = beat
	t.Cleanup(func() { setEndFilingBeat = was })
	core, logs := observer.New(zapcore.WarnLevel)
	m := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.New(core))
	t.Cleanup(m.Close)
	m.SetReadsSetEnds(true)
	return m, logs
}

// filingWarnings are the warnings that a set-end filing has not returned.
func filingWarnings(logs *observer.ObservedLogs) []observer.LoggedEntry {
	return logs.FilterFieldKey("beats").AllUntimed()
}

// waitForSetEnd waits, up to a bound no healthy run nears, for cond.
func waitForSetEnd(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A registration that has not returned is named, at each beat it runs
// through, by its parser's kind and the count of beats: a parser that
// ignores the read's context holds the read with a signal, and the warning
// says the context is done by then. The read waits as it did, and no
// warning follows the registration's return.
func TestMockManager_SetEndFilingThatHasNotReturnedIsNamedAtEachBeat(t *testing.T) {
	const beat = 20 * time.Millisecond
	m, logs := watchedSetEndManager(t, beat)
	release := make(chan struct{})
	mustRegister(t, m, func(context.Context) {})
	mustRegisterKind(t, m, models.REDIS, func(context.Context) { <-release })
	read, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()
	filed := make(chan error, 1)
	began := time.Now()
	go func() { filed <- m.FileAtSetEnd(read) }()

	waitForSetEnd(t, "two beats of a registration that has not returned", func() bool { return len(filingWarnings(logs)) >= 2 })
	select {
	case err := <-filed:
		t.Fatalf("the read returned (%v) while a registration it ran had not: the watch must change nothing of the read", err)
	default:
	}
	cancelRead()
	waitForSetEnd(t, "a beat after the read's context is done", func() bool {
		ws := filingWarnings(logs)
		return ws[len(ws)-1].ContextMap()["readContextDone"] == true
	})
	close(release)
	var err error
	select {
	case err = <-filed:
	case <-time.After(20 * time.Second):
		t.Fatal("the read did not return once its registrations had")
	}
	took := time.Since(began)
	if err == nil {
		t.Fatal("a read whose context ended while a registration ran was vouched for")
	}
	ws := filingWarnings(logs)
	if most := int(took / beat); len(ws) > most {
		t.Fatalf("%d warnings in a read of %s, at most one a beat (%s) is %d", len(ws), took, beat, most)
	}
	time.Sleep(5 * beat)
	if after := filingWarnings(logs); len(after) != len(ws) {
		t.Fatalf("%d warnings followed the registration's return: %v", len(after)-len(ws), after[len(ws):])
	}

	sawLive, sawDone := false, false
	for i, w := range ws {
		beats := i + 1
		want := fmt.Sprintf("set-end filing for Redis has not returned after %s", time.Duration(beats)*beat)
		if w.Message != want || w.Level != zapcore.WarnLevel {
			t.Fatalf("warning %d: %s %q, want WARN %q", beats, w.Level, w.Message, want)
		}
		fields := w.ContextMap()
		if fields["kind"] != "Redis" || fields["beats"] != int64(beats) || fields["beat"] != beat {
			t.Fatalf("warning %d: fields %v, want kind Redis, beats %d, beat %s", beats, fields, beats, beat)
		}
		switch fields["readContextDone"] {
		case false:
			if sawDone {
				t.Fatalf("warning %d says the read's context is live after one said it was done", beats)
			}
			sawLive = true
		case true:
			sawDone = true
		default:
			t.Fatalf("warning %d: readContextDone is %v, want a bool", beats, fields["readContextDone"])
		}
	}
	if !sawLive {
		t.Fatal("no warning said the read's context was live while it was")
	}
	if got := logs.FilterMessageSnippet("Kafka").Len(); got != 0 {
		t.Fatalf("%d warnings named the Kafka registration, which returned at once", got)
	}
}

// A registration that returns within its first beat, as one that files
// in-band does, is never warned of.
func TestMockManager_SetEndFilingThatReturnsIsNotWarnedOf(t *testing.T) {
	m, logs := watchedSetEndManager(t, setEndFilingBeat)
	n := 0
	mustRegister(t, m, func(context.Context) { n++ })
	if err := m.FileAtSetEnd(context.Background()); err != nil || n != 1 {
		t.Fatalf("the registration ran %d times (%v), want once", n, err)
	}
	if got := logs.Len(); got != 0 {
		t.Fatalf("a registration that returned at once was warned of: %v", logs.AllUntimed())
	}
}

// heldWarnCore is an observer core whose writes of a set-end filing warning
// (one with a beats field) wait for hold to close; the first to start closes
// writing. Others are written at once.
type heldWarnCore struct {
	zapcore.Core
	writing, hold chan struct{}
	started       sync.Once
}

func (c *heldWarnCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *heldWarnCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	if slices.ContainsFunc(fields, func(f zapcore.Field) bool { return f.Key == "beats" }) {
		c.started.Do(func() { close(c.writing) })
		<-c.hold
	}
	return c.Core.Write(e, fields)
}

// A read returns only once the watch of each registration it ran has
// ended: while a warning of one is still being written, the read that ran
// it waits, so no warning follows its return.
func TestMockManager_SetEndFilingWatchEndsBeforeTheReadReturns(t *testing.T) {
	was := setEndFilingBeat
	setEndFilingBeat = time.Millisecond
	t.Cleanup(func() { setEndFilingBeat = was })
	obs, logs := observer.New(zapcore.WarnLevel)
	core := &heldWarnCore{Core: obs, writing: make(chan struct{}), hold: make(chan struct{})}
	var release sync.Once
	releaseWrites := func() { release.Do(func() { close(core.hold) }) }
	t.Cleanup(releaseWrites)
	m := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.New(core))
	t.Cleanup(m.Close)
	m.SetReadsSetEnds(true)
	mustRegisterKind(t, m, models.REDIS, func(context.Context) { <-core.writing })

	filed := make(chan error, 1)
	go func() { filed <- m.FileAtSetEnd(context.Background()) }()
	select {
	case err := <-filed:
		t.Fatalf("the read returned (%v) while a warning of a registration it ran was still being written", err)
	case <-time.After(500 * time.Millisecond):
	}
	releaseWrites()
	select {
	case err := <-filed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the read did not return once the warning was written")
	}
	n := len(filingWarnings(logs))
	if n == 0 {
		t.Fatal("the warning held while the registration returned was not written")
	}
	time.Sleep(20 * setEndFilingBeat)
	if after := len(filingWarnings(logs)); after != n {
		t.Fatalf("%d warnings followed the read's return", after-n)
	}
}
