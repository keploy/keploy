package replay

import (
	"sort"
	"sync"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// requestsUnread are the test cases of the set RunTestSet is running whose
// opening filter, in record's full round, carried their request
// (models.MockFilterParams.TestRequest) and was answered by an agent that did
// not say its learn step took it (models.TestRequestReadHeader): an agent
// older than the replay. It learned from those tests without their requests,
// so a value the application took from a request (a part of it, a pick from
// it by the clock) may have been learned as noise, an echo or a variable
// whose bind takes any value: none of it is kept (withoutUnreadLearning).
type requestsUnread struct {
	mu    sync.Mutex
	tests map[string]bool
}

// reset forgets the previous set's.
func (u *requestsUnread) reset() {
	u.mu.Lock()
	u.tests = nil
	u.mu.Unlock()
}

// note marks test.
func (u *requestsUnread) note(test string) {
	u.mu.Lock()
	if u.tests == nil {
		u.tests = map[string]bool{}
	}
	u.tests[test] = true
	u.mu.Unlock()
}

// set is a copy of the marked tests, nil when none is.
func (u *requestsUnread) set() map[string]bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.tests) == 0 {
		return nil
	}
	out := make(map[string]bool, len(u.tests))
	for t := range u.tests {
		out[t] = true
	}
	return out
}

// noteOpening reads the agent's answer to params, a filter that opened a test
// case and was answered: one that carried the test case's request
// (record's full round) and that the agent did not say its learn step took
// marks the test case (requestsUnread). An instrumentation that cannot tell
// (no pkg.TestRequestReader) took none.
func (r *Replayer) noteOpening(params models.MockFilterParams) {
	if params.TestRequest == nil {
		return
	}
	if rd, ok := r.instrumentation.(pkg.TestRequestReader); ok && rd.AgentReadTestRequest() {
		return
	}
	r.requestsUnread.note(params.TestRequest.Test)
}

// withoutUnreadLearning is consumed without what the learn step learned on
// each mock a test whose request the agent did not read consumed (unread,
// consumers: the tests that consumed each mock): its noise (ReqBodyNoise),
// its variables (an echo, a use, a bind) and the variables it bound at an
// earlier mock (Binds), and what it took out, as "<mock> <what> (<kind>)",
// sorted. A variable is no safer than noise there: the place it is bound at
// takes any value its replay sends (a pick from the request bound at its
// first call, and only asserted equal at the next), so nothing an agent
// learned without the request is kept. consumed is not changed: the result
// holds copies of the states it changes.
func withoutUnreadLearning(consumed map[string]models.MockState, consumers map[string][]string, unread map[string]bool) (map[string]models.MockState, []string) {
	if len(unread) == 0 {
		return consumed, nil
	}
	out := make(map[string]models.MockState, len(consumed))
	var dropped []string
	for name, st := range consumed {
		took := false
		for _, test := range consumers[name] {
			if unread[test] {
				took = true
				break
			}
		}
		if !took {
			out[name] = st
			continue
		}
		for field := range st.ReqBodyNoise {
			dropped = append(dropped, name+" "+field+" (noise)")
		}
		for v, mv := range st.Variables {
			kind := "variable"
			if mv.Echo {
				kind = "echo"
			}
			dropped = append(dropped, name+" "+v+" ("+kind+")")
		}
		for at, vars := range st.Binds {
			for v := range vars {
				dropped = append(dropped, name+" "+v+" (bound at "+at+")")
			}
		}
		st.ReqBodyNoise, st.Variables, st.Binds = nil, nil, nil
		out[name] = st
	}
	sort.Strings(dropped)
	return out, dropped
}

// readLearning is consumed, what record's full round learned in testSetID,
// without what the agent learned as noise or as an echo for a test whose
// request it did not say it read (withoutUnreadLearning), which it says,
// naming the tests and the fields it did not keep.
func (r *Replayer) readLearning(testSetID string, consumed map[string]models.MockState, consumers map[string][]string) map[string]models.MockState {
	unread := r.requestsUnread.set()
	if unread == nil {
		return consumed
	}
	kept, dropped := withoutUnreadLearning(consumed, consumers, unread)
	r.logger.Warn("keploy did not keep what its agent learned on the mocks these tests consumed: the agent did not say it read their test cases' requests, so a value the application took from a request may have been learned as noise, or as a variable that takes any value; the recording keeps comparing those fields",
		zap.String("testSetID", testSetID),
		zap.Strings("tests", sortedTests(unread)),
		zap.Strings("notKept", dropped),
		zap.String("next_step", "run the keploy agent of this keploy version"))
	return kept
}

// sortedTests are set's tests, sorted.
func sortedTests(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
