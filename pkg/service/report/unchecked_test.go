package report

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// byKindAnswer is a background write the agent answered by its kind inside
// the test's window (models.AnswerByKind).
func byKindAnswer() models.UnmatchedCall {
	return models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "insert keploy.events", Answer: models.AnswerByKind,
		WriteKind: "Mongo insert keploy.events", Kind: models.MissInTest}
}

const uncheckedWant = "  post-url-1: 1 background write answered inside its window, unchecked (Mongo insert keploy.events)\n"

// The text report says, under a test's status, the background writes its
// window answered by their kind, unchecked, whatever the
// test's status; a provisional answer (a pairing) says nothing, and a test
// with no answers prints what it printed before answers existed.
func TestPrintTests_SaysATestsUncheckedWrites(t *testing.T) {
	render := func(tests ...models.TestResult) string {
		r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		if err := r.printTests(context.Background(), tests); err != nil {
			t.Fatalf("printTests: %v", err)
		}
		return buf.String()
	}
	passed := models.TestResult{TestCaseID: "post-url-1", Name: "test-set-0", Status: models.TestStatusPassed}
	bare := render(passed)
	if strings.Contains(bare, "unchecked") {
		t.Fatalf("a test with no answers has an unchecked line:\n%s", bare)
	}

	answered := passed
	answered.Answers = []models.UnmatchedCall{byKindAnswer()}
	got := render(answered)
	if strings.Count(got, uncheckedWant) != 1 {
		t.Fatalf("the report does not say the unchecked write once under the status:\n%s", got)
	}
	if status := strings.Index(got, "PASSED"); status < 0 || status > strings.Index(got, uncheckedWant) {
		t.Fatalf("the unchecked line is not under the test's status:\n%s", got)
	}
	// Without it, the bytes are the bare test's.
	if strings.Replace(got, uncheckedWant, "", 1) != bare {
		t.Fatalf("the line changed more than itself:\n%s\nvs\n%s", got, bare)
	}

	provisional := passed
	provisional.Answers = []models.UnmatchedCall{{Answer: models.AnswerProvisional, WriteKind: "Mongo insert keploy.events", Kind: models.MissInTest}}
	if got := render(provisional); got != bare {
		t.Fatalf("a provisional answer changed the report:\n%s", got)
	}
}

// A failed test's block says its unchecked writes too, after its misses.
func TestRenderSingleFailedTest_SaysATestsUncheckedWrites(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	failed := models.TestResult{TestCaseID: "post-url-1", Name: "test-set-0", Status: models.TestStatusFailed,
		Answers:     []models.UnmatchedCall{byKindAnswer()},
		FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /pay", Kind: models.MissInTest}}}}
	var sb strings.Builder
	if err := r.renderSingleFailedTest(context.Background(), &sb, failed); err != nil {
		t.Fatalf("renderSingleFailedTest: %v", err)
	}
	out := sb.String()
	miss := strings.Index(out, "POST /pay")
	line := strings.Index(out, uncheckedWant)
	if miss < 0 || line < 0 || line < miss || strings.Count(out, uncheckedWant) != 1 {
		t.Fatalf("the failed test's block does not say its unchecked write once, after its misses:\n%s", out)
	}
	failed.Answers = nil
	sb.Reset()
	if err := r.renderSingleFailedTest(context.Background(), &sb, failed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "unchecked") {
		t.Fatalf("a failed test with no answers has an unchecked line:\n%s", sb.String())
	}
}

// The text report names, under a test's status, each read the agent
// answered from before a write the replay had not made yet, after the
// test's unchecked writes, and changes nothing else.
func TestPrintTests_NamesAReadAnsweredFromBeforeAWrite(t *testing.T) {
	render := func(tests ...models.TestResult) string {
		r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
		var buf bytes.Buffer
		r.out = bufio.NewWriter(&buf)
		if err := r.printTests(context.Background(), tests); err != nil {
			t.Fatalf("printTests: %v", err)
		}
		return buf.String()
	}
	passed := models.TestResult{TestCaseID: "post-url-1", Name: "test-set-0", Status: models.TestStatusPassed}
	bare := render(passed)
	answered := passed
	answered.Answers = []models.UnmatchedCall{
		{Protocol: "MongoDB", ActualSummary: "find oauth.blocklist", Answer: models.AnswerBeforeUnmadeWrite, ClosestMock: "mock-3",
			NextSteps: "an extra read before mock-7, answered from mock-3", Kind: models.MissInTest},
		byKindAnswer(),
	}
	const want = "  post-url-1: MongoDB find oauth.blocklist: an extra read before mock-7, answered from mock-3\n"
	got := render(answered)
	if strings.Count(got, want) != 1 || strings.Index(got, want) < strings.Index(got, uncheckedWant) {
		t.Fatalf("the report does not name the read once, after the unchecked writes:\n%s", got)
	}
	if strings.Replace(strings.Replace(got, want, "", 1), uncheckedWant, "", 1) != bare {
		t.Fatalf("the lines changed more than themselves:\n%s\nvs\n%s", got, bare)
	}
}
