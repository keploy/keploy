package learn

import (
	"strings"
	"testing"
	"time"
)

// A consumer that fell behind makes a call of an older test in a later
// test's window. mock-a answers 7, and record's full round gave it the
// application only in an older test than the call's (an adapter hands the
// previous test's serves as places of the call's own test, marked so:
// Raw.ServedInAnOlderTest); mock-b, of the call's own test, answers 82. Both
// runs gave a, then b, before the call. The recording sent "slot 82", b's
// word: the application was on the window's test. Record's full round sent
// "slot 7", a's word: the application still worked on what the older test
// gave it. Each value is a copy of a place both runs gave, in the same
// order, so neither shows the other is no copy: the call is refused, naming
// the older serve, never learned as noise. So too where F served a changed
// and gave its view of it (the recording's a answered 5).
//
// The controls keep the rule a copy follows its source: a in the call's own
// test, where the order alone decides (an application copying the last
// answer took b's 82 in both runs, so F's 7 is a's by chance); and a word of
// the older serve the value holds by chance, where the recorded value holds
// no word of any place both runs gave (83): both noise, as a word of
// another test's recording is let go on the same terms (excusedGiven).
func TestALateCallsValueAnOlderServeGaveIsNeverLearnedAsNoise(t *testing.T) {
	const late = "record's full round gave the application mock-a only in an older test than the call's: the call may be a late call of that test"
	b := eAnswer("mock-b", 200*time.Millisecond, "82").ServedAs(2).ServedAsRecorded()
	asRecorded := eAnswer("mock-a", 300*time.Millisecond, "7").ServedAs(1).ServedAsRecorded()
	viewed := eAnswer("mock-a", 300*time.Millisecond, "5").ServedAs(1).ServedLive(eAnswer("mock-a", 300*time.Millisecond, "7"))
	for _, tc := range []struct {
		name     string
		recorded string
		a        Raw
		kind     Kind
		why      []string
	}{
		{"a served as recorded in an older test", "slot 82", asRecorded.ServedInAnOlderTest(), KindUnaccounted,
			[]string{"stands in mock-b", late, "the recording cannot show the value does not follow that word"}},
		{"a served changed in an older test, F's view gave 7", "slot 82", viewed.ServedInAnOlderTest(), KindUnaccounted,
			[]string{"stands in mock-b", late, "the recording cannot show the value does not follow that word"}},
		{"a served as recorded in the call's own test", "slot 82", asRecorded, KindNoise, nil},
		{"a served changed in the call's own test, F's view gave 7", "slot 82", viewed, KindNoise, nil},
		{"a served as recorded in an older test, the recorded value took no word", "slot 83", asRecorded.ServedInAnOlderTest(), KindNoise, nil},
		{"a served changed in an older test, the recorded value took no word", "slot 83", viewed.ServedInAnOlderTest(), KindNoise, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyF(t, tc.recorded, "slot 7", nil, []Raw{tc.a, b}, nil)
			if got.Kind != tc.kind {
				t.Fatalf("%s at record, slot 7 in F: %s (%q), want %s", tc.recorded, got.Kind, got.Why, tc.kind)
			}
			for _, w := range tc.why {
				if !strings.Contains(got.Why, w) {
					t.Errorf("why %q; want it to say %q", got.Why, w)
				}
			}
			// The rule's absence (ClassifyGiven) refuses every one of
			// them: the older serve is a place of the value.
			path := "bind.$8"
			rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: tc.recorded}}}
			live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "slot 7"}}}
			if g := ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, []Raw{tc.a, b}, nil, nil, nil); len(g) != 1 || g[0].Kind == KindNoise {
				t.Errorf("ClassifyGiven: %+v, want refused", g)
			}
		})
	}
}
