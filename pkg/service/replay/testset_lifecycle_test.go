package replay

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A set is begun on the agent once, before any test's filter, with what the
// agent needs to begin it (its first recorded test, every recorded test's
// window), and ended once every test's filter is sent. No test's filter
// stands for the set's begin: none starts at a sentinel time.
func TestRunTestSetBeginsAndEndsTheSetOnTheAgent(t *testing.T) {
	h := newPartialRunHarness(t, 3, 0)
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %q, want PASSED", status)
	}
	h.instr.mu.Lock()
	defer h.instr.mu.Unlock()
	if len(h.instr.begins) != 1 {
		t.Fatalf("the set began %d times, want once", len(h.instr.begins))
	}
	b := h.instr.begins[0]
	if b.TestSetID != "test-set-0" || !b.FirstRecordedTestStart.Equal(h.cases[0].HTTPReq.Timestamp) || len(b.RecordedWindows) != len(h.cases) {
		t.Fatalf("the set began with %+v; want test-set-0, its first recorded test and %d windows", b, len(h.cases))
	}
	if len(h.instr.allParams) != len(h.cases) {
		t.Fatalf("%d test filters for %d tests", len(h.instr.allParams), len(h.cases))
	}
	for _, p := range h.instr.allParams {
		if p.AfterTime.Equal(models.BaseTime) || p.AfterTime.Before(h.cases[0].HTTPReq.Timestamp.Add(-time.Hour)) {
			t.Fatalf("a test's filter starts at %v, before any recorded test", p.AfterTime)
		}
	}
	if len(h.instr.ends) != 1 || h.instr.ends[0] != "test-set-0" {
		t.Fatalf("ended %v, want test-set-0 once", h.instr.ends)
	}
}
