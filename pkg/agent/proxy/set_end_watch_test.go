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

// observedSetEndManager is setEndManager whose warnings and errors the test
// reads.
func observedSetEndManager(t *testing.T) (*MockManager, *observer.ObservedLogs) {
	t.Helper()
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

// handTicker makes the set-end filing watches' tickers
// (newSetEndFilingTicker) while a test runs: it keeps the beat each was
// made with, in the order made, and the test ticks each by hand. A tick is
// taken only by a watch that waits for one. A ticker not made by
// time.NewTicker sends only what is sent on its channel, and its Stop does
// nothing.
type handTicker struct {
	mu    sync.Mutex
	beats []time.Duration
	ticks []chan time.Time
}

// handTicked has the test's set-end filing watches ticked by hand.
func handTicked(t *testing.T) *handTicker {
	t.Helper()
	h := &handTicker{}
	was := newSetEndFilingTicker
	newSetEndFilingTicker = func(beat time.Duration) *time.Ticker {
		h.mu.Lock()
		defer h.mu.Unlock()
		c := make(chan time.Time)
		h.beats = append(h.beats, beat)
		h.ticks = append(h.ticks, c)
		return &time.Ticker{C: c}
	}
	t.Cleanup(func() { newSetEndFilingTicker = was })
	return h
}

// made is the beat of each ticker made so far.
func (h *handTicker) made() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.beats)
}

// tick ticks the i-th ticker made, and says whether its watch took the
// tick within the bound.
func (h *handTicker) tick(i int, within time.Duration) bool {
	h.mu.Lock()
	c := h.ticks[i]
	h.mu.Unlock()
	select {
	case c <- time.Now():
		return true
	case <-time.After(within):
		return false
	}
}

// A registration that has not returned is named, at each beat it runs
// through, by its parser's kind and the count of beats: a parser that
// ignores the read's context holds the read with a signal, and the warning
// says whether the context is done by then. Each watch's ticker is made
// with the beat and is ticked here by hand: one warning comes at each tick,
// none without one. The read waits as it did, and a watch takes no tick
// once its registration has returned, so no warning follows that.
func TestMockManager_SetEndFilingThatHasNotReturnedIsNamedAtEachBeat(t *testing.T) {
	const beat = 7 * time.Second
	was := setEndFilingBeat
	setEndFilingBeat = beat
	t.Cleanup(func() { setEndFilingBeat = was })
	m, logs := observedSetEndManager(t)
	ticker := handTicked(t)
	release := make(chan struct{})
	mustRegister(t, m, func(context.Context) {})
	mustRegisterKind(t, m, models.REDIS, func(context.Context) { <-release })
	read, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()
	filed := make(chan error, 1)
	go func() { filed <- m.FileAtSetEnd(read) }()

	const kafka, redis = 0, 1
	waitForSetEnd(t, "the watch of the registration that has not returned", func() bool { return len(ticker.made()) == 2 })
	if made := ticker.made(); !slices.Equal(made, []time.Duration{beat, beat}) {
		t.Fatalf("the watches' tickers were made with %v, want each with the beat, %s", made, beat)
	}
	if ticker.tick(kafka, 50*time.Millisecond) {
		t.Fatal("the watch of the Kafka registration, which returned at once, took a tick")
	}
	if got := len(filingWarnings(logs)); got != 0 {
		t.Fatalf("%d warnings before any tick, want none without one", got)
	}
	const beats = 3
	for n := 1; n <= beats; n++ {
		if n == beats {
			cancelRead()
		}
		if !ticker.tick(redis, 20*time.Second) {
			t.Fatalf("beat %d: the watch of the registration that has not returned took no tick", n)
		}
		waitForSetEnd(t, fmt.Sprintf("the warning of beat %d", n), func() bool { return len(filingWarnings(logs)) >= n })
		if got := len(filingWarnings(logs)); got != n {
			t.Fatalf("%d warnings after %d ticks, want one a tick", got, n)
		}
		select {
		case err := <-filed:
			t.Fatalf("the read returned (%v) while a registration it ran had not: the watch must change nothing of the read", err)
		default:
		}
	}
	close(release)
	select {
	case err := <-filed:
		if err == nil {
			t.Fatal("a read whose context ended while a registration ran was vouched for")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the read did not return once its registrations had")
	}
	if ticker.tick(redis, 50*time.Millisecond) {
		t.Fatal("the watch took a tick after its registration returned")
	}

	ws := filingWarnings(logs)
	if len(ws) != beats {
		t.Fatalf("%d warnings in all, want %d, one a tick: %v", len(ws), beats, ws)
	}
	for i, w := range ws {
		n := i + 1
		want := fmt.Sprintf("set-end filing for Redis has not returned after %s", time.Duration(n)*beat)
		if w.Message != want || w.Level != zapcore.WarnLevel {
			t.Fatalf("warning %d: %s %q, want WARN %q", n, w.Level, w.Message, want)
		}
		fields := w.ContextMap()
		if fields["kind"] != "Redis" || fields["beats"] != int64(n) || fields["beat"] != beat || fields["readContextDone"] != (n == beats) {
			t.Fatalf("warning %d: fields %v, want kind Redis, beats %d, beat %s, readContextDone %v", n, fields, n, beat, n == beats)
		}
	}
	if got := logs.FilterMessageSnippet("Kafka").Len() + logs.FilterField(zap.String("kind", string(models.KAFKA))).Len(); got != 0 {
		t.Fatalf("%d entries named the Kafka registration, which returned at once", got)
	}
}

// A registration that returns before its first beat, as one that files
// in-band does, is not warned of.
func TestMockManager_SetEndFilingThatReturnsIsNotWarnedOf(t *testing.T) {
	m, logs := observedSetEndManager(t)
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
