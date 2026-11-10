package variables

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The api-server's create flow under keploy record's carry: each test's
// minted app id is bound word by word at a field of its own (no other place
// of the mock holds it whole), each word named that id (WordBind.Of), and,
// where the carry runs, twice at each word: the test's own variable and its
// run twin (run-appIdN-carried-K), one live and one recorded word for both.
const (
	formSRec  = "8a1b2c3d-1e2f-4f2c-9aaa-0a0b0c0d0e0f"
	formTRec  = "5c6d7e8f-2a3b-43a1-8bbb-1a1b1c1d1e1f"
	formSLive = "11111111-2222-4abc-8333-444444444444"
	formTLive = "99999999-aaaa-4abc-bbbb-cccccccccccc" // group #2 equal to S's live by chance
	formAud   = "2de26cf5-e66e-4b5f-a866-f4c9723b5f77"
	formAudL  = "1a87a19d-57d4-4abc-93fc-1f81ee22ab38" // group #2 equal to T's own live app id
)

// idBind is one call's binds at the words of a text: at place body.<field>,
// live and recorded, its word w (from first) bound as each of names with
// the word's index appended (a name and its run twin: two binds at one
// word), each named of (WordBind.Of), elsewhere its mock's evidence (nil:
// none). skip leaves word indexes unbound.
type idBind struct {
	field, live, recorded, of string
	names                     []string
	elsewhere                 func(string) bool
	skip                      map[int]bool
	clock                     []models.ClockRead
}

func (x idBind) binds(t *testing.T) []Binding {
	t.Helper()
	return callBinds(t, x)
}

// callBinds is the binds one call makes at each of xs, their runs read
// together (WordRuns), as an adapter reads one call.
func callBinds(t *testing.T, xs ...idBind) []Binding {
	t.Helper()
	var ws []WordBind
	var out []Binding
	for _, x := range xs {
		lw, rw := wordsOf(x.live), wordsOf(x.recorded)
		if len(lw) != len(rw) {
			t.Fatalf("%s and %s hold %d and %d words", x.live, x.recorded, len(lw), len(rw))
		}
		for i := range lw {
			if x.skip[i] {
				continue
			}
			for _, name := range x.names {
				ws = append(ws, WordBind{Text: "body." + x.field, Word: i, Live: lw[i], Recorded: rw[i], RecordedText: x.recorded, Elsewhere: x.elsewhere, Of: x.of})
				out = append(out, Binding{Name: name + strconv.Itoa(i), Value: lw[i], Recorded: rw[i], Place: "body." + x.field + "#" + strconv.Itoa(i), Clock: x.clock})
			}
		}
	}
	for i, r := range WordRuns(ws) {
		out[i] = r.On(out[i])
	}
	return out
}

func runTwin(n int) string { return fmt.Sprintf("%sappId-carried-%d-", models.RunVariablePrefix, n) }

// carriedTable is a table in test T's scope after test S bound its app id
// (with its run twin when carried), so S's run twin is in force in T's.
func carriedTable(t *testing.T, carried bool) *Table {
	t.Helper()
	tb := &Table{}
	tb.KeepRun()
	tb.KeepEnded()
	tb.StartOpeningTest("TestS", func() {}, nil, nil, nil)
	names := []string{"appId"}
	if carried {
		names = append(names, runTwin(9))
	}
	s := idBind{field: "documents.0.appId", live: formSLive, recorded: formSRec, of: formSRec, names: names}
	if bad, with, _, ok := tb.BindAll(tb.Gen(), s.binds(t)); !ok {
		t.Fatalf("S's create did not bind: %+v beside %+v", bad, with)
	}
	tb.StartOpeningTest("TestT", func() {}, nil, nil, nil)
	if carried {
		if _, ok := tb.Lookup(runTwin(9) + "2"); !ok {
			t.Fatal("S's run twin is not in force in T's scope")
		}
	}
	return tb
}

// A minted UUID bound at every word, each word named it, is one value by its
// form: its span is the five words, held elsewhere or not, and a variable
// and its run twin at one word share one position. Short of that each word
// is its own span: a word left unbound, a learned value other than the UUID,
// a text or live text that is not one canonical UUID, twins that differ.
func TestWordRuns_OneUUIDByItsForm(t *testing.T) {
	whole := Run{Live: strings.ReplaceAll(formTLive, "-", wordRunSep), Recorded: strings.ReplaceAll(formTRec, "-", wordRunSep), At: 2}
	alone := Run{Live: "4abc", Recorded: "43a1", At: 0}
	spanOf := func(t *testing.T, x idBind, name string) Run {
		t.Helper()
		for _, b := range callBinds(t, x) {
			if b.Name == name {
				return Run{Live: b.WordRun, Recorded: b.WordRunRecorded, At: b.WordRunAt}
			}
		}
		t.Fatalf("the call makes no bind %s", name)
		return Run{}
	}
	base := idBind{field: "documents.0.appId", live: formTLive, recorded: formTRec, of: formTRec, names: []string{"appId"}}
	with := func(f func(*idBind)) idBind { x := base; f(&x); return x }
	for _, c := range []struct {
		name string
		x    idBind
		bind string
		want Run
	}{
		{"held nowhere, named the UUID", base, "appId2", whole},
		{"held elsewhere too", with(func(x *idBind) { x.elsewhere = heldAt(formTRec) }), "appId2", whole},
		{"with its run twin: the name", with(func(x *idBind) { x.names = []string{"appId", runTwin(21)} }), "appId2", whole},
		{"with its run twin: the twin", with(func(x *idBind) { x.names = []string{"appId", runTwin(21)} }), runTwin(21) + "2", whole},
		{"upper-case hex", with(func(x *idBind) {
			x.live, x.recorded, x.of = strings.ToUpper(formTLive), strings.ToUpper(formTRec), strings.ToUpper(formTRec)
		}),
			"appId2", Run{Live: strings.ReplaceAll(strings.ToUpper(formTLive), "-", wordRunSep), Recorded: strings.ReplaceAll(strings.ToUpper(formTRec), "-", wordRunSep), At: 2}},
		{"named no learned value", with(func(x *idBind) { x.of = "" }), "appId2", alone},
		{"named a value other than the UUID", with(func(x *idBind) { x.of = formSRec }), "appId2", alone},
		{"a word left unbound", with(func(x *idBind) { x.skip = map[int]bool{4: true} }), "appId2", alone},
		{"a word left unbound inside", with(func(x *idBind) {
			x.live = formTLive[:14] + formTRec[14:18] + formTLive[18:]
			x.skip = map[int]bool{2: true}
		}), "appId1", Run{Live: "aaaa", Recorded: "2a3b", At: 0}},
		{"the live text no canonical UUID (a short group)", with(func(x *idBind) { x.live = "9999999-aaaa-4abc-bbbb-cccccccccccc" }), "appId2", Run{Live: "4abc", Recorded: "43a1"}},
		{"the live text no canonical UUID (no hex)", with(func(x *idBind) { x.live = "9999999z-aaaa-4abc-bbbb-cccccccccccc" }), "appId2", Run{Live: "4abc", Recorded: "43a1"}},
		{"the recorded text no canonical UUID (another joint)", with(func(x *idBind) {
			x.recorded = strings.ReplaceAll(formTRec, "-", "_")
			x.of = x.recorded
		}), "appId2", Run{Live: "4abc", Recorded: "43a1"}},
		{"two equal groups side by side", with(func(x *idBind) {
			x.live, x.recorded, x.of = "99999999-aaaa-aaaa-bbbb-cccccccccccc", "5c6d7e8f-2a3b-2a3b-8bbb-1a1b1c1d1e1f", "5c6d7e8f-2a3b-2a3b-8bbb-1a1b1c1d1e1f"
		}), "appId2", Run{Live: "99999999-aaaa-aaaa-bbbb-cccccccccccc", Recorded: "5c6d7e8f-2a3b-2a3b-8bbb-1a1b1c1d1e1f", At: 2}},
		{"the UUID after a word, named itself", with(func(x *idBind) { x.live, x.recorded = "APP:"+formTLive, "APP:"+formTRec }), "appId3", whole},
		{"the recorded text no canonical UUID (no hex)", with(func(x *idBind) { x.recorded = "zzzzzzzz" + formTRec[8:]; x.of = x.recorded }), "appId2", alone},
		{"a last group of 13 digits", with(func(x *idBind) { x.live, x.recorded, x.of = formTLive+"c", formTRec+"f", formTRec+"f" }), "appId2", alone},
		{"the UUID inside a longer learned value", with(func(x *idBind) {
			x.live, x.recorded, x.of = "APP:"+formTLive, "APP:"+formTRec, "APP:"+formTRec
		}), "appId3", Run{Live: "4abc", Recorded: "43a1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := spanOf(t, c.x, c.bind); got != c.want {
				t.Fatalf("span of %s: %+v, want %+v", c.bind, got, c.want)
			}
		})
	}
	// Twins at one word that differ there (another live word) cut the run:
	// each word is its own.
	ws := []WordBind{}
	for i, w := range wordsOf(formTLive) {
		r := wordsOf(formTRec)[i]
		ws = append(ws, WordBind{Text: "body.id", Word: i, Live: w, Recorded: r, RecordedText: formTRec, Of: formTRec})
		other := w
		if i == 1 {
			other = "bbbb"
		}
		ws = append(ws, WordBind{Text: "body.id", Word: i, Live: other, Recorded: r, RecordedText: formTRec, Of: formTRec})
	}
	for i, r := range WordRuns(ws) {
		if r.At != 0 || strings.Contains(r.Live, wordRunSep) {
			t.Fatalf("bind %d of twins that differ at word 1 has span %+v, want its own word", i, r)
		}
	}
	// A twin named no value, or read against another recorded text, leaves
	// its word's position no one value, held elsewhere or not.
	for _, c := range []struct {
		name      string
		elsewhere func(string) bool
		of, text  string
	}{
		{"named no value", nil, "", formTRec},
		{"named no value, held elsewhere", heldAt(formTRec), "", formTRec},
		{"another recorded text", nil, formTRec, formSRec[:24] + formTRec[24:]},
	} {
		ws = ws[:0]
		for i, w := range wordsOf(formTLive) {
			r := wordsOf(formTRec)[i]
			of, text := formTRec, formTRec
			if i == 3 {
				of, text = c.of, c.text
			}
			ws = append(ws, WordBind{Text: "body.id", Word: i, Live: w, Recorded: r, RecordedText: formTRec, Elsewhere: c.elsewhere, Of: formTRec},
				WordBind{Text: "body.id", Word: i, Live: w, Recorded: r, RecordedText: text, Elsewhere: c.elsewhere, Of: of})
		}
		for i, r := range WordRuns(ws) {
			if strings.Contains(r.Live, wordRunSep) {
				t.Fatalf("%s: bind %d has span %+v, want its own word", c.name, i, r)
			}
		}
	}
	// A twin whose recorded word differs cuts the run at its word: the
	// words before keep the span their mock holds whole elsewhere.
	ws = []WordBind{
		{Text: "body.k", Word: 0, Live: "a", Recorded: "A", RecordedText: "A B C", Elsewhere: heldAt("A B"), Of: "A B"},
		{Text: "body.k", Word: 1, Live: "b", Recorded: "B", RecordedText: "A B C", Elsewhere: heldAt("A B"), Of: "A B"},
		{Text: "body.k", Word: 2, Live: "c", Recorded: "C", RecordedText: "A B C", Elsewhere: heldAt("A B")},
		{Text: "body.k", Word: 2, Live: "c", Recorded: "X", RecordedText: "A B C", Elsewhere: heldAt("A B")},
	}
	if got, want := fmt.Sprint(WordRuns(ws)), fmt.Sprint([]Run{{"a-b", "A-B", 0}, {"a-b", "A-B", 1}, {"c", "C", 0}, {"c", "X", 0}}); got != want {
		t.Fatalf("runs of a twin recorded with another word: %s, want %s", got, want)
	}
}

// Under the proven switch a new app id whose group #2 equals, live, a group
// of another id recorded with another value (an earlier test's run twin, or
// the same test's other id) binds, by chance, when each id is one UUID by
// its form; one sent as another's copy, whole or with a neighbour group, is
// refused, as is every pair with the switch off (main's word by word check).
func TestTable_AGroupOfTwoMintedUUIDsEqualByChanceBinds(t *testing.T) {
	tOwn := func(names ...string) idBind {
		return idBind{field: "documents.0.appId", live: formTLive, recorded: formTRec, of: formTRec, names: names}
	}
	type row struct {
		name    string
		carried bool
		call    []Binding
		on, off bool // binds with the switch on, off
	}
	rows := func(t *testing.T) []row {
		t.Helper()
		id := func(live string, skip map[int]bool, of string) []Binding {
			return idBind{field: "documents.0.appId", live: live, recorded: formTRec, of: of, names: []string{"appId"}, skip: skip}.binds(t)
		}
		return []row{
			{"S carried, T with its run twin", true, tOwn("appId", runTwin(21)).binds(t), true, false},
			{"S carried, T without a twin", true, tOwn("appId").binds(t), true, false},
			{"no carry, S's scope ended", false, tOwn("appId").binds(t), true, true},
			{"whole-id copy: T sends S's live id", true, id(formSLive, nil, formTRec), false, false},
			{"a neighbour group equal too: groups #2 and #3", true, id("99999999-aaaa-4abc-8333-cccccccccccc", nil, formTRec), false, false},
			{"T's id bound at four of its words", true, id(formTLive, map[int]bool{0: true}, formTRec), false, false},
			{"T's id named no learned value", true, id(formTLive, nil, ""), false, false},
		}
	}
	for _, sw := range []string{"1", ""} {
		t.Run("KEPLOY_PROVEN="+sw, func(t *testing.T) {
			t.Setenv(provenswitch.Env, sw)
			for _, r := range rows(t) {
				t.Run(r.name, func(t *testing.T) {
					tb := carriedTable(t, r.carried)
					bad, with, _, ok := tb.BindAll(tb.Gen(), r.call)
					if want := r.on && sw == "1" || r.off && sw == ""; ok != want {
						t.Fatalf("bound %v, want %v (bad %+v beside %+v)", ok, want, bad, with)
					}
				})
			}
			// One test, its audit id's group #2 equal to its own app id's.
			t.Run("the same test's two minted ids", func(t *testing.T) {
				var tb Table
				tb.StartOpeningTest("TestT", func() {}, nil, nil, nil)
				if bad, with, _, ok := tb.BindAll(tb.Gen(), tOwn("appId").binds(t)); !ok {
					t.Fatalf("T's app id did not bind: %+v beside %+v", bad, with)
				}
				aud := idBind{field: "documents.0._id", live: formAudL, recorded: formAud, of: formAud, names: []string{"_id"}}
				_, with, _, ok := tb.BindAll(tb.Gen(), aud.binds(t))
				if ok != (sw == "1") {
					t.Fatalf("the audit id bound %v beside %+v, want %v", ok, with, sw == "1")
				}
				// In one call, too.
				var one Table
				_, with, _, ok = one.BindAll(one.Gen(), callBinds(t, tOwn("appId"), aud))
				if ok != (sw == "1") {
					t.Fatalf("in one call, the two ids bound %v beside %+v, want %v", ok, with, sw == "1")
				}
			})
		})
	}
}

// #5153's shape stays word by word with no evidence held elsewhere, named
// or not: "<P>/7" and "<Q>/7" where "<B>/3" and "<D>/5" were recorded, each
// UUID one value by its form, leave "7" its own value, refused.
func TestTable_AWordBesideAUUIDByItsFormIsItsOwnValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const (
		recB  = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
		recD  = "aabbccdd-eeff-4a11-9b22-334455667788"
		liveP = "11112222-3333-4444-8555-666677778888"
		liveQ = "9999aaaa-bbbb-4ccc-9ddd-eeeeffff0000"
	)
	for _, c := range []struct {
		name string
		of   func(string, int) string
	}{
		{"each UUID named itself", learnedAs(recB, recD)},
		{"each text named one value", learnedAs(recB+"/3", recD+"/5")},
		{"named nothing", nil},
	} {
		k := textBindsNamed(t, heldNowhere, c.of, [4]string{"body.k1", "k", liveP + "/7", recB + "/3"})
		cc := textBindsNamed(t, heldNowhere, c.of, [4]string{"body.k2", "c", liveQ + "/7", recD + "/5"})
		var tb Table
		tb.BindAll(tb.Gen(), k)
		if _, _, _, ok := tb.BindAll(tb.Gen(), cc); ok {
			t.Errorf("%s: \"<P>/7\" and \"<Q>/7\" bound where \"<B>/3\" and \"<D>/5\" were recorded", c.name)
		}
	}
}

// Clock reads bound at words of a time stamp, as today: a shared word of two
// reads made an hour apart is refused, of two made in one second it binds.
func TestTable_ClockWordsBesideTheUUIDForm(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	at := time.Date(2026, 11, 11, 1, 2, 3, 0, time.UTC)
	read := func(d time.Duration) []models.ClockRead {
		return []models.ClockRead{{At: at.Add(d), Res: time.Second}}
	}
	for _, c := range []struct {
		name  string
		later time.Duration
		binds bool
	}{{"an hour apart", time.Hour, false}, {"in one second", 0, true}} {
		a := idBind{field: "a", live: "2026-11-11T09:00:00Z", recorded: "2026-11-11T01:02:03Z", of: "2026-11-11T01:02:03Z", names: []string{"ca"}, clock: read(0)}
		b := idBind{field: "b", live: "2026-11-11T09:00:00Z", recorded: "2026-11-11T01:02:04Z", of: "2026-11-11T01:02:04Z", names: []string{"cb"}, clock: read(c.later)}
		var tb Table
		if _, _, _, ok := tb.BindAll(tb.Gen(), a.binds(t)); !ok {
			t.Fatalf("%s: the first read did not bind", c.name)
		}
		if _, with, _, ok := tb.BindAll(tb.Gen(), b.binds(t)); ok != c.binds {
			t.Errorf("%s: the second read bound %v beside %+v, want %v", c.name, ok, with, c.binds)
		}
	}
}
