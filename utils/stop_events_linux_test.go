//go:build linux

package utils

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
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
// The child is held from its start until the stop has looked at the app three
// times as often as a stall is judged on, spinning until the test releases
// it, and had a fraction of the stall bound of CPU meanwhile, or the test
// held nothing; let go, it ends, the app with it, and the stop says no kill.
func TestAStopDoesNotJudgeAnAppWaitingOnAChildHeldOffTheCPU(t *testing.T) {
	starvetest.TakesACPU(t)
	const bound = 300 * time.Millisecond
	shortStall(t, bound)
	var looks atomic.Uint64
	looked := func() { looks.Add(1) }
	exitLooked.Store(&looked)
	defer exitLooked.Store(nil)
	dir := starvetest.MemDir(t)
	// The child says its pid through a FIFO, read as it writes: held open
	// for reading and writing here, its open waits for no writer.
	fifo := filepath.Join(dir, "child")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	said, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer said.Close()
	// Both its loops end with the test, however it ends (ShellSpin).
	pid, reaped := readyLeader(t, dir, `trap 'sh -c "echo \$\$ > child; `+starvetest.ShellSpin("[ ! -e release ]")+`"; exit 0' INT; touch ready; `+
		"while kill -0 "+strconv.Itoa(os.Getpid())+"; do sleep 0.01; done")
	type stopped struct {
		killed string
		err    error
	}
	done := make(chan stopped, 1)
	go func() {
		killed, _, err := InterruptProcessTreeStop(zap.NewNop(), pid, syscall.SIGINT, nil)
		done <- stopped{killed, err}
	}()
	childSaid := make(chan string, 1)
	go func() {
		b := make([]byte, 32)
		n, _ := said.Read(b)
		childSaid <- string(b[:n])
	}()
	var child int
	select {
	case line := <-childSaid:
		if child, err = strconv.Atoi(strings.TrimSpace(line)); err != nil {
			t.Fatalf("the app's trap's child said %q, not its pid", line)
		}
	case end := <-done:
		t.Fatalf("the stop ended before the app's trap started its child: %q (%v)", end.killed, end.err)
	}
	release := starvetest.Hold(t, child)
	ranBefore, _ := starvetest.Ran(child)
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
	ranHeld, _ := starvetest.Ran(child)
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	release()
	if ran := ranHeld - ranBefore; ran >= bound/2 {
		t.Fatalf("the app's child held off the CPU ran %v while held, half the stall bound (%v) or more: the test held nothing", ran, bound)
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
	if m := MarkExit([]Reached{p}); m.Running != 1 || m.Threads < 2 || m.Fds == 0 {
		t.Fatalf("MarkExit = %+v; want it running, with its threads and open files", m)
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
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: 100}, false},
		{ExitMark{Running: 1, Threads: 3, Fds: 9, IO: 200, OnStorage: true}, true},
	} {
		if got := r.Advance(tc.m); got != tc.want {
			t.Fatalf("%+v: progress %v, want %v", tc.m, got, tc.want)
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
// group's stop kills at once, and says so as the user's.
func TestAStopEndsOnItsProcessesGoneAndAtAPress(t *testing.T) {
	shortStall(t, time.Hour)
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

	offer := StopOffer
	StopOffer = 0
	t.Cleanup(func() { StopOffer = offer; ClearInterrupted(); ResetInterruptedAgain() })
	MarkInterrupted()
	go func() {
		for !MarkInterruptedAgain() {
			time.Sleep(5 * time.Millisecond)
		}
	}()
	pid, reaped := readyLeader(t, t.TempDir(), `trap "" INT; touch ready; exec sleep 600`)
	killed, _, err := InterruptProcessTreeStop(zap.NewNop(), pid, syscall.SIGINT, nil)
	if err != nil {
		t.Fatal(err)
	}
	if killed != "keploy killed a process of it at the press of Ctrl+C, before it had exited" {
		t.Fatalf("the stop said %q; want the press's kill", killed)
	}
	awaitReaped(t, "the app", reaped)
}

// pressAtOffer is a logger whose first offer to end a stop at the next
// Ctrl+C ("press Ctrl+C again") is answered by a press, as the user's, in
// the offer's own look: the press only a standing offer takes
// (MarkInterruptedAgain). It sets the offer at the run's interrupt
// (StopOffer 0, the run interrupted) for the test. said is the offer's text,
// and taken whether the press was taken; read them once the stop is over.
func pressAtOffer(t *testing.T) (logger *zap.Logger, said *atomic.Pointer[string], taken *atomic.Bool) {
	t.Helper()
	offer := StopOffer
	StopOffer = 0
	t.Cleanup(func() { StopOffer = offer; ClearInterrupted(); ResetInterruptedAgain() })
	MarkInterrupted()
	said, taken = &atomic.Pointer[string]{}, &atomic.Bool{}
	core, _ := observer.New(zapcore.InfoLevel)
	logger = zap.New(zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
		if strings.Contains(e.Message, "press Ctrl+C again") && said.CompareAndSwap(nil, &e.Message) {
			taken.Store(MarkInterruptedAgain())
		}
		return nil
	}))
	return logger, said, taken
}

// A stop through only on its own word (a container's daemon's, which comes
// after the kernel's) waits on past its processes' exit, in the same wait:
// one offer to end it at Ctrl+C, which says what a press does then, and a
// press at it ends the wait, saying nothing of the processes is left to kill.
// As two waits, one to the processes' exit and one after, a press taken at
// the first's offer in the look that found them gone was lost, and the stop
// held to its stall bound. The bound here is 10s, so a lost press fails the
// test (the wait ends stalled), not holds it.
func TestAStopThroughOnItsWordWaitsPastItsProcessesExit(t *testing.T) {
	shortStall(t, 10*time.Second)
	gone := func(t *testing.T) []Reached {
		t.Helper()
		cmd, reaped := startGroupLeader(t, "sleep", "600")
		p, ok := ProcOf(cmd.Process.Pid)
		if !ok {
			t.Fatal("cannot read the process")
		}
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		awaitReaped(t, "the process", reaped)
		return []Reached{p}
	}
	t.Run("its processes gone do not end it", func(t *testing.T) {
		procs := gone(t)
		stopped := make(chan struct{})
		var looks atomic.Int32
		looked := func() {
			if looks.Add(1) == 3 {
				close(stopped)
			}
		}
		exitLooked.Store(&looked)
		defer exitLooked.Store(nil)
		end, _, _ := WaitStopByEvents(zap.NewNop(), stopped, procs, nil)
		if n := looks.Load(); end != StopExited || n < 3 {
			t.Fatalf("the wait ended %v at look %d; want StopExited once its word came, at look 3", end, n)
		}
	})
	t.Run("a press at its offer, its processes gone", func(t *testing.T) {
		procs := gone(t)
		logger, said, taken := pressAtOffer(t)
		end, _, left := WaitStopByEvents(logger, make(chan struct{}), procs, nil)
		if end != StopPressed || !left || !taken.Load() {
			t.Fatalf("the wait ended %v, its processes gone %v, the press taken %v; want StopPressed, gone", end, left, taken.Load())
		}
		if s := said.Load(); s == nil || *s != "the application's processes have exited, and keploy waits for word that it has stopped; press Ctrl+C again to stop waiting" {
			t.Fatalf("the offer said %v; want that a press stops the wait, with nothing to kill", s)
		}
	})
	t.Run("a press at its offer, its process running", func(t *testing.T) {
		cmd, reaped := startGroupLeader(t, "sleep", "600")
		defer func() { _ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); awaitReaped(t, "the process", reaped) }()
		p, ok := ProcOf(cmd.Process.Pid)
		if !ok {
			t.Fatal("cannot read the process")
		}
		logger, said, taken := pressAtOffer(t)
		end, _, left := WaitStopByEvents(logger, make(chan struct{}), []Reached{p}, nil)
		if end != StopPressed || left || !taken.Load() {
			t.Fatalf("the wait ended %v, its processes gone %v, the press taken %v; want StopPressed, not gone", end, left, taken.Load())
		}
		if s := said.Load(); s == nil || !strings.Contains(*s, "press Ctrl+C again to kill it now") {
			t.Fatalf("the offer said %v; want that a press kills it", s)
		}
	})
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
// drains wait on; a process that only stays is no movement.
func TestAStopsStepsOutMoveTheRunsTeardown(t *testing.T) {
	shortStall(t, time.Hour)
	dir := t.TempDir()
	// It closes one of its files at each step file the test makes, and
	// exits at "go".
	cmd, reaped := startGroupLeader(t, "sh", "-c", `cd `+dir+` && exec 3</dev/null 4</dev/null 5</dev/null 6</dev/null 7</dev/null
: > ready
for fd in 3 4 5 6 7; do while [ ! -e step$fd ]; do sleep 0.01; done; eval "exec $fd<&-"; done
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
	// The steps, until the stop has seen one: the first may come before it
	// took its first look.
	for fd, deadline := 3, time.Now().Add(time.Minute); scope.Moves() == 0; fd++ {
		if fd <= 7 {
			if err := os.WriteFile(filepath.Join(dir, "step"+strconv.Itoa(fd)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if time.Now().After(deadline) { // only turns a hang into a failure
			t.Fatal("the app's steps out did not move the run's teardown")
		}
		time.Sleep(200 * time.Millisecond)
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
