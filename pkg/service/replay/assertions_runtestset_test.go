package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// keploy test fails a test case whose assertion this keploy does not
// implement, whatever the app answered, with the proven switch off or on.
// Such an assertion (a keploy/tests type, json_path here) replaced the
// comparison and passed any answer: a wrong one was PASSED. The right answer
// is the one that pins it: a wrong one fails on the comparison too, were the
// assertion sent back to it.
func TestRunTestSet_AnAssertionKeployDoesNotImplementFailsItsTest(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, answer := range []struct {
			name  string
			wrong bool
		}{{"the right answer", false}, {"a wrong answer", true}} {
			t.Run(switchName(on)+"/"+answer.name, func(t *testing.T) {
				setSwitch(t, on)
				h := newPartialRunHarness(t, 1, 0)
				h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-1": answer.wrong}}
				if !answer.wrong {
					// The answer is right: without the assertion, the set passes.
					if status := h.run(t); status != models.TestSetStatusPassed {
						t.Fatalf("without the assertion, set %s; want PASSED", status)
					}
					h = newPartialRunHarness(t, 1, 0)
				}
				h.cases[0].Assertions = map[models.AssertionType]interface{}{"json_path": "$.id"}
				h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-1": answer.wrong}}

				if status := h.run(t); status != models.TestSetStatusFailed {
					t.Fatalf("set %s; want FAILED", status)
				}
				if r := resultOf(t, h, "test-1"); r.Status != models.TestStatusFailed {
					t.Fatalf("test-1 is %s, want FAILED: its assertion judges nothing", r.Status)
				}
			})
		}
	}
}
