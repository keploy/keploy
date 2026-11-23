//go:build darwin

package utils

import (
	"slices"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/agent/procsock"
	"go.keploy.io/server/v3/pkg/agent/procsock/spawnhold"
)

// An app whose process group holds a posix_spawn in the kernel (a file action
// that opens a FIFO no one reads) is stopped with no hang. A signal sent to
// the group waits in XNU until that spawn ends, and the stop waited with it
// for as long as the spawn was held (here, the holder's own release after
// 60 s). The stop signals each process of the group by its pid, the spawn's
// maker before the spawn's child: a maker that takes SIGINT ends, and so does
// its spawn, at once; one that ignores SIGINT is killed by its pid after the
// grace, which ends the spawn, and the stop says it killed.
func TestInterruptProcessTreeStopsAGroupWhoseSpawnIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ignores bool
	}{
		{"the spawn's maker takes SIGINT", false},
		{"the spawn's maker ignores SIGINT", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := spawnhold.HoldInGroup(t, tc.ignores)
			if p, err := procsock.InfoOrZombie(h.Child); err != nil || p.PGID != h.Holder {
				t.Fatalf("the held spawn's child %d is in group %d (%v); want the holder's %d", h.Child, p.PGID, err, h.Holder)
			}
			core, logs := observer.New(zap.DebugLevel)
			type result struct {
				killed string
				err    error
				took   time.Duration
			}
			done := make(chan result, 1)
			go func() {
				began := time.Now()
				killed, err := InterruptProcessTreeKilled(zap.New(core), h.Holder, syscall.SIGINT)
				done <- result{killed, err, time.Since(began)}
			}()
			var r result
			select {
			case r = <-done:
			case <-time.After(TreeInterruptGrace + 15*time.Second):
				t.Fatalf("the stop of a group whose spawn is held had not returned after %s", TreeInterruptGrace+15*time.Second)
			}
			if r.err != nil {
				t.Fatalf("InterruptProcessTree: %v", r.err)
			}
			for _, e := range logs.FilterLevelExact(zap.ErrorLevel).All() {
				t.Errorf("the stop logged an ERROR: %q %v", e.Message, e.ContextMap())
			}
			if killed := r.killed != ""; killed != tc.ignores {
				t.Errorf("the stop said it killed %v (%q) in %s; want %v", killed, r.killed, r.took, tc.ignores)
			}
			if !tc.ignores && r.took >= TreeInterruptGrace {
				t.Errorf("the stop took %s: the holder, which takes SIGINT, was not signalled by its pid", r.took)
			}
			for _, pid := range []int{h.Holder, h.Child} {
				if p, err := procsock.InfoOrZombie(pid); err == nil && !p.Zombie && p.PGID == h.Holder {
					t.Errorf("process %d of the held spawn's group runs after the stop", pid)
				}
			}
		})
	}
}

// The stop's signals go to a group's processes parents first: the maker of a
// held spawn before the spawn's child, whose own signal waits until the
// spawn ends.
func TestParentsFirstPutsEachProcessAfterItsParent(t *testing.T) {
	parents := map[int]int{
		500: 1,   // the group's leader, its parent outside the group
		510: 500, // a child
		520: 510, // a grandchild: a held spawn's child
		530: 500,
		540: 77, // a process the group gained, its parent outside it
		550: 540,
		560: 560, // its own parent: no loop is followed
	}
	for round := 0; round < 20; round++ { // map order varies
		got := parentsFirst(parents)
		if len(got) != len(parents) {
			t.Fatalf("parentsFirst gave %v; want each of %d processes once", got, len(parents))
		}
		for pid, ppid := range parents {
			at, parentAt := slices.Index(got, pid), slices.Index(got, ppid)
			if at < 0 {
				t.Fatalf("parentsFirst gave %v without %d", got, pid)
			}
			if parentAt >= 0 && ppid != pid && parentAt > at {
				t.Fatalf("parentsFirst gave %v: %d before its parent %d", got, pid, ppid)
			}
		}
	}
	loop := map[int]int{600: 601, 601: 600, 602: 1}
	if got := parentsFirst(loop); len(got) != 3 || got[0] != 602 {
		t.Fatalf("parentsFirst of a loop gave %v; want 602 first and each of the 3 once", got)
	}
}
