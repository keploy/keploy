package replay

import (
	"context"
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

// The two tests that used to sit here -- one asserting the double recorded what
// it was handed, one asserting the double returned its own error -- exercised
// only the double: a mutation disabling the hook call in RunTestSet left both
// green. What they claimed to pin is now pinned against the real engine, in
// testcase_reporter_runtestset_test.go:
//
//	TestRunTestSetReportsEveryRecordedVerdict   what is handed over, and once each
//	TestAFailingReporterDoesNotFailTheRun       the "logs and carries on" contract
//
// TestTheReporterIsOptional stays here because it is about the TYPE: no engine
// can tell you whether an implementation without AfterTestCaseRun satisfies the
// interface.
