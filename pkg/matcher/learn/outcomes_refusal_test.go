package learn

import (
	"reflect"
	"strings"
	"testing"
	"time"
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

// Each of the nine kinds prints §5's line: the call, the word, both values,
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
}

// Every outcome's line says what was learned and where (test, mock, field,
// word), both values, the rule that gave it, and how to undo it, naming at
// most three places.
func TestOutcomeLearnedLinesSayWhatWhereWhyAndTheUndo(t *testing.T) {
	ms := time.Millisecond
	fAt := oAt.Add(FullRoundLead)
	src := oServed("mock-7", false, false, Response, oText("body.token", "tok-9a1c4e7b2d"), "tok-77f0e3a5c1", ms)
	clockIn := iCall("body.at", String, iMs(oAt.Add(-5*ms)), iMs(fAt.Add(-5*ms)), oAt, fAt, iWin(oAt, fAt, 50*ms), nil, nil)
	for _, c := range []struct {
		what, r, f string
		in         Input
		want       WordOutcome
		says       []string
	}{
		{"a Value", "9a1c4e7b2d", "77f0e3a5c1", oInput("header.Authorization", "Bearer tok-9a1c4e7b2d", "Bearer tok-77f0e3a5c1", src, nil), OutcomeValue, []string{"mock-7", "body.token"}},
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
