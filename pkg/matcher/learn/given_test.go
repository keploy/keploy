package learn

import (
	"reflect"
	"strings"
	"testing"
	"time"
	// The zones' rules a row reads (America/New_York's summer time), the
	// same on every host.
	_ "time/tzdata"

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

// The test case's request is read part by part, as the HTTP adapter reads an
// exchange: its URL's path segments and query values, never what addresses
// it; a JSON body's leaves and its object keys ahead of the body whole, each
// as recorded and as sent; the names of its query parameters, headers and
// form fields after, each side its own; no header that frames it. One that
// could not be read, or that the filter did not carry, is a place that says
// why, with nothing in it.
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
	// A body of many keys reads its leaves in path order, whatever order
	// its decoding holds them in: one input, one place named.
	var keys, wantMany []string
	for i := 0; i < 24; i++ {
		k := string(rune('a'+i)) + "k"
		keys = append(keys, `"`+k+`":"v`+k+`"`)
		wantMany = append(wantMany, "body."+k)
	}
	many := GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "body", Recorded: "{" + strings.Join(keys, ",") + "}", Sent: "{" + strings.Join(keys, ",") + "}"}}})
	var gotMany []string
	for _, s := range many.Seen[:len(wantMany)] {
		gotMany = append(gotMany, s.Leaf.Path)
	}
	if strings.Join(gotMany, " ") != strings.Join(wantMany, " ") {
		t.Errorf("a body of many keys reads %v; want %v", gotMany, wantMany)
	}
	want := []string{"url.path.0=book>book",
		"body.slots[0]=11:14>11:14", "body.slots[1]=11:15>11:16", "body{keys}=slots>slots", `body={"slots":["11:14","11:15"]}>{"slots":["11:14","11:16"]}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("leaves:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Its URL's path and query, the names of its parameters on each side,
	// and none of what addresses or frames it.
	e = GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{
		{Name: "url", Recorded: "http://localhost:8080/api/v1/orders/42?page=1&tag=b&tag=a&slot=11%3A14", Sent: "http://localhost:8080/api/v1/orders/43?page=1&tag=b&tag=a&slot=11%3A15"},
		{Name: "header.Content-Length", Recorded: "57", Sent: "61"}, {Name: "header.Host", Recorded: "localhost:8080", Sent: "localhost:8080"},
		{Name: "header.Keploy-Test-Id", Recorded: "t1", Sent: "t1"}, {Name: "header.X-Day", Recorded: "mon", Sent: "mon"},
		{Name: "header.X-Only-Sent", Recorded: "", Sent: "1"},
	}})
	got = nil
	for _, s := range e.Seen {
		got = append(got, s.Leaf.Path+"="+s.Leaf.Value+">"+s.Live)
	}
	want = []string{"url.path.0=api>api", "url.path.1=v1>v1", "url.path.2=orders>orders", "url.path.3=42>43",
		"url.query.page=1>1", "url.query.slot=11:14>11:15", "url.query.tag=a, b>a, b",
		"header.X-Day=mon>mon", "header.X-Only-Sent=>1",
		"request{names}=X-Day\npage\nslot\ntag>X-Day\nX-Only-Sent\npage\nslot\ntag"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("a URL and headers:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
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

// The test case's request is an earlier place of the test as a call is: a
// value that drifted with a part keploy test sent with a template value is a
// variable of the request (nothing binds there: the adapter refuses it),
// wherever it stands in the value, never noise; nothing is read back from it.
func TestTheTestRequestIsAnEarlierPlace(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	req := &models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "body", Recorded: `{"order":"ord-77","ref":"r-3f2b8c"}`, Sent: `{"order":"ord-78","ref":"r-3f2b8c"}`}}}
	given := []Earlier{GivenRequest(req)}
	const p = "body.ref"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	for _, f := range [][2]string{{"ref-3f2b8c-ord-77", "ref-3f2b8c-ord-78"}, {"ref-ord-77", "ref-ord-78"}, {"ord-77", "ord-78"}} {
		ln := ClassifyIn(NewDriftCache(), []string{p}, shape(f[1]), shape(f[0]), at, given, nil, nil, nil)[0]
		if ln.Kind != KindVariable || !ln.From.Given || ln.From.Mock != "test case test-1" || ln.From.Path != "body.order" || ln.From.Whole != (f[0] == "ord-77") {
			t.Errorf("%q -> %q, drifted with the request's template value: %s from %+v (%s); want a variable of the request's body.order", f[0], f[1], ln.Kind, ln.From, ln.Why)
		}
	}
	// A call of the test that sent it, inside a longer value, names it
	// before the request does: what the application was given ranks as an
	// answer does.
	call := Earlier{Mock: "mock-2", At: at.Add(time.Second), Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.q", Noise: "body.q", Type: String, Value: "for ord-77 now"}, Live: "for ord-78 now", Place: "body.q"}}}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("ord-78"), shape("ord-77"), at, append(append([]Earlier(nil), given...), call), nil, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-2" || ln.From.Path != "body.q" {
		t.Errorf("sent by a call too: %s from %+v (%s); want a variable of mock-2's body.q", ln.Kind, ln.From, ln.Why)
	}
	given[0].Seen[0].Written, given[0].Seen[0].Record = true, "orders/1"
	given[0].Seen[0].Place = "body.order"
	if _, ok := ReadBack(Leaf{Path: "body.order", Type: String, Value: "ord-77"}, "orders/1", given); ok {
		t.Error("a value read back from the test case's request")
	}
}

// A value the application picks from its test case's request (by its clock,
// or otherwise) is refused wherever a word of what changed stands in the
// request, as it is when an exchange F served holds it, never learned as
// noise: a shift, a slot's id, a number, a body's key. Each reason names the
// request's leaf.
func TestAPickFromTheTestRequestThatIsNoInstantIsRefused(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const p = "body.slot"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	for _, c := range []struct{ body, rec, live, at string }{
		{`{"shifts":["morning","evening"]}`, "morning", "evening", "body.shifts[0]"},
		{`{"slots":[{"id":"s17","at":"11:14"},{"id":"s18","at":"11:15"}]}`, "s17", "s18", "body.slots[0].id"},
		{`{"slots":[{"id":17,"at":"11:14"},{"id":18,"at":"11:15"}]}`, "17", "18", "body.slots[0].id"},
		{`{"open":"2026-10-22T09:00:00Z","close":"2026-10-22T17:00:00Z"}`, "open", "closed", "body{keys}"},
	} {
		given := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "body", Recorded: c.body, Sent: c.body}}})}
		ln := ClassifyIn(NewDriftCache(), []string{p}, shape(c.live), shape(c.rec), at, given, nil, nil, nil)[0]
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "appears in what the test case gave the application, in test case test-1 at "+c.at+",") {
			t.Errorf("%q -> %q from %s: %s (%s); want it refused, naming the request's %s", c.rec, c.live, c.body, ln.Kind, ln.Why, c.at)
		}
		// Unread, it is refused for the request keploy could not read,
		// saying to run keploy record with the agent of its version, never
		// to declare the field noise.
		if ln := ClassifyIn(NewDriftCache(), []string{p}, shape(c.live), shape(c.rec), at, []Earlier{GivenRequest(nil)}, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || ln.Why != unreadRequestWhy(unsentRequest) || strings.Contains(ln.Why, "declare it noise") || !strings.Contains(ln.Why, "Run keploy record with the keploy agent of its own version") {
			t.Errorf("%q -> %q, no request sent: %s (%s); want it refused for the request", c.rec, c.live, ln.Kind, ln.Why)
		}
	}
	// Its Content-Length, HTTP's framing, is no place: a length that
	// changed with a template value moves no count with it; nor is the
	// address the application is reached at.
	framed := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{
		{Name: "url", Recorded: "http://localhost:8080/api/orders", Sent: "http://localhost:8080/api/orders"},
		{Name: "header.content-length", Recorded: "42", Sent: "43"}, {Name: "header.Host", Recorded: "localhost:8080", Sent: "localhost:8080"},
		{Name: "body", Recorded: `{"q":"a"}`, Sent: `{"q":"b"}`}}})}
	for _, f := range [][2]string{{"item-42", "item-43"}, {"port 8080", "port 8081"}} {
		if ln := ClassifyIn(NewDriftCache(), []string{p}, shape(f[1]), shape(f[0]), at, framed, nil, nil, nil)[0]; ln.Kind != KindNoise {
			t.Errorf("%q -> %q beside a Content-Length that changed alike and the app's address: %s from %+v (%s); want noise", f[0], f[1], ln.Kind, ln.From, ln.Why)
		}
	}
	// A query value is a place.
	query := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "url", Recorded: "http://h/book?shift=morning", Sent: "http://h/book?shift=morning"}}})}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("evening"), shape("morning"), at, query, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "test case test-1 at url.query.shift,") {
		t.Errorf("a query value: %s (%s); want it refused, naming url.query.shift", ln.Kind, ln.Why)
	}
	// A header's name the request sends is a place too.
	given := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "header.X-Tenant-acme7", Recorded: "1", Sent: "1"}}})}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("tenant-beta2"), shape("tenant-acme7"), at, given, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "test case test-1 at request{names}") {
		t.Errorf("a header's name: %s (%s); want it refused, naming the request's names", ln.Kind, ln.Why)
	}
}

// Where the test case's request could not be read (keploy test sent none, a
// keploy older than its agent, or one of a kind keploy reads no request of),
// every value that changed is refused rather than learned as noise or an
// echo: it may have come from the request, an instant or not, and keploy
// cannot tell. A test no test case opened (no request at all: keploy
// mock's) learns as before.
func TestAnUnreadTestRequestRefusesEveryChange(t *testing.T) {
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
		if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("req-a7c4e2d10b3f"), shape("req-3f2b8c1e9a4d"), at, earlier, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || ln.Why != unreadRequestWhy(earlier[0].Unread) {
			t.Errorf("%s, no instant: %s (%s); want it refused for the unread request", name, ln.Kind, ln.Why)
		}
		typed := func(at time.Time) Shape {
			return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, time.Millisecond)}}
		}
		rec := time.Date(2026, 10, 22, 11, 14, 17, 152e6, time.UTC)
		if ln := ClassifyIn(NewDriftCache(), []string{p}, typed(rec.Add(FullRoundLead)), typed(rec), at, earlier, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "could not read the test case's own request") {
			t.Errorf("%s, a typed clock read: %s (%s); want it refused for the unread request", name, ln.Kind, ln.Why)
		}
	}
	for _, f := range [][2]string{{"11:14", "11:15"}, {"req-3f2b8c1e9a4d", "req-a7c4e2d10b3f"}} {
		if ln := ClassifyIn(NewDriftCache(), []string{p}, shape(f[1]), shape(f[0]), at, nil, nil, nil, nil)[0]; ln.Kind != KindNoise {
			t.Errorf("%q -> %q, no test case's request: %s (%s); want noise", f[0], f[1], ln.Kind, ln.Why)
		}
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
	if ln := ClassifyIn(NewDriftCache(), []string{p}, typed(rec.Add(FullRoundLead)), typed(rec), at, req(stamp, stamp), nil, nil, nil)[0]; ln.Kind != KindUnaccounted || ln.Why != requestHoldsWhy("test case test-1 at body.slots[0]", true) {
		t.Errorf("a typed value whose recorded moment the request holds: %s from %+v (%s); want it refused, naming the request's slot", ln.Kind, ln.From, ln.Why)
	}
	// A typed value that is the request's instant as recorded and as sent
	// (a template value): it drifted with the request's slot, a variable of
	// it, whole, found as a leaf that drifted with it before the echo reads
	// it; nothing is bound at the request (the adapter refuses it).
	sent := rec.Add(FullRoundLead).Format("2006-01-02T15:04:05.000Z")
	if ln := ClassifyIn(NewDriftCache(), []string{p}, typed(rec.Add(FullRoundLead)), typed(rec), at, req(stamp, sent), nil, nil, nil)[0]; ln.Kind != KindVariable || !ln.From.Given || !ln.From.Whole || ln.From.Echo || ln.From.Path != "body.slots[0]" {
		t.Errorf("a typed value the request sent: %s from %+v (%s); want a variable of the request's slot", ln.Kind, ln.From, ln.Why)
	}
}

// A pick from the test case's request whose pick in F the request does not
// list (none after the last slot, an empty value, the next day's opening in
// another layout) leaves the echo nothing to find: the request, sent as
// recorded, still holds the moment the value was recorded with, and the
// value is refused, naming the slot, in its window or by the lead too. With
// no request read, it is refused for the request keploy could not read. A
// slot that names the recorded moment by one rule (truncated) and F's only
// by another (rounded) does not give F's: the application cuts by one rule,
// so the recorded value may be the slot's pick, and it is refused too.
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
			if ln.Kind != KindUnaccounted || ln.Why != requestHoldsWhy("test case test-1 at body.slots[1]", true) {
				t.Errorf("11:14 -> %q, %s: %s (%s); want it refused, naming the request's slot", live, wname, ln.Kind, ln.Why)
			}
			ln = ClassifyIn(NewDriftCache(), []string{p}, shape(live), shape("11:14"), at, []Earlier{GivenRequest(nil)}, nil, nil, w)[0]
			if ln.Kind != KindUnaccounted || ln.Why != unreadRequestWhy(unsentRequest) {
				t.Errorf("11:14 -> %q, %s, no request sent: %s (%s); want it refused for the request", live, wname, ln.Kind, ln.Why)
			}
		}
	}
	// One slot names both moments, by two rules.
	cut := []Earlier{GivenRequest(slotRequest("11:14:40.120"))}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("11:15"), shape("11:14"), at, cut, nil, nil, w)[0]; ln.Kind != KindUnaccounted || ln.Why != requestHoldsWhy("test case test-1 at body.slots[0]", true) {
		t.Errorf("a minute read in its window beside a slot of both moments: %s (%s); want it refused, naming the slot", ln.Kind, ln.Why)
	}
	// Where the request as sent gives F's moment by the rule that gives the
	// recorded one, it explains F's value: no pick the request no longer
	// lists (the echo's to read).
	both := []Earlier{GivenRequest(slotRequest("11:14:20.120", "11:15:20.120"))}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("11:15"), shape("11:14"), at, both, nil, nil, w)[0]; ln.Kind == KindNoise || strings.HasPrefix(ln.Why, "the moment it was recorded with stands in test case test-1 at body.slots[0], which") {
		t.Errorf("a slot for each moment, by one rule: %s (%s); want the echo's, not a pick the request no longer lists", ln.Kind, ln.Why)
	}
	// The part keploy test sent with template values: the request as
	// recorded held the moment, the reason says how keploy test sent it,
	// and the request as sent is not what decides (it holds neither).
	templated := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "body",
		Recorded: `{"slots":["2026-10-22T11:13:00Z","2026-10-22T11:14:00Z"]}`, Sent: `{"slots":["2026-10-22T11:20:00Z","2026-10-22T11:21:00Z"]}`}}})}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, shape("none"), shape("11:14"), at, templated, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || ln.Why != requestHoldsWhy("test case test-1 at body.slots[1]", false) {
		t.Errorf("a pick from a part sent with template values: %s (%s); want it refused, naming the slot as recorded", ln.Kind, ln.Why)
	}
	if !strings.Contains(requestHoldsWhy("x", false), "which keploy test sent with template values in it") || !strings.Contains(requestHoldsWhy("x", true), "which keploy test sent as recorded") {
		t.Error("the reason does not say how keploy test sent the request's part")
	}
}

// clientTestCache is a DriftCache of one of keploy mock's client tests, as
// its tracker makes one (Tracker.SetClientTests), of an application whose
// zone is UTC.
func clientTestCache() *DriftCache { return clientTestCacheIn(nil) }

// clientTestCacheIn is clientTestCache of an application that writes a wall
// clock of no zone in zone, beside UTC (Tracker.SetAppZone).
func clientTestCacheIn(zone *time.Location) *DriftCache {
	dc := NewDriftCache()
	dc.clientTests, dc.zone = true, zone
	return dc
}

// ist is the zone of an application that runs in India, 5:30 ahead of UTC.
var ist = time.FixedZone("IST", 5*3600+1800)

// clientTestRefusal reports whether why is a client test's refusal of a
// value whose instant at part (a place in the value, "" for a typed leaf)
// changed: in its call's window (windowed), no clock read by the app
// matched it, or, where the call had none, keploy saw no read of the app's
// clock for it; with how to correct it, and no value quoted (values).
func clientTestRefusal(why, part string, windowed bool, values ...string) bool {
	which := ""
	if part != "" {
		which = " (an instant, " + part + ")"
	}
	head := "no clock read by the app matched it" + which + ": "
	if !windowed {
		head = "keploy saw no read of the app's clock for its call" + which + ": "
	}
	if !strings.HasPrefix(why, head) || !strings.Contains(why, "If your test supplies this value, have it supply the same value in every run") || strings.Contains(why, "template variable") {
		return false
	}
	for _, v := range values {
		if strings.Contains(why, v) {
			return false
		}
	}
	return true
}

// In keploy mock's client tests (a Playwright or CLI test: the harness gives
// the application its inputs, and keploy has no request place) a changed
// instant is learned as noise, or an echo, only where its call's window on
// the application's clock shows it a read of that clock in both runs: the
// application's own stamp, whole, inside a longer value, as an epoch, as a
// wall clock of its zone, typed. The lead alone shows no read there: a value
// the harness gave the application that moved by about the lead ("11:14" ->
// "11:15", no window) is refused, saying no clock read by the app matched it
// and how to correct it, as is one outside the window, a time of day (it
// names no day), a wall clock only one run's span holds in any zone, and a
// wall clock of no zone that only a zone other than UTC and the
// application's reads in the window (the harness's "now plus two hours"). A
// value with no instant is read as in any test. In keploy test (no client
// test) each of these refused values is noise by the lead or as a fresh
// value, as before. A refusal where the call had no window says keploy saw
// no read of the app's clock for it.
func TestAClientTestLearnsAChangedInstantOnlyWhereItsWindowShowsARead(t *testing.T) {
	rec := time.Date(2026, 10, 22, 11, 14, 16, 900_000_000, time.UTC) // the recorded call
	w := &Window{
		Rec: Span{From: rec.Add(-900 * time.Millisecond), To: rec},
		F:   Span{From: rec.Add(60_500 * time.Millisecond), To: rec.Add(61_400 * time.Millisecond)},
	}
	const p = "body.at"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	for _, tc := range []struct {
		name      string
		rec, live string
		w         *Window
		zone      *time.Location // the application's (nil: UTC)
		noise     bool           // in the client test
		part      string
	}{
		{"the app's stamp", "2026-10-22T11:14:16.473Z", "2026-10-22T11:15:17.950Z", w, nil, true, ""},
		{"the app's stamp inside a longer value", "sent at 2026-10-22T11:14:16.473Z", "sent at 2026-10-22T11:15:17.950Z", w, nil, true, ""},
		{"the app's stamp as an epoch", "1792667656473", "1792667717950", w, nil, true, ""},
		{"the app's stamp as its zone's wall clock", "2026-10-22 16:44:16", "2026-10-22 16:45:17", w, ist, true, ""},
		{"the app's stamp as UTC's wall clock, the app in another zone", "2026-10-22 11:14:16", "2026-10-22 11:15:17", w, ist, true, ""},
		{"the app's stamp as its zone's wall clock, the app's zone not known", "2026-10-22 16:44:16", "2026-10-22 16:45:17", w, nil, false, "its characters 1 to 19"},
		{"the app's stamp, no window", "2026-10-22T11:14:16.473Z", "2026-10-22T11:15:17.950Z", nil, nil, false, "its characters 1 to 24"},
		{"a slot the harness gave, a minute on", "11:14", "11:15", nil, nil, false, "its characters 1 to 5"},
		{"a slot the harness gave, in a window", "11:14:16", "11:15:17", w, nil, false, "its characters 1 to 8"},
		{"a time the harness gave, by the lead, outside the window", "book 2026-10-22T11:20Z", "book 2026-10-22T11:21Z", w, nil, false, "its characters 6 to 22"},
		{"a wall clock F's span holds in no zone the recording's does", "2026-10-22 16:44:16", "2026-10-22 16:45:30", w, ist, false, "its characters 1 to 19"},
		// The harness's now plus some hours or minutes, written with no zone:
		// a wall clock of some zone that is neither UTC nor the app's.
		{"the harness's now plus two hours, no zone", "2026-10-22 13:14:16", "2026-10-22 13:15:17", w, nil, false, "its characters 1 to 19"},
		{"the harness's now plus two hours, no zone, the app in India", "2026-10-22 13:14:16", "2026-10-22 13:15:17", w, ist, false, "its characters 1 to 19"},
		{"the harness's now plus 30 minutes, no zone", "2026-10-22T11:44:16.473", "2026-10-22T11:45:17.950", w, ist, false, "its characters 1 to 23"},
		{"the harness's now plus 45 minutes, no zone", "2026-10-22 11:59:16", "2026-10-22 12:00:17", w, ist, false, "its characters 1 to 19"},
		{"the harness's now less 5 hours, no zone", "2026-10-22 06:14:16", "2026-10-22 06:15:17", w, ist, false, "its characters 1 to 19"},
		{"the harness's now plus 14 hours, no zone", "2026-10-23 01:14:16", "2026-10-23 01:15:17", w, ist, false, "its characters 1 to 19"},
		{"the harness's now plus two hours, zoned", "2026-10-22T13:14:16Z", "2026-10-22T13:15:17Z", w, ist, false, "its characters 1 to 20"},
		{"a value with no instant", "a7f3c2", "9c21e0", nil, nil, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := ClassifyIn(clientTestCacheIn(tc.zone), []string{p}, str(tc.live), str(tc.rec), rec, nil, nil, nil, tc.w)[0]
			if tc.noise {
				if ln.Kind != KindNoise {
					t.Fatalf("%q -> %q: %s (%s); want noise", tc.rec, tc.live, ln.Kind, ln.Why)
				}
			} else if ln.Kind != KindUnaccounted || !clientTestRefusal(ln.Why, tc.part, tc.w != nil, tc.rec, tc.live) {
				t.Fatalf("%q -> %q: %s (%s); want it refused, naming %s, saying keploy saw no read of the app's clock that matched it, and quoting no value", tc.rec, tc.live, ln.Kind, ln.Why, tc.part)
			}
			if ln := ClassifyIn(NewDriftCache(), []string{p}, str(tc.live), str(tc.rec), rec, nil, nil, nil, nil)[0]; ln.Kind != KindNoise {
				t.Fatalf("keploy test, %q -> %q: %s (%s); want noise", tc.rec, tc.live, ln.Kind, ln.Why)
			}
		})
	}
	// An echo of the stamp in the call's own answer, only in the window.
	resp := []Leaf{{Path: "body.received", Noise: "body.received", Type: String, Value: "2026-10-22T11:14:16.473Z"}}
	if ln := ClassifyIn(clientTestCache(), []string{p}, str("2026-10-22T11:15:17.950Z"), str("2026-10-22T11:14:16.473Z"), rec, nil, nil, resp, w)[0]; ln.Kind != KindEcho {
		t.Errorf("an echoed stamp in its window: %s (%s); want an echo", ln.Kind, ln.Why)
	}
	if ln := ClassifyIn(clientTestCache(), []string{p}, str("2026-10-22T11:15:17.950Z"), str("2026-10-22T11:14:16.473Z"), rec, nil, nil, resp, nil)[0]; ln.Kind != KindUnaccounted {
		t.Errorf("an echoed stamp, no window: %s; want it refused", ln.Kind)
	}
	// Typed: the window, never the lead.
	typed := func(at time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, time.Millisecond)}}
	}
	tr, tl := rec.Add(-427*time.Millisecond), rec.Add(61_050*time.Millisecond)
	if ln := ClassifyIn(clientTestCache(), []string{p}, typed(tl), typed(tr), rec, nil, nil, nil, w)[0]; ln.Kind != KindNoise {
		t.Errorf("a typed stamp in its window: %s (%s); want noise", ln.Kind, ln.Why)
	}
	for _, c := range []struct {
		name string
		w    *Window
		l    time.Time
	}{{"no window", nil, tl}, {"by the lead, outside the window", w, tl.Add(5 * time.Second)}} {
		ln := ClassifyIn(clientTestCache(), []string{p}, typed(c.l), typed(tr), rec, nil, nil, nil, c.w)[0]
		if ln.Kind != KindUnaccounted || !clientTestRefusal(ln.Why, "", c.w != nil) {
			t.Errorf("a typed stamp, %s: %s (%s); want it refused, saying keploy saw no read of the app's clock that matched it", c.name, ln.Kind, ln.Why)
		}
		if c.w == nil {
			if ln := ClassifyIn(NewDriftCache(), []string{p}, typed(c.l), typed(tr), rec, nil, nil, nil, nil)[0]; ln.Kind != KindNoise {
				t.Errorf("keploy test, a typed stamp the lead moved: %s (%s); want noise", ln.Kind, ln.Why)
			}
		}
	}
}

// In a client test the lead makes no clock read of a value either where it
// drifted with an earlier exchange's instant: the slot the harness gave the
// application, sent by an earlier call and again inside a longer value, is
// that call's variable (followed, compared), not two clock reads of one
// moment, which in keploy test it is (noise).
func TestAClientTestFollowsAnInstantTheLeadMoved(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 14, 16, 900_000_000, time.UTC)
	const p = "body.note"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	call := []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.slot", Noise: "body.slot", Type: String, Value: "2026-10-22T11:20Z"}, Live: "2026-10-22T11:21Z", Place: "body.slot"}}}}
	if ln := ClassifyIn(clientTestCache(), []string{p}, str("book 2026-10-22T11:21Z"), str("book 2026-10-22T11:20Z"), at, call, nil, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Path != "body.slot" {
		t.Errorf("a client test: %s from %+v (%s); want a variable of mock-1's body.slot", ln.Kind, ln.From, ln.Why)
	}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, str("book 2026-10-22T11:21Z"), str("book 2026-10-22T11:20Z"), at, call, nil, nil, nil)[0]; ln.Kind != KindNoise {
		t.Errorf("keploy test: %s from %+v (%s); want noise, two clock reads the lead moved", ln.Kind, ln.From, ln.Why)
	}
}

// A client test's cache gives a value the lead moved no clock evidence
// (DriftCache.ClockReads), and reads a typed instant as a clock read by its
// call's window alone (DriftCache.InstantClockRead); keploy test's, and a
// nil cache, read both by the lead as before.
func TestAClientTestsCacheGivesNoClockEvidenceByTheLead(t *testing.T) {
	const rec, live = "2026-10-22T11:20Z", "2026-10-22T11:21Z"
	if got := clientTestCache().ClockReads(rec, live); got != nil {
		t.Errorf("a client test's clock evidence %v; want none", got)
	}
	want := ClockReads(rec, live)
	if len(want) == 0 {
		t.Fatalf("ClockReads(%q, %q) is empty: no row", rec, live)
	}
	var none *DriftCache
	for name, dc := range map[string]*DriftCache{"keploy test": NewDriftCache(), "no cache": none} {
		if got := dc.ClockReads(rec, live); !reflect.DeepEqual(got, want) {
			t.Errorf("%s's clock evidence %v; want %v", name, got, want)
		}
	}
	at := time.Date(2026, 10, 22, 11, 14, 16, 473_000_000, time.UTC)
	r, l := InstantLeaf("p", "p", at, time.Millisecond), InstantLeaf("p", "p", at.Add(FullRoundLead), time.Millisecond)
	if clientTestCache().InstantClockRead(r, l, nil) {
		t.Error("a client test reads a typed instant the lead moved as a clock read")
	}
	if !NewDriftCache().InstantClockRead(r, l, nil) || !none.InstantClockRead(r, l, nil) || !InstantClockRead(r, l, nil) {
		t.Error("keploy test does not read a typed instant the lead moved as a clock read")
	}
	w := &Window{Rec: Span{From: at.Add(-time.Second), To: at}, F: Span{From: at.Add(FullRoundLead - time.Second), To: at.Add(FullRoundLead)}}
	if !clientTestCache().InstantClockRead(r, l, w) {
		t.Error("a client test does not read a typed instant in its window as a clock read")
	}
}

// holdsRead reads an instant at its own resolution in each run's span; a
// wall clock of no zone as UTC's or the application's zone's in both runs
// (summer time too), never another zone's, nor one zone's in one run and
// another's in the other; a zoned instant in its own zone only; a time of
// day in no span.
func TestWindowHoldsRead(t *testing.T) {
	rec := time.Date(2026, 10, 22, 11, 14, 16, 900_000_000, time.UTC)
	w := &Window{
		Rec: Span{From: rec.Add(-900 * time.Millisecond), To: rec},
		F:   Span{From: rec.Add(60_500 * time.Millisecond), To: rec.Add(61_400 * time.Millisecond)},
	}
	read := func(s string) moment {
		sp := instantSpans(s)
		if len(sp) != 1 || !sp[0].keyed {
			t.Fatalf("%q holds %+v", s, sp)
		}
		return sp[0].moment()
	}
	ny, err := time.LoadLocation("America/New_York") // summer time on 2026-10-22: 4 hours behind
	if err != nil {
		t.Fatal(err)
	}
	minus5 := time.FixedZone("", -5*3600)
	for _, c := range []struct {
		rec, live string
		zone      *time.Location
		want      bool
	}{
		{"2026-10-22T11:14:16.473Z", "2026-10-22T11:15:17.950Z", nil, true},
		{"2026-10-22T11:14:16Z", "2026-10-22T11:15:17Z", nil, true},
		{"2026-10-22T11:14Z", "2026-10-22T11:15Z", nil, true},
		{"2026-10-22T11:14:15.999Z", "2026-10-22T11:15:17.950Z", nil, false},
		{"2026-10-22T11:14:16.473Z", "2026-10-22T11:15:18.401Z", nil, false},
		{"2026-10-22 11:14:16", "2026-10-22 11:15:17", nil, true},
		{"2026-10-22 11:14:16", "2026-10-22 11:15:17", ist, true},
		{"2026-10-22 16:44:16", "2026-10-22 16:45:17", ist, true},
		{"2026-10-22 16:44:16", "2026-10-22 16:45:17", nil, false},
		{"2026-10-22 16:44:16", "2026-10-22 16:45:17", minus5, false},
		{"2026-10-22 06:14:16", "2026-10-22 06:15:17", minus5, true},
		{"2026-10-22 06:14:16", "2026-10-22 06:15:17", ist, false},
		{"2026-10-22 07:14:16", "2026-10-22 07:15:17", ny, true},
		{"2026-10-22 06:14:16", "2026-10-22 06:15:17", ny, false},
		{"2026-10-22 13:14:16", "2026-10-22 13:15:17", ist, false},
		{"2026-10-22 13:14:16", "2026-10-22 13:15:17", time.FixedZone("", 2*3600), true},
		{"2026-10-22 16:44:16", "2026-10-22 06:15:17", ist, false},
		{"2026-10-22 16:44:16", "2026-10-22 11:15:17", ist, false},
		{"2026-10-22T16:44:16+05:30", "2026-10-22T16:45:17+05:30", nil, true},
		{"2026-10-22T16:44:16+05:00", "2026-10-22T16:45:17+05:00", ist, false},
		{"2026-10-22T16:44:16Z", "2026-10-22T16:45:17Z", ist, false},
		{"2026-10-22T16:44:16Z", "2026-10-22 16:45:17", ist, false},
		{"11:14:16", "11:15:17", nil, false},
	} {
		if got := w.holdsRead(read(c.rec), read(c.live), c.zone); got != c.want {
			t.Errorf("%q, %q in %v: %v; want %v", c.rec, c.live, c.zone, got, c.want)
		}
	}
	var none *Window
	if none.holdsRead(read("2026-10-22T11:14:16.473Z"), read("2026-10-22T11:15:17.950Z"), nil) {
		t.Error("no window holds a read")
	}
}
