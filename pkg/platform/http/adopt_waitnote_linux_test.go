//go:build linux

package http

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A process that handles the SIGTERM the stop asks it to stop with, and then
// writes to storage for good (read so at every check), holds the stop for as
// long as it runs: it is never killed for want of progress. The wait is never
// silent: once the stall window is over, and then every stopSayEvery, the
// stop says which process it waits for, by its pid and command, that it is
// still writing, and that Ctrl+C ends the wait; of a member gone (one its
// SIGTERM ended), it says nothing.
func TestAStopThatWaitsOnAWriterSaysWhichProcessAndHowToEndIt(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stopCheck, stopStallChecks, stopSayEvery = 20*time.Millisecond, 10, time.Second
	const script = "trap 'echo asked' TERM; while :; do sleep 0.05; done"
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	await(t, "the shell that handles SIGTERM", func() bool { return handlesSigterm(pid) })
	st, ok := utils.ReadProcStat(pid)
	if !ok {
		t.Fatalf("the shell %d cannot be read", pid)
	}
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
	// One the stop's SIGTERM ends at once, which no line names.
	gone := exec.Command("/bin/sleep", "60")
	if err := gone.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = gone.Process.Kill()
		_ = gone.Wait()
	})
	gst, ok := utils.ReadProcStat(gone.Process.Pid)
	if !ok {
		t.Fatalf("the sleep %d cannot be read", gone.Process.Pid)
	}
	// The shell, the sleep it runs now, and the sleep its SIGTERM ends.
	ms := handedMembers([]utils.Reached{{PID: pid, Start: st.StartTime, Ignores: true}, {PID: gone.Process.Pid, Start: gst.StartTime, Ignores: true}})
	if len(ms) < 2 || ms[0].pid != pid || ms[len(ms)-1].pid != gone.Process.Pid {
		t.Fatalf("premise: the members are %v; want the shell %d first and the sleep %d last", ms, pid, gone.Process.Pid)
	}
	defer func() {
		for _, m := range ms {
			closePidfd(m.fd)
		}
	}()
	core, logs := observer.New(zap.InfoLevel)
	began := time.Now()
	done := make(chan string, 1)
	go func() { done <- stopMembers(zap.New(core), ms, func(member) bool { return false }, nil) }()
	const note = "keploy is still waiting for the daemons the application left running"
	deadline := time.Now().Add(30 * time.Second)
	for logs.FilterMessageSnippet(note).Len() < 2 && time.Now().Before(deadline) {
		select {
		case why := <-done:
			t.Fatalf("the stop returned (%q) while the shell it waits for ran, writing", why)
		case <-time.After(10 * time.Millisecond):
		}
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("the stop had not returned 1m after the shell was killed")
	}
	lines := logs.FilterMessageSnippet(note).All()
	if len(lines) < 2 {
		t.Fatalf("the stop said what it waits for %d times in 30 s, past a stall window of %s and every %s; want at least twice", len(lines), stopStall(), stopSayEvery)
	}
	if n := logs.FilterMessageSnippet("made no progress on its way out after it was asked to stop").Len(); n > 0 {
		t.Errorf("the stop killed the shell for want of progress while it wrote to storage at every check (%d lines)", n)
	}
	if first := lines[0].Time.Sub(began); first < stopStall() || first >= stopStall()+stopSayEvery {
		t.Errorf("the stop first said what it waits for %s after it began; want once its stall window, %s, is over, at its next checks, not %s later", first, stopStall(), stopSayEvery)
	}
	// lines[0].Time is stamped by zap after check()'s /proc reads, while n.due
	// is set from now before them, so under load lines[1].Time - lines[0].Time
	// can be a few ms shy of stopSayEvery even when now1 - now0 >= stopSayEvery.
	// Check both wall-clock separation (at least half a period) and lines[1]'s
	// seen_over (computed from now1 - now0, always >= stopSayEvery).
	if gap := lines[1].Time.Sub(lines[0].Time); gap < stopSayEvery/2 {
		t.Errorf("the stop said what it waits for again %s after the first time; want every %s", gap, stopSayEvery)
	}
	if d, err := time.ParseDuration(fmt.Sprint(lines[1].ContextMap()["seen_over"])); err != nil || d < stopSayEvery {
		t.Errorf("the second line says seen_over %q; want at least %s", lines[1].ContextMap()["seen_over"], stopSayEvery)
	}
	for i, l := range lines {
		ctx := l.ContextMap()
		waiting, step := fmt.Sprint(ctx["waiting_for"]), fmt.Sprint(ctx["next_step"])
		if d, err := time.ParseDuration(fmt.Sprint(ctx["seen_over"])); err != nil || d <= 0 {
			t.Errorf("line %d says it saw what each did over %q; want the span of its readings", i+1, ctx["seen_over"])
		}
		if gonePid := "process " + strconv.Itoa(gone.Process.Pid) + " "; strings.Contains(waiting+" ", gonePid) {
			t.Errorf("line %d names %q as what the stop waits for; want no word of the sleep its SIGTERM ended", i+1, waiting)
		}
		for _, want := range []string{"process " + strconv.Itoa(pid) + " (/bin/sh -c " + script + ")", "still reading or writing storage"} {
			if !strings.Contains(waiting, want) {
				t.Errorf("line %d names %q as what the stop waits for; want it to say %q", i+1, waiting, want)
			}
		}
		// No signal began this stop, and keploy killed nothing: a first
		// press stops keploy's run, and only the press after its offer kills.
		if want := endWaitBy(false, false); step != want || !strings.Contains(step, "Ctrl+C") {
			t.Errorf("line %d says %q to end the wait; want %q, how Ctrl+C ends it", i+1, step, want)
		}
	}
}

// A stop's word on what it waits for names no process gone: where each it
// would name is gone, it says nothing.
func TestAWaitNoteSaysNothingOfProcessesGone(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	st, ok := utils.ReadProcStat(cmd.Process.Pid)
	if !ok {
		t.Fatalf("the sleep %d cannot be read", cmd.Process.Pid)
	}
	ms := handedMembers([]utils.Reached{{PID: cmd.Process.Pid, Start: st.StartTime, Ignores: true}})
	if len(ms) != 1 {
		t.Fatalf("premise: %d members of the sleep; want 1", len(ms))
	}
	defer closePidfd(ms[0].fd)
	key := [2]uint64{uint64(ms[0].pid), ms[0].start}
	n := &waitNote{marks: map[[2]uint64]utils.ExitMark{key: {Running: 1, Threads: 1}}, read: time.Now().Add(-time.Second)}
	_ = cmd.Process.Kill()
	await(t, "the sleep gone", ms[0].exited)
	core, logs := observer.New(zap.InfoLevel)
	n.check(zap.New(core), ms, time.Now(), false, false)
	if logs.Len() > 0 {
		t.Errorf("the note said %q of processes all gone; want nothing", logs.All()[0].ContextMap())
	}
}

// The way out of a stop's wait it says is the one that holds: Ctrl+C at
// once while a press kills the processes, Ctrl+C twice before, and none once
// keploy has killed them.
func TestEndWaitBySaysWhatEndsTheWaitNow(t *testing.T) {
	for _, c := range []struct {
		killed, offering bool
		want, not        string
	}{
		{offering: true, want: "press Ctrl+C again to kill them now"},
		{want: "then again once keploy says another press kills them", not: "kill them now"},
		{killed: true, want: "keploy killed them", not: "Ctrl+C"},
		{killed: true, offering: true, want: "keploy killed them", not: "Ctrl+C"},
	} {
		got := endWaitBy(c.killed, c.offering)
		if !strings.Contains(got, c.want) || (c.not != "" && strings.Contains(got, c.not)) {
			t.Errorf("endWaitBy(killed %v, offering %v) = %q; want %q in it, and not %q", c.killed, c.offering, got, c.want, c.not)
		}
	}
}

// What a stop says a process did between two readings is what changed:
// storage its threads did (a new thread's all of it), threads that ended,
// files closed between two settled readings; and no progress where none of
// that changed.
func TestDoneSinceSaysWhatTheProcessDid(t *testing.T) {
	base := utils.ExitMark{Running: 1, Threads: 4, Fds: 10, IO: map[int]uint64{1: 100, 2: 50}}
	with := func(f func(*utils.ExitMark)) utils.ExitMark {
		m := base
		m.IO = map[int]uint64{1: 100, 2: 50}
		f(&m)
		return m
	}
	for _, c := range []struct {
		name string
		now  utils.ExitMark
		want string
	}{
		{"nothing changed", with(func(*utils.ExitMark) {}), "no progress on its way out"},
		{"a thread wrote", with(func(m *utils.ExitMark) { m.IO[1] = 8292 }), "still reading or writing storage (8192 bytes)"},
		{"a new thread wrote", with(func(m *utils.ExitMark) { m.IO[3] = 4096; m.Threads = 5 }), "still reading or writing storage (4096 bytes)"},
		{"a thread is read lower", with(func(m *utils.ExitMark) { m.IO[1] = 10 }), "no progress on its way out"},
		{"threads ended", with(func(m *utils.ExitMark) { m.Threads = 2 }), "ended 2 of its threads"},
		{"a thread started", with(func(m *utils.ExitMark) { m.Threads = 6 }), "no progress on its way out"},
		{"files closed", with(func(m *utils.ExitMark) { m.Fds = 7 }), "closed 3 of its files"},
		{"files read unsettled", with(func(m *utils.ExitMark) { m.Fds = 7; m.FilesUnsettled = true }), "no progress on its way out"},
		{"all of it", with(func(m *utils.ExitMark) { m.IO[2] = 60; m.Threads = 3; m.Fds = 9 }), "still reading or writing storage (10 bytes), ended 1 of its threads, closed 1 of its files"},
	} {
		if got := doneSince(base, c.now); got != c.want {
			t.Errorf("%s: doneSince = %q; want %q", c.name, got, c.want)
		}
	}
	unsettled := base
	unsettled.FilesUnsettled = true
	if got := doneSince(unsettled, with(func(m *utils.ExitMark) { m.Fds = 7 })); got != "no progress on its way out" {
		t.Errorf("files fewer than an unsettled reading: doneSince = %q; want no progress", got)
	}
}

// A process is named by its pid and command line, cut to a line's length,
// or by its pid alone once it is gone.
func TestCommandOfNamesThePidAndCommand(t *testing.T) {
	long := strings.Repeat("x", 300)
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 60", long)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	await(t, "the sleep", func() bool {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		return err == nil && strings.HasPrefix(string(b), "sleep\x00")
	})
	if got, want := commandOf(pid), "process "+strconv.Itoa(pid)+" (sleep 60)"; got != want {
		t.Errorf("commandOf(sleep) = %q; want %q", got, want)
	}
	// A shell that runs on after its sleep, so its own command line, with
	// the long $0, is what is read.
	long2 := exec.Command("/bin/sh", "-c", "sleep 60; :", long)
	long2.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := long2.Start(); err != nil {
		t.Fatal(err)
	}
	lp := long2.Process.Pid
	end := func() {
		_ = syscall.Kill(-lp, syscall.SIGKILL)
		_ = long2.Wait()
	}
	t.Cleanup(end)
	await(t, "the long shell", func() bool {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(lp) + "/cmdline")
		return err == nil && strings.HasPrefix(string(b), "/bin/sh\x00")
	})
	zombie := exec.Command("/bin/sh", "-c", "exec sleep 0")
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zombie.Wait() })
	zp := zombie.Process.Pid
	await(t, "the zombie", func() bool {
		st, ok := utils.ReadProcStat(zp)
		return ok && st.State == 'Z'
	})
	if got, want := commandOf(zp), "process "+strconv.Itoa(zp)+" (sleep)"; got != want {
		t.Errorf("commandOf(a zombie, with no command line) = %q; want %q, by its comm", got, want)
	}
	if got := commandOf(lp); len(got) > 200 || !strings.HasSuffix(got, "...)") || !strings.HasPrefix(got, "process "+strconv.Itoa(lp)+" (/bin/sh -c sleep 60; : x") {
		t.Errorf("commandOf(a long command) = %q; want it cut to a line's length", got)
	}
	end()
	if got, want := commandOf(lp), "process "+strconv.Itoa(lp); got != want {
		t.Errorf("commandOf(a process gone) = %q; want %q", got, want)
	}
}

// keploy's interrupt leaves to the stop of the daemons only a process that
// stop ends: a daemon this process adopted, or one the kernel holds after a
// stop's kill. A child of keploy's own (one it holds a pidfd on), a zombie,
// and any process where keploy adopts nothing, it waits for itself.
func TestTheInterruptLeavesToTheDaemonStopOnlyWhatItEnds(t *testing.T) {
	if err := goUsesPidfds(); err != nil {
		t.Skipf("keploy adopts nothing here: %v", err)
	}
	was := adopting.Load()
	t.Cleanup(func() { adopting.Store(was) })
	adopting.Store(true)
	fork := func(argv ...string) int {
		t.Helper()
		pid, err := syscall.ForkExec(argv[0], argv, &syscall.ProcAttr{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(pid, &ws, 0, nil)
		})
		return pid
	}
	startOf := func(pid int) uint64 {
		t.Helper()
		st, ok := utils.ReadProcStat(pid)
		if !ok {
			t.Fatalf("process %d cannot be read", pid)
		}
		return st.StartTime
	}
	daemon := fork("/bin/sleep", "60")
	await(t, "the daemon running", func() bool {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(daemon) + "/comm")
		return err == nil && strings.TrimSpace(string(b)) == "sleep"
	})
	start := startOf(daemon)
	if !endsWithTheDaemons(daemon, start) {
		t.Fatal("a daemon keploy adopted (no pidfd of its own names it) is not left to the stop of the daemons")
	}
	if endsWithTheDaemons(daemon, start+1) {
		t.Error("a process whose start time is not the one the interrupt read is left to the stop of the daemons")
	}
	key := [2]uint64{uint64(daemon), start}
	leftToKernel.Store(key, true)
	if !endsWithTheDaemons(daemon, start) {
		t.Error("a daemon a stop killed that the kernel holds is not left to the stop of the daemons")
	}
	leftToKernel.Delete(key)
	own := exec.Command("/bin/sleep", "60")
	if err := own.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = own.Process.Kill()
		_ = own.Wait()
	})
	if endsWithTheDaemons(own.Process.Pid, startOf(own.Process.Pid)) {
		t.Error("a child of keploy's own (it holds a pidfd on it) is left to the stop of the daemons")
	}
	zombie := fork("/bin/true")
	zstart := startOf(zombie)
	await(t, "the zombie", func() bool {
		st, ok := utils.ReadProcStat(zombie)
		return ok && st.State == 'Z'
	})
	if endsWithTheDaemons(zombie, zstart) {
		t.Error("a zombie is left to the stop of the daemons")
	}
	adopting.Store(false)
	if endsWithTheDaemons(daemon, start) {
		t.Error("where keploy adopts nothing, a process is left to the stop of the daemons")
	}
}

// handlesSigterm reports whether process pid catches SIGTERM (its SigCgt).
func handlesSigterm(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "SigCgt:"); ok {
			mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			return err == nil && mask&(1<<(uint(syscall.SIGTERM)-1)) != 0
		}
	}
	return false
}
