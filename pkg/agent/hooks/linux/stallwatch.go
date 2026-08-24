//go:build linux

package linux

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// Default thresholds for watchStall. Loading the whole eBPF collection is
// normally well under a second, so 15s already means something is wrong, and
// repeating every 30s keeps a wedged agent saying so for as long as it is up
// without flooding the log.
const (
	stallFirstReport  = 15 * time.Second
	stallRepeatReport = 30 * time.Second
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
func watchStall(logger *zap.Logger, operation string, first, repeat time.Duration) (stop func()) {
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
				logger.Error("kernel call has not returned; the agent cannot become ready until it does",
					zap.String("operation", operation),
					zap.Duration("blockedFor", time.Since(start).Round(time.Second)),
					zap.String("likelyCause", "a stalled RCU-tasks grace period makes bpf(BPF_PROG_LOAD) block indefinitely at 0% CPU; check the host kernel log for 'tasks_rcu_exit_srcu_stall'"),
					zap.String("remediation", "on WSL2/Docker Desktop run 'wsl --shutdown', which reboots the shared kernel; terminating only the docker-desktop distro leaves the same kernel running and does not clear it"),
				)
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
