package replay

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// consumedRun runs a set whose tests each consume five session mocks of their
// own, while the set's three per-test mocks are consumed by the first test. It
// returns every filter-param send and every state of the CLI's consumed history
// a send could have carried.
func consumedRun(t *testing.T, tests int, perTestScope bool) ([]models.MockFilterParams, []string) {
	t.Helper()
	h := newPartialRunHarness(t, tests, 0)
	h.instr.perTestScope = perTestScope
	for i := 0; i < 3; i++ {
		h.mocks.filtered = append(h.mocks.filtered, &models.Mock{Name: fmt.Sprintf("per-test-%d", i)})
	}

	var mu sync.Mutex
	call := 0
	history := map[string]models.MockState{}
	snapshots := []string{"null"} // before anything was consumed
	h.replayer.hookImpl = prHooks{consumedFor: func() []models.MockState {
		mu.Lock()
		defer mu.Unlock()
		call++
		var out []models.MockState
		for j := 0; j < 5; j++ {
			name := fmt.Sprintf("session-%d-%d", call, j)
			out = append(out, models.MockState{Name: name, Usage: models.Updated, Lifetime: models.LifetimeSession, Type: "mocks"})
		}
		if call == 2 { // the first test (call 1 drains the setup)
			for i := 0; i < 3; i++ {
				name := fmt.Sprintf("per-test-%d", i)
				out = append(out, models.MockState{Name: name, Usage: models.Deleted, Lifetime: models.LifetimePerTest, Type: "mocks"})
			}
		}
		for _, st := range out {
			history[st.Name] = st
		}
		b, err := json.Marshal(history)
		if err != nil {
			t.Error(err)
		}
		snapshots = append(snapshots, string(b))
		return out
	}}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("precondition: the set reported %q; want PASSED", status)
	}
	h.instr.mu.Lock()
	defer h.instr.mu.Unlock()
	if len(h.instr.allParams) < tests {
		t.Fatalf("precondition: %d filter-param sends for %d tests", len(h.instr.allParams), tests)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]models.MockFilterParams(nil), h.instr.allParams...), append([]string(nil), snapshots...)
}

// An agent that has not said how it reads the consumed history gets all of it
// with every send, exactly as before: agents from v3.0.0-beta1 through v3.3.22
// also applied it to the session pool.
func TestRunTestSetSendsTheWholeConsumedHistoryToAnAgentThatDoesNotSayItsScope(t *testing.T) {
	const tests = 12
	sends, snapshots := consumedRun(t, tests, false)
	seen := map[string]bool{}
	for _, s := range snapshots {
		seen[s] = true
	}
	for i, p := range sends {
		b, err := json.Marshal(p.TotalConsumedMocks)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "{}" && !seen[string(b)] {
			t.Fatalf("send %d carried %d entries that are not the CLI's whole history at any point", i, len(p.TotalConsumedMocks))
		}
	}
	if last := sends[len(sends)-1]; len(last.TotalConsumedMocks) < 5*(tests-1) {
		t.Fatalf("the last send carried %d entries; the whole history by then holds at least %d", len(last.TotalConsumedMocks), 5*(tests-1))
	}
}

// TestRunTestSetConsumedPayloadDoesNotGrowWithTheTestIndex: once the agent has
// said it reads the consumed history only for its per-test mocks, the history
// the CLI hands it must stop growing with the tests already run. The session
// mocks each test consumes (all of a lax-mode MySQL replay's data mocks) are
// never read by it; they used to be re-sent, and re-decoded, with every test.
func TestRunTestSetConsumedPayloadDoesNotGrowWithTheTestIndex(t *testing.T) {
	const tests = 30
	sends, _ := consumedRun(t, tests, true)

	// The first send follows the store, before the agent has answered
	// anything: the whole history, which is empty or nearly so at that point.
	// From the second test on, the payload is the three consumed per-test
	// entries, however many session mocks the tests before it consumed.
	var sizes []int
	for i, p := range sends[2:] {
		if got := len(p.TotalConsumedMocks); got != 3 {
			t.Fatalf("send %d carried %d consumed entries; want the 3 per-test ones", i+2, got)
		}
		for name, st := range p.TotalConsumedMocks {
			if st.Usage != models.Deleted || st.Name != name {
				t.Fatalf("send %d carried %q as %+v; the per-test history must reach the agent unchanged", i+2, name, st)
			}
		}
		b, err := json.Marshal(p.TotalConsumedMocks)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(b))
	}
	if first, last := sizes[0], sizes[len(sizes)-1]; first != last {
		t.Fatalf("the consumed payload grew from %d to %d bytes across the set", first, last)
	}
}
