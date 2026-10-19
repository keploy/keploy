//go:build linux

package utils

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
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
// progress (it ignores the interrupt) is killed once the stall bound of
// keploy's runnable time has passed, and the stop says so.
func TestAStopWaitsForTheAppsExitAndKillsOnlyOnAStall(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		stall         time.Duration
		killed        bool
	}{
		{"a shutdown longer than the old grace", `trap 'sleep 4; exit 0' INT; touch ready; while :; do sleep 0.2; done`, StopStallBudget, false},
		{"a shutdown that keeps making progress past the stall bound", `exec 3</dev/null 4</dev/null 5</dev/null 6</dev/null 7</dev/null 8</dev/null
trap 'for fd in 3 4 5 6 7 8; do eval "exec $fd<&-"; sleep 0.3; done; exit 0' INT; touch ready; while :; do sleep 0.1; done`, 500 * time.Millisecond, false},
		{"one that ignores the interrupt", `trap "" INT; touch ready; exec sleep 600`, 300 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortStall(t, tc.stall)
			pid, reaped := readyLeader(t, t.TempDir(), tc.command)
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
			if tc.killed && !strings.Contains(killed, "made no progress on its way out for "+tc.stall.String()+" of keploy's runnable time") {
				t.Fatalf("the kill is said as %q; want the stall rule named", killed)
			}
			awaitReaped(t, tc.name, reaped)
		})
	}
}

// A process's way out is read on its exit marks: one ending, a thread ending,
// a file closed, storage read or written while it waits on it. A count that
// rises and falls back is none.
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
	if end := WaitExitByEvents(zap.NewNop(), never, []Reached{p}, nil); end != StopExited {
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
	go func() { ended <- WaitExitByEvents(zap.NewNop(), make(chan struct{}), []Reached{p}, scope) }()
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
