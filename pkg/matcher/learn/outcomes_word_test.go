package learn

import (
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"reflect"
	"slices"
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
	return RawOf(oPgMock(name, before, sql, rows, tag), false)
}

// oPgMock is oPg's recording.
func oPgMock(name string, before time.Duration, sql string, rows []models.PostgresV3Cells, tag string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: oAt.Add(-before),
		ResTimestampMock: oAt.Add(-before + time.Millisecond),
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: sql,
			Response:      &models.PostgresV3Response{Rows: rows, CommandComplete: tag},
		}},
	}}
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

// A conflict's give edit names the value as the line names its place: a
// part of the url by its whole path (url.path.2, url.query.order), as a
// path segment's index alone ("2") names nothing a person reads; a body or
// header field by its last key, and so is another adapter's field whose
// name only begins with "url". A word and an instant are named alike.
func TestOutcomeAGiveEditNamesAUrlPartByItsWholePath(t *testing.T) {
	gives := func(what string, d Decision, name string) {
		t.Helper()
		give := `keploytest.Given(t, ` + strconv.Quote(name) + `, v)`
		if d.Refusal == nil || !slices.Contains(d.Refusal.Edits, Edit{Kind: EditGive, Name: name}) || !strings.Contains(d.Line, give) {
			t.Errorf("%s: %s %q; want its give edit to say %s", what, oDescribe([]Decision{d}), d.Line, give)
		}
	}
	sent := oServed("mock-0", false, false, Request, oText("body.order", "o-1"), "o-9", time.Millisecond)
	for _, c := range [][2]string{{"url.path.2", "url.path.2"}, {"url.query.order", "url.query.order"}, {"body.ref", "ref"}, {"header.X-Order", "X-Order"}, {"urls.ref", "ref"}} {
		d := oWord(t, Decide(oInput(c[0], "o-1", "o-5", sent, nil)), "1", "5")
		oRefused(t, "a word at "+c[0], d, RefusalConflict, "mock-0")
		gives("a word at "+c[0], d, c[1])
	}
	// A start-up recording served as recorded sent the recorded moment in
	// round F too (TestOutcomeAMomentARecordingsRequestHoldsIsASend).
	rAt := oAt.Add(2*time.Hour + 400*time.Millisecond + 123*time.Microsecond)
	fAt := rAt.Add(time.Hour + 37*time.Second + 211*time.Millisecond + 7*time.Microsecond)
	rec, live := iMicro(rAt.Add(-time.Second)), iMicro(fAt.Add(-time.Second))
	raw := []Raw{RawOf(sentAtMock("mock-5", rec, "", rAt.Add(-time.Second), rAt.Add(-900*time.Millisecond)), true).ServedAs(1).ServedAsRecorded()}
	for _, c := range [][2]string{{"url.path.3", "url.path.3"}, {"body.at", "at"}} {
		d := oWord(t, Decide(iCall(c[0], String, rec, live, rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil, raw)), rec, live)
		oRefused(t, "an instant at "+c[0], d, RefusalConflict, "mock-5")
		gives("an instant at "+c[0], d, c[1])
	}
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
	if !oEdits(d.Refusal, "move-to-mocks", "move-to-startup", "noise-at-field") {
		t.Errorf("another scope's edits %+v; want move to mocks:, to startup:, or noise at its field", d.Refusal)
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

// A word only a recording answered after the call holds decides nothing: it
// gave the recorded application nothing before the call, and round F's value
// does not stand there, so the value is made anew each run, however its
// request was timed (one asked at the call's own instant too). One that
// round F's value stands in, or one answered at the call's own instant,
// stays refused.
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
	if d := call(held(rec, r1Insert)); d.Outcome != OutcomeFresh {
		t.Errorf("FALSE RED: the recorded word in a recording asked at the call's own instant and answered a second later: %s; want fresh", oDescribe([]Decision{d}))
	}
	at := held(rec, r1Insert)
	at.answered = r1Insert
	oRefused(t, "the recorded word in a recording answered at the call's own instant", call(at), RefusalUnknown, "mock-0", atCallWhy)
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

// oSent is an earlier call of the test that sent r, round F sending f, with
// its exchange's payload (oPayload).
func oSent(r, f string) []Earlier {
	return []Earlier{{Mock: "mock-0", At: oAt.Add(-time.Second), Own: true, Payload: oPayload("mock-0", oAt.Add(-time.Second)), Seen: []Seen{
		{Side: Request, Leaf: oText("body.user", r), Live: f, Place: "body.user"},
	}}}
}

// oPayload is the payload of an exchange recorded at at and answered a
// millisecond later that F served leaf by leaf, as its adapter reads it
// (RawOf), holding no word of its own: keploy refuses what no place binds
// beside an exchange whose payload it was not given (payloadless).
func oPayload(mock string, at time.Time) Raw {
	return RawOf(&models.Mock{Name: mock, Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}, true)
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

// An exchange F served leaf by leaf before the call whose payload keploy was
// not given, or could not read, is a place whose answer cannot be searched:
// a value no place binds (a word made anew, a read of the clock) may have
// been built on it, so it is refused, naming the exchange, as main refuses it
// (payloadUnread: offset.go, the offset and word rules). That holds for the
// call's own test, the pool's mocks and the start-up. What a place binds
// stands, as main binds it: a value its leaf or another recording's answer
// holds, and a read of the clock beside a send of the same coarse moment.
// An earlier test's place keeps no payload by design, and the test case's
// own request is no exchange: beside either the word stays fresh. Where one
// value stands at two fields, neither binds to the other.
func TestOutcomeAValueNoPlaceBindsBesideAnExchangeWithNoPayloadIsRefused(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	sent := func(a, b string, mod func(*Earlier)) []Earlier {
		e := Earlier{Mock: "mock-0", At: oAt.Add(-time.Second), Own: true, Seen: []Seen{
			{Side: Request, Leaf: oText("body.user", a), Live: b, Place: "body.user"}}}
		mod(&e)
		return []Earlier{e}
	}
	inst, instF := iMs(oAt.Add(-500*time.Millisecond)), iMs(oAt.Add(-300*time.Millisecond))
	clockAt := func(e []Earlier, raw []Raw) Input {
		return iCall("body.t", String, inst, instF, oAt, oAt, iWin(oAt, oAt, time.Second), e, raw)
	}
	twice := func(e []Earlier) Input {
		in := oInput("body.id", r, f, e, nil)
		in.Drift = append(in.Drift, "body.id2")
		in.Live.Leaves["body.id2"], in.Rec.Leaves["body.id2"] = oText("body.id2", f), oText("body.id2", r)
		return in
	}
	for _, sh := range []struct {
		what, why string
		mod       func(*Earlier)
	}{
		{"the call's own test", "keploy was given no payload of mock-0", func(*Earlier) {}},
		{"the pool's", "keploy was given no payload of mock-0", func(e *Earlier) { e.Own = false }},
		{"the start-up's", "keploy was given no payload of mock-0", func(e *Earlier) { e.Own, e.Run, e.StartUp = false, true, true }},
		{"a payload keploy could not read", "keploy could not read mock-0", func(e *Earlier) { e.Payload = Raw{mock: "mock-0", unread: "its kind is unknown"} }},
	} {
		for _, d := range Decide(oInput("body.id", r, f, sent("unrelated", "unrelated", sh.mod), nil)) {
			oRefused(t, sh.what+": a word made anew", d, RefusalUnread, "mock-0", sh.why)
		}
		for _, d := range Decide(twice(sent("unrelated", "unrelated", sh.mod))) {
			oRefused(t, sh.what+": one new value at two fields", d, RefusalUnread, "mock-0")
		}
		oRefused(t, sh.what+": a read of the clock", oWord(t, Decide(clockAt(sent("unrelated", "unrelated", sh.mod), nil)), inst, instF), RefusalUnread, "mock-0", sh.why)
		if d := oWord(t, Decide(oInput("body.id", r, f, sent(r, f, sh.mod), nil)), r, f); d.Outcome != OutcomeValue || d.From.Mock != "mock-0" {
			t.Errorf("%s: the leaf holds the value: %s; want a value of mock-0", sh.what, oDescribe([]Decision{d}))
		}
		raw := oAnswer("mock-5", 2*time.Second, inst).ServedAs(1).ServedLive(oAnswer("mock-5", 2*time.Second, instF))
		if d := oWord(t, Decide(clockAt(sent("unrelated", "unrelated", sh.mod), []Raw{raw})), inst, instF); d.Outcome != OutcomeValue || d.From.Mock != "mock-5" {
			t.Errorf("%s: an answer holds the moment: %s; want a value of mock-5", sh.what, oDescribe([]Decision{d}))
		}
		if d := oWord(t, Decide(clockAt(sent(inst, instF, sh.mod), nil)), inst, instF); d.Outcome != OutcomeClock {
			t.Errorf("%s: a send of the same moment: %s; want a Clock", sh.what, oDescribe([]Decision{d}))
		}
	}
	for _, sh := range []struct {
		what string
		mod  func(*Earlier)
	}{
		{"an earlier test's", func(e *Earlier) { e.Own, e.Run = false, true }},
		{"the test case's own request", func(e *Earlier) { e.Own, e.Given = false, true }},
		{"a payload keploy read", func(e *Earlier) { e.Payload = oPayload("mock-0", e.At) }},
	} {
		for _, d := range Decide(twice(sent("unrelated", "unrelated", sh.mod))) {
			if d.Outcome == OutcomeRefused {
				t.Errorf("%s: %s; want fresh, then its other use", sh.what, oDescribe([]Decision{d}))
			}
		}
		costFresh(t, sh.what+": a word made anew", oWord(t, Decide(oInput("body.id", r, f, sent("unrelated", "unrelated", sh.mod), nil)), r, f))
		iClock(t, sh.what+": a read of the clock", oWord(t, Decide(clockAt(sent("unrelated", "unrelated", sh.mod), nil)), inst, instF))
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

// An id another test sent, inside an encoding, is refused as the plain word
// is there: keploy binds only within a test and the start-up, and the line
// names the mappings edit.
func TestOutcomeAnIdAnotherTestSentInsideAnEncodingIsRefusedNamingTheEdit(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	enc := oEncoded()["a signed message"]
	sent := oSent(r, f)
	sent[0].Own, sent[0].Run = false, true
	ds := Decide(oInput("body.blob", enc(r), enc(f), sent, nil))
	if len(ds) == 0 {
		t.Fatal("no decision")
	}
	for _, d := range ds {
		oRefused(t, "another test's id in an encoding", d, RefusalAnotherScope, "mock-0", r)
		if !oEdits(d.Refusal, EditMoveToMocks, EditMoveToStartup, EditNoiseAtField) {
			t.Errorf("no mappings edit to act on: %+v", d.Refusal)
		}
	}
}

// A place that kept the id in both runs refuses the encoded value, as main
// refuses it (unaccounted: the place did not drift with the value): keploy
// binds no word in a signed message, so it cannot tell whether the id came
// from there. So does one that kept round F's id.
func TestOutcomeAnIdThatStayedAtItsPlaceRefusesAnEncodedValue(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	enc := oEncoded()["a signed message"]
	for _, kept := range []string{r, f} {
		oNoneFresh(t, "FALSE GREEN? a place kept "+kept, Decide(oInput("body.blob", enc(r), enc(f), oSent(kept, kept), nil)), RefusalUnknown, "mock-0")
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
		{name: "fresh, every word of a token too short to read as one value", rec: "Sfl-KxwRJ_SMe", live: "Q1x_Yp-9vTq", rendered: "Q1x-Yp_9vTq"},
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

// A recording served as recorded before the call holds round F's new word
// and not the recorded one: a conflict, whose line says what stood there.
// It must not say the recorded word stood whole at a recording it never was
// in.
func TestOutcomeAConflictNamesTheWordThatStoodAtTheRecording(t *testing.T) {
	const r, f = "fq9915ab", "kd7730cd"
	x := oAnswer("mock-31", 2*time.Second, f).ServedAs(3).ServedAsRecorded().OfAnotherTest()
	in := oInput("body.ref", r, f, nil, nil)
	in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x)}, nil }
	ds := Decide(in)
	d := oWord(t, ds, r, f)
	oRefused(t, "F's word at a recording served as recorded", d, RefusalConflict, "mock-31", f)
	if d.Refusal != nil && strings.Contains(d.Refusal.Why, strconv.Quote(r)+" stood whole") {
		t.Fatalf("the line says %q stood at a recording that never held it: %s", r, d.Refusal.Why)
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

// oCarried is an earlier call of the test that sent r at field, round F
// sending f there, its request recorded before the call and answered after
// answered, served at round F's serve first.
func oCarried(mock, field, r, f string, before, answered time.Duration, first int) Earlier {
	m := &models.Mock{Name: mock, Spec: models.MockSpec{ReqTimestampMock: oAt.Add(-before), ResTimestampMock: oAt.Add(-before + answered)}}
	return Earlier{Mock: mock, At: oAt.Add(-before), Own: true, Payload: RawOf(m, true).ServedAs(first),
		Seen: []Seen{{Side: Request, Leaf: oText(field, r), Live: f, Place: field}}}
}

// Serve order tells which candidate the application took a word from, and
// that decides only where no place shows the pair. A place before the call
// that holds the recorded word in the recording and round F's word in round
// F, at one field, proves the link, in whatever order the others came: an
// id the application sent on two calls that were in flight together, or
// that round F served in another order, is bound at the earliest, as main
// binds it. A candidate that kept the recorded word changes nothing: it is
// not where F's word came from, and a wrong bind goes red at replay, which
// checks the bound value at every use. With no place showing the pair the
// order still decides, and the word stays refused
// (TestOutcomeAWordCopiedFromAnswersGivenInAnotherOrderIsUnknown).
func TestOutcomeAWordOnePlaceShowsIsBoundWhateverTheOrder(t *testing.T) {
	const r, f = "78b0ada8", "46a13c03"
	first := oCarried("mock-1", "body.docs.0.appId", r, f, 2*time.Second, time.Second, 1)
	second := oCarried("mock-2", "body.filter.appId", r, f, 1500*time.Millisecond, 100*time.Millisecond, 2)
	swapped := oCarried("mock-2", "body.filter.appId", r, f, 1500*time.Millisecond, 100*time.Millisecond, 1)
	answeredFirst := oCarried("mock-1", "body.docs.0.appId", r, f, 2*time.Second, 100*time.Millisecond, 2)
	again := first
	again.Payload = first.Payload.ServedIn(1, 3)
	kept := oCarried("mock-3", "body.note", r, r, 1800*time.Millisecond, 100*time.Millisecond, 3)
	for _, tc := range []struct {
		name    string
		earlier []Earlier
	}{
		{"their requests were in flight together", []Earlier{first, second}},
		{"round F served them in another order", []Earlier{answeredFirst, swapped}},
		{"round F served one of them twice", []Earlier{again, second}},
		{"a candidate kept the recorded word, in flight with the others", []Earlier{first, second, kept}},
		{"a candidate kept the recorded word, round F served them in another order", []Earlier{answeredFirst, swapped, kept}},
	} {
		d := oWord(t, Decide(oInput("body.cmd.filter.appId", "id "+r, "id "+f, tc.earlier, nil)), r, f)
		if d.Outcome != OutcomeValue || d.From.Mock != tc.earlier[0].Mock {
			t.Errorf("%s: %s; want a Value bound at the earliest place", tc.name, oDescribe([]Decision{d}))
		}
	}
	// No place shows the pair: which candidate the application had last
	// is all that could tie the word to one, and keploy cannot tell it.
	d := oWord(t, Decide(oInput("body.cmd.filter.appId", "id "+r, "id "+f, []Earlier{kept, oCarried("mock-4", "body.note", r, r, 1800*time.Millisecond, 100*time.Millisecond, 3)}, nil)), r, f)
	oRefused(t, "only candidates that kept the recorded word, in flight together", d, RefusalUnknown, "at one instant")
}

// A fresh UUID's 16-bit group, beside its 32-bit group that changed too,
// where an unchanged place holds both runs' groups: short hex stands all
// over a set, so the place says nothing of a piece of one value, and the
// word is fresh. The value's 32-bit and 48-bit groups still judge the place.
// A place that holds the whole recorded id is a conflict. A word that is the
// field's whole value is no piece: a request that lists both stays unknown
// (TestOutcomeCostAnUnchangedPlaceHoldingOneShortWordIsChance).
func TestOutcomeAPieceOfAnIdAtAnUnchangedPlaceHoldingBothWordsIsChance(t *testing.T) {
	const (
		rec, live = "a1b2c3d4-08c8-4e5f-9a0b-c1d2e3f40516", "e5f60718-b054-4c6d-8e9f-a0b1c2d3e4f5"
		r, f      = "08c8", "b054"
	)
	holds := func(mock, text string) Earlier {
		return oCarried(mock, "body.note", text, text, 2*time.Second, time.Second, 1)
	}
	both := []Earlier{holds("mock-45", "pool 08c8 and b054 ready")}
	d := oWord(t, Decide(oInput("body._id", rec, live, both, nil)), r, f)
	if d.Outcome != OutcomeFresh {
		t.Errorf("a 16-bit group at an unchanged place holding both: %s; want fresh", oDescribe([]Decision{d}))
	}
	if !strings.Contains(d.Line, "mock-45") {
		t.Errorf("a 16-bit group set aside at mock-45: the line must name it: %q", d.Line)
	}
	const r8, f8 = "a1b2c3d4", "e5f60718"
	d = oWord(t, Decide(oInput("body._id", rec, live, []Earlier{holds("mock-46", "pool a1b2c3d4 and e5f60718 ready")}, nil)), r8, f8)
	oRefused(t, "the 32-bit group at an unchanged place holding both", d, RefusalUnknown, "mock-46")
	d = oWord(t, Decide(oInput("body._id", rec, live, []Earlier{holds("mock-47", "owner "+rec+" and b054")}, nil)), r, f)
	oRefused(t, "the whole recorded id at an unchanged place that holds the group too", d, RefusalConflict, "mock-47")
	d = oWord(t, Decide(oInput("body.slot", "08c8", "b054", both, nil)), r, f)
	oRefused(t, "a word that is the field's whole value", d, RefusalUnknown, "mock-45")
	// The same at a place of the start-up, sent unchanged in both runs.
	for _, side := range []Side{Request, Response} {
		boot := func(mock, text string) []Earlier {
			return oServed(mock, true, true, side, oText("body.note", text), text, time.Second)
		}
		d = oWord(t, Decide(oInput("body._id", rec, live, boot("mock-48", "pool 08c8 and b054 ready"), nil)), r, f)
		if d.Outcome != OutcomeFresh {
			t.Errorf("a 16-bit group at an unchanged start-up %v holding both: %s; want fresh", side, oDescribe([]Decision{d}))
		}
		d = oWord(t, Decide(oInput("body._id", rec, live, boot("mock-49", "pool a1b2c3d4 and e5f60718 ready"), nil)), r8, f8)
		oRefused(t, "the 32-bit group at an unchanged start-up place holding both", d, RefusalUnknown, "mock-49")
	}
}

// A field that is one URL-safe base64 token in each run, whose '-' and '_'
// stand elsewhere in round F: those are letters of the token, so its words
// do not pair, and word by word replay could never write it. It is one value:
// fresh at its first send, and bound where an earlier place shows it whole.
// A short id whose separator changed is no such token, and a word an earlier
// place holds may be carried inside it: the word rules read those.
func TestOutcomeAWholeURLSafeTokenIsOneValue(t *testing.T) {
	const (
		same  = "Sfl-KxwRJ_SMeKKF2QT4fwpMeJf36POk6yJV_adQssw"
		other = "Q1x_Yp-9vTq3LmN0Rr4sWkHd2cJ7aBfE5gUoZ_iXyPq"
		rec   = "kepreq_9fsSK78UnjdjTSFj51vJvm_CkQdnzdp6aRCUCH0PP5g"
		live  = "kepreq_WWEEXXKJO_kzw3aZ8Wxfl5wsJUE3V7QGpBKzyTlYA_0"
	)
	for _, tc := range []struct{ name, r, f string }{
		{"as many words in each run", same, other},
		{"a word more in round F, beside a kept prefix", rec, live},
	} {
		ds := Decide(oInput("body.docs.msgs.0._id", tc.r, tc.f, nil, nil))
		if len(ds) != 1 || ds[0].Outcome != OutcomeFresh || ds[0].At.Word != -1 || ds[0].Recorded != tc.r || ds[0].Full != tc.f {
			t.Errorf("%s: %s; want one fresh value, the field whole", tc.name, oDescribe(ds))
		}
	}
	d := Decide(oInput("body.cmd.query._id", rec, live, oSent(rec, live), nil))
	if len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != "mock-0" {
		t.Errorf("a later send: %s; want a Value bound at the first send", oDescribe(d))
	}
	if ds := Decide(oInput("body.ref", "9fsSK78UnjdjTSFj_x", "WWEEXXKJOkzw3aZ8_x-y", oSent("9fsSK78UnjdjTSFj", "WWEEXXKJOkzw3aZ8"), nil)); len(ds) == 0 || ds[0].Outcome != OutcomeRefused {
		t.Errorf("an id an earlier call sent, carried inside the token: %s; want refused", oDescribe(ds))
	}
	if ds := Decide(oInput("body.ref", "a-17", "a_42", nil, nil)); len(ds) != 1 || ds[0].At.Word < 0 {
		t.Errorf("a short id: %s; want its word, not the field whole", oDescribe(ds))
	}
}

// A field of the URL-safe alphabet is one token only where it reads as one
// random value in both runs. Text keploy can name is none: a word beside a
// UUID (one length in R, another in F), a UUID's hex groups reordered, and a
// base64 text that decodes to a name of a script keploy does not split into
// words, which may be an earlier value carried inside it.
func TestOutcomeAFieldKeployCanNameIsNoToken(t *testing.T) {
	const u1, u2 = "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5", "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812"
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct{ name, r, f string }{
		{"a word before a UUID", "Thu-" + u1, u2},
		{"a word after a UUID", u1 + "-Thu", u2},
		{"F's value has the word", u1, u2 + "-Fri"},
		{"a UUID's groups reordered", "48d1-0c1d2e3f-4a5b", "0c1d2e3f-4a5b-48d1"},
		{"a name of another script in base64 JSON", b64(`{"name":"Иван Петров"}`), b64(`{"name":"Пётр Сидоров"}`)},
		{"the same behind a kept prefix", "st_" + b64(`{"name":"Иван Петров"}`), "st_" + b64(`{"name":"Пётр Сидоров"}`)},
	} {
		ds := Decide(oInput("body.appId", tc.r, tc.f, nil, nil))
		if slices.ContainsFunc(ds, func(d Decision) bool { return d.Outcome == OutcomeFresh && d.At.Word < 0 }) {
			t.Errorf("FALSE GREEN? %s: %s; want no one fresh value", tc.name, oDescribe(ds))
		}
		if len(ds) == 0 || ds[0].Outcome != OutcomeRefused {
			t.Errorf("%s: %s; want refused", tc.name, oDescribe(ds))
		}
	}
}

// Two places that both show a word, whose requests were in flight together,
// are each a source, and either binds the same value: a place before the
// call that holds the recorded word in the recording and round F's word in
// round F, at one field, proves the link, so the order of their answers,
// which keploy cannot tell, is never asked. The word is bound at the
// earliest, a copy, never refused. This is no false green: the two carry
// the same value in both runs, and replay checks the bound value at every
// use, so a replay whose two places part goes red. A group of an id (a
// 16-bit piece of a UUID the call carries whole) reads alike. Where one of
// the two keeps the recorded word, it is no source and the other binds;
// where neither shows the pair at one field (round F gave it at another
// field), their order is all that could tie the word to one and it is
// refused.
func TestOutcomeAWordTwoRequestsInFlightTogetherBothShowIsACopy(t *testing.T) {
	const r, f = "6f1c2a9e-77b2-4303-9d1e-3c5a7b9e0f12", "0b8e4d21-1a3f-4d9c-8e2b-5d7f9a1c3e64"
	insert := oCarried("mock-1497", "body.docs.0.appId", r, f, 3*time.Second, 200*time.Millisecond, 1)
	// The two finds: mock-1500's request was recorded before mock-1499 was
	// answered, on another connection.
	find := oCarried("mock-1499", "body.filter.appId", r, f, 2*time.Second, 300*time.Millisecond, 2)
	other := oCarried("mock-1500", "body.filter.appId", r, f, 1900*time.Millisecond, 300*time.Millisecond, 3)
	call := func(earlier ...Earlier) []Decision {
		return Decide(oInput("body.cmd.documents.0._id", r, f, earlier, nil))
	}
	for _, tc := range []struct {
		name    string
		earlier []Earlier
		from    string
	}{
		{"the two finds alone", []Earlier{find, other}, "mock-1499"},
		{"the insert before the two finds", []Earlier{insert, find, other}, "mock-1497"},
		{"one find kept the recorded id", []Earlier{find, oCarried("mock-1500", "body.filter.appId", r, r, 1900*time.Millisecond, 300*time.Millisecond, 3)}, "mock-1499"},
	} {
		ds := call(tc.earlier...)
		for _, w := range [][2]string{{"6f1c2a9e", "0b8e4d21"}, {"4303", "4d9c"}} {
			if d := oWord(t, ds, w[0], w[1]); d.Outcome != OutcomeValue || d.From.Mock != tc.from {
				t.Errorf("%s, word %q: %s; want a Value bound at %s", tc.name, w[0], oDescribe([]Decision{d}), tc.from)
			}
		}
	}
	moved := func(e Earlier, field string) Earlier {
		e.Seen = []Seen{{Side: Request, Leaf: oText(e.Seen[0].Place, r), Live: "", Place: e.Seen[0].Place}, {Side: Request, Leaf: oText(field, "none"), Live: f, Place: field}}
		return e
	}
	d := oWord(t, call(moved(find, "body.filter.ownerId"), moved(other, "body.filter.ownerId")), "4303", "4d9c")
	oRefused(t, "neither find shows the pair at one field", d, RefusalUnknown, "mock-1499", "in flight together")
}

// No excuse from a place served only in an older test (a founder ruling,
// 2026-11-02): a place round F gave the application only in an older test
// of the set than the call's (Raw.ServedInAnOlderTest) shows nothing of
// what the application did with the value in the tests since, so a word
// held there where F gave another text, too little to be a conflict, is
// not set aside as a copy that moved with its source: the value is
// refused as unknown, naming the place and why (fail closed). The same
// place in the call's own test still sets the word aside, and the value is
// fresh (a place served before the test: the row after this). Flyway's
// execution_time 50 beside VARCHAR(50) F sent unchanged; 82 at record and
// 1 in F beside SELECT 1; "slot 83" beside an answer of 7 and a later
// answer of 82 (older_test_word_test.go, late_call_older_serve_test.go).
// The rule covers whole words: a 4-hex piece of a fresh UUID beside
// another UUID's answer that holds that piece by chance, F's view of it
// read with no piece of F's new UUID, is still chance there, and the
// UUID is fresh either way.
func TestOutcomeAPlaceServedOnlyInAnOlderTestSetsNoWordAside(t *testing.T) {
	older := func(raw []Raw) []Raw {
		out := make([]Raw, len(raw))
		for i, x := range raw {
			out[i] = x.ServedInAnOlderTest()
		}
		return out
	}
	b := eAnswer("mock-b", 200*time.Millisecond, "82").ServedAs(2).ServedAsRecorded()
	a := eAnswer("mock-a", 300*time.Millisecond, "7").ServedAs(1).ServedAsRecorded()
	history := append(eStartup(), eHistory(eRecAt.Add(-150*time.Millisecond), 6, true))
	for _, tc := range []struct {
		what, rec, live string
		own, old        []Raw
		mock            string
	}{
		{"50 beside VARCHAR(50) F sent unchanged", "50", "7", history, older(history), "mock-5"},
		{"82 at record, 1 in F, beside SELECT 1", "82", "1", eStartup(), older(eStartup()), "mock-1"},
		{"slot 83 beside an answer of 7 served as recorded", "slot 83", "slot 7", []Raw{a, b}, []Raw{a.ServedInAnOlderTest().ServedBeforeTheTest(), b}, "mock-a"},
	} {
		r, f := strings.Fields(tc.rec), strings.Fields(tc.live)
		rw, fw := r[len(r)-1], f[len(f)-1]
		costFresh(t, tc.what+", in no older test", oWord(t, Decide(costF(tc.rec, tc.live, nil, tc.own, nil)), rw, fw))
		d := oWord(t, Decide(costF(tc.rec, tc.live, nil, tc.old, nil)), rw, fw)
		oRefused(t, tc.what+", F gave it only in an older test", d, RefusalUnknown, tc.mock)
		if d.Refusal != nil && !strings.Contains(d.Refusal.Why, "only in an older test of the set, and what it gave there shows nothing of what the application did with the value in the tests since") {
			t.Errorf("%s, F gave it only in an older test: refused %q; want it to say why", tc.what, d.Refusal.Why)
		}
	}
	rec, live := "1111aaaa-2222-3333-48d1-444455556666", "9999cccc-7777-8888-dddd-000011112222"
	u := eAnswer("mock-u", 300*time.Millisecond, "bb7ed9ee-670e-48d1-982f-b54b2b73982f").ServedAs(1)
	u = u.ServedLive(eAnswer("mock-u", 300*time.Millisecond, "0c1d2e3f-4a5b-6c7d-8e9f-a0b1c2d3e4f5"))
	for _, x := range []Raw{u, u.ServedInAnOlderTest()} {
		costFresh(t, "a UUID's 4-hex piece beside another UUID's answer, in an older test: "+strconv.FormatBool(x.InAnOlderTest()), oWord(t, Decide(costF(rec, live, nil, []Raw{x}, nil)), "48d1", "dddd"))
	}
}

// No excuse from a place served only before the call's test began, as main's
// late-call rule reads one (outsideTest, Earlier.outsideTest): a recording
// F gave the application only at the set's start-up or between tests
// (Raw.BeforeTheTest), an exchange F served only before the test
// (Earlier.beforeTest). A word held there where F gave another text, too
// little to be a conflict, is not set aside: the application may have kept
// what it was given there, so the value is refused as unknown, naming the
// place and why. Main
// refuses each (late_call_older_serve_test.go); where the recorded value
// took no word of another place ("slot 83"), main learns noise, and this
// refuses it too (fail closed). The same place in the call's own test, or
// the start-up exchange read at the start-up, still sets the word aside,
// and the value is fresh.
func TestOutcomeAPlaceServedOnlyBeforeTheTestSetsNoWordAside(t *testing.T) {
	b := eAnswer("mock-b", 200*time.Millisecond, "82").ServedAs(2).ServedAsRecorded()
	a := eAnswer("mock-a", 300*time.Millisecond, "7").ServedAs(1).ServedAsRecorded()
	startUp := Earlier{Mock: "mock-s", At: eRecAt.Add(-300 * time.Millisecond), Run: true, StartUp: true, beforeTest: true,
		Payload: eAnswer("mock-s", 300*time.Millisecond, "7").ServedAs(1),
		Seen:    []Seen{{Side: Response, Leaf: Leaf{Path: "body.token", Type: String, Value: "7"}, Live: "7"}}}
	atStartUp := startUp
	atStartUp.beforeTest = false
	const kept = "round F gave it to the application only before the call's test began, and the application may have kept what it gave there"
	for _, tc := range []struct {
		what, rec, live string
		earlier         []Earlier
		raw             []Raw
		name, why       string
	}{
		{"a served as recorded before the test", "slot 82", "slot 7", nil, []Raw{a.ServedBeforeTheTest(), b}, "mock-a", kept},
		{"a served as recorded before the test, the recording's call late", "slot 7", "slot 82", nil, []Raw{a.ServedBeforeTheTest(), b}, "mock-a", kept},
		{"a served as recorded before the test, the recorded value took no word", "slot 83", "slot 7", nil, []Raw{a.ServedBeforeTheTest(), b}, "mock-a", kept},
		{"the start-up exchange, read in a later test", "slot 82", "slot 7", []Earlier{startUp}, []Raw{b}, "mock-s body.token", kept},
	} {
		r, f := strings.Fields(tc.rec), strings.Fields(tc.live)
		d := oWord(t, Decide(costF(tc.rec, tc.live, tc.earlier, tc.raw, nil)), r[1], f[1])
		oRefused(t, tc.what, d, RefusalUnknown, tc.name)
		if d.Refusal != nil && !strings.Contains(d.Refusal.Why, tc.why) {
			t.Errorf("%s: refused %q; want it to say %q", tc.what, d.Refusal.Why, tc.why)
		}
	}
	costFresh(t, "a served as recorded in the call's own test", oWord(t, Decide(costF("slot 82", "slot 7", nil, []Raw{a, b}, nil)), "82", "7"))
	costFresh(t, "the start-up exchange, read at the start-up", oWord(t, Decide(costF("slot 82", "slot 7", []Earlier{atStartUp}, []Raw{b}, nil)), "82", "7"))
}

// A conflict comes before the older-test rule: a place F gave the
// application only in an older test, or only before the call's test began,
// that holds the recorded word whole where F gave another text there, a
// word of 32 bits that counts by its text, is a conflict (edits: fix the
// application, or give the value), not a place keploy cannot tell of
// (edit: re-record).
func TestOutcomeAConflictComesBeforeAPlaceServedOnlyBeforeTheTest(t *testing.T) {
	const rec, live = "slot 4f9a2c7e1b3d", "slot 9e8d7c6b5a40"
	a := eAnswer("mock-a", 300*time.Millisecond, "4f9a2c7e1b3d").ServedAs(1).ServedAsRecorded()
	for _, x := range []Raw{a.ServedInAnOlderTest(), a.ServedBeforeTheTest(), a.ServedInAnOlderTest().ServedBeforeTheTest()} {
		d := oWord(t, Decide(costF(rec, live, nil, []Raw{x}, nil)), "4f9a2c7e1b3d", "9e8d7c6b5a40")
		oRefused(t, "a 48-bit word whole at a place served only in an older test ("+strconv.FormatBool(x.InAnOlderTest())+") or before the test", d, RefusalConflict, "mock-a")
	}
}

// A request an older test's call left the run (Earlier.ofEarlierTest), as
// main's late-call rule reads it (olderTook): a word held there where F
// gave another text is set aside only where the value that lacks that word
// holds no word of another place a run gave the application before the
// call. Where it does ("82" at record and "7" in F beside a request that
// held 7 in both runs and a later answer of 82), the call may be a late
// call of that older test, so each value may be a copy of a word its run
// picked from what both runs gave: refused as unknown, naming the place,
// the other place and why. Main refuses each
// (late_call_older_serve_test.go). Where the recorded value took no such
// word ("slot 83"), or no other place stands beside the request, the word
// is set aside, as main learns it, and the value is fresh.
func TestOutcomeAnOlderTestsRequestSetsNoWordAsideWhereTheOtherValueTookAWord(t *testing.T) {
	b := eAnswer("mock-b", 200*time.Millisecond, "82").ServedAs(2).ServedAsRecorded()
	ran := Earlier{Mock: "mock-r", At: eRecAt.Add(-300 * time.Millisecond), Run: true,
		Payload: Raw{mock: "mock-r", at: eRecAt.Add(-300 * time.Millisecond), answered: eRecAt.Add(-299 * time.Millisecond), ordered: true}.ServedAs(1),
		Seen:    []Seen{{Side: Request, Leaf: Leaf{Path: "body.slot", Type: String, Value: "7 n1"}, Live: "7 n2", Place: "body.slot"}}}
	ranMoved := ran
	ranMoved.Seen = []Seen{{Side: Request, Leaf: Leaf{Path: "body.slot", Type: String, Value: "7 n1"}, Live: "5 n2", Place: "body.slot"}}
	// F gave 82 where the recording gave 90: the later answer holds F's
	// word only in F's view.
	bLive := eAnswer("mock-b", 200*time.Millisecond, "90").ServedAs(2).ServedLive(eAnswer("mock-b", 200*time.Millisecond, "82"))
	// Another older test's request: the recording sent 82 there and F 90.
	ran2 := ran
	ran2.Mock, ran2.At = "mock-r2", eRecAt.Add(-250*time.Millisecond)
	ran2.Payload = Raw{mock: "mock-r2", at: ran2.At, answered: eRecAt.Add(-249 * time.Millisecond), ordered: true}.ServedAs(2)
	ran2.Seen = []Seen{{Side: Request, Leaf: Leaf{Path: "body.slot", Type: String, Value: "82 n1"}, Live: "90 n2", Place: "body.slot"}}
	const sent = "sent mock-r's request only in an older test than the call's"
	for _, tc := range []struct {
		what, rec, live string
		e               []Earlier
		raw, set        []Raw
		by              string
	}{
		{"an older test's call's request", "slot 82", "slot 7", []Earlier{ran}, []Raw{b}, nil, "mock-b"},
		{"an older test's call's request, the recording's call late", "slot 7", "slot 82", []Earlier{ran}, []Raw{b}, nil, "mock-b"},
		{"an older test's call's request, moved in F, the recording's call late", "slot 7", "slot 82", []Earlier{ranMoved}, []Raw{b}, nil, "mock-b"},
		{"an older test's call's request, the later answer F changed to 82", "slot 7", "slot 82", []Earlier{ran}, []Raw{bLive}, nil, "mock-b"},
		{"an older test's call's request, the later answer read once for the set", "slot 82", "slot 7", []Earlier{ran}, nil, []Raw{b}, "mock-b"},
		{"an older test's call's request, the later answer F changed to 82 read once for the set", "slot 7", "slot 82", []Earlier{ran}, nil, []Raw{bLive}, "mock-b"},
		{"an older test's call's request, beside another whose recording sent 82", "slot 82", "slot 7", []Earlier{ran, ran2}, nil, nil, "mock-r2 body.slot"},
	} {
		r, f := strings.Fields(tc.rec), strings.Fields(tc.live)
		in := costF(tc.rec, tc.live, tc.e, tc.raw, nil)
		if tc.set != nil {
			in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(tc.set[0])}, nil }
		}
		d := oWord(t, Decide(in), r[1], f[1])
		oRefused(t, tc.what, d, RefusalUnknown, "mock-r body.slot")
		// The value that lacks the request's word holds 82, in each case.
		for _, w := range []string{sent, `"82" stands in ` + tc.by} {
			if d.Refusal != nil && !strings.Contains(d.Refusal.Why, w) {
				t.Errorf("%s: refused %q; want it to say %q", tc.what, d.Refusal.Why, w)
			}
		}
	}
	costFresh(t, "the recorded value took no word", oWord(t, Decide(costF("slot 83", "slot 7", []Earlier{ran}, []Raw{b}, nil)), "83", "7"))
	costFresh(t, "no other place beside the request", oWord(t, Decide(costF("slot 82", "slot 7", []Earlier{ran}, nil, nil)), "82", "7"))
	costFresh(t, "the later answer recorded after the call", oWord(t, Decide(costF("slot 82", "slot 7", []Earlier{ran}, []Raw{eAnswer("mock-b", -200*time.Millisecond, "82").ServedAs(2).ServedAsRecorded()}, nil)), "82", "7"))
	ownWord := ran
	ownWord.Seen = []Seen{{Side: Request, Leaf: Leaf{Path: "body.slot", Type: String, Value: "7 82 n1"}, Live: "7 5 n2", Place: "body.slot"}}
	costFresh(t, "the request held F's word only at record, and no other place", oWord(t, Decide(costF("slot 7", "slot 82", []Earlier{ownWord}, nil, nil)), "7", "82"))
	// A piece of a longer value stays chance there, as at an older test's
	// answer: a UUID's 4-hex group the request held at record, beside a
	// later answer that gave F's group in both runs.
	ranU := ran
	ranU.Seen = []Seen{{Side: Request, Leaf: Leaf{Path: "body.id", Type: String, Value: "bb7ed9ee-670e-48d1-982f-b54b2b73982f"}, Live: "0c1d2e3f-4a5b-6c7d-8e9f-a0b1c2d3e4f5", Place: "body.id"}}
	bU := eAnswer("mock-b", 200*time.Millisecond, "dddd").ServedAs(2).ServedAsRecorded()
	costFresh(t, "a UUID's 4-hex piece at the request, F's piece in a later answer", oWord(t, Decide(costF("1111aaaa-2222-3333-48d1-444455556666", "9999cccc-7777-8888-dddd-000011112222", []Earlier{ranU}, []Raw{bU}, nil)), "48d1", "dddd"))
	costFresh(t, "the call's own test's request", oWord(t, Decide(costF("slot 82", "slot 7", []Earlier{{Mock: ran.Mock, At: ran.At, Payload: oPayload(ran.Mock, ran.At).ServedAs(1), Seen: ran.Seen}}, []Raw{b}, nil)), "82", "7"))
}

// Two signatures in upper-case hex (64 characters): read without their
// first two characters, as an escape in a quoted-printable form reads them,
// neither is a run that decodes.
const (
	oSigA = "F0A1C2E3D4B5A69788796A5B4C3D2E1F0F1E2D3C4B5A69788796A5B4C3D2E1F0"
	oSigB = "7C4E19B2A0D35F8E61C7B9042A3E5D8F1B6C0E9A2D4F7B3C5E8A1D6F0B2C4E97"
)

// oNoneFresh checks ds decides a word and refuses each it decides, naming
// why: as kind, or as any kind where kind is "".
func oNoneFresh(t *testing.T, what string, ds []Decision, kind RefusalKind, why string) {
	t.Helper()
	if len(ds) == 0 {
		t.Errorf("%s: no decision; want each changed word refused", what)
	}
	for _, d := range ds {
		k := kind
		if k == "" && d.Refusal != nil {
			k = d.Refusal.Kind
		}
		oRefused(t, what, d, k, why)
	}
}

// What only a form main's readings check shows is refused, never learned
// fresh, as main refuses it (readings.go unparsed, "what only a checked form
// shows is refused"): the reader reads no such form whole, so a changed word
// beside it may be part of what that form changed. Each way: a soft line
// break joining a run of the base64 alphabet that does not decode (in a
// header, in a JSON string, in a mail that says it is quoted-printable),
// non-ASCII text that changed behind its escapes, a '%' in what a base64 run
// decodes to, a stray '%' as written, and a hex signature a header's
// padding shows is read as escapes, a run that does not decode. Where main reads the value
// whole (a hex signature's change, a signature in a value nothing shows is
// quoted-printable), the word is still fresh.
func TestOutcomeAWordOnlyACheckedFormShowsIsNeverFresh(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	mail := func(tok string) string {
		return "From: shop@test\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nToken: =2B" + tok + " =E2=80=94 ok\r\n"
	}
	const run, script, escape = "a run of the base64 alphabet that does not decode", "text of a script keploy does not split into words", "a '%' or '\\'"
	for _, c := range []struct{ name, rec, live, why string }{
		{"a soft line break joins a base64 run", "Token: ab+=\r\ncd ok", "Token: ab+=\r\nxy ok", run},
		{"the same, in a JSON string, a '!' escaped", `Token: ab+=\r\ncd ok=21`, `Token: ab+=\r\nxy ok=21`, run},
		{"a mail that says it is quoted-printable", mail("YmFy"), mail("YmFz"), run},
		{"non-ASCII text behind its escapes", "Commande =C3=A9t=C3=A9 ok", "Commande =C3=A8t=C3=A8 ok", script},
		{"a Latin-1 escape of no text beside a soft line break", "caf=E9 ab+=\r\ncd ok", "caf=E9 ab+=\r\nce ok", run},
		{"a '%' in base64 text", "data " + b64("price 10% off a7k29q"), "data " + b64("price 10% off b8l30r"), escape},
		{"a stray '%'", "Save 20% now: 3f2b8c1e9a4d", "Save 20% now: a7c4e2d10b3f", escape},
		{"a hex signature after a mail header", "X-A: YWJj=\r\nX-Sig: v1=2B" + oSigA[2:], "X-A: YWJj=\r\nX-Sig: v1=41" + oSigB[2:], run},
	} {
		for _, p := range [][2]string{{c.rec, c.live}, {c.live, c.rec}} {
			oNoneFresh(t, c.name+", "+strconv.Quote(p[0]), Decide(oInput("url.query.state", p[0], p[1], nil, nil)), RefusalCannotCheck, c.why)
		}
	}
	for _, c := range []struct{ name, rec, live string }{
		{"an id main reads whole", "Token: A7K29QXM ok", "Token: Zq81Lm0X ok"},
		{"a hex signature's change", "X-A: YWJj=\r\nX-Sig: v1=41" + oSigA[2:], "X-A: YWJj=\r\nX-Sig: v1=35" + oSigB[2:]},
		{"a signature nothing shows is quoted-printable", "v1=2B" + oSigA[2:], "v1=41" + oSigB[2:]},
	} {
		for _, p := range [][2]string{{c.rec, c.live}, {c.live, c.rec}} {
			ds := Decide(oInput("url.query.state", p[0], p[1], nil, nil))
			if len(ds) == 0 || slices.ContainsFunc(ds, func(d Decision) bool { return d.Outcome != OutcomeFresh }) {
				t.Errorf("%s, %q: %s; want every changed word fresh, as main reads it whole", c.name, p[0], oDescribe(ds))
			}
		}
	}
}

// The checked-form rule reads a value's clock words in the call's window,
// as main's readings do: a read of the clock there written with a zone
// ("+05:30") makes a run of the base64 alphabet that does not decode
// ("00+05"), and that run is the clock's, so an id beside it is still
// fresh. Where the window does not hold the read, the run is no clock's,
// and the id is refused, naming main's reason.
func TestOutcomeAClockReadInTheCallsWindowIsReadByTheCheckedFormRule(t *testing.T) {
	rAt := oAt.Add(300 * time.Millisecond)
	fAt := oAt.Add(time.Hour + 7*time.Second + 800*time.Millisecond)
	ist := time.FixedZone("IST", 5*3600+1800)
	rec := "at " + rAt.In(ist).Format(time.RFC3339) + " id A7K29QXMab"
	live := "at " + fAt.In(ist).Format(time.RFC3339) + " id Zq81Lm0Xcd"
	for _, c := range []struct {
		what  string
		w     *Window
		fresh bool
	}{
		{"the call's window holds the read", iWin(rAt, fAt, 2*time.Second), true},
		{"the call's window ends before round F's read", iWin(rAt, fAt.Add(-time.Minute), 2*time.Second), false},
	} {
		in := oInput("url.query.state", rec, live, nil, nil)
		in.At, in.Arrived, in.Window = rAt, fAt, c.w
		d := oWord(t, Decide(in), "A7K29QXMab", "Zq81Lm0Xcd")
		switch {
		case c.fresh && d.Outcome != OutcomeFresh:
			t.Errorf("%s: %s; want the id fresh", c.what, oDescribe([]Decision{d}))
		case !c.fresh:
			oRefused(t, c.what, d, RefusalCannotCheck, "a run of the base64 alphabet that does not decode")
		}
	}
}

// An id a decoded form of the value moved (swapped behind quoted-printable
// escapes, or inside a base64 run's text while the value holds it as
// written at its place in both runs) is a word of what changed, as main
// reads it: where a place before the call holds it, recorded for the
// recorded value's word, in round F for F's, in an older test or the
// call's own, no word of the value is learned fresh, as the reader refuses
// the same swap as written. Where no place holds it, the place holds
// another word, or round F served the place only after the call arrived,
// the words stay fresh.
func TestOutcomeAnIdThatMovedInADecodedFormIsNeverFresh(t *testing.T) {
	const a, b = "A7K29QXM", "Zq81Lm0X"
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	shapes := []struct{ name, rec, live string }{
		{"swapped behind escapes", "x=3D=23" + a + " y=3D=23" + b, "x=3D=23" + b + " y=3D=23" + a},
		{"swapped inside base64 text", "ref " + a + " " + b + " =23 " + b64(`{"a":"`+a+`","b":"`+b+`"}`), "ref " + a + " " + b + " =23 " + b64(`{"a":"`+b+`","b":"`+a+`"}`)},
	}
	// held is mock-11 holding "ref r", "ref f" in round F, in an older test
	// (run) or the call's own; served after the call arrived in round F
	// (#5018's arrival cut: no place before the call there) where after.
	held := func(run bool, r, f string, after bool) []Earlier {
		e := oServed("mock-11", run, false, Request, oText("body.ref", "ref "+r), "ref "+f, time.Millisecond)
		e[0].Payload.servedAfter = after
		return e
	}
	for _, s := range shapes {
		for _, p := range [][2]string{{s.rec, s.live}, {s.live, s.rec}} {
			for _, h := range []struct {
				what string
				e    []Earlier
			}{
				{"an older test held it in both runs", held(true, a, a, false)},
				{"an older test held it recorded", held(true, a, "other", false)},
				{"an older test held it in round F", held(true, "other", a, false)},
				{"the call's own test held it in both runs", held(false, a, a, false)},
			} {
				oNoneFresh(t, s.name+", "+h.what+", "+strconv.Quote(p[0]), Decide(oInput("url.query.state", p[0], p[1], h.e, nil)), "", "mock-11")
			}
			for _, h := range []struct {
				what string
				e    []Earlier
			}{
				{"no place", nil},
				{"a place held another word", held(true, "B8L30RQW", "B8L30RQW", false)},
				{"round F served the place only after the call arrived", held(false, a, a, true)},
			} {
				ds := Decide(oInput("url.query.state", p[0], p[1], h.e, nil))
				if len(ds) == 0 || slices.ContainsFunc(ds, func(d Decision) bool { return d.Outcome != OutcomeFresh }) {
					t.Errorf("%s, %s, %q: %s; want every changed word fresh", s.name, h.what, p[0], oDescribe(ds))
				}
			}
		}
	}
}

// The moved-word rule reads each side on its own side and only whole ids:
//   - an id a base64 run's text holds once more in round F only is a word
//     of what changed in F alone: a place that held it only as recorded
//     (round F gave another word there) does not refuse it, one that gave
//     it in round F does;
//   - a word under 32 bits that moved there stands all over a set, so a
//     place that holds it is chance, as for a piece of a longer value;
//   - an id that changed there (one run lost it), not moved, is the
//     decoded-form rule's: a place that kept it refuses the value, as main
//     does (inDecodedForm).
func TestOutcomeTheMovedWordRuleReadsEachSideAndWholeIdsOnly(t *testing.T) {
	const a = "A7K29QXM"
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	in := func(rec, live, r, f string) Input {
		return oInput("url.query.state", rec, live, oServed("mock-11", true, false, Request, oText("body.ref", "ref "+r), "ref "+f, time.Millisecond), nil)
	}
	fresh := func(what string, ds []Decision) {
		t.Helper()
		if len(ds) == 0 || slices.ContainsFunc(ds, func(d Decision) bool { return d.Outcome != OutcomeFresh }) {
			t.Errorf("%s: %s; want every changed word fresh", what, oDescribe(ds))
		}
	}
	once, twice := "=23 "+b64(`{"a":"`+a+`"}`), "=23 "+b64(`{"a":"`+a+`","b":"`+a+`"}`)
	fresh("an id once more in F, the place held it only as recorded", Decide(in(once, twice, a, "other")))
	oNoneFresh(t, "an id once more in F, the place gave it in F", Decide(in(once, twice, "other", a)), "", "mock-11")
	fresh("an id once more as recorded, the place gave it only in F", Decide(in(twice, once, "other", a)))
	oNoneFresh(t, "an id once more as recorded, the place held it as recorded", Decide(in(twice, once, a, "other")), "", "mock-11")
	fresh("a 3-letter word swapped in base64 text, held at a place", Decide(in("=23 "+b64(`{"a":"k7q","b":"m2x"}`), "=23 "+b64(`{"a":"m2x","b":"k7q"}`), "k7q", "k7q")))
	oNoneFresh(t, "FALSE GREEN? an id an escape hides that changed, not moved, the place kept it", Decide(in("x=3D=23"+a+" ok", "x=3D=23B8L30RQW ok", a, a)), "", "mock-11")
}

// An id that moved in the value as written (stayed at one place in round F
// while it changed at another, held once more by one run, or swapped with
// another word), held at a place before the call, leaves no word beside it
// fresh, as main refuses the value ("unaccounted"): keploy cannot tell
// whether the other words that changed came from that place too. The
// reader judges the moved id by its own rules (a conflict here), and the
// rule holds where only the place's case-folded text holds it. A place that
// holds another word leaves the other word fresh.
func TestOutcomeAnIdThatMovedAsWrittenLeavesNoWordBesideItFresh(t *testing.T) {
	const a, b, w1, w2 = "A7K29QXMab", "Pq81Lm0Xzz", "Hd73Kw2Nqe", "Rt58Vb1Mxo"
	shapes := []struct{ name, rec, live string }{
		{"kept at one place, changed at another", "a=" + a + " b=" + w1 + " c=" + a, "a=" + b + " b=" + w2 + " c=" + a},
		{"held once more as recorded", "a=" + a + " b=" + a + " c=" + w1, "a=" + a + " b=" + b + " c=" + w2},
		{"held once more in round F", "a=" + b + " b=" + w1 + " c=" + a, "a=" + a + " b=" + w2 + " c=" + a},
		{"swapped with another word", "a=" + a + " b=" + b + " c=" + w1, "a=" + b + " b=" + a + " c=" + w2},
	}
	held := func(run bool, r, f string) []Earlier {
		return oServed("mock-11", run, false, Request, oText("body.ref", "ref "+r), "ref "+f, time.Millisecond)
	}
	for _, s := range shapes {
		for _, run := range []bool{false, true} {
			for _, h := range [][2]string{{a, a}, {strings.ToLower(a), strings.ToLower(a)}} {
				what := fmt.Sprintf("%s, held %q (an older test: %v)", s.name, h[0], run)
				oNoneFresh(t, what, Decide(oInput("url.query.state", s.rec, s.live, held(run, h[0], h[1]), nil)), "", "mock-11")
			}
			if d := oWord(t, Decide(oInput("url.query.state", s.rec, s.live, held(run, "other", "other"), nil)), w1, w2); d.Outcome != OutcomeFresh {
				t.Errorf("%s, a place holding another word (an older test: %v): %s; want fresh", s.name, run, oDescribe([]Decision{d}))
			}
		}
	}
}

// A run of the value that is a read of the clock in the call's window is not
// decoded, as main reads the value: here an epoch in microseconds whose
// digits read as hex text (" ABCDEFG"), beside the same id written in hex
// and, in round F only, in base64 (another word's base64 as recorded). Read
// so, round F holds the id once more,
// and the place that holds it refuses the changed word beside it; decoding
// the clock read too would pair the id with the read's and leave the word
// fresh, where main refuses it. A place that holds another word leaves it
// fresh.
func TestOutcomeAClockReadInTheCallsWindowIsNoDecodedText(t *testing.T) {
	const bus = 2041424344454647
	b := time.UnixMicro(bus).UTC()
	c := b.Add(time.Hour + 7*time.Second)
	rAt, fAt := b.Add(300*time.Millisecond), c.Add(300*time.Millisecond)
	id := "ABCDEFG"
	B, C := strconv.FormatInt(bus, 10), strconv.FormatInt(c.UnixMicro(), 10)
	rec := "t=" + B + " x=" + hex.EncodeToString([]byte(id)) + " y=" + base64.StdEncoding.EncodeToString([]byte("HIJKLMN")) + " c=Hd73Kw2Nqe"
	live := "t=" + C + " x=" + hex.EncodeToString([]byte(id)) + " y=" + base64.StdEncoding.EncodeToString([]byte(id)) + " c=Rt58Vb1Mxo"
	in := func(h string) Input {
		e := oServed("mock-11", false, false, Request, oText("body.ref", "ref "+h), "ref "+h, time.Millisecond)
		in := oInput("url.query.state", rec, live, e, nil)
		in.At, in.Arrived, in.Window = rAt, fAt, iWin(rAt, fAt, 2*time.Second)
		return in
	}
	if d := oWord(t, Decide(in(id)), "Hd73Kw2Nqe", "Rt58Vb1Mxo"); d.Outcome != OutcomeRefused || d.Refusal == nil || !strings.Contains(d.Refusal.Why, "mock-11") {
		t.Errorf("FALSE GREEN? a place holding the id: %s; want refused naming mock-11", oDescribe([]Decision{d}))
	}
	if d := oWord(t, Decide(in("other")), "Hd73Kw2Nqe", "Rt58Vb1Mxo"); d.Outcome != OutcomeFresh {
		t.Errorf("a place holding another word: %s; want fresh", oDescribe([]Decision{d}))
	}
}

// The decoded-form rule reads the value's forms in the call's window, as
// main does: a run that is a read of the clock there is not decoded. Here an
// epoch in microseconds whose digits read as hex text (" ABCDEFG") is read
// twice as recorded, a read of the clock in the recorded call's window both
// times, while round F holds that read once, an hour outside its own
// window, beside its own read: only round F's value carries "ABCDEFG", in a
// decoded form. A place that held it in both runs, in the call's test, an
// older test, or served by round F only after the call arrived, refuses the
// changed word beside it, as main refuses the value (unaccounted). With no
// window both runs decode the recorded read, so both carry "ABCDEFG" and the
// place says nothing of it: the word is fresh, as main learns the value. A
// place that holds another word leaves it fresh.
func TestOutcomeADecodedFormIsReadInTheCallsWindow(t *testing.T) {
	const bus = 2041424344454647
	b := time.UnixMicro(bus).UTC()
	c := b.Add(time.Hour + 7*time.Second)
	rAt, fAt := b.Add(300*time.Millisecond), c.Add(300*time.Millisecond)
	B, C := strconv.FormatInt(bus, 10), strconv.FormatInt(c.UnixMicro(), 10)
	const w1, w2 = "Hd73Kw2Nqe", "Rt58Vb1Mxo"
	in := func(h string, older, after, window bool) Input {
		e := oServed("mock-11", older, false, Request, oText("body.ref", "ref "+h), "ref "+h, time.Millisecond)
		e[0].Payload.servedAfter = after
		in := oInput("url.query.state", "a="+B+" b="+B+" c="+w1, "a="+B+" b="+C+" c="+w2, e, nil)
		if window {
			in.At, in.Arrived, in.Window = rAt, fAt, iWin(rAt, fAt, 2*time.Second)
		}
		return in
	}
	for _, p := range []struct {
		what         string
		older, after bool
	}{{"the call's test", false, false}, {"an older test", true, false}, {"the call's test, served by round F after the call arrived", false, true}} {
		if d := oWord(t, Decide(in("ABCDEFG", p.older, p.after, true)), w1, w2); d.Outcome != OutcomeRefused || d.Refusal == nil || !strings.Contains(d.Refusal.Why, "mock-11") {
			t.Errorf("FALSE GREEN? in the call's window, a place in %s held the decoded id in both runs: %s; want refused naming mock-11", p.what, oDescribe([]Decision{d}))
		}
		for _, c := range []struct {
			what   string
			h      string
			window bool
		}{{"in the call's window, a place holding another word", "other", true}, {"with no window, a place holding the id both runs decode", "ABCDEFG", false}} {
			if d := oWord(t, Decide(in(c.h, p.older, p.after, c.window)), w1, w2); d.Outcome != OutcomeFresh {
				t.Errorf("%s, in %s: %s; want fresh", c.what, p.what, oDescribe([]Decision{d}))
			}
		}
	}
}

// A word of 32 bits or more that only one run's value carries, and only in
// a decoded form (behind a quoted-printable escape, inside a base64 run),
// leaves no word of the value fresh where a place keploy reads holds it:
// as recorded or in round F, whatever the place gave in the other run (the
// same word, the same text around it, another word), in the call's test or
// an older one, case-folded, or in a view of an answer round F served
// changed. keploy binds no word in such a form, so it can check no copy of
// the place, and main refuses each of these (unaccounted), or binds the
// value where it followed the place. A place that holds it only in the
// other run's text refuses too, where main reads such a place only in a
// recording's payload: one rule across places. A place, or a view, that
// holds another word leaves the value's word fresh.
func TestOutcomeAWordOnlyADecodedFormCarriesIsRefusedWhereAPlaceHoldsIt(t *testing.T) {
	const r, f, o = "A7K29QXM", "B8L30RQW", "Zq19Wd83"
	const recCarries, fCarries = "which only the recorded value carries", "which only round F's value carries"
	b64 := func(w string) string {
		return "x=" + base64.StdEncoding.EncodeToString([]byte(`{"a":"`+w+`"}`)) + " ok"
	}
	esc := func(w string) string { return "x=3D=23" + w + " ok" }
	ans := func(v string) Raw { return oAnswer("mock-7", 100*time.Millisecond, v) }
	fresh := func(what string, ds []Decision) {
		t.Helper()
		if len(ds) == 0 || slices.ContainsFunc(ds, func(d Decision) bool { return d.Outcome != OutcomeFresh }) {
			t.Errorf("%s: %s; want every changed word fresh", what, oDescribe(ds))
		}
	}
	for _, enc := range []struct {
		name string
		of   func(string) string
	}{{"behind a quoted-printable escape", esc}, {"inside a base64 run", b64}} {
		rec, live := enc.of(r), enc.of(f)
		at := func(older bool, pr, pf string) Input {
			return oInput("url.query.state", rec, live, oServed("mock-11", older, false, Request, oText("body.ref", pr), pf, time.Millisecond), nil)
		}
		for _, older := range []bool{false, true} {
			for _, c := range []struct{ what, pr, pf, carries string }{
				{"the recorded id, kept", "ref " + r, "ref " + r, recCarries},
				{"the recorded id, kept, the text around it changed", "ref " + r + " n=1", "ref " + r + " n=2", recCarries},
				{"the recorded id as recorded, round F gave another word", "ref " + r, "ref " + o, recCarries},
				{"round F's id, kept", "ref " + f, "ref " + f, fCarries},
				{"round F's id, kept, the text around it changed", "ref " + f + " n=1", "ref " + f + " n=2", fCarries},
				{"round F's id in round F, the recording gave another word", "ref " + o, "ref " + f, fCarries},
				{"the recorded id case-folded, kept", "ref " + strings.ToLower(r), "ref " + strings.ToLower(r), recCarries},
				{"the recorded id only in round F", "ref " + o, "ref " + r, recCarries},
				{"round F's id only as recorded", "ref " + f, "ref " + o, fCarries},
			} {
				what := fmt.Sprintf("FALSE GREEN? %s, a place (an older test: %v) held %s", enc.name, older, c.what)
				ds := Decide(at(older, c.pr, c.pf))
				oNoneFresh(t, what, ds, "", "mock-11")
				oNoneFresh(t, what, ds, "", c.carries)
			}
			fresh(fmt.Sprintf("%s, a place (an older test: %v) holding another word", enc.name, older), Decide(at(older, "ref "+o, "ref "+o)))
		}
		view := func(v string) Input {
			return oInput("url.query.state", rec, live, nil, []Raw{ans("ok").ServedAs(1).ServedChanged(ans("ok"), ans(v))})
		}
		ds := Decide(view(f))
		oNoneFresh(t, "FALSE GREEN? "+enc.name+", a view of an answer round F served changed held round F's id", ds, "", "mock-7")
		oNoneFresh(t, "FALSE GREEN? "+enc.name+", a view of an answer round F served changed held round F's id", ds, "", fCarries)
		fresh(enc.name+", a view of an answer round F served changed holding another word", Decide(view(o)))
	}
}
