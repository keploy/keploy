package models

import "time"

// OwnerCandidate is a test that may own a call, as CallOwner weighs it: the
// worker that reported the test (a test scope's pid; 0 for a window's, or a
// scope with none) and when the test began.
type OwnerCandidate struct {
	PID   uint32
	Start time.Time
}

// CallOwner is, under the proven switch, the one rule by which keploy tells
// which of the tests a call may belong to made it: the Generic recorder's
// held first exchange (syncMock.SyncMockManager.TestSpanAt) and keploy mock
// record's mapping of a per-test mock (testAt) both ask it, with the same
// candidates and the same chain, so a call the recorder states as one test's
// is that test's in the mapping too. cands are the tests whose span holds the
// call's time; chain is the calling connection's process and its ancestors,
// nearest first (utils.ProcAncestors; nil when unknown). It returns the
// owner's index in cands, or -1 when no one owner can be told:
//
//   - A test whose worker is on the chain (the process itself, or a process
//     it descends from: a browser or CLI the worker started) may own the
//     call, and only such tests then. When they are all one worker's, the
//     innermost of them, the latest begun, owns it (a subtest inside its
//     parent). When they are more than one worker's, or the innermost two
//     began at one instant, no owner can be told.
//   - With no candidate's worker on the chain, the only candidate owns it;
//     of more than one, none can be told.
//
// No time decides between candidates beyond that nesting: two tests whose
// spans overlap at the call, with nothing in the process tree to tell them
// apart, are never split by which began first.
func CallOwner(cands []OwnerCandidate, chain []uint32) int {
	owner, worker := -1, uint32(0)
	tied := false
	for i, c := range cands {
		if c.PID == 0 || !onChain(chain, c.PID) {
			continue
		}
		switch {
		case owner < 0:
			owner, worker = i, c.PID
		case c.PID != worker:
			return -1
		case c.Start.After(cands[owner].Start):
			owner, tied = i, false
		case c.Start.Equal(cands[owner].Start):
			tied = true
		}
	}
	if owner >= 0 {
		if tied {
			return -1
		}
		return owner
	}
	if len(cands) == 1 {
		return 0
	}
	return -1
}

func onChain(chain []uint32, pid uint32) bool {
	for _, p := range chain {
		if p == pid {
			return true
		}
	}
	return false
}
