//go:build linux

package http

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// keploy's interrupt of the application leaves to its caller each process it
// reached that ignores it (a shell's background command ignores SIGINT): it
// neither waits for them nor kills them. The caller ends them, where keploy
// adopts nothing too (it could not: no subreaper): once the run's stop of the
// application is over, no process its interrupt reached runs, for the stop
// at a set's end (utils.InterruptProcessTreeStop) and for any other stop
// (utils.InterruptProcessTreeReached). One that acts on SIGTERM exits on it;
// one that takes SIGTERM and never gets out is killed once it makes no
// progress, and the set's end says so; one that ignores SIGTERM too is
// killed as well (it has no way out of its own for the kill to cut).
func TestTheSetEndsStopEndsWhatItsInterruptLeftWhereNothingIsAdopted(t *testing.T) {
	stopOfWhatTheInterruptLeft(t, models.ErrAppStoppedForSetEnd)
}

// The same, for a stop of the run that is not a set's end.
func TestTheRunsStopEndsWhatItsInterruptLeftWhereNothingIsAdopted(t *testing.T) {
	stopOfWhatTheInterruptLeft(t, context.Canceled)
}

func stopOfWhatTheInterruptLeft(t *testing.T, cause error) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	if adopting.Load() {
		t.Fatal("this process adopts the application's daemons; the row is of one that does not")
	}
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	dir := t.TempDir()
	bin := stopHelperBinary(t, false)
	client := newRunClient(fmt.Sprintf("%s & %s & %s & wait", helperCommand(bin, "ignoring", dir, false),
		helperCommand(bin, "stalls", dir, false), helperCommand(bin, "deaf", dir, false)))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	type proc struct {
		pid   int
		start uint64
	}
	procs := map[string]proc{}
	for _, name := range []string{"ignoring", "stalls", "deaf"} {
		pid := helperPid(t, dir, name)
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		st, ok := utils.ReadProcStat(pid)
		if !ok {
			t.Fatalf("the %s helper %d cannot be read", name, pid)
		}
		procs[name] = proc{pid, st.StartTime}
	}
	cancel(cause)
	var appErr models.AppError
	select {
	case appErr = <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("the run's stop had not returned 2m after it began")
	}
	for name, p := range procs {
		if st, ok := utils.ReadProcStat(p.pid); ok && st.StartTime == p.start && !utils.Exited(p.pid, st) {
			t.Errorf("the %s helper %d, which the interrupt reached, runs after the stop of the application returned", name, p.pid)
		}
	}
	for _, name := range []string{"ignoring", "stalls"} {
		if _, err := os.Stat(filepath.Join(dir, name+".term")); err != nil {
			t.Errorf("the %s helper, which ignores the interrupt, was not asked to stop with SIGTERM: %v", name, err)
		}
	}
	if cause == models.ErrAppStoppedForSetEnd && !strings.Contains(appErr.StopKilled, "made no progress on its way out") {
		t.Errorf("the set's end said %q; want the kill of the helper that made no progress on its way out", appErr.StopKilled)
	}
}

// What stopHanded ends of what the interrupt reached: one that ignored it and
// is still that process, not one that acted on it, nor a process its pid
// names now that is not the one it reached (another start time), nor one an
// earlier stop killed and the kernel holds (leftToKernel).
func TestStopHandedTakesOnlyWhatTheInterruptLeft(t *testing.T) {
	pid := startStopped(t)
	st, ok := utils.ReadProcStat(pid)
	if !ok {
		t.Fatalf("the sleep %d cannot be read", pid)
	}
	read := func(r utils.Reached) []int {
		ms := handedMembers([]utils.Reached{r})
		var pids []int
		for _, m := range ms {
			closePidfd(m.fd)
			pids = append(pids, m.pid)
		}
		return pids
	}
	if got := read(utils.Reached{PID: pid, Start: st.StartTime, Ignores: true}); len(got) != 1 || got[0] != pid {
		t.Errorf("handedMembers of the sleep that ignored the interrupt = %v; want [%d]", got, pid)
	}
	if got := read(utils.Reached{PID: pid, Start: st.StartTime}); len(got) != 0 {
		t.Errorf("handedMembers of the sleep that acts on the interrupt = %v; want none", got)
	}
	if got := read(utils.Reached{PID: pid, Start: st.StartTime + 1, Ignores: true}); len(got) != 0 {
		t.Errorf("handedMembers of another process with the sleep's pid = %v; want none", got)
	}
	key := [2]uint64{uint64(pid), st.StartTime}
	leftToKernel.Store(key, true)
	defer leftToKernel.Delete(key)
	if got := read(utils.Reached{PID: pid, Start: st.StartTime, Ignores: true}); len(got) != 0 {
		t.Errorf("handedMembers of the sleep the kernel holds after a kill = %v; want none", got)
	}
}

// startStopped starts a sleep, which its cleanup kills and reaps.
func startStopped(t *testing.T) int {
	t.Helper()
	proc, err := os.StartProcess("/bin/sleep", []string{"sleep", "600"}, &os.ProcAttr{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = proc.Kill()
		_, _ = proc.Wait()
	})
	return proc.Pid
}
