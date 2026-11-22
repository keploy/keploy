//go:build linux

package linux

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// neverError is an errorFrom no test reaches: every report a WARN.
const neverError = math.MaxInt

func newObservedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	return zap.New(core), logs
}

// A call that returns promptly must produce no output at all. The agent loads
// eBPF on every start, so a watchdog that chattered on the happy path would be
// pure noise and would train readers to ignore it.
func TestWatchStallSilentWhenCallReturnsQuickly(t *testing.T) {
	logger, logs := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", "", time.Hour, time.Hour, stallErrorFrom)
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
// itself, and say how long it has been blocked. Before this, the agent
// blocked before its first log line and the container looked merely "not
// ready" for five minutes.
func TestWatchStallReportsBlockedCall(t *testing.T) {
	logger, logs := newObservedLogger()
	// errorFrom=1 makes the very first report terminal, which is the state
	// this test is about: a call that is never coming back.
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD) via ebpf.CollectionSpec.LoadAndAssign", "", 10*time.Millisecond, time.Hour, 1)
	defer stop()

	firstReport(logs)
	e := logs.All()[0]
	if e.Level != zap.ErrorLevel {
		t.Errorf("a permanently blocked agent should report at ERROR; got %v", e.Level)
	}
	fields := e.ContextMap()
	op, _ := fields["operation"].(string)
	if !strings.Contains(op, "BPF_PROG_LOAD") {
		t.Errorf("report must name the blocked operation; got %q", op)
	}
	if _, ok := fields["blockedFor"]; !ok {
		t.Error("report must say how long the call has been blocked")
	}
}

// fakeTask writes a /proc/self/task/<tid> look-alike: the files watchStall
// reads, as the kernel writes them.
func fakeTask(t *testing.T, stat, wchan, stack, schedstat string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"stat": stat, "wchan": wchan, "stack": stack, "schedstat": schedstat} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// firstReport waits for watchStall's first report, however long the test
// process takes to get to run, and returns its fields. A watchdog that never
// reports is the test binary's timeout.
func firstReport(logs *observer.ObservedLogs) map[string]interface{} {
	for logs.Len() == 0 {
		time.Sleep(time.Millisecond)
	}
	return logs.All()[0].ContextMap()
}

// A report says what the kernel says of the thread making the call, and
// nothing it does not: the thread's state, its innermost kernel frames, and
// the CPU it ran since the call began. These are a 5.10 kernel's files for a
// verifier waiting on the tracing event_mutex.
// It names no cause and no remedy: a cause printed without reading it off
// the thread was a guess (a stalled RCU-tasks grace period, on kernels built
// without RCU-tasks), and its remedy (reboot the node) the wrong thing to do.
func TestWatchStallReportsWhatTheThreadIsDoing(t *testing.T) {
	stack := "[<0>] trace_set_clr_event+0x5c/0xe0\n" +
		"[<0>] bpf_get_trace_printk_proto+0xe/0x30\n" +
		"[<0>] check_helper_call+0x16a/0x2bd0\n" +
		"[<0>] do_check_common+0x1d5e/0x2d70\n" +
		"[<0>] bpf_check+0x1c1d/0x2770\n" +
		"[<0>] bpf_prog_load+0x3c4/0x8b0\n"
	// wchan is "0" here, as 5.10's x86-64 ORC unwinder gives it for most
	// sleeping threads: the stack says where the thread waits.
	dir := fakeTask(t, "868 (keploy (agent)) D 1 868 868 0 -1 4194560", "0\n", stack, "1000000 5000 12\n")
	logger, logs := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", dir, 10*time.Millisecond, time.Hour, neverError)
	defer stop()
	fields := firstReport(logs)
	if got := fields["threadState"]; got != "D" {
		t.Errorf("threadState = %v, want D (read past the parenthesised command name)", got)
	}
	if got, ok := fields["waitingIn"]; ok {
		t.Errorf("waitingIn = %v reported beside the stack, which says it better", got)
	}
	want := "trace_set_clr_event < bpf_get_trace_printk_proto < check_helper_call < do_check_common < bpf_check < bpf_prog_load"
	if got := fields["kernelStack"]; got != want {
		t.Errorf("kernelStack = %v, want %q", got, want)
	}
	if got, ok := fields["ranOnCPU"].(time.Duration); !ok || got != 0 {
		t.Errorf("ranOnCPU = %v, want 0: the thread has not run since the call began", fields["ranOnCPU"])
	}
	for _, guess := range []string{"likelyCause", "remediation"} {
		if v, ok := fields[guess]; ok {
			t.Errorf("report carries %s = %v: it must say only what it read off the thread", guess, v)
		}
	}
}

// Where the stack cannot be read (a process without CAP_SYS_ADMIN), the
// function the thread waits in (its wchan) stands in for it.
func TestThreadFieldsFallBackToTheWchan(t *testing.T) {
	dir := fakeTask(t, "868 (keploy) S 1 868", "pipe_read\n", "", "")
	got := map[string]string{}
	for _, f := range threadFields(dir, 0, false) {
		got[f.Key] = f.String
	}
	if got["waitingIn"] != "pipe_read" {
		t.Errorf("waitingIn = %q, want pipe_read when there is no stack to read", got["waitingIn"])
	}
	if _, ok := got["kernelStack"]; ok {
		t.Errorf("kernelStack = %q reported with no stack file", got["kernelStack"])
	}
}

// The WSL match is tested directly: it is the one thing in choosing the
// remedy that can be wrong.
func TestIsWSLRelease(t *testing.T) {
	cases := []struct {
		release string
		want    bool
	}{
		{"5.15.153.1-microsoft-standard-WSL2", true},
		{"4.4.0-19041-Microsoft", true}, // WSL1 capitalises it
		{"6.12.63+deb13-amd64", false},
		{"5.10.0-46-cloud-amd64", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isWSLRelease(tc.release); got != tc.want {
			t.Errorf("isWSLRelease(%q) = %v, want %v", tc.release, got, tc.want)
		}
	}
}

// The ERROR report gives a remedy under WSL only, where restarting the WSL
// VM ends the wait whatever it is; elsewhere none, and no WARN report gives
// one.
func TestWatchStallGivesARemedyOnlyUnderWSL(t *testing.T) {
	logger, logs := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", "", time.Millisecond, time.Hour, 1)
	fields := firstReport(logs)
	stop()
	got, ok := fields["remediation"]
	switch {
	case runningUnderWSL() && got != wslRemediation:
		t.Errorf("under WSL the ERROR report's remediation is %v, want %q", got, wslRemediation)
	case !runningUnderWSL() && ok:
		t.Errorf("off WSL the ERROR report gives a remedy: %v", got)
	}
	logger, logs = newObservedLogger()
	stop = watchStall(logger, "bpf(BPF_PROG_LOAD)", "", time.Millisecond, time.Hour, neverError)
	fields = firstReport(logs)
	stop()
	if got, ok := fields["remediation"]; ok {
		t.Errorf("a WARN report gives a remedy: %v", got)
	}
}

// ranOnCPU is the CPU the thread ran from the call's start to the report:
// a load the verifier works through runs, one waiting on what another task
// holds does not.
func TestThreadFieldsSayHowLongTheThreadRan(t *testing.T) {
	dir := fakeTask(t, "868 (keploy) R 1 868", "0\n", "", "3000000 5000 14\n")
	var ran interface{}
	for _, f := range threadFields(dir, time.Millisecond, true) {
		if f.Key == "ranOnCPU" {
			ran = time.Duration(f.Integer)
		}
	}
	if ran != 2*time.Millisecond {
		t.Errorf("ranOnCPU = %v, want 2ms (3 ms run at the report, 1 ms when the call began)", ran)
	}
	for _, f := range threadFields(dir, 0, false) {
		if f.Key == "ranOnCPU" {
			t.Errorf("ranOnCPU reported though the thread's run time was not read when the call began")
		}
	}
}

// A file the kernel does not let the agent read (a thread's stack, without
// CAP_SYS_ADMIN), a "0" wchan (a running thread), or a thread already gone
// leaves that field out rather than reporting it empty.
func TestWatchStallLeavesOutWhatItCannotRead(t *testing.T) {
	dir := fakeTask(t, "868 (keploy) R 1 868", "0\n", "", "0 0 0\n")
	logger, logs := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", dir, 10*time.Millisecond, time.Hour, neverError)
	defer stop()
	fields := firstReport(logs)
	if got := fields["threadState"]; got != "R" {
		t.Errorf("threadState = %v, want R", got)
	}
	for _, absent := range []string{"waitingIn", "kernelStack", "ranOnCPU"} {
		if v, ok := fields[absent]; ok {
			t.Errorf("%s = %v reported, though there was nothing to read", absent, v)
		}
	}

	logger, logs = newObservedLogger()
	stop2 := watchStall(logger, "bpf(BPF_PROG_LOAD)", filepath.Join(t.TempDir(), "gone"), 10*time.Millisecond, time.Hour, neverError)
	defer stop2()
	fields = firstReport(logs)
	for _, absent := range []string{"threadState", "waitingIn", "kernelStack", "ranOnCPU"} {
		if v, ok := fields[absent]; ok {
			t.Errorf("%s = %v reported for a thread that is gone", absent, v)
		}
	}
	if _, ok := fields["blockedFor"]; !ok {
		t.Error("the report must still say how long the call has been blocked")
	}
}

// blockedReader starts a goroutine that, on a thread of its own, runs call
// with a function that blocks in read(2) on a pipe until release is called,
// and returns once the kernel says that thread is in the read: sleeping, in
// the read system call (its syscall file, which this process can read for
// its own threads). It returns the thread's id and release, which also waits
// for call to have returned.
func blockedReader(t *testing.T, call func(blockingRead func() error) error) (tid int, release func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	tids := make(chan int, 1) // buffered: the goroutine's next wait is the read
	done := make(chan struct{})
	var once sync.Once
	unblock := func() {
		once.Do(func() {
			_, _ = w.Write([]byte{1})
			<-done
			_ = w.Close()
			_ = r.Close()
		})
	}
	// A test that fails on the way still lets the reader go.
	t.Cleanup(unblock)
	go func() {
		defer close(done)
		_ = call(func() error {
			tids <- unix.Gettid()
			var b [1]byte
			_, err := unix.Read(int(r.Fd()), b[:]) // blocks until w is written
			return err
		})
	}()
	select {
	case tid = <-tids:
	case <-done:
		t.Fatal("the call returned without reading")
	}
	for {
		select {
		case <-done:
			t.Fatal("the call returned before its thread was seen in the read")
		default:
		}
		stat, err := os.ReadFile(filepath.Join(taskDir(tid), "stat"))
		if err != nil {
			t.Fatal(err)
		}
		sc, err := os.ReadFile(filepath.Join(taskDir(tid), "syscall"))
		if err != nil {
			t.Fatal(err)
		}
		if f := strings.Fields(string(sc)); taskState(stat) == "S" && len(f) > 0 && f[0] == strconv.Itoa(unix.SYS_READ) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return tid, unblock
}

// The live thread: a goroutine locked to its thread and blocked in a read
// is reported as sleeping, from this process's own /proc (taskDir).
func TestWatchStallReadsALiveThread(t *testing.T) {
	tid, release := blockedReader(t, func(blockingRead func() error) error {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		return blockingRead()
	})
	logger, logs := newObservedLogger()
	stop := watchStall(logger, "read(2)", taskDir(tid), time.Millisecond, time.Hour, neverError)
	fields := firstReport(logs)
	stop()
	release()
	if got := fields["threadState"]; got != "S" {
		t.Errorf("threadState = %v, want S for a thread blocked in read(2)", got)
	}
	if got := fields["thread"]; got != strconv.Itoa(tid) {
		t.Errorf("thread = %v, want %d", got, tid)
	}
	if st, ok := fields["kernelStack"].(string); ok && !strings.Contains(st, "pipe_read") {
		t.Errorf("kernelStack = %q, want the pipe read the thread sleeps in", st)
	}
}

// watchedCall watches the thread fn runs on (taskDir of its gettid), and
// keeps fn on it: the report names fn's thread, and fn's goroutine does not
// leave it however often it yields.
func TestWatchedCallWatchesTheThreadTheCallRunsOn(t *testing.T) {
	logger, logs := newObservedLogger()
	moved := make(chan string, 1)
	tid, release := blockedReader(t, func(blockingRead func() error) error {
		return watchedCallWith(logger, "read(2)", time.Millisecond, time.Hour, neverError, func() error {
			first := unix.Gettid()
			for i := 0; i < 500; i++ {
				runtime.Gosched()
				if now := unix.Gettid(); now != first {
					moved <- fmt.Sprintf("fn's goroutine moved from thread %d to %d", first, now)
					break
				}
			}
			return blockingRead()
		})
	})
	select {
	case m := <-moved:
		t.Error(m)
	default:
	}
	fields := firstReport(logs)
	release()
	if got := fields["thread"]; got != strconv.Itoa(tid) {
		t.Errorf("the report names thread %v, want %d, the one in the call", got, tid)
	}
	if got := fields["operation"]; got != "read(2)" {
		t.Errorf("operation = %v, want read(2)", got)
	}
	// Not its state: the one report can come before the thread is in the
	// read (TestWatchStallReadsALiveThread reads the state of one that is).
}

// A call that is merely SLOW must not be reported at ERROR. ERROR is a terminal
// signal here: the DaemonSet e2e fails a run on any agent ERROR and the sample-app
// CI lanes gate on `grep ERROR` over the agent log, so a load that blocks past the first
// tick and then succeeds would fail an otherwise perfectly healthy run — which
// is exactly what happened before this split (BPF load returned at ~16s, all 34
// tests passed, the lane still exited 1).
func TestWatchStallReportsSlowCallAtWarnNotError(t *testing.T) {
	logger, logs := newObservedLogger()
	// The first report, with the agent's own errorFrom: the call may still
	// return, WARN, not ERROR.
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", "", 10*time.Millisecond, time.Hour, stallErrorFrom)
	defer stop()

	firstReport(logs)
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

// The watch the agent's calls get (agentStall, what watchedCall passes)
// gives a call at least 2 minutes before its first ERROR: 135 s at the
// soonest, the number stallErrorFrom's comment gives.
func TestStallErrorFromComesAfterTwoMinutes(t *testing.T) {
	soonest := agentStall.first + time.Duration(agentStall.errorFrom-1)*agentStall.repeat
	if soonest != 135*time.Second {
		t.Errorf("the first ERROR comes at %v blocked at the soonest; stallErrorFrom's comment says 135 s", soonest)
	}
	if soonest < 2*time.Minute {
		t.Errorf("the first ERROR comes at %v blocked: a call is given at least 2 minutes", soonest)
	}
}

// The escalation itself — reports that start at WARN and become ERROR from
// report errorFrom on. Testing only the two endpoints (errorFrom=1 and
// errorFrom=never) would pass against an implementation that derived the
// severity from "errorFrom == 1" and never crossed anything. A count, so a
// first report that comes late is still a WARN.
func TestWatchStallEscalatesFromWarnToError(t *testing.T) {
	logger, logs := newObservedLogger()
	// ERROR from the third report on: the first two are WARN however late
	// they come: the level is the report's number, not the time.
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", "", time.Millisecond, time.Millisecond, 3)
	defer stop()
	for logs.Len() < 4 {
		time.Sleep(time.Millisecond)
	}
	stop()

	all := logs.All()
	for i, want := range []zapcore.Level{zap.WarnLevel, zap.WarnLevel, zap.ErrorLevel, zap.ErrorLevel} {
		if all[i].Level != want {
			t.Errorf("report %d is %v, want %v: WARN while the call could still return, ERROR from errorFrom on", i+1, all[i].Level, want)
		}
	}
}

// It must keep reporting: a single line scrolls away, and the operator who
// looks at a wedged container ten minutes later needs to see it is still stuck.
func TestWatchStallRepeatsWhileBlocked(t *testing.T) {
	logger, logs := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", "", 10*time.Millisecond, 10*time.Millisecond, 1)
	defer stop()

	// However long the reports take to come: a watchdog that stopped
	// reporting is the test binary's timeout.
	for logs.Len() < 3 {
		time.Sleep(time.Millisecond)
	}
}

// stop() must be synchronous: if it returned while the goroutine was mid-report,
// a successful load could still emit a scary ERROR after the fact.
func TestWatchStallStopIsRaceFree(t *testing.T) {
	for i := 0; i < 50; i++ {
		logger, logs := newObservedLogger()
		stop := watchStall(logger, "op", "", time.Microsecond, time.Microsecond, 1)
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
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", "", time.Hour, time.Hour, stallErrorFrom)
	stop()
	stop() // would panic on a second close(done)
	stop()
}

// Concurrent stops must be safe too, and every one of them must observe the
// watchdog actually stopped rather than returning early.
func TestWatchStallStopIsConcurrencySafe(t *testing.T) {
	logger, _ := newObservedLogger()
	stop := watchStall(logger, "bpf(BPF_PROG_LOAD)", "", time.Hour, time.Hour, stallErrorFrom)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); stop() }()
	}
	// Concurrent stops that deadlock are the test binary's timeout.
	wg.Wait()
}

// A schedstat run time that does not fit a Duration, or carries a sign, is no
// reading: the thread's CPU time is unknown then, not a wrapped negative figure.
func TestThreadRanRefusesARunTimeItCannotHold(t *testing.T) {
	for _, first := range []string{"18446744073709551615", "9223372036854775808", "-1"} {
		dir := fakeTask(t, "", "", "", first+" 0 0\n")
		if got, ok := threadRan(dir); ok {
			t.Errorf("threadRan(%s) = %v, true; want unknown", first, got)
		}
	}
	dir := fakeTask(t, "", "", "", "9223372036854775807 0 0\n")
	if got, ok := threadRan(dir); !ok || got != time.Duration(9223372036854775807) {
		t.Errorf("threadRan(max int64) = %v, %v; want the reading", got, ok)
	}
}
