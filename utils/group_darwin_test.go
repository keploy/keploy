//go:build darwin

package utils

import (
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/procsock"
)

// startGroup starts command under sh as the leader of a process group of
// its own, as ExecuteCommand starts a native application, and kills the
// whole group when the test ends.
func startGroup(t *testing.T, command string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %q: %v", command, err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	return cmd
}

// waitUntil waits, 10 s at most, for done.
func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !done(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10 s for %s", what)
		}
	}
}

// Once a group's leader has exited, a process it started that is still in
// the group runs on; once that one ends, the group runs on no longer.
func TestGroupRunsOnSeesWhatItsLeaderLeft(t *testing.T) {
	cmd := startGroup(t, "sleep 60 & exit 0")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the leader: %v", err)
	}
	pgid := cmd.Process.Pid
	if !GroupRunsOn(pgid) {
		t.Fatal("the leader exited and left a sleep in its group: GroupRunsOn said none runs")
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the group: %v", err)
	}
	waitUntil(t, "the killed group to run on no longer", func() bool { return !GroupRunsOn(pgid) })
}

// A group whose one process has exited, a zombie its parent has not reaped
// yet, runs on no longer.
func TestGroupRunsOnCountsNoZombie(t *testing.T) {
	cmd := startGroup(t, "exit 0")
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })
	waitUntil(t, "the leader to exit unreaped", func() bool { return isZombie(t, pid) })
	if GroupRunsOn(pid) {
		t.Fatal("the group's one process is a zombie: GroupRunsOn said it runs")
	}
}

// The test's own group runs (the read works); no group is read below 2:
// 0 (the kernel's) and 1 (launchd's) are no application's.
func TestGroupRunsOnReadsNoGroupBelowTwo(t *testing.T) {
	if !GroupRunsOn(syscall.Getpgrp()) {
		t.Fatal("the test's own group: GroupRunsOn said none runs")
	}
	for _, pgid := range []int{0, 1, -1} {
		if GroupRunsOn(pgid) {
			t.Fatalf("GroupRunsOn(%d) said it runs", pgid)
		}
	}
}

// A process whose record cannot be read, one that has gone or is exiting
// between the list and the read, does not run on, and the tree leaves it out.
func TestAProcessWhoseRecordCannotBeReadRunsNoLonger(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	waitUntil(t, "the sleep to show in its group", func() bool { return GroupRunsOn(pid) })
	if kids, _, err := findChildPIDs(os.Getpid()); err != nil || !slices.Contains(kids, pid) {
		t.Fatalf("findChildPIDs(this process) = %v, %v; want the sleep %d", kids, err, pid)
	}
	read := procRecord
	t.Cleanup(func() { procRecord = read })
	procRecord = func(p int) (procsock.Proc, error) {
		if p == pid {
			return procsock.Proc{}, syscall.ESRCH
		}
		return read(p)
	}
	if GroupRunsOn(pid) {
		t.Fatal("the group's one process cannot be read: GroupRunsOn said it runs")
	}
	if kids, _, err := findChildPIDs(os.Getpid()); err != nil || slices.Contains(kids, pid) {
		t.Fatalf("findChildPIDs(this process) = %v, %v; want no sleep %d", kids, err, pid)
	}
}

// A group whose list cannot be read has not exited, as far as keploy knows:
// a refused signal to it stays an error. Nor does it run on.
func TestAGroupWhoseListCannotBeReadHasNotExited(t *testing.T) {
	cmd := startGroup(t, "exec sleep 60")
	pgid := cmd.Process.Pid
	waitUntil(t, "the sleep to show in its group", func() bool { return GroupRunsOn(pgid) })
	if groupExited(pgid) {
		t.Fatalf("group %d runs a sleep: groupExited said it has exited", pgid)
	}
	list := groupPIDs
	t.Cleanup(func() { groupPIDs = list })
	groupPIDs = func(int) ([]int, error) { return nil, syscall.EINVAL }
	if groupExited(pgid) || GroupRunsOn(pgid) {
		t.Fatalf("group %d's list cannot be read: groupExited %v, GroupRunsOn %v; want neither", pgid, groupExited(pgid), GroupRunsOn(pgid))
	}
}
