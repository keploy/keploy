package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// slotRequest is a test case's request listing slots, as recorded and as
// keploy test sent it (the same).
func slotRequest(slots ...string) *models.TestRequest {
	body := `{"slots":["` + strings.Join(slots, `","`) + `"]}`
	return &models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{
		{Name: "url", Recorded: "http://localhost:8080/book", Sent: "http://localhost:8080/book"},
		{Name: "header.Content-Type", Recorded: "application/json", Sent: "application/json"},
		{Name: "body", Recorded: body, Sent: body},
	}}
}

// The test case's request is read part by part, a JSON body's leaves ahead of
// the body whole, each as recorded and as sent; one that could not be read,
// or that the filter did not carry, is a place that says why, with nothing
// in it.
func TestGivenRequestReadsEachPart(t *testing.T) {
	req := slotRequest("11:14", "11:15")
	req.Parts[2].Sent = `{"slots":["11:14","11:16"]}`
	e := GivenRequest(req)
	if !e.Given || e.Unread != "" || e.Mock != "test case test-1" {
		t.Fatalf("%+v; want the test case's readable place", e)
	}
	var got []string
	for _, s := range e.Seen {
		got = append(got, s.Leaf.Path+"="+s.Leaf.Value+">"+s.Live)
		if s.Side != Request || s.Place != "" || s.Written {
			t.Errorf("%s: %+v; want a request leaf no variable binds at", s.Leaf.Path, s)
		}
	}
	want := []string{"url=http://localhost:8080/book>http://localhost:8080/book", "header.Content-Type=application/json>application/json",
		"body.slots[0]=11:14>11:14", "body.slots[1]=11:15>11:16", `body={"slots":["11:14","11:15"]}>{"slots":["11:14","11:16"]}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("leaves:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if e := GivenRequest(nil); !e.Given || e.Unread != unsentRequest || len(e.Seen) != 0 {
		t.Errorf("no request sent: %+v", e)
	}
	if e := GivenRequest(&models.TestRequest{Test: "test-2", Unread: "keploy reads no request of a test case of kind X", Parts: req.Parts}); e.Unread == "" || len(e.Seen) != 0 || e.Mock != "test case test-2" {
		t.Errorf("an unread request: %+v", e)
	}
}

// A slot the application picks by its clock from its test case's own
// request (the next after now) moves as a read of the clock does. The
// request is the test's earliest place, which the echo reads: the pick is a
// variable of the request's slot that held its recorded moment, which
// nothing binds at (refused by the adapter), or, the request listing only
// F's pick, unaccounted, naming where F's moment stands. Never noise, in any
// layout, by the lead or not.
func TestAPickFromTheTestRequestIsRefused(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const p = "body.slot"
	for _, f := range [][2]string{{"2026-10-22 11:14", "2026-10-22 11:15"}, {"2026-10-22T11:14:00Z", "2026-10-22T11:15:00Z"}, {"11:14", "11:15"}} {
		rec := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: f[0]}}}
		live := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: f[1]}}}
		// Without the request, nothing earlier holds it: noise (what the
		// learn step did before it read the request).
		if ln := ClassifyIn(NewDriftCache(), []string{p}, live, rec, at, nil, nil, nil, nil)[0]; ln.Kind != KindNoise {
			t.Errorf("%q -> %q, no test case's request: %s (%s); want noise", f[0], f[1], ln.Kind, ln.Why)
		}
		for _, slots := range [][]string{{"11:13", "11:14", "11:15", "11:16"}, {"2026-10-22T11:14:00Z", "2026-10-22T11:15:00Z"}, {"2026-10-22 11:14", "2026-10-22 11:15"}} {
			given := []Earlier{GivenRequest(slotRequest(slots...))}
			ln := ClassifyIn(NewDriftCache(), []string{p}, live, rec, at, given, nil, nil, nil)[0]
			if ln.Kind != KindVariable || !ln.From.Given || !ln.From.Echo || ln.From.Mock != "test case test-1" || !strings.HasPrefix(ln.From.Path, "body.slots[") || !strings.Contains(ln.Why, "stands in test case test-1 at body.slots[") {
				t.Errorf("%q -> %q, slots %v: %s from %+v (%s); want a variable of the test case's slot", f[0], f[1], slots, ln.Kind, ln.From, ln.Why)
			}
		}
		// The request lists F's pick only.
		given := []Earlier{GivenRequest(slotRequest("11:15", "11:16"))}
		if ln := ClassifyIn(NewDriftCache(), []string{p}, live, rec, at, given, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "stands in test case test-1 at body.slots[0]") {
			t.Errorf("%q -> %q, F's pick listed only: %s (%s); want it refused, naming the test case's slot", f[0], f[1], ln.Kind, ln.Why)
		}
	}
	// A typed pick too.
	typed := func(at time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, time.Second)}}
	}
	given := []Earlier{GivenRequest(slotRequest("11:14", "11:15"))}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, typed(time.Date(2026, 10, 22, 11, 15, 0, 0, time.UTC)), typed(time.Date(2026, 10, 22, 11, 14, 0, 0, time.UTC)), at, given, nil, nil, nil)[0]; ln.Kind != KindVariable || !ln.From.Given {
		t.Errorf("a typed pick: %s from %+v (%s); want a variable of the test case's slot", ln.Kind, ln.From, ln.Why)
	}
}

// The echo alone reads the test case's request: a word of a drifted value
// that the request holds is no source of it (the request is sent as
// recorded), a value read back finds no write there, and its values bind no
// variable.
func TestTheTestRequestIsTheEchosAlone(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	req := &models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "body", Recorded: `{"order":"ord-77","ref":"r-3f2b8c"}`, Sent: `{"order":"ord-78","ref":"r-3f2b8c"}`}}}
	given := []Earlier{GivenRequest(req)}
	const p = "body.ref"
	rec := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "ref-3f2b8c-ord-77"}}}
	live := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "ref-3f2b8c-ord-78"}}}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, live, rec, at, given, nil, nil, nil)[0]; ln.Kind != KindNoise {
		t.Errorf("a word the request holds, drifted with it: %s from %+v (%s); want noise: only the echo reads the request", ln.Kind, ln.From, ln.Why)
	}
	given[0].Seen[0].Written, given[0].Seen[0].Record = true, "orders/1"
	given[0].Seen[0].Place = "body.order"
	if _, ok := ReadBack(Leaf{Path: "body.order", Type: String, Value: "ord-77"}, "orders/1", given); ok {
		t.Error("a value read back from the test case's request")
	}
}

// Where the test case's request could not be read (keploy test sent none, a
// keploy older than its agent, or one of a kind keploy reads no request of),
// a value with a changed instant is refused rather than learned as noise or
// an echo: the echo could not read the request it may have been picked from.
// A value with no changed instant, and a test no test case opened (no
// request at all: keploy mock's), learn as before.
func TestAnUnreadTestRequestRefusesAChangedInstant(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const p = "body.slot"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	unsent := []Earlier{GivenRequest(nil)}
	unread := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Unread: "keploy reads no request of a test case of kind X"})}
	for name, earlier := range map[string][]Earlier{"none sent": unsent, "unread": unread} {
		for _, f := range [][2]string{{"11:14", "11:15"}, {"2026-10-22T11:14:00Z", "2026-10-22T11:15:01Z"}, {"at 1792667640", "at 1792667701"}} {
			ln := ClassifyIn(NewDriftCache(), []string{p}, shape(f[1]), shape(f[0]), at, earlier, nil, nil, nil)[0]
			if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "keploy could not read the test case's own request ("+earlier[0].Unread+")") {
				t.Errorf("%s, %q -> %q: %s (%s); want it refused for the unread request", name, f[0], f[1], ln.Kind, ln.Why)
			}
			// Echoed in its answer: refused too.
			resp := []Leaf{{Path: "body.slot", Noise: "body.slot", Type: String, Value: f[0]}}
			if ln := ClassifyIn(NewDriftCache(), []string{p}, shape(f[1]), shape(f[0]), at, earlier, nil, resp, nil)[0]; ln.Kind != KindUnaccounted {
				t.Errorf("%s, %q -> %q echoed: %s (%s); want it refused for the unread request", name, f[0], f[1], ln.Kind, ln.Why)
			}
		}
		if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("req-a7c4e2d10b3f"), shape("req-3f2b8c1e9a4d"), at, earlier, nil, nil, nil)[0]; ln.Kind != KindNoise {
			t.Errorf("%s, no instant: %s (%s); want noise", name, ln.Kind, ln.Why)
		}
		typed := func(at time.Time) Shape {
			return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, time.Millisecond)}}
		}
		rec := time.Date(2026, 10, 22, 11, 14, 17, 152e6, time.UTC)
		if ln := ClassifyIn(NewDriftCache(), []string{p}, typed(rec.Add(FullRoundLead)), typed(rec), at, earlier, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "could not read the test case's own request") {
			t.Errorf("%s, a typed clock read: %s (%s); want it refused for the unread request", name, ln.Kind, ln.Why)
		}
	}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("11:15"), shape("11:14"), at, nil, nil, nil, nil)[0]; ln.Kind != KindNoise {
		t.Errorf("no test case's request: %s (%s); want noise", ln.Kind, ln.Why)
	}
}

// The tracker gives the running test its test case's request as its earliest
// place, until the next test begins; one given with no test open is no
// test's.
func TestTrackerGivesTheRunningTestItsRequest(t *testing.T) {
	tr := NewTracker()
	tr.Give(GivenRequest(slotRequest("11:14")))
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || !earlier[0].Given || earlier[0].Unread != strayRequest || len(earlier[0].Seen) != 0 {
		t.Fatalf("given with no test open: %+v; want a request that could not be read", earlier)
	}
	// A test keploy test opens awaits its request: until it comes, one that
	// could not be read.
	tr.Begin()
	tr.Await()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || !earlier[0].Given || earlier[0].Unread != unopenedRequest {
		t.Fatalf("awaited: %+v; want a request that could not be read", earlier)
	}
	tr.End()
	tr.Await()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || earlier[0].Unread != unopenedRequest {
		t.Fatalf("awaited with no test open: %+v", earlier)
	}
	tr.Begin()
	n, _, _, _ := tr.Test()
	tr.Served(n, Earlier{Mock: "mock-1", At: time.Now(), Own: true})
	tr.Give(GivenRequest(slotRequest("11:14")))
	_, earlier, _, _ := tr.Test()
	if len(earlier) != 2 || !earlier[0].Given || earlier[0].Mock != "test case test-1" || earlier[1].Mock != "mock-1" {
		t.Fatalf("earlier %+v; want the test case's request first", earlier)
	}
	tr.End()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 2 || !earlier[0].Given {
		t.Errorf("after the test ended, a straggler's earlier %+v; want the request still", earlier)
	}
	tr.Begin()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 0 {
		t.Errorf("the next test: %+v; want nothing", earlier)
	}
	tr.Give(Earlier{Mock: "x"})
	tr.NewSet()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 0 {
		t.Errorf("a new set: %+v; want nothing", earlier)
	}
	var none *Tracker
	none.Give(GivenRequest(nil))
	none.Await()
}

// The echo reads the test case's request as keploy test sent it too (its
// template values): F's pick held there alone is followed from where the
// recorded request holds its recorded moment, or, held nowhere, refused,
// naming the sent request's slot. A typed value whose recorded moment the
// request, sent as recorded, holds, while F saw another, is refused before
// its provenance excuses it: it may be a pick from the request.
func TestTheEchoReadsTheTestRequestAsSent(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const p = "body.slot"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	req := func(recorded, sent string) []Earlier {
		return []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "body", Recorded: `{"slots":["` + recorded + `"]}`, Sent: `{"slots":["` + sent + `"]}`}}})}
	}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("11:15"), shape("11:14"), at, req("11:14", "11:15"), nil, nil, nil)[0]; ln.Kind != KindVariable || !ln.From.Given || ln.From.Path != "body.slots[0]" {
		t.Errorf("F's pick in the request as sent: %s from %+v (%s); want a variable of the request's slot", ln.Kind, ln.From, ln.Why)
	}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("11:15"), shape("11:14"), at, req("09:00", "11:15"), nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "stands in test case test-1 at body.slots[0]") {
		t.Errorf("F's pick in the request as sent only: %s (%s); want it refused, naming the request's slot", ln.Kind, ln.Why)
	}
	rec := time.Date(2026, 10, 22, 11, 14, 17, 152e6, time.UTC)
	typed := func(at time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, time.Millisecond)}}
	}
	stamp := rec.Format("2006-01-02T15:04:05.000Z")
	if ln := ClassifyIn(NewDriftCache(), []string{p}, typed(rec.Add(FullRoundLead)), typed(rec), at, req(stamp, stamp), nil, nil, nil)[0]; ln.Kind != KindUnaccounted || ln.Why != requestHoldsWhy("test case test-1 at body.slots[0]") {
		t.Errorf("a typed value whose recorded moment the request holds: %s from %+v (%s); want it refused, naming the request's slot", ln.Kind, ln.From, ln.Why)
	}
	// A typed value that is the request's instant as recorded and as sent
	// (a template value): the echo's find, a variable of the request's
	// slot, never a leaf it drifted with: nothing is bound at the request.
	sent := rec.Add(FullRoundLead).Format("2006-01-02T15:04:05.000Z")
	if ln := ClassifyIn(NewDriftCache(), []string{p}, typed(rec.Add(FullRoundLead)), typed(rec), at, req(stamp, sent), nil, nil, nil)[0]; ln.Kind != KindVariable || !ln.From.Given || !ln.From.Echo || ln.From.Path != "body.slots[0]" || !strings.Contains(ln.Why, "stands in test case test-1 at body.slots[0]") {
		t.Errorf("a typed value the request sent: %s from %+v (%s); want the echo's variable of the request's slot", ln.Kind, ln.From, ln.Why)
	}
}

// A pick from the test case's request whose pick in F the request does not
// list (none after the last slot, an empty value, the next day's opening in
// another layout) leaves the echo nothing to find: the request, sent as
// recorded, still holds the moment the value was recorded with, and the
// value is refused, naming the slot, in its window or by the lead too. With
// no request read, it is refused for the request keploy could not read. A
// slot the request holds that names both the recorded moment and F's (one
// truncated, one rounded) is no pick: a clock read at a minute stays noise.
func TestAPickTheRequestNoLongerListsIsRefused(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const p = "body.slot"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	w := &Window{Rec: Span{From: at.Add(-time.Minute), To: at.Add(time.Minute)}, F: Span{From: at.Add(-time.Minute), To: at.Add(3 * time.Minute)}}
	read := []Earlier{GivenRequest(slotRequest("11:13", "11:14"))}
	for _, live := range []string{"none", "", "2026-10-23 09:00", "11:20"} {
		for wname, w := range map[string]*Window{"no window": nil, "in its window": w} {
			ln := ClassifyIn(NewDriftCache(), []string{p}, shape(live), shape("11:14"), at, read, nil, nil, w)[0]
			if ln.Kind != KindUnaccounted || ln.Why != requestHoldsWhy("test case test-1 at body.slots[1]") {
				t.Errorf("11:14 -> %q, %s: %s (%s); want it refused, naming the request's slot", live, wname, ln.Kind, ln.Why)
			}
			ln = ClassifyIn(NewDriftCache(), []string{p}, shape(live), shape("11:14"), at, []Earlier{GivenRequest(nil)}, nil, nil, w)[0]
			if ln.Kind != KindUnaccounted || ln.Why != unreadRequestWhy(unsentRequest) {
				t.Errorf("11:14 -> %q, %s, no request sent: %s (%s); want it refused for the request", live, wname, ln.Kind, ln.Why)
			}
		}
	}
	// One slot names both moments: no pick.
	cut := []Earlier{GivenRequest(slotRequest("11:14:40.120"))}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("11:15"), shape("11:14"), at, cut, nil, nil, w)[0]; ln.Kind != KindNoise {
		t.Errorf("a minute read in its window beside a slot of both moments: %s (%s); want noise", ln.Kind, ln.Why)
	}
}
