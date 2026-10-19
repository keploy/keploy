package replay

import (
	"context"
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// reporterHooks implements the optional TestCaseReporter and records what it
// was handed.
type reporterHooks struct {
	TestHooks
	calls []string
	err   error
}

func (h *reporterHooks) AfterTestCaseRun(_ context.Context, testSetID string, tc *models.TestCase, result *models.TestResult) error {
	name := ""
	if tc != nil {
		name = tc.Name
	}
	status := ""
	if result != nil {
		status = string(result.Status)
	}
	h.calls = append(h.calls, testSetID+"/"+name+"="+status)
	return h.err
}

func TestTheReporterIsOptional(t *testing.T) {
	// plainHooks (test_set_orderer_test.go) implements TestHooks and NOT this
	// extension: the assertion must miss it, which is what lets every existing
	// implementation stay untouched.
	if _, ok := interface{}(&plainHooks{}).(TestCaseReporter); ok {
		t.Fatal("a hooks implementation without AfterTestCaseRun satisfies TestCaseReporter; the extension is not optional")
	}
	if _, ok := interface{}(&reporterHooks{}).(TestCaseReporter); !ok {
		t.Fatal("a hooks implementation WITH AfterTestCaseRun does not satisfy TestCaseReporter")
	}
}

// The signature carries what a consumer needs to report a case without going
// back to the engine: which set, which case, and what the verdict was.
func TestTheReporterIsHandedTheSettledVerdict(t *testing.T) {
	h := &reporterHooks{}
	var r TestCaseReporter = h
	err := r.AfterTestCaseRun(context.Background(), "set-1",
		&models.TestCase{Name: "get-health-1"},
		&models.TestResult{Status: models.TestStatusPassed})
	if err != nil {
		t.Fatalf("AfterTestCaseRun: %v", err)
	}
	if len(h.calls) != 1 || h.calls[0] != "set-1/get-health-1=PASSED" {
		t.Fatalf("reporter saw %v, want [set-1/get-health-1=PASSED]", h.calls)
	}
}

// A reporter that cannot report must not fail a run whose tests passed: the
// call site logs and carries on, so returning an error is survivable by
// contract, not by accident.
func TestAFailingReporterStillReturnsItsError(t *testing.T) {
	want := errors.New("progress file is on a full disk")
	h := &reporterHooks{err: want}
	var r TestCaseReporter = h
	if got := r.AfterTestCaseRun(context.Background(), "set-1", &models.TestCase{Name: "a"}, &models.TestResult{}); !errors.Is(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(h.calls) != 1 {
		t.Fatalf("the reporter was called %d times, want 1", len(h.calls))
	}
}
