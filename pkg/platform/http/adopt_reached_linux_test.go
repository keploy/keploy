//go:build linux

package http

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/starvetest"
	"go.keploy.io/server/v3/pkg/client/app"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// stopHelperEnv names what a stop helper (TestStopHelperProcess) is; the
// directory it writes in is stopHelperDirEnv; stopHelperHeldEnv, set, says
// the test holds it off the CPU (see TestStopHelperProcess).
const (
	stopHelperEnv     = "KEPLOY_TEST_STOP_HELPER"
	stopHelperDirEnv  = "KEPLOY_TEST_STOP_HELPER_DIR"
	stopHelperHeldEnv = "KEPLOY_TEST_STOP_HELPER_HELD"
)

// TestStopHelperProcess is not a test: it is a process of the application
// the tests below run, as stopHelperEnv names it. Each writes <name>.ready,
// with its pid, once its signals are set up, and a file for each signal that
// reaches it:
//
//   - acting acts on SIGINT, Go's way: notified of it, and of SIGCONT; its
//     SIGTERM is Go's default, which ends it, as a second signal ends an
//     application whose signal.NotifyContext's stop was called. Once it has
//     had SIGINT it writes acting.int, waits for the SIGCONT keploy's stop of
//     what the application left sends, and writes acting.done as it exits.
//   - ignoring ignores SIGINT, as an application started in the background
//     of a shell does (POSIX), and stops on SIGTERM: ignoring.term.
//   - daemon stops on SIGINT (daemon.int) or SIGTERM (daemon.term).
//   - mapped ignores SIGINT and SIGTERM, as a server daemon with no stop of
//     its own does, and sleeps; its exit takes the kernel tens of
//     milliseconds of CPU (starvetest.HeavyExit), which a killed one held off
//     the CPU does not get in a test's window.
//
// Held (stopHelperHeldEnv), each spins at the start of its way out, the
// signal that starts it had, until the test writes released in its directory
// (starvetest.Spin): a hold slows a process to a trickle of turns, and a way
// out as short as these would otherwise get through on them.
func TestStopHelperProcess(t *testing.T) {
	name := os.Getenv(stopHelperEnv)
	if name == "" {
		t.Skip("a process of the stop tests' application")
	}
	dir := os.Getenv(stopHelperDirEnv)
	note := func(file string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(3)
		}
	}
	held := os.Getenv(stopHelperHeldEnv) != ""
	// wayOut starts the helper's way out: held, once the test releases it.
	wayOut := func() {
		if held {
			starvetest.Spin(filepath.Join(dir, "released"))
		}
	}
	switch name {
	case "acting":
		ints, conts := make(chan os.Signal, 1), make(chan os.Signal, 1)
		signal.Notify(ints, syscall.SIGINT)
		signal.Notify(conts, syscall.SIGCONT)
		note(name + ".ready")
		<-ints
		wayOut()
		note(name + ".int")
		<-conts
		note(name + ".done")
	case "ignoring":
		signal.Ignore(syscall.SIGINT)
		terms := make(chan os.Signal, 1)
		signal.Notify(terms, syscall.SIGTERM)
		note(name + ".ready")
		<-terms
		wayOut()
		note(name + ".term")
	case "hangs":
		// Acts on SIGINT, ignores SIGTERM, and never gets out.
		signal.Ignore(syscall.SIGTERM)
		ints := make(chan os.Signal, 1)
		signal.Notify(ints, syscall.SIGINT)
		note(name + ".ready")
		<-ints
		note(name + ".int")
		select {}
	case "daemon":
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		note(name + ".ready")
		s := <-sigs
		wayOut()
		if s == syscall.SIGINT {
			note(name + ".int")
		} else {
			note(name + ".term")
		}
	case "mapped":
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
		if err := starvetest.HeavyExit(mappedHelperMaps); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		note(name + ".ready")
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(0)
}

// mappedHelperMaps is how many mappings the mapped stop helper's exit tears
// down.
const mappedHelperMaps = 60000

// stopHelperBinary is a copy of this test binary, to run the stop helpers
// from: what runs this process's own executable is keploy's, which the stop
// of what the application left never stops (adoptedRunning). On storage, as
// an application's program is; for a test of processes held off the CPU
// (held), on a memory filesystem where one lets it run (starvetest.ExecDir):
// a page of it read back from storage holds a helper in the kernel, and a
// test of the CPU is then one of the disk too.
func stopHelperBinary(t *testing.T, held bool) string {
	t.Helper()
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if held {
		dir = starvetest.ExecDir(t, int64(len(self)))
	}
	bin := filepath.Join(dir, "stop-helper")
	if err := os.WriteFile(bin, self, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// helperCommand is the shell words that run stop helper name from bin,
// writing in dir, held by the test or not. A helper built with the race
// detector would sleep a second on its way out (GORACE's atexit_sleep_ms), a
// way out the stop rightly judges stalled at the tests' bound: it exits as it
// would without it.
func helperCommand(bin, name, dir string, held bool) string {
	env := ""
	if held {
		env = stopHelperHeldEnv + "=1 "
	}
	return fmt.Sprintf(`env GORACE="${GORACE:+$GORACE }atexit_sleep_ms=0" %s%s=%s %s=%s %s -test.run='^TestStopHelperProcess$'`, env, stopHelperEnv, name, stopHelperDirEnv, dir, bin)
}

// helperPid is the pid stop helper name wrote in dir as it got ready.
func helperPid(t *testing.T, dir, name string) int {
	t.Helper()
	var pid int
	await(t, name+" ready", func() bool {
		b, err := os.ReadFile(filepath.Join(dir, name+".ready"))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(string(b))
		return err == nil
	})
	return pid
}

// newRunClient is an agent client whose application is command, run as keploy
// runs a native one.
func newRunClient(command string) *AgentClient {
	cfg := &config.Config{}
	cfg.Agent.AgentURI = "http://127.0.0.1:1/agent"
	client := New(zap.NewNop(), nil, cfg)
	client.apps.Store(uint64(0), app.NewApp(zap.NewNop(), command, nil,
		models.SetupOptions{CommandType: string(utils.Native)}))
	return client
}

// keploy's stop of the application interrupts it (SIGINT to each of its
// process groups) and waits for each group's leader. A shell that waits on
// the app it started in the background (`app & wait`) dies on the interrupt
// at once, and the app, orphaned, is one of the processes the run adopted,
// which the stop of those ends (stopAdopted). One the interrupt reached that
// acts on it is stopping already, and is not asked again: a second signal
// ends many an application mid-shutdown (Go's signal.NotifyContext, once its
// stop is called), and the set's end would be judged on a shutdown keploy
// cut. One the interrupt reached that ignores it (as a shell starts a
// background command) is asked with SIGTERM, as is a daemon the interrupt
// did not reach (a session of its own); each ends, and the stop says no kill.
func TestTheStopDoesNotAskAgainWhatActsOnTheInterrupt(t *testing.T) {
	stopOfWhatTheAppLeft(t, false)
}

// The same stop, of processes held off the CPU (a loaded host, a cgroup
// whose quota their siblings spent) from before keploy interrupts the app
// until its stop of them has checked them three times as often as a stall is
// judged on: none of them could run to act on its signal, so none made
// progress, and none is judged for it. Each spins on its way out until the
// test releases it (a hold slows it to a trickle of turns, on which a way out
// this short would get through), and had a fraction of the stall bound of CPU
// meanwhile, or the test held nothing. Released, each ends, and the stop says
// no kill. Counted in checks, as before, each was killed for want of progress
// it could not make.
func TestTheStopDoesNotJudgeWhatIsHeldOffTheCPU(t *testing.T) {
	stopOfWhatTheAppLeft(t, true)
}

// stopOfWhatTheAppLeft runs the stop of an app that leaves a process acting
// on keploy's interrupt, one ignoring it, and a daemon, held off the CPU
// while the stop checks them for three times the checks a stall is judged on
// (held), or not.
func stopOfWhatTheAppLeft(t *testing.T, held bool) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	var looks atomic.Uint64
	looked := func() { looks.Add(1) }
	stopLooked.Store(&looked)
	defer stopLooked.Store(nil)
	// What the helpers write on their way out is on storage, as an
	// application's files are: a loaded host's disk holds them in the
	// kernel as they write, which the stop takes for a wait while it
	// serves. Held, on none: the test is of the CPU.
	dir := t.TempDir()
	if held {
		dir = starvetest.MemDir(t)
	}
	bin := stopHelperBinary(t, held)
	client := newRunClient(fmt.Sprintf("(setsid %s &); %s & %s & wait",
		helperCommand(bin, "daemon", dir, held), helperCommand(bin, "acting", dir, held), helperCommand(bin, "ignoring", dir, held)))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	pids := map[string]int{}
	for _, name := range []string{"acting", "ignoring", "daemon"} {
		pid := helperPid(t, dir, name)
		pids[name] = pid
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	}
	// The daemon is adopted before the stop: not the tree's, nor its groups'.
	await(t, "the daemon adopted", func() bool { return parentIs(pids["daemon"], os.Getpid()) })
	exists := func(file string) bool { _, err := os.Stat(filepath.Join(dir, file)); return err == nil }
	release := func() {}
	// What the helpers ran on a CPU, together.
	ran := func() time.Duration {
		var all time.Duration
		for _, pid := range pids {
			r, _ := starvetest.Ran(pid)
			all += r
		}
		return all
	}
	var ranBefore time.Duration
	if held {
		release = starvetest.Hold(t, pids["acting"], pids["ignoring"], pids["daemon"])
		ranBefore = ran()
	}
	cancel(models.ErrAppStoppedForSetEnd)
	var appErr models.AppError
	ended := false
	if held {
		// Held across three times the checks a stall is judged on, or until
		// the stop ends: one that judged them ends with a kill.
		for !ended && looks.Load() < 3*stopStallChecks {
			select {
			case appErr = <-done:
				ended = true
			case <-time.After(stopCheck):
			}
		}
		heldRan := ran() - ranBefore
		var out []string
		for _, file := range []string{"acting.int", "acting.done", "ignoring.term", "daemon.term"} {
			if exists(file) {
				out = append(out, file)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "released"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		release()
		// What the stop read of them was the checks of processes that could
		// not run: none got out, and they had far less CPU than the stall
		// bound it judges on.
		if len(out) > 0 {
			t.Fatalf("%v written while their helpers were held off the CPU, before the test released them (pids %v)", out, pids)
		}
		if heldRan >= stopStall()/2 {
			t.Fatalf("the helpers held off the CPU ran %v while held, half the stall bound (%v) or more: the test held nothing (pids %v)", heldRan, stopStall(), pids)
		}
	}
	if !ended {
		appErr = <-done
	}
	if appErr.StopKilled != "" {
		t.Errorf("the stop killed: %q", appErr.StopKilled)
	}
	for file, want := range map[string]bool{
		"acting.int": true, "acting.done": true,
		"ignoring.term": true,
		"daemon.term":   true, "daemon.int": false,
	} {
		if got := exists(file); got != want {
			t.Errorf("%s written %v, want %v (pids %v)", file, got, want, pids)
		}
	}
	for name, pid := range pids {
		if parentIs(pid, os.Getpid()) {
			t.Errorf("%s (%d) outlived the stop", name, pid)
		}
	}
}

// A daemon the stop killed is waited for while it is held off the CPU, which
// it needs to get out: a kill is a wake, and the dying process must run. It
// ignores keploy's signals and sleeps, so the stop kills it for want of
// progress once it and keploy could run for the stall bound from the stop's
// first check (on a loaded host, keploy runs less often than it checks, and
// that takes more checks). Held from its kill across the checks after which
// a stop counted in checks went on without it, leaving it to the kernel
// (stopStallChecks), it is no process the kernel holds without progress, and
// the stop never leaves it so; released, it is gone, and the stop with it.
//
// A hold slows a process to a trickle of turns; it does not freeze one, and
// a dying one cannot be made to wait for the test. Its exit takes the kernel
// tens of milliseconds of CPU (the mapped helper), so it mostly outlasts
// those checks; where it gets out sooner, the stop that ended on its exit is
// right, and the test says that this run could not tell it from a stop
// counted in checks. Whether the stop left it to the kernel is read from
// what the stop marks so (leftToKernel), not from what is left once it ended:
// a daemon leaving the kernel as the stop gives up on it is still caught.
func TestAKilledDaemonHeldOffTheCPUIsWaitedForUntilGone(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	var looks atomic.Uint64
	looked := func() { looks.Add(1) }
	stopLooked.Store(&looked)
	defer stopLooked.Store(nil)
	dir := starvetest.MemDir(t)
	bin := stopHelperBinary(t, true)
	client := newRunClient(fmt.Sprintf("(setsid %s &); exec sleep 600", helperCommand(bin, "mapped", dir, false)))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	pid := helperPid(t, dir, "mapped")
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	await(t, "the daemon adopted", func() bool { return parentIs(pid, os.Getpid()) })
	release := starvetest.Hold(t, pid)
	// The time both the daemon and keploy could run (utils.ExitClock): what
	// the stop judges its stall on. Held and asleep, the daemon could run
	// all of it, and keploy only what a loaded host gives it, which its
	// checks outpace.
	proc, ok := utils.ProcOf(pid)
	if !ok {
		t.Fatalf("the daemon (%d) was gone before the stop", pid)
	}
	could, releaseClock := utils.NewExitClock([]utils.Reached{proc}, stopStall())
	defer releaseClock()
	cancel(models.ErrAppStoppedForSetEnd)
	ended := false
	// The stop of what the app left starts once the app's own stop is over,
	// which on a loaded host takes keploy a while: its stall is counted from
	// its first check, and so is this clock, which then spans no more of the
	// time than the stop's.
	for !ended && looks.Load() == 0 {
		select {
		case <-done:
			ended = true
		case <-time.After(stopCheck):
		}
	}
	from := could.Runnable()
	// Until its kill: a stop that never kills a daemon that makes no
	// progress fails here, once it and keploy could run for three times the
	// stall it is judged on.
	for !ended && !killedOrGone(pid) {
		if stalled := could.Runnable() - from; stalled >= 3*stopStall() {
			release()
			t.Fatalf("the stop did not kill the daemon (%d) that made no progress for %v of the time both it and keploy could run, three times the stall it is judged on (%v)", pid, stalled, stopStall())
		}
		select {
		case <-done:
			ended = true
		case <-time.After(stopCheck):
		}
	}
	// Held from its kill across the checks after which a stop counted in
	// checks went on without it, and two more for the test's own count of
	// them, which may lag the stop's by one.
	killedAt := looks.Load()
	gotOut := uint64(0) // the checks after its kill by which it was gone
	for !ended && looks.Load()-killedAt < stopStallChecks+2 {
		select {
		case <-done:
			ended = true
		case <-time.After(stopCheck):
		}
		if gotOut == 0 && !threadsLeft(pid) {
			gotOut = max(looks.Load()-killedAt, 1)
		}
	}
	release()
	if !ended {
		<-done
	}
	if _, left := leftToKernel.Load([2]uint64{uint64(pid), proc.Start}); left {
		t.Fatalf("the stop went on without the daemon (%d) it killed while it was held off the CPU, leaving it to the kernel", pid)
	}
	if threadsLeft(pid) {
		t.Errorf("the daemon (%d) outlived the stop", pid)
	}
	if gotOut > 0 && gotOut < stopStallChecks {
		t.Logf("the daemon (%d) got out %d checks after its kill, its exit's %d mappings torn down on the turns a hold still gives it: before a stop counted in checks would have gone on without it, so this run could not tell one from the stop", pid, gotOut, mappedHelperMaps)
	}
}

// killedOrGone reports whether process pid has had a SIGKILL: one pending
// (it waits for a CPU to act on it), or a thread of it exiting (one that had
// a turn and acted on it: PF_EXITING in its flags), or its leader exited, or
// it is gone.
func killedOrGone(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return true
	}
	if threadExiting(pid) {
		return true
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch k {
		case "State":
			if strings.HasPrefix(strings.TrimSpace(v), "Z") {
				return true
			}
		case "SigPnd", "ShdPnd":
			if mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64); err == nil && mask&(1<<(syscall.SIGKILL-1)) != 0 {
				return true
			}
		}
	}
	return false
}

// threadExiting reports whether a thread of process pid is on its way out of
// the kernel's exit: PF_EXITING (0x4) in the flags of its stat (field 9).
func threadExiting(pid int) bool {
	tasks, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
	if err != nil {
		return false
	}
	for _, task := range tasks {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/task/" + task.Name() + "/stat")
		if err != nil {
			continue
		}
		// After the command's closing parenthesis: state, ppid, pgrp,
		// session, tty, tpgid, flags.
		end := strings.LastIndexByte(string(b), ')')
		if end < 0 {
			continue
		}
		f := strings.Fields(string(b[end+1:]))
		if len(f) < 7 {
			continue
		}
		if flags, err := strconv.ParseUint(f[6], 10, 64); err == nil && flags&0x4 != 0 {
			return true
		}
	}
	return false
}

// threadsLeft reports whether a thread of process pid is still there, not
// exited: a process whose leader has exited is still there while another
// thread of it is.
func threadsLeft(pid int) bool {
	tasks, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
	if err != nil {
		return false
	}
	for _, task := range tasks {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/task/" + task.Name() + "/stat")
		if err != nil {
			continue
		}
		if st, ok := utils.ParseProcStat(b); ok && st.State != 'Z' && st.State != 'X' {
			return true
		}
	}
	return false
}

// commIs reports whether process pid runs command comm.
func commIs(pid int, comm string) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	return err == nil && strings.TrimSpace(string(b)) == comm
}

// When keploy's stop had to kill the application itself, the run says that
// kill, not a later one of a daemon the application left: the first kill is
// what cut the set's end.
func TestTheAppsKillIsSaidBeforeADaemonsStallKill(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	budget := utils.StopStallBudget
	utils.StopStallBudget = 300 * time.Millisecond
	t.Cleanup(func() { utils.StopStallBudget = budget })
	dir := t.TempDir()
	daemonPid, ready := filepath.Join(dir, "daemon"), filepath.Join(dir, "ready")
	client := newRunClient(fmt.Sprintf(`(setsid sh -c 'trap "" INT TERM; echo $$ > %s; exec sleep 600' &); trap "" INT; touch %s; exec sleep 600`, daemonPid, ready))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	await(t, "the app ready", func() bool { _, err := os.Stat(ready); return err == nil })
	var pid int
	await(t, "the daemon adopted", func() bool {
		b, err := os.ReadFile(daemonPid)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && parentIs(pid, os.Getpid())
	})
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel(models.ErrAppStoppedForSetEnd)
	appErr := <-done
	if !strings.Contains(appErr.StopKilled, "after keploy interrupted it, so keploy killed it") || strings.Contains(appErr.StopKilled, "which the application left running") {
		t.Fatalf("the run said %q; want the app's kill, and only it", appErr.StopKilled)
	}
	if parentIs(pid, os.Getpid()) {
		t.Errorf("the daemon (%d) outlived the stop", pid)
	}
}

// A process acting on keploy's interrupt that then never gets out is killed
// for want of progress, and the kill is said, though it ignores SIGTERM: it
// was on its way out, and the kill cut it.
func TestAKillOfOneActingOnTheInterruptIsSaid(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	dir := t.TempDir()
	bin := stopHelperBinary(t, false)
	client := newRunClient(fmt.Sprintf("%s & wait", helperCommand(bin, "hangs", dir, false)))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	pid := helperPid(t, dir, "hangs")
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel(models.ErrAppStoppedForSetEnd)
	appErr := <-done
	if !strings.Contains(appErr.StopKilled, "made no progress on its way out") {
		t.Fatalf("the run said %q; want the kill of the process acting on the interrupt said", appErr.StopKilled)
	}
}

// Whether a daemon ignores SIGTERM is read as keploy asks it to stop with it:
// one whose handler then ignores further TERMs while it shuts down, and that
// stalls on its way out, had a way out, and its kill is said.
func TestAKillOfOneThatIgnoredTermOnlyOnItsWayOutIsSaid(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	dir := t.TempDir()
	script := filepath.Join(dir, "daemon.sh")
	if err := os.WriteFile(script, []byte(`trap 'trap "" TERM; while :; do sleep 1; done' TERM
touch "$1"
while :; do sleep 1; done
`), 0o755); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(dir, "ready")
	client := newRunClient(fmt.Sprintf("sh %s %s & wait", script, ready))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	await(t, "the daemon ready", func() bool { _, err := os.Stat(ready); return err == nil })
	cancel(models.ErrAppStoppedForSetEnd)
	appErr := <-done
	if !strings.Contains(appErr.StopKilled, "made no progress on its way out") {
		t.Fatalf("the run said %q; want the kill of a daemon that was on its way out said", appErr.StopKilled)
	}
}

// The stop at a set's end of the daemons an application left moves the
// teardown of the run that ran it (AgentClient.Run hands stopAdopted its
// context's scope) at each step they take on their way out: that run's
// drains wait on them. The app itself ends at its interrupt, at once.
func TestTheDaemonsStepsOutMoveTheTeardownOfTheRunThatRanTheApp(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 100
	dir := t.TempDir()
	daemon := filepath.Join(dir, "daemon.sh")
	if err := os.WriteFile(daemon, []byte(`exec 3</dev/null 4</dev/null 5</dev/null 6</dev/null 7</dev/null
trap 'for fd in 3 4 5 6 7; do eval "exec $fd<&-"; sleep 0.3; done; exit 0' TERM
echo $$ > "$1"
while :; do sleep 0.05; done
`), 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "daemon.pid")
	client := newRunClient(fmt.Sprintf("(setsid sh %s %s &); exec sleep 600", daemon, pidFile))
	runCtx, scope := utils.NewTeardownScope(context.Background())
	ctx, cancel := context.WithCancelCause(runCtx)
	defer cancel(nil)
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	var pid int
	await(t, "the daemon started", func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 0
	})
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	await(t, "the daemon adopted", func() bool { return parentIs(pid, os.Getpid()) })
	cancel(models.ErrAppStoppedForSetEnd)
	if appErr := <-done; appErr.StopKilled != "" {
		t.Fatalf("the stop killed: %q", appErr.StopKilled)
	}
	// Five files closed on its way out, each a step its stop looks at.
	if n := scope.Moves(); n < 4 {
		t.Fatalf("the daemon's steps out moved the teardown of the run that ran the app %d times, want 4 or more", n)
	}
}
