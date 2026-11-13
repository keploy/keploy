package learn

import (
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// usedWordsCall is an insert's exchange whose learn step used a client
// test's claim variable at its authTime's word (api-server's
// temp_login_codes insert), with the uses a word name must not reach.
func usedWordsCall() (Earlier, models.MockVariables) {
	e := Earlier{Mock: "mock-856", Own: true, Seen: []Seen{
		{Side: Request, Place: "body.authTime", Leaf: Leaf{Path: "body.authTime", Type: Number, Value: "1794453057"}, Live: "1794449579"},
		{Side: Request, Place: "body.name", Leaf: Leaf{Path: "body.name", Type: String, Value: "e2e-alpha"}, Live: "e2e-beta"},
		{Side: Request, Place: "body.token", Leaf: Leaf{Path: "body.token", Type: String, Value: "aa.bb"}, Live: "cc.dd"},
		{Side: Request, Leaf: Leaf{Path: "body.{keys}", Type: Keys, Value: "x"}, Live: "y"},
	}}
	vars := models.MockVariables{
		"Cookiejwtauth_time0": {Use: []string{"body.authTime#0"}},
		// Two variables used at one word: the first by name, as Named
		// names a whole place.
		"name1b": {Use: []string{"body.name#1"}},
		"name1a": {Use: []string{"body.name#1"}},
		// A bound word is the tracker's (BoundWords), not a use.
		"name0": {Bind: "body.name#0"},
		// A word of the text a step opens is none of the place's words.
		"sub0": {Use: []string{"body.token|jwt.sub#0"}},
		// An echo is its call's alone (Named).
		"echo0": {Echo: true, Use: []string{"body.token#0"}},
		// No word: not written as keploy writes one, or of no place.
		"lead": {Use: []string{"body.token#01"}},
		"neg":  {Use: []string{"body.token#-1"}},
		"plus": {Use: []string{"body.token#+1"}},
		"none": {Use: []string{"body.token#"}},
		"bare": {Use: []string{"#0"}},
		// A whole place is Seen.Name's.
		"whole": {Use: []string{"body.token"}},
	}
	return e, vars
}

// servedUsed is a tracker whose running test served e, its learn step's
// variables vars, and the test's number.
func servedUsed(e Earlier, vars models.MockVariables) (*Tracker, uint64) {
	tr := NewTracker()
	tr.Begin()
	n, _, _ := tr.State()
	tr.ServedNamed(n, e, vars, nil)
	return tr, n
}

// ServedNamed is Served of the named exchange, and keeps each variable its
// learn step used at one word of a request place: UsedWords gives them for
// a request leaf, in word order, with the word's text in each run: a use,
// the first by name where two are used at one word; no bind, echo, word of
// a text a step opens, or word keploy does not write so.
func TestUsedWordsGivesTheWordsAVariableIsUsedAt(t *testing.T) {
	e, vars := usedWordsCall()
	tr, n := servedUsed(e, vars)
	_, earlier, _, _ := tr.Test()
	if len(earlier) != 1 || earlier[0].Seen[0].Name != "" || earlier[0].Mock != "mock-856" {
		t.Fatalf("the test's earlier places are %+v; want the named insert, its word place unnamed whole", earlier)
	}
	if got := tr.UsedWords(n, "mock-856", e.Seen[0]); !slices.Equal(got, []BoundWord{{Name: "Cookiejwtauth_time0", Word: 0, Recorded: "1794453057", Live: "1794449579", Place: "body.authTime"}}) {
		t.Fatalf("the authTime leaf names %+v; want the claim's variable at word 0", got)
	}
	if got := tr.UsedWords(n, "mock-856", e.Seen[1]); !slices.Equal(got, []BoundWord{{Name: "name1a", Word: 1, Recorded: "alpha", Live: "beta", Place: "body.name"}}) {
		t.Fatalf("the name leaf names %+v; want name1a at word 1 alone", got)
	}
	for _, s := range e.Seen[2:] {
		if got := tr.UsedWords(n, "mock-856", s); got != nil {
			t.Errorf("leaf %s (place %q) names %+v; want none", s.Leaf.Path, s.Place, got)
		}
	}
	if got := tr.UsedWords(n, "mock-857", e.Seen[0]); got != nil {
		t.Errorf("another mock's leaf of that place names %+v; want none", got)
	}
}

// UsedWords names none for a leaf whose two runs' texts have other word
// counts, an answer's leaf at the request place's path, or a test that is
// no longer the running one; and the next test begins with none.
func TestUsedWordsNamesNoneWhereNoWordIsKnown(t *testing.T) {
	e, vars := usedWordsCall()
	tr, n := servedUsed(e, vars)
	for _, s := range []Seen{
		{Side: Request, Place: "body.authTime", Leaf: Leaf{Path: "body.authTime", Type: Number, Value: "1794453057"}, Live: "1794-449579"},
		{Side: Response, Place: "body.authTime", Leaf: Leaf{Path: "body.authTime", Type: Number, Value: "1794453057"}, Live: "1794449579"},
	} {
		if got := tr.UsedWords(n, "mock-856", s); got != nil {
			t.Errorf("%s leaf (%q, %q) names %+v; want none", s.Side, s.Leaf.Value, s.Live, got)
		}
	}
	if got := tr.UsedWords(n+1, "mock-856", e.Seen[0]); got != nil {
		t.Errorf("another test's number names %+v; want none", got)
	}
	// The next test serves the mock with no variable: the last test's use
	// names nothing there, and neither does a serve told with its number.
	tr.End()
	tr.Begin()
	m, _, _ := tr.State()
	tr.ServedNamed(m, e, nil, nil)
	tr.ServedNamed(n, e, vars, nil)
	if got := tr.UsedWords(m, "mock-856", e.Seen[0]); got != nil {
		t.Errorf("the next test names %+v from the last test's use, or from a serve told with its number; want none", got)
	}
	// A new set's mocks are named anew (mock-0 again): one served with no
	// variable names nothing the last set's use of its name did.
	tr.ServedNamed(m, e, vars, nil)
	tr.NewSet()
	k, _, _ := tr.State()
	tr.ServedNamed(k, e, nil, nil)
	if got := tr.UsedWords(k, "mock-856", e.Seen[0]); got != nil {
		t.Errorf("a new set names %+v; want none", got)
	}
	var none *Tracker
	none.ServedNamed(n, e, vars, nil)
	if got := none.UsedWords(n, "mock-856", e.Seen[0]); got != nil {
		t.Errorf("a nil tracker names %+v; want none", got)
	}
}

// The start-up's exchange, served before the set's first test begins,
// keeps its words until that test begins.
func TestUsedWordsOfTheStartUpLastUntilTheFirstTest(t *testing.T) {
	e, vars := usedWordsCall()
	tr := NewTracker()
	n, _, _ := tr.State()
	tr.ServedNamed(n, e, vars, nil)
	if got := tr.UsedWords(n, "mock-856", e.Seen[0]); len(got) != 1 || got[0].Name != "Cookiejwtauth_time0" {
		t.Fatalf("the start-up's exchange names %+v; want the claim's variable", got)
	}
	tr.Begin()
	m, _, _ := tr.State()
	if got := tr.UsedWords(m, "mock-856", e.Seen[0]); got != nil {
		t.Errorf("the first test names %+v from the start-up's use; want none", got)
	}
}

// An exchange that is not the test's own (the pool's) keeps only the run's
// variable at a word: another test's variable is unbound in this test's
// replay.
func TestUsedWordsOfAnExchangeNotTheTestsOwnAreTheRunsOnly(t *testing.T) {
	e := Earlier{Mock: "mock-9", Seen: []Seen{{Side: Request, Place: "body.ids", Leaf: Leaf{Path: "body.ids", Type: String, Value: "a1 b1"}, Live: "a2 b2"}}}
	vars := models.MockVariables{"ids0": {Use: []string{"body.ids#0"}}, models.RunVariablePrefix + "ids1": {Use: []string{"body.ids#1"}}}
	tr, n := servedUsed(e, vars)
	if got := tr.UsedWords(n, "mock-9", e.Seen[0]); !slices.Equal(got, []BoundWord{{Name: models.RunVariablePrefix + "ids1", Word: 1, Recorded: "b1", Live: "b2", Place: "body.ids"}}) {
		t.Fatalf("the pool's exchange names %+v; want the run's variable alone", got)
	}
	e.Own = true
	tr, n = servedUsed(e, vars)
	if got := tr.UsedWords(n, "mock-9", e.Seen[0]); len(got) != 2 || got[0].Name != "ids0" || got[1].Word != 1 {
		t.Fatalf("the test's own exchange names %+v; want ids0 at word 0, then the run's at word 1", got)
	}
}
