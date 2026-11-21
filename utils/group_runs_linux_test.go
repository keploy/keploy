//go:build linux

package utils

import (
	"errors"
	"io/fs"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

// failingFS is a /proc that cannot be listed.
type failingFS struct{}

func (failingFS) Open(string) (fs.File, error) { return nil, fs.ErrPermission }

// A process group runs while one of its processes runs: a zombie, or a dead
// process, does not count, nor does a process of another group, a stat that
// cannot be parsed, or an entry that is no pid. A /proc that cannot be
// listed is an error, and no group at or below 1 is read.
func TestGroupRunsInReadsEachProcessOfTheGroup(t *testing.T) {
	proc := fstest.MapFS{
		"20/stat":   statLine(20, "sh", 'Z', 1, 20),     // a zombie leader
		"21/stat":   statLine(21, "server", 'S', 1, 20), // the server it left
		"80/stat":   statLine(80, "server", 'Z', 1, 80), // main thread exited, another runs
		"30/stat":   statLine(30, "sh", 'Z', 1, 30),     // zombies only
		"31/stat":   statLine(31, "worker", 'Z', 1, 30),
		"40/stat":   statLine(40, "app", 'X', 1, 40), // dead
		"50/stat":   statLine(50, "app", 'R', 1, 51), // in another group
		"60/stat":   {Data: []byte("60 (app) S 1")},  // cut short
		"self/stat": statLine(70, "self", 'R', 1, 70),
		"1/stat":    statLine(1, "init", 'S', 0, 1),
	}
	threadRuns := func(pid int) bool { return pid == 80 }
	for pgid, want := range map[int]bool{20: true, 30: false, 40: false, 51: true, 50: false, 60: false, 70: false, 1: false, 0: false, 80: true} {
		got, err := groupRunsIn(proc, pgid, nil, threadRuns, nil)
		if err != nil || got != want {
			t.Errorf("groupRunsIn(%d) = %v, %v; want %v", pgid, got, err, want)
		}
	}
	if got, err := groupRunsIn(failingFS{}, 20, nil, threadRuns, nil); !errors.Is(err, fs.ErrPermission) || got {
		t.Errorf("groupRunsIn of a /proc that cannot be listed = %v, %v; want the error", got, err)
	}
	// The server the zombie leader left, handed to the caller as it ignored
	// the signal (its pid and the start time statLine gives), is not
	// waited for; the same pid with another start time is another process.
	if got, err := groupRunsIn(proc, 20, []Reached{{PID: 21, Start: 77, Ignores: true}}, threadRuns, nil); err != nil || got {
		t.Errorf("groupRunsIn(20) without the server handed over = %v, %v; want false", got, err)
	}
	if got, err := groupRunsIn(proc, 20, []Reached{{PID: 21, Start: 78, Ignores: true}}, threadRuns, nil); err != nil || !got {
		t.Errorf("groupRunsIn(20) with another process of pid 21 handed over = %v, %v; want true", got, err)
	}
}

// The interrupt hands its caller each process its signal reached that
// ignored it, but the group's leader, and waits for the rest.
func TestHandedOverIsWhatIgnoredTheSignalButTheLeader(t *testing.T) {
	members := []Reached{
		{PID: 10, Start: 1, Ignores: true},  // the leader, ignoring
		{PID: 11, Start: 2, Ignores: false}, // acting on it
		{PID: 12, Start: 3, Ignores: true},  // a background command
	}
	if got, want := handedOver(10, members), []Reached{members[2]}; !slices.Equal(got, want) {
		t.Errorf("handedOver = %+v; want %+v", got, want)
	}
	if got, want := waitedFor(10, members), members[:2]; !slices.Equal(got, want) {
		t.Errorf("waitedFor = %+v; want %+v", got, want)
	}
}

// The stop under the proven switch waits for each process its signal
// reached in a group, its leader or not: closed only once every one has
// exited, and at once for none (a group of zombies only).
func TestGroupGoneWaitsForEachProcessItsSignalReached(t *testing.T) {
	isClosed := func(c <-chan struct{}) bool {
		select {
		case <-c:
			return true
		case <-time.After(300 * time.Millisecond):
			return false
		}
	}
	gone, release := groupGone(0, nil)
	if !isClosed(gone) {
		t.Fatal("a group whose signal reached no process is not gone")
	}
	release()

	start := func() (*exec.Cmd, Reached) {
		cmd := exec.Command("sleep", "600")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		p, ok := ProcOf(cmd.Process.Pid)
		if !ok {
			t.Fatal("cannot read the sleep")
		}
		return cmd, p
	}
	first, a := start()
	second, b := start()
	gone, release = groupGone(0, []Reached{a, b})
	defer release()
	if isClosed(gone) {
		t.Fatal("a group whose processes both run is gone")
	}
	_ = first.Process.Signal(syscall.SIGKILL)
	_ = first.Wait()
	if isClosed(gone) {
		t.Fatal("a group whose second process runs is gone: only the first exited")
	}
	_ = second.Process.Signal(syscall.SIGKILL)
	_ = second.Wait()
	select {
	case <-gone:
	case <-time.After(time.Minute):
		t.Fatal("the group was not gone once each of its processes had exited")
	}
}

// A process whose main thread has exited while another thread of it runs
// on reads Z in its stat, but has not exited: it holds its files (a
// SIGKILLed Go server's port, until its last thread is gone). Neither
// isProcessRunning nor the group wait (zombieRuns) takes it for exited.
func TestAZombieMainThreadWithAThreadRunningHasNotExited(t *testing.T) {
	cmd := startMainThreadExited(t)
	pid := cmd.Process.Pid
	if exitedUnreaped(pid) {
		t.Errorf("exitedUnreaped(%d): a process a thread of which runs is taken for a zombie", pid)
	}
	if running, err := isProcessRunning(pid); err != nil || !running {
		t.Errorf("isProcessRunning(%d) = %v, %v; want true", pid, running, err)
	}
	if !zombieRuns(pid) {
		t.Errorf("zombieRuns(%d): the thread of it that runs is not seen", pid)
	}
}

// What the caller or keploy's stop of the daemons ends, the group wait
// neither waits for nor counts: a process handed to the caller, one, but the
// leader, that the stop of the daemons ends (a shell's app adopted once the
// shell died on the interrupt), and what either runs, however deep. A
// process of the group that is neither, nor runs under one, still runs it,
// and so does a leader the stop of the daemons would end, and what it runs.
// A loop of parents
// (a /proc read while it changes) ends the walk.
func TestGroupRunsInLeavesWhatTheCallerOrTheDaemonStopEnds(t *testing.T) {
	proc := fstest.MapFS{
		"1/stat":  statLine(1, "init", 'S', 0, 1),
		"10/stat": statLine(10, "keploy", 'S', 1, 10),
		"20/stat": statLine(20, "sh", 'Z', 10, 20),     // the leader, exited
		"21/stat": statLine(21, "inner", 'S', 10, 20),  // adopted: its sh died
		"22/stat": statLine(22, "sleep", 'S', 21, 20),  // what it runs
		"23/stat": statLine(23, "sleep", 'S', 22, 20),  // and what that runs
		"40/stat": statLine(40, "app", 'S', 10, 40),    // a leader keploy runs
		"41/stat": statLine(41, "worker", 'S', 40, 40), // and what it runs
		"30/stat": statLine(30, "a", 'S', 31, 30),      // a loop of parents
		"31/stat": statLine(31, "b", 'S', 30, 30),
	}
	none := func(int) bool { return false }
	adopted := func(pids ...int) func(int, uint64) bool {
		return func(pid int, start uint64) bool { return slices.Contains(pids, pid) && start == 77 }
	}
	for _, tc := range []struct {
		name   string
		pgid   int
		except []Reached
		left   func(int, uint64) bool
		want   bool
	}{
		{"nothing left to another", 20, nil, nil, true},
		{"the adopted shell, and what it runs", 20, nil, adopted(21), false},
		{"the handed shell, and what it runs", 20, []Reached{{PID: 21, Start: 77, Ignores: true}}, nil, false},
		{"only what the adopted shell runs", 20, nil, adopted(22), true},
		{"another process of pid 21 adopted", 20, nil, func(pid int, start uint64) bool { return pid == 21 && start == 78 }, true},
		{"a leader the stop of the daemons would end", 40, nil, adopted(40), true},
		{"what a leader the stop of the daemons would end runs", 40, nil, adopted(40), true},
		{"a loop of parents", 30, nil, adopted(), true},
	} {
		if got, err := groupRunsIn(proc, tc.pgid, tc.except, none, tc.left); err != nil || got != tc.want {
			t.Errorf("%s: groupRunsIn(%d) = %v, %v; want %v", tc.name, tc.pgid, got, err, tc.want)
		}
	}
}
