package learn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// api-server's T2 suite (rev2; mongo-vars-2; var-inside-longer-value r8s3):
// the application mints a random id (a UUIDv4 _id of an app_config_audits
// or eventsV2 insert, a session token's jti, an OAuth request's _id), and
// keploy refused it because one short word of it stood in an earlier test's
// recording or in the set's start-up: "a word of its recorded value (its
// characters 10 to 13) appears in an earlier test of the set, in mock-231",
// "(its characters 6 to 6) appears in the test set's start-up, in mock-188
// at body{keys}". A 4-hex group of a UUID carries at most 21 bits: rev2's
// 7186 UUIDs held 83% of the values of a v4 UUID's third group, so a fresh
// UUID shares such a word with the set nine times in ten, and a word of one
// character stands in nearly every recording. Such a word, of an id that
// changed whole, which no id of the place holds beside another word of the
// value's id, is no evidence the value came from there (chanceBits): the
// value classifies as with no such place. POST /run answered 502 on it, and
// TestTestRunReportOnClient's job heartbeats failed.
//
// Everything that is evidence stays so, refused or a variable naming the
// place: the id whole there, a word of at least 32 bits (an 8-hex group, a
// 10-digit number), two words of the id together there, a word of an id that
// kept another (a counter in "order-18"), a word the value holds whole, a
// word of at least 16 bits (aloneBits: "905c") a place holds as an id of
// its own. Four decimal digits alone there ("2486", a count) carry fewer.
// Noise names the place it read past.
func TestAShortWordOfAFreshIdIsNoEvidenceItCameFromAnotherTestOrTheStartUp(t *testing.T) {
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
	// An exchange served before the call, which did not drift: an earlier
	// test's (Run), or the set's start-up's (Run, StartUp).
	served := func(mock string, startUp bool, l Leaf) []Earlier {
		return []Earlier{{Mock: mock, At: at.Add(-2 * time.Second), Run: true, StartUp: startUp, Payload: payloadOf(mock, at.Add(-2*time.Second)), Seen: []Seen{{Side: Response, Leaf: l, Live: l.Value}}}}
	}
	type classify func(dc *DriftCache, earlier []Earlier, recorded, now string) Learned
	for name, c := range map[string]classify{
		"ClassifyIn": func(dc *DriftCache, earlier []Earlier, recorded, now string) Learned {
			return ClassifyIn(dc, []string{p}, str(now), str(recorded), at, earlier, nil, nil, w)[0]
		},
		"ClassifyFollowing": func(dc *DriftCache, earlier []Earlier, recorded, now string) Learned {
			return ClassifyFollowing(dc, []string{p}, str(now), str(recorded), at, earlier, nil, nil, w, nil)[0]
		},
	} {
		for _, mode := range []struct {
			name string
			dc   func() *DriftCache
		}{{"client test", clientTestCache}, {"keploy test", clockedDrifts}} {
			for _, row := range []struct {
				what, mock string
				rec, live  string
				earlier    []Earlier
			}{
				{"an earlier test's id holds the recorded id's third group", "mock-231", rec, live, served("mock-231", false, text("body.cursor.firstBatch.0._id", other))},
				{"an earlier test's id holds the fresh id's third group", "mock-279", rec, live, served("mock-279", false, text("body.cursor.firstBatch.0.jobId", "0c1d2e3f-4a5b-4b8a-8e9f-a0b1c2d3e4f5"))},
				{"the start-up's id holds the fresh id's fourth group", "mock-448", rec, live, served("mock-448", true, text("body.request_id", "5a6b7c8d-1e2f-4a3b-9e21-0f1e2d3c4b5a"))},
				{"a start-up key is a word of one character of the id", "mock-188", tokRec, tokLive, served("mock-188", true, Leaf{Path: "body{keys}", Type: Keys, Value: "_id\nn\nok"})},
				{"an earlier test's field holds four digits of the id alone", "mock-231", "ed35a78f-2486-48d1-bc3d-a07311678c73", live, served("mock-231", false, text("body.count", "2486"))},
			} {
				base := c(mode.dc(), nil, row.rec, row.live)
				got := c(mode.dc(), row.earlier, row.rec, row.live)
				// Noise names the place it read past (learning is legible);
				// anything else is as with no such exchange.
				named := strings.Contains(got.Why, "stands in "+row.mock+" at ") && strings.Contains(got.Why, " by chance: a word of ") && strings.HasSuffix(got.Why, "and keploy learns it as varying")
				if got.Kind != base.Kind || got.Bindable != base.Bindable || got.Kind == KindNoise && !named || got.Kind != KindNoise && strings.Contains(got.Why, row.mock) {
					t.Errorf("%s, %s: %s: %s bindable=%v (%q); want as with no such exchange: %s bindable=%v, noise naming %s", name, mode.name, row.what, got.Kind, got.Bindable, got.Why, base.Kind, base.Bindable, row.mock)
				}
			}
			for _, row := range []struct {
				what      string
				rec, live string
				earlier   []Earlier
			}{
				{"the id whole", rec, live, served("mock-231", false, text("body.cursor.firstBatch.0._id", rec))},
				{"an 8-hex group of the id", rec, live, served("mock-231", false, text("body.cursor.firstBatch.0._id", "ed35a78f-1111-4222-8333-944445555666"))},
				{"two groups of the id together", rec, live, served("mock-231", false, text("body.cursor.firstBatch.0._id", "ffff0000-905c-48d1-aaaa-bbbbccccdddd"))},
				{"a group of an id that kept another", rec, "ed35a78f-7e6d-4b8a-9e21-5c6d7e8f9a0b", served("mock-231", false, text("body.cursor.firstBatch.0._id", other))},
				{"a counter of an id that kept its prefix", "order-17", "order-18", served("mock-231", false, text("body.value.seq", "17"))},
				{"the value whole a short word", "48d1", "7e6d", served("mock-231", false, text("body.cursor.firstBatch.0._id", other))},
				{"a 10-digit word of a fresh id, alone there", "kq-1730361600-z", "pw-2840472711-y", served("mock-231", true, text("body.cursor.firstBatch.0.n", "1730361600"))},
				{"a word of letters and digits of the id alone there (aloneBits)", rec, live, served("mock-231", false, text("body.cursor.firstBatch.0.code", "905c"))},
			} {
				got := c(mode.dc(), row.earlier, row.rec, row.live)
				variable := got.Kind == KindVariable && got.From.Mock == "mock-231"
				refused := got.Kind == KindUnaccounted && !got.Bindable && strings.Contains(got.Why, "mock-231")
				if !variable && !refused {
					t.Errorf("FALSE GREEN? %s, %s, %s in mock-231: %s from %q bindable=%v (%q); want a variable of mock-231 or refused naming it", name, mode.name, row.what, got.Kind, got.From.Mock, got.Bindable, got.Why)
				}
			}
		}
	}
}

// wordBits reads a word's alphabet by the classes of its characters: the
// bound (chanceBits) falls between 4 and 8 hex digits, 9 and 10 decimal
// ones, 5 and 6 letters and digits of both cases, 6 and 7 lower-case ones.
func TestWordBitsBoundsAChanceWord(t *testing.T) {
	for _, row := range []struct {
		word   string
		chance bool
	}{
		{"n", true}, {"48d1", true}, {"905c", true}, {"4123", true}, {"a1b2c3", true}, {"123456789", true}, {"Xa9bT", true}, {"status", true},
		{"ed35a78f", false}, {"a07311678c73", false}, {"1730361600", false}, {"Xa9bT2", false}, {"a1b2c3d", false}, {"stopped", false}, {"ж", true}, {"жжжж", false},
	} {
		if got := wordBits(row.word) < chanceBits; got != row.chance {
			t.Errorf("%q: %.1f bits, chance=%v; want chance=%v", row.word, wordBits(row.word), got, row.chance)
		}
	}
}

// idReading names the reading whose words are a value's ids whole (idByte):
// the chance rule reads a place's ids by it (byChance), and a wrong index
// would read no ids there.
func TestIDReadingIsTheIDByteReading(t *testing.T) {
	if reflect.ValueOf(readings[idReading]).Pointer() != reflect.ValueOf(idByte).Pointer() {
		t.Fatalf("readings[%d] is not idByte", idReading)
	}
}

// A mixed-case word carries its bits in the value's form folded to lower
// case too ("Xa9bT2", 35.7 bits, read as "xa9bt2", 31): no chance word in
// either form; a word of fewer bits as the value holds it is one in both.
func TestAFoldedWordCarriesTheBitsOfTheValuesOwn(t *testing.T) {
	for _, row := range []struct {
		rec, live, word string
		chance          bool
	}{
		{"Xa9bT2-Qz7kPw", "Lm4rT9-Zc8dVy", "xa9bt2", false},
		{"Xa9bT2-Qz7kPw", "Lm4rT9-Zc8dVy", "Xa9bT2", false},
		{"Xa9bT-Qz7kPw", "Lm4rT-Zc8dVy", "xa9bt", true},
	} {
		v := clockedDrifts().of(row.rec, row.live)
		if _, ok := v.idParts[0][row.word]; !ok {
			t.Fatalf("%q: %q is no word of an id that changed whole: %v", row.rec, row.word, v.idParts[0])
		}
		if _, got := v.chanceWord(row.word, true); got != row.chance {
			t.Errorf("%q in %q: chance=%v (%.1f bits); want %v", row.word, row.rec, got, v.partBits(row.word, true), row.chance)
		}
	}
}

// A place that moved with the value holds a word of what the value changed
// on the other side (otherSideWords: of the recorded side's words, those it
// holds now), and a place read with no text at all is no place a word
// stands in by chance (fail-closed).
func TestAChanceWordReadsTheOtherSidesWordsAndNeedsAText(t *testing.T) {
	const rec, live = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
	v := clockedDrifts().of(rec, live)
	has := func(ws []string, w string) bool {
		for _, x := range ws {
			if x == w {
				return true
			}
		}
		return false
	}
	if r, l := v.otherSideWords(true), v.otherSideWords(false); !has(r, "7e6d") || has(r, "905c") || !has(l, "905c") || has(l, "7e6d") {
		t.Errorf("other side's words: of the recorded side %v, of the live side %v; want the live words, then the recorded ones", r, l)
	}
	if !chanceIn(v, "48d1", true, "x-48d1") {
		t.Errorf("48d1 alone in another id: not by chance; want by chance")
	}
	if chanceIn(v, "48d1", true, "x-48d1", "7e6d") {
		t.Errorf("48d1 where a text holds the live word 7e6d (the place moved with the value): by chance; want evidence")
	}
	if chanceIn(v, "48d1", true) {
		t.Errorf("48d1 in a place read with no text: by chance; want evidence (fail-closed)")
	}
}

// api-server's T2 suite (TestTestRunDetails, cross-test-word-coincidence
// s3): a testSuiteEvents insert's Keploy-Test-Name, a fresh UUIDv4, was
// refused because its fourth group "9457" stood in the second group of
// another id in mock-11208, an aggregate of 564 KB holding 398 UUIDs that
// record's full round gave the application before the call: "a word of its
// recorded value (its characters 20 to 23) appears in mock-11208, a
// recording record's full round gave the application before the call that
// is another test's". The word stands there by chance (chanceBits), but the
// check that the place moved with the value read the whole answer, as
// recorded and as F gave it, for any word of the value's other side: in
// an answer that big a 4-hex group of the fresh id stands somewhere by
// chance about one time in five. A place moved with the value where the id
// holding the word holds the value's other word in its place, or the field
// holding it does as the other run gave it: only there.
//
// Everything that is evidence stays so: the id that holds the word holding,
// as the other run gave it, the value's other word in its place, and an
// answer holding a word of the value's other side not by chance (its 8-hex
// group: the place gave the application the value's id). Another test's
// answer is read as words (givenWords), not field by field, so an id of F's
// view that holds the value's other word where the recorded answer's id
// holds the word, in the same shape, is read as the place that moved,
// whatever else of it changed, where the answer holds few ids of that
// shape. Among thousands (all of an app's testSuiteEvents) a fresh UUID's
// group stands in some id's place by chance nearly always: the pair is no
// evidence there (movedID counts the ids of the shape each run gave).
func TestAShortWordOfAFreshIdInABigAnswerIsNoEvidenceWhereTheAnswerHoldsAnotherGroupElsewhere(t *testing.T) {
	const (
		p    = "body.docs.msgs.0.delta.steps.0.headers.Keploy-Test-Name"
		rec  = "83f5cd07-3ac1-4d09-9457-254ee4b744ec"
		live = "6e1b2c3d-7f80-4a91-b2c3-d4e5f6a7b8c9"
		// Another test's id: its second group is the recorded id's fourth.
		held = "a8a8b96f-9457-42d1-b67b-0d57d8146f85"
		// Another id of the answer: its third group is the fresh id's third.
		third = "1c2d3e4f-5a6b-4a91-8c7d-9e0f1a2b3c4d"
		// What F gave at the message id instead.
		fresh = "f0e1d2c3-b4a5-4968-8776-655443322110"
		// The message id, its second group swapped for the fresh id's fourth.
		swapped = "a8a8b96f-b2c3-42d1-b67b-0d57d8146f85"
	)
	before := r1Insert.Add(-time.Minute)
	answer := func(id, body string) *models.Mock {
		m := receiveOf("mock-11208", id, before)
		m.Spec.HTTPResp.Body = strings.Replace(m.Spec.HTTPResp.Body, `"Body": "S12 message"`, `"Body": "`+body+`"`, 1)
		return m
	}
	// list is an answer whose message id is id, listing ids too, each its
	// own leaf.
	list := func(id, ids string) *models.Mock {
		m := receiveOf("mock-11208", id, before)
		m.Spec.HTTPResp.Body = strings.Replace(m.Spec.HTTPResp.Body, `"Body": "S12 message"`, `"Body": "S12 message", "Others": ["`+strings.ReplaceAll(ids, " ", `", "`)+`"]`, 1)
		return m
	}
	type classify func(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier) []Learned
	for name, c := range map[string]classify{"ClassifyGiven": ClassifyGiven, "ClassifyFollowing": ClassifyFollowing} {
		for _, row := range []struct {
			what     string
			m, view  *models.Mock
			evidence bool
		}{
			{"no id of the answer holds a group of the fresh id", answer(held, "S12 message"), answer(fresh, "S12 message"), false},
			{"the recorded answer holds a group of the fresh id in another id", answer(held, third), answer(fresh, "S12 message"), false},
			{"F's view holds a group of the fresh id in another id", answer(held, "S12 message"), answer(fresh, third), false},
			{"F's view holds the recorded answer's id with the word swapped for the fresh id's", answer(held, "S12 message"), answer(swapped, "S12 message"), true},
			{"F's view holds a fresh id holding the fresh id's group where the recorded answer's id holds the word", answer(held, "S12 message"), answer("0f0f0f0f-b2c3-4abc-8def-0123456789ab", "S12 message"), true},
			{"the recorded answer holds the fresh id's 8-hex group in another id", answer(held, "6e1b2c3d-0000-4111-8222-933334444555"), answer(fresh, "S12 message"), true},
			// api-server T2 (s8 trace, mock-11206: all of an app's
			// testSuiteEvents, 3706 ids as F gave it): among that many
			// fresh UUIDs one holds a 4-hex group of the value's fresh id
			// in the recorded id's place by chance most times, so finding
			// one there is no evidence the place moved with the value
			// (movedID).
			{"F's view of an answer of hundreds of ids holds, at another of its ids, a fresh id holding the fresh id's group where the recorded answer's id holds the word", list(held, manyIDs("rec", 300, rec, live, held)), list(fresh, manyIDs("F", 299, rec, live, held)+" 0f0f0f0f-b2c3-4abc-8def-0123456789ab"), false},
		} {
			base := c(clockedDrifts(), []string{p}, strLeaf(p, live), strLeaf(p, rec), r1Insert, nil, nil, nil, nil, nil)[0]
			dc := clockedDrifts()
			x := dc.RawOf(row.m, true).ServedAs(3).ServedLive(dc.RawOf(row.view, true)).OfAnotherTest()
			got := c(dc, []string{p}, strLeaf(p, live), strLeaf(p, rec), r1Insert, nil, []Raw{x}, nil, nil, nil)[0]
			if row.evidence {
				variable := got.Kind == KindVariable && got.From.Mock == "mock-11208"
				refused := got.Kind == KindUnaccounted && !got.Bindable && strings.Contains(got.Why, "mock-11208")
				if !variable && !refused {
					t.Errorf("FALSE GREEN? %s: %s: %s from %q bindable=%v (%q); want a variable of mock-11208 or refused naming it", name, row.what, got.Kind, got.From.Mock, got.Bindable, got.Why)
				}
				continue
			}
			named := strings.Contains(got.Why, "stands in mock-11208 by chance: a word of ") && strings.HasSuffix(got.Why, "and keploy learns it as varying")
			if got.Kind != base.Kind || got.Bindable != base.Bindable || got.Kind == KindNoise && !named || got.Kind != KindNoise && strings.Contains(got.Why, "mock-11208") {
				t.Errorf("%s: %s: %s bindable=%v (%q); want as with no such recording: %s bindable=%v, noise naming mock-11208", name, row.what, got.Kind, got.Bindable, got.Why, base.Kind, base.Bindable)
			}
		}
		// Where F served the answer and keploy cannot tell what it gave
		// the application there (a view it could not read, or none its
		// adapter stated), the place may have moved with the value: the
		// recorded word found there stays evidence (fail-closed).
		for what, serve := range map[string]func(Raw) Raw{
			"F served it changed and keploy could not read its view": func(x Raw) Raw { return x.ServedChanged() },
			"F served it and its adapter stated no view":             func(x Raw) Raw { return x },
		} {
			dc := clockedDrifts()
			x := serve(dc.RawOf(answer(held, "S12 message"), true).ServedAs(3)).OfAnotherTest()
			got := c(dc, []string{p}, strLeaf(p, live), strLeaf(p, rec), r1Insert, nil, []Raw{x}, nil, nil, nil)[0]
			// The recorded word found there is refused first, before
			// any word F may have given there (servedWhy): a recorded word
			// read past by chance would leave only the latter's refusal.
			refused := got.Kind == KindUnaccounted && !got.Bindable && strings.HasPrefix(got.Why, "a word of its recorded value (its characters 20 to 23) appears in mock-11208")
			if !refused || strings.Contains(got.Why, " by chance") {
				t.Errorf("FALSE GREEN? %s: %s: %s from %q bindable=%v (%q); want refused on the recorded word found in mock-11208, nothing read past by chance", name, what, got.Kind, got.From.Mock, got.Bindable, got.Why)
			}
		}
	}
}

// A place moved with the value only where a word of what the value changed
// on the other side stands for the word found: at the id holding it, that
// word stands between the same separators (movedID: "x-48d1-y" recorded,
// "q-7e6d-z" as the other run gave it), at the field holding it as the
// other run gave it, or at its leaf where the field holds a JSON document
// (atPlace), or anywhere where it stands not by chance (movedWord: an 8-hex
// group of the fresh id). A 4-hex group of the fresh id inside another id
// of the place, at another field, or at another leaf of a body, shows
// nothing.
func TestAPlaceMovedWithTheValueOnlyWhereTheOtherWordStandsForTheWordFound(t *testing.T) {
	const rec, live = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
	dc := clockedDrifts()
	v := dc.of(rec, live)
	moved := v.otherSideWords(true)
	chance := []string{`{"a":"x-48d1-y","b":"c0c0c0c0-1111-4b8a-8222-933334444555"}`}
	if movedID(v, "48d1", true, chance, chance) {
		t.Errorf("4b8a inside another id of another shape: read as the id holding 48d1 moved; want not")
	}
	for _, other := range []string{`{"a":"x-7e6d-y"}`, `{"z":"q-7e6d-1700000555"}`, `["n","r-7e6d-s"]`} {
		if !movedID(v, "48d1", true, chance, []string{other}) {
			t.Errorf("x-48d1-y recorded, %s as the other run gave it: not read as moved; want moved", other)
		}
	}
	if movedID(v, "48d1", true, chance, []string{`"q-7e6d"`, `"7e6d-q-r"`, `"q7e6d-r"`}) {
		t.Errorf("7e6d in ids of another shape, or inside a longer word: read as moved; want not")
	}
	// The ids are read by shape, not paired: the more ids of that shape the
	// other run gave there, the likelier one holds 7e6d in the place of
	// 48d1 by chance. Two 4-hex words carry about 41 bits: among a hundred
	// ids "x-....-y" the pair stays evidence, among two thousand it falls
	// below chanceBits.
	many := func(n int) []string {
		out := []string{`{"a":"x-7e6d-y"}`}
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf(`{"a":"x-%04x-y"}`, i))
		}
		return out
	}
	if !movedID(v, "48d1", true, chance, many(100)) {
		t.Errorf("x-48d1-y recorded, x-7e6d-y among 100 ids of its shape as the other run gave it: not read as moved; want moved")
	}
	if movedID(v, "48d1", true, chance, many(2000)) {
		t.Errorf("x-48d1-y recorded, x-7e6d-y among 2000 ids of its shape as the other run gave it: read as moved; want by chance")
	}
	has := func(texts ...string) func(string) bool {
		return func(w string) bool {
			for _, s := range texts {
				if ContainsToken(s, w) {
					return true
				}
			}
			return false
		}
	}
	all := func(texts ...string) func(string) []string { return func(string) []string { return texts } }
	if movedWord(v, true, has(chance...), all(chance...)) {
		t.Errorf("4b8a inside another id: read as a place that moved; want by chance")
	}
	long := []string{`{"b":"3f0c9a12-0000-4111-8222-933334444555"}`}
	if !movedWord(v, true, has(long...), all(long...)) {
		t.Errorf("the fresh id's 8-hex group in the place: not read as moved; want moved")
	}
	if !movedWord(v, true, has("4b8a"), func(string) []string { return nil }) {
		t.Errorf("a word of the other side with no text read: not moved; want moved (fail-closed)")
	}
	for _, row := range []struct {
		what      string
		rec, live Raw
		want      bool
	}{
		{"the field holding 48d1 holds 7e6d as F gave it", eRow("x-48d1", "1", "2"), eRow("q-7e6d-z", "1", "2"), true},
		{"7e6d at another field", eRow("x-48d1", "1", "2"), eRow("q", "7e6d", "2"), false},
		{"no field holding 48d1", eRow("x", "1", "2"), eRow("x", "7e6d", "2"), false},
	} {
		moves := fieldMovesOf(row.rec, dc.searched(row.rec, false)[0], []Raw{row.live}, []string{dc.searched(row.live, false)[0]})
		if got := atPlace(moves, "48d1", true, moved); got != row.want {
			t.Errorf("%s: moved %v; want %v", row.what, got, row.want)
		}
		// Read from the other side: 7e6d, as F gave it, at the field the
		// recording gave 48d1 at.
		if got := atPlace(moves, "7e6d", false, v.otherSideWords(false)); got != row.want && row.what != "no field holding 48d1" {
			t.Errorf("%s, read from F's side: moved %v; want %v", row.what, got, row.want)
		}
	}
	// A body is one field of many leaves: a place moved only at the leaf
	// holding the word.
	doc := func(json string) Raw {
		m := receiveOf("mock-b", "id", r1Insert.Add(-time.Minute))
		m.Spec.HTTPResp.Body = json
		return dc.RawOf(m, true)
	}
	body := func(a, b string) Raw { return doc(`{"a":"` + a + `","b":["` + b + `"]}`) }
	for _, row := range []struct {
		what      string
		rec, live Raw
		want      bool
	}{
		{"the leaf holding 48d1 holds 7e6d as F gave it", body("x-48d1", "1"), body("q-7e6d-z", "1"), true},
		{"7e6d at another leaf of the body", body("x-48d1", "1"), body("x-48d1", "q-7e6d-z"), false},
		{"7e6d at the leaf, which F gave holding 48d1 too", body("x-48d1", "1"), body("48d1 7e6d", "1"), true},
		// Leaves that do not pair are read as the whole field (fail-closed).
		{"7e6d in the key F gave where the recorded key held 48d1", doc(`{"x-48d1":1}`), doc(`{"q-7e6d-z":1}`), true},
		{"7e6d at a leaf of another path", doc(`{"a":"x-48d1"}`), doc(`{"a":{"id":"q-7e6d-z"}}`), true},
		{"7e6d at one of two leaves of one path", doc(`{"a.b":"x-48d1","a":{"b":"1"}}`), doc(`{"a.b":"q-7e6d-z","a":{"b":"1"}}`), true},
		{"7e6d where the separators changed", doc(`{"a":"x-48d1"}`), doc(`{"a":"q_7e6d"}`), true},
	} {
		moves := fieldMovesOf(row.rec, dc.searched(row.rec, false)[0], []Raw{row.live}, []string{dc.searched(row.live, false)[0]})
		if got := atPlace(moves, "48d1", true, moved); got != row.want {
			t.Errorf("body: %s: moved %v; want %v", row.what, got, row.want)
		}
	}
	for text, want := range map[string]bool{`{"a":[1,{"b":"x"}],"c":null}`: true, `{"a":`: false, `1 2`: false, `plain`: false} {
		spans, ok := leafSpans(text)
		if ok != want {
			t.Errorf("leafSpans(%s): ok %v; want %v", text, ok, want)
		}
		if ok && (len(spans) != 3 || strings.TrimLeft(text[spans[".a[1].b"][0]:spans[".a[1].b"][1]], ":,") != `"x"` || text[spans[".a[0]"][0]:spans[".a[0]"][1]] != "[1" && strings.TrimLeft(text[spans[".a[0]"][0]:spans[".a[0]"][1]], ":,[") != "1") {
			t.Errorf("leafSpans(%s) = %v; want .a[0], .a[1].b, .c, each its scalar", text, spans)
		}
	}
}

// Each place the chance rule reads (a leaf, a recording of the call's own
// test, another test's recording) reads it the same way (byChanceAt), so a
// check of the place one makes the others make too (review of the whole
// head, 2026-10-31):
//   - a connection's start-up is evidence whole in another test's
//     recording too: keploy keeps no other start-up beside it there to
//     tell which of its fields a swap of start-ups can change (swapHolds),
//     so a backend pid "81" a value copied is never read past by chance;
//   - a field that moved with the value, holding the recorded word "48d1"
//     as recorded and the fresh word "7e6d" as F gave it, in ids of
//     another shape ("x-48d1", "q-7e6d-z"), is a copy in another test's
//     recording as in the call's own (atPlace), from either side.
func TestEveryPlaceReadsTheChanceRuleAlike(t *testing.T) {
	type classify func(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier) []Learned
	const p = "bind.$8"
	shape := func(v string) Shape { return Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: v}}} }
	for name, c := range map[string]classify{"ClassifyGiven": ClassifyGiven, "ClassifyFollowing": ClassifyFollowing} {
		run := func(rec, live string, raw ...Raw) Learned {
			got := c(clockedDrifts(), []string{p}, shape(live), shape(rec), eRecAt, nil, raw, nil, nil, nil)
			if len(got) != 1 {
				t.Fatalf("%s: %q recorded, %q in F: %+v", name, rec, live, got)
			}
			return got[0]
		}
		startUps := []Raw{
			RawOf(eSession("mock-hs-81", 900*time.Millisecond, 81), false).ServedAs(1).ServedAsRecorded().OfAnotherTest(),
			RawOf(eSession("mock-hs-82", 850*time.Millisecond, 82), false).ServedAs(2).ServedAsRecorded().OfAnotherTest(),
		}
		if got := run("81-ab12cd34ef", "82-c0de5eed01", startUps...); got.Kind == KindNoise || strings.Contains(got.Why, " by chance") || !strings.Contains(got.Why, "mock-hs-8") {
			t.Errorf("FALSE GREEN? %s: a pid of another test's start-up: %s (%q); want refused naming the start-up, nothing read past by chance", name, got.Kind, got.Why)
		}
		const rec, live = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		for _, row := range []struct {
			what     string
			x        Raw
			evidence bool
		}{
			{"the call's own test's row, F gave 7e6d at the cell holding 48d1", eRow("x-48d1", "1", "2").ServedAs(6).ServedLive(eRow("q-7e6d-z", "1", "2")), true},
			{"another test's row, F gave 7e6d at the cell holding 48d1", eRow("x-48d1", "1", "2").ServedAs(6).ServedLive(eRow("q-7e6d-z", "1", "2")).OfAnotherTest(), true},
			{"another test's row, F gave 7e6d at another cell", eRow("x-48d1", "1", "2").ServedAs(6).ServedLive(eRow("x-48d1", "q-7e6d-z", "2")).OfAnotherTest(), false},
			{"the call's own test's row, F gave 7e6d at another cell", eRow("x-48d1", "1", "2").ServedAs(6).ServedLive(eRow("x-48d1", "q-7e6d-z", "2")), false},
		} {
			got := run(rec, live, row.x)
			chance := got.Kind == KindNoise && strings.Contains(got.Why, "stands in mock-6") && strings.Contains(got.Why, " by chance")
			if row.evidence && (got.Kind == KindNoise || strings.Contains(got.Why, " by chance") || !strings.Contains(got.Why, "mock-6")) {
				t.Errorf("FALSE GREEN? %s: %s: %s (%q); want a variable of mock-6 or refused naming it", name, row.what, got.Kind, got.Why)
			}
			if !row.evidence && !chance {
				t.Errorf("%s: %s: %s (%q); want noise naming 48d1 read past in mock-6 by chance", name, row.what, got.Kind, got.Why)
			}
		}
	}
}

// A place's fields are read once for the test (fieldMovesIn: the search
// reads the place for each word of each value), and anew where F's views of
// it are others: a cache keyed by the recording alone would hand a later
// call the fields of a view F gave at another cell, and a field that moved
// with the value would be read past by chance (review of the whole head,
// R2/R3, 2026-10-31).
func TestAPlaceIsReadOnceForTheTestWithTheViewsFGaveIt(t *testing.T) {
	type classify func(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier) []Learned
	const p = "bind.$8"
	const rec, live = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
	shape := func(v string) Shape { return Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: v}}} }
	moved := func(other bool) Raw {
		x := eRow("x-48d1", "1", "2").ServedAs(6).ServedLive(eRow("q-7e6d-z", "1", "2"))
		if other {
			x = x.OfAnotherTest()
		}
		return x
	}
	still := func(other bool) Raw {
		x := eRow("x-48d1", "1", "2").ServedAs(6).ServedLive(eRow("x-48d1", "q-7e6d-z", "2"))
		if other {
			x = x.OfAnotherTest()
		}
		return x
	}
	for name, c := range map[string]classify{"ClassifyGiven": ClassifyGiven, "ClassifyFollowing": ClassifyFollowing} {
		for _, other := range []bool{false, true} {
			whose := map[bool]string{false: "the call's own test's row", true: "another test's row"}[other]
			dc := clockedDrifts()
			run := func(x Raw) Learned {
				got := c(dc, []string{p}, shape(live), shape(rec), eRecAt, nil, []Raw{x}, nil, nil, nil)
				if len(got) != 1 {
					t.Fatalf("%s: %s: %+v", name, whose, got)
				}
				return got[0]
			}
			chance := func(got Learned) bool {
				return got.Kind == KindNoise && strings.Contains(got.Why, "stands in mock-6") && strings.Contains(got.Why, " by chance")
			}
			before := fieldMoveReads.Load()
			if got := run(still(other)); !chance(got) {
				t.Errorf("%s: %s, F gave 7e6d at another cell: %s (%q); want noise naming 48d1 read past in mock-6 by chance", name, whose, got.Kind, got.Why)
			}
			first := fieldMoveReads.Load() - before
			if got := run(still(other)); !chance(got) {
				t.Errorf("%s: %s, F gave 7e6d at another cell, the test's second call: %s (%q); want noise as at its first", name, whose, got.Kind, got.Why)
			}
			if again := fieldMoveReads.Load() - before - first; first == 0 || again != 0 {
				t.Errorf("%s: %s: the place's fields read %d times at the test's first call, %d at its second; want read at the first and kept for the test", name, whose, first, again)
			}
			// F's view of the same recording gives 7e6d at the cell holding
			// 48d1 now: the field moved with the value.
			if got := run(moved(other)); got.Kind == KindNoise || strings.Contains(got.Why, " by chance") || !strings.Contains(got.Why, "mock-6") {
				t.Errorf("FALSE GREEN? %s: %s, F's view now gives 7e6d at the cell holding 48d1, read with the cache of an earlier view: %s (%q); want a variable of mock-6 or refused naming it", name, whose, got.Kind, got.Why)
			}
		}
	}
}

// manyIDs are n distinct v4-shaped UUIDs, made from seed, space-separated,
// none of which holds a word of avoid: an answer listing many records.
func manyIDs(seed string, n int, avoid ...string) string {
	words := map[string]bool{}
	for _, a := range avoid {
		for _, w := range strings.Split(a, "-") {
			words[w] = true
		}
	}
	var out []string
	for i := 0; len(out) < n; i++ {
		h := sha256.Sum256([]byte(seed + "/" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + hex.EncodeToString([]byte{byte(i), byte(i >> 8), byte(i >> 16)})))
		x := hex.EncodeToString(h[:16])
		id := x[:8] + "-" + x[8:12] + "-4" + x[13:16] + "-" + string("89ab"[h[16]%4]) + x[17:20] + "-" + x[20:32]
		ok := true
		for _, w := range strings.Split(id, "-") {
			if words[w] {
				ok = false
			}
		}
		if ok {
			out = append(out, id)
		}
	}
	return strings.Join(out, " ")
}

// api-server's T2 suite (s9 head-b, TestStopGenerationWhileExecuting): a
// jobEvents insert's response.response_time, a duration in nanoseconds,
// changed to 29782478, and keploy refused it: "a word of what record's full
// round saw in it (a part of it in another form) appears earlier in the
// test, in mock-2571 at body.password". The eight digits read as hex decode
// to the text ")x$x" (b64Text), and its word "x" stood in a password hash.
// One 8-digit number in six reads as hex text so (16.5%), and a word of one
// or two characters of it stands in nearly every recording. A word of the
// text a run of the value decodes to stands in the value only inside that
// run, in another form: where the run changed whole it is a part of it, and
// the chance rule (chanceBits) reads it as it reads a word of any id that
// changed whole.
//
// Evidence stays: the run whole there, a decoded text that is one word
// whole, a decoded word of at least 32 bits, and a decoded word beside one
// the other side holds (a counter "n:18" of "n:17").
func TestAShortWordOfARunsDecodedTextIsNoEvidenceWhereTheRunChangedWhole(t *testing.T) {
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	w := &Window{
		Rec: Span{From: at.Add(-900 * time.Millisecond), To: at},
		F:   Span{From: at.Add(60_500 * time.Millisecond), To: at.Add(61_400 * time.Millisecond)},
	}
	const p = "body.docs.msgs.0.event.response.response_time"
	if d, ok := decodeText("29782478"); !ok || d != ")x$x" {
		t.Fatalf("29782478 read as hex: %q %v; want the text \")x$x\"", d, ok)
	}
	shape := func(typ Type, v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: typ, Value: v}}}
	}
	text := func(path, v string) Leaf { return Leaf{Path: path, Type: String, Value: v} }
	// An exchange served before the call, which did not drift: of the
	// call's own test, or an earlier test's (Run).
	served := func(run bool, l Leaf) []Earlier {
		return []Earlier{{Mock: "mock-2571", At: at.Add(-2 * time.Second), Run: run, Payload: payloadOf("mock-2571", at.Add(-2*time.Second)), Seen: []Seen{{Side: Response, Leaf: l, Live: l.Value}}}}
	}
	hash := text("body.password", "$2a$10$Qz7kP.x.Xa9bT2wLm4rTqZc8dV1yAbCdEfGhIjKlMnOpQrStUv")
	hexOf := func(s string) string { return hex.EncodeToString([]byte(s)) }
	for _, mode := range []struct {
		name string
		dc   func() *DriftCache
	}{{"client test", clientTestCache}, {"keploy test", clockedDrifts}} {
		for _, row := range []struct {
			what      string
			typ       Type
			rec, live string
			earlier   []Earlier
		}{
			{"an earlier place of the test holds a word of the number's hex reading", Number, "5586980", "29782478", served(false, hash)},
			{"an earlier test holds a word of the number's hex reading", Number, "5586980", "29782478", served(true, hash)},
			{"an earlier place holds a word of the recorded number's hex reading", Number, "29782478", "5586980", served(false, hash)},
		} {
			base := ClassifyIn(mode.dc(), []string{p}, shape(row.typ, row.live), shape(row.typ, row.rec), at, nil, nil, nil, w)[0]
			got := ClassifyIn(mode.dc(), []string{p}, shape(row.typ, row.live), shape(row.typ, row.rec), at, row.earlier, nil, nil, w)[0]
			named := strings.Contains(got.Why, "stands in mock-2571 at ") && strings.Contains(got.Why, " by chance: a word of ") && strings.HasSuffix(got.Why, "and keploy learns it as varying")
			if got.Kind != base.Kind || got.Bindable != base.Bindable || got.Kind == KindNoise && !named || got.Kind != KindNoise && strings.Contains(got.Why, "mock-2571") {
				t.Errorf("%s: %s: %s bindable=%v (%q); want as with no such exchange: %s bindable=%v, noise naming mock-2571", mode.name, row.what, got.Kind, got.Bindable, got.Why, base.Kind, base.Bindable)
			}
		}
		for _, row := range []struct {
			what      string
			typ       Type
			rec, live string
			earlier   []Earlier
		}{
			{"the number whole", Number, "5586980", "29782478", served(false, text("body.n", "29782478"))},
			{"a decoded text that is one word whole", String, hexOf("abce"), hexOf("abcd"), served(false, text("body.code", "abcd"))},
			{"a decoded word of at least 32 bits", String, hexOf("k=Wm4rTq9Z;"), hexOf("k=Xa9bT2wL;"), served(false, text("body.token", "Xa9bT2wL"))},
			{"a decoded word beside one the other side holds", String, hexOf("n:17"), hexOf("n:18"), served(false, text("body.seq", "18"))},
		} {
			got := ClassifyIn(mode.dc(), []string{p}, shape(row.typ, row.live), shape(row.typ, row.rec), at, row.earlier, nil, nil, w)[0]
			variable := got.Kind == KindVariable && got.From.Mock == "mock-2571"
			refused := got.Kind == KindUnaccounted && !got.Bindable && strings.Contains(got.Why, "mock-2571")
			if !variable && !refused {
				t.Errorf("FALSE GREEN? %s: %s in mock-2571: %s from %q bindable=%v (%q); want a variable of mock-2571 or refused naming it", mode.name, row.what, got.Kind, got.From.Mock, got.Bindable, got.Why)
			}
		}
	}
}
