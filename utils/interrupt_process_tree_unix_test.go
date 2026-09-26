//go:build linux || darwin
// +build linux darwin

package utils

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// buildSignalIgnorer compiles a tiny server that binds `port` and IGNORES the
// graceful signals (SIGINT/SIGTERM), simulating an app that under contention does
// not exit on the graceful signal within the wait window — it only dies on SIGKILL.
func buildSignalIgnorer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	prog := `package main
import ("net";"os";"os/signal";"syscall";"time")
func main(){
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM) // survive the graceful signal
	ln,err:=net.Listen("tcp",":"+os.Args[1]); if err!=nil{os.Exit(3)}
	_=ln
	time.Sleep(10*time.Minute) // until SIGKILL'd
}`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "ignorer")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build ignorer: %v\n%s", err, out)
	}
	return bin
}

func ephemeralPortUtil(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	return p
}

// TestInterruptProcessTreeEscalatesToSIGKILL guards the "address already in use"
// app-lifecycle flake (go-docker-timefreeze): if the app ignores the graceful
// signal (or is too contention-slow to exit within the wait), InterruptProcessTree
// must escalate to SIGKILL so the app is actually dead and its port freed before
// keploy starts the next app. The previous implementation gave up after the
// graceful wait and returned with the app still alive.
func TestInterruptProcessTreeEscalatesToSIGKILL(t *testing.T) {
	bin := buildSignalIgnorer(t)
	port := ephemeralPortUtil(t)

	cmd := exec.Command(bin, port)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { _ = cmd.Wait() }() // reap on exit

	// Wait until the ignorer has bound the port.
	deadline := time.Now().Add(8 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ignorer never bound the port")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Send the graceful signal it ignores; InterruptProcessTree must escalate.
	if err := InterruptProcessTree(zap.NewNop(), cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatalf("InterruptProcessTree: %v", err)
	}

	// Contract: once it returns, the process tree is dead and the port is free.
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		t.Fatalf("port %s still held after InterruptProcessTree returned (process survived the graceful signal, never SIGKILL'd): %v", port, err)
	}
	_ = ln.Close()
}

// buildOwnGroupRunner compiles a stand-in test runner whose child sits in a
// process group of its own -- as a test runner's worker, a dev server or a
// shell job can -- and whose grandchild sits in yet another. Each writes the
// pid of the process it started to a file, then waits. Nothing ignores
// SIGINT: every one of them ends as soon as it is sent one.
func buildOwnGroupRunner(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	prog := `package main
import ("os";"os/exec";"strconv";"syscall";"time")
func spawn(mode, pidFile string){
	c:=exec.Command(os.Args[0], mode, os.Args[2])
	c.SysProcAttr=&syscall.SysProcAttr{Setpgid: true}
	if err:=c.Start(); err!=nil { os.Exit(3) }
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(c.Process.Pid)), 0o644)
	_ = c.Wait()
}
func main(){
	switch os.Args[1] {
	case "leaf": time.Sleep(10*time.Minute)
	case "child": spawn("leaf", os.Args[2]+".leaf")
	default: spawn("child", os.Args[2])
	}
}`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "runner")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, out)
	}
	return bin
}

// ownGroupPID waits for the pid written to pidFile, and for that process to
// have made itself the leader of its own process group.
func ownGroupPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
				if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
					return pid
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no process came up in a process group of its own (%s)", pidFile)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestInterruptProcessTreeReachesADescendantInItsOwnProcessGroup is the
// Ctrl+C of a native run. The runner is signalled through its process group,
// and a descendant in another group is reached only by walking the process
// tree.
//
// On macOS that walk read /proc, which does not exist there: it found no
// descendants, so the child outlived the run, and every interrupt logged
// "failed to find unique process groups: open /proc/<pid>/status" at ERROR.
func TestInterruptProcessTreeReachesADescendantInItsOwnProcessGroup(t *testing.T) {
	bin := buildOwnGroupRunner(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")

	cmd := exec.Command(bin, "runner", pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // as keploy starts a native command
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { _ = cmd.Wait() }() // reap on exit

	child := ownGroupPID(t, pidFile)
	leaf := ownGroupPID(t, pidFile+".leaf")
	t.Cleanup(func() {
		_ = syscall.Kill(child, syscall.SIGKILL)
		_ = syscall.Kill(leaf, syscall.SIGKILL)
	})

	core, logs := observer.New(zap.DebugLevel)
	if err := InterruptProcessTree(zap.New(core), cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatalf("InterruptProcessTree: %v", err)
	}

	for _, e := range logs.FilterLevelExact(zap.ErrorLevel).All() {
		t.Errorf("an interrupt that went as it should logged an ERROR: %q %v", e.Message, e.ContextMap())
	}
	// Once its parent is gone a dead process is re-parented and reaped, so its
	// pid stops answering; until then it can linger as a zombie.
	deadline := time.Now().Add(5 * time.Second)
	for _, p := range []struct {
		pid  int
		what string
	}{{child, "child"}, {leaf, "grandchild"}} {
		for syscall.Kill(p.pid, 0) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("the runner's %s %d, in a process group of its own, outlived the interrupt", p.what, p.pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// buildZombieRunner compiles a stand-in test runner that starts a subprocess
// and never waits for it -- as a Go test that calls exec.Cmd.Start without
// Wait does, or a Python one that calls Popen without wait. The subprocess
// exits at once, and stays in the process table as a zombie until its parent
// reaps it, which this one never does. The runner writes the subprocess's
// pid to a file, then waits to be interrupted.
func buildZombieRunner(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	prog := `package main
import ("os";"os/exec";"strconv";"time")
func main(){
	if len(os.Args) > 2 { return }
	c:=exec.Command(os.Args[0], os.Args[1], "exit")
	if err:=c.Start(); err!=nil { os.Exit(3) }
	_ = os.WriteFile(os.Args[1], []byte(strconv.Itoa(c.Process.Pid)), 0o644)
	time.Sleep(10*time.Minute)
}`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "zombie-runner")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, out)
	}
	return bin
}

// TestInterruptProcessTreeWithAZombieDescendant interrupts a runner that
// left a zombie behind. The zombie is part of the tree the walk finds, and
// looking up its process group must not fail the interrupt.
//
// On macOS it did: getpgid(2) answers ESRCH for a zombie, so every such
// interrupt logged "failed to find unique process groups: no such process"
// at ERROR and fell back to signalling each pid as if it were a group.
func TestInterruptProcessTreeWithAZombieDescendant(t *testing.T) {
	bin := buildZombieRunner(t)
	pidFile := filepath.Join(t.TempDir(), "zombie.pid")

	cmd := exec.Command(bin, pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // as keploy starts a native command
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }() // reap on exit
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	deadline := time.Now().Add(8 * time.Second)
	for {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 && isZombie(t, pid) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner's subprocess never became a zombie")
		}
		time.Sleep(20 * time.Millisecond)
	}

	core, logs := observer.New(zap.DebugLevel)
	if err := InterruptProcessTree(zap.New(core), cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatalf("InterruptProcessTree: %v", err)
	}
	for _, e := range logs.FilterLevelExact(zap.ErrorLevel).All() {
		t.Errorf("an interrupt that went as it should logged an ERROR: %q %v", e.Message, e.ContextMap())
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner outlived the interrupt")
	}
}

// findChildPIDs reads each descendant's process group as the kernel has it.
// A process that joined another's group is reached through that group: there
// is no group of its own pid to signal, and signalling one fails silently
// (ESRCH is read as "already gone").
func TestFindChildPIDsReadsEachDescendantsProcessGroup(t *testing.T) {
	start := func(attr *syscall.SysProcAttr) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("sleep", "60")
		cmd.SysProcAttr = attr
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	leader := start(&syscall.SysProcAttr{Setpgid: true})
	member := start(&syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid})

	children, groupOf, err := findChildPIDs(os.Getpid())
	if err != nil {
		t.Fatalf("findChildPIDs: %v", err)
	}
	for _, c := range []*exec.Cmd{leader, member} {
		found := false
		for _, pid := range children {
			found = found || pid == c.Process.Pid
		}
		if !found {
			t.Fatalf("findChildPIDs(%d) = %v, missing child %d", os.Getpid(), children, c.Process.Pid)
		}
		if got, err := groupOf(c.Process.Pid); err != nil || got != leader.Process.Pid {
			t.Fatalf("process group of %d = %d, %v; want %d", c.Process.Pid, got, err, leader.Process.Pid)
		}
	}
}
