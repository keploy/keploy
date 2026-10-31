package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// api-server's T2 suite (rev2; mongo-vars-2; var-inside-longer-value r8s3;
// s8/s9): the application mints a random id (a UUIDv4 _id of an
// app_config_audits or eventsV2 insert, a session token's jti, an OAuth
// request's _id) or counts a duration, and keploy test refused it because
// one short word of it stood in an earlier test's recording or in the set's
// start-up: "a word of its recorded value (its characters 10 to 13) appears
// in an earlier test of the set, in mock-231", "(its characters 6 to 6)
// appears in the test set's start-up, in mock-188 at body{keys}", a
// duration_ms "40" beside another test's 40. A 4-hex group of a UUID carries
// 16 bits: rev2's 7186 UUIDs held 83% of the values of a v4 UUID's third
// group, so a fresh UUID shares such a word with the set nine times in ten.
//
// The founder's rule (2026-10-31): a changed word is evidence by its text
// only at 32 bits or more (wordBits), alone or as its whole token. A
// shorter one goes through the rule a copy follows its source (follows.reads)
// under keploy test too, with three outcomes:
//   - disproven: both runs gave the application the word at its place
//     before the call and the value moved away from it: chance, learned as
//     varying, naming the place;
//   - shown: the place changed with the value (to the value F saw or to
//     any other): evidence, as before;
//   - unknown (no answer time, answered at or after the call, a serve
//     with no view): refused, naming the place, never noise.
//
// ClassifyFollowing reads every word by the rule, as before, and sets a
// place that changed to another value than the value's aside; the rows of a
// word of 32 bits or more ("text: ") and of such a place ("given: ") are
// keploy test's alone.
func TestAShortWordIsChanceOnlyWhereBothRunsShowItsPlaceDidNotMoveWithTheValue(t *testing.T) {
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	w := &Window{
		Rec: Span{From: at.Add(-900 * time.Millisecond), To: at},
		F:   Span{From: at.Add(60_500 * time.Millisecond), To: at.Add(61_400 * time.Millisecond)},
	}
	const (
		p    = "body.docs.msgs.0._id"
		rec  = "ed35a78f-905c-48d1-bc3d-a07311678c73"
		live = "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		// Another test's random id: its third group is the recorded id's.
		other = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718"
		// A random token of the URL-safe alphabet: "n" a word of it.
		tokRec  = "Qz7kP-n-Xa9bT2_w"
		tokLive = "Lm4rT-q-Zc8dV1_y"
	)
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	text := func(path, v string) Leaf { return Leaf{Path: path, Type: String, Value: v} }
	// An exchange served before the call: an earlier test's (Run), or the
	// set's start-up's (Run, StartUp), recorded 2 s before the call and
	// answered a millisecond later (answered: its answer time), its leaf l
	// served as F saw it (live).
	served := func(mock string, startUp bool, l Leaf, live string, answered time.Duration) []Earlier {
		m := &models.Mock{Name: mock, Spec: models.MockSpec{ReqTimestampMock: at.Add(-2 * time.Second)}}
		if answered != 0 {
			m.Spec.ResTimestampMock = at.Add(-2*time.Second + answered)
		}
		return []Earlier{{Mock: mock, At: at.Add(-2 * time.Second), Run: true, StartUp: startUp, Payload: RawOf(m, true).ServedAs(3), Seen: []Seen{{Side: Response, Leaf: l, Live: live}}}}
	}
	alike := func(mock string, startUp bool, l Leaf) []Earlier {
		return served(mock, startUp, l, l.Value, time.Millisecond)
	}
	// What an earlier test's call sent (its request: an insert's _id), as
	// recorded and as F's application sent it (live).
	sent := func(mock string, l Leaf, live string) []Earlier {
		e := served(mock, false, l, live, time.Millisecond)
		e[0].Seen[0].Side = Request
		return e
	}
	own := func(e []Earlier) []Earlier {
		e[0].Run = false
		return e
	}
	type row struct {
		what, mock string
		rec, live  string
		earlier    []Earlier
	}
	for name, c := range map[string]func(earlier []Earlier, recorded, now string) Learned{
		"ClassifyIn": func(earlier []Earlier, recorded, now string) Learned {
			return ClassifyIn(clientTestCache(), []string{p}, str(now), str(recorded), at, earlier, nil, nil, w)[0]
		},
		"ClassifyGiven": func(earlier []Earlier, recorded, now string) Learned {
			return ClassifyGiven(clockedDrifts(), []string{p}, str(now), str(recorded), at, earlier, nil, nil, w, nil)[0]
		},
		"ClassifyFollowing": func(earlier []Earlier, recorded, now string) Learned {
			return ClassifyFollowing(clockedDrifts(), []string{p}, str(now), str(recorded), at, earlier, nil, nil, w, nil)[0]
		},
	} {
		// Disproven: chance, learned as varying, naming the place.
		for _, r := range []row{
			{"an earlier test's id holds the recorded id's third group", "mock-231", rec, live, alike("mock-231", false, text("body.cursor.firstBatch.0._id", other))},
			{"an earlier test's id holds the fresh id's third group", "mock-279", rec, live, alike("mock-279", false, text("body.cursor.firstBatch.0.jobId", "0c1d2e3f-4a5b-4b8a-8e9f-a0b1c2d3e4f5"))},
			{"the start-up's id holds the fresh id's fourth group", "mock-448", rec, live, alike("mock-448", true, text("body.request_id", "5a6b7c8d-1e2f-4a3b-9e21-0f1e2d3c4b5a"))},
			{"a start-up key is a word of one character of the id", "mock-188", tokRec, tokLive, alike("mock-188", true, Leaf{Path: "body{keys}", Type: Keys, Value: "_id\nn\nok"})},
			{"an earlier test's field holds four digits of the id alone", "mock-231", "ed35a78f-2486-48d1-bc3d-a07311678c73", live, alike("mock-231", false, text("body.count", "2486"))},
			{"an earlier test's id holds the value whole, a short word", "mock-231", "48d1", "7e6d", alike("mock-231", false, text("body.cursor.firstBatch.0._id", other))},
			{"two groups of the id together", "mock-231", rec, live, alike("mock-231", false, text("body.cursor.firstBatch.0._id", "ffff0000-905c-48d1-aaaa-bbbbccccdddd"))},
			{"a group of an id that kept another", "mock-231", rec, "ed35a78f-7e6d-4b8a-9e21-5c6d7e8f9a0b", alike("mock-231", false, text("body.cursor.firstBatch.0._id", other))},
			{"a counter of an id that kept its prefix, its place given alike", "mock-231", "order-17", "order-18", alike("mock-231", false, text("body.value.seq", "17"))},
			// api-server's authorization request ids: "kepreq_" and a
			// random tail; a word of one character of the tail stood in a
			// start-up's wire header (TestStaffBlockAnOAuthClient).
			{"a start-up's header holds a word of one character of a token", "mock-0", "kepreq_" + tokRec, "kepreq_" + tokLive, alike("mock-0", true, text("spec.responses[0].message.response_flags", "n"))},
			{"a count beside another test's equal count, given alike", "mock-231", "46", "4499", alike("mock-231", false, text("body.count", "46"))},
			// Co-variation disproven: the place moved too, to a value the
			// value took no word of (an earlier test's random id beside the
			// call's own, each made afresh in F).
			{"given: an earlier test's id F gave another id at, which the value took no word of", "mock-231", rec, live, served("mock-231", false, text("body.cursor.firstBatch.0._id", other), "c0ffee00-1234-4321-abcd-0000000000ff", time.Millisecond)},
		} {
			if strings.HasPrefix(r.what, "given: ") && name == "ClassifyFollowing" {
				continue // it keeps its reading of a place that moved
			}
			got := c(r.earlier, r.rec, r.live)
			if got.Kind != KindNoise || !strings.Contains(got.Why, r.mock) || !strings.HasSuffix(got.Why, "keploy learns it as varying") {
				t.Errorf("%s: disproven: %s: %s (%q); want noise naming %s", name, r.what, got.Kind, got.Why, r.mock)
			}
		}
		// Shown, or unknown, or evidence by its text: never noise; bound
		// to the place, or refused naming it.
		for _, r := range []row{
			// Shown: the place changed with the value (B2's probes).
			// The other way about: the value F saw took a word of what F
			// sent or gave at a leaf (an earlier test's insert _id holding a
			// group of it), where the recording's text there lacks it. The
			// value took the place's new word: refused, as a whole echo of
			// a leaf F served is (TestACopyFollowsItsSourceForEveryKindOfPlace).
			{"given: shown: an earlier test's insert F sent an id holding a group of the fresh id at", "mock-694", rec, live, sent("mock-694", text("body.docs.msgs.0._id", "b0fd3b58-3ff5-4a1a-a1b2-c3d4e5f60718"), "c1d2e3f4-5a6b-4b8a-8e9f-a0b1c2d3e4f5")},
			{"shown: an earlier test's id held the group F's id holds in F", "mock-231", rec, live, served("mock-231", false, text("body.cursor.firstBatch.0._id", other), "b0fd3b58-3ff5-4b8a-a1b2-c3d4e5f60718", time.Millisecond)},
			{"shown: an earlier test's count drifted with the value", "mock-231", "40", "2081", served("mock-231", false, text("body.count", "40"), "2081", time.Millisecond)},
			{"shown: an earlier test's counter drifted with the id that holds it", "mock-231", "order-17", "order-18", served("mock-231", false, text("body.value.seq", "17"), "18", time.Millisecond)},
			{"shown: an earlier test's status drifted with the value", "mock-231", "no", "ok", served("mock-231", false, text("body.status", "no"), "ok", time.Millisecond)},
			{"shown: the call's own test's count drifted with the value", "mock-231", "40", "2081", own(served("mock-231", false, text("body.count", "40"), "2081", time.Millisecond))},
			// The call's own test sent o-1 and the value was o-1: a variable
			// of it, refused where F's value is not the one the test sent.
			{"given: shown: an earlier call's order id drifted to another value than the value's", "mock-231", "o-1", "o-5", own(served("mock-231", false, text("body.order", "o-1"), "o-9", time.Millisecond))},
			// Shown the other way about: F gave the value's new word at a
			// place whose recorded word the recorded value holds.
			{"given: shown: an earlier test's field F gave the fresh word at, whose recorded word the recorded value holds", "mock-231", "k-17", "k-18", served("mock-231", false, text("body.seq", "k"), "18", time.Millisecond)},
			{"given: shown: an earlier test's insert F sent the fresh count at, where the recording sent the recorded one", "mock-694", "17", "18", sent("mock-694", text("body.docs.msgs.0.n", "17"), "18")},
			// Unknown: refused.
			{"unknown: no answer time", "mock-231", rec, live, served("mock-231", false, text("body.cursor.firstBatch.0._id", other), other, 0)},
			{"unknown: answered at the call", "mock-231", rec, live, served("mock-231", false, text("body.cursor.firstBatch.0._id", other), other, 2*time.Second)},
			{"unknown: a short count, no answer time", "mock-231", "46", "4499", served("mock-231", false, text("body.count", "46"), "46", 0)},
			// Evidence by its text: 32 bits or more.
			{"text: the id whole", "mock-231", rec, live, alike("mock-231", false, text("body.cursor.firstBatch.0._id", rec))},
			{"text: an 8-hex group of the id", "mock-231", rec, live, alike("mock-231", false, text("body.cursor.firstBatch.0._id", "ed35a78f-1111-4222-8333-944445555666"))},
			{"text: a 10-digit word of a fresh id, alone there", "mock-231", "kq-1730361600-z", "pw-2840472711-y", alike("mock-231", true, text("body.cursor.firstBatch.0.n", "1730361600"))},
			{"text: a whole token of the URL-safe alphabet", "mock-231", "kepreq_" + tokRec, "kepreq_" + tokLive, alike("mock-231", false, text("body.cursor.firstBatch.0._id", "kepreq_"+tokRec))},
		} {
			if (strings.HasPrefix(r.what, "text: ") || strings.HasPrefix(r.what, "given: ")) && name == "ClassifyFollowing" {
				continue // it reads every word by the rule, as before
			}
			got := c(r.earlier, r.rec, r.live)
			variable := got.Kind == KindVariable && got.From.Mock == r.mock
			refused := got.Kind == KindUnaccounted && !got.Bindable && strings.Contains(got.Why, r.mock)
			if !variable && !refused {
				t.Errorf("FALSE GREEN? %s: %s in %s: %s from %q bindable=%v (%q); want a variable of it or refused naming it", name, r.what, r.mock, got.Kind, got.From.Mock, got.Bindable, got.Why)
			}
		}
	}
}

// The same three outcomes in a payload F served (Raw): a Postgres row of
// another test or of the call's own, after the start-up (eStartup), served
// as recorded, with F's view of it, or with none; a short value whole (a
// duration_ms of 40 or 46) as a short word of an id.
func TestAShortWordOfAPayloadIsChanceOnlyWhereBothRunsShowItDidNotMoveWithTheValue(t *testing.T) {
	const p = "body.docs.msgs.0.duration_ms.$numberLong"
	const (
		rec   = "ed35a78f-905c-48d1-bc3d-a07311678c73"
		live  = "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		other = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718"
	)
	shape := func(v string) Shape { return Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: v}}} }
	cell := func(v string) Raw { return eRow(v, "1", "2").ServedAs(6) }
	asRecorded := func(v string) Raw { return cell(v).ServedAsRecorded() }
	viewed := func(v, f string) Raw { return cell(v).ServedLive(eRow(f, "1", "2")) }
	another := func(x Raw) []Raw { return append(eStartup(), x.OfAnotherTest()) }
	own := func(x Raw) []Raw { return append(eStartup(), x) }
	for name, c := range map[string]func(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier) []Learned{
		"ClassifyGiven": ClassifyGiven, "ClassifyFollowing": ClassifyFollowing,
	} {
		run := func(recorded, now string, raw []Raw, resp []Leaf) Learned {
			got := c(clockedDrifts(), []string{p}, shape(now), shape(recorded), eRecAt, nil, raw, resp, nil, nil)
			if len(got) != 1 {
				t.Fatalf("%s: %q recorded, %q in F: %+v", name, recorded, now, got)
			}
			return got[0]
		}
		for _, row := range []struct {
			what, rec, live string
			raw             []Raw
		}{
			{"another test's cell holds the recorded value, served as recorded", "40", "2081", another(asRecorded("40"))},
			{"another test's cell holds the recorded value, F gave it alike", "46", "4499", another(viewed("46", "46"))},
			{"the call's own test's cell holds the recorded value, F gave it alike", "46", "4499", own(viewed("46", "46"))},
			{"another test's cell holds five digits, F gave them alike", "29782", "41377", another(viewed("29782", "29782"))},
			{"another test's id holds the recorded id's group, served as recorded", rec, live, another(asRecorded(other))},
			{"the call's own test's id holds the recorded id's group, served as recorded", rec, live, own(asRecorded(other))},
			// Disproven: F gave another id there, which the value took no
			// word of.
			{"given: another test's id F gave another id at", rec, live, another(viewed(other, "c0ffee00-1234-4321-abcd-0000000000ff"))},
			{"given: another test's id F gave another id at, the value's word F's", rec, live, another(viewed("c0ffee00-1234-4b8a-abcd-0000000000ff", "b0fd3b58-3ff5-4a1a-a1b2-c3d4e5f60718"))},
		} {
			if strings.HasPrefix(row.what, "given: ") && name == "ClassifyFollowing" {
				continue // it keeps its reading of a place that moved
			}
			if got := run(row.rec, row.live, row.raw, nil); got.Kind != KindNoise || !strings.Contains(got.Why, "mock-6") {
				t.Errorf("%s: disproven: %s (%q recorded, %q in F): %s (%q); want noise naming mock-6", name, row.what, row.rec, row.live, got.Kind, got.Why)
			}
		}
		answeredAfter := ePgAnswered("mock-6", 100*time.Millisecond, eRecAt.Add(time.Second), "SELECT a, b, c FROM t", []models.PostgresV3Cells{{{Value: other}, {Value: "1"}, {Value: "2"}}}, "SELECT 1").ServedAs(6).ServedAsRecorded()
		for _, row := range []struct {
			what, rec, live string
			raw             []Raw
			resp            []Leaf
		}{
			// Shown.
			{"shown: another test's cell F gave the fresh value at", "40", "2081", another(viewed("40", "2081")), nil},
			{"shown: the call's own test's cell F gave the fresh value at", "40", "2081", own(viewed("40", "2081")), nil},
			{"shown: another test's id F gave the fresh group at", rec, live, another(viewed("x-48d1", "q-7e6d-z")), nil},
			// Unknown.
			{"unknown: another test's cell served with no view", "40", "2081", another(cell("40")), nil},
			{"unknown: another test's id served with no view", rec, live, another(cell(other)), nil},
			{"unknown: answered after the call", rec, live, another(answeredAfter), nil},
			// The call's recorded answer echoes the value beside a place
			// both runs gave alike: an echo keploy cannot answer beside a
			// source it set aside.
			{"an echo beside another test's id served as recorded", rec, live, another(asRecorded(other)), []Leaf{{Path: "body.id", Type: String, Value: rec}}},
			// Evidence by its text.
			{"text: a value of ten digits in another test's cell", "4821739056", "9930217845", another(viewed("4821739056", "4821739056")), nil},
		} {
			if (strings.HasPrefix(row.what, "text: ") || strings.HasPrefix(row.what, "given: ")) && name == "ClassifyFollowing" {
				continue // it reads every word by the rule, as before
			}
			got := run(row.rec, row.live, row.raw, row.resp)
			if got.Kind == KindNoise || !strings.Contains(got.Why+got.From.Mock, "mock-6") {
				t.Errorf("FALSE GREEN? %s: %s (%q recorded, %q in F): %s from %q (%q); want a variable of mock-6 or refused naming it", name, row.what, row.rec, row.live, got.Kind, got.From.Mock, got.Why)
			}
		}
	}
}

// wordBits counts a word's bits by the one class all its characters fall
// in: digits (log2 10), hex digits (log2 16), letters (log2 26), letters and
// digits (log2 36); case is not read, a separator ('-', '_') adds none, a
// letter beyond ASCII counts as a letter. The bound (chanceBits) falls
// between 7 and 8 hex digits, 9 and 10 decimal ones, 6 and 7 letters and
// digits, 6 and 7 letters.
func TestWordBitsBoundsAChanceWord(t *testing.T) {
	for _, row := range []struct {
		word   string
		chance bool
	}{
		{"n", true}, {"48d1", true}, {"905c", true}, {"4123", true}, {"a1b2c3", true}, {"abc1234", true}, {"123456789", true},
		{"Xa9bT", true}, {"Xa9bT2", true}, {"status", true}, {"a-1", true}, {"id-1", true}, {"en-US", true}, {"ж", true}, {"жжжж", true},
		{"ed35a78f", false}, {"a07311678c73", false}, {"1730361600", false}, {"Xa9bT2z", false}, {"stopped", false},
		{"order-18", false}, {"kepreq_Qz7kP-n-Xa9bT2_w", false}, {"ed35a78f-905c-48d1-bc3d-a07311678c73", false},
	} {
		if got := wordBits(row.word) < chanceBits; got != row.chance {
			t.Errorf("%q: %.1f bits, chance=%v; want chance=%v", row.word, wordBits(row.word), got, row.chance)
		}
	}
}

// The set's recordings (Order.GivenPlaces) read F's value at the field the
// same way: an older test's row, told with the one view F gave of it (the
// set reads its moved fields once, beside its words: viewWords). Where F
// gave another id there, which the value took no word of, the short word
// the recorded value shares with the recorded id is chance under keploy
// test; where F's id holds a word of the value F saw, it is refused.
// ClassifyFollowing keeps its reading: the set's recordings give it no
// field both runs gave alike, so it refuses.
func TestASetRecordingFGaveAnotherIdAtIsReadAtItsField(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: chanceValRec}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: chanceValLive}}}
	m := jobsMock(chanceSrcRec, "5")
	for _, c := range []struct {
		name, viewID string
		noise        bool
	}{
		{"F gave another id there, which the value took no word of", "e1f2a3b4-c5d6-4e7f-8a9b-9f8e7d6c5b4a", true},
		{"F gave an id there that holds a word of the value F saw", "e1f2a3b4-c5d6-4e7f-dddd-9f8e7d6c5b4a", false},
	} {
		tr := NewTracker()
		tr.Begin()
		tr.ServedMock(m)
		tr.ServedMockLive(m, "a", func() (*models.Mock, error) { return jobsMock(c.viewID, "9"), nil })
		tr.End()
		tr.Begin()
		tr.End()
		tr.Begin()
		o := tr.Placed().Order
		given := func() ([]GivenPlaces, error) {
			g, err := o.GivenPlaces(m)
			return []GivenPlaces{g}, err
		}
		got := ClassifyGiven(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, nil, nil, nil, given)[0]
		switch {
		case c.noise && (got.Kind != KindNoise || !strings.Contains(got.Why, "stands in mock-6, where record's full round gave the application another value, which the value took none of the words of")):
			t.Errorf("%s: %s (%q); want noise naming mock-6", c.name, got.Kind, got.Why)
		case !c.noise && (got.Kind == KindNoise || !strings.Contains(got.Why, "mock-6")):
			t.Errorf("FALSE GREEN? %s: %s (%q); want refused naming mock-6", c.name, got.Kind, got.Why)
		}
		if f := ClassifyFollowing(clockedDrifts(), []string{path}, live, rec, eRecAt, nil, nil, nil, nil, given)[0]; f.Kind == KindNoise {
			t.Errorf("%s: ClassifyFollowing: noise (%q); want its reading kept (refused)", c.name, f.Why)
		}
	}
}
