package models

// Read calls (count-free reads, under the proven switch). A browser page asks
// for what it shows on its own: a poll on a timer, a refetch as React draws
// again once answers come in another order. How many times a page read the
// same thing is then a matter of timing, not of what the application does,
// while what it changed (a write) never is. Only a parser can tell a read from
// a write, by its protocol's own terms (HTTP: a safe method, or a GraphQL
// query); nothing generic does. The end of a test whose process drives a
// browser page (InstanceReq.Browser) does not fail for a read made more or
// fewer times than recorded where the recording proves the count is all that
// differs (agent: countFreeReads); every write is judged as before.

// RegisterReadCall declares, for mocks of kind, which recorded calls are
// reads: isRead reports whether the call a mock recorded changes nothing at
// its dependency, by its protocol. Predicates for one kind are OR-ed; a nil
// one is ignored.
//
// The returned function removes this registration; production code ignores
// it, tests use it to leave the registry as they found it.
func RegisterReadCall(kind Kind, isRead func(*Mock) bool) (unregister func()) {
	return readHooks.add(kind, isRead)
}

var readHooks mockPredicates

// IsReadCall reports whether m recorded a read, as its parser declared
// (RegisterReadCall).
func IsReadCall(m *Mock) bool {
	return m != nil && readHooks.match(m)
}
