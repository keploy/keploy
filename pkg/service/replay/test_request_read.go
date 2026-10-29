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
// well, served it as the pool's recording a matcher filed a copy of over the
// one the staging put there: from names, by each such test, the tests whose
// requests were not read that consumed its mocks, sorted). A variable is no
// safer than noise there: the place it is bound at
// takes any value its replay sends (a pick from the request bound at its
// first call, and only asserted equal at the next), so nothing an agent
// learned without the request is kept. consumed is not changed: the result
// holds copies of the states it changes.
func withoutUnreadLearning(consumed map[string]models.MockState, consumers map[string][]string, unread map[string]bool) (out map[string]models.MockState, dropped []string, byTest, from map[string][]string) {
	if len(unread) == 0 {
		return consumed, nil, nil, nil
	}
	out = make(map[string]models.MockState, len(consumed))
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
		for field, o := range st.Clocks {
			mock = append(mock, name+" "+field+" ("+o.String()+")")
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
			for _, test := range consumers[name] {
				for _, u := range consumers[name] {
					if !unread[test] && unread[u] {
						if from == nil {
							from = map[string][]string{}
						}
						from[test] = appendOnce(from[test], u)
					}
				}
			}
		}
		st.ReqBodyNoise, st.Variables, st.Binds, st.Clocks = nil, nil, nil, nil
		out[name] = st
	}
	sort.Strings(dropped)
	for test := range byTest {
		sort.Strings(byTest[test])
	}
	for test := range from {
		sort.Strings(from[test])
	}
	return out, dropped, byTest, from
}

// readLearning is consumed, what record's full round learned in testSetID,
// without what the agent learned for a test whose request it did not say it
// read (withoutUnreadLearning), and, by test, what it did not keep of what
// each test consumed (notKept), which it says, naming the tests and the
// fields: each such test has no verdict (learningNotKept). from names, by
// each of those tests whose own request was read, the tests whose requests
// were not read that consumed its mocks (withoutUnreadLearning).
func (r *Replayer) readLearning(testSetID string, consumed map[string]models.MockState, consumers map[string][]string) (kept map[string]models.MockState, notKept, from map[string][]string) {
	unread := r.requestsUnread.set()
	if unread == nil {
		return consumed, nil, nil
	}
	kept, dropped, notKept, from := withoutUnreadLearning(consumed, consumers, unread)
	if len(dropped) > 0 {
		r.logger.Error("keploy did not keep what its agent learned on the mocks these tests consumed, and the tests have no verdict: the agent did not say it read their test cases' requests, as an agent older than this keploy does not, so a value the application took from a request may have been learned as noise, or as a variable that takes any value; the recording keeps comparing those fields",
			zap.String("testSetID", testSetID),
			zap.Strings("tests", sortedTests(byName(notKept))),
			zap.Strings("notKept", dropped),
			zap.String("agent", pkg.AgentDidNotSay(r.requestsUnread.agentVersion(), "it read their test cases' requests")),
			zap.String("next_step", learningNotKeptRemedy()))
	}
	return kept, notKept, from
}

// keptLearning is what of consumed readLearning keeps, said nowhere: the
// unresolved shares stripped (exactPlan.stripUnresolved), and what the agent
// learned for a test whose request it did not say it read
// (withoutUnreadLearning). The set's retry cycles are served it
// (serveFirstCycleLearned) before readLearning says what is not kept.
func (r *Replayer) keptLearning(consumed map[string]models.MockState, consumers map[string][]string) map[string]models.MockState {
	kept := r.exact.stripUnresolved(consumed)
	if unread := r.requestsUnread.set(); unread != nil {
		kept, _, _, _ = withoutUnreadLearning(kept, consumers, unread)
	}
	return kept
}

// learningNotKept is why a test's learning was not kept, naming what, and
// the agent by the version it named (agent; "" where it named none)
// (FailureInfo.LearningNotKept). from are the tests whose requests the agent
// did not say it read that consumed those mocks too, for a test whose own
// request it read (withoutUnreadLearning); none for one whose it did not.
func learningNotKept(fields, from []string, agent string) string {
	if len(from) > 0 {
		return "keploy did not keep what its agent learned on the mocks this test consumed (" + firstNames(fields, 10) + "), which " + strconv.Itoa(len(from)) + " other test case(s) consumed too (" + firstNames(from, 10) + "): " + pkg.AgentDidNotSay(agent, "it read their requests") + ", so a value the application took from one of them may have been learned as noise, or as a variable that takes any value, and this test's run leaned on it; " + learningNotKeptRemedy()
	}
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
// verdict, which nothing kept or not kept beside it could have made. from
// and agent are learningNotKept's: by test, the tests whose requests were
// not read that consumed its mocks, and the version the agent named.
func markLearningNotKept(results []models.TestResult, notKept, from map[string][]string, agent string) []string {
	var marked []string
	for i := range results {
		fields, ok := notKept[results[i].TestCaseID]
		if !ok {
			continue
		}
		results[i].FailureInfo.LearningNotKept = learningNotKept(fields, from[results[i].TestCaseID], agent)
		if len(results[i].VerdictUnknown()) > 0 {
			marked = appendOnce(marked, results[i].TestCaseID)
		}
	}
	sort.Strings(marked)
	return marked
}

// noteLearnedAfterEnd records, by test, the mocks of consumed (the test's
// drain) a parser learned on after the set's learn step ended
// (MockState.LearnedAfterEnd): in a retry cycle, or in the streaming pass
// after the set's first run, where keploy keeps nothing learned.
func noteLearnedAfterEnd(late map[string][]string, test string, consumed []models.MockState) {
	for _, m := range consumed {
		if m.LearnedAfterEnd {
			late[test] = appendOnce(late[test], m.Name)
		}
	}
}

// learnedAfterEnd is FailureInfo.LearningNotKept of a test that consumed
// mocks (sorted) a parser learned on after the set's learn step ended.
func learnedAfterEnd(mocks []string) string {
	return "a parser learned on the mocks this test consumed (" + firstNames(mocks, 10) + ") after the test set's learn step ended (in a retry cycle, or in the streaming pass after the set's first run), and keploy keeps nothing learned then: this test's run leaned on that learning, so it has no verdict; " + learnedAfterEndRemedy
}

// learnedAfterEndRemedy is what to do about a test learnedAfterEnd marks.
const learnedAfterEndRemedy = "run keploy record without --retry-passing-test (retryPassing in keploy.yml), and record each streaming test case after the set's other test cases or in a test set of its own, so the test is judged where its learning is kept"

// learnedAfterEndNote names the tests of a set that have no verdict because
// a parser learned on mocks they consumed after the set's learn step ended
// (tests, sorted), and what to do.
func learnedAfterEndNote(tests []string) string {
	return "a parser learned after the test set's learn step ended on mocks " + strconv.Itoa(len(tests)) + " test(s) consumed (" + firstNames(tests, 10) + "), which have no verdict: keploy keeps nothing learned then; " + learnedAfterEndRemedy
}

// markLearnedAfterEnd says, on each row of a test late names, that a parser
// learned on mocks it consumed after the set's learn step ended
// (learnedAfterEnd, in FailureInfo.LearningNotKept, after what is there).
// It returns, sorted, the tests a row of which has no verdict for it: a
// FAILED row keeps its verdict, which no learning could have made.
func markLearnedAfterEnd(results []models.TestResult, late map[string][]string) []string {
	var marked []string
	for i := range results {
		mocks, ok := late[results[i].TestCaseID]
		if !ok || len(mocks) == 0 {
			continue
		}
		sorted := append([]string(nil), mocks...)
		sort.Strings(sorted)
		results[i].FailureInfo.LearningNotKept = joinNotes(results[i].FailureInfo.LearningNotKept, learnedAfterEnd(sorted))
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
