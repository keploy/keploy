package http

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// Two tests each PUT a key record whose expiresAt the application stamps
// with its clock's second plus a year. When recorded, the two PUTs fell in
// one second, so the later test's expiresAt was bound whole to the earlier
// test's (mock-3272), whose field keploy learned as a read of the clock plus
// 365 days. In round F the two stamps fall in different seconds: the bind
// does not hold, and the field is a time all the same, an offset-built one,
// so the time default checks it by its form alone instead of refusing the
// call. The nearness an epoch number must have to its call is that call
// moved by the offset learning read there too (an expiry two years on).
// Every other case keeps the refusal, on the run's exact line: the time
// default off (keploy.yml's times: strict), a bound place learning read no
// clock at, and no window.
func TestAnExpiryBoundToAnotherTestsExpiryIsCheckedByItsForm(t *testing.T) {
	// The bound place is another test's call: the run's variable, carried
	// only under the proven switch.
	t.Setenv("KEPLOY_PROVEN", "1")
	const field, bound = "body.data.expiresAt", "body.data.expiresAt"
	const year = 365 * 24 * time.Hour
	recAt := time.Date(2026, 11, 12, 2, 13, 32, 412_000_000, time.UTC)
	liveAt := time.Date(2026, 11, 12, 2, 16, 26, 655_000_000, time.UTC)
	window := &learn.Window{
		Rec: learn.Span{From: recAt.Add(-200 * time.Millisecond), To: recAt},
		F:   learn.Span{From: liveAt.Add(-200 * time.Millisecond), To: liveAt},
	}
	secs := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	refusal := field + " does not hold the value the test sent at " + bound + " of mock-3272, which it held when recorded"
	m := &models.Mock{Name: "mock-3521"}
	jsonReq := learn.Shape{Fixed: map[string]string{"body": "json"}}
	offset := func(d time.Duration) *models.ClockOffset {
		return &models.ClockOffset{Min: d, Max: d + time.Second, Res: time.Second}
	}
	for _, tc := range []struct {
		what string
		// ttl is the bound place's offset from the clock as learned, and
		// the expiry each run's stamp stands at from its call; liveTTL,
		// where not 0, round F's own.
		ttl, liveTTL time.Duration
		// read is what learning read the bound place as: an offset read
		// (nil: none).
		read     *models.ClockOffset
		timeForm bool
		w        *learn.Window
		refused  bool
	}{
		{what: "a year on, stamped in another second in round F", ttl: year, read: offset(year), timeForm: true, w: window},
		{what: "a year on, the time default off", ttl: year, read: offset(year), w: window, refused: true},
		{what: "a year on, a place learning read no clock at", ttl: year, timeForm: true, w: window, refused: true},
		{what: "a year on, no window", ttl: year, read: offset(year), timeForm: true, refused: true},
		{what: "two years on, near its call moved by the offset", ttl: 2 * year, read: offset(2 * year), timeForm: true, w: window},
		{what: "two years on, round F's own a day on", ttl: 2 * year, liveTTL: 24 * time.Hour, read: offset(2 * year), timeForm: true, w: window},
		{what: "three years on, a place read as the clock a year on", ttl: 3 * year, read: offset(year), timeForm: true, w: window, refused: true},
	} {
		t.Run(tc.what, func(t *testing.T) {
			tr := learn.NewTracker()
			tr.Begin()
			test := tr.Epoch()
			liveTTL := tc.ttl
			if tc.liveTTL != 0 {
				liveTTL = tc.liveTTL
			}
			rec := learn.Leaf{Path: field, Value: secs(recAt.Add(tc.ttl)), Type: learn.Number}
			live := learn.Leaf{Path: field, Value: secs(liveAt.Add(liveTTL)), Type: learn.Number}
			// The other test's PUT stamped its key a second before this one
			// in round F.
			from := learn.Place{Mock: "mock-3272", Side: learn.Request, Path: bound, Var: bound, Live: secs(liveAt.Add(-time.Second).Add(tc.ttl)), Whole: true, Run: true, Offset: tc.read}
			learned := boundTimesOf([]learn.Learned{{Kind: learn.KindVariable, Recorded: rec, Live: live, From: from}}, nil, m, tc.w, tc.timeForm)
			p, why := learnPlan(tr, test, m, jsonReq, true, learned, nil, nil)
			if tc.refused {
				if why != refusal || len(p.clocks) != 0 || len(p.vars) != 0 {
					t.Fatalf("why %q, clocks %v, variables %v; want refused: %s", why, p.clocks, p.vars, refusal)
				}
				return
			}
			o, ok := p.clocks[field]
			if why != "" || !ok || o.Form != "number 9999999999" || !o.Plain() || len(p.vars) != 0 {
				t.Fatalf("why %q, clocks %+v, variables %v; want %s checked by its form alone, no variable", why, p.clocks, p.vars, field)
			}
			if !strings.Contains(o.Why, "where keploy learned a read of the application's clock plus an offset") || !strings.HasSuffix(o.Why, ": "+refusal) {
				t.Errorf("the clock's why %q; want the offset-built time named, then the bind that did not hold", o.Why)
			}
			// A replay passes the next second's expiry under the time
			// default, and refuses it under strict.
			later := learn.Leaf{Path: field, Value: secs(liveAt.Add(time.Hour).Add(tc.ttl)), Type: learn.Number}
			if held, no := learn.CheckClockReadForm(field, o, later, learn.Span{}, true, true); !held {
				t.Errorf("a later replay's expiry of the same form is refused: %s", no)
			}
			if held, _ := learn.CheckClockReadForm(field, o, later, learn.Span{}, true, false); held {
				t.Error("a later replay's expiry passes under strict times; want it refused")
			}
		})
	}
}
