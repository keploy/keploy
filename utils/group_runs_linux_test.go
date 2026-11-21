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
		"30/stat":   statLine(30, "sh", 'Z', 1, 30),     // zombies only
		"31/stat":   statLine(31, "worker", 'Z', 1, 30),
		"40/stat":   statLine(40, "app", 'X', 1, 40), // dead
		"50/stat":   statLine(50, "app", 'R', 1, 51), // in another group
		"60/stat":   {Data: []byte("60 (app) S 1")},  // cut short
		"self/stat": statLine(70, "self", 'R', 1, 70),
		"1/stat":    statLine(1, "init", 'S', 0, 1),
	}
	for pgid, want := range map[int]bool{20: true, 30: false, 40: false, 51: true, 50: false, 60: false, 70: false, 1: false, 0: false} {
		got, err := groupRunsIn(proc, pgid, nil)
		if err != nil || got != want {
			t.Errorf("groupRunsIn(%d) = %v, %v; want %v", pgid, got, err, want)
		}
	}
	if got, err := groupRunsIn(failingFS{}, 20, nil); !errors.Is(err, fs.ErrPermission) || got {
		t.Errorf("groupRunsIn of a /proc that cannot be listed = %v, %v; want the error", got, err)
	}
	// The server the zombie leader left, handed to the caller as it ignored
	// the signal (its pid and the start time statLine gives), is not
	// waited for; the same pid with another start time is another process.
	if got, err := groupRunsIn(proc, 20, []Reached{{PID: 21, Start: 77, Ignores: true}}); err != nil || got {
		t.Errorf("groupRunsIn(20) without the server handed over = %v, %v; want false", got, err)
	}
	if got, err := groupRunsIn(proc, 20, []Reached{{PID: 21, Start: 78, Ignores: true}}); err != nil || !got {
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
	gone, release := groupGone(nil)
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
	gone, release = groupGone([]Reached{a, b})
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
