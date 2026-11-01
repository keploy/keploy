package integrations

import (
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

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

// madeWhere is where call was made, as a mapping-edit hint says it: in the
// window of the test its Sequence names, or between tests when none.
func madeWhere(call CallRound) string {
	if sv, _ := call.Sequence(); sv.Test != "" {
		return "in " + sv.Test + "'s window"
	}
	return "between tests"
}

// andJoin is tests as a hint lists them: "a", "a and b", "a, b and c".
func andJoin(tests []string) string {
	if len(tests) < 2 {
		return strings.Join(tests, "")
	}
	return strings.Join(tests[:len(tests)-1], ", ") + " and " + tests[len(tests)-1]
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
// recorded_in holds them. HTTP, Generic, Mongo, Postgres and SQS word this
// miss with it.
func NotOwnedMappingEdit(name string, call CallRound) string {
	made := madeWhere(call)
	const scope = " keploy mock does not decide whose a call is: each test's begin and end set its window."
	const worker = " to worker: in mappings.yaml, which answers it in any test's window or between tests and requires it of none"
	listed, known := call.ListedFor(name)
	switch {
	case len(listed) > 0:
		owners, from := andJoin(listed), "out of "+listed[0]+"'s list"
		if len(listed) > 1 {
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

// NamedAgain is, under keploy mock replay (call.Unrouted() ==
// models.UnroutedUnstaged), of a refused call the store named no recording
// made elsewhere for, the recording that is the call which an earlier
// refused call of the staging was already named as (NamedAgainFinder): the
// application made the call again, and the mapping edit the earlier miss was
// told answers one of the two at most. "" under any other replay, whose
// misses keep their words, and from a store that cannot tell. keys and match
// are what the parser asked the store's lookup with.
func NamedAgain(mockDb MockMemDb, kind models.Kind, call CallRound, keys []string, match func(*models.Mock) bool) string {
	if call.Unrouted() != models.UnroutedUnstaged {
		return ""
	}
	f, ok := mockDb.(NamedAgainFinder)
	if !ok {
		return ""
	}
	return f.FindNamedAgain(kind, call, keys, match)
}

// NotOwnedAgainMappingEdit is what a parser tells, under keploy mock replay,
// of a refused call that is the recording name an earlier refused call was
// already named as (NamedAgain): a recording answers one call at most, under
// worker: as in a test's list, so the mapping edit the earlier miss was told
// answers one of them, never this one too. The call is one more than the
// mock set holds, which only a recording of each call answers. It names
// where the call was made and, as NotOwnedMappingEdit does, the tests whose
// lists hold name.
func NotOwnedAgainMappingEdit(name string, call CallRound) string {
	lists := ""
	switch listed, known := call.ListedFor(name); {
	case len(listed) > 0:
		lists = ", which mappings.yaml lists for " + andJoin(listed)
	case known:
		lists = ", which mappings.yaml lists for no test"
	}
	return "This call, made " + madeWhere(call) + ", is " + name + lists + ", again: an earlier call this replay made outside " + name + "'s window was already named as it." +
		" A recording answers one call at most, under worker: as in a test's list, so moving " + name + " to worker: answers one of these calls, never both." +
		" The application made this call more times than the mock set holds it; if it should, the mock set needs a recording of each, which 'keploy mock record' makes."
}
