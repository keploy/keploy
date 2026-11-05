package proxy

import (
	"context"
	"fmt"
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
	if err == nil {
		t.Fatal("a read whose context ended while a registration ran was vouched for")
	}
	ws := filingWarnings(logs)
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
