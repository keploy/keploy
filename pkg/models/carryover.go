package models

import "time"

// CarryOverLookahead is how far ahead of its own release window a carry-over
// mock becomes reachable, in recorded time: a registered mock recorded at t is
// reachable once the release window of t-CarryOverLookahead is current (see
// WindowSchedule). It covers a publish that the application makes while
// handling a pushed message and that lands a few short windows before its own
// because the recorded push-to-publish chain crossed them — about 11x the
// longest push handler in the recording this was measured on (86 ms). It is a
// bound on time, not a rule about content.
const CarryOverLookahead = time.Second

// RegisterCarryOver declares that the per-test mocks of kind for which
// isCarryOver returns true do not depend on one test window: the application
// can produce them a few windows away from where they were recorded, because
// it produces them asynchronously (a publish made while handling a server-push
// message), and the replay runs its tests back to back.
//
// The agent's MockManager then keeps such a mock reachable from the release
// window of its recorded time minus CarryOverLookahead until it is consumed,
// instead of only inside its own window: it is loaded ahead of its window, and
// when its window closes unconsumed it moves to the carry-over tier rather than
// being dropped. Consuming it there (DeleteFilteredMock falls back per-test,
// then startup, then carry-over) is reported with MockState.CarryOver.
//
// Registering a kind also marks the other per-test mocks of that kind that it
// consumes outside the current window through MarkMockAsUsed (a server push
// delivered after its window) as CarryOver, so the replay attributes them to
// their own window rather than to the running test.
//
// isCarryOver looks at the mock alone (its Kind and Spec.Metadata), is called
// on every per-test mock of kind at ingest and at each window change, and must
// be cheap. Call RegisterCarryOver from an init function. Several registrations
// for one kind are OR-ed; a nil isCarryOver is ignored. Kinds nobody registers
// are unaffected.
//
// The returned function removes this registration; production code ignores
// it, tests use it to leave the registry as they found it.
func RegisterCarryOver(kind Kind, isCarryOver func(*Mock) bool) (unregister func()) {
	return carryOverHooks.add(kind, isCarryOver)
}

var carryOverHooks mockPredicates

// IsCarryOver reports whether m is a per-test mock that a RegisterCarryOver
// predicate accepts. The mock's lifetime must already be derived. A
// window-bound kind (WindowBound) never carries over, whatever is registered
// for it: carrying it into a later window is serving it in a test it does not
// belong to.
func IsCarryOver(m *Mock) bool {
	return m != nil && !WindowBound(m.Kind) && m.TestModeInfo.Lifetime == LifetimePerTest && carryOverHooks.match(m)
}

// CarryOverKind reports whether any RegisterCarryOver predicate is registered
// for kind. Never for a window-bound kind (see IsCarryOver).
func CarryOverKind(kind Kind) bool {
	return !WindowBound(kind) && carryOverHooks.hasKind(kind)
}

// CarryOverRegistered reports whether any kind registered a carry-over
// predicate. When none did, every carry-over code path is skipped, and the
// agent does not keep the recorded windows of the set being replayed
// (MockFilterParams.RecordedWindows).
func CarryOverRegistered() bool {
	return carryOverHooks.any()
}
