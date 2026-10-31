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
