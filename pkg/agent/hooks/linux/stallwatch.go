//go:build linux

package linux

import (
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// isWSLRelease reports whether a kernel release string is a WSL one. Both
// generations advertise themselves there — WSL2 as "...-microsoft-standard-WSL2"
// and WSL1 as "4.4.0-19041-Microsoft" — so the lowercased substring covers both.
//
// Split out as a pure function so the parsing is testable on its own: stubbing a
// runningUnderWSL var instead would have left the one thing that can actually be
// wrong (this match) uncovered, and a var read from the watchdog goroutine while
// a test writes it is a data race waiting for the first t.Parallel().
func isWSLRelease(release string) bool {
	return strings.Contains(strings.ToLower(release), "microsoft")
}

// runningUnderWSL reports whether this kernel is a WSL one. It reads the release
// via uname(2) rather than /proc so it keeps working under a masked or
// unmounted /proc (procMount, hardened seccomp/AppArmor profiles).
func runningUnderWSL() bool {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return false
	}
	return isWSLRelease(unix.ByteSliceToString(uts.Release[:]))
}

// stallRemediation returns advice that matches the kernel actually running.
//
// The WSL fix ("wsl --shutdown") is real but WSL-only, and emitting it
// unconditionally sent operators on Linux hosts — every CI runner and every
// production node — chasing a command that does not exist there, on a log line
// that already reads as an emergency. Advice that cannot apply is worse than no
// advice, so each platform gets the step that actually clears its own stall.
func stallRemediation() string {
	if runningUnderWSL() {
		return "on WSL2/Docker Desktop run 'wsl --shutdown', which reboots the shared kernel; terminating only the docker-desktop distro leaves the same kernel running and does not clear it"
	}
	return "the stall is in the HOST kernel, so it outlives this container: check the host kernel log (dmesg) for 'tasks_rcu_exit_srcu_stall' and drain/reboot that node — restarting the agent or the pod cannot clear it"
}

// Default thresholds for watchStall. Loading the whole eBPF collection is
// normally well under a second, so 15s already means something is wrong, and
// repeating every 30s keeps a wedged agent saying so for as long as it is up
// without flooding the log.
const (
	stallFirstReport  = 15 * time.Second
	stallRepeatReport = 30 * time.Second
	// stallErrorAfter is when a slow call stops being slow and starts being
	// wedged, and therefore when the report escalates from WARN to ERROR.
	//
	// The distinction is the whole point of the severity. ERROR means "this
	// degraded and you need to know" — a terminal outcome — and consumers act on
	// it: the DaemonSet e2e fails a run on any agent ERROR, and the sample-app CI
	// lanes gate on `grep ERROR` over the agent log. Reporting the FIRST 15s tick at ERROR
	// made every load that was merely slow-then-successful indistinguishable
	// from a permanently wedged one, so a completely healthy run (BPF load
	// returned at ~16s, every test passed) was failed by its own logging. It
	// also contradicted this file's own contract: stop() is synchronous
	// specifically "so a successful load can never emit a scary ERROR", which
	// only held when the load beat the first tick.
	//
	// A load blocked this long is not slow. The condition this watchdog exists
	// for — a stalled RCU-tasks grace period — blocks bpf(BPF_PROG_LOAD) at 0%
	// CPU for as long as the grace period is stuck (the kernel does not even
	// REPORT one until rcu_task_stall_timeout, 600s by default), whereas a
	// contended-but-progressing load clears this bar in seconds. It shrinks the
	// false-ERROR window to near zero rather than proving it shut: a stall that
	// resolves because the blocking task exits can still cross it. 2 minutes sits far
	// above the slowest observed real load (~16s) and far below the point where
	// an operator would have given up anyway.
	stallErrorAfter = 2 * time.Minute
)

// watchStall reports that a kernel call has not returned yet, and keeps
// reporting until the returned stop func is called.
//
// It exists because bpf(BPF_PROG_LOAD) is uninterruptible. When the kernel
// blocks inside it, no context cancellation frees the goroutine, the thread
// sits in D state, and the process cannot be killed — SIGKILL leaves the
// container as "tried to kill container, but did not receive an exit event".
// This is not hypothetical: a stalled RCU-tasks grace period on the host
// (kernel log: "tasks_rcu_exit_srcu_stall") makes every BPF_PROG_LOAD wait
// forever at 0% CPU.
//
// The agent could not previously say any of that. It blocked before its first
// log line, so the only visible symptom was a container that never became
// ready, an empty log, and a healthcheck that timed out minutes later —
// diagnosing it needed a goroutine dump taken off the CI host by hand.
//
// The syscall cannot be aborted, so this does not try to. It makes the failure
// legible instead: what is blocked, for how long, and the kernel condition that
// usually explains it.
//
// Reports start at WARN and escalate to ERROR once the call has been blocked for
// errorAfter, so the severity distinguishes "slow" from "never coming back" — see
// stallErrorAfter. A zero errorAfter reports at ERROR from the first tick.
func watchStall(logger *zap.Logger, operation string, first, repeat, errorAfter time.Duration) (stop func()) {
	if logger == nil {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		start := time.Now()
		timer := time.NewTimer(first)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-timer.C:
				// Round once and branch on the SAME value that gets logged, so a
				// line can never read "blockedFor=2m0s" while being emitted at WARN
				// because the raw duration was a few ms short of the threshold.
				blocked := time.Since(start).Round(time.Second)
				fields := []zap.Field{
					zap.String("operation", operation),
					zap.Duration("blockedFor", blocked),
					zap.String("likelyCause", "a stalled RCU-tasks grace period makes bpf(BPF_PROG_LOAD) block indefinitely at 0% CPU; check the host kernel log for 'tasks_rcu_exit_srcu_stall'"),
					zap.String("remediation", stallRemediation()),
				}
				// WARN while the call may still return, ERROR once it has been
				// blocked long enough that it never will. See stallErrorAfter.
				if blocked >= errorAfter {
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
