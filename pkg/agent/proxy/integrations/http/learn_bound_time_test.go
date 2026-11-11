package http

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A key's createdAt, stamped by the application's clock, that fell in the
// same second as the app's audit createdAt (an earlier Mongo insert of the
// test, whose field keploy learned as a read of the clock) was bound whole
// to it when recorded. In round F the two reads fall in different seconds:
// the bind does not hold, and the field is a time, so the time default
// checks it by its form alone instead of refusing the call. Every other
// case keeps the refusal: a value that is no time, the time default off
// (keploy.yml's times: strict), a value written in another form in round
// F, and an epoch number far from its call. A bind that holds stays the
// variable.
func TestATimeWhoseWholeBindDoesNotHoldIsCheckedByItsForm(t *testing.T) {
	const field, sourceVar = "body.data.createdAt", "body.docs.msgs.{}.app.createdAt.{}"
	recAt := time.Date(2026, 11, 11, 18, 35, 46, 351_000_000, time.UTC)
	liveAt := time.Date(2026, 11, 11, 18, 37, 5, 527_000_000, time.UTC)
	window := &learn.Window{
		Rec: learn.Span{From: recAt.Add(-200 * time.Millisecond), To: recAt},
		F:   learn.Span{From: liveAt.Add(-200 * time.Millisecond), To: liveAt},
	}
	secs := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	refusal := field + " does not hold the value the test sent at " + sourceVar + " of mock-3232, which it held when recorded"
	m := &models.Mock{Name: "mock-3240"}
	jsonReq := learn.Shape{Fixed: map[string]string{"body": "json"}}
	clockRule := func(path string) func(string) []models.DeclaredSource {
		return func(string) []models.DeclaredSource {
			return []models.DeclaredSource{{Rule: "key-created-at", Kind: models.DeclaredSourceClock, Field: path, Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}}}
		}
	}
	for _, tc := range []struct {
		what       string
		live       string
		sourceLive string
		clock      bool
		rules      func(string) []models.DeclaredSource
		timeForm   bool
		w          *learn.Window
		refused    string
		bound      bool
		// answered: the bound call answered the value too; byForm: the
		// bound variable's use keeps a check by form for a replay where
		// the bind does not hold (models.MockVariable.ByForm).
		answered bool
		byForm   bool
	}{
		{what: "bound to a read of the clock, two seconds apart in round F", live: secs(liveAt), sourceLive: secs(liveAt.Add(-time.Second)), clock: true, timeForm: true, w: window},
		{what: "a field a clock rule declares", live: secs(liveAt), sourceLive: secs(liveAt.Add(-time.Second)), rules: clockRule(field), timeForm: true, w: window},
		{what: "a minted rule on the field", live: secs(liveAt), sourceLive: secs(liveAt.Add(-time.Second)), rules: func(string) []models.DeclaredSource {
			return []models.DeclaredSource{{Rule: "key-created-id", Kind: models.DeclaredSourceMinted, Field: field, Minted: &models.MintedForm{MinLen: 10, MaxLen: 10}}}
		}, timeForm: true, w: window, refused: refusal},
		{what: "a value that is no time", live: secs(liveAt), sourceLive: secs(liveAt.Add(-time.Second)), timeForm: true, w: window, refused: refusal},
		{what: "a clock rule on another field", live: secs(liveAt), sourceLive: secs(liveAt.Add(-time.Second)), rules: clockRule("body.data.updatedAt"), timeForm: true, w: window, refused: refusal},
		{what: "the time default off", live: secs(liveAt), sourceLive: secs(liveAt.Add(-time.Second)), clock: true, w: window, refused: refusal},
		{what: "milliseconds in round F", live: strconv.FormatInt(liveAt.UnixMilli(), 10), sourceLive: secs(liveAt.Add(-time.Second)), clock: true, timeForm: true, w: window, refused: refusal},
		{what: "an epoch far from its call", live: secs(liveAt), sourceLive: secs(liveAt.Add(-time.Second)), clock: true, timeForm: true, w: &learn.Window{Rec: window.Rec, F: learn.Span{From: liveAt.AddDate(2, 0, 0), To: liveAt.AddDate(2, 0, 0)}}, refused: refusal},
		{what: "no window", live: secs(liveAt), sourceLive: secs(liveAt.Add(-time.Second)), clock: true, timeForm: true, refused: refusal},
		{what: "a bind that holds", live: secs(liveAt), sourceLive: secs(liveAt), clock: true, timeForm: true, w: window, bound: true, byForm: true},
		{what: "a bind that holds, of a field a clock rule declares", live: secs(liveAt), sourceLive: secs(liveAt), rules: clockRule(field), timeForm: true, w: window, bound: true, byForm: true},
		{what: "a bind that holds, of a value that is no time", live: secs(liveAt), sourceLive: secs(liveAt), timeForm: true, w: window, bound: true},
		{what: "a bind that holds, the time default off", live: secs(liveAt), sourceLive: secs(liveAt), clock: true, w: window, bound: true},
		{what: "a bind that holds, of a value the bound call answered", live: secs(liveAt), sourceLive: secs(liveAt), clock: true, timeForm: true, w: window, bound: true, answered: true},
	} {
		t.Run(tc.what, func(t *testing.T) {
			tr := learn.NewTracker()
			tr.Begin()
			test := tr.Epoch()
			rec := learn.Leaf{Path: field, Value: secs(recAt), Type: learn.Number}
			live := learn.Leaf{Path: field, Value: tc.live, Type: learn.Number}
			from := learn.Place{Mock: "mock-3232", Side: learn.Request, Path: "body.docs.msgs.0.app.createdAt.$numberLong", Var: sourceVar, Live: tc.sourceLive, Whole: true, Own: true, Clock: tc.clock}
			// A read of the clock the call learned first, at another field,
			// stays beside it.
			updated := learn.Leaf{Path: "body.data.updatedAt", Value: secs(recAt), Type: learn.Number}
			read := learn.Learned{Kind: learn.KindClockRead, Recorded: updated, Live: learn.Leaf{Path: updated.Path, Value: tc.live, Type: learn.Number}, Read: &models.ClockOffset{Res: time.Second}}
			learned := boundTimesOf([]learn.Learned{read, {Kind: learn.KindVariable, Recorded: rec, Live: live, From: from}}, tc.rules, m, tc.w, tc.timeForm)
			var earlier []learn.Earlier
			if tc.answered {
				earlier = []learn.Earlier{{Mock: "mock-3232", Own: true, Seen: []learn.Seen{{Side: learn.Response, Place: "body.app.createdAt", Leaf: learn.Leaf{Path: "body.app.createdAt", Value: secs(recAt), Type: learn.Number}}}}}
			}
			p, why := learnPlan(tr, test, m, jsonReq, true, learned, earlier, nil)
			switch {
			case tc.refused != "":
				if why != tc.refused || len(p.clocks) > 1 || len(p.vars) != 0 {
					t.Fatalf("why %q, clocks %v, variables %v; want refused: %s", why, p.clocks, p.vars, tc.refused)
				}
			case tc.bound:
				if why != "" || len(p.clocks) != 1 || len(p.vars) != 1 {
					t.Fatalf("why %q, clocks %v, variables %v; want the variable bound", why, p.clocks, p.vars)
				}
				for name, v := range p.vars {
					o, ok := v.ByForm[field]
					if !tc.byForm {
						if len(v.ByForm) != 0 {
							t.Fatalf("variable %s's use is checked by form %+v; want it checked exactly", name, v.ByForm)
						}
						continue
					}
					if !ok || len(v.ByForm) != 1 || o.Form != "number 9999999999" || !strings.Contains(o.Why, "checked by its form alone") {
						t.Fatalf("variable %s (uses %v) is checked by form %+v; want %s checked by its form where the bind does not hold", name, v.Use, v.ByForm, field)
					}
					later := learn.Leaf{Path: field, Value: secs(liveAt.Add(time.Second)), Type: learn.Number}
					if !learn.HoldsByForm(o, later, true) || learn.HoldsByForm(o, later, false) {
						t.Errorf("a replay's next second passes by form %v under the time default, %v under strict; want only the time default", learn.HoldsByForm(o, later, true), learn.HoldsByForm(o, later, false))
					}
				}
			default:
				o, ok := p.clocks[field]
				if _, kept := p.clocks["body.data.updatedAt"]; !kept {
					t.Errorf("clocks %+v; want the call's read of the clock at body.data.updatedAt kept", p.clocks)
				}
				if why != "" || !ok || o.Form != "number 9999999999" || !o.Plain() || o.Res != 0 || len(p.vars) != 0 {
					t.Fatalf("why %q, clocks %+v, variables %v; want %s checked by its form alone, no variable", why, p.clocks, p.vars, field)
				}
				if !strings.Contains(o.Why, "checked by its form alone") || !strings.HasSuffix(o.Why, ": "+refusal) {
					t.Errorf("the clock's why %q; want the time default named, then the bind that did not hold", o.Why)
				}
				// A replay checks it as it reads the store's times setting
				// (CheckClockReadForm): at the default, any value of the form
				// passes and another form does not; under strict a time
				// learned by its form alone is refused.
				later := learn.Leaf{Path: field, Value: secs(liveAt.Add(time.Hour)), Type: learn.Number}
				if held, no := learn.CheckClockReadForm(field, o, later, learn.Span{}, true, true); !held {
					t.Errorf("a later replay's createdAt of the same form is refused: %s", no)
				}
				if held, _ := learn.CheckClockReadForm(field, o, learn.Leaf{Path: field, Value: strconv.FormatInt(liveAt.UnixMilli(), 10), Type: learn.Number}, learn.Span{}, true, true); held {
					t.Error("a later replay's createdAt in milliseconds passes; want it refused by its form")
				}
				if held, no := learn.CheckClockReadForm(field, o, later, learn.Span{}, true, false); held || !strings.Contains(no, "re-record with mock.times: strict") {
					t.Errorf("a later replay under strict times: held %v (%q); want it refused, saying to re-record under strict", held, no)
				}
			}
		})
	}
}

// One variable the call uses at two fields, each a time stamped by the
// application and bound whole to the same earlier read of the clock: each
// use keeps its own check by form, so a replay where the bind does not hold
// at either field checks that field by its form.
func TestTwoUsesOfOneBoundTimeEachKeepTheirCheckByForm(t *testing.T) {
	const created, issued = "body.data.createdAt", "body.data.issuedAt"
	recAt := time.Date(2026, 11, 11, 18, 35, 46, 351_000_000, time.UTC)
	liveAt := time.Date(2026, 11, 11, 18, 37, 5, 527_000_000, time.UTC)
	window := &learn.Window{
		Rec: learn.Span{From: recAt.Add(-200 * time.Millisecond), To: recAt},
		F:   learn.Span{From: liveAt.Add(-200 * time.Millisecond), To: liveAt},
	}
	secs := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	tr := learn.NewTracker()
	tr.Begin()
	test := tr.Epoch()
	from := learn.Place{Mock: "mock-3232", Side: learn.Request, Path: "body.docs.msgs.0.app.createdAt.$numberLong", Var: "body.docs.msgs.{}.app.createdAt.{}", Live: secs(liveAt), Whole: true, Own: true, Clock: true}
	var learned []learn.Learned
	for _, field := range []string{created, issued} {
		rec := learn.Leaf{Path: field, Value: secs(recAt), Type: learn.Number}
		live := learn.Leaf{Path: field, Value: secs(liveAt), Type: learn.Number}
		learned = append(learned, learn.Learned{Kind: learn.KindVariable, Recorded: rec, Live: live, From: from})
	}
	m := &models.Mock{Name: "mock-3240"}
	p, why := learnPlan(tr, test, m, learn.Shape{Fixed: map[string]string{"body": "json"}}, true, boundTimesOf(learned, nil, m, window, true), nil, nil)
	if why != "" || len(p.vars) != 1 {
		t.Fatalf("why %q, variables %v; want one variable bound", why, p.vars)
	}
	for name, v := range p.vars {
		if len(v.Use) != 2 || len(v.ByForm) != 2 {
			t.Fatalf("variable %s uses %v, checked by form at %v; want both fields used and checked by form", name, v.Use, v.ByForm)
		}
		for _, field := range []string{created, issued} {
			if o, ok := v.ByForm[field]; !ok || o.Form != "number 9999999999" {
				t.Errorf("variable %s's use at %s is checked by form %+v (%v); want its form kept", name, field, o, ok)
			}
		}
	}
}
