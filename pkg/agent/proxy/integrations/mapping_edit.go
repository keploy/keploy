package integrations

import "strings"

// InListing is c, of a read whose view was installed with listed: of keploy
// mock replay's mappings (mappings.yaml), the tests whose lists hold each
// recording, by recording name, as its agent handed them to the store
// (agent.ExactListingSetter). A store sets it as it marks the call, from the
// view it read, with InRouting. nil: the agent handed none.
func (c CallRound) InListing(listed map[string][]string) CallRound {
	c.listed = listed
	return c
}

// ListedFor is the tests whose lists in keploy mock replay's mappings hold
// the recording name, as c's read found them (InListing): none when no
// test's does (a startup: or worker: entry, or another test process's).
// known is false when the store was handed no listing: an agent that does
// not hand one, the zero CallRound, a keploy test set, and the gap after a
// session reset until the session's table comes.
func (c CallRound) ListedFor(name string) (tests []string, known bool) {
	if c.listed == nil {
		return nil, false
	}
	return c.listed[name], true
}

// NotOwnedMappingEdit is what a parser tells, under keploy mock replay
// (call.Unrouted() == models.UnroutedUnstaged), of a refused call that is
// the recording name, which the view the call read does not show: keploy
// mock marks no set and routes none, the test framework's begin and end set
// each test's window, and a call made outside the window its recording is
// listed for is a mapping edit the reader makes (worker:), never a record
// again. It names the test whose window the call was made in (call's
// Sequence; between tests when none) and the tests whose lists hold name
// (call's ListedFor). With no listing it names the receipt, whose
// recorded_in holds them. Every parser words this miss with it.
func NotOwnedMappingEdit(name string, call CallRound) string {
	sv, _ := call.Sequence()
	made := "between tests"
	if sv.Test != "" {
		made = "in " + sv.Test + "'s window"
	}
	const scope = " keploy mock does not decide whose a call is: each test's begin and end set its window."
	const worker = " to worker: in mappings.yaml, which answers it in any test's window or between tests and requires it of none"
	listed, known := call.ListedFor(name)
	switch {
	case len(listed) > 0:
		owners, from := strings.Join(listed, ", "), "out of "+listed[0]+"'s list"
		if len(listed) > 1 {
			owners = strings.Join(listed[:len(listed)-1], ", ") + " and " + listed[len(listed)-1]
			from = "out of the lists of " + owners
		}
		return "This call, made " + made + ", is " + name + ", which mappings.yaml lists for " + owners + "." + scope +
			" If the call is the application's own work (a queue consumer, a scheduler), move " + name + " " + from + worker +
			"; if it is the test's own, it was made outside the window of the test that recorded it."
	case known:
		return "This call, made " + made + ", is " + name + ", which mappings.yaml lists for no test: in startup:, which answers between tests only, or for another test process." + scope +
			" If a test needs it in its window, move " + name + " to the worker: of the test process that made the call, in mappings.yaml, which answers it in any of that process's tests' windows or between them and requires it of none."
	default:
		return "This call, made " + made + ", is " + name + ", which mappings.yaml lists where this replay did not make it: for another test (the receipt names it under recorded_in), in startup:, which answers between tests only, or for another test process." + scope +
			" If the call is the application's own work (a queue consumer, a scheduler), move " + name + worker +
			"; if it is the test's own, it was made outside the window of the test that recorded it."
	}
}
