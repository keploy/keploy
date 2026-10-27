package models

func init() {
	// Declared here, not in the HTTP parser, so every binary that removes
	// mocks knows it: whatever links the mock store links this package.
	RegisterMockDeps(HTTP, httpMockDeps)
}

// HTTPStatefulSequenceKey names a stateful HTTP sequence whose recordings are
// reusable across tests (StatefulSequenceKey).
func HTTPStatefulSequenceKey(sequence string) string {
	return "http stateful sequence " + sequence
}

// httpMockDeps links the recordings of one stateful HTTP sequence, so that
// whatever removes mocks keeps the sequence whole (founder decision 27). A
// replay serves a sequence's recordings in record order and then the last
// again (ConsumeCursorSaturate), across tests when they are reusable: the
// prune after a replay used to keep the ones the replay consumed and drop the
// rest, so after a test that failed having consumed one member (the prune
// drops a failed test's mocks unless preserveFailedMocks is set, and neither
// keploy test by default nor the k8s-proxy's auto-replay sets it), every
// later test was answered with the recording after its own.
//
// The sequence is the one the HTTP matcher's cursor groups by
// (StatefulSequenceKey: method, URL and body, byte-exact). Only reusable
// sequences are linked. A per-test one (a recorder that states lifetimes) is
// served to the test it was recorded in, its cursor started over for each
// test, so another test's failure cannot shift it, and linking it across
// tests would keep every test's recordings of the request for any one's sake.
// The per-test recordings no open test owns (cursorGroup's other band: the
// startup band, those mapped to no test and, while no test is open, every
// test's) are served as one sequence across stagings; they are not linked
// here (the prune keeps the startup band's by its startup cutoff). Under the
// proven switch keploy test does not prune at all (--remove-unused-mocks is
// ignored there), so the strict and variable passes' walks need no links.
// They would if it did: a test's own mocks answer once each with no cursor,
// but the pool served between tests keeps its served marks across the gaps
// of a test set. An async lane's recordings are served by the async engine,
// not the cursor (their own rule keeps what a replay serves).
//
// Each member provides and requires the sequence: keeping any keeps all. Each
// answers on its own, so the requirement is standalone.
func httpMockDeps(m *Mock) MockDeps {
	if m == nil || m.IsAsync() {
		return MockDeps{}
	}
	key, ok := StatefulSequenceKey(m)
	if !ok || IsPerTestSequenceKey(key) {
		return MockDeps{}
	}
	k := HTTPStatefulSequenceKey(key)
	return MockDeps{Provides: []string{k}, Requires: []MockRequirement{{Key: k, Standalone: true}}}
}
