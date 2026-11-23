//go:build linux

package http

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A process keploy's interrupt left to its caller that ignores SIGTERM too
// and runs on busily (it writes a page to a file over and over, as fast as
// it can) is on no way out of its own: what it does is no progress, so the
// stop of what the application left kills it once the stall is over and
// returns, where keploy adopts nothing; and as it had no way out for the
// kill to cut, the set's end does not say the kill cut one.
func TestTheStopKillsABusyProcessTheInterruptLeftThatIgnoresSigterm(t *testing.T) {
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
	client := newRunClient(fmt.Sprintf("%s & wait", helperCommand(bin, "busy", dir, false)))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	pid := helperPid(t, dir, "busy")
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	st, ok := utils.ReadProcStat(pid)
	if !ok {
		t.Fatalf("the busy helper %d cannot be read", pid)
	}
	cancel(models.ErrAppStoppedForSetEnd)
	var appErr models.AppError
	select {
	case appErr = <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("the run's stop had not returned 2m after it began: it waited on a busy process that ignores SIGTERM")
	}
	if now, ok := utils.ReadProcStat(pid); ok && now.StartTime == st.StartTime && !utils.Exited(pid, now) {
		t.Errorf("the busy helper %d, which the interrupt reached, runs after the stop of the application returned", pid)
	}
	if strings.Contains(appErr.StopKilled, "made no progress on its way out") {
		t.Errorf("the set's end said %q: a kill that cut a way out, of a process that ignored SIGTERM and so had none", appErr.StopKilled)
	}
}

// What a read of a process that ignores SIGTERM shows it doing is no
// progress of the stop's: read so, the process here writes to storage and
// waits on it at every check, and the stop kills it once the stall is over
// all the same, and returns.
func TestWhatAProcessThatIgnoresSigtermDoesHoldsNoStop(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	r := startIgnoringSigterm(t)
	var written uint64
	readExitMark = func(procs []utils.Reached) utils.ExitMark {
		m := utils.ExitMark{IO: map[int]uint64{}}
		for _, p := range procs {
			written += 4096
			m.Running++
			m.Threads++
			m.IO[p.PID] = written
			m.OnStorage = true
		}
		return m
	}
	defer func() { readExitMark = utils.MarkExit }()
	ms := handedMembers([]utils.Reached{r})
	if len(ms) != 1 {
		t.Fatalf("premise: %d members of the process that ignores SIGTERM; want 1", len(ms))
	}
	defer closePidfd(ms[0].fd)
	done := make(chan string, 1)
	go func() { done <- stopMembers(zap.NewNop(), ms, func(member) bool { return false }, nil) }()
	select {
	case why := <-done:
		if why != "" {
			t.Errorf("the stop said %q; want no way out said cut, of a process that ignored SIGTERM", why)
		}
	case <-time.After(30 * time.Second):
		_ = syscall.Kill(r.PID, syscall.SIGKILL)
		<-done
		t.Fatal("the stop had not returned 30 s after it began: it took what the process that ignores SIGTERM did for progress on a way out")
	}
	if !ms[0].exited() {
		t.Error("the stop returned and the process that ignores SIGTERM runs")
	}
}

// From its kill on, the stop judges each process on its way out under the
// kill: one that ignored SIGTERM, whose doings were no progress before, is
// waited for while it makes progress after its kill (here its threads end,
// one at each check, the kill withheld as where the kernel holds the
// process), and only then left to the kernel.
func TestAKilledProcessThatIgnoredSigtermIsWaitedForWhileItGetsOut(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	kill := killMembers
	killMembers = func([]member) {}
	defer func() { killMembers = kill }()
	r := startIgnoringSigterm(t)
	key := [2]uint64{uint64(r.PID), r.Start}
	defer leftToKernel.Delete(key)
	const steps = 100
	reads := 0
	readExitMark = func(procs []utils.Reached) utils.ExitMark {
		m := utils.ExitMark{IO: map[int]uint64{}}
		for range procs {
			reads++
			m.Running++
			m.Fds++
			m.Threads += 1 + steps - min(reads, steps)
		}
		return m
	}
	defer func() { readExitMark = utils.MarkExit }()
	ms := handedMembers([]utils.Reached{r})
	if len(ms) != 1 {
		t.Fatalf("premise: %d members of the process that ignores SIGTERM; want 1", len(ms))
	}
	defer closePidfd(ms[0].fd)
	done := make(chan string, 1)
	go func() { done <- stopMembers(zap.NewNop(), ms, func(member) bool { return false }, nil) }()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("the stop had not returned 1m after it began")
	}
	if _, held := leftToKernel.Load(key); !held {
		t.Fatal("premise: the stop did not leave the process, whose kill was withheld, to the kernel")
	}
	if reads < steps {
		t.Fatalf("the stop left the process to the kernel after %d reads of it since its kill, while its threads still ended at each (%d of them): it judged its way out against a mark of before the kill", reads, steps)
	}
}

// startIgnoringSigterm starts a sleep that ignores SIGTERM (a shell's trap,
// which the exec it ends in keeps), in a process group of its own, which
// its cleanup kills; it is what the interrupt reached, ignoring it.
func startIgnoringSigterm(t *testing.T) utils.Reached {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; exec sleep 600")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	await(t, "the sleep that ignores SIGTERM", func() bool {
		comm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
		return err == nil && strings.TrimSpace(string(comm)) == "sleep" && ignoresSigterm(pid)
	})
	st, ok := utils.ReadProcStat(pid)
	if !ok {
		t.Fatalf("the sleep %d cannot be read", pid)
	}
	return utils.Reached{PID: pid, Start: st.StartTime, Ignores: true}
}
