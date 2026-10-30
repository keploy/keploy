package models

import (
	"slices"
	"sort"
)

// MockOwners is, for owned (each test's owned mocks, by test name), the tests
// that own each mock, by mock name, sorted: what UnmatchedCall.NameRecordedIn
// reads. Both replays build it from the per-test table they served: keploy
// test's from its exact plan, keploy mock replay's from the table it pushed
// to the agent.
func MockOwners(owned map[string][]string) map[string][]string {
	out := map[string][]string{}
	for test, names := range owned {
		for _, n := range names {
			if !slices.Contains(out[n], test) {
				out[n] = append(out[n], test)
			}
		}
	}
	for _, tests := range out {
		sort.Strings(tests)
	}
	return out
}

// NameRecordedIn names, on c, a call test made that no mock answered, the
// other test that owns the recording it is (RecordedIn), and reports whether
// it named one. c must be NotOwned: the parser matched it to ClosestMock, a
// recording test does not own. owners (MockOwners) gives ClosestMock's
// owners; the first is named. A recording test owns too, or one no test owns
// (recorded between tests), names none, and so does a call that is not
// NotOwned: it leaves c as it was. Under the proven switch only: without it
// no call is NotOwned.
func (c *UnmatchedCall) NameRecordedIn(test string, owners map[string][]string) bool {
	by := owners[c.ClosestMock]
	if !c.NotOwned || c.ClosestMock == "" || len(by) == 0 || slices.Contains(by, test) {
		return false
	}
	c.RecordedIn = by[0]
	return true
}
