package learn

import (
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/variables"
)

// TestReplayAndLearnShareOneWordRule (TY3): variables.Words, the word rule
// replay reads, finds the runs learn's isWord finds.
func TestReplayAndLearnShareOneWordRule(t *testing.T) {
	runs := func(s string) [][2]int {
		var out [][2]int
		for i := 0; i < len(s); i++ {
			if !isWord(s[i]) {
				continue
			}
			j := i
			for j < len(s) && isWord(s[j]) {
				j++
			}
			out = append(out, [2]int{i, j})
			i = j
		}
		return out
	}
	for _, s := range []string{
		"order-17", "uploads/17/a.png", "Order+17", "6f1c2b9e-3d4a-4e5f-8a7b-0c1d2e3f4a5b",
		"eyJpZCI6Im8tMTcifQ_-x9", "cart_42", "", "---", "é17ü", "a.b=c,d;e f\tg",
	} {
		if got, want := variables.Words(s), runs(s); !reflect.DeepEqual(got, want) {
			t.Errorf("Words(%q) = %v; learn's isWord runs are %v", s, got, want)
		}
	}
	got := variables.Words("order-17")
	if len(got) != 2 || "order-17"[got[1][0]:got[1][1]] != "17" {
		t.Errorf(`the "17" of "order-17" is not a word: %v`, got)
	}
}

// TestDecideWithNoOrderRefusesEveryChangedWord (TY4): with no order
// registered, Decide refuses every drifted word as one keploy cannot check
// yet, a leaf whose words do not pair by position whole, and never gives a
// Clock, a Value or a fresh word.
func TestDecideWithNoOrderRefusesEveryChangedWord(t *testing.T) {
	leaves := func(kv ...string) map[string]Leaf {
		m := map[string]Leaf{}
		for i := 0; i < len(kv); i += 2 {
			l := Leaf{Path: kv[i], Type: String, Value: kv[i+1]}
			if strings.HasPrefix(kv[i], "header.date") {
				l.Type = Instant
			}
			m[kv[i]] = l
		}
		return m
	}
	in := Input{
		Test: "test-1", Mock: "mock-3",
		Rec:   Shape{Leaves: leaves("body.id", "order-17-a-5", "body.sep", "a-b", "header.date", "2026-11-01T10:00:00Z", "body.more", "a b"), Body: map[string]interface{}{}},
		Live:  Shape{Leaves: leaves("body.id", "order-42-a-6", "body.sep", "a_b", "header.date", "2026-11-01T10:00:05Z", "body.more", "a b c"), Body: map[string]interface{}{}},
		Drift: []string{"body.id", "body.sep", "header.date", "body.more", "body.gone"},
	}
	ds := Decide(in)
	type got struct {
		field, at, r, f string
		word            int
	}
	var gots []got
	for _, d := range ds {
		if d.Outcome != OutcomeRefused || d.Clock != nil || !reflect.DeepEqual(d.From, Place{}) || len(d.Echo) != 0 || d.Refusal == nil {
			t.Fatalf("Decide gave %+v; want a refusal alone", d)
		}
		r := d.Refusal
		if r.Kind != RefusalCannotCheck || r.Test != "test-1" || r.Mock != "mock-3" || r.Recorded != d.Recorded || r.Full != d.Full || len(r.Edits) != 1 || r.Edits[0].Kind != EditNoiseAtField {
			t.Errorf("refusal %+v", r)
		}
		if !strings.HasPrefix(d.Line, "test-1 mock-3 "+r.Field) || !strings.Contains(d.Line, "keploy cannot check") || !strings.Contains(d.Line, "keploy's limit") {
			t.Errorf("line %q", d.Line)
		}
		gots = append(gots, got{r.Field, d.At.String(), d.Recorded, d.Full, r.Word})
	}
	want := []got{
		{"body.id", "body.id#1", "17", "42", 1},
		{"body.id", "body.id#3", "5", "6", 3},
		{"body.sep", "body.sep", "a-b", "a_b", -1},
		{"header.date", "header.date", "2026-11-01T10:00:00Z", "2026-11-01T10:00:05Z", -1},
		{"body.more", "body.more", "a b", "a b c", -1},
		{"body.gone", "", "", "", -1},
	}
	if !reflect.DeepEqual(gots, want) {
		t.Errorf("Decide refused\n%+v\nwant\n%+v", gots, want)
	}
	if !strings.HasPrefix(ds[0].Line, `test-1 mock-3 body.id word 1: recorded "17", round F "42": `) {
		t.Errorf("line %q", ds[0].Line)
	}
}

// TestTheSeamsDefaultToMainsReadings: until v5's order and clock register,
// Decide's order refuses every word, and its clock seams are main's
// readings (offsetReading, sinceTestBegin).
func TestTheSeamsDefaultToMainsReadings(t *testing.T) {
	same := func(a, b interface{}) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	if !same(decideBy, refuseAll) || !same(clockInWindow, offsetReading) || !same(clockSinceBegin, sinceTestBegin) {
		t.Fatal("a seam does not default to main's reading")
	}
	if ln, ok := clockSinceBegin(Learned{Kind: KindNoise}); ok || ln.Kind != KindNoise {
		t.Errorf("clockSinceBegin of noise = %+v, %v", ln, ok)
	}
	if _, why, ok := clockInWindow("body.at", typedInstant{}, typedInstant{}, nil, nil); ok || why != "" {
		t.Errorf("clockInWindow with no window = %q, %v", why, ok)
	}
}
