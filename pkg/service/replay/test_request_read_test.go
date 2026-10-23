package replay

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// readsInstr is the harness's agent, which answers every filter that opens a
// test case saying its learn step read the request the filter carried, or
// not (pkg.TestCaseFilterSender), as reads says while the set runs, and
// names version as its own (pkg.AgentVersionReader; "" for none).
type readsInstr struct {
	*prInstr
	reads   bool
	version string
}

func (a *readsInstr) AgentVersion() string { return a.version }

func (a *readsInstr) SendTestCaseFilter(ctx context.Context, p models.MockFilterParams) (bool, error) {
	if err := a.UpdateMockParams(ctx, p); err != nil {
		return false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads, nil
}

// What an agent learned on the mocks a test consumed is kept only where the
// agent said it read the test case's request the opening filter carried: an
// agent older than the replay ignores the request, and a value the
// application took from it may have been learned as noise, an echo, or a
// variable whose bind takes any value. None of it is kept otherwise, and no
// test of the set passes on it: each test that consumed a mock whose
// learning was not kept has no verdict, saying on its row in the set's report
// what was not kept and why, the run counts it among the tests with no
// verdict, and the set is not judged. An instrumentation that cannot say is
// an agent that did not. The reasons name the agent by the version it named,
// or as one that named none, and this keploy by its own.
func TestRunTestSet_TheFullRoundKeepsNoNoiseAnAgentLearnedWithoutTheRequest(t *testing.T) {
	own := utils.Version
	utils.Version = "v3.7.0"
	t.Cleanup(func() { utils.Version = own })
	const named, unnamed = "the agent (keploy v3.6.100) did not say it read ", "the agent did not say it read "
	learned := models.MockState{Name: "mock-1", Kind: models.HTTP, ReqBodyNoise: map[string][]string{"body.at": {}},
		Variables: models.MockVariables{"echo_1": {Recorded: "r-1", Bind: "body.rid", Echo: true}, "id_1": {Recorded: "u-1", Use: []string{"body.id"}}},
		Binds:     map[string]models.MockVariables{"mock-0": {"id_1": {Recorded: "u-1", Bind: "body.id"}}}}
	for _, tc := range []struct {
		name    string
		reads   *bool
		version string
		who     string
	}{
		{"an agent that read it", ptr(true), "v3.7.0", ""},
		{"an agent that did not", ptr(false), "v3.6.100", named},
		{"an agent that did not, nor named its version", ptr(false), "", unnamed},
		{"an instrumentation that cannot say", nil, "", unnamed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nor := ""
			if tc.version == "" {
				nor = ", nor its version"
			}
			provenOn(t, true)
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Test.Learn = true
			store := &learnedStore{prMockDB: h.mocks}
			h.replayer.mockDB = store
			if tc.reads != nil {
				h.replayer.instrumentation = &readsInstr{prInstr: h.instr, reads: *tc.reads, version: tc.version}
			}
			h.replayer.hookImpl = prHooks{consumed: []models.MockState{learned}}
			status := h.run(t)
			got, ok := store.persisted["mock-1"]
			if !ok {
				t.Fatalf("persisted %v lacks mock-1", store.persisted)
			}
			if h.report.report == nil || len(h.report.report.Tests) != 2 {
				t.Fatalf("report %+v; want the set's report with its 2 tests", h.report.report)
			}
			if tc.reads != nil && *tc.reads {
				if status != models.TestSetStatusPassed {
					t.Fatalf("status %s, want PASSED", status)
				}
				if !reflect.DeepEqual(got.ReqBodyNoise, learned.ReqBodyNoise) || !reflect.DeepEqual(got.Variables, learned.Variables) || !reflect.DeepEqual(got.Binds, learned.Binds) {
					t.Fatalf("kept %+v; want what was learned", got)
				}
				for _, res := range h.report.report.Tests {
					if res.FailureInfo.LearningNotKept != "" || len(res.VerdictUnknown()) > 0 {
						t.Fatalf("%s: %q; want its learning kept, and its verdict", res.TestCaseID, res.FailureInfo.LearningNotKept)
					}
				}
				if nv := h.replayer.noVerdicts(); len(nv) != 0 {
					t.Fatalf("tests with no verdict %v; want none", nv)
				}
				return
			}
			if status != models.TestSetStatusInternalErr {
				t.Fatalf("status %s; want the set not judged (INTERNAL_ERR)", status)
			}
			if got.ReqBodyNoise != nil || got.Variables != nil || got.Binds != nil {
				t.Fatalf("kept %+v; want nothing the agent learned", got)
			}
			if len(learned.Variables) != 2 || len(learned.ReqBodyNoise) != 1 || len(learned.Binds) != 1 {
				t.Fatal("the consumed state itself was changed")
			}
			for _, res := range h.report.report.Tests {
				why := res.FailureInfo.LearningNotKept
				if !strings.Contains(why, "mock-1 body.at (noise), mock-1 echo_1 (echo), mock-1 id_1 (bound at mock-0), mock-1 id_1 (variable)") ||
					!strings.Contains(why, tc.who+"the test case's request"+nor+", as an agent older than this keploy (v3.7.0) does not") || !strings.Contains(why, "run keploy record with the keploy agent of its own version (v3.7.0)") {
					t.Fatalf("%s: learning not kept %q", res.TestCaseID, why)
				}
				if res.Status != models.TestStatusPassed || len(res.VerdictUnknown()) == 0 {
					t.Fatalf("%s: %s, verdict unknown %v; want a passed row with no verdict", res.TestCaseID, res.Status, res.VerdictUnknown())
				}
			}
			if got, want := fmt.Sprint(sortedNoVerdicts(h.replayer)), "map[test-set-0:[test-1 test-2]]"; got != want {
				t.Fatalf("the run's tests with no verdict %s; want %s", got, want)
			}
			if !strings.Contains(h.report.report.FailureReason, "not judged: keploy did not keep what its agent learned for 2 test(s) (test-1, test-2), which have no verdict: "+tc.who+"their test cases' requests"+nor+", as an agent older than this keploy (v3.7.0) does not") {
				t.Fatalf("report reason %q; want the set not judged, naming the tests", h.report.report.FailureReason)
			}
		})
	}
}

// A set with a verdict of its own keeps it where tests' learning was not
// kept: a test that failed failed whatever was learned beside it, and keeps
// its verdict; the one that passed has none, and the set's reason names it.
func TestRunTestSet_AFailedSetKeepsItsVerdictBesideLearningNotKept(t *testing.T) {
	provenOn(t, true)
	learned := models.MockState{Name: "mock-1", Kind: models.HTTP, ReqBodyNoise: map[string][]string{"body.at": {}}}
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Test.Learn = true
	store := &learnedStore{prMockDB: h.mocks}
	h.replayer.mockDB = store
	h.replayer.instrumentation = &readsInstr{prInstr: h.instr, reads: false}
	h.replayer.hookImpl = prHooks{consumed: []models.MockState{learned}, wrongBody: map[string]bool{"test-1": true}}
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s; want FAILED, the set's own verdict", status)
	}
	if h.report.report == nil || len(h.report.report.Tests) != 2 {
		t.Fatalf("report %+v; want the set's report with its 2 tests", h.report.report)
	}
	for _, res := range h.report.report.Tests {
		if res.FailureInfo.LearningNotKept == "" {
			t.Fatalf("%s: learning not kept unsaid", res.TestCaseID)
		}
		switch res.TestCaseID {
		case "test-1":
			if res.Status != models.TestStatusFailed || len(res.VerdictUnknown()) != 0 {
				t.Fatalf("test-1: %s, verdict unknown %v; want FAILED, its verdict kept", res.Status, res.VerdictUnknown())
			}
		case "test-2":
			if res.Status != models.TestStatusPassed || len(res.VerdictUnknown()) == 0 {
				t.Fatalf("test-2: %s, verdict unknown %v; want a passed row with no verdict", res.Status, res.VerdictUnknown())
			}
		}
	}
	if got, want := fmt.Sprint(sortedNoVerdicts(h.replayer)), "map[test-set-0:[test-2]]"; got != want {
		t.Fatalf("the run's tests with no verdict %s; want %s", got, want)
	}
	if why := h.report.report.FailureReason; !strings.Contains(why, "keploy did not keep what its agent learned for 1 test(s) (test-2), which have no verdict") || strings.Contains(why, "not judged: keploy did not keep") {
		t.Fatalf("report reason %q; want the set's own verdict, naming test-2", why)
	}
}

// What a set's tests' openings said is that set's: a test of the same name in
// the next set (test-set-1 after test-set-0), whose agent read its request,
// keeps what was learned for it, and has its verdict.
func TestRunTestSet_ARequestUnreadInOneSetIsNotTheNextSets(t *testing.T) {
	provenOn(t, true)
	learned := models.MockState{Name: "mock-1", Kind: models.HTTP, ReqBodyNoise: map[string][]string{"body.at": {}}}
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Test.Learn = true
	store := &learnedStore{prMockDB: h.mocks}
	h.replayer.mockDB = store
	instr := &readsInstr{prInstr: h.instr, reads: false}
	h.replayer.instrumentation = instr
	h.replayer.hookImpl = prHooks{consumed: []models.MockState{learned}}
	if status := h.run(t); status != models.TestSetStatusInternalErr {
		t.Fatalf("the first set, its agent reading no request: %s; want INTERNAL_ERR", status)
	}
	instr.mu.Lock()
	instr.reads = true
	instr.mu.Unlock()
	h.report.ClearTestCaseResults(context.Background(), "", "")
	store.persisted = nil
	h.set = "test-set-1"
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("the next set, test-set-1, its agent reading every request: %s; want PASSED", status)
	}
	if got := store.persisted["mock-1"]; !reflect.DeepEqual(got.ReqBodyNoise, learned.ReqBodyNoise) {
		t.Fatalf("the next set kept %+v; want what was learned", got)
	}
}

func ptr[T any](v T) *T { return &v }

// sortedNoVerdicts is the run's tests with no verdict (Replayer.noVerdicts),
// each set's sorted: the order the results were read in is not the rows'.
func sortedNoVerdicts(r *Replayer) map[string][]string {
	out := r.noVerdicts()
	for set := range out {
		sort.Strings(out[set])
	}
	return out
}

// withoutUnreadLearning takes everything learned off the mocks a test whose
// request was not read consumed, and nothing off the others, and says what
// it took, sorted, as one text for one input.
func TestWithoutUnreadLearning(t *testing.T) {
	consumed := map[string]models.MockState{
		"mock-1": {Name: "mock-1", ReqBodyNoise: map[string][]string{"body.b": {}, "body.a": {}}, Variables: models.MockVariables{"e": {Echo: true}, "v": {Use: []string{"body.v"}}},
			Binds: map[string]models.MockVariables{"mock-0": {"v": {Bind: "body.v"}}}},
		"mock-2": {Name: "mock-2", ReqBodyNoise: map[string][]string{"body.c": {}}},
		"boot":   {Name: "boot", ReqBodyNoise: map[string][]string{"body.d": {}}},
	}
	consumers := map[string][]string{"mock-1": {"test-1", "test-2"}, "mock-2": {"test-2"}}
	for i := 0; i < 50; i++ {
		kept, dropped, byTest := withoutUnreadLearning(consumed, consumers, map[string]bool{"test-1": true})
		want := "mock-1 body.a (noise),mock-1 body.b (noise),mock-1 e (echo),mock-1 v (bound at mock-0),mock-1 v (variable)"
		if strings.Join(dropped, ",") != want {
			t.Fatalf("dropped %v; want %s", dropped, want)
		}
		// Every test that consumed the mock leaned on it, test-2 too,
		// whose own request was read.
		if len(byTest) != 2 || strings.Join(byTest["test-1"], ",") != want || strings.Join(byTest["test-2"], ",") != want {
			t.Fatalf("by test %v; want test-1 and test-2, each %s", byTest, want)
		}
		if kept["mock-1"].ReqBodyNoise != nil || kept["mock-1"].Variables != nil || kept["mock-1"].Binds != nil || len(kept["mock-2"].ReqBodyNoise) != 1 || len(kept["boot"].ReqBodyNoise) != 1 {
			t.Fatalf("kept %+v", kept)
		}
	}
	if len(consumed["mock-1"].ReqBodyNoise) != 2 {
		t.Fatal("the consumed states were changed")
	}
	if kept, dropped, byTest := withoutUnreadLearning(consumed, consumers, nil); dropped != nil || byTest != nil || !reflect.DeepEqual(kept, consumed) {
		t.Fatalf("with every request read: kept %v, dropped %v, by test %v", kept, dropped, byTest)
	}
	// A mock the unread test consumed that learned nothing marks no test.
	if _, dropped, byTest := withoutUnreadLearning(map[string]models.MockState{"mock-3": {Name: "mock-3"}}, map[string][]string{"mock-3": {"test-1"}}, map[string]bool{"test-1": true}); dropped != nil || byTest != nil {
		t.Fatalf("nothing learned: dropped %v, by test %v", dropped, byTest)
	}
}
