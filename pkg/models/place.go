package models

import "time"

// A recorded call's place is the moment of the recording a replay must reach
// before it serves the call: the window that holds it is the test it is
// served to, a moment before every window is start-up traffic, and a moment
// in the gap between two windows belongs to no test (DeriveWindowMappings,
// the agent's window loads and the mock manager's window filters). For most
// calls the place is when the call was asked: its request time.
//
// A call a parser holds until its answer comes, and whose answer carries what
// happened while it waited, is placed at its answer instead: a consumer's
// long poll (a Kafka Fetch) sent while one test runs, or while the app boots,
// and answered in the next test with the record that test produced. Placed at
// its request, it was served to the test it was sent in, or at boot, before
// the record it carries was produced; or, sent between two tests and answered
// in the next, it needed the in-flight rule (InFlightEligible) to reach that
// test, which still gave it to the next test only, whenever its answer came.
// Placed at its answer, it is served to the test its answer was recorded in,
// and never earlier: the parser's hold of a caught-up poll waits for it, and
// matches again at each window change.

// RegisterAnswerPlaced declares that the per-test mocks of kind that match
// accepts are placed at their answer (PlaceOf): a call the kind's parser holds
// until a match is reachable, matching again at each window change, as a
// broker holds a long poll. A parser that answers a call it has no match for
// at once must not declare it: its call would be a miss in the test it was
// sent in.
//
// match looks at the mock alone (its Kind and Spec.Metadata), is called on
// every per-test mock of kind wherever a mock is placed, and must be cheap.
// Call it from an init function. Several registrations for one kind are OR-ed.
// A nil match is a programming error, and panics. Kinds nobody registers are
// unaffected.
//
// The returned function removes this registration; production code ignores
// it, tests use it to leave the registry as they found it.
func RegisterAnswerPlaced(kind Kind, match func(*Mock) bool) (unregister func()) {
	if match == nil {
		panic("models.RegisterAnswerPlaced(" + string(kind) + "): a nil match")
	}
	return answerPlacedHooks.add(kind, match)
}

var answerPlacedHooks mockPredicates

// AnswerPlacedRegistered reports whether any kind is placed at its answer:
// when none is, every mock is placed at its request.
func AnswerPlacedRegistered() bool { return answerPlacedHooks.any() }

// IsAnswerPlaced reports whether m is placed at its answer (PlaceOf): a
// per-test mock a RegisterAnswerPlaced registration accepts, with a recorded
// answer no earlier than its request (one stamped before it, which only the
// proven switch keeps, is placed at its request). A carry-over mock
// (RegisterCarryOver, a StagingRelease need) is released by its request time
// whatever else is registered, a StagingWithStartup need's mock is staged
// with the set's startup by its request time (DiskMocks.LoadStaged), and a
// window-bound kind (a connection failure) is served from its own window
// only: none is.
func IsAnswerPlaced(m *Mock) bool {
	if m == nil || !answerPlacedHooks.match(m) || m.Spec.ResTimestampMock.IsZero() || m.Spec.ResTimestampMock.Before(m.Spec.ReqTimestampMock) {
		return false
	}
	return !WindowBound(m.Kind) && !carryOverHooks.match(m) && !releaseHooks.match(m) && !stagedHooks.match(m) && lifetimeOf(m) == LifetimePerTest
}

// PlaceOf is m's place: its answer's time for a mock placed at its answer
// (IsAnswerPlaced), its request time for every other. Every site that places a
// per-test mock in a window reads it: the agent's on-disk window and start-up
// loads (DiskMocks), its filter of a window's mocks, the mock manager's
// staging, its windowed read, the test that owns a mock and whether a consume
// is out of its window, the window mappings (DeriveWindowMappings), a
// mapping-mode replay's load (pkg.FilterTcsMocksMappingInWindow) and the
// replay's attribution of a consume (MockState.AnswerPlaced). A --sync
// recording's mock manager attributes by request time
// (syncMock.ownerOrSharedLocked), as it does a call in flight: a mapping it
// writes lists a poll under the test it was sent in, which a mapping-mode
// replay reads alongside the polls placed in each window.
//
// A kind registers from an init function, before any mock is read: the
// agent's on-disk store keeps the place it computed when a mock was added.
func PlaceOf(m *Mock) time.Time {
	if m == nil {
		return time.Time{}
	}
	if IsAnswerPlaced(m) {
		return m.Spec.ResTimestampMock
	}
	return m.Spec.ReqTimestampMock
}
