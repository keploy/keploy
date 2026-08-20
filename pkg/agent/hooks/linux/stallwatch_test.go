//go:build linux

package linux

import (
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func newObservedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	return zap.New(core), logs
}

// A call that returns promptly must produce no output at all. The agent loads
// eBPF on every start, so a watchdog that chattered on the happy path would be
// pure noise and would train readers to ignore it.
func TestWatchStallSilentWhenCallReturnsQuickly(t *testing.T) {
	logger, logs := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", time.Hour, time.Hour)
	// Give a watchdog that reports too eagerly real time to do so before
	// stopping it. Stopping immediately would let a broken implementation win
	// the race against its own first report and pass this test.
	time.Sleep(50 * time.Millisecond)
	stop()
	if n := logs.Len(); n != 0 {
		t.Fatalf("watchdog logged %d entries for a call that returned immediately: %v", n, logs.All())
	}
}

// The point of the watchdog: a call that does not return must say so, name
// itself, and carry the kernel condition that explains it. Before this, the
// agent blocked before its first log line and the container looked merely
// "not ready" for five minutes.
func TestWatchStallReportsBlockedCall(t *testing.T) {
	logger, logs := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD) via ebpf.CollectionSpec.LoadAndAssign", 10*time.Millisecond, time.Hour)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for logs.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if logs.Len() == 0 {
		t.Fatal("watchdog stayed silent while the call was still blocked")
	}

	e := logs.All()[0]
	if e.Level != zap.ErrorLevel {
		t.Errorf("a permanently blocked agent should report at ERROR; got %v", e.Level)
	}
	fields := e.ContextMap()
	op, _ := fields["operation"].(string)
	if !strings.Contains(op, "BPF_PROG_LOAD") {
		t.Errorf("report must name the blocked operation; got %q", op)
	}
	// Without these two the reader still has to go and take a goroutine dump
	// off the CI host, which is the situation this replaces.
	if cause, _ := fields["likelyCause"].(string); !strings.Contains(cause, "tasks_rcu_exit_srcu_stall") {
		t.Errorf("report must name the kernel symptom to grep for; got %q", cause)
	}
	if rem, _ := fields["remediation"].(string); !strings.Contains(rem, "wsl --shutdown") {
		t.Errorf("report must name the remediation; got %q", rem)
	}
	if _, ok := fields["blockedFor"]; !ok {
		t.Error("report must say how long the call has been blocked")
	}
}

// It must keep reporting: a single line scrolls away, and the operator who
// looks at a wedged container ten minutes later needs to see it is still stuck.
func TestWatchStallRepeatsWhileBlocked(t *testing.T) {
	logger, logs := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", 10*time.Millisecond, 10*time.Millisecond)
	defer stop()

	deadline := time.Now().Add(3 * time.Second)
	for logs.Len() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if logs.Len() < 3 {
		t.Fatalf("watchdog reported %d times; a still-blocked call must keep reporting", logs.Len())
	}
}

// stop() must be synchronous: if it returned while the goroutine was mid-report,
// a successful load could still emit a scary ERROR after the fact.
func TestWatchStallStopIsRaceFree(t *testing.T) {
	for i := 0; i < 50; i++ {
		logger, logs := newObservedLogger()
		stop := watchStall(logger, "op", time.Microsecond, time.Microsecond)
		time.Sleep(time.Duration(i%5) * time.Millisecond)
		stop()
		before := logs.Len()
		time.Sleep(2 * time.Millisecond)
		if after := logs.Len(); after != before {
			t.Fatalf("watchdog logged %d more entries after stop() returned", after-before)
		}
	}
}

// stop() must tolerate being called more than once. The intended usage is
// `defer stop()`, which pairs naturally with an explicit stop on some paths, and
// a helper whose whole job is diagnosing a wedged startup must not be the thing
// that panics the process with "close of closed channel".
func TestWatchStallStopIsIdempotent(t *testing.T) {
	logger, _ := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", time.Hour, time.Hour)
	stop()
	stop() // would panic on a second close(done)
	stop()
}

// Concurrent stops must be safe too, and every one of them must observe the
// watchdog actually stopped rather than returning early.
func TestWatchStallStopIsConcurrencySafe(t *testing.T) {
	logger, _ := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", time.Hour, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); stop() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent stop() calls deadlocked")
	}
}
