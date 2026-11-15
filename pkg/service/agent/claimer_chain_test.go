package agent

import "testing"

// proc is a process of a made-up process table: its parent and creation
// time, and whether it is alive.
type proc struct {
	ppid    int
	created uint64
	dead    bool
}

func descendsIn(table map[int]proc, pid, ancestor int) bool {
	return descendsByCreation(pid, ancestor,
		func(p int) (int, bool) {
			e, ok := table[p]
			return e.ppid, ok
		},
		// An exited process's time comes with false: a walk that used it
		// anyway would go on as if it were alive.
		func(p int) (uint64, bool) {
			e, ok := table[p]
			return e.created, ok && !e.dead
		})
}

// A process two links below the ancestor descends from it, and the ancestor
// from itself; a process of another tree, and the ancestor's own parent, do
// not.
func TestDescendsByCreationFollowsParents(t *testing.T) {
	table := map[int]proc{
		8:   {ppid: 4, created: 100},
		100: {ppid: 8, created: 200},   // the run's CLI
		120: {ppid: 100, created: 300}, // go test
		140: {ppid: 120, created: 400}, // the test binary
		160: {ppid: 8, created: 500},   // another program
	}
	for _, tc := range []struct {
		pid, ancestor int
		want          bool
	}{
		{140, 100, true},
		{120, 100, true},
		{100, 100, true},
		{160, 100, false},
		{8, 100, false},
	} {
		if got := descendsIn(table, tc.pid, tc.ancestor); got != tc.want {
			t.Errorf("descends(%d, %d) = %v, want %v", tc.pid, tc.ancestor, got, tc.want)
		}
	}
}

// The ancestor is itself even when its times cannot be read.
func TestDescendsByCreationTakesTheAncestorAsItself(t *testing.T) {
	if !descendsIn(map[int]proc{}, 100, 100) {
		t.Fatal("a process does not descend from itself")
	}
}

// A process whose parent pid now names a process created after it (the
// parent exited and its pid was taken again) does not descend from that
// process, nor through it.
func TestDescendsByCreationRefusesAReusedParentPID(t *testing.T) {
	table := map[int]proc{
		100: {ppid: 8, created: 200},   // the run's CLI
		120: {ppid: 100, created: 900}, // a new process that took pid 120
		140: {ppid: 120, created: 400}, // its old child, created before it
	}
	if descendsIn(table, 140, 120) {
		t.Fatal("a process descends from the later process that took its parent's pid")
	}
	if descendsIn(table, 140, 100) {
		t.Fatal("a process descends through the later process that took its parent's pid")
	}
	// Created at the same tick as its child, the parent is taken.
	table[120] = proc{ppid: 100, created: 400}
	if !descendsIn(table, 140, 100) {
		t.Fatal("a parent created at its child's tick is not taken")
	}
	// Each link is checked against its own child: a grandparent pid taken
	// by a process created after the parent, though before the child, is
	// another process.
	table[120] = proc{ppid: 100, created: 150}
	if descendsIn(table, 140, 100) {
		t.Fatal("a process descends through a grandparent pid taken after its parent was created")
	}
}

// A parent the snapshot does not list is no parent, whatever pid comes with
// the answer.
func TestDescendsByCreationTakesNoUnlistedParent(t *testing.T) {
	table := map[int]proc{100: {ppid: 8, created: 200}, 140: {ppid: 100, created: 400}}
	unlisted := func(p int) (int, bool) { return table[p].ppid, false }
	created := func(p int) (uint64, bool) { e, ok := table[p]; return e.created, ok }
	if descendsByCreation(140, 100, unlisted, created) {
		t.Fatal("a process descends through a parent the snapshot does not list")
	}
}

// A process whose parent has exited, or is not in the table, descends from
// no one above it, and neither does a process that has exited itself.
func TestDescendsByCreationStopsAtAnExitedOrUnknownProcess(t *testing.T) {
	table := map[int]proc{
		100: {ppid: 8, created: 200},
		120: {ppid: 100, created: 300, dead: true},
		140: {ppid: 120, created: 400},
		160: {ppid: 100, created: 500, dead: true},
		180: {ppid: 999, created: 600},
	}
	for _, tc := range []struct {
		name     string
		pid, anc int
	}{
		{"through an exited parent", 140, 100},
		{"an exited process", 160, 100},
		{"through a parent no table lists", 180, 999},
		{"a process no table lists", 77, 100},
	} {
		if descendsIn(table, tc.pid, tc.anc) {
			t.Errorf("%s: descends(%d, %d) = true", tc.name, tc.pid, tc.anc)
		}
	}
	// The exited parent is the ancestor asked about: still no.
	if descendsIn(table, 140, 120) {
		t.Error("a process descends from its exited parent")
	}
}

// A parent chain of pid 0 or a negative pid ends, and a chain that loops
// (pids at equal times) ends without an answer of yes.
func TestDescendsByCreationEndsAtNoPIDAndOnALoop(t *testing.T) {
	table := map[int]proc{
		0:   {ppid: 0, created: 0},
		4:   {ppid: 0, created: 0},
		140: {ppid: 4, created: 400},
		200: {ppid: 210, created: 700},
		210: {ppid: 200, created: 700},
		300: {ppid: -1, created: 800},
	}
	if descendsIn(table, 140, 0) {
		t.Error("a process descends from pid 0")
	}
	if descendsIn(table, 200, 100) {
		t.Error("a looping chain descends")
	}
	if descendsIn(table, 300, -1) {
		t.Error("a process descends from pid -1")
	}
}

// kin is a process of a made-up process table for directChild: its parent
// (unanswered when noParent), its creation time, and whether it is alive.
// What an unanswered read comes with is kept, so a check that used it anyway
// would take it.
type kin struct {
	ppid     int
	created  uint64
	dead     bool
	noParent bool
}

func directChildIn(table map[int]kin, child, parent int) (uint64, bool) {
	return directChild(child, parent,
		func(p int) (int, bool) {
			e, ok := table[p]
			return e.ppid, ok && !e.noParent
		},
		func(p int) (uint64, bool) {
			e, ok := table[p]
			return e.created, ok && !e.dead
		})
}

// A claim for another process is taken only for a running child of the
// caller, created no earlier than it, and answers the child's own creation
// time: never a grandchild, a process of another parent, the caller itself,
// an ended child, a child whose parent pid a later process took, or a child
// or parent the kernel does not answer for.
func TestDirectChildTakesOnlyARunningChildOfTheCaller(t *testing.T) {
	table := map[int]kin{
		50:  {ppid: 4, created: 100},
		100: {ppid: 50, created: 200},                  // the launcher
		140: {ppid: 100, created: 300},                 // the test binary it started
		141: {ppid: 100, created: 200},                 // started at the launcher's own tick
		160: {ppid: 140, created: 400},                 // the test binary's child
		170: {ppid: 50, created: 260},                  // another program
		180: {ppid: 100, created: 350, dead: true},     // a child that ended
		190: {ppid: 120, created: 250},                 // a child of an earlier holder of pid 120
		120: {ppid: 50, created: 500},                  // a later process that took pid 120
		210: {ppid: 220, created: 300},                 // a child whose parent is unanswered
		230: {ppid: 240, created: 300},                 // a child of an ended process
		240: {ppid: 50, created: 100, dead: true},      // that ended process
		250: {ppid: 100, created: 300, noParent: true}, // a child whose parent read fails
	}
	for _, tc := range []struct {
		name          string
		child, parent int
		want          bool
	}{
		{"the launcher's child", 140, 100, true},
		{"a child created at the launcher's tick", 141, 100, true},
		{"the launcher's grandchild", 160, 100, false},
		{"another program", 170, 100, false},
		{"the launcher itself", 100, 100, false},
		{"the launcher's parent", 50, 100, false},
		{"a child that ended", 180, 100, false},
		{"a child of a pid a later process took", 190, 120, false},
		{"a child whose parent is unanswered", 210, 220, false},
		{"a child of an ended process", 230, 240, false},
		{"a child whose parent read fails", 250, 100, false},
		{"a pid no process has", 999, 100, false},
	} {
		got, ok := directChildIn(table, tc.child, tc.parent)
		if ok != tc.want {
			t.Errorf("%s: directChild(%d, %d) = %d, %v, want %v", tc.name, tc.child, tc.parent, got, ok, tc.want)
			continue
		}
		if want := map[bool]uint64{true: table[tc.child].created}[ok]; got != want {
			t.Errorf("%s: directChild(%d, %d) answered time %d, want %d", tc.name, tc.child, tc.parent, got, want)
		}
	}
}
