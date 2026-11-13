package relay

import (
	"errors"
	"fmt"
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

// stalledTee is a tee of a channel of buf chunks, on clk, logging to a fresh
// observer, whose consumer never goes away, and its reader's count of the
// bytes it took (tee.consumed): take, or a read off the channel and a count
// added to took, as a FakeConn counts what its reader took.
func stalledTee(t *testing.T, clk stall.Clock, buf int) (*tee, *atomic.Int64, *dropRecorder, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	rec := &dropRecorder{}
	tt := newTee(fakeconn.FromDest, 1<<30, buf, testStallGrace, rec.record, zap.New(core))
	tt.clock = clk
	took := &atomic.Int64{}
	tt.consumed = took.Load
	tt.start(make(chan struct{}))
	t.Cleanup(func() {
		tt.close()
		for range tt.readCh() {
		}
		tt.waitDone()
	})
	return tt, took, rec, logs
}

// take reads the next chunk off tt's channel and counts it taken.
func take(t *testing.T, tt *tee, took *atomic.Int64) {
	t.Helper()
	select {
	case c := <-tt.readCh():
		took.Add(int64(len(c.Bytes)))
	case <-time.After(5 * time.Second):
		t.Fatal("no chunk delivered")
	}
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

// waitDelivered waits until tt's drain has handed every chunk queued on to its
// channel, n chunks in it.
func waitDelivered(t *testing.T, tt *tee, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		tt.mu.Lock()
		done := len(tt.q) == 0 && len(tt.out) == n
		tt.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the drain never handed on its %d chunks", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A parser that takes none of what it was handed for a whole stall window, on
// a connection still open, has every goroutine's stack written to one file,
// said in one WARN that names it, and its connection is waited on as before:
// nothing is dropped. However few bytes wait: its channel full, a few chunks
// in it, or only a chunk its FakeConn took off the channel and read none of.
func TestTee_AStalledParserWritesTheStacksOnceWhileItsConnectionIsOpen(t *testing.T) {
	for _, c := range []struct {
		name                        string
		buf, pushed, offChan, taken int
		queued, inChan              int
		chunks, bytes               int64
	}{
		{name: "channel full", buf: 1, pushed: 3, queued: 1, inChan: 1, chunks: 2, bytes: 2},
		{name: "a few chunks in the channel", buf: 16, pushed: 3, inChan: 3, chunks: 3, bytes: 3},
		{name: "a chunk taken off the channel, none of it read", buf: 16, pushed: 1, offChan: 1, chunks: 0, bytes: 1},
		{name: "after taking some", buf: 16, pushed: 3, taken: 1, inChan: 2, chunks: 2, bytes: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			freshStacks(t)
			clk := &stall.ManualClock{}
			tt, took, rec, logs := stalledTee(t, clk, c.buf)
			for i := 0; i < c.pushed; i++ {
				tt.push(mkChunk("x"))
			}
			for i := 0; i < c.offChan; i++ {
				<-tt.readCh() // its FakeConn took it, and its reader read none of it
			}
			for i := 0; i < c.taken; i++ {
				take(t, tt, took)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				tt.mu.Lock()
				at := len(tt.q) == c.queued && len(tt.out) == c.inChan
				tt.mu.Unlock()
				if at {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the drain never handed its chunks on")
				}
				time.Sleep(time.Millisecond)
			}
			// The process runs on, and the parser takes nothing.
			for stacksFiles.Load() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("no stacks written for a parser that took nothing")
				}
				clk.Advance(stall.CheckEvery(testStallGrace))
				time.Sleep(time.Millisecond)
			}
			warns := logs.FilterMessage(StalledParserMsg).All()
			if len(warns) != 1 || warns[0].Level != zapcore.WarnLevel {
				t.Fatalf("%d stall WARNs; want one: %v", len(warns), warns)
			}
			f := warns[0].ContextMap()
			if f["stacks"] != "/logs/keploy-stacks-1.txt" || f["direction"] != fakeconn.FromDest.String() ||
				f["chunksWaiting"] != c.chunks || f["bytesWaiting"] != c.bytes {
				t.Errorf("the WARN names stacks %v, direction %v, %v chunks and %v bytes waiting; want /logs/keploy-stacks-1.txt, %s, %d, %d",
					f["stacks"], f["direction"], f["chunksWaiting"], f["bytesWaiting"], fakeconn.FromDest, c.chunks, c.bytes)
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
			for i := c.offChan + c.taken; i < c.pushed; i++ {
				take(t, tt, took)
			}
			if n := stacksFiles.Load(); n != 1 {
				t.Errorf("%d stack files; want 1", n)
			}
		})
	}
}

// A parser that is slow, but takes some of its bytes within each stall
// window, is no stall: however long it takes in all, no stacks are written.
// So whether the drain waits on its full channel or on an empty queue.
func TestTee_ASlowParserThatKeepsTakingWritesNoStacks(t *testing.T) {
	for _, buf := range []int{1, 16} {
		t.Run(fmt.Sprint("channel of ", buf), func(t *testing.T) {
			freshStacks(t)
			clk := &stall.ManualClock{}
			tt, took, rec, logs := stalledTee(t, clk, buf)
			const chunks = 6
			for i := 0; i < chunks; i++ {
				tt.push(mkChunk("x"))
			}
			for i := 0; i < chunks; i++ {
				// The drain waits with a look armed, and has looked at the
				// last advance: two windows in all pass with each take.
				deadline := time.Now().Add(10 * time.Second)
				for clk.Pending() == 0 {
					if time.Now().After(deadline) {
						t.Fatalf("chunk %d: the drain never looked at its parser", i)
					}
					time.Sleep(time.Millisecond)
				}
				clk.Advance(testStallGrace * 2 / 3)
				time.Sleep(time.Millisecond)
				clk.Advance(testStallGrace * 2 / 3)
				take(t, tt, took)
			}
			// It has taken all it was handed: nothing waits, so no window
			// runs however long the process runs on.
			for i := 0; i < 30; i++ {
				clk.Advance(stall.CheckEvery(testStallGrace))
				time.Sleep(time.Millisecond)
			}
			if n := stacksFiles.Load(); n != 0 {
				t.Fatalf("%d stack files for a parser that took some within every window; want none", n)
			}
			if n := logs.FilterMessage(StalledParserMsg).Len(); n != 0 {
				t.Fatalf("%d stall WARNs for a slow parser; want none", n)
			}
			if got := rec.snapshot(); len(got) != 0 {
				t.Fatalf("dropped %v", got)
			}
		})
	}
}

// A second stall, on another connection, writes no second file and says
// nothing: one set of stacks a run. And a drain that waits once the run's
// stacks are written does not look at its parser at all.
func TestTee_ASecondStallWritesNoSecondFile(t *testing.T) {
	freshStacks(t)
	first, _, _, firstLogs := stalledTee(t, &stall.ManualClock{Step: stall.OnTime}, 1)
	first.push(mkChunk("a"))
	first.push(mkChunk("b"))
	waitStacks(t, 1)
	clk := &stall.ManualClock{Step: stall.OnTime}
	second, _, _, secondLogs := stalledTee(t, clk, 16)
	second.push(mkChunk("a"))
	second.push(mkChunk("b"))
	waitDelivered(t, second, 2)
	parserStalled(nil, fakeconn.FromClient, testStallGrace, 2, 2) // and a third, with no logger
	time.Sleep(50 * time.Millisecond)
	if n := stacksFiles.Load(); n != 1 {
		t.Fatalf("%d stack files; want 1 for the run", n)
	}
	if n := firstLogs.FilterMessage(StalledParserMsg).Len() + secondLogs.FilterMessage(StalledParserMsg).Len(); n != 1 {
		t.Fatalf("%d stall WARNs; want 1 for the run", n)
	}
	if clk.Waits != 0 {
		t.Fatalf("the second drain asked its clock for %d look(s) once the run's stacks were written; want none", clk.Waits)
	}
}

// A parser that closed its FakeConn has stopped reading, not stalled: the
// chunks handed it before are never looked at.
func TestTee_AParserThatClosedItsReaderIsNoStall(t *testing.T) {
	freshStacks(t)
	clk := &stall.ManualClock{Step: stall.OnTime}
	tt := newTee(fakeconn.FromClient, 1<<30, 16, testStallGrace, (&dropRecorder{}).record, zap.NewNop())
	tt.clock = clk
	took := &atomic.Int64{}
	tt.consumed = took.Load
	gone := make(chan struct{})
	close(gone)
	tt.start(gone)
	t.Cleanup(func() {
		tt.close()
		tt.waitDone()
	})
	for i := 0; i < 3; i++ {
		tt.push(mkChunk("x"))
	}
	waitDelivered(t, tt, 3)
	time.Sleep(50 * time.Millisecond)
	if n, w := stacksFiles.Load(), clk.Waits; n != 0 || w != 0 {
		t.Fatalf("%d stack file(s), %d look(s) for a parser that closed its reader; want none", n, w)
	}
}

// A drain that ends with a look armed leaves no timer of it behind.
func TestTee_ADrainThatEndsLeavesNoLookBehind(t *testing.T) {
	freshStacks(t)
	clk := &stall.ManualClock{}
	tt := newTee(fakeconn.FromClient, 1<<30, 16, testStallGrace, (&dropRecorder{}).record, zap.NewNop())
	tt.clock = clk
	took := &atomic.Int64{}
	tt.consumed = took.Load
	tt.start(make(chan struct{}))
	tt.push(mkChunk("x"))
	deadline := time.Now().Add(10 * time.Second)
	for clk.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the drain never looked at its parser")
		}
		time.Sleep(time.Millisecond)
	}
	tt.close()
	tt.waitDone()
	if n := clk.Pending(); n != 0 {
		t.Fatalf("%d look timer(s) left behind by a drain that ended; want none", n)
	}
	for range tt.readCh() {
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
	took := &atomic.Int64{}
	tt.consumed = took.Load
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
	warns := logs.FilterMessage(StalledParserMsg).All()
	if len(warns) != 1 {
		t.Fatalf("%d stall WARNs; want 1", len(warns))
	}
	if f := warns[0].ContextMap(); f["chunksWaiting"] != int64(2) || f["bytesWaiting"] != int64(2) {
		t.Errorf("the WARN says %v chunks and %v bytes waiting; want 2 and 2", f["chunksWaiting"], f["bytesWaiting"])
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
	parserStalled(zap.New(core), fakeconn.FromClient, testStallGrace, 2, 2)
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
	parserStalled(nil, fakeconn.FromClient, testStallGrace, 2, 2)
	if n := stacksFiles.Load(); n != 1 {
		t.Fatalf("%d stack files; want 1", n)
	}
}
