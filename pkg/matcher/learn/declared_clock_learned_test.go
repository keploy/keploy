package learn

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// DeclaredClockLearned settles what learning could not tell of a value's
// source at a field a clock rule declares (the PAT touch's lastUsedAt, whose
// second a test's UnixNano app name holds in both runs): an echo, noise or an
// unaccounted drift is a read of the clock where the rule's range holds both
// runs' values in the call's span; under the time default, a time of one
// form (an epoch number near its call) checked by that form, a range read
// too. Noise neither check holds is refused naming the rule; an echo or an
// unaccounted drift keeps its refusal. Every other decision and rule kind
// stands.
func TestDeclaredClockLearnedSettlesWhatLearningCannotTellOfItsSource(t *testing.T) {
	recAt := time.Date(2026, 11, 11, 16, 11, 22, 519_000_000, time.UTC)
	fAt := time.Date(2026, 11, 11, 16, 13, 20, 260_000_000, time.UTC)
	w := &Window{
		Rec: Span{From: recAt.Add(-5 * time.Millisecond), To: recAt},
		F:   Span{From: fAt.Add(-5 * time.Millisecond), To: fAt},
	}
	epoch := func(at time.Time) Leaf {
		return Leaf{Path: "body.data.lastUsedAt", Type: Number, Value: strconv.FormatInt(at.Unix(), 10)}
	}
	stamp := func(at time.Time) Leaf {
		return Leaf{Path: "body.data.updatedAt", Type: String, Value: at.Format(time.RFC3339)}
	}
	echo := func(rec, live Leaf) Learned {
		return Learned{Kind: KindVariable, Recorded: rec, Live: live, From: Place{Mock: "test case post-client-v1-apps-1", Path: "body.name", Echo: true}}
	}
	as := func(k Kind, l Learned) Learned {
		return Learned{Kind: k, Recorded: l.Recorded, Live: l.Live, Why: "learning's own reason", Refuted: "a source set aside"}
	}
	// bySource is l refused for its source alone (Learned.sourceWhy).
	bySource := func(l Learned) Learned {
		l.sourceWhy = l.Why
		return l
	}
	stale := func(l Learned) Learned {
		l.sourceWhy = "an earlier reason"
		return l
	}
	millis := func(at time.Time) Leaf {
		return Leaf{Path: "body.data.lastUsedAt", Type: Number, Value: strconv.FormatInt(at.UnixMilli(), 10)}
	}
	tod := func(at time.Time) Leaf {
		return Leaf{Path: "body.data.lastUsedAt", Type: String, Value: at.Format(time.TimeOnly)}
	}
	clock := Source{Kind: SourceClock, Rule: "pat-last-used-at", Clock: models.ClockOffset{Min: -time.Second, Res: time.Second}}
	noRange := Source{Kind: SourceClock, Rule: "pat-last-used-at", Clock: models.ClockOffset{Max: time.Second}}
	minted := Source{Kind: SourceMinted, Rule: "pat-last-used-at"}
	held := echo(epoch(recAt), epoch(fAt))
	lateF := echo(epoch(recAt), epoch(fAt.Add(time.Minute)))
	lateRec := echo(epoch(recAt.Add(-time.Minute)), epoch(fAt))
	name := Leaf{Path: "body.data.lastUsedAt", Type: String, Value: "e2e-go"}
	const epochForm, stampForm = "number 9999999999", "text 9999-99-99a99:99:99a"
	const (
		kept    = ""
		ranged  = "range"
		refused = "refused"
	)
	for _, c := range []struct {
		name     string
		ln       Learned
		src      Source
		w        *Window
		timeForm bool
		want     string // kept, ranged (a range read), refused, or a form
		form     string // of a range read: "" for none
		why      string // of a refusal, a part of its Why
	}{
		{"an echo both runs read in the span", held, clock, w, false, ranged, "", ""},
		{"an echo both runs read in the span, time default", held, clock, w, true, ranged, epochForm, ""},
		{"an echo round F read a minute late", lateF, clock, w, false, kept, "", ""},
		{"an echo round F read a minute late, time default", lateF, clock, w, true, epochForm, "", ""},
		{"an echo whose runs write two forms", echo(epoch(recAt), Leaf{Path: "body.data.lastUsedAt", Type: Number, Value: strconv.FormatInt(fAt.Add(time.Minute).UnixMilli(), 10)}), clock, w, true, kept, "", ""},
		{"an echo of no time", echo(name, name), clock, w, true, kept, "", ""},
		{"an echo round F read over a year from its call", echo(epoch(recAt), epoch(fAt.Add(400*24*time.Hour))), clock, w, true, kept, "", ""},
		{"an echo recorded over a year from its call", echo(epoch(recAt.Add(-400*24*time.Hour)), epoch(fAt.Add(time.Minute))), clock, w, true, kept, "", ""},
		{"an epoch echo with no window, time default", held, clock, nil, true, kept, "", ""},
		{"a stamp echo with no window, time default", echo(stamp(recAt), stamp(fAt.Add(time.Minute))), clock, nil, true, stampForm, "", ""},
		{"a stamp echo with no window", echo(stamp(recAt), stamp(fAt.Add(time.Minute))), clock, nil, false, kept, "", ""},
		{"noise both runs read in the span", as(KindNoise, held), clock, w, false, ranged, "", ""},
		{"noise round F read a minute late", as(KindNoise, lateF), clock, w, false, refused, "", "keploy.yml's rule pat-last-used-at declares it a read of the clock, and round F's value is no such read in its run"},
		{"noise recorded a minute early", as(KindNoise, lateRec), clock, w, false, refused, "", "and the recorded value is no such read in its run"},
		{"noise round F read a minute late, time default", as(KindNoise, lateF), clock, w, true, epochForm, "", ""},
		{"noise of no time, time default", as(KindNoise, echo(name, name)), clock, w, true, refused, "", "its two runs write no one form of an instant (none recorded, none in round F)"},
		{"noise over a year from its call, time default", as(KindNoise, echo(epoch(recAt), epoch(fAt.Add(400*24*time.Hour)))), clock, w, true, refused, "", "an epoch number, it names no moment near its call in both runs"},
		{"noise with no window", as(KindNoise, held), clock, nil, false, refused, "", "keploy knows no span of the call"},
		{"noise under a range keploy does not check", as(KindNoise, held), noRange, w, false, refused, "", "its range is one keploy does not check"},
		{"noise under a range keploy does not check, time default", as(KindNoise, lateF), noRange, w, true, epochForm, "", ""},
		{"noise in two layouts both runs read in the span", as(KindNoise, echo(epoch(recAt), millis(fAt))), clock, w, false, refused, "", "and its two runs write it in different layouts"},
		{"noise in two layouts, time default", as(KindNoise, echo(epoch(recAt), millis(fAt))), clock, w, true, refused, "", "write no one form of an instant (number 9999999999 recorded, number 9999999999999 in round F)"},
		{"an echo in two layouts both runs read in the span", echo(epoch(recAt), millis(fAt)), clock, w, false, kept, "", ""},
		{"noise of a time of day", as(KindNoise, echo(tod(recAt), tod(fAt))), clock, w, false, refused, "", "keploy reads no moment in its value in a run"},
		{"noise of a time of day, time default", as(KindNoise, echo(tod(recAt), tod(fAt))), clock, w, true, refused, "", "write no one form of an instant (text 99:99:99 recorded, text 99:99:99 in round F)"},
		{"a drift refused for its source both runs read in the span", bySource(as(KindUnaccounted, held)), clock, w, false, ranged, "", ""},
		{"a drift refused for its source round F read a minute late", bySource(as(KindUnaccounted, lateF)), clock, w, false, kept, "", ""},
		{"a drift refused for its source round F read a minute late, time default", bySource(as(KindUnaccounted, lateF)), clock, w, true, epochForm, "", ""},
		{"a drift refused for another reason", as(KindUnaccounted, held), clock, w, true, kept, "", ""},
		{"a drift refused with no reason", Learned{Kind: KindUnaccounted, Recorded: held.Recorded, Live: held.Live}, clock, w, true, kept, "", ""},
		{"a drift refused for its source, then for another reason", stale(as(KindUnaccounted, held)), clock, w, true, kept, "", ""},
		{"an offset from the clock learning found", Learned{Kind: KindClockOffset, Recorded: lateF.Recorded, Live: lateF.Live, Offset: &models.ClockOffset{Min: time.Minute, Max: time.Minute, Res: time.Second}}, clock, w, true, kept, "", ""},
		{"a variable bound where it stands whole", Learned{Kind: KindVariable, Recorded: held.Recorded, Live: held.Live, From: Place{Mock: "mock-3", Path: "body.lastUsedAt", Whole: true}}, clock, w, true, kept, "", ""},
		{"a read of the clock learning found", Learned{Kind: KindClockRead, Recorded: lateF.Recorded, Live: lateF.Live, Read: &models.ClockOffset{Res: time.Minute}}, clock, w, true, kept, "", ""},
		{"an echo the answer carries", as(KindEcho, held), clock, w, true, kept, "", ""},
		{"a minted rule", held, minted, w, true, kept, "", ""},
		{"a minted rule on noise", as(KindNoise, lateF), minted, w, false, kept, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := DeclaredClockLearned(c.ln, c.src, c.w, c.timeForm)
			switch c.want {
			case kept:
				if !reflect.DeepEqual(got, c.ln) {
					t.Fatalf("got %+v; want learning's decision %+v kept", got, c.ln)
				}
				return
			case refused:
				if got.Kind != KindUnaccounted || got.Read != nil || got.Recorded != c.ln.Recorded || got.Live != c.ln.Live || got.Refuted != c.ln.Refuted || !strings.Contains(got.Why, c.why) || !strings.Contains(got.Why, "pat-last-used-at") {
					t.Fatalf("got %+v; want it refused, naming the rule and %q", got, c.why)
				}
				return
			}
			o, ok := got.Clock()
			if got.Kind != KindClockRead || !ok || got.Recorded != c.ln.Recorded || got.Live != c.ln.Live || got.Refuted != c.ln.Refuted || got.Why != o.Why || !strings.Contains(o.Why, "pat-last-used-at") {
				t.Fatalf("got %+v (clock %+v); want a read of the clock the rule names", got, o)
			}
			if c.want == ranged {
				if o.Res != time.Second || o.Form != c.form || o.Layout != "epoch-s" {
					t.Fatalf("clock %+v; want the rule's range checked at a cut of 1s in the runs' layout, form %q", o, c.form)
				}
				return
			}
			if o.Form != c.want || o.Res != 0 {
				t.Fatalf("clock %+v; want a time checked by its form alone, %q", o, c.want)
			}
		})
	}
}

// DeclaredBodySources reads a rule where a replay checks a declared clock
// (DeclaredClocks): a body field at its exact leaf path. A header's rule
// names no leaf the learn step decides, and a list of no body rule is nil.
func TestDeclaredBodySourcesReadsABodyFieldAtItsLeafPath(t *testing.T) {
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	body := models.DeclaredSource{Rule: "pat-last-used-at", Kind: models.DeclaredSourceClock, Field: "body.data.lastUsedAt", Clock: plain}
	header := models.DeclaredSource{Rule: "sent-at", Kind: models.DeclaredSourceClock, Field: "header.X-Sent-At", Clock: plain}
	src := DeclaredBodySources([]models.DeclaredSource{body, header})
	if src == nil {
		t.Fatal("a body rule beside a header's: nil; want the body rule read")
	}
	if s, ok := src("body.data.lastUsedAt"); !ok || s.Kind != SourceClock || s.Rule != "pat-last-used-at" || s.Clock != *plain {
		t.Fatalf("body.data.lastUsedAt: %+v %v; want the body rule", s, ok)
	}
	for _, leaf := range []string{"header.X-Sent-At", "header.x-sent-at", "body.data", "body.data.lastUsedAt.x"} {
		if s, ok := src(leaf); ok {
			t.Errorf("%s: %+v; want no rule", leaf, s)
		}
	}
	if DeclaredBodySources([]models.DeclaredSource{header}) != nil || DeclaredBodySources(nil) != nil {
		t.Fatal("no body rule: want nil")
	}
}
