package learn

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// amongBindsCase is the prompt of a run's ids and its time (aPrompt), read
// by Decide with the adapter's caps and the time default as given, and the
// decision on its time word. The time is a read of the clock its call's
// window holds, and every id group around it is learned fresh or bound.
func amongBindsCase(t *testing.T, caps Caps, timeForm bool, edit func(text string, at time.Time) string) (rec, live, rTime, fTime string, tw Decision, ds []Decision) {
	t.Helper()
	return amongBindsAgo(t, caps, timeForm, 0, 0, edit)
}

// amongBindsAgo is amongBindsCase with the time rAgo earlier when recorded
// and fAgo earlier in round F: past the call's window, no plain read of the
// clock that window holds.
func amongBindsAgo(t *testing.T, caps Caps, timeForm bool, rAgo, fAgo time.Duration, edit func(text string, at time.Time) string) (rec, live, rTime, fTime string, tw Decision, ds []Decision) {
	t.Helper()
	rAt, fAt := oAt, oAt.Add(73*time.Minute)
	rRead, fRead := rAt.Add(-time.Second-rAgo), fAt.Add(-time.Second-fAgo)
	rTime, fTime = fmt.Sprint(rRead.Unix()), fmt.Sprint(fRead.Unix())
	rec, live = aPrompt(0, rRead, true, ""), aPrompt(1, fRead, true, "")
	if edit != nil {
		rec, live = edit(rec, rAt), edit(live, fAt)
	}
	in := oInput("body.messages.1.content", rec, live, nil, nil)
	in.Window = iWin(rAt, fAt, 5*time.Second)
	in.Caps, in.TimeForm = caps, timeForm
	ds = Decide(in)
	n := 0
	for _, d := range ds {
		if d.Recorded == rTime && d.Full == fTime {
			tw, n = d, n+1
		}
	}
	if n != 1 {
		t.Fatalf("the time decided %d times; want once: %s", n, oDescribe(ds[:min(len(ds), 6)]))
	}
	return rec, live, rTime, fTime, tw, ds
}

var amongCaps = Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true, ClockAmongBinds: true}

// A time inside a text whose every other changed word is an id learned
// fresh or bound (the 22 UUIDs of a run's prompt) is, under the time
// default, a Clock checked by its form alone at its word in the recorded
// text: an adapter that puts the ids' recorded values back first
// (Caps.ClockAmongBinds) reads the text around the time as recorded, so the
// replay's check (CheckClockReadForm) passes any epoch second there, and
// refuses an epoch of another unit, or the text where an id was not put
// back. Before, the time was refused cannot-check, since the text around it
// changed.
func TestATimeAmongBoundWordsIsCheckedByItsForm(t *testing.T) {
	rec, _, rTime, fTime, d, ds := amongBindsCase(t, amongCaps, true, nil)
	for _, o := range ds {
		if o.Recorded != rTime && (o.Refusal != nil || o.Outcome != OutcomeFresh && o.Outcome != OutcomeValue) {
			t.Fatalf("fixture: %s; want every id group learned", oDescribe([]Decision{o}))
		}
	}
	if d.Outcome != OutcomeClock || d.Clock == nil || d.Refusal != nil {
		t.Fatalf("the time: %s; want a Clock checked by its form", oDescribe([]Decision{d}))
	}
	o := *d.Clock
	if o.Form == "" || o.Word.IsZero() || !o.Word.Valid() || o.Min != 0 || o.Max != 0 {
		t.Fatalf("the time's check %+v; want its form alone, at a word of the text", o)
	}
	i := strings.Index(rec, rTime)
	if want := models.ClockWordOf(rec, i, i+len(rTime)); o.Word != want {
		t.Errorf("the time's word %+v; want its place in the recorded text %+v", o.Word, want)
	}
	putBack := func(word string) Leaf {
		return Leaf{Path: "body.messages.1.content", Type: String, Value: strings.Replace(rec, rTime, word, 1)}
	}
	for _, c := range []struct {
		name string
		live Leaf
		pass bool
	}{
		{"round F's time", putBack(fTime), true},
		{"another epoch second", putBack(fmt.Sprint(oAt.Add(240 * time.Hour).Unix())), true},
		{"an epoch in milliseconds", putBack(fmt.Sprint(oAt.UnixMilli())), false},
		{"a word that is no time", putBack("soon"), false},
		{"an id not put back", Leaf{Path: "body.messages.1.content", Type: String, Value: strings.Replace(strings.Replace(rec, rTime, fTime, 1), aUUID(0, 0), aUUID(1, 0), 1)}, false},
	} {
		if ok, why := CheckClockReadForm("body.messages.1.content", o, c.live, Span{}, true, true); ok != c.pass {
			t.Errorf("%s: the replay's check passed=%v (%s); want %v", c.name, ok, why, c.pass)
		}
	}
}

// Fail closed: the time stays refused cannot-check, as before, where the
// adapter does not put the other words back first (no ClockAmongBinds),
// under strict times (the time default off), and where another changed word
// of the text is refused (the replay would not put it back, so the text
// around the time never compares as recorded).
func TestATimeAmongBoundWordsStaysRefusedOtherwise(t *testing.T) {
	refused := func(name string, d Decision) {
		t.Helper()
		if d.Refusal == nil || d.Refusal.Kind != RefusalCannotCheck || d.Clock != nil {
			t.Errorf("%s: the time %s; want refused cannot-check, as before", name, oDescribe([]Decision{d}))
		}
	}
	noCaps := amongCaps
	noCaps.ClockAmongBinds = false
	_, _, _, _, d, _ := amongBindsCase(t, noCaps, true, nil)
	refused("an adapter that checks the text as recorded", d)
	_, _, _, _, d, _ = amongBindsCase(t, amongCaps, false, nil)
	refused("strict times", d)
	// A second read of the clock, two seconds before each run's call: a
	// Clock (or refused), which no replay puts back, so neither time's text
	// compares as recorded around it.
	_, _, rTime, _, d, ds := amongBindsCase(t, amongCaps, true, func(text string, at time.Time) string {
		return text + fmt.Sprintf("Finished at %d.\n", at.Add(-2*time.Second).Unix())
	})
	second := 0
	for _, o := range ds {
		if o.Recorded == fmt.Sprint(oAt.Add(-2*time.Second).Unix()) {
			second++
			if o.Clock != nil && o.Clock.Form != "" || o.Outcome == OutcomeValue || o.Outcome == OutcomeFresh {
				t.Errorf("the second time: %s; want it refused, or a Clock in its window, never by its form or bound", oDescribe([]Decision{o}))
			}
		}
	}
	if second != 1 {
		t.Fatalf("fixture: the second time decided %d times; want once", second)
	}
	refused("beside another read of the clock ("+rTime+")", d)
}

// Among bound words the time default settles what it settles at a word of a
// text the same around it (decider.settles), and nothing else. A look-back
// (the clock an hour before each run's call, a read plus an offset this
// replay does not compare) is refused cannot-check under strict times; under
// the time default it is a Clock checked by its form alone at its word
// (founder 11-11: a time learned as a clock read passes by type and format
// in T2, those refused cannot-check included). A time learning read as no
// clock read (an hour before its call when recorded, three in round F) is no
// time field: it keeps its refusal (no-clock-read) under both.
func TestATimeAmongBoundWordsIsSettledAsAWordOfAnUnchangedText(t *testing.T) {
	learnedAround := func(name string, ds []Decision, rTime string) {
		t.Helper()
		for _, o := range ds {
			if o.Recorded != rTime && (o.Refusal != nil || o.Outcome != OutcomeFresh && o.Outcome != OutcomeValue) {
				t.Fatalf("fixture (%s): %s; want every id group learned", name, oDescribe([]Decision{o}))
			}
		}
	}
	rec, _, rTime, _, strict, _ := amongBindsAgo(t, amongCaps, false, time.Hour, time.Hour, nil)
	if strict.Refusal == nil || strict.Refusal.Kind != RefusalCannotCheck || strict.Clock != nil {
		t.Fatalf("fixture: the look-back under strict times %s; want refused cannot-check", oDescribe([]Decision{strict}))
	}
	_, _, _, _, d, ds := amongBindsAgo(t, amongCaps, true, time.Hour, time.Hour, nil)
	learnedAround("the look-back", ds, rTime)
	i := strings.Index(rec, rTime)
	if d.Outcome != OutcomeClock || d.Refusal != nil || d.Clock == nil || d.Clock.Form == "" || d.Clock.Min != 0 || d.Clock.Max != 0 ||
		d.Clock.Word != models.ClockWordOf(rec, i, i+len(rTime)) {
		t.Errorf("the look-back: %s; want a Clock checked by its form alone at its word", oDescribe([]Decision{d}))
	}
	for _, timeForm := range []bool{false, true} {
		_, _, rTime, _, d, ds := amongBindsAgo(t, amongCaps, timeForm, time.Hour, 3*time.Hour, nil)
		learnedAround("no clock read", ds, rTime)
		if d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalNoClockRead || d.Clock != nil {
			t.Errorf("a time learning read as no clock read (time default %v): %s; want refused no-clock-read, never a Clock by its form", timeForm, oDescribe([]Decision{d}))
		}
	}
}

// The refusals the time default settles among bound words (settles, typed
// never: a text's word is no decoder-typed time): one where learning read
// the value as the clock (readClock: clock's own refusal of a read at a word
// of a text that changed around it, a look-back, an echoed read) of the
// kinds timeSettles names, any of those where a clock rule names the field,
// and two sources that are both reads of the clock. A refusal learning gave
// of a value it read as no clock read (a pick, a conflict, no clock read)
// gets no check by its form, nor does any where the time default is off.
func TestATimeAmongBoundWordsSettlesAsTheTimeDefault(t *testing.T) {
	rAt, fAt := oAt, oAt.Add(73*time.Minute)
	r, f := rAt.Add(-time.Second), fAt.Add(-time.Second)
	rs, fs := fmt.Sprint(r.Unix()), fmt.Sprint(f.Unix())
	rec := "sent " + rs + " by job 7"
	word := models.ClockWordOf(rec, len("sent "), len("sent "+rs))
	rm := moment{key: instantKey{at: r.UnixNano()}, res: time.Second, zoned: true}
	lm := moment{key: instantKey{at: f.UnixNano()}, res: time.Second, zoned: true}
	for _, c := range []struct {
		name              string
		kind              RefusalKind
		readClock, decl   bool
		timeForm, settled bool
	}{
		{"control: clock's refusal of a read at its word", RefusalCannotCheck, true, false, true, true},
		{"the same under strict times", RefusalCannotCheck, true, false, false, false},
		{"cannot-check of a value read as no clock read", RefusalCannotCheck, false, false, true, false},
		{"no-clock-read of a value read as no clock read", RefusalNoClockRead, false, false, true, false},
		{"no-clock-read where a clock rule names the field", RefusalNoClockRead, false, true, true, true},
		{"another scope's read of the clock", RefusalAnotherScope, true, false, true, true},
		{"two sources, each a read of the clock", RefusalTwoSources, true, false, true, true},
		{"two sources that are no reads", RefusalTwoSources, false, false, true, false},
		{"a pick, though read as the clock", RefusalPick, true, false, true, false},
		{"a conflict where a clock rule names the field", RefusalConflict, false, true, true, false},
	} {
		in := oInput("body.note", rec, "sent "+fs+" by job 8", nil, nil)
		in.Window = iWin(rAt, fAt, 5*time.Second)
		in.Caps, in.TimeForm = amongCaps, c.timeForm
		dc := &decider{in: in, readClock: c.readClock}
		ci := changedInstant{r: rs, l: fs, rm: &rm, lm: &lm, text: true, noWord: noWordAround, around: word}
		d := dc.refuse("body.note", variables.Address{}, 1, rs, fs, c.kind, "as learned", nil)
		got := dc.timeDefault("body.note", variables.Address{}, 1, ci, d, c.decl)
		if got.Refusal == nil || got.Refusal.Kind != c.kind || got.Clock != nil {
			t.Errorf("%s: the decision %s; want the refusal kept, its check by form held apart (around)", c.name, oDescribe([]Decision{got}))
		}
		switch {
		case c.settled && (dc.around == nil || dc.around.Clock == nil || dc.around.Clock.Form == "" || dc.around.Clock.Word != word):
			t.Errorf("%s: around %+v; want a Clock checked by its form at its word", c.name, dc.around)
		case !c.settled && dc.around != nil:
			t.Errorf("%s: around %s; want none (it stays refused)", c.name, oDescribe([]Decision{*dc.around}))
		}
	}
}

// The check by form stands at a word of the recorded text, and never at a
// key (amongBinds): a read with no word there (no around word: the recorded
// text is the read whole, while round F's has more around it) gets no
// check, nor does one at a key no clock rule names (atKey: a URL path
// segment, a leaf under _id), nor one the two runs write in no one form of
// an instant (timeFormOf: here an epoch number two years from its call);
// each stays refused cannot-check. The controls: the same read at its
// recorded word is checked by its form there, at a key too where a clock
// rule names it.
func TestATimeAmongBoundWordsNeedsItsRecordedWord(t *testing.T) {
	rAt, fAt := oAt, oAt.Add(73*time.Minute)
	word := models.ClockWordOf("sent 1793437199", len("sent "), len("sent 1793437199"))
	for _, c := range []struct {
		name, path string
		around     models.ClockWord
		declared   bool
		ago        time.Duration
		checked    bool
	}{
		{"control: the read at its recorded word", "body.note", word, false, 0, true},
		{"a read with no word in the recorded text", "body.note", models.ClockWord{}, false, 0, false},
		{"a read at a key (a leaf under _id)", "body.filter._id", word, false, 0, false},
		{"a read at a URL path segment", "url.path.2", word, false, 0, false},
		{"control: a read at a key a clock rule names", "body.filter._id", word, true, 0, true},
		{"an epoch number two years from its call (no form)", "body.note", word, false, 2 * 366 * 24 * time.Hour, false},
	} {
		r, f := rAt.Add(-time.Second-c.ago), fAt.Add(-time.Second-c.ago)
		rs, fs := fmt.Sprint(r.Unix()), fmt.Sprint(f.Unix())
		rec := "sent " + rs
		rm := moment{key: instantKey{at: r.UnixNano()}, res: time.Second, zoned: true}
		lm := moment{key: instantKey{at: f.UnixNano()}, res: time.Second, zoned: true}
		in := oInput(c.path, rec, "sent "+fs, nil, nil)
		in.Window = iWin(rAt, fAt, 5*time.Second)
		in.Caps, in.TimeForm = amongCaps, true
		dc := &decider{in: in}
		ci := changedInstant{r: rs, l: fs, rm: &rm, lm: &lm, text: true, noWord: noWordAround, around: c.around}
		d := dc.cannotCheck(c.path, variables.Address{}, 1, rs, fs, noWordAround)
		got := dc.amongBinds(c.path, variables.Address{}, 1, ci, d, c.declared)
		switch {
		case c.checked && (got == nil || got.Clock == nil || got.Clock.Form == "" || got.Clock.Word != c.around):
			t.Errorf("%s gave %+v; want a Clock checked by its form at its word", c.name, got)
		case !c.checked && got != nil:
			t.Errorf("%s gave %s; want no check (it stays refused cannot-check)", c.name, oDescribe([]Decision{*got}))
		}
	}
}

// aroundWordOf names the word a read stands at in its recorded text only
// where that text changed around it (noWordAround) and text stands on a
// side of it: a read that is the recorded text whole, or one refused for
// another reason (a part of the text keploy opened), has none, so
// amongBinds checks neither by its form.
func TestAroundWordOfNamesOnlyAWordOfAChangedText(t *testing.T) {
	rec := "sent 1793437199 by job 7"
	at := [2]int{len("sent "), len("sent 1793437199")}
	opened := "it stands in a part of the text keploy opened, where a replay reads no clock yet"
	for _, c := range []struct {
		name, rec, noWord string
		at                [2]int
		want              models.ClockWord
	}{
		{"a word of a text that changed around it", rec, noWordAround, at, models.ClockWordOf(rec, at[0], at[1])},
		{"the recorded text whole", "1793437199", noWordAround, [2]int{0, len("1793437199")}, models.ClockWord{}},
		{"a word of a part of the text keploy opened", rec, opened, at, models.ClockWord{}},
		{"a word whose text is the same around it", rec, "", at, models.ClockWord{}},
	} {
		if got := aroundWordOf(c.rec, &instantWord{RecAt: c.at}, c.noWord); got != c.want {
			t.Errorf("%s: the around word %+v; want %+v", c.name, got, c.want)
		}
	}
}
