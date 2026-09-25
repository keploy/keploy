//go:build darwin

package utils

import (
	"os"
	"sort"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDescendantsOfWalksTheWholeTree(t *testing.T) {
	// 1 -> 10 -> 11 -> 12, and 10 -> 13; 20 belongs to someone else.
	parents := map[int]int{1: 0, 10: 1, 11: 10, 12: 11, 13: 10, 20: 1}
	got := descendantsOf(10, parents)
	sort.Ints(got)
	if want := []int{11, 12, 13}; len(got) != len(want) || got[0] != 11 || got[1] != 12 || got[2] != 13 {
		t.Fatalf("descendantsOf(10) = %v, want %v", got, want)
	}
}

// The table is read from the kernel while processes come and go, and its top
// is its own parent (kernel_task: pid 0, parent 0). A walk that trusted it to
// be a tree could spin forever inside an interrupt.
func TestDescendantsOfSurvivesALoop(t *testing.T) {
	parents := map[int]int{0: 0, 1: 0, 30: 31, 31: 30}
	done := make(chan []int, 1)
	go func() { done <- descendantsOf(0, parents) }()
	select {
	case got := <-done:
		sort.Ints(got)
		if len(got) != 1 || got[0] != 1 {
			t.Fatalf("descendantsOf(0) = %v, want [1]", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("descendantsOf never finished walking a table that loops")
	}
	done2 := make(chan []int, 1)
	go func() { done2 <- descendantsOf(30, parents) }()
	select {
	case got := <-done2:
		if len(got) != 1 || got[0] != 31 {
			t.Fatalf("descendantsOf(30) = %v, want [31]", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("descendantsOf never finished walking a table that loops")
	}
}

// The groups findChildPIDs hands back are the kernel's, read from the same
// snapshot as the tree. A pid that snapshot did not hold is an error, never a
// group of 0: signalled as -0, that is keploy's own process group.
func TestFindChildPIDsReadsGroupsFromItsSnapshot(t *testing.T) {
	_, groupOf, err := findChildPIDs(os.Getpid())
	if err != nil {
		t.Fatalf("findChildPIDs: %v", err)
	}
	if got, err := groupOf(os.Getpid()); err != nil || got != syscall.Getpgrp() {
		t.Fatalf("groupOf(self) = %d, %v; want this process's group %d", got, err, syscall.Getpgrp())
	}
	if got, err := groupOf(-1); err == nil {
		t.Fatalf("groupOf(a pid the snapshot never held) = %d, want an error", got)
	}
}

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
