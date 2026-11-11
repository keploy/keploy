//go:build darwin

package utils

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
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
