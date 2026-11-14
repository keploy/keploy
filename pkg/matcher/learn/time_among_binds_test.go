package learn

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// amongBindsCase is the prompt of a run's ids and its time (aPrompt), read
// by Decide with the adapter's caps and the time default as given, and the
// decision on its time word. The time is a read of the clock its call's
// window holds, and every id group around it is learned fresh or bound.
func amongBindsCase(t *testing.T, caps Caps, timeForm bool, edit func(text string, at time.Time) string) (rec, live, rTime, fTime string, tw Decision, ds []Decision) {
	t.Helper()
	rAt, fAt := oAt, oAt.Add(73*time.Minute)
	rTime, fTime = fmt.Sprint(rAt.Add(-time.Second).Unix()), fmt.Sprint(fAt.Add(-time.Second).Unix())
	rec, live = aPrompt(0, rAt.Add(-time.Second), true, ""), aPrompt(1, fAt.Add(-time.Second), true, "")
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
