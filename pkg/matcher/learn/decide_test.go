package learn

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// The seams as decide.go declares them: package variables are all set
// before any init runs, so these hold the defaults even where another file's
// init registers an order or a clock.
var declaredDecideBy, declaredClockInWindow, declaredClockSinceBegin = decideBy, clockInWindow, clockSinceBegin

// TestReplayAndLearnShareOneWordRule: variables.Words, the word rule
// replay reads, finds the runs learn's isWord finds.
func TestReplayAndLearnShareOneWordRule(t *testing.T) {
	for c := 0; c < 256; c++ {
		if s := string([]byte{byte(c)}); (len(variables.Words(s)) == 1) != isWord(byte(c)) {
			t.Errorf("byte %#x: replay's word rule and learn's isWord (%v) differ", c, isWord(byte(c)))
		}
	}
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

// TestDecideWithNoOrderRefusesEveryChangedWord: with no order
// registered, Decide refuses every drifted word as one keploy cannot check
// yet, a leaf whose words do not pair by position whole, and never gives a
// Clock, a Value or a fresh word.
func TestDecideWithNoOrderRefusesEveryChangedWord(t *testing.T) {
	was := decideBy
	decideBy = refuseAll
	t.Cleanup(func() { decideBy = was })
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
		Rec:   Shape{Leaves: leaves("body.id", "order-17-a-5", "body.sep", "a-b", "header.date", "2026-11-01T10:00:00Z", "body.more", "a b", "body.mix", "a-17", "body.nul", "a\x00\x00b", "body.tail", "a-17", "body.when", "17-a", "body.lead", "-a17", "body.was", "17-a", "body.first", "17-a"), Body: map[string]interface{}{}},
		Live:  Shape{Leaves: leaves("body.id", "order-42-a-6", "body.sep", "a_b", "header.date", "2026-11-01T10:00:05Z", "body.more", "a b c", "body.mix", "a_42", "body.added", "new", "body.nul", "\x00c\x00d", "body.tail", "a-42-", "body.when", "42-a", "body.lead", "a42", "body.was", "42-a", "body.first", "42-a"), Body: map[string]interface{}{}},
		Drift: []string{"body.id", "body.sep", "header.date", "body.more", "body.gone", "body.mix", "body.added", "body.nul", "body.tail", "body.when", "body.lead", "body.was", "body.first"},
	}
	// Typed an instant in one round alone: still not split into words.
	when := in.Live.Leaves["body.when"]
	when.Type = Instant
	in.Live.Leaves["body.when"] = when
	recWhen := in.Rec.Leaves["body.was"]
	recWhen.Type = Instant
	in.Rec.Leaves["body.was"] = recWhen
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
		if r.Kind != RefusalCannotCheck || r.Test != "test-1" || r.Mock != "mock-3" || r.Recorded != d.Recorded || r.Full != d.Full || len(r.Edits) != 1 {
			t.Errorf("refusal %+v", r)
		}
		// A word's edit is noise at its whole field, never at the word.
		if want := (Edit{Kind: EditNoiseAtField, Mock: "mock-3", Field: r.Field, Word: -1}); len(r.Edits) == 1 && r.Edits[0] != want {
			t.Errorf("%s word %d: edit %+v, want %+v", r.Field, r.Word, r.Edits[0], want)
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
		{"body.mix", "body.mix", "a-17", "a_42", -1},
		{"body.added", "body.added", "", "new", -1},
		{"body.nul", "body.nul", "a\x00\x00b", "\x00c\x00d", -1},
		{"body.tail", "body.tail", "a-17", "a-42-", -1},
		{"body.when", "body.when", "17-a", "42-a", -1},
		{"body.lead", "body.lead", "-a17", "a42", -1},
		{"body.was", "body.was", "17-a", "42-a", -1},
		{"body.first", "body.first#0", "17", "42", 0},
	}
	if !reflect.DeepEqual(gots, want) {
		t.Errorf("Decide refused\n%+v\nwant\n%+v", gots, want)
	}
	if !strings.HasPrefix(ds[0].Line, `test-1 mock-3 body.id word 1: recorded "17", round F "42": `) {
		t.Errorf("line %q", ds[0].Line)
	}
	// Word 0 is a word: its line names it, not the field.
	if l := ds[len(ds)-1].Line; !strings.HasPrefix(l, `test-1 mock-3 body.first word 0: recorded "17", round F "42": keploy cannot check this word yet`) {
		t.Errorf("line %q", l)
	}
}

// TestTheSeamsDefaultToMainsReadings: as declared, Decide's order refuses
// every word, and its clock seams are main's readings (offsetReading,
// sinceTestBegin); Decide asks whichever order is registered.
func TestTheSeamsDefaultToMainsReadings(t *testing.T) {
	same := func(a, b interface{}) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	if !same(declaredDecideBy, refuseAll) || !same(declaredClockInWindow, offsetReading) || !same(declaredClockSinceBegin, sinceTestBegin) {
		t.Fatal("a seam does not default to main's reading")
	}
	if ln, ok := declaredClockSinceBegin(Learned{Kind: KindNoise}); ok || ln.Kind != KindNoise {
		t.Errorf("clockSinceBegin of noise = %+v, %v", ln, ok)
	}
	if _, why, ok := declaredClockInWindow("body.at", typedInstant{}, typedInstant{}, nil, nil); ok || why != "" {
		t.Errorf("clockInWindow with no window = %q, %v", why, ok)
	}
	was := decideBy
	t.Cleanup(func() { decideBy = was })
	decideBy = func(in Input) []Decision { return []Decision{{Line: in.Mock}} }
	if ds := Decide(Input{Mock: "mock-9"}); len(ds) != 1 || ds[0].Line != "mock-9" {
		t.Errorf("Decide does not ask the registered order: %+v", ds)
	}
}

// TestInputNamesEachMocksScopeAndWhereTheCallLanded: ScopeOf asks the
// adapter's lookup, an Input with no lookup knows no scope, the scopes are
// written as mappings.yaml's places are (models.Mapping.MockPlaces), and
// Landed names the window in a line.
func TestInputNamesEachMocksScopeAndWhereTheCallLanded(t *testing.T) {
	m := &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-1"}}}},
		Startup:   []models.MockEntry{{Name: "mock-3"}},
		Worker:    []models.MockEntry{{Name: "mock-4"}},
	}
	places := m.MockPlaces()
	in := Input{Scope: func(mock string) Scope {
		if p := places[mock]; len(p) == 1 {
			return Scope(p[0])
		}
		return ScopeUnknown
	}}
	for mock, want := range map[string]Scope{"mock-1": ScopeTest("test-1"), "mock-3": ScopeStartup, "mock-4": ScopeWorker, "mock-6": ScopeUnknown} {
		if got := in.ScopeOf(mock); got != want {
			t.Errorf("scope of %s = %q; want %q", mock, got, want)
		}
	}
	if got := (Input{}).ScopeOf("mock-1"); got != ScopeUnknown {
		t.Errorf("an Input with no lookup knows %s's scope: %q", "mock-1", got)
	}
	for l, want := range map[Landed]string{
		{}: "a window keploy was not told", {Where: LandedOwn}: "its own test's window",
		{Where: LandedOther, Test: "test-2"}: "test-2's window", {Where: LandedOther}: "another test's window",
		{Where: LandedBetween}: "between tests",
	} {
		if got := l.String(); got != want {
			t.Errorf("%+v names %q; want %q", l, got, want)
		}
	}
}

// TestABodylessShapeNamesItsPlacesByTheAdapter: a shape with no Body (Mongo's)
// gets its words' places from Input.PlaceOf, steps included, and its words
// are split in the value those steps open, so each decision's At reads its
// own Recorded and Full; where a step does not open a side, the field is
// refused whole, as written. With no PlaceOf set, VarPlace names a request
// leaf as JSON only where the request's body is, and an answer leaf as JSON.
func TestABodylessShapeNamesItsPlacesByTheAdapter(t *testing.T) {
	was := decideBy
	decideBy = refuseAll
	t.Cleanup(func() { decideBy = was })
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	type got struct {
		at, r, f string
		word     int
	}
	at := func(placeOf func(Leaf, bool) (string, bool), r, f string) []got {
		rec := Shape{Leaves: map[string]Leaf{"body.name": {Path: "body.name", Type: String, Value: r}}}
		live := Shape{Leaves: map[string]Leaf{"body.name": {Path: "body.name", Type: String, Value: f}}}
		var out []got
		for _, d := range Decide(Input{Rec: rec, Live: live, Drift: []string{"body.name"}, PlaceOf: placeOf}) {
			if w, ok := d.At.WordOf(r); !ok || w != d.Recorded {
				t.Errorf("At %q reads %q, %v in the recording; its Recorded is %q", d.At, w, ok, d.Recorded)
			}
			if w, ok := d.At.WordOf(f); !ok || w != d.Full {
				t.Errorf("At %q reads %q, %v in the full round; its Full is %q", d.At, w, ok, d.Full)
			}
			out = append(out, got{d.At.String(), d.Recorded, d.Full, d.Refusal.Word})
		}
		return out
	}
	withB64 := func(l Leaf, a bool) (string, bool) { s, ok := VarPlace(l, true); return s + "|b64", ok }
	withJSON := func(l Leaf, a bool) (string, bool) { s, ok := VarPlace(l, true); return s + "|json", ok }
	for _, c := range []struct {
		name    string
		placeOf func(Leaf, bool) (string, bool)
		r, f    string
		want    []got
	}{
		{"no PlaceOf: a body with no JSON names no place", nil, "order-17", "order-42", []got{{"", "17", "42", 1}}},
		{"the adapter's places", func(l Leaf, a bool) (string, bool) { return VarPlace(l, true) }, "order-17", "order-42", []got{{"body.name#1", "17", "42", 1}}},
		{"the adapter's steps open both sides", withB64, b64("order-17"), b64("order-42"), []got{{"body.name|b64#1", "17", "42", 1}}},
		{"a step opens neither side", withB64, "order-17", "order-42", []got{{"body.name", "order-17", "order-42", -1}}},
		{"a step opens one side", withB64, b64("order-17"), "order-42", []got{{"body.name", b64("order-17"), "order-42", -1}}},
		{"a step opens the full round only", withB64, "order-17", b64("order-42"), []got{{"body.name", "order-17", b64("order-42"), -1}}},
		{"opened, the words do not pair", withB64, b64("order-17"), b64("order-17-x"), []got{{"body.name|b64", "order-17", "order-17-x", -1}}},
		{"a word the adapter names is the leaf whole", func(l Leaf, a bool) (string, bool) { s, ok := withB64(l, a); return s + "#3", ok }, b64("order-17"), b64("order-42"), []got{{"body.name|b64#1", "17", "42", 1}}},
		{"a place that does not parse", func(Leaf, bool) (string, bool) { return "body", true }, "order-17", "order-42", []got{{"", "17", "42", 1}}},
		{"a place in another dialect", func(Leaf, bool) (string, bool) { return "filter.name", true }, "order-17", "order-42", []got{{"", "17", "42", 1}}},
		// Only the encoding changed: the field as written, so the line shows it.
		{"padded against raw base64 of one text", withB64, b64("order-17"), strings.TrimRight(b64("order-17"), "="), []got{{"body.name", "b3JkZXItMTc=", "b3JkZXItMTc", -1}}},
		{"a JSON escape against the plain byte", withJSON, `"order\u002d17"`, `"order-17"`, []got{{"body.name", `"order\u002d17"`, `"order-17"`, -1}}},
	} {
		if g := at(c.placeOf, c.r, c.f); !reflect.DeepEqual(g, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, g, c.want)
		}
	}
	rec := Shape{Leaves: map[string]Leaf{"body.name": {Path: "body.name", Type: String, Value: "order-17"}}}
	live := Shape{Leaves: map[string]Leaf{"body.name": {Path: "body.name", Type: String, Value: "order-42"}}}
	var asked []bool
	mongo := func(l Leaf, answer bool) (string, bool) { asked = append(asked, answer); return VarPlace(l, true) }
	Decide(Input{Rec: rec, Live: live, Drift: []string{"body.name"}, PlaceOf: mongo})
	if !reflect.DeepEqual(asked, []bool{false}) {
		t.Errorf("PlaceOf asked answer=%v for the request's leaf, want [false]", asked)
	}
	in := Input{Rec: rec}
	ans := Leaf{Path: "body.id", Type: String, Value: "17"}
	if got := in.AddressOf(ans, true); got.String() != "body.id" || got.Word != -1 {
		t.Errorf("answer leaf: %+v, want body.id word -1", got)
	}
	if got := in.AddressOf(ans, false); got.String() != "" || got.Word != -1 {
		t.Errorf("request leaf of a body with no JSON: %+v, want none", got)
	}
	in.PlaceOf = mongo
	if got := in.AddressOf(ans, true); got.String() != "body.id" || asked[len(asked)-1] != true {
		t.Errorf("PlaceOf on an answer leaf: %+v, asked %v", got, asked)
	}
	in.PlaceOf = func(Leaf, bool) (string, bool) { return "body.id|b64#3", true }
	if got := in.AddressOf(ans, true); got.String() != "body.id|b64" || got.Word != -1 {
		t.Errorf("a word PlaceOf names: %+v, want the leaf whole (word -1)", got)
	}
}
