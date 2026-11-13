package relay

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// stacksFiles counts the stack files this package's tests had written
// (writeStacks): none of them writes one to the temp directory.
var stacksFiles atomic.Int32

func init() {
	writeStacks = func() (string, error) {
		stacksFiles.Add(1)
		return "/logs/keploy-stacks-1.txt", nil
	}
}

// freshStacks makes the process one that has written no stacks yet, and
// counts the files written from now on.
func freshStacks(t *testing.T) {
	t.Helper()
	stacksWritten.Store(false)
	stacksFiles.Store(0)
	t.Cleanup(func() { stacksWritten.Store(false) })
}

// stalledTee is a tee of one chunk's channel, on clk, logging to a fresh
// observer, whose consumer never goes away.
func stalledTee(t *testing.T, clk stall.Clock) (*tee, *dropRecorder, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	rec := &dropRecorder{}
	tt := newTee(fakeconn.FromDest, 1<<30, 1, testStallGrace, rec.record, zap.New(core))
	tt.clock = clk
	tt.start(make(chan struct{}))
	t.Cleanup(func() {
		tt.close()
		for range tt.readCh() {
		}
		tt.waitDone()
	})
	return tt, rec, logs
}

// waitStacks waits until n stack files have been written.
func waitStacks(t *testing.T, n int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for stacksFiles.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d stack files written; want %d", stacksFiles.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A parser that takes nothing from its full channel for a whole stall window,
// on a connection still open, has every goroutine's stack written to one file,
// said in one WARN that names it, and its connection is waited on as before:
// nothing is dropped.
func TestTee_AStalledParserWritesTheStacksOnceWhileItsConnectionIsOpen(t *testing.T) {
	freshStacks(t)
	tt, rec, logs := stalledTee(t, &stall.ManualClock{Step: stall.OnTime})
	for i := 0; i < 3; i++ {
		tt.push(mkChunk("x"))
	}
	waitStacks(t, 1)
	warns := logs.FilterMessage(StalledParserMsg).All()
	if len(warns) != 1 || warns[0].Level != zapcore.WarnLevel {
		t.Fatalf("%d stall WARNs; want one: %v", len(warns), warns)
	}
	f := warns[0].ContextMap()
	if f["stacks"] != "/logs/keploy-stacks-1.txt" || f["direction"] != fakeconn.FromDest.String() || f["chunksWaiting"] != int64(2) {
		t.Errorf("the WARN names stacks %v, direction %v, %v chunks waiting; want /logs/keploy-stacks-1.txt, %s, 2",
			f["stacks"], f["direction"], f["chunksWaiting"], fakeconn.FromDest)
	}
	if f["stalledFor"].(time.Duration) < testStallGrace {
		t.Errorf("the WARN says the parser stalled for %v; want a whole window, %v", f["stalledFor"], testStallGrace)
	}
	if _, ok := f["stacksError"]; ok {
		t.Errorf("the WARN carries an error for stacks that were written: %v", f["stacksError"])
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("an open connection's tee dropped %v; it waits on a parser that is behind", got)
	}
	// The parser takes its chunks again: every one is there.
	for i := 0; i < 3; i++ {
		select {
		case <-tt.readCh():
		case <-time.After(5 * time.Second):
			t.Fatalf("chunk %d never delivered after the stall", i)
		}
	}
	if n := stacksFiles.Load(); n != 1 {
		t.Errorf("%d stack files; want 1", n)
	}
}

// A parser that is slow, but takes a chunk within each stall window, is no
// stall: however long it takes in all, no stacks are written.
func TestTee_ASlowParserThatKeepsTakingWritesNoStacks(t *testing.T) {
	freshStacks(t)
	clk := &stall.ManualClock{}
	tt, rec, logs := stalledTee(t, clk)
	const chunks = 6
	for i := 0; i < chunks; i++ {
		tt.push(mkChunk("x"))
	}
	for i := 0; i < chunks-1; i++ {
		// The drain has taken the next chunk off the queue (so the look of
		// the one before is stopped) and waits on the full channel with a
		// look of its own.
		deadline := time.Now().Add(10 * time.Second)
		for {
			tt.mu.Lock()
			queued := len(tt.q)
			tt.mu.Unlock()
			if queued == chunks-2-i && clk.Pending() > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("chunk %d: the drain never waited on the full channel (%d queued)", i, queued)
			}
			time.Sleep(time.Millisecond)
		}
		clk.Advance(testStallGrace * 2 / 3)
		select {
		case <-tt.readCh():
		case <-time.After(5 * time.Second):
			t.Fatalf("chunk %d never delivered", i)
		}
	}
	if n := stacksFiles.Load(); n != 0 {
		t.Fatalf("%d stack files for a parser that took a chunk within every window; want none", n)
	}
	if n := logs.FilterMessage(StalledParserMsg).Len(); n != 0 {
		t.Fatalf("%d stall WARNs for a slow parser; want none", n)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("dropped %v", got)
	}
}

// A second stall, on another connection, writes no second file and says
// nothing: one set of stacks a run.
func TestTee_ASecondStallWritesNoSecondFile(t *testing.T) {
	freshStacks(t)
	first, _, firstLogs := stalledTee(t, &stall.ManualClock{Step: stall.OnTime})
	first.push(mkChunk("a"))
	first.push(mkChunk("b"))
	waitStacks(t, 1)
	second, _, secondLogs := stalledTee(t, &stall.ManualClock{Step: stall.OnTime})
	second.push(mkChunk("a"))
	second.push(mkChunk("b"))
	// The second's look expires at once on its clock, and is gone once it has.
	deadline := time.Now().Add(10 * time.Second)
	for {
		second.mu.Lock()
		held := len(second.q) == 0 && len(second.out) == 1
		second.mu.Unlock()
		if held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second tee's drain never took its second chunk")
		}
		time.Sleep(time.Millisecond)
	}
	parserStalled(nil, fakeconn.FromClient, testStallGrace, 2) // and a third, with no logger
	time.Sleep(50 * time.Millisecond)
	if n := stacksFiles.Load(); n != 1 {
		t.Fatalf("%d stack files; want 1 for the run", n)
	}
	if n := firstLogs.FilterMessage(StalledParserMsg).Len() + secondLogs.FilterMessage(StalledParserMsg).Len(); n != 1 {
		t.Fatalf("%d stall WARNs; want 1 for the run", n)
	}
}

// A drain that first waits after its connection closed has no look of its own:
// the stall bound's verdict writes the stacks as it abandons the chunks, so
// the stall that costs the mocks is written down, and measured once.
func TestTee_AStallFirstSeenAtTheCloseWritesTheStacksAsItAbandons(t *testing.T) {
	freshStacks(t)
	core, logs := observer.New(zapcore.InfoLevel)
	clk := &stall.ManualClock{Step: func(d time.Duration) time.Duration { return d / 10 }}
	rec := &dropRecorder{}
	tt := newTee(fakeconn.FromClient, 1<<30, 1, testStallGrace, rec.record, zap.New(core))
	tt.clock = clk
	for i := 0; i < 3; i++ {
		tt.push(mkChunk("x"))
	}
	tt.close()
	tt.start(make(chan struct{}))
	tt.waitDone()
	if n := rec.count(DropConsumerGone); n != 1 {
		t.Fatalf("consumer_gone reported %d times, want 1", n)
	}
	if n := stacksFiles.Load(); n != 1 {
		t.Fatalf("%d stack files; want 1, written as the tee gave up", n)
	}
	if n := logs.FilterMessage(StalledParserMsg).Len(); n != 1 {
		t.Fatalf("%d stall WARNs; want 1", n)
	}
	check := stall.CheckEvery(testStallGrace)
	if got, want := clk.Waits, int(testStallGrace/(check/10)); got != want {
		t.Fatalf("the stall took %d looks, want %d: one window, measured once", got, want)
	}
}

// Stacks that could not be written are said with why, naming no file.
func TestTee_StacksThatCouldNotBeWrittenAreSaidWithWhy(t *testing.T) {
	freshStacks(t)
	prev := writeStacks
	writeStacks = func() (string, error) { return "", errors.New("no room") }
	t.Cleanup(func() { writeStacks = prev })
	core, logs := observer.New(zapcore.InfoLevel)
	parserStalled(zap.New(core), fakeconn.FromClient, testStallGrace, 2)
	warns := logs.FilterMessage(StalledParserMsg).All()
	if len(warns) != 1 {
		t.Fatalf("%d stall WARNs; want 1", len(warns))
	}
	f := warns[0].ContextMap()
	if _, ok := f["stacks"]; ok {
		t.Errorf("the WARN names a stacks file none was written to: %v", f["stacks"])
	}
	if f["stacksError"] != "no room" {
		t.Errorf("the WARN says %v of the failed write; want its error", f["stacksError"])
	}
}

// A tee with no logger still has the stacks written, and does not fall over.
func TestTee_AStallWithNoLoggerWritesTheStacks(t *testing.T) {
	freshStacks(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a stall with no logger panicked: %v", r)
		}
	}()
	parserStalled(nil, fakeconn.FromClient, testStallGrace, 2)
	if n := stacksFiles.Load(); n != 1 {
		t.Fatalf("%d stack files; want 1", n)
	}
}
