package learn

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Rows for the lines keploy prints: each of the nine refusal kinds' line
// and its edits (RefusalLine), read back whole (ParseRefusal, which keploy's
// explain and verdicts read), and each outcome's learned line: what and
// where, both values, the rule and the undo.

// rfBase is a refusal of word 1 of mock-300's body.slot in test-2.
func rfBase(kind RefusalKind, why string, places []string, edits ...Edit) Refusal {
	return Refusal{Kind: kind, Test: "test-2", Mock: "mock-300", Field: "body.slot", Word: 1,
		Recorded: "slot-17", Full: "slot-18", Why: why, Places: places, Edits: edits}
}

// Each of the nine kinds prints its line: the call, the word, both values,
// the kind's clause, the places (at most three), and its edits in words a
// person or an LLM acts on; and ParseRefusal reads the same Refusal back.
func TestOutcomeRefusalLinesNameTheWordAndTheirEdits(t *testing.T) {
	const prefix = `test-2 mock-300 body.slot word 1: recorded "slot-17", round F "slot-18": `
	word := Edit{Kind: "noise-at-word", Mock: "mock-300", Field: "body.slot", Word: 1}
	field := Edit{Kind: "noise-at-field", Mock: "mock-300", Field: "body.slot", Word: -1}
	for _, c := range []struct {
		r    Refusal
		want []string // what the line says beside the prefix and Why
	}{
		{rfBase("pick", "round F's moment stands unchanged in mock-12 body.next", []string{"mock-12 body.next"}, word, Edit{Kind: "none"}),
			[]string{"mock-12 body.next", "keploy.yml", "word 1 of body.slot"}},
		{rfBase("no-clock-read", "a time its call's window does not hold in both runs, that no answer gave and no request of this scope sent, but mock-9 in TestA did", []string{"mock-9 body.at"},
			Edit{Kind: "move-to-mocks", Mock: "mock-9"}, word, Edit{Kind: "re-record"}),
			[]string{"mock-9", "mocks:", "re-record"}},
		{rfBase("two-sources", "the same moment was sent at mock-4 and mock-7, two separate reads", []string{"mock-4 body.at", "mock-7 body.at"},
			Edit{Kind: "move-to-worker", Mock: "mock-7"}, Edit{Kind: "move-to-startup", Mock: "mock-7"}, word),
			[]string{"mock-7", "worker:", "startup:"}},
		{rfBase("conflict", `slot-17 stood whole at mock-5 body.id; round F gave slot-90 there and the value did not follow`, []string{"mock-5 body.id"},
			Edit{Kind: "fix-app"}, field, Edit{Kind: "give", Name: "slot"}),
			[]string{"mock-5 body.id", `keploytest.Given(t, "slot"`, "noise at body.slot"}},
		{rfBase("unknown", "keploy cannot tell what round F gave at mock-6 rows[0][0] before this call (view not stated)", []string{"mock-6 rows[0][0]"},
			Edit{Kind: "re-record"}, word),
			[]string{"mock-6 rows[0][0]", "view not stated", "re-record"}},
		{rfBase("another-scope", "the value mock-21 sent in TestA; keploy binds only within a test and the start-up", []string{"mock-21 body.token"},
			Edit{Kind: "move-to-mocks", Mock: "mock-21"}, Edit{Kind: "move-to-startup", Mock: "mock-21"}, word),
			[]string{"mock-21", "mocks:", "startup:"}},
		{rfBase("unread", "keploy could not read the test case's request (none was sent), which came before this call", nil,
			Edit{Kind: "re-record"}, field),
			[]string{"the test case's request", "re-record", "noise at body.slot"}},
		{rfBase("changed-shape", "a word was added or dropped", nil, field),
			[]string{"a word was added or dropped", "noise at body.slot"}},
		{rfBase("cannot-check", "Mongo cannot bind a word inside this field yet", nil, field),
			[]string{"keploy's limit", "noise at body.slot"}},
	} {
		line := RefusalLine(c.r)
		if !strings.HasPrefix(line, prefix) || !strings.Contains(line, c.r.Why) {
			t.Errorf("%s: %q; want it to start %q and hold its clause %q", c.r.Kind, line, prefix, c.r.Why)
		}
		for _, w := range c.want {
			if !strings.Contains(line, w) {
				t.Errorf("%s: %q; want it to say %q", c.r.Kind, line, w)
			}
		}
		back, ok := ParseRefusal(line)
		if !ok || !reflect.DeepEqual(back, c.r) {
			t.Errorf("%s: ParseRefusal(%q) = %+v, %v; want %+v", c.r.Kind, line, back, ok, c.r)
		}
	}
}

// A whole field prints no word index; values with quotes, colons and
// newlines read back as they were; more than three places print three; a
// line that is no refusal does not parse.
func TestOutcomeRefusalLinesReadBackWhatTheySay(t *testing.T) {
	r := rfBase("conflict", "it stood whole elsewhere", []string{"mock-1 a", "mock-2 b", "mock-3 c"}, Edit{Kind: "fix-app"})
	r.Word, r.Recorded, r.Full = -1, `say "hi": now`+"\n", `x: "y"; z`
	line := RefusalLine(r)
	if !strings.HasPrefix(line, `test-2 mock-300 body.slot: recorded `) || strings.Contains(line, "\n") {
		t.Errorf("a whole field: %q; want no word index, on one line", line)
	}
	if back, ok := ParseRefusal(line); !ok || !reflect.DeepEqual(back, r) {
		t.Errorf("ParseRefusal(%q) = %+v, %v; want %+v", line, back, ok, r)
	}
	many := r
	many.Places = []string{"mock-1 a", "mock-2 b", "mock-3 c", "mock-4 d"}
	if line := RefusalLine(many); strings.Contains(line, "mock-4 d") || !strings.Contains(line, "mock-3 c") {
		t.Errorf("four places: %q; want the first three", line)
	}
	for _, s := range []string{"", "test-2 mock-300 body.slot: learned a Clock", "keploy cannot tell"} {
		if _, ok := ParseRefusal(s); ok {
			t.Errorf("ParseRefusal(%q) read a refusal", s)
		}
	}
	// The edits end the line in one fixed form a tool reads as JSON.
	r.Edits = []Edit{{Kind: "move-to-mocks", Mock: "mock-9"}, {Kind: "noise-at-word", Mock: "mock-300", Field: "body.slot", Word: 0}, {Kind: "give", Name: `a "b"`}}
	const tail = ` edits=[{"kind":"move-to-mocks","mock":"mock-9"},{"kind":"noise-at-word","mock":"mock-300","field":"body.slot"},{"kind":"give","name":"a \"b\""}]`
	if line := RefusalLine(r); !strings.HasSuffix(line, "."+tail) {
		t.Errorf("edits: %q; want it to end %q", line, tail)
	}
	// Text that looks like the tail, in a value, a why, a place or a name,
	// reads back as it was; an empty why prints quoted.
	odd := rfBase("unknown", `see edits=[] here`, []string{`mock-1 a edits=[]`, `"q", r`}, Edit{Kind: "give", Name: `x. edits=[{"kind":"none"}]`})
	odd.Recorded, odd.Full, odd.Mock = ` edits=[{"kind":"none"}]`, `"`, `m edits=[]`
	empty := rfBase("pick", "", nil)
	for _, r := range []Refusal{odd, empty} {
		if back, ok := ParseRefusal(RefusalLine(r)); !ok || !reflect.DeepEqual(back, r) {
			t.Errorf("ParseRefusal(%q) = %+v, %v; want %+v", RefusalLine(r), back, ok, r)
		}
	}
	// An edit of no known kind, or a tail that is cut, is no refusal.
	unknown := rfBase("pick", "a pick", nil, Edit{Kind: "rewrite-history"})
	cut := RefusalLine(rfBase("pick", "a pick", nil, Edit{Kind: "re-record"}))
	for _, s := range []string{RefusalLine(unknown), cut[:len(cut)-2], strings.Replace(cut, "re-record", "none", 1)} {
		if _, ok := ParseRefusal(s); ok {
			t.Errorf("ParseRefusal(%q) read a refusal", s)
		}
	}
}

// Every outcome's line says what was learned and where (test, mock, field,
// word), both values, the rule that gave it, and how to undo it, naming at
// most three places.
func TestOutcomeLearnedLinesSayWhatWhereWhyAndTheUndo(t *testing.T) {
	ms := time.Millisecond
	fAt := oAt.Add(FullRoundLead)
	src := oServed("mock-7", false, false, Response, oText("body.order", "ord-9a1c4e7b2d"), "ord-77f0e3a5c1", ms)
	clockIn := iCall("body.at", String, iMs(oAt.Add(-5*ms)), iMs(fAt.Add(-5*ms)), oAt, fAt, iWin(oAt, fAt, 50*ms), nil, nil)
	for _, c := range []struct {
		what, r, f string
		in         Input
		want       WordOutcome
		says       []string
	}{
		{"a Value", "9a1c4e7b2d", "77f0e3a5c1", oInput("header.X-Order", "Order ord-9a1c4e7b2d", "Order ord-77f0e3a5c1", src, nil), OutcomeValue, []string{"mock-7", "body.order"}},
		{"a fresh value", "8c41f2", "e09b7d", oInput("body.ref", "ref-8c41f2", "ref-e09b7d", nil, nil), OutcomeFresh, []string{"first"}},
		{"a Clock", iMs(oAt.Add(-5 * ms)), iMs(fAt.Add(-5 * ms)), clockIn, OutcomeClock, []string{"clock"}},
	} {
		d := oWord(t, Decide(c.in), c.r, c.f)
		if d.Outcome != c.want {
			t.Errorf("%s: %s; want %s", c.what, oDescribe([]Decision{d}), oOutcomeName(Decision{Outcome: c.want}))
			continue
		}
		for _, s := range append([]string{c.in.Test, c.in.Mock, c.in.Drift[0], c.r, c.f, "undo"}, c.says...) {
			if !strings.Contains(strings.ToLower(d.Line), strings.ToLower(s)) {
				t.Errorf("%s: line %q; want it to say %q", c.what, d.Line, s)
			}
		}
	}
}

// A variable a call binds at another recording keeps its why through the
// fold (FoldLearned, MergeVariable), as one it declares at its own does
// (models.MergeVariables): the target's file says why it is a variable.
func TestOutcomeAVariableBoundAtAnotherRecordingKeepsItsWhy(t *testing.T) {
	const why = "a value mock-1's answer gave and mock-2 sent back: one value, checked at every use"
	_, binds := FoldLearned(map[string]models.MockState{"mock-2": {Name: "mock-2",
		Binds: map[string]models.MockVariables{"mock-1": {"v1": {Bind: "body.id", Recorded: "ab12", Why: why}}}}})
	if got := binds["mock-1"]["v1"].Why; got != why {
		t.Errorf("the variable mock-2 binds at mock-1 says why %q; want %q", got, why)
	}
	if got := MergeVariable(models.MockVariable{Bind: "body.id", Recorded: "ab12", Why: why}, models.MockVariable{Use: []string{"body.ref"}, Why: "another"}).Why; got != why {
		t.Errorf("two declarations of one variable say why %q; want the first's, %q", got, why)
	}
}

// A secret's values never print. Where a line's field, or a place it names,
// is a secret's by keploy's one rule for a name (the one its container
// fingerprint uses: a token field, an Authorization header), both values
// print as "***" in the line and in the refusal it carries, and so does every
// value its reason quotes; the decision itself keeps both values, which the
// adapter binds. Every other value prints in full (a createdAt).
func TestOutcomeASecretsValuesPrintMasked(t *testing.T) {
	ms := time.Millisecond
	const r, f, other = "tk1a2b3c4d", "tk5e6f7g8h", "tk9z8y7x6w"
	sentOwn := oServed("mock-0", false, false, Request, oText("body.session", r), other, ms)
	gaveToken := oServed("mock-0", false, false, Response, oText("body.access_token", r), f, ms)
	for _, c := range []struct {
		what string
		in   Input
		want WordOutcome
	}{
		{"a token field made anew", oInput("body.token", r, f, nil, nil), OutcomeFresh},
		{"an Authorization header whose value stopped following its source", oInput("header.Authorization", "Bearer "+r, "Bearer "+f, sentOwn, nil), OutcomeRefused},
		{"a value bound at a token field", oInput("header.X-Ref", "ref "+r, "ref "+f, gaveToken, nil), OutcomeValue},
	} {
		d := oWord(t, Decide(c.in), r, f)
		if d.Outcome != c.want {
			t.Errorf("%s: %s; want %s", c.what, oDescribe([]Decision{d}), oOutcomeName(Decision{Outcome: c.want}))
		}
		if d.Recorded != r || d.Full != f {
			t.Errorf("%s: the decision holds %q and %q; want both values, which the adapter binds", c.what, d.Recorded, d.Full)
		}
		said := d.Line
		if d.Refusal != nil {
			said += " " + d.Refusal.Recorded + " " + d.Refusal.Full + " " + d.Refusal.Why
		}
		for _, v := range []string{r, f, other} {
			if strings.Contains(said, v) {
				t.Errorf("LEAK %s: %q prints the value %q", c.what, said, v)
			}
		}
		if !strings.Contains(d.Line, `recorded "***", round F "***"`) {
			t.Errorf("%s: line %q; want both values printed as \"***\"", c.what, d.Line)
		}
	}
	const rAt, fAt = "2026-10-31T08:00:00Z", "2026-10-31T08:30:00Z"
	for _, d := range Decide(oInput("body.createdAt", rAt, fAt, nil, nil)) {
		if strings.Contains(d.Line, "***") || !strings.Contains(d.Line, strconv.Quote(d.Recorded)) || !strings.Contains(d.Line, strconv.Quote(d.Full)) {
			t.Errorf("a createdAt: line %q; want it to print %q and %q in full", d.Line, d.Recorded, d.Full)
		}
	}
}
