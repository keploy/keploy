package replay

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

type reportingHooks struct {
	prHooks
	mu    sync.Mutex
	names []string
	err   error
}

func (h *reportingHooks) AfterTestCaseRun(_ context.Context, _ string, tc *models.TestCase, _ *models.TestResult) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.names = append(h.names, tc.Name)
	return h.err
}

// EVERY recorded verdict is reported, not just the happy path's.
//
// The hook used to be called from the one site that fills
// finalTestCaseResults. A case whose app never answered is persisted by its own
// terminal branch and never reaches that site, so it was never reported --
// measured here: 4 verdicts recorded, 1 reported. k8s-proxy's in-Pod driver is
// the only consumer, and it treats this as the complete result stream: what it
// never hears about it never settles or retires, so those cases were written
// again by salvage and re-staged by the next round.
//
// This drives the real RunTestSet rather than calling the double, which is what
// the other reporter tests do -- a mutation that disables the hook has to fail
// something.
func TestRunTestSetReportsEveryRecordedVerdict(t *testing.T) {
	err := prClientTimeoutErr(t)
	h := newPartialRunHarness(t, 4, 0)
	rep := &reportingHooks{prHooks: prHooks{simErr: map[string]error{"test-2": err, "test-3": err, "test-4": err}}}
	h.replayer.hookImpl = rep
	_ = h.run(t)

	r := h.report.report
	if r == nil {
		t.Fatal("no report")
	}
	recorded := r.Success + r.Failure
	t.Logf("run recorded %d verdicts (success=%d failure=%d); reporter saw %d: %v",
		recorded, r.Success, r.Failure, len(rep.names), rep.names)
	if int(recorded) != len(rep.names) {
		t.Errorf("%d verdicts recorded, reporter saw %d (%v): a consumer that treats this as the "+
			"complete stream never settles the rest", recorded, len(rep.names), rep.names)
	}
	// And exactly once each, with no duplicates from the retry cycles.
	seen := map[string]int{}
	for _, n := range rep.names {
		seen[n]++
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("%s reported %d times, want exactly 1", n, c)
		}
	}
}

// A REPORTER THAT CANNOT REPORT MUST NOT FAIL A RUN WHOSE TESTS PASSED.
//
// The call site logs the error and carries on, which is a property of the
// ENGINE, not of a reporter. The test this replaces asserted only that the test
// double returned the error it was constructed with -- true of any double, and
// green even with the hook call removed.
func TestAFailingReporterDoesNotFailTheRun(t *testing.T) {
	h := newPartialRunHarness(t, 3, 0)
	rep := &reportingHooks{err: errors.New("progress file is on a full disk")}
	h.replayer.hookImpl = rep

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("a failing reporter turned a passing run into %q", status)
	}
	if got := len(h.report.results); got != 3 {
		t.Fatalf("recorded %d results, want 3: a failing reporter cost the run its verdicts", got)
	}
	if len(rep.names) != 3 {
		t.Errorf("the run stopped reporting after the first error: saw %v", rep.names)
	}
}
