package models

// The kinds of a miss under the proven switch (design v14 §5.3): where the
// call that no mock answered was made, and why none could. Every kind fails
// the test it is charged to. Without the switch no miss carries a kind.
const (
	// MissInTest is a call made while a test was open that none of the
	// test's own mocks, nor any protocol-phase mock, answers.
	MissInTest = "in_test"
	// MissExtraCall is a call made while a test was open that is identical
	// to a call the test's own mocks have already answered as many times as
	// it was recorded: each test mock answers at most one request.
	MissExtraCall = "extra_call"
	// MissOutsideTest is a call made while no test was open that the pool
	// slice served then does not answer. Under keploy mock it is charged to
	// no test: the test framework sets each test's window, and keploy moves
	// no call into one (the run's receipt lists it, Receipt.UnchargedMisses).
	// One filed as a test ended, made after it began, is that test's.
	MissOutsideTest = "outside_test"
)

// KindOutsideEveryTest names a call no recording answered that is charged to
// no test because the application made it outside every test (between two
// tests; UnmatchedCall.Between): it fails the run, and no test. It is the
// kind field of the line keploy prints for such a call, in the form keploy's
// docs document ("outside every test").
const KindOutsideEveryTest = "outside_every_test"

// MissMark is where a miss's call was when its parser marked it, its first
// read of the round under the proven switch (MockMismatchReport.Mark): the
// staging (Epoch) and the round's boundary count (Bounds) the read was made
// at, and the test whose view it found (Test, "" for a pool view or where the
// store did not name it). Every view's install moves the boundary count, so
// a mark tells whether the call was made before or after a test's view was
// installed, however late keploy decided it and its miss reached the agent.
// Known is false for a parser that marks no round, and for a store that
// keeps no boundary count: such a miss is placed by when it was filed.
type MissMark struct {
	Epoch, Bounds uint64
	Test          string
	Known         bool
}

// Before reports whether the mark was made before the round at epoch and
// bounds (a test view's install, read with the same lock): in an earlier
// staging, or earlier in the same one. False for a mark not known.
func (k MissMark) Before(epoch, bounds uint64) bool {
	if !k.Known {
		return false
	}
	return k.Epoch < epoch || (k.Epoch == epoch && k.Bounds < bounds)
}

// OutsideEveryTest reports whether keploy's replay fails the set with c as a
// call outside every test (KindOutsideEveryTest), under the proven switch:
// made between two tests (Between), or made in a test and decided only after
// that test ended (Late), unless a parser held it for a later test's
// recording (Held), which the replay says as held.
func (c UnmatchedCall) OutsideEveryTest() bool { return c.Between || c.Late && !c.Held }
