package integrations

// ONE CALL MORE THAN THE OPEN TEST RECORDED, AND THE BACKGROUND (under the
// proven switch, a set served by the background rules: S12Background).
//
// In a test, the store shows the set's background (its pool and the worker
// scope the run reached) after the open test's own recordings, so a call the
// application makes while the test is open, recorded between tests or by a
// worker, is answered. A call identical to one the open test's own
// recordings already answered is no such call: it is the test's own call,
// made once more than it was recorded (models.MissExtraCall), and the
// background never answers it. Answered by the background's copy of it, a
// duplicate of a recorded call (a write made twice, a retried request) would
// pass.
//
// The parser decides whether a call repeats the open test's own recordings,
// as its matcher reads "the same call", and matches that call on the store
// HideBackground returns: the background is in none of its reads and every
// consume door refuses it, so the call finds the test's own recordings not
// yet answered, if any, and otherwise misses as one call more than the test
// recorded. The background's recording stays unspent for a call it is the
// recording of.

// BackgroundHider is an optional capability of a store HoldToRound returned
// for one call: the store as a call one more than the open test's own
// recordings of it reads it (HideBackground). The agent's MockManager's held
// store implements it, and every MockMemDb wrapper that forwards
// RoundHolder must forward it.
type BackgroundHider interface {
	// HideBackground is the store with the set's background recordings
	// (the pool's and the worker scope's data-plane recordings, under the
	// background rules, none of them the open test's own) out of every
	// read and refused at every consume and spend door while a test is
	// open. It is one call's, as the held store is: it may change the
	// store in place and return it.
	HideBackground() MockMemDb
}

// HideBackground is db, a store HoldToRound returned for one call that is
// one more than the open test's own recordings of it, with the set's
// background out of every read and refused at every consume door
// (BackgroundHider): neither the pool nor the worker scope answers the call.
// db itself when it hides nothing (no BackgroundHider): a store that serves
// no set by the background rules shows no background in a test.
func HideBackground(db MockMemDb) MockMemDb {
	if h, ok := db.(BackgroundHider); ok {
		return h.HideBackground()
	}
	return db
}
