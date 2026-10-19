//go:build linux

package linux

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// Default thresholds for watchStall. Loading the whole eBPF collection is
// normally well under a second, so 15s already means something is wrong, and
// repeating every 30s keeps a wedged agent saying so for as long as it is up
// without flooding the log.
const (
	stallFirstReport  = 15 * time.Second
	stallRepeatReport = 30 * time.Second
	// stallErrorFrom is the report from which a slow call is taken for a
	// wedged one, and the report escalates from WARN to ERROR: the fifth,
	// so a call is given at least 2 minutes, for the fifth comes once it has
	// been blocked for 135 s at the soonest (15 s + 4 x 30 s; a timer fires
	// late, never early). A count, not a
	// time: a report that comes late, the watch's goroutine itself starved
	// or the process frozen, is still the report it is, and cannot jump to
	// ERROR before the WARNs that come first.
	//
	// The distinction is the whole point of the severity. ERROR means "this
	// degraded and you need to know" — a terminal outcome — and consumers act
	// on it: the DaemonSet e2e fails a run on any agent ERROR, and the
	// sample-app CI lanes gate on `grep ERROR` over the agent log. Reporting
	// the FIRST 15s tick at ERROR made every load that was merely
	// slow-then-successful indistinguishable from a permanently wedged one,
	// so a completely healthy run (BPF load returned at ~16s, every test
	// passed) was failed by its own logging. It also contradicted this file's
	// own contract: stop() is synchronous specifically "so a successful load
	// can never emit a scary ERROR", which only held when the load beat the
	// first tick.
	//
	// A load blocked this long is not slow. A bpf(BPF_PROG_LOAD) that waits
	// in the kernel on something another task holds (a lock, a grace period
	// the lock's holder waits for) waits at no CPU for as long as that does,
	// whereas a contended-but-progressing load clears this bar in seconds. It
	// shrinks the false-ERROR window to near zero rather than proving it
	// shut: a wait that ends because its holder lets go can still cross it.
	// 135 s sits above the loads seen to return (on CI's 5.10 hosts
	// they took 15-45 s, while keploy's objects still had prints that made
	// the verifier wait on the tracing event_mutex; others there stayed
	// blocked past 60 s, until the run was stopped) and below the point
	// where an operator would have given up anyway.
	//
	// The keploy that waits for this agent's start does not wait for this ERROR,
	// and need not: a load blocked at 0% CPU is a start that takes no step, which
	// it gives up on after utils.TeardownStallChecks of the agent's beats, often
	// before the ERROR lands, naming the stage in progress and its thread's state
	// (D, in the uninterruptible wait) from the agent's boot report. The ERROR is
	// the agent's own log's word on it, for whoever reads that log.
	stallErrorFrom = 5

	// stallStackFrames is how many of the blocked thread's innermost kernel
	// frames a report carries: enough to name what it waits in and who asked
	// for that, not the whole syscall path.
	stallStackFrames = 12
)

// watchStall reports that a kernel call has not returned yet, and keeps
// reporting until the returned stop func is called. thread is the
// /proc/self/task/<tid> directory of the thread making the call (taskDir):
// the caller keeps its goroutine on that thread (runtime.LockOSThread) until
// stop has returned.
//
// It exists because a kernel call such as bpf(BPF_PROG_LOAD) is
// uninterruptible while it waits in the kernel (watchedCall names the agent's
// start's): no context cancellation frees the goroutine, the thread sits in
// D state, and a kill takes effect only once the call gets past the wait. The
// agent blocked there before its first log line, so the only visible symptom
// was a container that never became ready and an empty log, and diagnosing it
// needed a goroutine dump and the kernel's view of the thread, taken off the
// host by hand.
//
// The syscall cannot be aborted, so this does not try to. It makes the wait
// legible instead, with what the kernel says of the thread and nothing it does
// not: what is blocked and for how long; which thread; the thread's state (R
// running or waiting to run, D in an uninterruptible wait, S sleeping); its
// innermost kernel frames (its stack, which only a process with CAP_SYS_ADMIN
// may read, as the agent normally has), or else the kernel function it waits
// in (its wchan); and the CPU time it ran since the call began, which tells a
// load the verifier is still working through on a starved host from one
// waiting on something another task holds. What the waiting means is for the
// reader, from those frames: a cause named here without reading it off the
// thread was a guess, and on the hosts where it was printed most it was wrong.
//
// Reports start at WARN and escalate to ERROR from report errorFrom on, so the
// severity distinguishes "slow" from "never coming back" — see stallErrorFrom.
// An errorFrom of 1 or less reports at ERROR from the first report.
func watchStall(logger *zap.Logger, operation, thread string, first, repeat time.Duration, errorFrom int) (stop func()) {
	if logger == nil {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	ranBefore, ranKnown := threadRan(thread)
	go func() {
		defer close(stopped)
		start := time.Now()
		timer := time.NewTimer(first)
		defer timer.Stop()
		reports := 0
		for {
			select {
			case <-done:
				return
			case <-timer.C:
				reports++
				fields := []zap.Field{
					zap.String("operation", operation),
					zap.Duration("blockedFor", time.Since(start).Round(time.Second)),
				}
				fields = append(fields, threadFields(thread, ranBefore, ranKnown)...)
				// WARN while the call may still return, ERROR from report
				// errorFrom on, by which a call that is still blocked is most
				// likely blocked for good. See stallErrorFrom.
				if reports >= errorFrom {
					if runningUnderWSL() {
						fields = append(fields, zap.String("remediation", wslRemediation))
					}
					logger.Error("kernel call has not returned; the agent cannot become ready until it does", fields...)
				} else {
					logger.Warn("kernel call is taking unusually long; the agent cannot become ready until it returns", fields...)
				}
				timer.Reset(repeat)
			}
		}
	}()
	// Idempotent, because the natural way to use this is `defer stop()` next to
	// an explicit stop on some path, and a second close(done) would panic — a
	// diagnostic helper must never be the thing that takes the process down.
	//
	// Waiting on stopped is what makes stop() synchronous: a caller that returns
	// straight after stopping cannot race a final report out of a watchdog it
	// already stopped, so a successful load can never emit a scary ERROR.
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-stopped
		})
	}
}

// wslRemediation is what the ERROR report adds under WSL (Docker Desktop on
// Windows): restarting the WSL VM, the kernel every container shares, ends a
// kernel call wedged this long, whatever it waits on. It is the one remedy a
// report gives, and only under WSL, for its cost: restarting the WSL VM is
// cheap and local to the user's machine, where rebooting a Linux node is
// costly and is not needed for every wait (on CI's 5.10 hosts the waits
// cleared by themselves); there the cause is the reader's to find from the
// thread's stack.
const wslRemediation = "under WSL2 (Docker Desktop), restarting the WSL VM ends a kernel call wedged this long, whatever it waits on: " +
	"run 'wsl --shutdown' and start Docker Desktop again; terminating the docker-desktop distro alone leaves the same kernel running"

// isWSLRelease reports whether a kernel release string is a WSL one. Both
// generations advertise themselves there — WSL2 as "...-microsoft-standard-WSL2"
// and WSL1 as "4.4.0-19041-Microsoft" — so the lowercased substring covers both.
// A pure function, so the match, the one thing here that can be wrong, is
// tested on its own.
func isWSLRelease(release string) bool {
	return strings.Contains(strings.ToLower(release), "microsoft")
}

// runningUnderWSL reports whether this kernel is a WSL one. It reads the
// release via uname(2) rather than /proc so it keeps working under a masked
// or unmounted /proc.
func runningUnderWSL() bool {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return false
	}
	return isWSLRelease(unix.ByteSliceToString(uts.Release[:]))
}

// taskDir is the /proc directory of this process's thread tid.
func taskDir(tid int) string { return "/proc/self/task/" + strconv.Itoa(tid) }

// isNumber says whether s is a thread id: digits only.
func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// watchedCall runs fn, a kernel call that can wait in the kernel for long
// without saying anything, on a thread kept for it, with watchStall reporting
// on that thread until fn returns: operation names the call. The hooks' calls
// that go through it are the ones seen to wait, and their kin: the eBPF loads
// (a verifier that locks the tracing event_mutex, on kernels before upstream
// 4580f4e0ebdf, for any bpf_trace_printk call), the perf-event tracepoint
// attaches (perf_trace_init locks event_mutex, which every tracer's attach
// and detach on the host holds) and the fexit attaches (their trampoline's
// locks). The cgroup attaches (cgroup_mutex) and the raw tracepoint ones
// (tracepoints_mutex) do not go through it yet.
func watchedCall(logger *zap.Logger, operation string, fn func() error) error {
	return watchedCallWith(logger, operation, agentStall.first, agentStall.repeat, agentStall.errorFrom, fn)
}

// stallThresholds are a watch's: its first report, the interval of the
// ones after, and the report from which they are ERROR.
type stallThresholds struct {
	first, repeat time.Duration
	errorFrom     int
}

// agentStall is the watch the agent's calls get (watchedCall).
var agentStall = stallThresholds{first: stallFirstReport, repeat: stallRepeatReport, errorFrom: stallErrorFrom}

// watchedCallWith is watchedCall with the watch's thresholds (watchStall).
// The goroutine stays on its thread from before the watch begins until after
// it has stopped, so the thread the reports read is the one fn runs on, and
// fn must make its kernel call on the calling goroutine.
func watchedCallWith(logger *zap.Logger, operation string, first, repeat time.Duration, errorFrom int, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	stop := watchStall(logger, operation, taskDir(unix.Gettid()), first, repeat, errorFrom)
	defer stop()
	return fn()
}

// threadFields are what the kernel says of the thread at dir: its id, its
// state, its innermost kernel frames (or, where they cannot be read, the
// function it waits in), and the CPU time it ran since ranBefore was read
// (when both reads worked). A file the kernel does not let this process read,
// or a thread already gone, leaves its field out: a report says only what was
// read.
func threadFields(dir string, ranBefore time.Duration, ranKnown bool) []zap.Field {
	if dir == "" {
		return nil
	}
	var fields []zap.Field
	if tid := filepath.Base(dir); isNumber(tid) {
		fields = append(fields, zap.String("thread", tid))
	}
	if b, err := os.ReadFile(dir + "/stat"); err == nil {
		if st := taskState(b); st != "" {
			fields = append(fields, zap.String("threadState", st))
		}
	}
	// The stack, where it can be read, says where the thread waits better
	// than its wchan, which on 5.10 x86-64 (ORC unwinder) is usually "0" for
	// a sleeping thread and now and then names the wrong function: the wchan
	// stands in only for a stack this process may not read.
	stacked := false
	if b, err := os.ReadFile(dir + "/stack"); err == nil {
		if frames := kernelFrames(b, stallStackFrames); len(frames) > 0 {
			fields = append(fields, zap.String("kernelStack", strings.Join(frames, " < ")))
			stacked = true
		}
	}
	if !stacked {
		if b, err := os.ReadFile(dir + "/wchan"); err == nil {
			// The kernel writes "0" for a thread that is running, and for a
			// reader it does not show kernel symbols to.
			if w := strings.TrimSpace(string(b)); w != "" && w != "0" {
				fields = append(fields, zap.String("waitingIn", w))
			}
		}
	}
	if ranKnown {
		if ran, ok := threadRan(dir); ok && ran >= ranBefore {
			fields = append(fields, zap.Duration("ranOnCPU", ran-ranBefore))
		}
	}
	return fields
}

// taskState reads a task's state letter from its stat file. The command
// name, in parentheses, may itself hold spaces and parentheses: the fields
// are counted from the last ')'.
func taskState(stat []byte) string {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return ""
	}
	f := bytes.Fields(stat[i+1:])
	if len(f) == 0 {
		return ""
	}
	return string(f[0])
}

// kernelFrames reads up to limit function names off a task's stack file, the
// innermost first: each line is "[<address>] function+offset/size", and only
// the function names are kept (the kernel prints every address as <0>, and
// the offsets mean nothing without the build).
func kernelFrames(stack []byte, limit int) []string {
	var frames []string
	for _, line := range bytes.Split(stack, []byte("\n")) {
		f := bytes.Fields(line)
		if len(f) < 2 {
			continue
		}
		name := f[1]
		if j := bytes.IndexByte(name, '+'); j > 0 {
			name = name[:j]
		}
		frames = append(frames, string(name))
		if len(frames) == limit {
			break
		}
	}
	return frames
}

// threadRan is the CPU time the thread at dir has run (schedstat's first
// field, in ns). A kernel without the account writes zeros or no file: ok is
// false then, and no report says how long the thread ran.
func threadRan(dir string) (time.Duration, bool) {
	if dir == "" {
		return 0, false
	}
	b, err := os.ReadFile(dir + "/schedstat")
	if err != nil {
		return 0, false
	}
	f := bytes.Fields(b)
	if len(f) < 3 {
		return 0, false
	}
	ns, err := strconv.ParseUint(string(f[0]), 10, 64)
	if err != nil {
		return 0, false
	}
	waited, _ := strconv.ParseUint(string(f[1]), 10, 64)
	slices, _ := strconv.ParseUint(string(f[2]), 10, 64)
	if ns == 0 && waited == 0 && slices == 0 {
		return 0, false
	}
	return time.Duration(ns), true
}
