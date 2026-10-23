package replay

import (
	"context"
	"sort"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// requestsUnread are the test cases of the set RunTestSet is running whose
// opening filter, in record's full round, carried their request
// (models.MockFilterParams.TestRequest) and was answered by an agent that did
// not say its learn step took it (models.TestRequestReadHeader): an agent
// older than the replay. It learned from those tests without their requests,
// so a value the application took from a request (a part of it, a pick from
// it by the clock) may have been learned as noise, an echo or a variable
// whose bind takes any value: none of it is kept (withoutUnreadLearning), and
// a test whose learning is not kept has no verdict, nor has its set
// (markLearningNotKept): it may have passed on that learning alone. RunTestSet
// forgets the previous set's as it begins (reset): test names restart in every
// set. The reasons name the agent by the version it named in its answer to
// the last such filter (agent, pkg.AgentVersionOf; "" where it named none).
type requestsUnread struct {
	mu    sync.Mutex
	tests map[string]bool
	agent string
}

// reset forgets the previous set's.
func (u *requestsUnread) reset() {
	u.mu.Lock()
	u.tests, u.agent = nil, ""
	u.mu.Unlock()
}

// note marks test, whose opening filter the agent that named agent as its
// version answered.
func (u *requestsUnread) note(test, agent string) {
	u.mu.Lock()
	if u.tests == nil {
		u.tests = map[string]bool{}
	}
	u.tests[test] = true
	u.agent = agent
	u.mu.Unlock()
}

// agentVersion is the version the agent named in its answer to the last
// filter noted, "" where it named none.
func (u *requestsUnread) agentVersion() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.agent
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

// sendFilter sends params to the agent (UpdateMockParams), and, of a filter
// that opens a test case carrying its request (record's full round), notes
// the agent's answer to that call (noteOpening): read is whether the answer
// said the learn step took the request (pkg.TestCaseFilterSender, whose
// answer is the call's own, whatever other filter is sent beside it). An
// instrumentation that cannot tell took none.
func (r *Replayer) sendFilter(ctx context.Context, params models.MockFilterParams) error {
	if params.TestRequest == nil {
		return r.instrumentation.UpdateMockParams(ctx, params)
	}
	read := false
	var err error
	if s, ok := r.instrumentation.(pkg.TestCaseFilterSender); ok {
		read, err = s.SendTestCaseFilter(ctx, params)
	} else {
		err = r.instrumentation.UpdateMockParams(ctx, params)
	}
	if err != nil {
		return err
	}
	r.noteOpening(params, read)
	return nil
}

// noteOpening reads the agent's answer to params, a filter that opened a test
// case and was answered (read: whether the answer said its learn step took
// the request): one that carried the test case's request (record's full
// round) and that the agent did not say its learn step took marks the test
// case (requestsUnread), with the version the agent named in its answer.
func (r *Replayer) noteOpening(params models.MockFilterParams, read bool) {
	if params.TestRequest == nil || read {
		return
	}
	r.requestsUnread.note(params.TestRequest.Test, pkg.AgentVersionOf(r.instrumentation))
}

// withoutUnreadLearning is consumed without what the learn step learned on
// each mock a test whose request the agent did not read consumed (unread,
// consumers: the tests that consumed each mock): its noise (ReqBodyNoise),
// its variables (an echo, a use, a bind) and the variables it bound at an
// earlier mock (Binds), and what it took out, as "<mock> <what> (<kind>)",
// sorted, by every test that consumed a mock it took something off (byTest:
// a test whose request was read that consumed the same mock leaned on it as
// well). A variable is no safer than noise there: the place it is bound at
// takes any value its replay sends (a pick from the request bound at its
// first call, and only asserted equal at the next), so nothing an agent
// learned without the request is kept. consumed is not changed: the result
// holds copies of the states it changes.
func withoutUnreadLearning(consumed map[string]models.MockState, consumers map[string][]string, unread map[string]bool) (map[string]models.MockState, []string, map[string][]string) {
	if len(unread) == 0 {
		return consumed, nil, nil
	}
	out := make(map[string]models.MockState, len(consumed))
	var dropped []string
	var byTest map[string][]string
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
		var mock []string
		for field := range st.ReqBodyNoise {
			mock = append(mock, name+" "+field+" (noise)")
		}
		for v, mv := range st.Variables {
			kind := "variable"
			if mv.Echo {
				kind = "echo"
			}
			mock = append(mock, name+" "+v+" ("+kind+")")
		}
		for at, vars := range st.Binds {
			for v := range vars {
				mock = append(mock, name+" "+v+" (bound at "+at+")")
			}
		}
		if len(mock) > 0 {
			dropped = append(dropped, mock...)
			if byTest == nil {
				byTest = map[string][]string{}
			}
			for _, test := range consumers[name] {
				byTest[test] = append(byTest[test], mock...)
			}
		}
		st.ReqBodyNoise, st.Variables, st.Binds = nil, nil, nil
		out[name] = st
	}
	sort.Strings(dropped)
	for test := range byTest {
		sort.Strings(byTest[test])
	}
	return out, dropped, byTest
}

// readLearning is consumed, what record's full round learned in testSetID,
// without what the agent learned for a test whose request it did not say it
// read (withoutUnreadLearning), and, by test, what it did not keep of what
// each test consumed (notKept), which it says, naming the tests and the
// fields: each such test has no verdict (learningNotKept).
func (r *Replayer) readLearning(testSetID string, consumed map[string]models.MockState, consumers map[string][]string) (map[string]models.MockState, map[string][]string) {
	unread := r.requestsUnread.set()
	if unread == nil {
		return consumed, nil
	}
	kept, dropped, notKept := withoutUnreadLearning(consumed, consumers, unread)
	if len(dropped) > 0 {
		r.logger.Error("keploy did not keep what its agent learned on the mocks these tests consumed, and the tests have no verdict: the agent did not say it read their test cases' requests, as an agent older than this keploy does not, so a value the application took from a request may have been learned as noise, or as a variable that takes any value; the recording keeps comparing those fields",
			zap.String("testSetID", testSetID),
			zap.Strings("tests", sortedTests(byName(notKept))),
			zap.Strings("notKept", dropped),
			zap.String("agent", pkg.AgentDidNotSay(r.requestsUnread.agentVersion(), "it read their test cases' requests")),
			zap.String("next_step", learningNotKeptRemedy()))
	}
	return kept, notKept
}

// learningNotKept is why a test's learning was not kept, naming what, and
// the agent by the version it named (agent; "" where it named none)
// (FailureInfo.LearningNotKept).
func learningNotKept(fields []string, agent string) string {
	return "keploy did not keep what its agent learned on the mocks this test consumed (" + firstNames(fields, 10) + "): " + pkg.AgentDidNotSay(agent, "it read the test case's request") + ", so a value the application took from the request may have been learned as noise, or as a variable that takes any value, and this test's run leaned on it; " + learningNotKeptRemedy()
}

// learningNotKeptRemedy is what to do about a test whose learning was not
// kept: run record with an agent that says it read each request, one of this
// keploy's own version.
func learningNotKeptRemedy() string {
	if utils.Version != "" {
		return "run keploy record with the keploy agent of its own version (" + utils.Version + ")"
	}
	return "run keploy record with the keploy agent of its own version"
}

// learningNotKeptReason is why a set is not judged where tests' learning was
// not kept, and they have no verdict for it (tests, sorted; agent as
// learningNotKeptNote's).
func learningNotKeptReason(tests []string, agent string) string {
	return "not judged: " + learningNotKeptNote(tests, agent)
}

// learningNotKeptNote names the tests of a set that have no verdict because
// keploy did not keep their learning (tests, sorted), the agent by the
// version it named (agent; "" where it named none), and what to do.
func learningNotKeptNote(tests []string, agent string) string {
	return "keploy did not keep what its agent learned for " + strconv.Itoa(len(tests)) + " test(s) (" + firstNames(tests, 10) + "), which have no verdict: " + pkg.AgentDidNotSay(agent, "it read their test cases' requests") + "; " + learningNotKeptRemedy()
}

// markLearningNotKept says, on each row of a test notKept names, what of its
// learning was not kept (FailureInfo.LearningNotKept). It returns, sorted,
// the tests a row of which has no verdict for it: a FAILED row keeps its
// verdict, which nothing kept or not kept beside it could have made. agent
// is the version the agent named (learningNotKept).
func markLearningNotKept(results []models.TestResult, notKept map[string][]string, agent string) []string {
	var marked []string
	for i := range results {
		fields, ok := notKept[results[i].TestCaseID]
		if !ok {
			continue
		}
		results[i].FailureInfo.LearningNotKept = learningNotKept(fields, agent)
		if len(results[i].VerdictUnknown()) > 0 {
			marked = appendOnce(marked, results[i].TestCaseID)
		}
	}
	sort.Strings(marked)
	return marked
}

// joinNotes is a, and b after it, "; " between: each a note of tests a set
// with a verdict of its own does not judge (runShape.staleWhy).
func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// byName is the set of m's keys.
func byName(m map[string][]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
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
