package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Rows for the one value a word made anew is a word of (Decision.Of, the
// value form): a whole id token whose every word changed together, each
// made anew in this call, and none sent apart from it, names the token as
// recorded on each of its words, and each word's line says so. Every other
// shape names none, so each word stays its own value at replay, as main
// reads it. Each row that names none fails alone on a rule it pins.

const (
	vfRec  = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	vfLive = "1c2d3e4f-5a6b-47c8-99d0-e1f2a3b4c5d6"
)

// vfInput is a call whose request leaves are leaves (path: recorded, round
// F), each drifted where its runs differ, named at body.<path>.
func vfInput(leaves ...[3]string) Input {
	in := oInput(leaves[0][0], leaves[0][1], leaves[0][2], nil, nil)
	in.Drift = nil
	in.Rec, in.Live = Shape{Leaves: map[string]Leaf{}}, Shape{Leaves: map[string]Leaf{}}
	for _, l := range leaves {
		in.Rec.Leaves[l[0]] = oText(l[0], l[1])
		in.Live.Leaves[l[0]] = oText(l[0], l[2])
		if l[1] != l[2] {
			in.Drift = append(in.Drift, l[0])
		}
	}
	in.PlaceOf = func(l Leaf, _ bool) (string, bool) { return "body." + l.Path, true }
	return in
}

// vfAt are the decisions of ds at field path.
func vfAt(ds []Decision, path string) []Decision {
	var out []Decision
	for _, d := range ds {
		if d.At.Place.String() == "body."+path {
			out = append(out, d)
		}
	}
	return out
}

// vfNamed checks the words of ds at path are n words made anew, each naming
// of (Decision.Of) and its line saying so.
func vfNamed(t *testing.T, what string, ds []Decision, path, of string, n int) {
	t.Helper()
	at := vfAt(ds, path)
	if len(at) != n {
		t.Fatalf("%s: %d words at %s, want %d: %s", what, len(at), path, n, oDescribe(ds))
	}
	for _, d := range at {
		if d.Outcome != OutcomeFresh || d.Of != of || !strings.Contains(d.Line, "a word of one value, "+strconv.Quote(of)+", the whole token") ||
			strings.Index(d.Line, "a word of one value") > strings.LastIndex(d.Line, undoMark) {
			t.Errorf("%s: %s of %q, line %q; want made anew, a word of %q, its line saying so before its undo", what, oDescribe([]Decision{d}), d.Of, d.Line, of)
		}
	}
}

// vfNone checks no word of ds names a value, and that the words at path
// are still each made anew where fresh is set (so the row pins the value
// form, not a refusal).
func vfNone(t *testing.T, what string, ds []Decision, path string, fresh bool) {
	t.Helper()
	at := vfAt(ds, path)
	if len(at) == 0 {
		t.Fatalf("%s: no word at %s: %s", what, path, oDescribe(ds))
	}
	for _, d := range ds {
		if d.Of != "" || strings.Contains(d.Line, "a word of one value") {
			t.Errorf("%s: %s names %q (line %q); want no value named", what, oDescribe([]Decision{d}), d.Of, d.Line)
		}
	}
	for _, d := range at {
		if fresh && d.Outcome != OutcomeFresh {
			t.Errorf("%s: %s; want each word still made anew", what, oDescribe([]Decision{d}))
		}
	}
}

func TestOutcomeAFreshUUIDsWordsAreWordsOfOneValue(t *testing.T) {
	vfNamed(t, "a UUID sent whole", Decide(vfInput([3]string{"q.id", vfRec, vfLive})), "q.id", vfRec, 5)
	vfNamed(t, "a UUID after a kept prefix and ':'", Decide(vfInput([3]string{"q.id", "APP:" + vfRec, "APP:" + vfLive})), "q.id", vfRec, 5)
	// Sent whole at another field too: the first place names it, the
	// other is a use of it, which names none.
	ds := Decide(vfInput([3]string{"q.id", vfRec, vfLive}, [3]string{"u.ref", "APP:" + vfRec, "APP:" + vfLive}))
	vfNamed(t, "a UUID also sent whole elsewhere", ds, "q.id", vfRec, 5)
	for _, d := range vfAt(ds, "u.ref") {
		if d.Outcome != OutcomeValue || d.Of != "" {
			t.Errorf("a later place of the UUID: %s of %q; want a use of the first, naming no value", oDescribe([]Decision{d}), d.Of)
		}
	}
	// The answer reads the UUID back whole: a read-back, not apart.
	in := vfInput([3]string{"q.id", vfRec, vfLive})
	in.Resp = []Leaf{oText("r.id", vfRec)}
	vfNamed(t, "a UUID its answer reads back whole", Decide(in), "q.id", vfRec, 5)
	// A typed instant of the answer is no text.
	in.Resp = append(in.Resp, Leaf{Path: "r.at", Type: Instant, Value: "4011-01-01T00:00:00Z"})
	vfNamed(t, "a UUID beside a typed instant that holds a word of it", Decide(in), "q.id", vfRec, 5)
}

func TestOutcomeAWordOfATokenLearningCannotReadWholeNamesNoValue(t *testing.T) {
	for _, tc := range []struct {
		what, rec, live string
		fresh           bool
	}{
		{"a word kept inside the token", "ORD-" + vfRec, "ORD-" + vfLive, true},
		{"one group kept by chance", vfRec, "1c2d3e4f-5a6b-4011-99d0-e1f2a3b4c5d6", true},
		{"its separators changed", "6f9619ff-8b86_4011", "1c2d3e4f_5a6b-47c8", false},
		{"one word in a longer token", "_6f9619ff00c04fc9", "_a1b2c3d4e5f60718", true},
		{"under 32 bits as recorded", "a1-b2", "6f9619ff-8b86", true},
		{"under 32 bits in round F", "6f9619ff-8b86", "a1-b2", true},
		{"one word twice: the second is a use of the first", "ab12cd34ef-ab12cd34ef", "9e8d7c6b5a-9e8d7c6b5a", false},
	} {
		vfNone(t, tc.what, Decide(vfInput([3]string{"q.id", tc.rec, tc.live})), "q.id", tc.fresh)
	}
	ds := Decide(vfInput([3]string{"q.id", "ab12cd34ef-ab12cd34ef", "9e8d7c6b5a-9e8d7c6b5a"}))
	if at := vfAt(ds, "q.id"); len(at) != 2 || at[0].Outcome != OutcomeFresh || at[1].Outcome != OutcomeValue {
		t.Errorf("one word twice: %s; want the first made anew and the second its use", oDescribe(ds))
	}
	// The separators row is decided word by word: each word made anew.
	ds = Decide(vfInput([3]string{"q.id", "6f9619ff-8b86_4011", "1c2d3e4f_5a6b-47c8"}))
	if at := vfAt(ds, "q.id"); len(at) != 3 || at[0].Outcome != OutcomeFresh || at[1].Outcome != OutcomeFresh || at[2].Outcome != OutcomeFresh {
		t.Errorf("its separators changed: %s; want three words made anew", oDescribe(ds))
	}
}

func TestOutcomeAValueWhoseWordTheCallSendsApartNamesNone(t *testing.T) {
	const upper = "6F9619FF-8B86-4011-B42D-00C04FC964FF"
	for _, tc := range []struct {
		what   string
		leaves [][3]string
		resp   []Leaf
	}{
		{"another field holds a recorded word alone", [][3]string{{"q.id", vfRec, vfLive}, {"q.g", "4011", "4011"}}, nil},
		{"another field holds a round F word alone", [][3]string{{"q.id", vfRec, vfLive}, {"q.g", "47c8", "47c8"}}, nil},
		{"its own field holds a word of it again", [][3]string{{"q.id", vfRec + "/4011", vfLive + "/47c8"}}, nil},
		{"another field holds it inside a longer token", [][3]string{{"q.id", vfRec, vfLive}, {"q.ref", "ORD-" + vfRec, "ORD-" + vfLive}}, nil},
		{"another field holds it before more id bytes", [][3]string{{"q.id", vfRec, vfLive}, {"q.ref", vfRec + "_old", vfLive + "_old"}}, nil},
		{"another field holds a word of it case-folded", [][3]string{{"q.id", upper, strings.ToUpper(vfLive)}, {"q.g", "6f9619ff", "6f9619ff"}}, nil},
		{"its answer reads it back and holds a word apart", [][3]string{{"q.id", vfRec, vfLive}}, []Leaf{oText("r.id", vfRec), oText("r.g", "4011")}},
	} {
		in := vfInput(tc.leaves...)
		in.Resp = tc.resp
		vfNone(t, tc.what, Decide(in), "q.id", false)
	}
}

// One word of the value bound to a place before the call: the other words
// are made anew, but the value is not one made anew whole.
func TestOutcomeAValueWithAWordBoundEarlierNamesNone(t *testing.T) {
	src := oServed("mock-23", false, false, Request, oText("body.owner", "6f9619ff"), "1c2d3e4f", 0)
	in := vfInput([3]string{"q.id", vfRec, vfLive})
	in.Earlier = src
	ds := Decide(in)
	vfNone(t, "a word bound before the call", ds, "q.id", false)
	if d := oWord(t, ds, "6f9619ff", "1c2d3e4f"); d.Outcome != OutcomeValue {
		t.Errorf("the bound word: %s; want a Value from mock-23", oDescribe([]Decision{d}))
	}
}

// A UUID whose whole token an earlier place of the call's test gave in both
// runs (the test's find gave the recorded UUID in both runs, and the request
// moved off it): each word is refused, naming the place, as main refuses
// the field. None is made anew, so none names the value: refused words are
// never bound as one.
func TestOutcomeAUUIDAPlaceOfTheTestGaveWholeInBothRunsNamesNone(t *testing.T) {
	in := vfInput([3]string{"q.id", vfRec, vfLive})
	in.Earlier = oServed("mock-0", false, false, Response, oText("body.ref", vfRec), vfRec, time.Millisecond)
	ds := Decide(in)
	vfNone(t, "a UUID a find gave whole in both runs", ds, "q.id", false)
	at := vfAt(ds, "q.id")
	if len(at) != 5 {
		t.Fatalf("a UUID a find gave whole in both runs: %d words, want 5: %s", len(at), oDescribe(ds))
	}
	for _, d := range at {
		oRefused(t, "a UUID a find gave whole in both runs", d, RefusalConflict, "stood whole at mock-0 body.ref")
	}
}

// A secret's field prints no value in the clause either.
func TestOutcomeAValueFormInASecretFieldPrintsMasked(t *testing.T) {
	ds := Decide(vfInput([3]string{"q.password", vfRec, vfLive}))
	at := vfAt(ds, "q.password")
	if len(at) != 5 {
		t.Fatalf("want five words: %s", oDescribe(ds))
	}
	for _, d := range at {
		if d.Of != vfRec || !strings.Contains(d.Line, "a word of one value, "+masked+",") || strings.Contains(d.Line, vfRec) {
			t.Errorf("a secret's value form: of %q, line %q; want it named, the line masked", d.Of, d.Line)
		}
	}
}

// The token's size bounds: a two-word id token names its value, and so does
// one of exactly chanceBits in each run (8 hex digits, in three words).
func TestOutcomeAValueFormAtItsBounds(t *testing.T) {
	vfNamed(t, "a two-word id token", Decide(vfInput([3]string{"q.id", "6f9619ff-8b86", "1c2d3e4f-5a6b"})), "q.id", "6f9619ff-8b86", 2)
	if b := wordBits("ab12-cd-ef"); b != chanceBits || wordBits("9e8d-7c-6b") != chanceBits {
		t.Fatalf("the row's tokens carry %v and %v bits; want exactly %d", b, wordBits("9e8d-7c-6b"), chanceBits)
	}
	vfNamed(t, "a token of exactly chanceBits", Decide(vfInput([3]string{"q.id", "ab12-cd-ef", "9e8d-7c-6b"})), "q.id", "ab12-cd-ef", 3)
}

// Only a whole id token (word bytes, '-' and '_') names a value: the words
// of a standard base64 run, whose '+' and '/' no id carries, are each their
// own value, though they changed together around the same separators in
// both runs, each made anew, and carry far more than chanceBits.
func TestOutcomeAWordsOfABase64RunNameNoValue(t *testing.T) {
	for _, tc := range [][2]string{
		{"Ox1Cm+lFL2O/gSsz", "qIX9IT+NEkr/J8Yc"}, // 12 bytes, decoded
		{"QmFz/ZTY0/dGV4", "Zm9v/YmFy/YmF6"},
	} {
		if wordBits(tc[0]) < chanceBits || wordBits(tc[1]) < chanceBits {
			t.Fatalf("the row's runs carry %v and %v bits; want %d or more", wordBits(tc[0]), wordBits(tc[1]), chanceBits)
		}
		ds := Decide(vfInput([3]string{"q.tok", tc[0], tc[1]}))
		vfNone(t, "the words of the base64 run "+tc[0], ds, "q.tok", true)
		if n := len(vfAt(ds, "q.tok")); n != 3 {
			t.Errorf("the base64 run %s: %d words, want 3: %s", tc[0], n, oDescribe(ds))
		}
	}
}

// A long leaf of a call in a test set (filterTextMin bytes or more) is asked
// through the set's filter of its words: one that holds a word of the value
// apart, as written or in another case, still names none, and one that holds
// none of them leaves the value named.
func TestOutcomeAValueSentApartInALongLeafOfASetNamesNone(t *testing.T) {
	const upper = "6F9619FF-8B86-4011-B42D-00C04FC964FF"
	pad := strings.Repeat("as served ", filterTextMin/10)
	for _, tc := range []struct {
		what, rec, live, note string
		named                 bool
	}{
		{"a word held apart as written", vfRec, vfLive, pad + " 4011 " + pad, false},
		{"a word held apart in lower case", upper, strings.ToUpper(vfLive), pad + " 6f9619ff " + pad, false},
		{"a word held apart in upper case", vfRec, vfLive, pad + " 6F9619FF " + pad, false},
		{"no word of it held", vfRec, vfLive, pad + " 5150 " + pad, true},
	} {
		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		in := vfInput([3]string{"q.id", tc.rec, tc.live}, [3]string{"q.note", tc.note, tc.note})
		in.Drifts = tr.Drifts()
		built := textsBuilt(in.Drifts)
		ds := Decide(in)
		if n := textsBuilt(in.Drifts) - built; n == 0 {
			t.Errorf("%s: the %d-byte leaf built no filter of its words; want it asked through one", tc.what, len(tc.note))
		}
		if tc.named {
			vfNamed(t, tc.what, ds, "q.id", tc.rec, 5)
		} else {
			vfNone(t, tc.what, ds, "q.id", true)
		}
		tr.End()
	}
}

// The call's texts are read for a word of a value sent apart only once
// every word of the value is made anew: a token whose words are uses of an
// earlier place names none whatever the call sends, so it costs no scan.
func TestOutcomeAValueFormScansOnlyAValueMadeAnew(t *testing.T) {
	scans := func(in Input) ([]Decision, int) {
		x := newReaderIndex(in.Drifts)
		defer x.release()
		dc := newDecider(in, false, x)
		ds, _ := dc.decide()
		return ds, dc.apartScans
	}
	ds, n := scans(vfInput([3]string{"q.id", vfRec, vfLive}))
	if at := vfAt(ds, "q.id"); n != 1 || len(at) != 5 || at[0].Of != vfRec {
		t.Errorf("a UUID made anew: %d scans, %s; want one scan and its words naming it", n, oDescribe(ds))
	}
	in := vfInput([3]string{"q.id", vfRec, vfLive})
	in.Earlier = oServed("mock-0", false, false, Response, oText("body.ref", vfRec), vfLive, time.Millisecond)
	ds, n = scans(in)
	at := vfAt(ds, "q.id")
	if n != 0 || len(at) != 5 {
		t.Errorf("a UUID an earlier place gave: %d scans, %d words; want none and five", n, len(at))
	}
	for _, d := range at {
		if d.Outcome != OutcomeValue {
			t.Errorf("a UUID an earlier place gave: %s; want a use of mock-0", oDescribe([]Decision{d}))
		}
	}
	vfNone(t, "a UUID an earlier place gave", Decide(in), "q.id", false)
}
