//go:build darwin

package procsock_test

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent/procsock"
	"go.keploy.io/server/v3/pkg/agent/procsock/spawnhold"
)

// While a posix_spawn is held in the kernel, every process is listed and
// read, its descriptors too, at once: none of those reads waits for the held
// spawn, as the kern.proc sysctls do. The row first shows the hold is real:
// kern.proc.pid of the held child waits until the spawn is released.
func TestProcessesAreReadWhileASpawnIsHeld(t *testing.T) {
	h := spawnhold.Hold(t)
	waited := make(chan struct{})
	go func() {
		_, _ = unix.SysctlKinfoProc("kern.proc.pid", h.Child)
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatalf("kern.proc.pid of the held child %d did not wait: the spawn is not held", h.Child)
	case <-time.After(time.Second):
	}

	type result struct {
		pids, group []int
		holder      procsock.Proc
		err         error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		if r.pids, r.err = procsock.PIDs(); r.err != nil {
			done <- r
			return
		}
		for _, pid := range r.pids {
			p, err := procsock.Info(pid)
			if err == nil && pid == h.Holder {
				r.holder = p
			}
			_, _ = procsock.SocketFDs(pid)
		}
		r.group, r.err = procsock.GroupPIDs(unix.Getpgrp())
		done <- r
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if !slices.Contains(r.pids, h.Child) || !slices.Contains(r.pids, os.Getpid()) {
			t.Fatalf("PIDs() = %d pids, without the held child %d or this process %d", len(r.pids), h.Child, os.Getpid())
		}
		if r.holder.PPID != os.Getpid() {
			t.Fatalf("Info(holder %d) = %+v; want this process %d as its parent", h.Holder, r.holder, os.Getpid())
		}
		if !slices.Contains(r.group, os.Getpid()) || !slices.Contains(r.group, h.Holder) {
			t.Fatalf("GroupPIDs(%d) = %v; want this process %d and the holder %d", unix.Getpgrp(), r.group, os.Getpid(), h.Holder)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading the processes waited for the held spawn")
	}
	h.Release(t)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("kern.proc.pid of the held child still waits after the spawn was released")
	}
}

// Info reads the short record: this process's parent and process group,
// and a process that is not there answers ESRCH, the kernel's own answer.
func TestInfoReadsParentAndGroup(t *testing.T) {
	p, err := procsock.Info(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if p.PID != os.Getpid() || p.PPID != os.Getppid() || p.PGID != unix.Getpgrp() {
		t.Fatalf("Info(self) = %+v; want pid %d, parent %d, group %d", p, os.Getpid(), os.Getppid(), unix.Getpgrp())
	}
	if _, err := procsock.Info(1 << 30); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("Info of a pid no process has answered %v; want ESRCH", err)
	}
}

// A process that has exited and waits for its parent to reap it, a zombie,
// has no record: Info answers ESRCH, as for a process that has gone. What
// reads the records (GroupRunsOn, the interrupt's tree) counts on it.
func TestInfoOfAZombieAnswersESRCH(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })
	// The child is a zombie once kern.proc.pid says SZOMB (bsd/sys/proc.h).
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			t.Fatalf("read process %d: %v", pid, err)
		}
		if kp.Proc.P_stat == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited 10 s for process %d to exit unreaped", pid)
		}
	}
	if p, err := procsock.Info(pid); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("Info(zombie %d) = %+v, %v; want ESRCH", pid, p, err)
	}
}
