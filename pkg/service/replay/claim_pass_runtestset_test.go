package replay

import (
	"fmt"
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A retry cycle (--retry-passing-test) runs the passing test cases again
// without staging the set again: what tells the agent the cycle is a new pass
// over the set is the count of the consumed history's rewinds every filter of
// it carries (models.MockFilterParams.ConsumedRewinds). The filter that opens
// test-1 in the second cycle carries one rewind, in both switches: under exact
// serving too, whose filters carried none, so the agent never started the
// pass and test-1 was not delivered again what its recording delivered it
// once per pass (an in-answer notification, MockManager.ClaimOnce).
func TestRunTestSetRetryCycleFiltersCarryTheRewindsInBothSwitches(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", on), func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.RetryPassing = true
			h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-2": true}}
			_ = h.run(t)

			start, _ := testWindowOf(h.cases[0])
			var opens, all []uint64
			exact := false
			h.instr.mu.Lock()
			for _, p := range h.instr.allParams {
				all = append(all, p.ConsumedRewinds)
				switch ex := p.Exact; {
				case ex != nil && ex.Test == "test-1":
					exact = true
					opens = append(opens, p.ConsumedRewinds)
				case ex == nil && p.AfterTime.Equal(start):
					opens = append(opens, p.ConsumedRewinds)
				}
			}
			h.instr.mu.Unlock()
			if exact != on {
				t.Fatalf("precondition: test-1 was opened by an exact filter = %v, want %v (the switch)", exact, on)
			}
			if want := []uint64{0, 1}; !slices.Equal(opens, want) {
				t.Fatalf("the filters opening test-1 carry rewinds %v, want %v: the second cycle is a new pass over the set", opens, want)
			}
			// Every filter of the set carries its cycle's count, the pool's
			// between test cases too: the count only rises within a set
			// (models.MockFilterParams.ConsumedRewinds). The window rewind
			// count (MockManager.WindowRewinds) takes any change of it as a
			// pass over the set again, so a filter that went back to a lower
			// one would count passes the run never started.
			for i := 1; i < len(all); i++ {
				if all[i] < all[i-1] {
					t.Fatalf("filter %d of the set carries rewinds %d after %d: every filter carries its cycle's count (all: %v)", i, all[i], all[i-1], all)
				}
			}
		})
	}
}
