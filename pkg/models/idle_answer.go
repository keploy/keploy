package models

// Idle answers (design v14, the founder's D4). A dependency a worker polls
// answers "nothing yet" for as long as it has nothing: an SQS long poll that
// found no message, a Kafka fetch that returned no record. How many such
// answers a run sees is a matter of timing, not of what the application does,
// so an idle answer is the set's worker scope's wherever it was recorded
// (Mapping.Worker, DeriveWindowMappings), and not one the set's end requires
// answered. Under exact serving it answers at most one poll, as any recording
// does; its parser answers a poll past every recorded one as the dependency
// would (enterprise SQS: its own empty answer). Only a parser can tell an idle
// answer from a meaningful one; nothing generic does: two identical requests
// answered alike are not idle for that alone.

// RegisterIdleAnswer declares, for mocks of kind, which recorded answers are
// idle: isIdle reports whether a recorded mock's answer is one its dependency
// gives while it has nothing. Predicates for one kind are OR-ed; a nil one is
// ignored.
//
// The returned function removes this registration; production code ignores
// it, tests use it to leave the registry as they found it.
func RegisterIdleAnswer(kind Kind, isIdle func(*Mock) bool) (unregister func()) {
	return idleHooks.add(kind, isIdle)
}

var idleHooks mockPredicates

// IsIdleAnswer reports whether m's recorded answer is idle, as its parser
// declared (RegisterIdleAnswer).
func IsIdleAnswer(m *Mock) bool {
	return m != nil && idleHooks.match(m)
}
