//go:build darwin

package utils

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// buildGroupLeavingRunner compiles a stand-in test runner that starts two
// subprocesses, each in a process group of its own, and reaps neither, as a
// harness that runs `server &` in a new session and never waits for it does.
// The first starts `sleep 300` in its group and exits; the second exits at
// once. Both stay in the process table as zombies, and the sleep, whose
// parent is now launchd, lives on in the first one's group. The runner
// writes "first sleep second" to a file, then waits to be interrupted.
func buildGroupLeavingRunner(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	prog := `package main
import ("fmt";"os";"os/exec";"syscall";"time")
func main(){
	if len(os.Args) > 2 {
		if os.Args[2] == "alone" { return }
		s := exec.Command("/bin/sleep", "300")
		if err := s.Start(); err != nil { os.Exit(4) }
		_ = os.WriteFile(os.Args[1]+".sleep", []byte(fmt.Sprint(s.Process.Pid)), 0o644)
		return
	}
	start := func(role string) int {
		c := exec.Command(os.Args[0], os.Args[1], role)
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil { os.Exit(3) }
		return c.Process.Pid
	}
	first, second := start("sleep"), start("alone")
	for {
		if b, err := os.ReadFile(os.Args[1]+".sleep"); err == nil && len(b) > 0 {
			_ = os.WriteFile(os.Args[1]+".tmp", []byte(fmt.Sprintf("%d %s %d", first, b, second)), 0o644)
			_ = os.Rename(os.Args[1]+".tmp", os.Args[1])
			break
		}
		time.Sleep(10*time.Millisecond)
	}
	time.Sleep(10*time.Minute)
}`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "group-leaving-runner")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, out)
	}
	return bin
}

// A zombie in the application's tree that led a process group of its own is
// how the interrupt reaches that group: the sleep in it, whose parent is now
// launchd, must not outlive the interrupt. And a zombie has exited: a group
// it leads costs the interrupt no grace wait and no SIGKILL, and a group of
// zombies only, which macOS answers EPERM, is no ERROR.
func TestInterruptProcessTreeReachesTheGroupOfAZombie(t *testing.T) {
	bin := buildGroupLeavingRunner(t)
	pidFile := filepath.Join(t.TempDir(), "pids")
	cmd := exec.Command(bin, pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // as keploy starts a native command
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { _ = cmd.Wait() }() // reap on exit
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	var first, sleep, second int
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil {
			f := strings.Fields(string(b))
			first, _ = strconv.Atoi(f[0])
			sleep, _ = strconv.Atoi(f[1])
			second, _ = strconv.Atoi(f[2])
			if isZombie(t, first) && isZombie(t, second) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner's subprocesses never became zombies")
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(sleep, syscall.SIGKILL) })
	if pgid, err := syscall.Getpgid(sleep); err != nil || pgid != first {
		t.Fatalf("the sleep %d is in group %d (%v); want the zombie %d's", sleep, pgid, err, first)
	}

	// The second zombie's group holds no process that runs: its signal is
	// no refusal, but a signal macOS refuses for another reason still is,
	// and so is EPERM from a process keploy may not signal (launchd).
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	if err := SendSignal(logger, -second, 0); err != nil {
		t.Errorf("signalling the group of zombies only %d answered %v; want none", second, err)
	}
	if err := SendSignal(logger, -second, syscall.Signal(999)); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("an invalid signal to the group of zombies only %d answered %v; want EINVAL", second, err)
	}
	if err := SendSignal(logger, 1, 0); !errors.Is(err, syscall.EPERM) {
		t.Errorf("signalling launchd answered %v; want EPERM", err)
	}
	if !exitedUnreaped(first) || exitedUnreaped(cmd.Process.Pid) || exitedUnreaped(sleep) {
		t.Errorf("exitedUnreaped: zombie %d %v, runner %d %v, sleep %d %v; want true, false, false",
			first, exitedUnreaped(first), cmd.Process.Pid, exitedUnreaped(cmd.Process.Pid), sleep, exitedUnreaped(sleep))
	}

	core, logs = observer.New(zap.DebugLevel)
	began := time.Now()
	killed, err := InterruptProcessTreeKilled(zap.New(core), cmd.Process.Pid, syscall.SIGINT)
	took := time.Since(began)
	if err != nil {
		t.Fatalf("InterruptProcessTree: %v", err)
	}
	for _, e := range logs.FilterLevelExact(zap.ErrorLevel).All() {
		t.Errorf("an interrupt that went as it should logged an ERROR: %q %v", e.Message, e.ContextMap())
	}
	if killed != "" {
		t.Errorf("the interrupt says it killed: %s", killed)
	}
	if took >= TreeInterruptGrace {
		t.Errorf("the interrupt took %s: it waited for a zombie as if it ran", took)
	}
	var groups []int
	for _, e := range logs.FilterMessage("Unique process groups").All() {
		list, _ := e.ContextMap()["uniqueProcess"].([]interface{})
		for _, g := range list {
			if pgid, err := strconv.Atoi(fmt.Sprint(g)); err == nil {
				groups = append(groups, pgid)
			}
		}
	}
	if !slices.Contains(groups, first) || !slices.Contains(groups, second) || !slices.Contains(groups, cmd.Process.Pid) {
		t.Errorf("the interrupt signalled groups %v; want the zombies' %d and %d and the runner's %d", groups, first, second, cmd.Process.Pid)
	}
	for end := time.Now().Add(5 * time.Second); syscall.Kill(sleep, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("the sleep %d, in the zombie %d's group, outlived the interrupt of the application's tree", sleep, first)
		}
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
