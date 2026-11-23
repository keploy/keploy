//go:build darwin

package utils

import (
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent/procsock/spawnhold"
)

// isZombie reports whether pid has exited and is waiting for its parent to
// reap it.
func isZombie(t *testing.T, pid int) bool {
	t.Helper()
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		t.Fatalf("read process %d: %v", pid, err)
	}
	return p.Proc.P_stat == 5 // SZOMB, sys/proc.h
}

// While a posix_spawn is held in the kernel, the tree an interrupt signals
// and whether a group runs on are still read at once: neither waits for the
// held spawn, as reading them through the kern.proc sysctls did.
func TestTheTreeIsReadWhileASpawnIsHeld(t *testing.T) {
	h := spawnhold.Hold(t)
	type tree struct {
		children []int
		group    int
		groupErr error
		err      error
		runs     bool
	}
	done := make(chan tree, 1)
	go func() {
		var r tree
		var groupOf func(int) (int, error)
		if r.children, groupOf, r.err = findChildPIDs(unix.Getpid()); r.err == nil {
			r.group, r.groupErr = groupOf(h.Holder)
		}
		r.runs = GroupRunsOn(unix.Getpgrp())
		done <- r
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if !slices.Contains(r.children, h.Holder) || !slices.Contains(r.children, h.Child) {
			t.Fatalf("findChildPIDs(%d) = %v; want the holder %d and its held child %d", unix.Getpid(), r.children, h.Holder, h.Child)
		}
		if r.groupErr != nil || r.group != unix.Getpgrp() {
			t.Fatalf("the holder's group = %d, %v; want %d", r.group, r.groupErr, unix.Getpgrp())
		}
		if !r.runs {
			t.Fatalf("GroupRunsOn(%d) = false for this process's own group", unix.Getpgrp())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading the process tree waited for the held spawn")
	}
}
