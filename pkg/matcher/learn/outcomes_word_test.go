package learn

import (
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"reflect"
	"strconv"
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
// is refused, never fresh (follows_test.go:462-487, :605-665 on main). One
// candidate has such an order too where F served it twice and the
// recording gave it once (follows_test.go:481). A recording whose answer
// is stamped before its own request has no answer time keploy can read
// (follows_test.go:553). Where F gave them in the recorded order, both
// places are unchanged and neither holds the other run's word: chance, so
// the word is made anew each run.
func TestOutcomeAWordCopiedFromAnswersGivenInAnotherOrderIsUnknown(t *testing.T) {
	const path = "bind.$8"
	a := oAnswer("mock-a", 300*time.Millisecond, "7")
	b := oAnswer("mock-b", 200*time.Millisecond, "82")
	nine := oAnswer("mock-b", 200*time.Millisecond, "9")
	backwards := RawOf(&models.Mock{Name: "mock-a", Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: oAt.Add(-300 * time.Millisecond),
		ResTimestampMock: oAt.Add(-400 * time.Millisecond),
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "SELECT next_slot FROM mock-a",
			Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: "7"}}}, CommandComplete: "SELECT 1"},
		}},
	}}, false)
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
		{"F served a twice, b between holds neither word", []Raw{a.ServedIn(1, 3).ServedAsRecorded(), nine.ServedAs(2).ServedAsRecorded()}},
		{"a's answer stamped before its request", []Raw{backwards.ServedAs(1).ServedAsRecorded(), nine.ServedAs(2).ServedAsRecorded()}},
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
		{"F served a once, b after it holds neither word", []Raw{a.ServedAs(1).ServedAsRecorded(), nine.ServedAs(2).ServedAsRecorded()}},
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

// A read-back: a find by an id ("app-0b1e") whose changed word an earlier
// insert of the test sent inside its longer name binds that word there, and
// its own answer, which holds the find's recorded id whole, is answered at
// the same word with this run's word, its line naming the answer place.
// Only there: a longer text holding the word (the name read back) and the
// word alone stay served as recorded. An adapter that cannot answer at a
// word, or an answer place keploy cannot name, refuses it: a Value whose
// read-back keploy would serve as recorded is never learned.
func TestOutcomeAReadBackIsAnsweredWhereItsAnswerHoldsTheRequestsTextWhole(t *testing.T) {
	src := oServed("mock-23", false, false, Request, oText("body.documents[0].name", "owner-app-0b1e"), "owner-app-77c4", time.Millisecond)
	in := func() Input {
		in := oInput("body.cmd.filter._id", "app-0b1e", "app-77c4", src, nil)
		in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
		in.Resp = []Leaf{oText("body.cursor.firstBatch[0]._id", "app-0b1e"), oText("body.cursor.firstBatch[0].name", "owner-app-0b1e"),
			oText("body.cursor.firstBatch[0].tag", "0b1e"), oText("body.cursor.firstBatch[1]._id", "app-0b1e")}
		return in
	}
	d := oWord(t, Decide(in()), "0b1e", "77c4")
	var echo []string
	for _, a := range d.Echo {
		echo = append(echo, a.String())
	}
	want := "body.cursor.firstBatch[0]._id#1, body.cursor.firstBatch[1]._id#1"
	if d.Outcome != OutcomeValue || d.FromAt.String() != "body.documents[0].name#2" || strings.Join(echo, ", ") != want ||
		!strings.Contains(d.Line, "answered at "+want+", where the call's answer holds \"app-0b1e\" whole as recorded") {
		t.Errorf("FALSE RED: a find whose cursor reads its id back: %s from %q, echo %v, line %q; want a Value from body.documents[0].name#2 answered at %s, its line naming them",
			oDescribe([]Decision{d}), d.FromAt.String(), echo, d.Line, want)
	}
	noWord := in()
	noWord.Caps.AnswerAtWord = false
	oRefused(t, "a read-back an adapter cannot answer at its word", oWord(t, Decide(noWord), "0b1e", "77c4"), RefusalCannotCheck, "cannot answer a word")
	unnamed := in()
	unnamed.Resp = append(unnamed.Resp, Leaf{Path: "body.cursor.firstBatch[2]._id", Type: String, Value: "app-0b1e", Unnamed: true})
	oRefused(t, "a read-back at a place keploy cannot name", oWord(t, Decide(unnamed), "0b1e", "77c4"), RefusalCannotCheck, "cannot name")
	keyed := in()
	keyed.Resp = append(keyed.Resp, Leaf{Path: "body.cursor{keys}", Type: Keys, Value: "_id\napp-0b1e\nname", Unnamed: true})
	oRefused(t, "a read-back at an answer key", oWord(t, Decide(keyed), "0b1e", "77c4"), RefusalCannotCheck, "body.cursor{keys}")
	// No answer leaf holds the id whole: no echo, the Value as before.
	none := in()
	none.Resp = none.Resp[1:3]
	if d := oWord(t, Decide(none), "0b1e", "77c4"); d.Outcome != OutcomeValue || len(d.Echo) != 0 || strings.Contains(d.Line, "answered at") {
		t.Errorf("a find whose answer holds the word only inside other text: %s, echo %v; want a Value with no echo", oDescribe([]Decision{d}), d.Echo)
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

// A field whose adapter names decode steps at its place (Input.PlaceOf, a
// Mongo field read as base64) is read in the text those steps open: each
// changed word is named through the steps, so its address reads its own
// recorded and round F values. A step that does not open both runs, or
// opens them to one text (only the encoding changed), refuses the field
// whole, as written, at its place with no step.
func TestOutcomeAnAdaptersStepsOpenAFieldBeforeItsWordsAreRead(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	withB64 := func(l Leaf, a bool) (string, bool) { s, ok := VarPlace(l, true); return s + "|b64", ok }
	for _, c := range []struct {
		name, r, f, at string
		word           int
		refused        bool
	}{
		{"the steps open both runs", b64("order-17"), b64("order-42"), "body.name|b64#1", 1, false},
		{"a step opens one run", b64("order-17"), "order-42", "body.name", -1, true},
		{"a step opens neither run", "order-17", "order-42", "body.name", -1, true},
		{"only the encoding changed", b64("order-17"), strings.TrimRight(b64("order-17"), "="), "body.name", -1, true},
	} {
		in := oInput("body.name", c.r, c.f, nil, nil)
		in.PlaceOf = withB64
		ds := Decide(in)
		if len(ds) != 1 {
			t.Errorf("%s: %s; want one decision", c.name, oDescribe(ds))
			continue
		}
		d := ds[0]
		if w, ok := d.At.WordOf(c.r); !ok || w != d.Recorded {
			t.Errorf("%s: At %q reads %q, %v in the recording; its Recorded is %q", c.name, d.At, w, ok, d.Recorded)
		}
		if w, ok := d.At.WordOf(c.f); !ok || w != d.Full {
			t.Errorf("%s: At %q reads %q, %v in round F; its Full is %q", c.name, d.At, w, ok, d.Full)
		}
		if d.At.String() != c.at || d.At.Word != c.word || (d.Outcome == OutcomeRefused) != c.refused {
			t.Errorf("%s: %s at %q word %d; want at %q word %d, refused %v", c.name, oDescribe(ds), d.At, d.At.Word, c.at, c.word, c.refused)
		}
	}
}

// A changed word with no source is fresh, and the call's own answer is
// rewritten with this run's word only where an answer field holds the
// request's whole recorded text, at the same word: the rule a read-back
// follows. Every other answer text that holds the word ("batch-14" sent,
// "seen 14 times" or a date whose minute is 14 answered; "14" sent, the
// same count answered) is served as recorded: a word that short may stand
// there by chance, and a test that reads it goes red, never green by a
// rewrite (main refuses the first: instants_test.go:838). The line names
// only the places rewritten.
func TestOutcomeAFreshWordIsAnsweredOnlyWhereItsAnswerHoldsTheRequestsTextWhole(t *testing.T) {
	call := func(rec, live string, resp ...Leaf) Decision {
		in := oInput("body.batch", rec, live, nil, nil)
		in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
		in.Resp = resp
		return oWord(t, Decide(in), "14", "15")
	}
	for _, c := range []struct {
		name, rec, live string
		resp            []Leaf
		echo            string
	}{
		{"a count holding a word of the request", "batch-14", "batch-15", []Leaf{oText("body.at", "seen 14 times")}, ""},
		{"a date whose minute is a word of the request", "batch-14", "batch-15", []Leaf{oText("body.at", "Thu, 22 Oct 2026 11:14:16 GMT")}, ""},
		{"a count holding the whole request", "14", "15", []Leaf{oText("body.at", "seen 14 times")}, ""},
		{"the request's text read back whole", "batch-14", "batch-15",
			[]Leaf{oText("body.at", "seen 14 times"), oText("body.batch", "batch-14")}, "body.batch#1"},
		{"the whole request read back whole", "14", "15", []Leaf{oText("body.at", "seen 14 times"), oText("body.n", "14")}, "body.n#0"},
	} {
		d := call(c.rec, c.live, c.resp...)
		var echo []string
		for _, a := range d.Echo {
			echo = append(echo, a.String())
		}
		got := strings.Join(echo, ", ")
		answered := strings.Contains(d.Line, "answered at")
		if d.Outcome != OutcomeFresh || got != c.echo || answered != (c.echo != "") || c.echo != "" && !strings.Contains(d.Line, "answered at "+c.echo+",") {
			t.Errorf("%s (%s to %s): %s, echo %q, line %q; want fresh, answered at %q only, its line naming just that",
				c.name, c.rec, c.live, oDescribe([]Decision{d}), got, d.Line, c.echo)
		}
	}
}

// A key of the call's answer that is the request's whole recorded text
// ({"A7K29QXM":{"ok":true}} answering a lookup of A7K29QXM) reads the
// request back, as a field holding it whole does; keploy cannot name a key
// to the adapter, so the word is refused, never learned fresh with that
// key served as recorded (main refuses it: http learn_test.go:2367). A key
// that only holds the request inside a longer text is served as recorded,
// as such a field is.
func TestOutcomeAnAnswerKeyThatIsTheRequestsWholeTextIsRefused(t *testing.T) {
	call := func(keys string) Decision {
		in := oInput("body.id", "A7K29QXM", "Zq81Lm0X", nil, nil)
		in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
		in.Resp = []Leaf{{Path: "body{keys}", Type: Keys, Value: keys, Unnamed: true}, {Path: "body.ok", Noise: "body.ok", Type: Bool, Value: "true"}}
		return oWord(t, Decide(in), "A7K29QXM", "Zq81Lm0X")
	}
	oRefused(t, "an answer key that is the request's whole text", call("A7K29QXM\nok"), RefusalCannotCheck, "body{keys}")
	if d := call("A7K29QXM-meta\nok"); d.Outcome != OutcomeFresh || len(d.Echo) > 0 {
		t.Errorf("an answer key holding the request inside a longer text: %s, echo %v; want fresh, served as recorded", oDescribe([]Decision{d}), d.Echo)
	}
}

// A request's number sent as text (Mongo's {"$numberInt":"3"}) that the
// call's answer holds whole as a number (a BSON timestamp's ordinal, 3)
// reads the request back, as a field of its own type does: main finds it
// whatever the two leaves' scalar types (WholeEcho; integrations' mongo
// learn_test.go TestLearn_AnAnswersTimestampOrdinalIsTheNumberItHolds).
// Learned fresh with that answer served as recorded, replay would hand the
// application the old 3 beside its new 2. Named, it is answered there; at a
// place keploy cannot name, refused. A number holding another value is no
// read-back.
func TestOutcomeAReadBackOfAnotherScalarTypeIsAnsweredThere(t *testing.T) {
	call := func(a Leaf) Decision {
		in := oInput("body.filter.retry", "3", "2", nil, nil)
		in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
		in.Resp = []Leaf{a}
		return oWord(t, Decide(in), "3", "2")
	}
	for _, typ := range []Type{Number, String} {
		d := call(Leaf{Path: "body.seen.i", Noise: "body.seen.i", Type: typ, Value: "3"})
		if d.Outcome != OutcomeFresh || len(d.Echo) != 1 || d.Echo[0].String() != "body.seen.i#0" {
			t.Errorf("a %c answer holding the request's whole text: %s, echo %v; want fresh, answered at body.seen.i#0", typ, oDescribe([]Decision{d}), d.Echo)
		}
	}
	oRefused(t, "a number read back at a place keploy cannot name", call(Leaf{Path: "body.seen.i", Type: Number, Value: "3", Unnamed: true}), RefusalCannotCheck, "body.seen.i")
	if d := call(Leaf{Path: "body.seen.i", Noise: "body.seen.i", Type: Number, Value: "33"}); d.Outcome != OutcomeFresh || len(d.Echo) > 0 {
		t.Errorf("a number holding another value: %s, echo %v; want fresh, served as recorded", oDescribe([]Decision{d}), d.Echo)
	}
}

// A fresh word's line names the earlier place it set aside as its source
// (learning legible; main's line named it): the test's earlier find answered
// "blue-17" in both runs, and the call sends "red-42" where the recording
// sent "blue-17". A copy moves with its source, so the find is no source:
// each changed word is fresh, and its line says the find held the recorded
// word and round F gave it unchanged there. With no such find the line names
// nothing set aside. (Integrations' mongo learn_test.go
// TestLearn_NoiseLearnedPastASourceSetAsideNamesItOnTheLearnLine.)
func TestOutcomeAFreshWordNamesThePlaceItSetAside(t *testing.T) {
	for _, withFind := range []bool{true, false} {
		var earlier []Earlier
		if withFind {
			earlier = oServed("mock-0", false, false, Response, oText("body.tag", "blue-17"), "blue-17", time.Millisecond)
		}
		ds := Decide(oInput("body.note", "blue-17", "red-42", earlier, nil))
		for _, w := range [][2]string{{"blue", "red"}, {"17", "42"}} {
			d := oWord(t, ds, w[0], w[1])
			named := strings.Contains(d.Line, "mock-0") && strings.Contains(d.Line, "set aside")
			if d.Outcome != OutcomeFresh || named != withFind {
				t.Errorf("the earlier find %v, %s to %s: %s, line %q; want fresh, naming mock-0 set aside: %v", withFind, w[0], w[1], oDescribe([]Decision{d}), d.Line, withFind)
			}
		}
	}
}

// A value bound at a place of the start-up is the run's, so its From says
// Run, as Earlier.Run said it to the adapter: every adapter then names that
// value as the run's variable from the decision alone. One bound at the
// call's own test's place is not.
func TestOutcomeAValueBoundAtTheStartUpIsTheRuns(t *testing.T) {
	for _, startUp := range []bool{true, false} {
		earlier := oServed("mock-0", startUp, startUp, Response, oText("body.id", "app-0b1e"), "app-77c4", time.Millisecond)
		d := oWord(t, Decide(oInput("body.user", "app-0b1e", "app-77c4", earlier, nil)), "0b1e", "77c4")
		if d.Outcome != OutcomeValue || d.From.Mock != "mock-0" || d.From.Run != startUp {
			t.Errorf("bound at the start-up %v: %s, from %+v; want a value bound to mock-0, Run %v", startUp, oDescribe([]Decision{d}), d.From, startUp)
		}
	}
}

// A variable bound at a word of a leaf is named at that word: a later answer
// that holds the leaf whole is answered at the same word, so the tracker
// tells which words of an earlier request leaf hold a variable of the test,
// with each word in both runs (a find by id answering the name the test made
// and first sent in a distinct). A word no variable holds, another test's
// variable and a live text of other words name nothing.
func TestOutcomeAVariableBoundAtAWordIsNamedAtThatWord(t *testing.T) {
	tr := NewTracker()
	tr.Begin()
	n := tr.Epoch()
	const place = "body.cmd.filter.name"
	name, ok := tr.Name(n, "mock-201", place+"#3", nil)
	if !ok {
		t.Fatal("the tracker named no variable")
	}
	seen := Seen{Side: Request, Leaf: oText(place, "[E2E] app-crud-1793224420036884439"), Live: "[E2E] app-crud-1793311234567890123", Place: place}
	want := []BoundWord{{Name: name, Word: 3, Recorded: "1793224420036884439", Live: "1793311234567890123"}}
	if got := tr.BoundWords(n, "mock-201", seen); !reflect.DeepEqual(got, want) {
		t.Fatalf("FALSE RED: the words of the first send: %+v; want %+v", got, want)
	}
	for what, got := range map[string][]BoundWord{
		"another exchange's leaf":       tr.BoundWords(n, "mock-202", seen),
		"another place":                 tr.BoundWords(n, "mock-201", Seen{Side: Request, Leaf: seen.Leaf, Live: seen.Live, Place: "body.cmd.key"}),
		"a place of its answer":         tr.BoundWords(n, "mock-201", Seen{Side: Response, Leaf: seen.Leaf, Live: seen.Live, Place: place}),
		"a live text of other words":    tr.BoundWords(n, "mock-201", Seen{Side: Request, Leaf: seen.Leaf, Live: "1793311234567890123", Place: place}),
		"a test that is not the runner": tr.BoundWords(n+1, "mock-201", seen),
	} {
		if len(got) != 0 {
			t.Errorf("%s: %+v; want no variable", what, got)
		}
	}
	tr.Begin()
	if got := tr.BoundWords(tr.Epoch(), "mock-201", seen); len(got) != 0 {
		t.Errorf("the next test: %+v; variables are named per test", got)
	}
	var none *Tracker
	if got := none.BoundWords(n, "mock-201", seen); got != nil {
		t.Errorf("no tracker: %+v", got)
	}
}

// A run's variable (a place of the start-up) keeps its word across the set.
func TestOutcomeARunVariableBoundAtAWordIsNamedInEveryTest(t *testing.T) {
	tr := NewTracker()
	tr.Begin()
	const place = "body.session"
	name, ok := tr.RunName(tr.Epoch(), "mock-1", place+"#1", nil)
	if !ok {
		t.Fatal("the tracker named no run variable")
	}
	tr.Begin()
	seen := Seen{Side: Request, Leaf: oText(place, "sess-0b1e"), Live: "sess-77c4", Place: place}
	want := []BoundWord{{Name: name, Word: 1, Recorded: "0b1e", Live: "77c4"}}
	if got := tr.BoundWords(tr.Epoch(), "mock-1", seen); !reflect.DeepEqual(got, want) {
		t.Errorf("a run's variable in a later test: %+v; want %+v", got, want)
	}
}

// A word read as an instant, copied from an earlier send, is read back as
// any Value is: a find by a name that holds a run number, whose cursor holds
// the name whole, is answered at the same word; one its adapter cannot
// answer there, or at a place keploy cannot name, is refused.
func TestOutcomeAnInstantWordCopiedFromASendIsAnsweredWhereItsAnswerHoldsItWhole(t *testing.T) {
	const rec, live = "[E2E] app-crud-1793224420036884439", "[E2E] app-crud-1793311234567890123"
	const rw, lw = "1793224420036884439", "1793311234567890123"
	src := oServed("mock-insert", false, false, Request, oText("body.documents[0].name", rec), live, time.Millisecond)
	in := func() Input {
		in := oInput("body.cmd.filter.name", rec, live, src, nil)
		in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
		in.Resp = []Leaf{oText("body.cursor.firstBatch[0].name", rec), oText("body.cursor.firstBatch[0].title", "created "+rec)}
		return in
	}
	d := oWord(t, Decide(in()), rw, lw)
	want := "body.cursor.firstBatch[0].name#3"
	if d.Outcome != OutcomeValue || len(d.Echo) != 1 || d.Echo[0].String() != want ||
		!strings.Contains(d.Line, "answered at "+want+", where the call's answer holds "+strconv.Quote(rec)+" whole as recorded") {
		t.Errorf("FALSE RED: a find by a name holding a run number: %s, echo %v, line %q; want a Value answered at %s", oDescribe([]Decision{d}), d.Echo, d.Line, want)
	}
	noWord := in()
	noWord.Caps.AnswerAtWord = false
	oRefused(t, "an instant's read-back an adapter cannot answer at its word", oWord(t, Decide(noWord), rw, lw), RefusalCannotCheck, "cannot answer a word")
	unnamed := in()
	unnamed.Resp = append(unnamed.Resp, Leaf{Path: "body.cursor.firstBatch[1].name", Type: String, Value: rec, Unnamed: true})
	oRefused(t, "an instant's read-back at a place keploy cannot name", oWord(t, Decide(unnamed), rw, lw), RefusalCannotCheck, "cannot name")
}

// A word only a recording made after the call holds decides nothing: it gave
// the recorded application nothing before the call, and round F's value does
// not stand there, so the value is made anew each run. One that round F's
// value stands in, or one recorded at the call's own instant, stays refused.
func TestOutcomeARecordedWordOnlyALaterRecordingHoldsIsFresh(t *testing.T) {
	const p = "body.msg_id"
	const rec, live = "k1b2e4d70", "k5d0e8a33"
	held := func(id string, at time.Time) Raw {
		m := receiveOf("mock-0", id, at)
		return clockedDrifts().RawOf(m, true).ServedAs(3).ServedAsRecorded()
	}
	call := func(x Raw) Decision {
		in := oInput(p, rec, live, nil, nil)
		in.At, in.Arrived = r1Insert, r1Insert
		in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x)}, nil }
		return oWord(t, Decide(in), rec, live)
	}
	if d := call(held(rec, r1Insert.Add(30*time.Second))); d.Outcome != OutcomeFresh {
		t.Errorf("FALSE RED: the recorded word in a recording made after the call: %s; want fresh", oDescribe([]Decision{d}))
	}
	oRefused(t, "round F's word in a recording made after the call", call(held(live, r1Insert.Add(30*time.Second))), RefusalUnknown, "mock-0")
	oRefused(t, "the recorded word in a recording made at the call's own instant", call(held(rec, r1Insert)), RefusalUnknown, "mock-0")
}

// The tracker's recorded begin is what an adapter fills Input.RecBegan with.
func TestOutcomeTheTrackerGivesItsTestsRecordedBegin(t *testing.T) {
	tr := NewTracker()
	began := oAt.Add(-time.Hour)
	tr.RecordedBegin(began)
	tr.Begin()
	if got := tr.RecBegan(); !got.Equal(began) {
		t.Errorf("RecBegan %v; want %v", got, began)
	}
	var none *Tracker
	if got := none.RecBegan(); !got.IsZero() {
		t.Errorf("no tracker: RecBegan %v", got)
	}
}

// oEncoded are the encodings the reader does not open as a step, each
// carrying id inside a longer value: what round F must not learn fresh when
// an earlier call shows the id in both runs.
func oEncoded() map[string]func(id string) string {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	wrap := func(e string, width int, nl string) string {
		var sb strings.Builder
		for len(e) > width {
			sb.WriteString(e[:width] + nl)
			e = e[width:]
		}
		return sb.String() + e + nl
	}
	doc := func(id string) string { return `{"note":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx","ref":"` + id + `","v":1}` }
	digest := func(s string) string {
		sum := sha1.Sum([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	return map[string]func(string) string{
		"wrapped base64 and a line of text": func(id string) string { return wrap(b64(doc(id)), 60, "\n") + "sent by worker 3\n" },
		"wrapped base64 after a key":        func(id string) string { return "data=" + wrap(b64(doc(id)), 60, "\n") },
		"indented wrapped base64":           func(id string) string { return "<blob>\n    " + wrap(b64(doc(id)), 64, "\n    ") + "</blob>" },
		"wrapped base64 with escaped CRLFs": func(id string) string { return `{"blob":"` + wrap(b64(doc(id)), 76, `\r\n`) + `"}` },
		"a signed message": func(id string) string {
			d := b64(`{"_rails":{"message":"` + id + `","exp":null,"pur":"x"}}`)
			return d + "--" + digest(d)
		},
		"padded base32": func(id string) string { return base32.StdEncoding.EncodeToString([]byte(id + "-" + id)) },
		"0x hex":        func(id string) string { return "0x" + hex.EncodeToString([]byte(id)) },
		"base64 glued between a prefix and a digest": func(id string) string {
			d := base64.RawURLEncoding.EncodeToString([]byte(doc(id)))
			return "v1_" + d + "--" + digest(d)
		},
	}
}

// oSent is an earlier call of the test that sent r, round F sending f.
func oSent(r, f string) []Earlier {
	return []Earlier{{Mock: "mock-0", At: oAt.Add(-time.Second), Own: true, Seen: []Seen{
		{Side: Request, Leaf: oText("body.user", r), Live: f, Place: "body.user"},
	}}}
}

// An id an earlier call sent, carried inside an encoding keploy cannot bind a
// word in, is the value that moved: no word of the encoded text may be
// learned fresh, or the first call would go unchecked. Refused, and the
// refusal names the place, the value and what to do.
func TestOutcomeAnIdInsideAnEncodingTheReaderDoesNotOpenIsRefused(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	for name, enc := range oEncoded() {
		t.Run(name, func(t *testing.T) {
			ds := Decide(oInput("body.blob", enc(r), enc(f), oSent(r, f), nil))
			if len(ds) == 0 {
				t.Fatal("no decision")
			}
			for _, d := range ds {
				oRefused(t, name, d, RefusalUnknown, "mock-0", r)
				if !oEdits(d.Refusal, EditReRecord, EditNoiseAtField) {
					t.Errorf("no edit to act on: %+v", d.Refusal)
				}
			}
		})
	}
}

// The base64 of a whole value, wrapped or not, is an encoding the reader
// does open: the id binds to its place inside it.
func TestOutcomeAnIdInsideWholeWrappedBase64Binds(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	enc := func(id string) string {
		e := base64.StdEncoding.EncodeToString([]byte(`{"note":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx","ref":"` + id + `","v":1}`))
		return e[:60] + "\n" + e[60:] + "\n"
	}
	ds := Decide(oInput("body.blob", enc(r), enc(f), oSent(r, f), nil))
	if d := oWord(t, ds, r, f); d.Outcome != OutcomeValue || d.From.Mock != "mock-0" {
		t.Fatalf("want a value of mock-0: %s", oDescribe(ds))
	}
}

// The same value with nothing earlier to explain it stays what it is: a
// value made anew each run, checked at every later use.
func TestOutcomeAnEncodedValueNoPlaceHoldsTheWordsOfStaysFresh(t *testing.T) {
	for name, enc := range oEncoded() {
		t.Run(name, func(t *testing.T) {
			ds := Decide(oInput("body.blob", enc("rAHcEraS7Qx1"), enc("Zq81Lm0XpVb4"), oSent("unrelated", "unrelated"), nil))
			for _, d := range ds {
				if d.Outcome != OutcomeFresh {
					t.Errorf("want fresh: %s", oDescribe(ds))
				}
			}
		})
	}
}

// Only a value of 32 bits or more says anything: a shorter one in both runs
// is chance, as at any other place (rule B).
func TestOutcomeAShortIdInsideAnEncodingIsChance(t *testing.T) {
	enc := oEncoded()["a signed message"]
	const r, f = "a1b2c3", "d4e5f6" // 24 bits
	ds := Decide(oInput("body.blob", enc(r), enc(f), oSent(r, f), nil))
	for _, d := range ds {
		if d.Outcome != OutcomeFresh {
			t.Errorf("want fresh: %s", oDescribe(ds))
		}
	}
	const r32, f32 = "a1b2c3d4", "e5f6a7b8" // 32 bits, the bound
	ds = Decide(oInput("body.blob", enc(r32), enc(f32), oSent(r32, f32), nil))
	for _, d := range ds {
		oRefused(t, "32 bits", d, RefusalUnknown, "mock-0")
	}
}

// An id another test sent is out of the call's scope: this rule reads only
// the call's scope, and the encoded value's words stay fresh, as they do
// with no place at all.
func TestOutcomeAnIdAnotherTestSentInsideAnEncodingIsNotThisRule(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	enc := oEncoded()["a signed message"]
	sent := oSent(r, f)
	sent[0].Own, sent[0].Run = false, true
	ds := Decide(oInput("body.blob", enc(r), enc(f), sent, nil))
	for _, d := range ds {
		if d.Outcome != OutcomeFresh {
			t.Errorf("want fresh: %s", oDescribe(ds))
		}
	}
}

// A place that kept its value says nothing: the id did not move there, so
// the encoded value's change is not its copy.
func TestOutcomeAnIdThatStayedAtItsPlaceSaysNothingOfAnEncodedValue(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	enc := oEncoded()["a signed message"]
	ds := Decide(oInput("body.blob", enc(r), enc(f), oSent(r, r), nil))
	for _, d := range ds {
		if d.Outcome != OutcomeFresh {
			t.Errorf("want fresh: %s", oDescribe(ds))
		}
	}
}

// An encoding the reader does open keeps its word-level answer: the id binds
// to its place, and the fresh nonce beside it stays fresh, the id in the
// value's decoded form being read there, not hidden.
func TestOutcomeAWordOfAnOpenedEncodingKeepsItsAnswerBesideAnId(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	const rn, fn = "Hq6Ys0Bc5DkW", "Tn2Pj9Vm3LsX"
	enc := func(id, nonce string) string {
		return base64.StdEncoding.EncodeToString([]byte("id=" + id + " nonce=" + nonce))
	}
	ds := Decide(oInput("body.blob", enc(r, rn), enc(f, fn), oSent(r, f), nil))
	if d := oWord(t, ds, r, f); d.Outcome != OutcomeValue {
		t.Fatalf("the id: %s", oDescribe(ds))
	}
	if d := oWord(t, ds, rn, fn); d.Outcome != OutcomeFresh {
		t.Fatalf("the nonce: %s", oDescribe(ds))
	}
}

// A separator that changed beside a changed word, inside one whole token
// ("a-17" to "a_42"). Replay writes the word into the recorded text and
// compares the text whole (Address.ReplaceWord, as an adapter's replay
// does), so the recorded separator stands, the application's request differs
// and replay goes red. Whichever way the word is learned, the rendered
// request must never equal what the application sent: that would be a green
// that checked nothing of the change.
func TestOutcomeASeparatorChangedBesideAWordGoesRedAtReplay(t *testing.T) {
	for _, tc := range []struct {
		name, rec, live, rendered string
		earlier                   []Earlier
	}{
		{name: "fresh", rec: "a-17", live: "a_42", rendered: "a-42"},
		{name: "bound", rec: "a-17", live: "a_42", rendered: "a-42", earlier: oSent("17", "42")},
		{name: "fresh, the separator after the word", rec: "order-17_x", live: "order-42-x", rendered: "order-42_x"},
		{name: "fresh, every word of a token", rec: "Sfl-KxwRJ_SMe", live: "Q1x_Yp-9vTq", rendered: "Q1x-Yp_9vTq"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := Decide(oInput("body.ref", tc.rec, tc.live, tc.earlier, nil))
			text := tc.rec
			for _, d := range ds {
				if d.Outcome == OutcomeRefused {
					t.Fatalf("want it learned, its red pinned here: %s", oDescribe(ds))
				}
				out, ok := d.At.ReplaceWord(text, d.Full)
				if !ok {
					t.Fatalf("replay cannot write %q at %+v in %q", d.Full, d.At, text)
				}
				text = out
			}
			if text == tc.live {
				t.Fatalf("FALSE GREEN: replay renders the changed separator: %s", oDescribe(ds))
			}
			if text != tc.rendered {
				t.Fatalf("rendered %q, want %q: %s", text, tc.rendered, oDescribe(ds))
			}
		})
	}
}

// A secret's field prints neither value of an id its decoded form carries,
// though the line quotes words the field's own value does not hold as
// written.
func TestOutcomeAnIdInsideAnEncodedSecretIsNotPrinted(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	enc := oEncoded()["a signed message"]
	d := Decide(oInput("body.token", enc(r), enc(f), oSent(r, f), nil))[0]
	oRefused(t, "an encoded secret", d, RefusalUnknown, "mock-0")
	if strings.Contains(d.Line, r) || strings.Contains(d.Line, f) {
		t.Fatalf("the line prints a secret's value: %s", d.Line)
	}
}
