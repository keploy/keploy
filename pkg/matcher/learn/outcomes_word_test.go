package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Rows for the one entry's order on a word that is no instant (Decide): a
// source in the call's own scope or the start-up binds it, a conflict or a
// place whose state keploy cannot tell refuses it, a source only in another
// test or a worker refuses it naming the mappings edit, and a word nothing
// explains is fresh, bound at its first call. Each row names the outcome
// exactly, so none passes on an entry that refuses every word.

// oAt is the recorded instant of the call the rows decide.
var oAt = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

// oDrifts is a drift cache of an application whose clock keploy reads.
func oDrifts() *DriftCache {
	dc := NewDriftCache()
	dc.appClock = true
	return dc
}

// oClientDrifts is oDrifts of a client test (keploy mock's tests).
func oClientDrifts() *DriftCache {
	dc := oDrifts()
	dc.clientTests = true
	return dc
}

// oShape is a request whose one leaf at path holds v.
func oShape(path, v string) Shape {
	return Shape{Leaves: map[string]Leaf{path: {Path: path, Noise: path, Type: String, Value: v}}}
}

// oInput is the call at path, recorded rec and seen in record's full round
// as live, beside the earlier places given.
func oInput(path, rec, live string, earlier []Earlier, raw []Raw) Input {
	return Input{
		Drifts: oDrifts(), Drift: []string{path},
		Live: oShape(path, live), Rec: oShape(path, rec),
		At: oAt, Arrived: oAt, Earlier: earlier, Raw: raw,
		Test: "test-2", Mock: "mock-300",
		Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
	}
}

// oWord is the decision on the changed word r (recorded) and f (round F) of
// ds. An entry that decides a whole field rather than its word is read at the
// field that holds both words, so a row reports the outcome it got, not a
// missing word.
func oWord(t *testing.T, ds []Decision, r, f string) Decision {
	t.Helper()
	for _, d := range ds {
		if d.Recorded == r && d.Full == f {
			return d
		}
	}
	for _, d := range ds {
		if strings.Contains(d.Recorded, r) && strings.Contains(d.Full, f) {
			return d
		}
	}
	t.Fatalf("no decision on %q to %q in %s", r, f, oDescribe(ds))
	return Decision{}
}

// oDescribe prints ds for a failing row.
func oDescribe(ds []Decision) string {
	var b strings.Builder
	for _, d := range ds {
		b.WriteString("[" + d.At.String() + " " + d.Recorded + "->" + d.Full + " " + oOutcomeName(d))
		if d.Refusal != nil {
			b.WriteString(" " + string(d.Refusal.Kind) + ": " + d.Refusal.Why)
		}
		if d.From.Mock != "" {
			b.WriteString(" from " + d.From.Mock)
		}
		b.WriteString("] ")
	}
	return b.String()
}

func oOutcomeName(d Decision) string {
	switch d.Outcome {
	case OutcomeRefused:
		return "refused"
	case OutcomeClock:
		return "clock"
	case OutcomeValue:
		return "value"
	case OutcomeFresh:
		return "fresh"
	}
	return "?"
}

// oRefused checks d is refused as kind, its line naming every one of names.
func oRefused(t *testing.T, what string, d Decision, kind RefusalKind, names ...string) {
	t.Helper()
	if d.Outcome != OutcomeRefused || d.Refusal == nil {
		t.Errorf("FALSE GREEN? %s: %s; want refused %q", what, oDescribe([]Decision{d}), kind)
		return
	}
	if d.Refusal.Kind != kind {
		t.Errorf("%s: %s; want refused %q", what, oDescribe([]Decision{d}), kind)
		return
	}
	// A line a person reads says each reason once, in one pair of
	// parentheses.
	if strings.Contains(d.Refusal.Why, "( (") || strings.Contains(d.Refusal.Why, "))") {
		t.Errorf("%s: refused %q; its reason nests its parentheses", what, d.Refusal.Why)
	}
	for _, n := range names {
		if !strings.Contains(d.Refusal.Why, n) && !strings.Contains(d.Line, n) {
			t.Errorf("%s: refused %q (%q, line %q); want it to name %s", what, kind, d.Refusal.Why, d.Line, n)
		}
	}
}

// oEdits reports whether r offers an edit of each kind.
func oEdits(r *Refusal, kinds ...EditKind) bool {
	if r == nil {
		return false
	}
	for _, k := range kinds {
		found := false
		for _, e := range r.Edits {
			if e.Kind == k {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// oServed is an exchange served before the call, recorded 2 s before it and
// answered after answered (0: its mock holds no answer time), its leaf l as
// round F gave it (live): an earlier test's (run), the start-up's (run and
// startUp), or the call's own test's (neither).
func oServed(mock string, run, startUp bool, side Side, l Leaf, live string, answered time.Duration) []Earlier {
	m := &models.Mock{Name: mock, Spec: models.MockSpec{ReqTimestampMock: oAt.Add(-2 * time.Second)}}
	if answered != 0 {
		m.Spec.ResTimestampMock = oAt.Add(-2*time.Second + answered)
	}
	return []Earlier{{Mock: mock, At: oAt.Add(-2 * time.Second), Own: !run, Run: run, StartUp: startUp,
		Payload: RawOf(m, true).ServedAs(3), Seen: []Seen{{Side: side, Leaf: l, Live: live}}}}
}

func oText(path, v string) Leaf { return Leaf{Path: path, Noise: path, Type: String, Value: v} }

// oPg is a Postgres start-up recording made before before oAt, answering
// rows a millisecond after its request.
func oPg(name string, before time.Duration, sql string, rows []models.PostgresV3Cells, tag string) Raw {
	return RawOf(&models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: oAt.Add(-before),
		ResTimestampMock: oAt.Add(-before + time.Millisecond),
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: sql,
			Response:      &models.PostgresV3Response{Rows: rows, CommandComplete: tag},
		}},
	}}, false)
}

// oAnswer is a start-up recording answering one row, word.
func oAnswer(name string, before time.Duration, word string) Raw {
	return oPg(name, before, "SELECT next_slot FROM "+name, []models.PostgresV3Cells{{{Value: word}}}, "SELECT 1")
}

// A copy by position from answers round F gave in another order than the
// recording did: each candidate place's state in F is unknown, so the word
// is refused, never fresh (follows_test.go:462-487, :605-665 on main). Where
// F gave them in the recorded order, both places are unchanged and neither
// holds the other run's word: chance, so the word is made anew each run.
func TestOutcomeAWordCopiedFromAnswersGivenInAnotherOrderIsUnknown(t *testing.T) {
	const path = "bind.$8"
	a := oAnswer("mock-a", 300*time.Millisecond, "7")
	b := oAnswer("mock-b", 200*time.Millisecond, "82")
	together := oAnswer("mock-b", 300*time.Millisecond, "82")
	first := oAnswer("mock-9", 300*time.Millisecond, "7").SentOn("0", 9)
	second := oAnswer("mock-10", 300*time.Millisecond, "82").SentOn("0", 10)
	for _, tc := range []struct {
		name string
		raw  []Raw
	}{
		{"F served b then a", []Raw{a.ServedAs(2).ServedAsRecorded(), b.ServedAs(1).ServedAsRecorded()}},
		{"F served a, b and a again", []Raw{a.ServedIn(1, 3).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded()}},
		{"F served a where keploy does not number it", []Raw{a.WithServed(true).ServedAsRecorded(), b.ServedAs(1).ServedAsRecorded()}},
		{"F served a twice with a serve keploy cannot place between", []Raw{a.ServedIn(1, 3).ServedAsRecorded(), b.ServedAs(4).ServedAsRecorded()}},
		{"the recording gave a and b at one instant", []Raw{a.ServedAs(1).ServedAsRecorded(), together.ServedAs(2).ServedAsRecorded()}},
		{"one write, F served it against the connection's order", []Raw{first.ServedAs(2).ServedAsRecorded(), second.ServedAs(1).ServedAsRecorded()}},
		{"one instant on two connections", []Raw{first.ServedAs(1).ServedAsRecorded(), second.SentOn("1", 10).ServedAs(2).ServedAsRecorded()}},
		{"one instant, a position unknown", []Raw{first.ServedAs(1).ServedAsRecorded(), second.SentOn("0", 0).ServedAs(2).ServedAsRecorded()}},
		{"one instant, no connection", []Raw{first.ServedAs(1).ServedAsRecorded(), second.SentOn("", 10).ServedAs(2).ServedAsRecorded()}},
		{"one instant, one position twice", []Raw{first.ServedAs(1).ServedAsRecorded(), second.SentOn("0", 9).ServedAs(2).ServedAsRecorded()}},
	} {
		d := oWord(t, Decide(oInput(path, "slot 82", "slot 7", nil, tc.raw)), "82", "7")
		oRefused(t, tc.name, d, "unknown", "mock-")
	}
	// The control: F gave them in the recorded order. Both places are
	// unchanged and each holds only one run's word: chance, fresh.
	for _, tc := range []struct {
		name string
		raw  []Raw
	}{
		{"F served a then b", []Raw{a.ServedAs(1).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded()}},
		{"one write, F served it in the connection's order", []Raw{first.ServedAs(1).ServedAsRecorded(), second.ServedAs(2).ServedAsRecorded()}},
	} {
		if d := oWord(t, Decide(oInput(path, "slot 82", "slot 7", nil, tc.raw)), "82", "7"); d.Outcome != OutcomeFresh {
			t.Errorf("%s: %s; want fresh (chance at both places)", tc.name, oDescribe([]Decision{d}))
		}
	}
}

// A place whose answer time keploy cannot read, or that was answered at the
// call's own instant, is no evidence either way: refused, naming it, never
// chance (90457abcd word_coincidence_test.go:151-153).
func TestOutcomeAPlaceOfUnknownStateRefusesTheWord(t *testing.T) {
	const (
		p     = "body.docs.msgs.0._id"
		rec   = "ed35a78f-905c-48d1-bc3d-a07311678c73"
		live  = "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		other = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718"
	)
	id := oText("body.cursor.firstBatch.0._id", other)
	for _, tc := range []struct {
		what, rec, live, r, f string
		earlier               []Earlier
	}{
		{"no answer time", rec, live, "48d1", "4b8a", oServed("mock-231", true, false, Response, id, other, 0)},
		{"answered at the call", rec, live, "48d1", "4b8a", oServed("mock-231", true, false, Response, id, other, 2*time.Second)},
		{"a short count, no answer time", "46", "4499", "46", "4499", oServed("mock-231", true, false, Response, oText("body.count", "46"), "46", 0)},
	} {
		d := oWord(t, Decide(oInput(p, tc.rec, tc.live, tc.earlier, nil)), tc.r, tc.f)
		oRefused(t, tc.what, d, "unknown", "mock-231")
	}
}

// The test's own short value that stopped following its source: the test
// sent {"order":"o-1"} (F: o-9) and a later call sends /v1/orders/o-1 (F:
// o-5). The place held the value's whole recorded token and F moved it: a
// conflict at any length, refused naming the place
// (TestLearnMatchFollowsAVariable's second half).
func TestOutcomeTheTestsOwnValueThatStoppedFollowingItsSourceIsAConflict(t *testing.T) {
	sent := oServed("mock-0", false, false, Request, oText("body.order", "o-1"), "o-9", time.Millisecond)
	d := oWord(t, Decide(oInput("url.path.2", "o-1", "o-5", sent, nil)), "1", "5")
	oRefused(t, "own o-1 sent, F moved it to o-9, the call sent o-5", d, "conflict", "mock-0")
	if !oEdits(d.Refusal, "fix-app") {
		t.Errorf("a conflict's edits %+v; want fix the app or re-record first", d.Refusal)
	}
	// The same at the start-up.
	sent = oServed("mock-0", true, true, Request, oText("body.order", "o-1"), "o-9", time.Millisecond)
	d = oWord(t, Decide(oInput("url.path.2", "o-1", "o-5", sent, nil)), "1", "5")
	oRefused(t, "start-up o-1 sent, F moved it to o-9", d, "conflict", "mock-0")
}

// A word inside a longer token is a word: the "17" of "order-17", the "42" of
// "cart_42". It is judged by the places that hold it, never fresh because the
// token as a whole stands nowhere (90457abcd word_coincidence_test.go:140,
// :148, :154).
func TestOutcomeAWordInsideATokenIsJudgedByItsPlaces(t *testing.T) {
	const p = "body.ref"
	seq := func(run bool, r, f string) []Earlier {
		return oServed("mock-231", run, false, Response, oText("body.value.seq", r), f, time.Millisecond)
	}
	// Shown in another test: refused, naming the mappings edit.
	d := oWord(t, Decide(oInput(p, "order-17", "order-18", seq(true, "17", "18"), nil)), "17", "18")
	oRefused(t, "order-17 to order-18 beside another test's 17 to 18", d, "another-scope", "mock-231")
	if !oEdits(d.Refusal, "move-to-mocks", "move-to-startup", "noise-at-word") {
		t.Errorf("another scope's edits %+v; want move to mocks:, to startup:, or noise at the word", d.Refusal)
	}
	// Shown in the call's own test: a Value bound there.
	d = oWord(t, Decide(oInput(p, "order-17", "order-18", seq(false, "17", "18"), nil)), "17", "18")
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-231" {
		t.Errorf("order-17 to order-18 beside the own test's 17 to 18: %s; want a Value from mock-231", oDescribe([]Decision{d}))
	}
	for _, tc := range []struct{ run bool }{{false}, {true}} {
		d = oWord(t, Decide(oInput(p, "cart_42", "cart_43", seq(tc.run, "42", "43"), nil)), "42", "43")
		switch {
		case tc.run:
			oRefused(t, "cart_42 to cart_43 beside another test's 42 to 43", d, "another-scope", "mock-231")
		case d.Outcome != OutcomeValue || d.From.Mock != "mock-231":
			t.Errorf("cart_42 to cart_43 beside the own test's 42 to 43: %s; want a Value from mock-231", oDescribe([]Decision{d}))
		}
	}
	// Only F's place took F's new word ("k" to "18"): unknown.
	d = oWord(t, Decide(oInput(p, "k-17", "k-18", oServed("mock-231", true, false, Response, oText("body.seq", "k"), "18", time.Millisecond), nil)), "17", "18")
	oRefused(t, "k-17 to k-18 beside k to 18", d, "unknown", "mock-231")
	// A 10-digit word is an instant: a start-up place holds it unchanged, no
	// window holds it, no answer gave F's. Refused as no clock read, never
	// fresh.
	d = oWord(t, Decide(oInput(p, "kq-1730361600-z", "pw-2840472711-y", oServed("mock-231", true, true, Response, oText("body.cursor.firstBatch.0.n", "1730361600"), "1730361600", time.Millisecond), nil)), "1730361600", "2840472711")
	oRefused(t, "kq-1730361600-z beside the start-up's 1730361600", d, "no-clock-read")
}

// A record with no Body (Mongo's shape) names its places through its
// adapter's namer (Input.PlaceOf): the changed word, and a Value's bind
// place at the earlier answer that gave it, at the word that changed.
func TestOutcomeABodylessRecordNamesItsPlacesThroughItsAdapter(t *testing.T) {
	src := oServed("mock-23", false, false, Response, oText("body.documents[0]._id", "o-41"), "o-42", time.Millisecond)
	in := oInput("body.cmd.filter._id", "o-41", "o-42", src, nil)
	in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
	d := oWord(t, Decide(in), "41", "42")
	if d.Outcome != OutcomeValue || d.At.String() != "body.cmd.filter._id#1" || d.FromAt.String() != "body.documents[0]._id#1" {
		t.Errorf("o-41 to o-42 bound to an earlier answer, a record with no Body: %s at %q from %q; want a Value at body.cmd.filter._id#1 from body.documents[0]._id#1",
			oDescribe([]Decision{d}), d.At.String(), d.FromAt.String())
	}
}

// A word that stands in a connection's start-up where a swap of start-ups
// can change it (a backend pid, a secret key, a server parameter set per
// pool) is unknown, refused naming the start-up: keploy gives each
// connection a recorded start-up by the order the connections open in, not
// by what each sent, so round F's connection may have got another's, and the
// word F sent may be a copy of it (follows_test.go:819, :937). So is the pid
// of the one start-up a set recorded: a pid is each connection's own. A word
// every start-up carries alike (the protocol's 3.0) is no swap's: an
// unchanged place that holds only one of the two words is chance, fresh.
func TestOutcomeAWordInAStartUpKeployMaySwapIsUnknown(t *testing.T) {
	const p = "bind.$8"
	served := func(ms ...*models.Mock) []Raw {
		var raw []Raw
		for i, m := range ms {
			raw = append(raw, RawOf(m, false).ServedAs(i+1).ServedAsRecorded())
		}
		return raw
	}
	call := func(rec, live string, raw []Raw) []Decision {
		in := oInput(p, rec, live, nil, raw)
		in.At, in.Arrived = eRecAt, eRecAt
		return Decide(in)
	}
	pool := func(name string, before time.Duration, pid int32, app string) *models.Mock {
		m := eLaneSession(name, before, pid)
		m.Spec.PostgresV3.Session.ParameterStatus["application_name"] = app
		return m
	}
	two := served(eLaneSession("mock-hs-81", 900*time.Millisecond, 81), eLaneSession("mock-hs-82", 850*time.Millisecond, 82))
	for _, tc := range []struct {
		what, rec, live, r, f string
		raw                   []Raw
	}{
		{"the backend pid", "81", "82", "81", "82", two},
		{"the pid inside a longer value", "conn-81", "conn-82", "81", "82", two},
		{"the secret key", "7081", "7082", "7081", "7082", two},
		{"an application_name set per pool", "flyway", "hikari", "flyway", "hikari",
			served(pool("mock-hs-81", 900*time.Millisecond, 81, "flyway"), pool("mock-hs-82", 850*time.Millisecond, 82, "hikari"))},
		{"the pid of the one start-up recorded", "81", "82", "81", "82", served(eLaneSession("mock-hs-81", 900*time.Millisecond, 81))},
	} {
		d := oWord(t, call(tc.rec, tc.live, tc.raw), tc.r, tc.f)
		oRefused(t, tc.what+", copied from a start-up", d, RefusalUnknown, "mock-hs-81", "a connection start-up keploy may swap")
	}
	if d := oWord(t, call("3", "12", two), "3", "12"); d.Outcome != OutcomeFresh {
		t.Errorf("3 at record, a word every start-up carries alike: %s; want fresh (chance)", oDescribe([]Decision{d}))
	}
	// A recorded word no start-up holds is no swap's, whatever round F's
	// word is (33 at record, a start-up's pid 82 in F): chance, fresh.
	if d := oWord(t, call("33", "82", two), "33", "82"); d.Outcome != OutcomeFresh {
		t.Errorf("33 at record, a start-up's pid in F: %s; want fresh (chance)", oDescribe([]Decision{d}))
	}
}

// One new value the call sent at several places (an update's filter and its
// $set, a suite id at several fields of one insert) is one variable: bound
// at its first place, made anew each run, and used at the others, each use
// naming the first in its line, so a replay that sends the places apart
// misses. A value recorded at several places that round F sent as two
// different new values is two reads keploy cannot tell apart: each place is
// refused, naming the other.
func TestOutcomeOneNewValueAtSeveralPlacesOfACallIsOneVariable(t *testing.T) {
	const r = "6721a9f0c3d4e5f60718293a"
	shape := func(vs ...string) Shape {
		s := Shape{Leaves: map[string]Leaf{}}
		for i, p := range []string{"q.id", "u.set.id", "u.set.owner"} {
			s.Leaves[p] = Leaf{Path: p, Noise: p, Type: String, Value: vs[i]}
		}
		return s
	}
	call := func(f1, f2 string) Input {
		in := oInput("q.id", r, f1, nil, nil)
		in.Drift = []string{"q.id", "u.set.id", "u.set.owner"}
		in.Rec, in.Live = shape(r, r, "user-"+r), shape(f1, f2, "user-"+f2)
		in.PlaceOf = func(l Leaf, _ bool) (string, bool) { return "body." + l.Path, true }
		return in
	}
	const f = "6721b2c4d5e6f708192a3b4c"
	ds := Decide(call(f, f))
	if len(ds) != 3 {
		t.Fatalf("want three decided words, got %s", oDescribe(ds))
	}
	if ds[0].Outcome != OutcomeFresh {
		t.Errorf("the first place: %s; want a value made anew, bound there", oDescribe(ds[:1]))
	}
	for _, d := range ds[1:] {
		if d.Outcome != OutcomeValue || d.From.Mock != "mock-300" || d.FromAt.String() != "body.q.id#0" || !strings.Contains(d.Line, "mock-300 q.id") {
			t.Errorf("a later place of the same new value: %s (from %s at %+v); want a Value used from the call's own first place, its line naming mock-300 q.id", oDescribe([]Decision{d}), d.From.Mock, d.FromAt)
		}
	}
	const f2 = "6721c3d5e6f708192a3b4c5d"
	ds = Decide(call(f, f2))
	for i, d := range ds[:2] {
		other := []string{"mock-300 u.set.id", "mock-300 q.id"}[i]
		oRefused(t, "one recorded value round F sent as two new values", d, "two-sources", other)
	}
}
