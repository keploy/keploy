//go:build linux

package linux

import (
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg"
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
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", time.Hour, time.Hour, stallErrorAfter)
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
	// errorAfter=0 makes the very first tick terminal, which is the state this
	// test is about: a call that is never coming back.
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD) via ebpf.CollectionSpec.LoadAndAssign", 10*time.Millisecond, time.Hour, 0)
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
	if rem, _ := fields["remediation"].(string); rem == "" {
		t.Error("report must name a remediation")
	}
	if _, ok := fields["blockedFor"]; !ok {
		t.Error("report must say how long the call has been blocked")
	}
}

// A call that is merely SLOW must not be reported at ERROR. ERROR is a terminal
// signal here: the DaemonSet e2e fails a run on any agent ERROR and the sample-app
// CI lanes gate on `grep ERROR` over the agent log, so a load that blocks past the first
// tick and then succeeds would fail an otherwise perfectly healthy run — which
// is exactly what happened before this split (BPF load returned at ~16s, all 34
// tests passed, the lane still exited 1).
func TestWatchStallReportsSlowCallAtWarnNotError(t *testing.T) {
	logger, logs := newObservedLogger()
	// The call has been blocked for ~10ms, far below errorAfter, so it may still
	// return: WARN, not ERROR.
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", 10*time.Millisecond, time.Hour, time.Hour)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for logs.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if logs.Len() == 0 {
		t.Fatal("watchdog stayed silent while the call was still blocked")
	}

	e := logs.All()[0]
	if e.Level != zap.WarnLevel {
		t.Errorf("a still-running call must report at WARN, not %v — ERROR is reserved for a call that is never coming back", e.Level)
	}
	// The diagnostic value must survive the severity change.
	fields := e.ContextMap()
	if op, _ := fields["operation"].(string); !strings.Contains(op, "BPF_PROG_LOAD") {
		t.Errorf("the WARN report must still name the blocked operation; got %q", op)
	}
	if _, ok := fields["blockedFor"]; !ok {
		t.Error("the WARN report must still say how long the call has been blocked")
	}
}

// The escalation itself — a report that starts at WARN and becomes ERROR once the
// call has been blocked past errorAfter. Testing only the two endpoints
// (errorAfter=0 and errorAfter=huge) would pass against an implementation that
// derived the severity from "errorAfter == 0" and never crossed anything.
func TestWatchStallEscalatesFromWarnToError(t *testing.T) {
	logger, logs := newObservedLogger()
	// blockedFor is rounded to whole seconds before the comparison, so a 1s
	// threshold crosses once ~500ms has elapsed. Ticking every 10ms gives plenty
	// of reports on both sides of that line.
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", 10*time.Millisecond, 10*time.Millisecond, time.Second)
	defer stop()

	deadline := time.Now().Add(5 * time.Second)
	sawWarn, sawError := false, false
	for time.Now().Before(deadline) && !sawError {
		for _, e := range logs.All() {
			switch e.Level {
			case zap.WarnLevel:
				sawWarn = true
			case zap.ErrorLevel:
				sawError = true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !sawWarn {
		t.Error("the early reports, while the call could still return, must be WARN")
	}
	if !sawError {
		t.Error("a call still blocked past errorAfter must escalate to ERROR")
	}
	// Order matters: WARN must come first, or the escalation is not an escalation.
	all := logs.All()
	if len(all) > 0 && all[0].Level != zap.WarnLevel {
		t.Errorf("the FIRST report must be WARN; got %v", all[0].Level)
	}
}

// A genuinely wedged agent must still produce an ERROR before keploy gives up
// waiting for it, or the escalation is unreachable in practice and the ERROR
// signal is lost. Reports tick at first, then every repeat, so the first tick at
// or past stallErrorAfter is what has to land inside the ready budget.
func TestStallErrorLandsBeforeTheAgentReadyBudget(t *testing.T) {
	firstErrorAt := stallFirstReport
	for firstErrorAt < stallErrorAfter {
		firstErrorAt += stallRepeatReport
	}
	if firstErrorAt >= pkg.DefaultAgentReadyTimeout {
		t.Fatalf("the first ERROR lands at %v, at or past the %v agent-ready budget: a wedged agent would be given up on before it ever reported ERROR",
			firstErrorAt, pkg.DefaultAgentReadyTimeout)
	}
}

// The WSL match is the one thing in the remediation split that can actually be
// wrong, so it is tested directly rather than stubbed away.
func TestIsWSLRelease(t *testing.T) {
	cases := []struct {
		release string
		want    bool
	}{
		{"5.15.153.1-microsoft-standard-WSL2", true},
		{"4.4.0-19041-Microsoft", true}, // WSL1 capitalises it
		{"6.12.63+deb13-amd64", false},
		{"5.10.0-28-cloud-amd64", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isWSLRelease(tc.release); got != tc.want {
			t.Errorf("isWSLRelease(%q) = %v, want %v", tc.release, got, tc.want)
		}
	}
}

// The remediation must match the kernel actually running: "wsl --shutdown" does
// not exist on a Linux CI runner or a production node, and advice that cannot
// apply is worse than none on a line that already reads as an emergency.
func TestStallRemediationMatchesPlatform(t *testing.T) {
	rem := stallRemediation()
	if runningUnderWSL() {
		if !strings.Contains(rem, "wsl --shutdown") {
			t.Errorf("under WSL the remediation must name the WSL fix; got %q", rem)
		}
		return
	}
	if strings.Contains(rem, "wsl --shutdown") {
		t.Errorf("off WSL the remediation must not send the operator to a command that does not exist; got %q", rem)
	}
	if !strings.Contains(rem, "tasks_rcu_exit_srcu_stall") {
		t.Errorf("the non-WSL remediation must still name what to grep the host kernel log for; got %q", rem)
	}
}

// It must keep reporting: a single line scrolls away, and the operator who
// looks at a wedged container ten minutes later needs to see it is still stuck.
func TestWatchStallRepeatsWhileBlocked(t *testing.T) {
	logger, logs := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", 10*time.Millisecond, 10*time.Millisecond, 0)
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
		stop := watchStall(logger, "op", time.Microsecond, time.Microsecond, 0)
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
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", time.Hour, time.Hour, stallErrorAfter)
	stop()
	stop() // would panic on a second close(done)
	stop()
}

// Concurrent stops must be safe too, and every one of them must observe the
// watchdog actually stopped rather than returning early.
func TestWatchStallStopIsConcurrencySafe(t *testing.T) {
	logger, _ := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", time.Hour, time.Hour, stallErrorAfter)
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
