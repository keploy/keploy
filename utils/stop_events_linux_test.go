//go:build linux

package utils

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"go.keploy.io/server/v3/internal/starvetest"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// shortStall sets StopStallBudget to d for the test.
func shortStall(t *testing.T, d time.Duration) {
	t.Helper()
	budget := StopStallBudget
	StopStallBudget = d
	t.Cleanup(func() { StopStallBudget = budget })
}

// readyLeader starts command (sh -c, in dir) as a group leader, as keploy
// starts a native app, once it has written dir/ready.
func readyLeader(t *testing.T, dir, command string) (pid int, reaped <-chan struct{}) {
	t.Helper()
	cmd, r := startGroupLeader(t, "sh", "-c", "cd "+dir+" && "+command)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the app did not start")
		}
	}
	return cmd.Process.Pid, r
}

// Under the switch the stop waits for the app's exit event, not a number of
// seconds: an app whose shutdown takes longer than the old grace (3 s), here
// sleeping 4 s on its way out, is no kill; nor is one that keeps closing what
// it holds past the stall bound, as that is progress. One that makes no
// progress (it ignores the interrupt) is killed once the stall bound of the
// time both it and keploy could run has passed, and the stop says so; it is
// stopped once it has settled into the command it ends in (settles), asleep,
// as a step it takes on its way there (its touch exiting, its exec) is one
// the stop rightly takes for progress.
func TestAStopWaitsForTheAppsExitAndKillsOnlyOnAStall(t *testing.T) {
	for _, tc := range []struct {
		name, command, settles string
		stall                  time.Duration
		killed                 bool
	}{
		{"a shutdown longer than the old grace", `trap 'sleep 4; exit 0' INT; touch ready; while :; do sleep 0.2; done`, "", StopStallBudget, false},
		{"a shutdown that keeps making progress past the stall bound", `exec 3</dev/null 4</dev/null 5</dev/null 6</dev/null 7</dev/null 8</dev/null
trap 'for fd in 3 4 5 6 7 8; do eval "exec $fd<&-"; sleep 0.3; done; exit 0' INT; touch ready; while :; do sleep 0.1; done`, "", 500 * time.Millisecond, false},
		{"one that ignores the interrupt", `trap "" INT; touch ready; exec sleep 600`, "sleep", 300 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortStall(t, tc.stall)
			pid, reaped := readyLeader(t, t.TempDir(), tc.command)
			if tc.settles != "" {
				asleep(t, Reached{PID: pid}, tc.settles)
			}
			scope := &TeardownScope{}
			killed, _, err := InterruptProcessTreeStop(zap.NewNop(), pid, syscall.SIGINT, scope)
			if err != nil {
				t.Fatal(err)
			}
			// Its steps out move the run's teardown, which the run's drains
			// wait on; one that ignores the interrupt takes none.
			if tc.name == "a shutdown that keeps making progress past the stall bound" && scope.Moves() == 0 {
				t.Fatal("a shutdown that kept making progress moved the run's teardown not once")
			}
			if tc.killed && scope.Moves() != 0 {
				t.Fatalf("an app that ignored the interrupt moved the run's teardown %d times", scope.Moves())
			}
			if (killed != "") != tc.killed {
				t.Fatalf("the stop said %q; want a kill %v", killed, tc.killed)
			}
			if tc.killed && !strings.Contains(killed, "made no progress on its way out for "+tc.stall.String()+" of its and keploy's runnable time") {
				t.Fatalf("the kill is said as %q; want the stall rule named", killed)
			}
			awaitReaped(t, tc.name, reaped)
		})
	}
}

// An app held off the CPU (a loaded host, a cgroup whose quota its siblings
// spent) from before keploy interrupts it, until the stop has looked at it
// three times as often as a stall is judged on, could not run to act on the
// interrupt: it made no progress, and is not judged for it. Its way out spins
// until the test releases it (a hold slows it to a trickle of turns, on which
// a way out this short would get through), and it had a fraction of the
// stall bound of CPU meanwhile, or the test held nothing. Released, it exits
// on its interrupt, and the stop says no kill. Counted in keploy's runnable
// time alone, it was killed for want of progress it could not make.
func TestAStopDoesNotJudgeAnAppHeldOffTheCPU(t *testing.T) {
	starvetest.TakesACPU(t)
	const bound = 300 * time.Millisecond
	shortStall(t, bound)
	var looks atomic.Uint64
	looked := func() { looks.Add(1) }
	exitLooked.Store(&looked)
	defer exitLooked.Store(nil)
	dir := starvetest.MemDir(t)
	pid, reaped := readyLeader(t, dir, `trap '`+starvetest.ShellSpin("[ ! -e released ]")+`; touch out; exit 0' INT; touch ready; `+starvetest.ShellSpin(""))
	release := starvetest.Hold(t, pid)
	ranBefore, _ := starvetest.Ran(pid)
	type stopped struct {
		killed string
		err    error
	}
	done := make(chan stopped, 1)
	go func() {
		killed, _, err := InterruptProcessTreeStop(zap.NewNop(), pid, syscall.SIGINT, nil)
		done <- stopped{killed, err}
	}()
	var end stopped
	ended := false
	judged := 3 * uint64(bound/stall.CheckEvery(bound))
	for !ended && looks.Load() < judged {
		select {
		case end = <-done:
			ended = true
		case <-time.After(10 * time.Millisecond):
		}
	}
	ranHeld, _ := starvetest.Ran(pid)
	_, outErr := os.Stat(filepath.Join(dir, "out"))
	if err := os.WriteFile(filepath.Join(dir, "released"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	release()
	if outErr == nil {
		t.Fatal("the app got out while it was held off the CPU, before the test released it")
	}
	if ran := ranHeld - ranBefore; ran >= bound/2 {
		t.Fatalf("the app held off the CPU ran %v while held, half the stall bound (%v) or more: the test held nothing", ran, bound)
	}
	if !ended {
		end = <-done
	}
	if end.err != nil {
		t.Fatal(end.err)
	}
	if end.killed != "" {
		t.Fatalf("the stop killed: %q", end.killed)
	}
	awaitReaped(t, "the app", reaped)
}

// An app whose way out waits on a process it starts as it stops (a shell's
// trap, a stop command), held off the CPU, is as far as that process gets:
// it sleeps, waiting, and is not judged for the time the child could not run.
// The child is held from its start: the app is held before it is
// interrupted, so the child its trap starts is started held
// (starvetest.Holding.TakeChild), and the app is let go once it has. The
// child spins until the test releases it, the stop looks at it three times
// as often as a stall is judged on, and it had a fraction of the stall bound
// of CPU from its start, or the test held nothing; let go, it ends, the app
// with it, and the stop says no kill. The stop judges the child from its
// start, so one held only once the test read its pid ran free until then
// and, on a loaded runner, was killed for a stall before the test held it:
// the test takes it once seen, and again only once the stop has looked at it
// as often as the window it judges.
func TestAStopDoesNotJudgeAnAppWaitingOnAChildHeldOffTheCPU(t *testing.T) {
	for _, tc := range []struct {
		name string
		late bool
	}{{"taken once seen", false}, {"taken once the stop has judged it", true}} {
		t.Run(tc.name, func(t *testing.T) {
			starvetest.TakesACPU(t)
			const bound = 300 * time.Millisecond
			shortStall(t, bound)
			var looks atomic.Uint64
			looked := func() { looks.Add(1) }
			exitLooked.Store(&looked)
			defer exitLooked.Store(nil)
			dir := starvetest.MemDir(t)
			// It makes ready with a builtin and loops with builtins alone,
			// so the trap's child is the one process it starts; both its
			// loops end with the test, however it ends (ShellSpin). It is
			// held only once it has no child: one it had before the hold
			// (a `touch ready` not yet reaped as it was held) would be
			// taken for the trap's.
			pid, reaped := readyLeader(t, dir, `trap 'sh -c "`+starvetest.ShellSpin("[ ! -e release ]")+`"; exit 0' INT; : > ready; `+starvetest.ShellSpin(""))
			task := "/proc/" + strconv.Itoa(pid) + "/task/" + strconv.Itoa(pid)
			for kids, _ := threadChildren(task); len(kids) > 0; kids, _ = threadChildren(task) {
				time.Sleep(time.Millisecond)
			}
			hold := starvetest.StartHold(t)
			hold.Take(t, pid)
			type stopped struct {
				killed string
				err    error
			}
			done := make(chan stopped, 1)
			go func() {
				killed, _, err := InterruptProcessTreeStop(zap.NewNop(), pid, syscall.SIGINT, nil)
				done <- stopped{killed, err}
			}()
			var end stopped
			ended := false
			judged := 3 * uint64(bound/stall.CheckEvery(bound))
			// await waits until the stop has looked n times more, or ended.
			await := func(n uint64) {
				from := looks.Load()
				for !ended && looks.Load()-from < n {
					select {
					case end = <-done:
						ended = true
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			interrupted := time.Now()
			var child int
			for child == 0 {
				select {
				case end := <-done:
					t.Fatalf("the stop ended before the app's trap started its child: %q (%v)", end.killed, end.err)
				default:
				}
				if kids, _ := threadChildren(task); len(kids) > 0 {
					child = kids[0]
				} else {
					time.Sleep(time.Millisecond)
				}
			}
			t.Logf("the app, held, started its trap's child %v after the stop began", time.Since(interrupted))
			if tc.late {
				await(judged)
				if ended {
					t.Fatalf("the stop ended before the test took the app's child: %q (%v)", end.killed, end.err)
				}
			}
			hold.TakeChild(t, child, pid)
			hold.Let(pid)
			await(judged)
			ranHeld, _ := starvetest.Ran(child)
			if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			hold.Release()
			if ranHeld >= bound/2 {
				t.Fatalf("the app's child, held from its start, ran %v, half the stall bound (%v) or more: the test held nothing", ranHeld, bound)
			}
			if !ended {
				end = <-done
			}
			if end.err != nil {
				t.Fatal(end.err)
			}
			if end.killed != "" {
				t.Fatalf("the stop killed: %q", end.killed)
			}
			awaitReaped(t, "the app", reaped)
		})
	}
}

// kernelHoldEnv names the directory TestKernelHoldHelper writes in.
const kernelHoldEnv = "KEPLOY_TEST_KERNEL_HOLD"

// kernelTurnsEnv, set to a duration, has TestKernelHoldHelper's waits in the
// kernel end in turns (holdInTheKernelInTurns) instead of lasting.
const kernelTurnsEnv = "KEPLOY_TEST_KERNEL_TURNS"

// TestKernelHoldHelper is not a test: with kernelHoldEnv set it is an app
// that, interrupted, goes into the kernel on its way out and stays there, in
// an uninterruptible sleep (D), until a process it starts there ends
// (holdInTheKernel); then it writes "out" and exits. It writes "ready", with
// its pid, once its interrupt is set up, and "held" as it goes into the
// kernel. With kernelTurnsEnv set to d, it goes into the kernel again and
// again, for d at a time, until the file "released" exists
// (holdInTheKernelInTurns), making progress once, between two of them,
// once the file "progress" exists (it closes a file it opened as it
// started), and writes how many times it went in ("turns") before "out".
func TestKernelHoldHelper(t *testing.T) {
	dir := os.Getenv(kernelHoldEnv)
	if dir == "" {
		t.Skip("an app of the stop tests")
	}
	// Held in the kernel for good, it waits for the test: it ends with it.
	starvetest.EndWithOwner()
	// No collection: one would wait for ever on the thread held in the
	// kernel, which does not stop for it.
	debug.SetGCPercent(-1)
	ints := make(chan os.Signal, 1)
	signal.Notify(ints, syscall.SIGINT)
	note := func(file string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(3)
		}
	}
	var turn time.Duration
	var spare *os.File
	if v := os.Getenv(kernelTurnsEnv); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			os.Exit(4)
		}
		turn = d
		if spare, err = os.Open(os.DevNull); err != nil {
			os.Exit(3)
		}
	}
	note("ready")
	<-ints
	runtime.LockOSThread()
	note("held")
	if turn > 0 {
		n := holdInTheKernelInTurns(dir, turn, spare)
		if err := os.WriteFile(filepath.Join(dir, "turns"), []byte(strconv.Itoa(n)), 0o644); err != nil {
			os.Exit(3)
		}
	} else {
		holdInTheKernel()
	}
	note("out")
	os.Exit(0)
}

// holdInTheKernelInTurns holds the calling thread in the kernel, in an
// uninterruptible sleep, for d at a time (a vfork whose child sleeps d and
// exits), and has it run between, until dir/released exists: a thread whose
// waits in the kernel each end, as one waiting on storage that serves it
// does. Once dir/progress exists it closes spare, between two waits: its
// one step out. It returns how many times it went into the kernel.
func holdInTheKernelInTurns(dir string, d time.Duration, spare *os.File) (turns int) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "released")); err == nil {
			return turns
		}
		if spare != nil {
			if _, err := os.Stat(filepath.Join(dir, "progress")); err == nil {
				_ = spare.Close()
				spare = nil
			}
		}
		if pid := vforkSleeping(d); pid > 0 {
			turns++
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(pid, &ws, 0, nil)
		}
	}
}

// vforkSleeping holds the calling thread in the kernel until the child it
// vforks (its memory copied, not shared) has slept d and exited, and returns
// the child's pid. Raw calls only, as in holdInTheKernel.
//
//go:nosplit
//go:norace
func vforkSleeping(d time.Duration) int {
	const cloneVfork, sigSetmask = 0x4000, 2
	var all, old [1]uint64
	all[0] = ^uint64(0)
	ts := syscall.NsecToTimespec(int64(d))
	syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, sigSetmask, uintptr(unsafe.Pointer(&all)), uintptr(unsafe.Pointer(&old)), 8, 0, 0)
	pid, _, errno := syscall.RawSyscall6(syscall.SYS_CLONE, cloneVfork|uintptr(syscall.SIGCHLD), 0, 0, 0, 0, 0)
	if errno == 0 && pid == 0 {
		syscall.RawSyscall6(syscall.SYS_PPOLL, 0, 0, uintptr(unsafe.Pointer(&ts)), uintptr(unsafe.Pointer(&all)), 8, 0)
		syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)
	}
	syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, sigSetmask, uintptr(unsafe.Pointer(&old)), 0, 8, 0, 0)
	if errno != 0 {
		return -1
	}
	return int(pid)
}

// holdInTheKernel holds the calling thread in the kernel, in an
// uninterruptible sleep, until the child it starts there ends: it vforks
// (clone with CLONE_VFORK and its memory copied, not shared), and the kernel
// holds a vfork's parent, killable, until the child exits or execs. The child
// blocks every signal it can and sleeps until it is killed, as it is once
// its parent is gone (PR_SET_PDEATHSIG, and its parent looked for after:
// one gone before it set that does not send it). Raw calls only: the child
// runs on a copy of this goroutine's stack, with no other thread, so nothing
// here may grow the stack or reach the runtime.
//
//go:nosplit
//go:norace
func holdInTheKernel() {
	const cloneVfork, sigSetmask = 0x4000, 2
	var all, old [1]uint64
	all[0] = ^uint64(0)
	parent, _, _ := syscall.RawSyscall(syscall.SYS_GETPID, 0, 0, 0)
	syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, sigSetmask, uintptr(unsafe.Pointer(&all)), uintptr(unsafe.Pointer(&old)), 8, 0, 0)
	pid, _, errno := syscall.RawSyscall6(syscall.SYS_CLONE, cloneVfork|uintptr(syscall.SIGCHLD), 0, 0, 0, 0, 0)
	if errno == 0 && pid == 0 {
		syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0)
		if now, _, _ := syscall.RawSyscall(syscall.SYS_GETPPID, 0, 0, 0); now != parent {
			syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)
		}
		for {
			syscall.RawSyscall6(syscall.SYS_PPOLL, 0, 0, 0, uintptr(unsafe.Pointer(&all)), 8, 0)
		}
	}
	syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, sigSetmask, uintptr(unsafe.Pointer(&old)), 0, 8, 0, 0)
}

// startKernelHolder starts TestKernelHoldHelper as an app keploy started (a
// group leader), writing in dir, and returns its pid once it is ready. With
// turn above 0, its waits in the kernel end in turns of turn
// (kernelTurnsEnv).
func startKernelHolder(t *testing.T, dir string, turn time.Duration) (pid int, reaped <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestKernelHoldHelper$")
	// A race-detector build sleeps a second on its way out (GORACE's
	// atexit_sleep_ms), a way out the stop rightly judges at the tests'
	// bound: it exits as it would without one.
	cmd.Env = append(os.Environ(), kernelHoldEnv+"="+dir, "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"), starvetest.Owner())
	if turn > 0 {
		cmd.Env = append(cmd.Env, kernelTurnsEnv+"="+turn.String())
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := make(chan struct{})
	go func() { _ = cmd.Wait(); close(r) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-r })
	awaitFile(t, filepath.Join(dir, "ready"), r)
	return cmd.Process.Pid, r
}

// awaitFile waits until path exists, failing t if the process behind reaped
// exits first; the test's own timeout is its only bound.
func awaitFile(t *testing.T, path string, reaped <-chan struct{}) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-reaped:
			t.Fatalf("the app exited before it wrote %s", filepath.Base(path))
		case <-time.After(time.Millisecond):
		}
	}
}

// awaitNote waits until the app has written path, failing t if the stop
// ends first; the test's own timeout is its only bound.
func awaitNote(t *testing.T, path string, stopped <-chan struct{}) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-stopped:
			t.Fatalf("the stop ended before the app wrote %s", filepath.Base(path))
		case <-time.After(time.Millisecond):
		}
	}
}

// storageMode is what the storage a test's stops read does.
type storageMode int

const (
	// storageIdle serves no I/O.
	storageIdle storageMode = iota
	// storageServing completes I/O between any two reads, with I/O in
	// flight throughout (a disk a loaded host keeps busy).
	storageServing
	// storageSlow has I/O in flight throughout, and completes some only at
	// every third read: between two reads it mostly stands still (a loaded
	// virtual disk, a request that outlasts a look).
	storageSlow
	// storageDead has I/O in flight throughout, and completes none.
	storageDead
	// storageStacked is an encrypted volume on a partition: the volume has
	// I/O in flight throughout and completes none, and the partition under
	// it completes I/O between any two reads (a volume that holds its I/O
	// while its disk works through what is ahead of it).
	storageStacked
	// storageStackedDead is that volume and partition, both with I/O in
	// flight throughout, completing none.
	storageStackedDead
)

// storageFor has the stops of the test read storage that does as mode says;
// every thread can reach only storage on that disk. The kernel's hung-task
// time is hung, as the test sets it: where it is 0, none is read, and a stop
// takes the stall bound for it (the kernel's two minutes would hold a test
// for that long).
func storageFor(t *testing.T, mode storageMode, hung time.Duration) {
	t.Helper()
	var reads atomic.Uint64
	st := &storageReader{
		disks: func() (diskMark, bool) {
			n := reads.Add(1)
			switch mode {
			case storageServing:
				return diskMark{"259:0": {done: n, inFlight: 1, busy: time.Duration(n) * time.Hour}}, true
			case storageSlow:
				// Its busy time moves only as its requests end.
				return diskMark{"259:0": {done: n / 3, inFlight: 1, busy: time.Duration(n/3) * time.Hour}}, true
			case storageDead:
				return diskMark{"259:0": {done: 5, inFlight: 1}}, true
			case storageStacked:
				return diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 1}, "259:3": {name: "nvme0n1p3", done: n, inFlight: 1}}, true
			case storageStackedDead:
				return diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 1}, "259:3": {name: "nvme0n1p3", done: 7, inFlight: 1}}, true
			}
			return diskMark{"259:0": {done: 5}}, true
		},
		below: func(name string) []string {
			if name == "dm-0" {
				return []string{"nvme0n1p3"}
			}
			return nil
		},
		seen: func(string) bool { return true },
	}
	if hung > 0 {
		st.hung = func() time.Duration { return hung }
	}
	testStorage.Store(st)
	t.Cleanup(func() { testStorage.Store(nil) })
}

// An app whose way out waits in the kernel on storage (a file closed and
// flushed, a page of its program read back, an fsync at shutdown on a disk a
// loaded host keeps busy) waits for no CPU, and makes no progress keploy can
// see while it waits. While the storage serves I/O, that time is a wait on
// it, as a wait for a CPU is: held in the kernel again and again (each wait
// ending, as one on storage that serves it does, a sixth of the stall bound
// at a time) from the stop's interrupt until the stop has looked at it three
// times as often as a stall is judged on, it is not judged for it; nor where
// the storage is slow, completing I/O at only some of the stop's looks and
// standing still with I/O in flight in between. Released, it exits, and the
// stop says no kill. Counted as time it could run, or a look standing still
// as a dead disk, it was killed for want of progress it could not make.
func TestAStopDoesNotJudgeAnAppHeldInTheKernelWhileItsStorageServes(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode storageMode
	}{
		{"serving", storageServing},
		{"slow", storageSlow},
		{"a volume on a disk that serves", storageStacked},
	} {
		t.Run(tc.name, func(t *testing.T) { stopOfAnAppHeldInTheKernel(t, tc.mode) })
	}
}

// stopOfAnAppHeldInTheKernel runs the stop of an app held in the kernel on
// storage that does as mode says (TestAStopDoesNotJudgeAnAppHeldInTheKernel
// WhileItsStorageServes).
func stopOfAnAppHeldInTheKernel(t *testing.T, mode storageMode) {
	const bound = 300 * time.Millisecond
	shortStall(t, bound)
	// A hung-task time far beyond the test's window: the stop credits the
	// app's waits in the kernel, since its last progress, for no longer.
	storageFor(t, mode, 100*bound)
	var looks atomic.Uint64
	looked := func() { looks.Add(1) }
	exitLooked.Store(&looked)
	defer exitLooked.Store(nil)
	dir := starvetest.MemDir(t)
	pid, reaped := startKernelHolder(t, dir, bound/6)
	type stopped struct {
		killed string
		err    error
	}
	done, over := make(chan stopped, 1), make(chan struct{})
	go func() {
		killed, _, err := InterruptProcessTreeStop(zap.NewNop(), pid, syscall.SIGINT, nil)
		done <- stopped{killed, err}
		close(over)
	}()
	// From its note on: the app goes into the kernel in turns until the
	// test releases it. A look for it there (D) would race its turns.
	awaitNote(t, filepath.Join(dir, "held"), over)
	from := looks.Load()
	judged := 3 * uint64(bound/stall.CheckEvery(bound))
	var end stopped
	ended := false
	for !ended && looks.Load()-from < judged {
		select {
		case end = <-done:
			ended = true
		case <-time.After(10 * time.Millisecond):
		}
	}
	if ended {
		t.Fatalf("the stop ended while the app was held in the kernel, its storage serving: it said %q (%v)", end.killed, end.err)
	}
	if err := os.WriteFile(filepath.Join(dir, "released"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	end = <-done
	if end.err != nil {
		t.Fatal(end.err)
	}
	if end.killed != "" {
		t.Fatalf("the stop killed: %q", end.killed)
	}
	awaitReaped(t, "the app", reaped)
	if _, err := os.Stat(filepath.Join(dir, "out")); err != nil {
		t.Fatal("the app did not get out on its own once let go")
	}
	b, err := os.ReadFile(filepath.Join(dir, "turns"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := strconv.Atoi(string(b)); err != nil || n < 1 {
		t.Fatalf("the app went into the kernel %q times, want at least once", b)
	}
}

// cpuExitClock is an ExitClock of process pid (and the processes it runs)
// that takes no wait in the kernel for one on storage: the time it and keploy
// could run by their waits for a CPU alone. Where its storage serves none,
// that is the time a stop judges its stall on, by a rule that does not rest
// on the storage rule a test judges.
func cpuExitClock(t *testing.T, pid int) (*ExitClock, func()) {
	t.Helper()
	proc, ok := ProcOf(pid)
	if !ok {
		t.Fatalf("process %d is gone", pid)
	}
	own, release := stall.Process()
	read := func(procs []Reached) (map[int]taskTime, bool) {
		return readTaskTimesSeen(procs, func(string) bool { return false })
	}
	noDisks := func() (diskMark, bool) { return nil, false }
	return newExitClock([]Reached{proc}, StopStallBudget, StopStallBudget, StopStallBudget, own, time.Now, read, noDisks, nil), release
}

// An app held in the kernel while no storage serves I/O is held on no I/O
// the host's disks show (a lock in the kernel, a vfork): such a wait may
// never end, and the stop must still go on. Its time counts, and the stop
// kills it on the stall bound and says so, by the time it and keploy could
// run at three times the bound from the stop's first look on: on a loaded
// host far more of the stop's looks than the bound is of them. So with a dead
// disk, one that stands still with I/O in flight: it is taken for slow for
// the bound, then for dead, and the stop kills the app on the bound after
// that, by four times it; the kernel's hung-task time is far beyond that, so
// only the rule of a dead disk judges it in time. So too while the host's
// storage serves, but serves the app nothing: held in one wait in the
// kernel, given no CPU, it is taken to wait on storage for the bound (the
// hung-task time the test's storage reads none of), then for wedged, and the
// stop kills it on the bound after that, by three times it. The app says it
// went into the kernel ("held"), as the stop may judge it before the test
// would see it there. Taken for a wait, the stop would wait on it for ever.
func TestAStopJudgesAnAppHeldInTheKernelWhileNoStorageServesIt(t *testing.T) {
	const bound = 300 * time.Millisecond
	for _, tc := range []struct {
		name   string
		mode   storageMode
		hung   time.Duration
		bounds time.Duration
	}{
		{"idle", storageIdle, 0, 3},
		{"dead", storageDead, 100 * bound, 4},
		{"a volume on a dead disk", storageStackedDead, 100 * bound, 4},
		{"serving others", storageServing, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopJudgingAnAppHeldInTheKernel(t, tc.mode, bound, tc.hung, 0, tc.bounds*bound)
		})
	}
}

// An app whose way out goes into the kernel again and again, each wait
// ending as one on storage that serves it does, and which makes no progress
// between them (a driver's loop of short sleeps in the kernel, a cgroup's
// memory.high throttling it, a sync loop while others dirty pages) may wait
// there for ever while the host's disks serve others: the stop credits its
// waits there, since its last progress, for no longer in all than the kernel
// lets a task stand in the kernel before it reports it hung, and counts them
// after. Here its storage serves throughout, each wait lasts a sixth of the
// stall bound, and the hung-task time is three times the bound: the stop
// kills it, and says so, by twice that and the bound of the time it and
// keploy could run from the stop's first look on. Credited for as long as
// the host's disks served others, each turn on a CPU ending its stretch in
// the kernel, its stall accrued only at the rate it ran between its waits:
// on .189 it was killed after 72 times the bound.
func TestAStopJudgesAnAppInTheKernelInTurnsThatMakesNoProgress(t *testing.T) {
	const bound = 300 * time.Millisecond
	const hung = 3 * bound
	stopJudgingAnAppHeldInTheKernel(t, storageServing, bound, hung, bound/6, 2*(hung+bound))
}

// The credit of an app's waits in the kernel starts again at its progress.
// Its way out goes into the kernel in turns, each wait a sixth of the stall
// bound and ending as one on storage that serves it does, its storage
// serving throughout. It waits there until it and keploy could run for
// seven tenths of the kernel's hung-task time (ten times the bound) since
// the stop's first look, makes progress once (it closes a file), waits
// there for as long again, and is let go: it exits, and the stop says no
// kill, as its waits since its progress never outlasted the hung-task time.
// Credited from the stop's start, they outlasted it in its second stretch,
// and it was killed on the bound after, a step out it made forgotten.
func TestAStopCreditsAnAppsWaitsInTheKernelAgainOnceItMakesProgress(t *testing.T) {
	const bound = 300 * time.Millisecond
	const hung = 10 * bound
	shortStall(t, bound)
	storageFor(t, storageServing, hung)
	var looks atomic.Uint64
	looked := func() { looks.Add(1) }
	exitLooked.Store(&looked)
	defer exitLooked.Store(nil)
	dir := starvetest.MemDir(t)
	pid, reaped := startKernelHolder(t, dir, bound/6)
	could, releaseClock := cpuExitClock(t, pid)
	defer releaseClock()
	scope := &TeardownScope{}
	type stopped struct {
		killed string
		err    error
	}
	done := make(chan stopped, 1)
	go func() {
		killed, _, err := InterruptProcessTreeStop(zap.NewNop(), pid, syscall.SIGINT, scope)
		done <- stopped{killed, err}
	}()
	// ended fails the test where the stop has ended, which it must not
	// before the app is let go; else it waits a little.
	ended := func(when string) {
		select {
		case end := <-done:
			t.Fatalf("the stop ended %s, the app in the kernel in turns, its storage serving: it said %q (%v)", when, end.killed, end.err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	for looks.Load() == 0 {
		ended("before its first look")
	}
	// Each stretch is counted from the stop's last sight of the app's
	// progress (its moves of the teardown): one it saw early on (a file
	// the app wrote as it went into the kernel, closed) starts it again.
	moves, from := scope.Moves(), could.Runnable()
	inTheKernel := func(when string) {
		for could.Runnable()-from < 7*hung/10 {
			if m := scope.Moves(); m != moves {
				moves, from = m, could.Runnable()
			}
			ended(when)
		}
	}
	inTheKernel("before the app made progress")
	if err := os.WriteFile(filepath.Join(dir, "progress"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for scope.Moves() == moves {
		ended("before it saw the app's progress")
	}
	moves, from = scope.Moves(), could.Runnable()
	inTheKernel("after the app made progress")
	if err := os.WriteFile(filepath.Join(dir, "released"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	end := <-done
	if end.err != nil {
		t.Fatal(end.err)
	}
	if end.killed != "" {
		t.Fatalf("the stop killed: %q", end.killed)
	}
	awaitReaped(t, "the app", reaped)
	if _, err := os.Stat(filepath.Join(dir, "out")); err != nil {
		t.Fatal("the app did not get out on its own once let go")
	}
}

// stopJudgingAnAppHeldInTheKernel runs the stop, at stall bound, of an app
// held in the kernel (in turns of turn, or in one wait for good where turn is
// 0) on storage that does as mode says and serves it nothing, the kernel's
// hung-task time hung (storageFor), which must kill it by the time it and
// keploy could run for limit.
func stopJudgingAnAppHeldInTheKernel(t *testing.T, mode storageMode, bound, hung, turn, limit time.Duration) {
	shortStall(t, bound)
	storageFor(t, mode, hung)
	var looks atomic.Uint64
	looked := func() { looks.Add(1) }
	exitLooked.Store(&looked)
	defer exitLooked.Store(nil)
	dir := starvetest.MemDir(t)
	pid, reaped := startKernelHolder(t, dir, turn)
	could, releaseClock := cpuExitClock(t, pid)
	defer releaseClock()
	type stopped struct {
		killed string
		err    error
	}
	done := make(chan stopped, 1)
	go func() {
		killed, _, err := InterruptProcessTreeStop(zap.NewNop(), pid, syscall.SIGINT, nil)
		done <- stopped{killed, err}
	}()
	var end stopped
	ended := false
	// The stall is counted from the stop's first look, and so is this
	// clock, which then spans no more of the time than the stop's.
	for !ended && looks.Load() == 0 {
		select {
		case end = <-done:
			ended = true
		case <-time.After(10 * time.Millisecond):
		}
	}
	from := could.Runnable()
	for !ended {
		if stalled := could.Runnable() - from; stalled >= limit {
			t.Fatalf("the stop did not judge an app held in the kernel with no storage serving it, though it and keploy could run for %v, %.1f times the stall bound: it would wait on it for ever", stalled, float64(stalled)/float64(bound))
		}
		select {
		case end = <-done:
			ended = true
		case <-time.After(10 * time.Millisecond):
		}
	}
	if end.err != nil {
		t.Fatal(end.err)
	}
	if !strings.Contains(end.killed, "made no progress on its way out for "+bound.String()+" of its and keploy's runnable time") {
		t.Fatalf("the stop said %q; want the stall's kill", end.killed)
	}
	if _, err := os.Stat(filepath.Join(dir, "held")); err != nil {
		t.Fatal("the stop killed the app before it went into the kernel")
	}
	awaitReaped(t, "the app", reaped)
}

// A process's way out is read on its exit marks: one ending, a thread ending,
// a file closed, storage read or written while it waits on it. A count that
// rises and falls back is none.
// A process whose main thread exited while another runs on is on no way out
// of its own: MarkExit counts it running, with its threads and its open
// files, read through the thread that runs (its main thread shows none),
// until every thread of it has exited.
func TestMarkExitCountsAProcessWhoseMainThreadExited(t *testing.T) {
	cmd := startMainThreadExited(t)
	st, ok := ReadProcStat(cmd.Process.Pid)
	if !ok {
		t.Fatal("premise: its stat could not be read")
	}
	p := Reached{PID: cmd.Process.Pid, Start: st.StartTime}
	// Its open files are at least its stdin, stdout and stderr: three opens
	// of /dev/null (os/exec's, for streams it was given none of).
	if m := MarkExit([]Reached{p}); m.Running != 1 || m.Threads < 2 || m.Fds < 3 {
		t.Fatalf("MarkExit = %+v; want it running, with its threads and open files (its stdin, stdout and stderr at least)", m)
	}
	_ = cmd.Process.Kill()
	deadline := time.Now().Add(time.Minute) // turns a hang into a failure
	for MarkExit([]Reached{p}).Running != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("MarkExit still counts process %d running after its kill", p.PID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// MarkExit reads the storage I/O each thread of a process did itself, by its
// tid (readThreadsIO), and never the process's own count, which takes in the
// I/O of each child it reaps: a thread's count is kept apart from the
// others', so that one that ends takes none of the mark from them
// (ExitRatchet). Where the threads could not be listed whole (a count of
// them no listing makes up), it says so, and the mark of one the listing
// left out is kept (ExitMark.IOPartial). MarkExit, of this test's process,
// reads its threads' I/O from the fake /proc (threadsIO): its IO is theirs,
// and it is IOPartial only where their listing was not whole.
func TestReadThreadsIOReadsEachThreadsOwnCount(t *testing.T) {
	type proc = struct {
		state   byte
		start   uint64
		threads []fakeThread
	}
	root := fakeProc(t, map[int]proc{
		900: {state: 'S', start: 9, threads: []fakeThread{{tid: 900, state: 'S'}, {tid: 901, state: 'S'}}},
	})
	write := func(path string, read, written uint64) {
		s := fmt.Sprintf("rchar: 1\nwchar: 1\nsyscr: 1\nsyscw: 1\nread_bytes: %d\nwrite_bytes: %d\ncancelled_write_bytes: 0\n", read, written)
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "900", "io"), 1<<30, 0)
	write(filepath.Join(root, "900", "task", "900", "io"), 4096, 0)
	write(filepath.Join(root, "900", "task", "901", "io"), 8192, 4096)
	want := map[int]uint64{900: 4096, 901: 12288}
	got := map[int]uint64{}
	if !readThreadsIO(root, 900, got) {
		t.Fatal("both threads listed: read as a listing not whole")
	}
	if !maps.Equal(got, want) {
		t.Fatalf("read %v; want %v: each thread's own count, by its tid, and not the process's", got, want)
	}
	read := threadsIO
	t.Cleanup(func() { threadsIO = read })
	threadsIO = func(_ int, io map[int]uint64) bool { return readThreadsIO(root, 900, io) }
	self, ok := ReadProcStat(os.Getpid())
	if !ok {
		t.Fatal("premise: this test's stat could not be read")
	}
	marked := func(partial bool) {
		t.Helper()
		m := MarkExit([]Reached{{PID: os.Getpid(), Start: self.StartTime}})
		if m.IOPartial != partial || !maps.Equal(m.IO, want) {
			t.Fatalf("MarkExit read %+v; want IO %v, its threads' in the fake /proc, and IOPartial %v", m, want, partial)
		}
	}
	marked(false)
	// Its stat counts a third thread that no listing shows.
	stat, err := os.ReadFile(filepath.Join(root, "900", "stat"))
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(stat))
	f[19] = "3"
	if err := os.WriteFile(filepath.Join(root, "900", "stat"), []byte(strings.Join(f, " ")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = map[int]uint64{}
	if readThreadsIO(root, 900, got) {
		t.Fatal("two of three threads listed: read as a whole listing")
	}
	if !maps.Equal(got, want) {
		t.Fatalf("read %v from a listing not whole; want %v, the threads it showed", got, want)
	}
	marked(true)
}

func TestTheExitRatchetTakesOnlyProgress(t *testing.T) {
	var r ExitRatchet
	r.Advance(ExitMark{Running: 2, Threads: 4, Fds: 10})
	for _, tc := range []struct {
		m    ExitMark
		want bool
	}{
		{ExitMark{Running: 2, Threads: 4, Fds: 11}, false},
		{ExitMark{Running: 2, Threads: 4, Fds: 10}, false},
		{ExitMark{Running: 2, Threads: 4, Fds: 9}, true},
		{ExitMark{Running: 1, Threads: 4, Fds: 9}, true},
		{ExitMark{Running: 1, Threads: 3, Fds: 9}, true},
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: map[int]uint64{7: 100}}, false},
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: map[int]uint64{7: 200}, OnStorage: true}, true},
		// Each thread's I/O is held against its own most: one that
		// ends takes none of the mark from the others, and one not
		// seen before has done none.
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: map[int]uint64{8: 50}, OnStorage: true}, true},
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: map[int]uint64{8: 50}, OnStorage: true}, false},
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: map[int]uint64{8: 60}, OnStorage: true}, true},
		// A thread a partial listing leaves out keeps its most; one
		// whose count could not be read (0) keeps it too.
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: map[int]uint64{9: 10}, IOPartial: true}, false},
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: map[int]uint64{8: 0, 9: 10}, OnStorage: true}, false},
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: map[int]uint64{8: 60, 9: 10}, OnStorage: true}, false},
		// One a whole listing leaves out has ended: its tid, given
		// again, names a thread whose I/O starts at none.
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: map[int]uint64{9: 10}}, false},
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: map[int]uint64{8: 5, 9: 10}, OnStorage: true}, true},
	} {
		if got := r.Advance(tc.m); got != tc.want {
			t.Fatalf("%+v: progress %v, want %v", tc.m, got, tc.want)
		}
	}
	// A read of open files that did not settle is no evidence: it neither
	// passes the mark nor sets it, and the first settled one sets it.
	for _, seq := range [][]struct {
		m    ExitMark
		want bool
	}{
		{
			{ExitMark{Running: 1, Fds: 9}, false},
			{ExitMark{Running: 1, Fds: 5, FilesUnsettled: true}, false},
			{ExitMark{Running: 1, Fds: 9}, false},
			{ExitMark{Running: 1, Fds: 8}, true},
		},
		// A first read that counted too many: the later, true ones are
		// no progress, until one passes the first settled one.
		{
			{ExitMark{Running: 1, Fds: 5, FilesUnsettled: true}, false},
			{ExitMark{Running: 1, Fds: 3}, false},
			{ExitMark{Running: 1, Fds: 3}, false},
			{ExitMark{Running: 1, Fds: 2, FilesUnsettled: true}, false},
			{ExitMark{Running: 1, Fds: 2}, true},
		},
	} {
		var r ExitRatchet
		for i, tc := range seq {
			if got := r.Advance(tc.m); got != tc.want {
				t.Fatalf("reading %d, %+v: progress %v, want %v", i, tc.m, got, tc.want)
			}
		}
	}
	// A process gone, or whose pid names another, is not running.
	if m := MarkExit([]Reached{{PID: os.Getpid(), Start: 1}}); m.Running != 0 {
		t.Fatalf("a pid naming another process counted: %+v", m)
	}
	if st, ok := ReadProcStat(os.Getpid()); ok {
		if m := MarkExit([]Reached{{PID: os.Getpid(), Start: st.StartTime}}); m.Running != 1 || m.Threads == 0 {
			t.Fatalf("this process read as %+v", m)
		}
		// Its threads are its count of them (processThreads), not a
		// listing, which can leave out one that runs.
		count := processThreads
		t.Cleanup(func() { processThreads = count })
		processThreads = func(pid int) int {
			if pid != os.Getpid() {
				t.Errorf("threads counted of %d; want only this process's (%d)", pid, os.Getpid())
			}
			return 4242
		}
		if m := MarkExit([]Reached{{PID: os.Getpid(), Start: st.StartTime}}); m.Threads != 4242 {
			t.Fatalf("this process's threads: %d; want 4242, its count", m.Threads)
		}
		processThreads = count
	}
	if n := processThreads(os.Getpid()); n == 0 {
		t.Fatal("this process counts no threads")
	}
	if n := processThreads(1<<22 + 1); n != 0 {
		t.Fatalf("a pid beyond pid_max counts %d threads; want 0", n)
	}
}

// A process's open files are its files, not its descriptors: a duplicate
// counts once with what it duplicates, and closing it is no step of its way
// out (dash's `exec 3<&-` saves fd 3 to fd 10 and closes both: a churn of one
// file read by descriptors looks two steps deep). Closing the last descriptor
// of a file is one, and two opens of one file are two files. The child holds
// the descriptors the row gives it and no others (TestMain: a CI runner's
// step hands the test binary descriptors open on exec, which the child would
// hold too, each a file more). Where the kernel refuses to compare the
// child's files (kcmp(2) of another process needs
// PTRACE_MODE_READ_REALCREDS), each of its descriptors counts, and the
// reading says why: closing a duplicate is then a step out, which the
// ratchet bounds (ExitRatchet). Which of the two it is, is MarkExit's own
// answer (ExitMark.FilesUncompared): the row logs why the kernel refused and
// holds keploy to that.
// KEPLOY_TEST_KCMP_REQUIRED=1 (go-test.yaml's run of it as root) fails it
// there instead, so the comparison itself runs in CI.
func TestMarkExitCountsOpenFilesNotDescriptors(t *testing.T) {
	dir := t.TempDir()
	open := func(name string) *os.File {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	in, out, f := open("in"), open("out"), open("f")
	g, err := os.Open(f.Name()) // f's file opened again: another open file
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	ready, closedDup, closedLast := filepath.Join(dir, "ready"), filepath.Join(dir, "dup"), filepath.Join(dir, "last")
	cmd := exec.Command("/bin/sh", "-c", `trap 'exec 5<&-; touch "$2"' USR1; trap 'exec 3<&-; touch "$3"' USR2; touch "$1"; while :; do sleep 1; done`, "sh", ready, closedDup, closedLast)
	// stdout and stderr one file; fds 3 and 5 one file, 4 another open of it.
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.ExtraFiles = in, out, out, []*os.File{f, g, f}
	// A group of its own, killed whole: its sleep ends with it. Killed by
	// the kernel too when the thread that started it ends (Pdeathsig), so a
	// test binary killed mid-test, its cleanups never run, leaves no loop
	// behind: the test keeps to this thread until it ends.
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-reaped })
	awaitFile(t, ready, reaped)
	st, ok := ReadProcStat(cmd.Process.Pid)
	if !ok {
		t.Fatal("premise: its stat could not be read")
	}
	procs := []Reached{{PID: cmd.Process.Pid, Start: st.StartTime}}
	fdDir := "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/fd"
	list := func() []os.DirEntry {
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			t.Fatalf("premise: the child's descriptors could not be listed: %v", err)
		}
		return fds
	}
	fds := list()
	var names []string
	for _, e := range fds {
		names = append(names, e.Name())
	}
	if slices.Sort(names); !slices.Equal(names, []string{"0", "1", "2", "3", "4", "5"}) {
		t.Fatalf("premise: the child holds descriptors %s; want 0 to 5 alone, those the row gave it (TestMain's keeping of the binary's own: %v)", descriptorsOf(fdDir, fds), errDescriptorsKept)
	}
	var r ExitRatchet
	m := MarkExit(procs)
	if m.FilesUncompared != nil {
		var errno syscall.Errno
		// A refusal (no kcmp, or keploy may not ask), not a slip of the read.
		if !errors.As(m.FilesUncompared, &errno) || (errno != syscall.EPERM && errno != syscall.EACCES && errno != syscall.ENOSYS) ||
			!strings.Contains(m.FilesUncompared.Error(), "process "+strconv.Itoa(cmd.Process.Pid)+"'s") {
			t.Fatalf("a reading of files the kernel would not compare says %q; want the child's pid and the kernel's refusal (EPERM, EACCES or ENOSYS)", m.FilesUncompared)
		}
		why := kcmpRefusal(cmd.Process.Pid)
		if os.Getenv("KEPLOY_TEST_KCMP_REQUIRED") == "1" {
			t.Fatalf("KEPLOY_TEST_KCMP_REQUIRED=1, but the kernel refuses to compare a child's files (%v): %s", m.FilesUncompared, why)
		}
		t.Logf("the kernel refuses to compare a child's files (%v, errno %d): %s", m.FilesUncompared, int(errno), why)
		core, logs := observer.New(zap.InfoLevel)
		var note FilesUncomparedNote
		for i, step := range []func(){
			nil,
			func() { _ = cmd.Process.Signal(syscall.SIGUSR1); awaitFile(t, closedDup, reaped) },
			func() { _ = cmd.Process.Signal(syscall.SIGUSR2); awaitFile(t, closedLast, reaped) },
		} {
			if step != nil {
				step()
				fds, m = list(), MarkExit(procs)
			}
			note.Say(zap.New(core), m)
			// Each read closed a descriptor: one fewer is a step out.
			if m.Running != 1 || m.Fds != len(fds) || m.FilesUnsettled || !errors.Is(m.FilesUncompared, errno) || r.Advance(m) != (i > 0) {
				t.Fatalf("read %d of a process whose descriptors %s the kernel will not compare: %+v; want running, each descriptor counted, settled, uncompared for %v, and progress %v", i, descriptorsOf(fdDir, fds), m, errno, i > 0)
			}
		}
		said := logs.FilterMessageSnippet("as their descriptors").All()
		if len(said) != 1 || logs.Len() != 1 || !strings.Contains(fmt.Sprint(said[0].ContextMap()["error"]), "process "+strconv.Itoa(cmd.Process.Pid)+"'s") {
			t.Fatalf("the stop said %d times (of %d lines) that it counts the child's descriptors, and why; want once, naming the child", len(said), logs.Len())
		}
		return
	}
	if m.Fds != 4 || m.FilesUnsettled {
		t.Fatalf("a process holding stdin, one file as stdout and stderr, and two opens of one file over three descriptors (%s) reads %+v; want 4 open files, settled", descriptorsOf(fdDir, fds), m)
	}
	r.Advance(m)
	_ = cmd.Process.Signal(syscall.SIGUSR1)
	awaitFile(t, closedDup, reaped)
	// The reading's progress is taken once: a second Advance of the same
	// reading passes no mark, and would say none whatever the first said.
	if m := MarkExit(procs); m.Fds != 4 || m.FilesUnsettled || m.FilesUncompared != nil {
		t.Fatalf("closing a duplicate read as %+v; want 4 open files, settled, compared", m)
	} else if r.Advance(m) {
		t.Fatalf("closing a duplicate read as %+v: progress; want none", m)
	}
	_ = cmd.Process.Signal(syscall.SIGUSR2)
	awaitFile(t, closedLast, reaped)
	if m := MarkExit(procs); m.Fds != 3 || m.FilesUnsettled || m.FilesUncompared != nil || !r.Advance(m) {
		t.Fatalf("closing a file's last descriptor read as %+v; want 3 open files, settled, compared, and progress", m)
	}
}

// reapIOEnv has this test binary be, from its start, an app of
// TestStorageIOItsChildDidIsNoProgressOfAnApp: "child" starts a child
// ("reader") that reads the file reapIOFileEnv names from storage, kills and
// reaps it once a line comes on its stdin and goes into the kernel; "self"
// reads that file itself in between. It says "ready" (self), "started"
// (child) once it has started the reader and closed what it opened to (the
// pipe the reader's exec is reported on), "held" as it goes into the kernel,
// and the reader "read N", N the storage I/O its process has done. It runs
// from init: the thread that goes into the kernel is then its main one, whose
// state its stat shows (ExitMark.OnStorage). It opens and closes no file
// between the line and the kernel (the child is started with no pidfd and
// let go by a kill): its files are no progress.
const (
	reapIOEnv     = "KEPLOY_TEST_REAP_IO"
	reapIOFileEnv = "KEPLOY_TEST_REAP_IO_FILE"
)

func init() {
	mode := os.Getenv(reapIOEnv)
	if mode == "" {
		return
	}
	read := func() {
		b, err := os.ReadFile(os.Getenv(reapIOFileEnv))
		if err != nil || len(b) == 0 {
			os.Exit(3)
		}
		fmt.Printf("read %d\n", readStorageIO("/proc/self/io"))
	}
	if mode == "reader" {
		read()
		for { // until its parent kills it
			time.Sleep(time.Hour)
		}
	}
	// No collection: one would wait for ever on the thread held in the
	// kernel, which does not stop for it.
	debug.SetGCPercent(-1)
	child := 0
	if mode == "child" {
		// Its own mode left out: a process's first value of a name is
		// the one it reads, and a reader that read "child" would start
		// another, without end.
		env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, reapIOEnv+"=") })
		env = append(env, reapIOEnv+"=reader")
		pid, err := syscall.ForkExec("/proc/self/exe", []string{os.Args[0]}, &syscall.ProcAttr{Env: env, Files: []uintptr{0, 1, 2}})
		if err != nil {
			os.Exit(3)
		}
		child = pid
		// Once ForkExec has returned: it closes the pipe its child
		// reports its exec on only after the child has exec'd, so a
		// reading taken on the reader's line alone could count that
		// pipe, which the app then closes: a step out.
		fmt.Println("started")
	} else {
		fmt.Println("ready")
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		os.Exit(3)
	}
	if child > 0 {
		var ws syscall.WaitStatus
		if syscall.Kill(child, syscall.SIGKILL) != nil {
			os.Exit(3)
		}
		if _, err := syscall.Wait4(child, &ws, 0, nil); err != nil {
			os.Exit(3)
		}
	} else {
		read()
	}
	fmt.Println("held")
	holdInTheKernel()
	os.Exit(0)
}

// A process is on its way out through storage by the storage I/O it does
// itself, not by its children's: /proc/<pid>/io takes in the I/O of each
// child a process reaps, so a shell looping on `sleep 1` beside a cold page
// cache, read as it vforked the next (a vfork waits in the kernel, D, for
// its child to exec), read as progress (TestMarkExitCountsOpenFilesNotDescriptors
// failed so in CI). The app's child reads a file from storage before the
// first reading; the app reaps it, goes into the kernel and is read again:
// no progress. The app reading the file itself, between the two, is.
func TestStorageIOItsChildDidIsNoProgressOfAnApp(t *testing.T) {
	file := storageReadFile(t)
	for _, tc := range []struct {
		mode     string
		progress bool
	}{{"child", false}, {"self", true}} {
		t.Run(tc.mode, func(t *testing.T) {
			evictFromCache(t, file)
			cmd := exec.Command("/proc/self/exe")
			cmd.Env = append(os.Environ(), reapIOEnv+"="+tc.mode, reapIOFileEnv+"="+file)
			cmd.Stderr = os.Stderr
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			// A group of its own, killed whole, and by the kernel when
			// the thread that started it ends (TestMarkExitCountsOpenFilesNotDescriptors).
			runtime.LockOSThread()
			t.Cleanup(runtime.UnlockOSThread)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
			lines := bufio.NewScanner(stdout)
			said := func(want string) string {
				if !lines.Scan() {
					t.Fatalf("the app ended before it said %q (%v)", want, lines.Err())
				}
				if !strings.HasPrefix(lines.Text(), want) {
					t.Fatalf("the app said %q; want %q", lines.Text(), want)
				}
				return lines.Text()
			}
			readLine := func(line string) {
				rest, ok := strings.CutPrefix(line, "read ")
				if !ok {
					t.Fatalf("the app said %q; want %q", line, "read N")
				}
				if n, err := strconv.ParseUint(rest, 10, 64); err != nil || n == 0 {
					t.Fatalf("premise: the reader of %s did no storage I/O (%d, %v)", file, n, err)
				}
			}
			readSaid := func() { readLine(said("read ")) }
			st, ok := ReadProcStat(cmd.Process.Pid)
			if !ok {
				t.Fatal("premise: the app's stat could not be read")
			}
			procs := []Reached{{PID: cmd.Process.Pid, Start: st.StartTime}}
			if tc.mode == "child" {
				// The reader's line and the app's, in either order.
				for got := map[string]bool{}; !got["read"] || !got["started"]; {
					if !lines.Scan() {
						t.Fatalf("the app ended before it said %q and %q (%v)", "read N", "started", lines.Err())
					}
					switch line := lines.Text(); {
					case line == "started" && !got["started"]:
						got["started"] = true
					case !got["read"]:
						readLine(line)
						got["read"] = true
					default:
						t.Fatalf("the app said %q; want %q and %q, once each", line, "read N", "started")
					}
				}
			} else {
				said("ready")
			}
			var r ExitRatchet
			whole := "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/io"
			first, firstWhole := MarkExit(procs), readStorageIO(whole)
			r.Advance(first)
			if _, err := io.WriteString(stdin, "go\n"); err != nil {
				t.Fatal(err)
			}
			if tc.mode == "self" {
				readSaid()
			}
			said("held")
			// Held in the kernel, until it is killed, once it has its
			// vfork's child (its only one: the reader is reaped before
			// "held") and then reads D: a wait in the kernel before that
			// (a page fault, the lock the vfork takes to copy its memory)
			// is short, and may be over by MarkExit's read.
			app := "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/task/" + strconv.Itoa(cmd.Process.Pid)
			for {
				kids, _ := threadChildren(app)
				st, ok := ReadProcStat(cmd.Process.Pid)
				if !ok {
					t.Fatal("the app is gone before it was read in the kernel")
				}
				if len(kids) > 0 && st.State == 'D' {
					break
				}
				time.Sleep(time.Millisecond)
			}
			m := MarkExit(procs)
			if !m.OnStorage {
				t.Fatalf("premise: the app held in the kernel reads %+v, not in an uninterruptible sleep", m)
			}
			if got := r.Advance(m); got != tc.progress {
				how := "reaped a child that read storage"
				if tc.mode == "self" {
					how = "read storage itself"
				}
				t.Fatalf("an app that %s, then went into the kernel, read %+v after %+v (its process's own count of its I/O, children's in: %d after %d): progress %v, want %v",
					how, m, first, readStorageIO(whole), firstWhole, got, tc.progress)
			}
		})
	}
}

// storageIORequiredEnv, set to 1, fails a test that needs reads to reach
// storage (storageReadFile) where none do, instead of skipping it: go-test.yaml
// sets it, as its runner's disks take them.
const storageIORequiredEnv = "KEPLOY_TEST_STORAGE_IO_REQUIRED"

// storageReadFile is a file of 1 MiB whose reads, once it is out of the
// page cache (evictFromCache), go to storage: in t's temp dir, or, where
// that is a memory file system (tmpfs), the package's own directory. Where
// neither reaches storage (both in memory, or a cache that the kernel's
// eviction does not empty, as ZFS's ARC), it skips the test, or fails it
// with storageIORequiredEnv=1.
func storageReadFile(t *testing.T) string {
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil { // no compression keeps it from storage
		t.Fatal(err)
	}
	var tried []string
	for _, dir := range []string{t.TempDir(), "."} {
		f, err := os.CreateTemp(dir, "storage-read-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(f.Name()) })
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		evictFromCache(t, f.Name())
		runtime.LockOSThread()
		before := readStorageIO("/proc/thread-self/io")
		_, err = os.ReadFile(f.Name())
		after := readStorageIO("/proc/thread-self/io")
		runtime.UnlockOSThread()
		if err != nil {
			t.Fatal(err)
		}
		if after > before {
			path, err := filepath.Abs(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			return path
		}
		tried = append(tried, dir)
	}
	why := fmt.Sprintf("reads of a file out of the page cache reach no storage in %v", tried)
	if os.Getenv(storageIORequiredEnv) == "1" {
		t.Fatalf("%s=1, but %s", storageIORequiredEnv, why)
	}
	t.Skip(why)
	return ""
}

// evictFromCache drops file's pages from the page cache (none maps it).
func evictFromCache(t *testing.T, file string) {
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		t.Fatal(err)
	}
}

// kcmpRefusal is what ptrace_may_access reads of this process and of child
// pid, which kcmp(2) of the child's files needs (PTRACE_MODE_READ_REALCREDS):
// both one's credentials, the child's dumpable state (its /proc directory is
// root's where it is not dumpable), the LSMs and their labels, and the
// sysctls that bear on it.
func kcmpRefusal(pid int) string {
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			return "(" + err.Error() + ")"
		}
		return strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
	}
	creds := func(proc string) string {
		var kept []string
		for _, line := range strings.Split(read(proc+"/status"), "\n") {
			for _, key := range []string{"Uid:", "Gid:", "CapPrm:", "CapEff:", "NoNewPrivs:", "Seccomp:"} {
				if strings.HasPrefix(line, key) {
					kept = append(kept, strings.Join(strings.Fields(line), " "))
				}
			}
		}
		return strings.Join(kept, ", ") + ", label " + read(proc+"/attr/current")
	}
	child := "/proc/" + strconv.Itoa(pid)
	owner := "(unread)"
	if fi, err := os.Stat(child); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			owner = strconv.Itoa(int(st.Uid))
		}
	}
	return "kernel " + read("/proc/sys/kernel/osrelease") +
		"; this process: " + creds("/proc/self") +
		"; the child: " + creds(child) + ", its /proc directory uid " + owner +
		"; fs.suid_dumpable " + read("/proc/sys/fs/suid_dumpable") +
		", kernel.yama.ptrace_scope " + read("/proc/sys/kernel/yama/ptrace_scope") +
		", LSMs " + read("/sys/kernel/security/lsm")
}

// A read of a process's open files that did not settle (its descriptors
// kept changing as they were compared) says so in the reading: it is no
// evidence (ExitRatchet).
func TestMarkExitSaysWhereItsFilesDidNotSettle(t *testing.T) {
	st, ok := ReadProcStat(os.Getpid())
	if !ok {
		t.Fatal("premise: this process's stat could not be read")
	}
	// Each descriptor closed as it is compared with another, and open as
	// it is with itself.
	never := func(_, a, b int) (int, error) {
		if a != b {
			return 0, syscall.EBADF
		}
		return 0, nil
	}
	filesCompared.Store(&never)
	t.Cleanup(func() { filesCompared.Store(nil) })
	if m := MarkExit([]Reached{{PID: os.Getpid(), Start: st.StartTime}}); m.Running != 1 || !m.FilesUnsettled {
		t.Fatalf("this process, its descriptors never holding still, read as %+v; want running, its files unsettled", m)
	}
}

// A reading of a process whose files the kernel will not compare counts its
// descriptors, settled, and says why, naming the process; a stop says so
// once (FilesUncomparedNote), at its first look, not at each.
func TestMarkExitSaysWhyItCountedDescriptors(t *testing.T) {
	st, ok := ReadProcStat(os.Getpid())
	if !ok {
		t.Fatal("premise: this process's stat could not be read")
	}
	refused := func(int, int, int) (int, error) { return 0, syscall.EPERM }
	filesCompared.Store(&refused)
	t.Cleanup(func() { filesCompared.Store(nil) })
	procs := []Reached{{PID: os.Getpid(), Start: st.StartTime}}
	m := MarkExit(procs)
	if m.Running != 1 || m.Fds < 3 || m.FilesUnsettled || !errors.Is(m.FilesUncompared, syscall.EPERM) ||
		!strings.Contains(m.FilesUncompared.Error(), "process "+strconv.Itoa(os.Getpid())+"'s") {
		t.Fatalf("this process, its files the kernel will not compare, read as %+v; want running, its descriptors counted, settled, and uncompared for EPERM, naming it", m)
	}
	core, logs := observer.New(zap.InfoLevel)
	var note FilesUncomparedNote
	note.Say(zap.New(core), ExitMark{Running: 1, Fds: 3})
	if logs.Len() != 0 {
		t.Fatalf("a reading that compared its files was said: %v", logs.All())
	}
	for range 3 {
		note.Say(zap.New(core), m)
	}
	if all := logs.All(); len(all) != 1 || !strings.Contains(fmt.Sprint(all[0].ContextMap()["error"]), "operation not permitted") {
		t.Fatalf("three readings that counted descriptors were said %d times (%v); want once, with why", len(all), all)
	}
	// A stop says it at its first look: one whose app has exited already.
	exited := make(chan struct{})
	close(exited)
	core, logs = observer.New(zap.InfoLevel)
	if end, _ := WaitExitByEvents(zap.New(core), exited, procs, nil); end != StopExited {
		t.Fatalf("a stop of an app that exited ended %v", end)
	}
	if n := logs.FilterMessageSnippet("as their descriptors").Len(); n != 1 {
		t.Fatalf("a stop whose look counted descriptors said so %d times; want once", n)
	}
}

// A stop whose first look compared the files of the process it waits for,
// and whose later looks could not (the app ran a program its user may not
// read, or changed its credentials), says that it counts descriptors at the
// first of those later looks, naming the process, and not again at the next.
func TestAStopSaysItCountsDescriptorsFromTheLookThatDid(t *testing.T) {
	st, ok := ReadProcStat(os.Getpid())
	if !ok {
		t.Fatal("premise: this process's stat could not be read")
	}
	procs := []Reached{{PID: os.Getpid(), Start: st.StartTime}}
	// Each descriptor is a file of its own, until the kernel refuses.
	compared := func(_, a, b int) (int, error) {
		switch {
		case a < b:
			return 1, nil
		case a > b:
			return 2, nil
		}
		return 0, nil
	}
	refused := func(int, int, int) (int, error) { return 0, syscall.EPERM }
	filesCompared.Store(&compared)
	t.Cleanup(func() { filesCompared.Store(nil) })
	if m := MarkExit(procs); m.Running != 1 || m.FilesUnsettled || m.FilesUncompared != nil {
		t.Fatalf("premise: this process, its files compared, read as %+v; want running, settled, compared", m)
	}
	core, logs := observer.New(zap.InfoLevel)
	said := func() int64 { return int64(logs.FilterMessageSnippet("as their descriptors").Len()) }
	// The stop's first look compares; the kernel refuses from the loop's
	// first look on, and the app exits at the loop's second. Each count is
	// taken as the loop is about to look again: what the looks before said.
	exited := make(chan struct{})
	var looks atomic.Int32
	var atFirst, atSecond atomic.Int64
	looked := func() {
		switch looks.Add(1) {
		case 1:
			atFirst.Store(said())
			filesCompared.Store(&refused)
		case 2:
			atSecond.Store(said())
			close(exited)
		}
	}
	exitLooked.Store(&looked)
	t.Cleanup(func() { exitLooked.Store(nil) })
	if end, _ := WaitExitByEvents(zap.New(core), exited, procs, nil); end != StopExited {
		t.Fatalf("a stop of an app that exited ended %v", end)
	}
	if n := looks.Load(); n < 2 {
		t.Fatalf("the stop looked %d times after its first look; want 2", n)
	}
	if n := atFirst.Load(); n != 0 {
		t.Fatalf("a stop whose first look compared the files said %d times that it counts descriptors; want never", n)
	}
	if n := atSecond.Load(); n != 1 {
		t.Fatalf("a stop whose look counted descriptors had said so %d times by its next look; want once, at that look", n)
	}
	if all := logs.FilterMessageSnippet("as their descriptors").All(); len(all) != 1 ||
		!strings.Contains(fmt.Sprint(all[0].ContextMap()["error"]), "process "+strconv.Itoa(os.Getpid())+"'s descriptors: operation not permitted") {
		t.Fatalf("a stop whose two later looks counted descriptors said so %d times (%v); want once, naming the process and why", len(all), all)
	}
}

// A reading of several processes (a stop of the daemons an app left, or of a
// group) did not settle where the read of one of them did not, whichever is
// read last: its Fds are no evidence.
func TestMarkExitOfSeveralSaysWhereOnesFilesDidNotSettle(t *testing.T) {
	self, ok := ReadProcStat(os.Getpid())
	if !ok {
		t.Fatal("premise: this process's stat could not be read")
	}
	cmd := exec.Command("sleep", "1000")
	// A group of its own, killed whole; and by the kernel when the thread
	// that started it ends (Pdeathsig), so it does not outlive a test binary
	// killed mid-test: the test keeps to this thread until it ends.
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-reaped })
	child, ok := ReadProcStat(cmd.Process.Pid)
	if !ok {
		t.Fatal("premise: the child's stat could not be read")
	}
	if _, err := os.ReadDir("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/fd"); err != nil {
		t.Fatalf("premise: the child's descriptors could not be listed: %v", err)
	}
	procs := []Reached{{PID: os.Getpid(), Start: self.StartTime}, {PID: cmd.Process.Pid, Start: child.StartTime}}
	// The descriptors of task never hold still (closed as each is compared
	// with another, open as it is with itself); every other's is a file of
	// its own.
	var task atomic.Int64
	cmp := func(of, a, b int) (int, error) {
		switch {
		case of == int(task.Load()) && a != b:
			return 0, syscall.EBADF
		case a < b:
			return 1, nil
		case a > b:
			return 2, nil
		}
		return 0, nil
	}
	filesCompared.Store(&cmp)
	t.Cleanup(func() { filesCompared.Store(nil) })
	for _, tc := range []struct {
		name          string
		task          int
		wantUnsettled bool
	}{
		{"both settled", 0, false},
		{"the first read unsettled, the last settled", os.Getpid(), true},
		{"the first read settled, the last unsettled", cmd.Process.Pid, true},
	} {
		task.Store(int64(tc.task))
		if m := MarkExit(procs); m.Running != 2 || m.FilesUnsettled != tc.wantUnsettled {
			t.Errorf("%s: read as %+v; want 2 running, files unsettled %v", tc.name, m, tc.wantUnsettled)
		}
	}
}

// Descriptors the kernel will not list are no evidence: a read of a process
// that is not dumpable (it runs a program its user may not read, so its
// /proc files are root's) did not settle.
func TestMarkExitOfDescriptorsTheKernelWillNotListIsUnsettled(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists every process's descriptors")
	}
	src, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	// Copied by cp, so that no descriptor of this process is open on it
	// when it runs (a fork as it is written would hold one: ETXTBSY).
	bin := filepath.Join(t.TempDir(), "sleep")
	if out, err := exec.Command("cp", src, bin).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v: %s", err, out)
	}
	if err := os.Chmod(bin, 0o111); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "1000")
	// A group of its own, killed whole; and by the kernel when the thread
	// that started it ends (Pdeathsig): the test keeps to this thread.
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-reaped })
	// Start can return before the exec of the copy is done (once it closed
	// this process's descriptors): the kernel sets the process not dumpable
	// as it runs it, then names it sleep, so it is waited for by that name,
	// not by its descriptors hidden. With fs.suid_dumpable=1 (a debugging
	// setting) the kernel leaves such a process dumpable, its descriptors
	// listed.
	proc := "/proc/" + strconv.Itoa(cmd.Process.Pid)
	for {
		if b, err := os.ReadFile(proc + "/stat"); err == nil && strings.Contains(string(b), " (sleep) ") {
			break
		}
		select {
		case <-reaped:
			t.Fatal("the copy of sleep exited before it was named sleep")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := os.ReadDir(proc + "/fd"); !errors.Is(err, fs.ErrPermission) {
		if b, _ := os.ReadFile("/proc/sys/fs/suid_dumpable"); strings.TrimSpace(string(b)) == "1" {
			t.Skipf("fs.suid_dumpable=1 leaves a process that runs a program its user may not read dumpable: its descriptors are listed (%v)", err)
		}
		t.Fatalf("premise: a process running a program its user may not read had its descriptors listed (%v)", err)
	}
	st, ok := ReadProcStat(cmd.Process.Pid)
	if !ok {
		t.Fatal("premise: its stat could not be read")
	}
	if m := MarkExit([]Reached{{PID: cmd.Process.Pid, Start: st.StartTime}}); m.Running != 1 || !m.FilesUnsettled {
		t.Fatalf("a process whose descriptors the kernel will not list read as %+v; want running, its files unsettled", m)
	}
}

// A kernel that cannot compare files (no kcmp, or keploy may not ask: a
// filter may give any errno, the largest being 4095) leaves each descriptor
// counted, the read settled and saying why; one that stops
// comparing part way through a read leaves it unsettled, no evidence. A
// process of one descriptor needs no comparison. A churn of descriptors so
// counted reads as steps out, but moves a stop at most once for each
// descriptor of the reading that set the mark (ExitRatchet): a longer wait,
// but a bounded one.
func TestOpenFilesAreDescriptorsWhereTheKernelCannotCompareThem(t *testing.T) {
	listed := []os.DirEntry{fdEntry("0"), fdEntry("1"), fdEntry("2"), fdEntry("3")}
	for _, no := range []error{syscall.ENOSYS, syscall.EPERM, syscall.EACCES, syscall.EINVAL, syscall.Errno(4095)} {
		cannot := func(int, int, int) (int, error) { return 0, no }
		if n, settled, why := openFilesBy(cannot, 1, listed); n != len(listed) || !settled || !errors.Is(why, no) {
			t.Fatalf("kcmp failing with %v: %d open files of %d descriptors (settled %v, uncompared %v); want each counted, settled, uncompared for %v", no, n, len(listed), settled, why, no)
		}
		if n, settled, why := openFilesBy(cannot, 1, listed[:1]); n != 1 || !settled || why != nil {
			t.Fatalf("kcmp failing with %v: one descriptor read as %d open files (settled %v, uncompared %v); want 1, settled, no comparison asked", no, n, settled, why)
		}
	}
	// The test's churning shell (fd 3 saved to fd 10, then both closed),
	// read at each of its steps over and over: 4, 5, 3 and 4 descriptors.
	// It moves the stop once, from the 4 of the reading that set the mark to
	// its fewest, 3, and never again.
	var r ExitRatchet
	cannot := func(int, int, int) (int, error) { return 0, syscall.EPERM }
	moves := 0
	for range 100 {
		for _, names := range [][]string{{"0", "1", "2", "3"}, {"0", "1", "10", "2", "3"}, {"0", "1", "2"}, {"0", "1", "10", "2"}} {
			var fds []os.DirEntry
			for _, name := range names {
				fds = append(fds, fdEntry(name))
			}
			n, settled, why := openFilesBy(cannot, 1, fds)
			if r.Advance(ExitMark{Running: 1, Threads: 1, Fds: n, FilesUnsettled: !settled, FilesUncompared: why}) {
				moves++
			}
		}
	}
	if moves != 1 {
		t.Fatalf("400 reads of a churn of 3 to 5 descriptors the kernel will not compare, from 4, moved the stop %d times; want 1", moves)
	}
	// One refused part way through a read (as the files are checked, each
	// against the next: the process's credentials changed) did not settle.
	files := map[int]int{0: 1, 1: 2, 2: 2, 3: 3}
	refused := func(_, a, b int) (int, error) {
		switch {
		case a < b:
			return 0, syscall.EPERM
		case files[a] < files[b]:
			return 1, nil
		case files[a] > files[b]:
			return 2, nil
		}
		return 0, nil
	}
	if n, settled, why := openFilesBy(refused, 1, listed); settled || n > 3 || why != nil {
		t.Fatalf("kcmp refused part way through: %d open files of %d descriptors, settled %v, uncompared %v; want at most the 3 files, unsettled, compared", n, len(listed), settled, why)
	}
	// One that reads closed as it is compared with another, then is
	// refused as it is asked of itself, is not taken for open: the read did
	// not settle.
	calls := 0
	selfRefused := func(_, a, b int) (int, error) {
		calls++
		switch {
		case a == b:
			return 0, syscall.EPERM
		case calls == 1:
			return 0, syscall.EBADF
		case files[a] < files[b]:
			return 1, nil
		case files[a] > files[b]:
			return 2, nil
		}
		return 0, nil
	}
	if n, settled, _ := openFilesBy(selfRefused, 1, []os.DirEntry{fdEntry("0"), fdEntry("3")}); settled || n > 2 {
		t.Fatalf("kcmp refused asking a descriptor of itself: %d open files, settled %v; want at most 2, unsettled", n, settled)
	}
}

// A first comparison that fails in passing, not as a refusal (ESRCH: the
// process, or the thread read for it, was released after its descriptors
// were listed, as kcmp finds both tasks before it asks whether keploy may
// read them; EINTR: the comparison was interrupted), is a read that did not
// settle: no evidence, and no word of the host, whose kernel compares files.
// A stop that looks so says nothing of it.
func TestAFirstComparisonThatFailsInPassingIsNoRefusal(t *testing.T) {
	listed := []os.DirEntry{fdEntry("0"), fdEntry("1"), fdEntry("2"), fdEntry("3")}
	st, ok := ReadProcStat(os.Getpid())
	if !ok {
		t.Fatal("premise: this process's stat could not be read")
	}
	procs := []Reached{{PID: os.Getpid(), Start: st.StartTime}}
	t.Cleanup(func() { filesCompared.Store(nil) })
	for _, no := range []error{syscall.ESRCH, syscall.EINTR} {
		failing := func(int, int, int) (int, error) { return 0, no }
		if n, settled, why := openFilesBy(failing, 1, listed); settled || why != nil {
			t.Fatalf("kcmp failing with %v from the first comparison: %d open files, settled %v, uncompared %v; want unsettled, no refusal", no, n, settled, why)
		}
		filesCompared.Store(&failing)
		m := MarkExit(procs)
		if m.Running != 1 || !m.FilesUnsettled || m.FilesUncompared != nil {
			t.Fatalf("this process, its first comparison failing with %v, read as %+v; want running, its files unsettled, no refusal", no, m)
		}
		core, logs := observer.New(zap.InfoLevel)
		exited := make(chan struct{})
		close(exited)
		if end, _ := WaitExitByEvents(zap.New(core), exited, procs, nil); end != StopExited {
			t.Fatalf("a stop of an app that exited ended %v", end)
		}
		if all := logs.FilterMessageSnippet("as their descriptors").All(); len(all) != 0 {
			t.Fatalf("readings whose first comparison failed with %v said the host's kernel will not compare files: %v", no, all)
		}
	}
}

// The same on the kernel: a child's descriptors listed, the child killed and
// reaped, then its files read from that listing, as MarkExit reads them once
// it has listed them (an app that exits on the stop's signal, reaped at once
// by its parent, as keploy reaps the app it runs). Where the kernel compares
// a running child's files, this read did not settle and says no refusal.
// Where it does not, the read of the child gone is a refusal again (a
// seccomp filter refuses before kcmp finds the task) or did not settle (a
// ptrace rule is asked after); KEPLOY_TEST_KCMP_REQUIRED=1 fails it there.
func TestAChildReapedBeforeItsFilesAreComparedIsNoRefusal(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("/bin/sh", "-c", `exec 3</dev/null 4</dev/null; : > "$1"; exec sleep 1000`, "sh", ready)
	// Killed by the kernel too when the thread that started it ends
	// (Pdeathsig), so a test binary killed mid-test leaves no sleep behind:
	// the test keeps to this thread until it ends.
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); <-reaped })
	awaitFile(t, ready, reaped)
	fds, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/fd")
	if err != nil {
		t.Fatalf("premise: the child's descriptors could not be listed: %v", err)
	}
	_, _, before := openFiles(pid, fds)
	refusal := ""
	if before != nil {
		refusal = kcmpRefusal(pid) // read while the child runs
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	<-reaped
	n, settled, why := openFiles(pid, fds)
	if before == nil {
		if settled || why != nil {
			t.Fatalf("a child reaped after its %d descriptors were listed read as %d open files, settled %v, uncompared %v; want unsettled, no refusal: the kernel compared its files while it ran", len(fds), n, settled, why)
		}
		return
	}
	if os.Getenv("KEPLOY_TEST_KCMP_REQUIRED") == "1" {
		t.Fatalf("KEPLOY_TEST_KCMP_REQUIRED=1, but the kernel refuses to compare a child's files (%v): %s", before, refusal)
	}
	t.Logf("the kernel refuses to compare a child's files (%v: %s); reaped, its read is %d, settled %v, uncompared %v", before, refusal, n, settled, why)
	if why != nil && !errors.Is(why, before) || why == nil && settled {
		t.Fatalf("a child reaped after its files were refused (%v) read as %d open files, settled %v, uncompared %v; want the same refusal, or unsettled", before, n, settled, why)
	}
}

// A task has begun to exit where its stat says it is a zombie or dead, or
// has PF_EXITING (0x4) set, whatever its state: keploy#4999's CI read a
// task with flags 4227084 (0x40800C) as S. The flag is the kernel's 0x4,
// written so here, not as pfExiting, which a row would only test against
// itself. 0x8 alone is no exit: from 5.16 it is PF_POSTCOREDUMP, which
// do_exit sets just before PF_EXITING (so on the CI kernels the two mark
// one window, and a real-kernel row cannot tell them apart), but 5.10,
// the kernel floor, sets no 0x8 as a task exits, and a task with only
// that bit still holds its files.
func TestATaskThatBeganToExitReadsSo(t *testing.T) {
	for _, tc := range []struct {
		state byte
		flags uint32
		want  bool
	}{
		{'S', 4194560, false}, // 0x400100, a sleeping process's
		{'R', 4194304, false},
		{'D', 4194560, false},
		{'S', 0x400008, false}, // PF_POSTCOREDUMP alone: before exit_signals
		{'S', 4227084, true},
		{'R', 4227084, true},
		{'D', 0x4, true},
		{'Z', 4194560, true},
		{'X', 0, true},
	} {
		if got := exiting(ProcStat{State: tc.state, Flags: tc.flags}); got != tc.want {
			t.Errorf("a task of state %c and flags %#x read as exiting %v; want %v", tc.state, tc.flags, got, tc.want)
		}
	}
}

// A read of a process's files through a task that began to exit as they were
// read (a thread that let its files go, exit_files, before it is released:
// its descriptors then list as none, and kcmp of every one is EBADF) is no
// evidence, whatever the comparisons said: the files may be held still by
// another thread of the process. Here a child is killed as its files are
// read, and is a zombie, not reaped, when the read ends. Its comparisons
// fail with EBADF on every one, as they do through a task that let its
// files go, which alone would read as every descriptor but the first
// closed; with ESRCH, as for a task released; with EINTR, which kcmp gives
// where keploy's own thread takes a fatal signal as it waits on the task
// (kcmp_lock), so not from the task, but which reads, as ESRCH does, as
// no evidence (kcmpRefused); and with EPERM, a refusal, where each
// descriptor counts: the read through the exiting task is no evidence
// still, and its refusal is kept, so the stop says once why it counts
// descriptors.
func TestAReadThroughATaskThatBeganToExitIsUnsettled(t *testing.T) {
	// Killed by the kernel too when the thread that started it ends
	// (Pdeathsig), so a test binary killed mid-test leaves no sleep behind:
	// the test keeps to this thread until it ends.
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	t.Cleanup(func() { filesCompared.Store(nil) })
	for _, no := range []error{syscall.ESRCH, syscall.EINTR, syscall.EBADF, syscall.EPERM} {
		ready := filepath.Join(t.TempDir(), "ready")
		cmd := exec.Command("/bin/sh", "-c", `exec 3</dev/null 4</dev/null; : > "$1"; exec sleep 1000`, "sh", ready)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		reaped := make(chan struct{})
		// Reaped only once it has been read: until then it is a zombie.
		reap := func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _ = cmd.Wait(); close(reaped) }
		t.Cleanup(func() {
			select {
			case <-reaped:
			default:
				reap()
			}
		})
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			var info unix.Siginfo
			if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil); err != nil || info.Signo != 0 {
				t.Fatalf("premise: the child exited before it was ready (%v)", err)
			}
			runtime.Gosched()
		}
		st, ok := ReadProcStat(pid)
		if !ok {
			t.Fatal("premise: the child's stat could not be read")
		}
		calls := 0
		dying := func(int, int, int) (int, error) {
			if calls++; calls == 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				var info unix.Siginfo
				if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
					t.Errorf("waiting for the child to be a zombie: %v", err)
				}
			}
			return 0, no
		}
		filesCompared.Store(&dying)
		m := MarkExit([]Reached{{PID: pid, Start: st.StartTime}})
		filesCompared.Store(nil)
		refused := no == syscall.EPERM
		if calls == 0 || m.Running != 1 || !m.FilesUnsettled || refused != (m.FilesUncompared != nil) || refused && !errors.Is(m.FilesUncompared, no) {
			want := "no refusal"
			if refused {
				want = "the refusal kept"
			}
			t.Fatalf("a child killed as its files were read, its comparisons failing with %v, read as %+v after %d comparisons; want running as it was first read, its files unsettled, %s", no, m, calls, want)
		}
		reap()
	}
}

// The same on the kernel: a process whose main thread exited holds its files
// through its other threads, read through one of them (MarkExit), as they
// exit one by one, 0.3 ms apart. No reading settles below the files the
// process held while ten or more of its threads were left after it.
func TestAReadThroughExitingThreadsNeverSettlesLow(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler (cc) to build the process of many threads")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "threads.c")
	// Its threads wait on fd 3, a pipe, until the test closes its end. It
	// closes what it was left beside it, and opens /dev/null five times.
	if err := os.WriteFile(src, []byte(`#include <pthread.h>
#include <stdlib.h>
#include <unistd.h>
#include <fcntl.h>
static void *run(void *a) {
	char c;
	while (read(3, &c, 1) > 0) {}
	usleep((long)a * 300);
	return 0;
}
int main(int argc, char **argv) {
	long n = atol(argv[1]);
	for (int fd = 4; fd < 4096; fd++) close(fd);
	for (int i = 0; i < 5; i++) open("/dev/null", O_RDONLY);
	pthread_t t;
	for (long i = 0; i < n; i++)
		if (pthread_create(&t, 0, run, (void *)i)) return 1;
	pthread_exit(0);
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "threads")
	if out, err := exec.Command(cc, "-pthread", "-O1", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("building the process of many threads: %v: %s", err, out)
	}
	// Killed by the kernel too when the thread that started it ends
	// (Pdeathsig), so a test binary killed mid-test leaves none behind.
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	const processes, threads = 20, 200
	settledLooks, low := 0, map[int]int{}
	for range processes {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, strconv.Itoa(threads))
		cmd.ExtraFiles = []*os.File{r}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		pid := cmd.Process.Pid
		reaped := make(chan struct{})
		go func() { _ = cmd.Wait(); close(reaped) }()
		t.Cleanup(func() { _ = w.Close(); _ = syscall.Kill(-pid, syscall.SIGKILL); <-reaped })
		// Its threads wait on the pipe, so it stays until it is let go.
		var st ProcStat
		for {
			s, ok := ReadProcStat(pid)
			if !ok {
				t.Fatal("premise: the process of many threads ended before its main thread read as exited")
			}
			if s.State == 'Z' {
				st = s
				break
			}
			runtime.Gosched()
		}
		procs := []Reached{{PID: pid, Start: st.StartTime}}
		held := MarkExit(procs)
		if held.Running != 1 || held.FilesUnsettled || held.Fds < 6 {
			t.Fatalf("premise: the process of many threads, its main thread exited and the others waiting, read as %+v; want running, at least its pipe and five /dev/null, settled", held)
		}
		_ = w.Close()
		for {
			m := MarkExit(procs)
			if m.Running == 0 {
				break
			}
			if m.FilesUnsettled {
				continue
			}
			settledLooks++
			if m.Fds < held.Fds && processThreads(pid) >= 10 {
				low[m.Fds]++
			}
		}
		<-reaped
	}
	if len(low) > 0 {
		t.Fatalf("%d processes of %d threads exiting: settled readings below the files held, read while ten or more threads were left after, by count read: %v", processes, threads, low)
	}
	if settledLooks == 0 {
		t.Fatalf("%d processes of %d threads exiting: no reading as they exited settled; the row read nothing", processes, threads)
	}
}

// A descriptor closed while its process's files are read is not held: one
// listed and closed before it is compared is not counted, and one counted
// already and closed since gives way to the descriptor compared with it. One
// that closes and opens again as they are read (a churn, as dash's
// `exec 3<&-` makes) takes none of the other files with it, nor counts its
// file twice: the files read stay between the fewest and the most the
// process held as they were read. One that never holds still spends its own
// share of the comparisons, not the others': it is set aside, the read did
// not settle, and the stable files are still each counted once. The
// descriptors are listed as os.ReadDir lists /proc/<pid>/fd, by name (0 1 10
// 2 3), as the product reads them.
func TestOpenFilesLetGoOfADescriptorClosedAsTheyAreRead(t *testing.T) {
	const closed = -1
	for _, tc := range []struct {
		name  string
		files map[int]int // descriptor to file, as the read begins; files order as their numbers
		// at[fd][n] changes descriptors (to the file each names from then
		// on, or closed) as the read compares fd for the n+1st time.
		at map[int][]map[int]int
		// churn, from its churnFrom+1st comparison on, is closed as it is
		// compared with another, and open on a new file as it is with
		// itself.
		churn, churnFrom int
		want             int
		unsettled        bool
	}{
		{"one file over two descriptors", map[int]int{0: 1, 1: 2, 2: 2, 3: 3, 10: 3}, nil, -1, 0, 3, false},
		{"a duplicate of a file read before the last", map[int]int{0: 2, 1: 1, 5: 2}, nil, -1, 0, 2, false},
		{"a listed descriptor closed before it is compared", map[int]int{0: 1, 1: 2, 2: 2, 3: 3},
			map[int][]map[int]int{3: {{3: closed}}}, -1, 0, 2, false},
		{"a counted descriptor closed, its duplicate read after", map[int]int{0: 1, 1: 2, 3: 3, 4: 3},
			map[int][]map[int]int{4: {{3: closed}}}, -1, 0, 3, false},
		{"a counted descriptor closed, its duplicate and another file read after", map[int]int{0: 1, 3: 3, 4: 3, 5: 5},
			map[int][]map[int]int{4: {{3: closed}}}, -1, 0, 3, false},
		{"a counted descriptor closed, another file read after", map[int]int{0: 1, 3: 3, 4: 4},
			map[int][]map[int]int{4: {{3: closed}}}, -1, 0, 2, false},
		// Closed as it is compared with a counted one, open again as it is
		// asked of itself: the counted one is not let go for it.
		{"a descriptor closed and opened again between two looks at it", map[int]int{0: 1, 1: 2, 2: 2, 3: 3},
			map[int][]map[int]int{3: {{3: closed}, {3: 4}, {3: closed}}}, -1, 0, 2, false},
		// fd 3 and its duplicate fd 4 closed, and fd 3 opened again on a
		// file that orders after both others, then duplicated to fd 4: fd
		// 3 is held where its first file ordered, and fd 4 is not found
		// there.
		{"a counted descriptor opened again on a file of another order", map[int]int{0: 10, 1: 20, 3: 5, 4: 5},
			map[int][]map[int]int{4: {{3: 25, 4: 25}}}, -1, 0, 3, false},
		{"a counted file whose duplicate is opened again on another file", map[int]int{0: 10, 3: 5, 4: 5, 5: 30},
			map[int][]map[int]int{5: {{3: closed, 4: 25}}}, -1, 0, 3, false},
		{"a counted descriptor closed as the files are checked", map[int]int{0: 1, 1: 2, 3: 3},
			map[int][]map[int]int{3: {nil, {3: closed}}}, -1, 0, 2, false},
		// The first of two compared closed as the files are checked: it is
		// let go, and the read settles.
		{"the first of two counted descriptors closed as the files are checked", map[int]int{0: 1, 1: 2, 3: 3},
			map[int][]map[int]int{1: {nil, nil, nil, {1: closed}}}, -1, 0, 2, false},
		// Closed as the files are checked, its duplicate holding its file
		// yet: the duplicate is read again, and counted.
		{"a counted descriptor closed as the files are checked, its duplicate holding its file yet", map[int]int{0: 1, 3: 3, 4: 3},
			map[int][]map[int]int{3: {nil, nil, {3: closed}}}, -1, 0, 2, false},
		// fd 3 opened again on a file that orders first as the files are
		// checked, and fd 7 closed: fd 4 holds fd 3's first file yet.
		{"a counted descriptor opened again on a file ordering first, its duplicate holding its file yet", map[int]int{0: 10, 3: 30, 4: 30, 7: 40},
			map[int][]map[int]int{3: {nil, nil, nil, {3: 5, 7: closed}}}, -1, 0, 3, false},
		// fd 0 opened again on a file that orders after fd 3 as the files
		// are checked: fd 0 is the one of the pair that changed, and fd 1
		// holds its first file yet.
		{"a counted descriptor opened again on a file ordering after the next, its duplicate holding its file yet", map[int]int{0: 10, 1: 10, 3: 30, 7: 40},
			map[int][]map[int]int{0: {nil, nil, {0: 35}}}, -1, 0, 4, false},
		// fd 4 made a duplicate of fd 3 as the files are checked (`exec
		// 4>&3`): one file over two held descriptors is read again, and
		// counted once.
		{"a counted descriptor opened again on the file of the one before it as the files are checked", map[int]int{0: 1, 3: 3, 4: 4},
			map[int][]map[int]int{4: {nil, {4: 3}}}, -1, 0, 2, false},
		// Never holding still: set aside, the stable files counted.
		{"a descriptor that never holds still as it is read", map[int]int{0: 1, 1: 2, 2: 2, 3: 3}, nil, 3, 0, 2, true},
		// dash's saved descriptor fd 10 lists before fd 2, the duplicate of
		// stdout: it spends its own share, and fd 2 is still found to be
		// stdout's file.
		{"a churning descriptor listed before a stable duplicate", map[int]int{0: 1, 1: 2, 2: 2, 10: 3}, nil, 10, 0, 2, true},
		{"a churning saved descriptor listed before a stable duplicate and its own", map[int]int{0: 1, 1: 2, 2: 2, 3: 3, 10: 3}, nil, 10, 0, 3, true},
		// A counted descriptor that starts to churn as the files are
		// checked: set aside, its duplicate holds and counts its file.
		{"a counted descriptor churning from the check of the files, its duplicate holding its file", map[int]int{0: 1, 3: 3, 4: 3}, nil, 3, 2, 2, true},
		// The same, its share spent at a comparison with the one before it
		// (its search for its place took two): set aside there too.
		{"a counted descriptor whose share runs out as it is compared with the one before it", map[int]int{0: 1, 1: 5, 3: 3, 4: 3}, nil, 3, 3, 3, true},
	} {
		files := map[int]int{}
		var names []string
		for fd, file := range tc.files {
			files[fd] = file
			names = append(names, strconv.Itoa(fd))
		}
		sort.Strings(names)
		seen, next := map[int]int{}, 100
		change := func(fd int) {
			if steps := tc.at[fd]; seen[fd] < len(steps) {
				for d, f := range steps[seen[fd]] {
					if f == closed {
						delete(files, d)
					} else {
						files[d] = f
					}
				}
			}
			seen[fd]++
		}
		kcmp := func(_, a, b int) (int, error) {
			change(a)
			if b != a {
				change(b)
			}
			if (a == tc.churn || b == tc.churn) && seen[tc.churn] > tc.churnFrom {
				if a == b {
					next++
					files[a] = next
				} else {
					delete(files, tc.churn)
				}
			}
			fa, okA := files[a]
			fb, okB := files[b]
			switch {
			case !okA || !okB:
				return 0, syscall.EBADF
			case fa < fb:
				return 1, nil
			case fa > fb:
				return 2, nil
			}
			return 0, nil
		}
		var listed []os.DirEntry
		for _, n := range names {
			listed = append(listed, fdEntry(n))
		}
		if n, settled, _ := openFilesBy(kcmp, 1, listed); n != tc.want || settled == tc.unsettled {
			t.Errorf("%s (listed %v): %d open files, settled %v; want %d, settled %v", tc.name, names, n, settled, tc.want, !tc.unsettled)
		}
	}
}

// A read of files that hold still ends with two checks of their order, each
// n-1 comparisons of the n files held, one after the other: it counts once
// two checks in a row are clean, not after one (which a churn can fool) nor
// after more (each costs n-1 comparisons, at each look of a stop). A check
// that lets go of a descriptor closed since the last is not clean: two clean
// checks of the files left follow it.
func TestAReadOfFilesThatHoldStillChecksTheirOrderTwice(t *testing.T) {
	for _, tc := range []struct {
		name string
		// closeAt closes fd 3 as it is compared for that time (0: never).
		closeAt int
		want    int
		check   [][2]int
	}{
		// Listed by name, the files in that order: each check compares 0
		// and 1, 1 and 2, then 2 and 3. The holds compare a descriptor
		// listed later first, so no comparison of theirs reads as one of a
		// check.
		{"4 files that held still", 0, 4, [][2]int{{0, 1}, {1, 2}, {2, 3}}},
		// fd 3 is compared twice as it is held, then in the first check,
		// and closes as the second compares it.
		{"4 files, one closed after a clean check", 4, 3, [][2]int{{0, 1}, {1, 2}}},
	} {
		files := map[int]int{0: 1, 1: 2, 2: 3, 3: 4}
		var pairs [][2]int
		looks := 0
		kcmp := func(_, a, b int) (int, error) {
			pairs = append(pairs, [2]int{a, b})
			if a == 3 || b == 3 {
				if looks++; looks == tc.closeAt {
					delete(files, 3)
				}
			}
			fa, okA := files[a]
			fb, okB := files[b]
			switch {
			case !okA || !okB:
				return 0, syscall.EBADF
			case fa < fb:
				return 1, nil
			case fa > fb:
				return 2, nil
			}
			return 0, nil
		}
		listed := []os.DirEntry{fdEntry("0"), fdEntry("1"), fdEntry("2"), fdEntry("3")}
		if n, settled, _ := openFilesBy(kcmp, 1, listed); n != tc.want || !settled {
			t.Errorf("%s: read as %d, settled %v; want %d, settled", tc.name, n, settled, tc.want)
			continue
		}
		checks := 0
		for len(pairs) >= len(tc.check) && slices.Equal(pairs[len(pairs)-len(tc.check):], tc.check) {
			pairs = pairs[:len(pairs)-len(tc.check)]
			checks++
		}
		if checks != 2 {
			t.Errorf("%s: the read ended with %d checks of the order of the files left; want 2", tc.name, checks)
		}
	}
}

// A pair of held files found out of order is let go before both are read
// again: neither is compared with its own place, which names a file it may
// not hold now, and spends its share on that look (the sooner unsettled).
func TestAPairOutOfOrderIsLetGoBeforeItIsReadAgain(t *testing.T) {
	// As "a counted descriptor opened again on a file ordering after the
	// next, its duplicate holding its file yet": fd 0, held with fd 1 its
	// duplicate, is opened again on a file ordering after fd 3 as the
	// first check compares it (its third comparison).
	files := map[int]int{0: 10, 1: 10, 3: 30, 7: 40}
	looks := 0
	var self [][2]int
	kcmp := func(_, a, b int) (int, error) {
		if a == 0 || b == 0 {
			if looks++; looks == 3 {
				files[0] = 35
			}
		}
		if a == b {
			self = append(self, [2]int{a, b})
		}
		switch fa, fb := files[a], files[b]; {
		case fa < fb:
			return 1, nil
		case fa > fb:
			return 2, nil
		}
		return 0, nil
	}
	listed := []os.DirEntry{fdEntry("0"), fdEntry("1"), fdEntry("3"), fdEntry("7")}
	if n, settled, _ := openFilesBy(kcmp, 1, listed); n != 4 || !settled {
		t.Errorf("read as %d, settled %v; want 4, settled", n, settled)
	}
	if len(self) > 0 {
		t.Errorf("with no descriptor closed, the read compared %v, each a descriptor with itself", self)
	}
}

// churnShell is the adopt test's daemon, dash running `exec 3</dev/null;
// exec 3<&-` (open fd 3 on a new file, dup fd 3 to fd 10, close fd 3, close
// fd 10), as a fake kcmp: listed (as os.ReadDir lists them: 0 1 10 2 3) with
// fds 3 and 10 open on its first cycle's file, it takes steps[n] of those
// steps as the read makes its n+1st comparison, and none past the last.
// Stdin and stdout (stderr its duplicate) are two files; each cycle's file
// orders before both or after both, in turn. peak is the most open files it
// held at any comparison.
func churnShell(steps []int) (kcmp func(task, a, b int) (int, error), listed []os.DirEntry, peak *int) {
	const stdin, stdout = 10, 20
	cycle := []int{1, 30, 2, 40, 3, 50, 4, 60}
	fds := map[int]int{0: stdin, 1: stdout, 2: stdout, 3: cycle[0], 10: cycle[0]}
	k, at, calls, most := 1, 2, 0, 0
	step := func() {
		switch at {
		case 0:
			fds[3] = cycle[k%len(cycle)]
			k++
		case 1:
			fds[10] = fds[3]
		case 2:
			delete(fds, 3)
		case 3:
			delete(fds, 10)
		}
		at = (at + 1) % 4
	}
	for _, n := range []string{"0", "1", "10", "2", "3"} {
		listed = append(listed, fdEntry(n))
	}
	return func(_, a, b int) (int, error) {
		if calls < len(steps) {
			for range steps[calls] {
				step()
			}
		}
		calls++
		files := map[int]bool{}
		for _, f := range fds {
			files[f] = true
		}
		most = max(most, len(files))
		fa, okA := fds[a]
		fb, okB := fds[b]
		switch {
		case !okA || !okB:
			return 0, syscall.EBADF
		case fa < fb:
			return 1, nil
		case fa > fb:
			return 2, nil
		}
		return 0, nil
	}, listed, &most
}

// The adopt test's daemon read as it churns, stepping on by 0 or 1 of its
// syscalls between two of keploy's comparisons, as it does: a settled read
// counts no more files than the shell held at any comparison, so as a stop's
// first reading, followed by the true 3, 2, 3, 2, it moves the stop once at
// most. Both rows read 4, settled, on a read that took one clean check of
// the files' order for settled (kcmp compares two at a time, never all at
// one instant):
//   - fd 10, held at its first file's place, is out of order beside fd 0 a
//     cycle later; read again alone, fd 10 and fd 3 were never compared at
//     one instant (both are read again);
//   - fd 10 passes its check and closes before fd 3, opened on the next
//     cycle's file, passes its own (the next check finds fd 10 closed, or
//     out of order on the file it names then).
func TestOpenFilesOfAChurningShellMoveAStopOnceAtMost(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []int
	}{
		{"fd 10 out of order beside fd 0", []int{0, 0, 1, 0, 1, 0, 1, 0, 0, 1, 1, 1, 0, 1, 1, 1, 0, 0, 1, 1}},
		{"fd 10 checked and closed before fd 3 on the next file is checked", []int{1, 0, 0, 1, 1, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1}},
	} {
		kcmp, listed, peak := churnShell(tc.steps)
		n, settled, _ := openFilesBy(kcmp, 1, listed)
		if settled && n > *peak {
			t.Errorf("%s: a shell that held at most %d open files read as %d, settled", tc.name, *peak, n)
		}
		var r ExitRatchet
		moves := 0
		for _, m := range []ExitMark{{Running: 1, Fds: n, FilesUnsettled: !settled}, {Running: 1, Fds: 3}, {Running: 1, Fds: 2}, {Running: 1, Fds: 3}, {Running: 1, Fds: 2}} {
			if r.Advance(m) {
				moves++
			}
		}
		if moves > 1 {
			t.Errorf("%s: read as %d (settled %v) first, then 3, 2, 3, 2, a daemon that only churned moved the stop %d times", tc.name, n, settled, moves)
		}
	}
}

// fdEntry is a listed descriptor, as os.ReadDir lists /proc/<pid>/fd.
type fdEntry string

func (e fdEntry) Name() string               { return string(e) }
func (e fdEntry) IsDir() bool                { return false }
func (e fdEntry) Type() os.FileMode          { return os.ModeSymlink }
func (e fdEntry) Info() (os.FileInfo, error) { return nil, os.ErrNotExist }

// A group's leader is waited for as its signal reached it: one not among
// what the signal reached (gone, or a zombie, by then), or whose pid now
// names another process, has exited; the one it reached exits on its exit
// event.
func TestAGroupsLeaderIsWaitedForAsTheSignalReachedIt(t *testing.T) {
	cmd, reaped := startGroupLeader(t, "sleep", "600")
	pid := cmd.Process.Pid
	st, ok := ReadProcStat(pid)
	if !ok {
		t.Fatal("cannot read the leader")
	}
	isClosed := func(c <-chan struct{}) bool {
		select {
		case <-c:
			return true
		case <-time.After(300 * time.Millisecond):
			return false
		}
	}
	for _, tc := range []struct {
		name    string
		members []Reached
		exited  bool
	}{
		{"not among those reached", []Reached{{PID: pid + 1, Start: st.StartTime}}, true},
		{"its pid naming another process", []Reached{{PID: pid, Start: st.StartTime + 1}}, true},
		{"the one reached, running", []Reached{{PID: pid, Start: st.StartTime}}, false},
	} {
		exited, release := leaderExited(pid, tc.members)
		if got := isClosed(exited); got != tc.exited {
			t.Fatalf("%s: exited %v, want %v", tc.name, got, tc.exited)
		}
		release()
	}
	exited, release := leaderExited(pid, []Reached{{PID: pid, Start: st.StartTime}})
	defer release()
	_ = syscall.Kill(pid, syscall.SIGKILL)
	select {
	case <-exited:
	case <-time.After(time.Minute):
		t.Fatal("the leader's exit was not seen")
	}
	awaitReaped(t, "the leader", reaped)
}

// The processes a stop watches all gone is the app's exit, though its event
// has not come (a starved daemon slow to say its container stopped); and at
// the user's Ctrl+C, which the stop offers once the run is interrupted, a
// group's stop kills at once, and says so as the user's. The press comes at
// the offer, from its log line (pressAtOffer). The stall bound is 10 s, so a
// stop that missed either fails the test (it ends stalled), not holds it to
// go test's timeout, as the hour's bound and a press tried every 5 ms did.
func TestAStopEndsOnItsProcessesGoneAndAtAPress(t *testing.T) {
	shortStall(t, 10*time.Second)
	cmd, reaped := startGroupLeader(t, "sh", "-c", "sleep 1")
	p, ok := ProcOf(cmd.Process.Pid)
	if !ok {
		t.Fatal("cannot read the process")
	}
	never := make(chan struct{})
	if end, _ := WaitExitByEvents(zap.NewNop(), never, []Reached{p}, nil); end != StopExited {
		t.Fatalf("its processes gone: the wait ended %v, want StopExited", end)
	}
	awaitReaped(t, "the process", reaped)

	pid, reaped := readyLeader(t, t.TempDir(), `trap "" INT; touch ready; exec sleep 600`)
	logger, taken := pressAtOffer(t)
	killed, _, err := InterruptProcessTreeStop(logger, pid, syscall.SIGINT, nil)
	if err != nil {
		t.Fatal(err)
	}
	if killed != "keploy killed a process of it at the press of Ctrl+C, before it had exited" || !taken.Load() {
		t.Fatalf("the stop said %q, the press taken %v; want the press's kill", killed, taken.Load())
	}
	awaitReaped(t, "the app", reaped)
}

// pressAtOffer is a logger whose first offer to end a stop at the next
// Ctrl+C ("press Ctrl+C again") is answered by a press, as the user's, in
// the offer's own look: the press only a standing offer takes
// (MarkInterruptedAgain). It sets the offer at the run's interrupt
// (StopOffer 0, the run interrupted) for the test. taken is whether the
// press was taken; read it once the stop is over.
func pressAtOffer(t *testing.T) (logger *zap.Logger, taken *atomic.Bool) {
	t.Helper()
	offer := StopOffer
	StopOffer = 0
	t.Cleanup(func() { StopOffer = offer; ClearInterrupted(); ResetInterruptedAgain() })
	MarkInterrupted()
	taken = &atomic.Bool{}
	var offered atomic.Bool
	core, _ := observer.New(zapcore.InfoLevel)
	logger = zap.New(zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
		if strings.Contains(e.Message, "press Ctrl+C again") && offered.CompareAndSwap(false, true) {
			taken.Store(MarkInterruptedAgain())
		}
		return nil
	}))
	return logger, taken
}

// A process is named by its pid and start time: a pid that names another
// now, or none, has exited, and is no process to read.
func TestAProcessIsNamedByItsStart(t *testing.T) {
	p, ok := ProcOf(os.Getpid())
	if !ok {
		t.Fatal("cannot read this process")
	}
	exited, release := ExitOf(Reached{PID: p.PID, Start: p.Start + 1})
	defer release()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("a pid naming another process was watched")
	}
	if _, ok := ProcOf(0); ok {
		t.Fatal("pid 0 read as a process")
	}
}

// Each step an app's processes take on their way out (a file closed, a
// thread or a process gone) moves the run's teardown scope, which the run's
// drains wait on; a process that only stays is no movement. The app takes
// its step once the stop has looked at it once: by then the stop has its
// mark of where the app began, and a step before that mark is in it, no step
// the stop sees. Taken at the test's own pace, a step every fifth of a
// second, the app's five steps were all over before a stop slowed by a
// loaded host took that mark, and it saw none.
func TestAStopsStepsOutMoveTheRunsTeardown(t *testing.T) {
	shortStall(t, time.Hour)
	var looks atomic.Uint64
	looked := func() { looks.Add(1) }
	exitLooked.Store(&looked)
	defer exitLooked.Store(nil)
	dir := t.TempDir()
	// It closes a file of its own at "step", and exits at "go".
	cmd, reaped := startGroupLeader(t, "sh", "-c", `cd `+dir+` && exec 3</dev/null
: > ready
while [ ! -e step ]; do sleep 0.01; done; exec 3<&-
while [ ! -e go ]; do sleep 0.01; done`)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the app did not start")
		}
	}
	p, ok := ProcOf(cmd.Process.Pid)
	if !ok {
		t.Fatal("cannot read the process")
	}
	scope := &TeardownScope{}
	ended := make(chan StopEnd, 1)
	go func() {
		end, _ := WaitExitByEvents(zap.NewNop(), make(chan struct{}), []Reached{p}, scope)
		ended <- end
	}()
	// awaitLooks waits until the stop has looked at the app n times; the
	// test's own timeout is its only bound.
	awaitLooks := func(n uint64, when string) {
		for looks.Load() < n {
			select {
			case end := <-ended:
				t.Fatalf("the wait ended %v %s", end, when)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	awaitLooks(1, "before its first look")
	if err := os.WriteFile(filepath.Join(dir, "step"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fd3 := filepath.Join("/proc", strconv.Itoa(p.PID), "fd", "3")
	for {
		if _, err := os.Lstat(fd3); errors.Is(err, fs.ErrNotExist) {
			break
		}
		select {
		case end := <-ended:
			t.Fatalf("the wait ended %v before the app's step", end)
		case <-reaped:
			t.Fatal("the app exited before its step")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// A look that began after the step reads the app past it, and the look
	// after that begins once that one is over.
	awaitLooks(looks.Load()+2, "before it looked at the app past its step")
	if scope.Moves() == 0 {
		t.Fatal("the app's step out did not move the run's teardown")
	}
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case end := <-ended:
		if end != StopExited {
			t.Fatalf("the wait ended %v, want StopExited", end)
		}
	case <-time.After(time.Minute):
		t.Fatal("the wait did not end with the app's exit")
	}
	awaitReaped(t, "the app", reaped)
}

// MarkExit counts as running a process whose stat says Z and whose threads
// could not be read, as Exited does; read, the same zombie has exited.
func TestMarkExitCountsAProcessWhoseThreadsCouldNotBeReadAsRunning(t *testing.T) {
	pid, st := startZombie(t)
	procs := []Reached{{PID: pid, Start: st.StartTime}}
	if m := MarkExit(procs); m.Running != 0 {
		t.Fatalf("zombie %d, its threads read: %d running; want 0", pid, m.Running)
	}
	threadsUnreadable(t)
	if m := MarkExit(procs); m.Running != 1 {
		t.Fatalf("zombie %d, its threads not read: %d running; want 1: the read failed, so it is not known to have ended", pid, m.Running)
	}
}
