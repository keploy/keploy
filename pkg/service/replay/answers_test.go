package replay

import (
	"slices"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// S12 v16 §3.3: an answered call (models.UnmatchedCall.Answer) the agent
// reports with the misses is split from them at intake. A test's goes to its
// TestResult.Answers, the set's (made before its first test or after its
// last) to its report's SetAnswers and to SetAnswersHook; none lands in a
// miss list, and none changes a status.

// answered is a call the agent answered as how says, made in a test or, with
// beforeSet, before the set's first test.
func answered(summary, how string, beforeSet bool) models.UnmatchedCall {
	c := models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: summary, Destination: "mongo:27017",
		Answer: how, WriteKind: "Mongo insert keploy.events", Kind: models.MissInTest,
		Source: &models.Origin{Comm: "sqs", Exe: "./sqs"}}
	if beforeSet {
		c.BeforeSet, c.Kind = true, models.MissOutsideTest
	}
	if how == models.AnswerProvisional {
		c.ClosestMock, c.NotOwned, c.RecordedIn, c.Drift = "mock-6", true, "test-2", true
	}
	return c
}

func reads(calls ...models.UnmatchedCall) func() ([]models.UnmatchedCall, error) {
	return func() ([]models.UnmatchedCall, error) { return calls, nil }
}

// setAnswersRecorder is the run's hooks with SetAnswersHook.
type setAnswersRecorder struct {
	TestHooks
	mu   sync.Mutex
	sets []string
	got  [][]models.UnmatchedCall
}

func (h *setAnswersRecorder) SetAnswers(testSetID string, answers []models.UnmatchedCall) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sets = append(h.sets, testSetID)
	h.got = append(h.got, slices.Clone(answers))
}

func TestRunTestSet_AnswersNeverFailATestOrTheSet(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	before := answered("insert before", models.AnswerByKind, true)
	inFirst := answered("insert in test-1", models.AnswerByKind, false)
	paired := answered("insert in test-2", models.AnswerProvisional, false)
	after := answered("insert after", models.AnswerByKind, false)
	h.instr.mockErrorsFor = misses(reads(before, inFirst), reads(paired), reads(after))

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s (reason %q); answered calls fail nothing", status, h.report.report.FailureReason)
	}
	rep := h.report.report
	want := map[string][]models.UnmatchedCall{"test-1": {inFirst}, "test-2": {paired}}
	for _, r := range rep.Tests {
		if r.Status != models.TestStatusPassed {
			t.Errorf("%s is %s; a test whose only calls were answered passes", r.TestCaseID, r.Status)
		}
		if len(r.FailureInfo.UnmatchedCalls) != 0 {
			t.Errorf("%s's misses hold its answers: %+v", r.TestCaseID, r.FailureInfo.UnmatchedCalls)
		}
		if got := r.Answers; len(got) != 1 || got[0].ActualSummary != want[r.TestCaseID][0].ActualSummary || got[0].Answer != want[r.TestCaseID][0].Answer {
			t.Errorf("%s's answers are %+v; want %+v", r.TestCaseID, got, want[r.TestCaseID])
		}
	}
	if len(rep.UnclaimedCalls) != 0 {
		t.Errorf("the set's unclaimed calls hold its answers: %+v", rep.UnclaimedCalls)
	}
	if got := rep.SetAnswers; len(got) != 2 || got[0].ActualSummary != "insert before" || got[1].ActualSummary != "insert after" {
		t.Errorf("the set's answers are %+v; want the one before its first test, then the one after its last", got)
	}
	if failures := h.replayer.GetMockMismatchFailures(); len(failures) != 0 {
		t.Errorf("the mismatch report lists answered calls: %+v", failures)
	}
}

// A miss read with an answer is still a miss: it fails its test.
func TestRunTestSet_AMissBesideAnAnswerStillFailsItsTest(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	miss := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /pay", Destination: "pay:80", Kind: models.MissInTest}
	h.instr.mockErrorsFor = misses(reads(answered("insert", models.AnswerByKind, false), miss))

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s; the miss fails its test, and the set", status)
	}
	for _, r := range h.report.report.Tests {
		if r.TestCaseID != "test-1" {
			continue
		}
		if r.Status != models.TestStatusFailed || len(r.FailureInfo.UnmatchedCalls) != 1 || r.FailureInfo.UnmatchedCalls[0].ActualSummary != "POST /pay" {
			t.Fatalf("test-1 is %s with misses %+v; want FAILED on POST /pay alone", r.Status, r.FailureInfo.UnmatchedCalls)
		}
		if len(r.Answers) != 1 {
			t.Fatalf("test-1's answers are %+v; want the insert", r.Answers)
		}
	}
}

// The set's answers its end noted reach SetAnswersHook as the set's end is
// told, under the switch only.
func TestSetAnswersAreToldToTheHooks(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 1, 0)
	rec := &setAnswersRecorder{TestHooks: h.replayer.GetTestHooks()}
	h.replayer.SetTestHooks(rec)
	before := answered("insert before", models.AnswerByKind, true)
	h.instr.mockErrorsFor = misses(reads(before))

	status := h.run(t)
	h.replayer.tellSetEnd("test-set-0", status, nil)
	if len(rec.sets) != 1 || rec.sets[0] != "test-set-0" || len(rec.got[0]) != 1 || rec.got[0][0].ActualSummary != "insert before" {
		t.Fatalf("SetAnswersHook was told %v %+v; want test-set-0's answer, once", rec.sets, rec.got)
	}
	// Without the switch the hook is not told: there are no answers.
	provenOn(t, false)
	h.replayer.tellSetEnd("test-set-0", status, nil)
	if len(rec.sets) != 1 {
		t.Fatalf("SetAnswersHook was told %d times; without the switch it is not told", len(rec.sets))
	}
}

func TestSplitAnswersKeepsReadOrder(t *testing.T) {
	m1, m2 := models.UnmatchedCall{ActualSummary: "m1"}, models.UnmatchedCall{ActualSummary: "m2"}
	a1, a2 := answered("a1", models.AnswerByKind, false), answered("a2", models.AnswerProvisional, false)
	read := []models.UnmatchedCall{a1, m1, a2, m2}
	gotMisses, gotAnswers := splitAnswers(read)
	if len(gotMisses) != 2 || gotMisses[0].ActualSummary != "m1" || gotMisses[1].ActualSummary != "m2" {
		t.Fatalf("misses %+v; want m1, m2", gotMisses)
	}
	if len(gotAnswers) != 2 || gotAnswers[0].ActualSummary != "a1" || gotAnswers[1].ActualSummary != "a2" {
		t.Fatalf("answers %+v; want a1, a2", gotAnswers)
	}
	only := []models.UnmatchedCall{m1, m2}
	if gotMisses, gotAnswers := splitAnswers(only); len(gotAnswers) != 0 || &gotMisses[0] != &only[0] {
		t.Fatal("a read with no answer is not returned as it is")
	}
}
