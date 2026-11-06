package models

import (
	"strings"
	"testing"
)

// A note on a recording made outside every test names it so, in its
// sentence and its clause, whatever test the replay later names as its
// consumer, says why a test is not served it, and ends with the parser's
// hint. One on an earlier test's recording keeps its words, and a note with
// no hint ends where it did.
func TestARepeatNoteNamesARecordingMadeOutsideEveryTest(t *testing.T) {
	const hint = "declare it in keploy.yaml (sessionCalls: [...])."
	outside := &RepeatNote{Mock: "mock-0", OutsideTests: true, Hint: hint, ReplayGapSeconds: 0.5}
	for _, n := range []*RepeatNote{outside, func() *RepeatNote { c := outside.Clone(); c.Test = "rates::TestQuote"; return c }()} {
		got := n.Explain("keploy mock record")
		for _, w := range []string{"identical to mock-0, a recording made outside every test (in TestMain or at start-up)", "only for a call keploy.yaml's sessionCalls declare"} {
			if !strings.Contains(got, w) {
				t.Errorf("Explain (test %q) = %q, want it to say %q", n.Test, got, w)
			}
		}
		if strings.Contains(got, "earlier test") || !strings.HasSuffix(got, " "+hint) {
			t.Errorf("Explain (test %q) = %q: names an earlier test, or does not end with the hint", n.Test, got)
		}
		if consumed := strings.Contains(got, "(consumed by test rates::TestQuote)"); consumed != (n.Test != "") {
			t.Errorf("Explain (test %q) = %q: names its consumer %v, want %v", n.Test, got, consumed, n.Test != "")
		}
		if b := n.Brief(false); !strings.Contains(b, "mock-0, made outside every test") || strings.Contains(b, "earlier test") {
			t.Errorf("Brief (test %q) = %q, want the recording named as made outside every test", n.Test, b)
		}
	}
	earlier := &RepeatNote{Mock: "mock-0", ReplayGapSeconds: 0.5}
	got := earlier.Explain("keploy mock record")
	if !strings.Contains(got, "a recording of an earlier test of this set") || strings.Contains(got, "outside every test") ||
		!strings.HasSuffix(got, "or make the call in every test that needs it.") {
		t.Errorf("an earlier test's note says %q", got)
	}
	if b := earlier.Brief(false); !strings.Contains(b, "which answered an earlier test's call") {
		t.Errorf("an earlier test's clause is %q", b)
	}
}
