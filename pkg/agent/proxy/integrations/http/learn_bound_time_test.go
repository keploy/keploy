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
		{what: "a bind that holds", live: secs(liveAt), sourceLive: secs(liveAt), clock: true, timeForm: true, w: window, bound: true},
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
			p, why := learnPlan(tr, test, m, jsonReq, true, learned, nil, nil)
			switch {
			case tc.refused != "":
				if why != tc.refused || len(p.clocks) > 1 || len(p.vars) != 0 {
					t.Fatalf("why %q, clocks %v, variables %v; want refused: %s", why, p.clocks, p.vars, tc.refused)
				}
			case tc.bound:
				if why != "" || len(p.clocks) != 1 || len(p.vars) != 1 {
					t.Fatalf("why %q, clocks %v, variables %v; want the variable bound", why, p.clocks, p.vars)
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
				if held, no := learn.CheckClockRead(field, o, learn.Leaf{Path: field, Value: secs(liveAt.Add(time.Hour)), Type: learn.Number}, learn.Span{}, true); !held {
					t.Errorf("a later replay's createdAt of the same form is refused: %s", no)
				}
				if held, _ := learn.CheckClockRead(field, o, learn.Leaf{Path: field, Value: strconv.FormatInt(liveAt.UnixMilli(), 10), Type: learn.Number}, learn.Span{}, true); held {
					t.Error("a later replay's createdAt in milliseconds passes; want it refused by its form")
				}
			}
		})
	}
}
