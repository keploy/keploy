package learn

import "testing"

// ChangedWord finds the one word by which a value changed, every other byte
// alike, and nothing else: values alike, or apart in two words, in their
// count of words, or in a byte between words, have no such word.
func TestChangedWord(t *testing.T) {
	for _, tc := range []struct {
		rec, live string
		n         int
		ok        bool
	}{
		{"octo-org/repo-1762891234", "octo-org/repo-1762895678", 3, true},
		{"a1b2", "c3d4", 0, true},
		{"CI identity: github octo-org/repo-1", "CI identity: github octo-org/repo-2", 6, true},
		{"octo-org/repo-1", "octo-org/repo-1", -1, false},
		{"octo-org/repo-1", "octo-org/rope-2", -1, false},
		{"octo-org/repo-1", "octo-org/repo-1-2", -1, false},
		{"octo-org/repo-1-2", "octo-org/repo-1", -1, false},
		{"octo-org/repo-1", "octo-org/repo_2", -1, false},
		{"octo-org/repo-1", "octo-org/repo-2.", -1, false},
		{"octo-org/repo-1", "octo-org.repo-2", -1, false},
		{"octo-org/repo-1", "octo-org/repo_1", -1, false},
		{"--", "++", -1, false},
	} {
		if n, ok := ChangedWord(tc.rec, tc.live); n != tc.n || ok != tc.ok {
			t.Errorf("ChangedWord(%q, %q) = %d, %v; want %d, %v", tc.rec, tc.live, n, ok, tc.n, tc.ok)
		}
	}
}

// wordBindAt binds a client test's request field at the one word it changed
// by where the value changed by that same word, as recorded and as F saw it,
// and at the field whole where the field is the word whole. It binds no
// call's request, no field or value that changed by more than one word, and
// no word that is not the value's in either run.
func TestWordBindAt(t *testing.T) {
	const (
		v = "CI identity: github octo-org/repo-1"
		f = "CI identity: github octo-org/repo-2"
	)
	client := Place{Client: true, Var: "body.variables.input.conditions[0].value"}
	for _, tc := range []struct {
		what            string
		p               Place
		rec, live, v, f string
		want            string
	}{
		{"the word inside the field", client, "octo-org/repo-1", "octo-org/repo-2", v, f, "body.variables.input.conditions[0].value#3"},
		{"the field is the word whole", client, "1", "2", v, f, "body.variables.input.conditions[0].value"},
		{"a call's request", Place{Var: "body.event"}, "trusted octo-org/repo-1", "trusted octo-org/repo-2", v, f, ""},
		{"F's word is not the value's", client, "octo-org/repo-1", "octo-org/repo-3", v, f, ""},
		{"the recorded word is not the value's", client, "octo-org/repo-3", "octo-org/repo-2", v, f, ""},
		{"the field changed by two words", client, "octo-org/repo-1-a", "octo-org/repo-2-b", v, f, ""},
		{"the field changed by a byte between words too", client, "octo-org/repo-1", "octo-org/repo_2", v, f, ""},
		{"the value changed by two words", client, "octo-org/repo-1", "octo-org/repo-2", v + " by a", f + " by b", ""},
		{"the value did not change", client, "octo-org/repo-1", "octo-org/repo-2", v, v, ""},
		{"a field at a word already", Place{Client: true, Var: "body.name#2"}, "octo-org/repo-1", "octo-org/repo-2", v, f, ""},
		{"no place", Place{Client: true}, "octo-org/repo-1", "octo-org/repo-2", v, f, ""},
	} {
		if got := wordBindAt(tc.p, tc.rec, tc.live, tc.v, tc.f); got != tc.want {
			t.Errorf("%s: wordBindAt = %q; want %q", tc.what, got, tc.want)
		}
	}
}
