package learn

import (
	"context"
	"encoding/base64"
	"reflect"
	"slices"
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
	many := GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "header.Content-Type", Recorded: "application/json", Sent: "application/json"}, {Name: "body", Recorded: "{" + strings.Join(keys, ",") + "}", Sent: "{" + strings.Join(keys, ",") + "}"}}})
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
		// learn step did before it read the request), and of a whole
		// instant, with no call window, a read of the clock a replay checks.
		// Written to the second, the pair is taken off the whole minute
		// (:07), where the learn step would learn it at the minute's cut.
		off := func(v string) Shape {
			if strings.HasSuffix(v, ":00Z") {
				v = strings.TrimSuffix(v, ":00Z") + ":07Z"
			}
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
		}
		if ln := ClassifyIn(clockedDrifts(), []string{p}, off(f[1]), off(f[0]), at, nil, nil, nil, nil)[0]; !noiseIn(ln, nil) || ln.Kind == KindUnaccounted {
			t.Errorf("%q -> %q (off the whole minute), no test case's request: %s Read=%+v (%s); want noise, and of a whole instant a read of the clock", f[0], f[1], ln.Kind, ln.Read, ln.Why)
		}
		for _, slots := range [][]string{{"11:13", "11:14", "11:15", "11:16"}, {"2026-10-22T11:14:00Z", "2026-10-22T11:15:00Z"}, {"2026-10-22 11:14", "2026-10-22 11:15"}} {
			given := []Earlier{GivenRequest(slotRequest(slots...))}
			ln := ClassifyIn(clockedDrifts(), []string{p}, live, rec, at, given, nil, nil, nil)[0]
			if ln.Kind != KindVariable || !ln.From.Given || !ln.From.Echo || ln.From.Mock != "test case test-1" || !strings.HasPrefix(ln.From.Path, "body.slots[") || !strings.Contains(ln.Why, "stands in test case test-1 at body.slots[") {
				t.Errorf("%q -> %q, slots %v: %s from %+v (%s); want a variable of the test case's slot", f[0], f[1], slots, ln.Kind, ln.From, ln.Why)
			}
		}
		// The request lists F's pick only.
		given := []Earlier{GivenRequest(slotRequest("11:15", "11:16"))}
		if ln := ClassifyIn(clockedDrifts(), []string{p}, live, rec, at, given, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "stands in test case test-1 at body.slots[0]") {
			t.Errorf("%q -> %q, F's pick listed only: %s (%s); want it refused, naming the test case's slot", f[0], f[1], ln.Kind, ln.Why)
		}
	}
	// A typed pick too.
	typed := func(at time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, time.Second)}}
	}
	given := []Earlier{GivenRequest(slotRequest("11:14", "11:15"))}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, typed(time.Date(2026, 10, 22, 11, 15, 0, 0, time.UTC)), typed(time.Date(2026, 10, 22, 11, 14, 0, 0, time.UTC)), at, given, nil, nil, nil)[0]; ln.Kind != KindVariable || !ln.From.Given {
		t.Errorf("a typed pick: %s from %+v (%s); want a variable of the test case's slot", ln.Kind, ln.From, ln.Why)
	}
}

// The test case's request is an earlier place of the test as a call is: a
// value that drifted with a part keploy test sent with a template value is a
// variable of the request (nothing binds there: the adapter refuses it),
// wherever it stands in the value, never noise; nothing is read back from it.
func TestTheTestRequestIsAnEarlierPlace(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	req := &models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{jsonType, {Name: "body", Recorded: `{"order":"ord-77","ref":"r-3f2b8c"}`, Sent: `{"order":"ord-78","ref":"r-3f2b8c"}`}}}
	given := []Earlier{GivenRequest(req)}
	const p = "body.ref"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	for _, f := range [][2]string{{"ref-3f2b8c-ord-77", "ref-3f2b8c-ord-78"}, {"ref-ord-77", "ref-ord-78"}, {"ord-77", "ord-78"}} {
		ln := ClassifyIn(clockedDrifts(), []string{p}, shape(f[1]), shape(f[0]), at, given, nil, nil, nil)[0]
		if ln.Kind != KindVariable || !ln.From.Given || ln.From.Mock != "test case test-1" || ln.From.Path != "body.order" || ln.From.Whole != (f[0] == "ord-77") {
			t.Errorf("%q -> %q, drifted with the request's template value: %s from %+v (%s); want a variable of the request's body.order", f[0], f[1], ln.Kind, ln.From, ln.Why)
		}
	}
	// A call of the test that sent it, inside a longer value, names it
	// before the request does: what the application was given ranks as an
	// answer does.
	call := Earlier{Mock: "mock-2", At: at.Add(-time.Second), Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.q", Noise: "body.q", Type: String, Value: "for ord-77 now"}, Live: "for ord-78 now", Place: "body.q"}}}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("ord-78"), shape("ord-77"), at, append(append([]Earlier(nil), given...), call), nil, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-2" || ln.From.Path != "body.q" {
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
		given := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{jsonType, {Name: "body", Recorded: c.body, Sent: c.body}}})}
		ln := ClassifyIn(clockedDrifts(), []string{p}, shape(c.live), shape(c.rec), at, given, nil, nil, nil)[0]
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "appears in what the test case gave the application, in test case test-1 at "+c.at+",") {
			t.Errorf("%q -> %q from %s: %s (%s); want it refused, naming the request's %s", c.rec, c.live, c.body, ln.Kind, ln.Why, c.at)
		}
		// Unread, it is refused for the request keploy could not read,
		// saying to run keploy record with the agent of its version, never
		// to declare the field noise.
		if ln := ClassifyIn(clockedDrifts(), []string{p}, shape(c.live), shape(c.rec), at, []Earlier{GivenRequest(nil)}, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || ln.Why != unreadRequestWhy(unsentRequest) || strings.Contains(ln.Why, "declare it noise") || !strings.Contains(ln.Why, "Run keploy record with the keploy agent of its own version") {
			t.Errorf("%q -> %q, no request sent: %s (%s); want it refused for the request", c.rec, c.live, ln.Kind, ln.Why)
		}
	}
	// Its Content-Length, HTTP's framing, is no place: a length that
	// changed with a template value moves no count with it; nor is the
	// address the application is reached at.
	framed := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{
		{Name: "url", Recorded: "http://localhost:8080/api/orders", Sent: "http://localhost:8080/api/orders"},
		{Name: "header.content-length", Recorded: "42", Sent: "43"}, {Name: "header.Host", Recorded: "localhost:8080", Sent: "localhost:8080"},
		jsonType, {Name: "body", Recorded: `{"q":"a"}`, Sent: `{"q":"b"}`}}})}
	for _, f := range [][2]string{{"item-42", "item-43"}, {"port 8080", "port 8081"}} {
		if ln := ClassifyIn(clockedDrifts(), []string{p}, shape(f[1]), shape(f[0]), at, framed, nil, nil, nil)[0]; ln.Kind != KindNoise {
			t.Errorf("%q -> %q beside a Content-Length that changed alike and the app's address: %s from %+v (%s); want noise", f[0], f[1], ln.Kind, ln.From, ln.Why)
		}
	}
	// A query value is a place.
	query := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "url", Recorded: "http://h/book?shift=morning", Sent: "http://h/book?shift=morning"}}})}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("evening"), shape("morning"), at, query, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "test case test-1 at url.query.shift,") {
		t.Errorf("a query value: %s (%s); want it refused, naming url.query.shift", ln.Kind, ln.Why)
	}
	// A header's name the request sends is a place too.
	given := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "header.X-Tenant-acme7", Recorded: "1", Sent: "1"}}})}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("tenant-beta2"), shape("tenant-acme7"), at, given, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "test case test-1 at request{names}") {
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
			ln := ClassifyIn(clockedDrifts(), []string{p}, shape(f[1]), shape(f[0]), at, earlier, nil, nil, nil)[0]
			if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "keploy could not read the test case's own request ("+earlier[0].Unread+")") {
				t.Errorf("%s, %q -> %q: %s (%s); want it refused for the unread request", name, f[0], f[1], ln.Kind, ln.Why)
			}
			// Echoed in its answer: refused too.
			resp := []Leaf{{Path: "body.slot", Noise: "body.slot", Type: String, Value: f[0]}}
			if ln := ClassifyIn(clockedDrifts(), []string{p}, shape(f[1]), shape(f[0]), at, earlier, nil, resp, nil)[0]; ln.Kind != KindUnaccounted {
				t.Errorf("%s, %q -> %q echoed: %s (%s); want it refused for the unread request", name, f[0], f[1], ln.Kind, ln.Why)
			}
		}
		if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("req-a7c4e2d10b3f"), shape("req-3f2b8c1e9a4d"), at, earlier, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || ln.Why != unreadRequestWhy(earlier[0].Unread) {
			t.Errorf("%s, no instant: %s (%s); want it refused for the unread request", name, ln.Kind, ln.Why)
		}
		typed := func(at time.Time) Shape {
			return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, time.Millisecond)}}
		}
		// Read before its call, at: a clock read the lead moved.
		rec := time.Date(2026, 10, 22, 11, 13, 39, 152e6, time.UTC)
		if ln := ClassifyIn(clockedDrifts(), []string{p}, typed(rec.Add(FullRoundLead)), typed(rec), at, earlier, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "could not read the test case's own request") {
			t.Errorf("%s, a typed clock read: %s (%s); want it refused for the unread request", name, ln.Kind, ln.Why)
		}
	}
	for _, f := range [][2]string{{"11:14", "11:15"}, {"req-3f2b8c1e9a4d", "req-a7c4e2d10b3f"}} {
		if ln := ClassifyIn(clockedDrifts(), []string{p}, shape(f[1]), shape(f[0]), at, nil, nil, nil, nil)[0]; !noiseIn(ln, nil) {
			t.Errorf("%q -> %q, no test case's request: %s (%s); want noise, and of a time of day a read of the clock modulo a day", f[0], f[1], ln.Kind, ln.Why)
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
	// A test keploy test opens awaits its request, and says so to a call
	// that reads its places (TestACallInTheGapWaitsForTheRequest waits): one
	// that ends without it leaves a request that could not be read.
	tr.BeginAwaiting()
	if _, earlier, _, _, awaiting := tr.Places(); !awaiting || len(earlier) != 1 || earlier[0].Unread != unopenedRequest {
		t.Fatalf("awaiting: %+v, %v; want a request that could not be read, awaited", earlier, awaiting)
	}
	tr.End()
	if _, _, _, _, awaiting := tr.Places(); awaiting {
		t.Fatal("a test that ended still awaits its request")
	}
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || !earlier[0].Given || earlier[0].Unread != unopenedRequest {
		t.Fatalf("ended awaiting: %+v; want a request that could not be read", earlier)
	}
	// Await marks no test that is not open, nor one given its request.
	tr.Await()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || earlier[0].Unread != unopenedRequest {
		t.Fatalf("awaited with no test open: %+v", earlier)
	}
	tr.Begin()
	tr.Give(GivenRequest(slotRequest("11:14")))
	tr.Await()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || earlier[0].Unread != "" || earlier[0].Mock != "test case test-1" {
		t.Fatalf("awaited after its request came: %+v; want the request", earlier)
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
	none.BeginAwaiting()
	none.OnAwait(nil)
	if err := none.AwaitRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A call that reads a test's places after keploy test opened the test's
// window and before the filter that opens it brought its test case's request
// (the gap between the tracker's BeginAwaiting and Give) waits for the
// request, and is learned from as a call made after it: the verdict on a
// background call's drift does not hang on where the scheduler put it. A
// pool call's nonce that drifted is noise either way; one the request holds
// a word of is refused, naming the request, either way. A test whose window
// closes without the request (End) refuses it, saying the filter never
// brought it; a call whose own end comes first learns nothing. Each order is
// forced through the tracker's OnAwait, through no clock, 200 times.
func TestACallInTheGapWaitsForTheRequest(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const p = "body.nonce"
	rec := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "n-5f2a91"}}}
	live := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "n-77c0d3"}}}
	request := func(q string) *models.TestRequest {
		return &models.TestRequest{Test: "test-2", Parts: []models.TestRequestPart{jsonType, {Name: "body", Recorded: `{"q":"` + q + `"}`, Sent: `{"q":"` + q + `"}`}}}
	}
	// verdict is the call's, as the HTTP adapter reads it: the test's places,
	// and, where the test still awaits its request, the request awaited and
	// the places read again.
	verdict := func(tr *Tracker) (Learned, error) {
		for {
			_, earlier, _, _, awaiting := tr.Places()
			if !awaiting {
				return ClassifyIn(clockedDrifts(), []string{p}, live, rec, at, earlier, nil, nil, nil)[0], nil
			}
			if err := tr.AwaitRequest(context.Background()); err != nil {
				return Learned{}, err
			}
		}
	}
	for _, row := range []struct {
		name string
		req  *models.TestRequest
		want func(Learned) bool
	}{
		{"a request that holds nothing of it", request("books"), func(ln Learned) bool { return ln.Kind == KindNoise }},
		{"a request that holds its word", request("n-5f2a91"), func(ln Learned) bool {
			return ln.Kind != KindNoise && ln.Kind != KindEcho && (ln.From.Given || strings.Contains(ln.Why, "test case test-2"))
		}},
	} {
		for i := 0; i < 200; i++ {
			// The request first: the filter answered before the call.
			tr := NewTracker()
			tr.BeginAwaiting()
			tr.Give(GivenRequest(row.req))
			// A Give that left the test awaiting fails here, not at the
			// test's timeout.
			if _, _, _, _, still := tr.Places(); still {
				t.Fatalf("%s: the request was given, and the test still awaits it", row.name)
			}
			after, err := verdict(tr)
			if err != nil || !row.want(after) {
				t.Fatalf("%s, after the request: %s (%s), %v", row.name, after.Kind, after.Why, err)
			}
			// The call first, waiting in the gap: the request after it.
			tr = NewTracker()
			waiting := make(chan struct{}, 1)
			tr.OnAwait(func() {
				select {
				case waiting <- struct{}{}:
				default:
				}
			})
			tr.BeginAwaiting()
			got := make(chan Learned, 1)
			go func() {
				ln, _ := verdict(tr)
				got <- ln
			}()
			var before Learned
			select {
			case <-waiting:
				tr.Give(GivenRequest(row.req))
				// Read at once, not by waiting on the call: a Give that
				// left it waiting fails here, not at the test's timeout.
				if _, _, _, _, still := tr.Places(); still {
					t.Fatalf("%s: the request was given, and the test still awaits it", row.name)
				}
				before = <-got
			case before = <-got:
				// Decided without waiting for the request.
				tr.Give(GivenRequest(row.req))
			}
			if before.Kind != after.Kind || before.Why != after.Why {
				t.Fatalf("%s: in the gap %s (%s), after the request %s (%s): two verdicts by where the call fell", row.name, before.Kind, before.Why, after.Kind, after.Why)
			}
		}
	}
	// The window closes without the request: refused, saying so.
	for i := 0; i < 200; i++ {
		tr := NewTracker()
		waiting := make(chan struct{}, 1)
		tr.OnAwait(func() {
			select {
			case waiting <- struct{}{}:
			default:
			}
		})
		tr.BeginAwaiting()
		got := make(chan Learned, 1)
		go func() {
			ln, _ := verdict(tr)
			got <- ln
		}()
		select {
		case <-waiting:
		case ln := <-got:
			t.Fatalf("decided before the window closed: %s (%s)", ln.Kind, ln.Why)
		}
		tr.End()
		if _, _, _, _, still := tr.Places(); still {
			t.Fatalf("the window closed, and its test still awaits its request")
		}
		if ln := <-got; ln.Kind != KindUnaccounted || ln.Why != unreadRequestWhy(unopenedRequest) {
			t.Fatalf("a window closed without its request: %s (%s); want it refused, the filter never having brought it", ln.Kind, ln.Why)
		}
	}
	// The window closes as the next test begins, one keploy test did not
	// open, or as the next set begins: a call that waited reads what then
	// stands, and no test awaits a request.
	for _, next := range []struct {
		name  string
		close func(*Tracker)
		after func(*Tracker) uint64
	}{
		{"the next test begins awaiting nothing", func(tr *Tracker) { tr.Begin() }, func(tr *Tracker) uint64 { n, _, _, _ := tr.Test(); return n }},
		{"the next set begins", func(tr *Tracker) { tr.NewSet() }, func(tr *Tracker) uint64 { n, _, _, _ := tr.Test(); return n }},
	} {
		tr := NewTracker()
		waiting := make(chan struct{}, 1)
		tr.OnAwait(func() { waiting <- struct{}{} })
		tr.BeginAwaiting()
		got := make(chan uint64, 1)
		go func() {
			_ = tr.AwaitRequest(context.Background())
			n, _, _, _ := tr.Test()
			got <- n
		}()
		<-waiting
		next.close(tr)
		if _, _, _, _, still := tr.Places(); still {
			t.Fatalf("%s: a request is still awaited", next.name)
		}
		if g, n := <-got, next.after(tr); g != n {
			t.Fatalf("%s: the waiting call read test %d; want %d", next.name, g, n)
		}
	}
	// A call whose own end comes first learns nothing, and never starts to
	// wait (OnAwait): were it to, the window's close the hook makes would
	// wake it to learn, which fails here at once, not at the test's timeout.
	tr := NewTracker()
	tr.OnAwait(func() { tr.End() })
	tr.BeginAwaiting()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tr.AwaitRequest(ctx); err != context.Canceled {
		t.Fatalf("a call ended before it waited: %v; want its context's error, no wait begun", err)
	}
	// A call whose own end comes as it waits learns nothing; one that wakes
	// to the request come learns from it, though its end came too: what it
	// reads as it wakes decides, never which the scheduler showed it first.
	tr = NewTracker()
	waiting := make(chan struct{}, 1)
	tr.OnAwait(func() { waiting <- struct{}{} })
	tr.BeginAwaiting()
	ctx, cancel = context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- tr.AwaitRequest(ctx) }()
	<-waiting
	cancel()
	if err := <-ended; err != context.Canceled {
		t.Fatalf("a call whose end came as it waited: %v; want its context's error", err)
	}
	for i := 0; i < 200; i++ {
		tr := NewTracker()
		ctx, cancel := context.WithCancel(context.Background())
		tr.OnAwait(func() {
			// Both before the call looks again: the request, and its end.
			tr.Give(GivenRequest(request("books")))
			cancel()
		})
		tr.BeginAwaiting()
		if err := tr.AwaitRequest(ctx); err != nil {
			t.Fatalf("a call that woke to its request, its end come too: %v; want it to learn from the request", err)
		}
	}
	// A call that waits stops waiting as its test's window closes, though
	// the next test began awaiting its own request before the call woke: it
	// was made in the test whose window closed, and never waits for, nor is
	// decided by, the next test's request. Its caller reads the next test's
	// number, a round that moved under the call. A wait begun again, for
	// the next test's request, fails here (OnAwait), not at the test's
	// timeout.
	for i := 0; i < 200; i++ {
		tr = NewTracker()
		waiting = make(chan struct{}, 2)
		tr.OnAwait(func() {
			select {
			case waiting <- struct{}{}:
			default:
			}
		})
		tr.BeginAwaiting()
		got := make(chan uint64, 1)
		go func() {
			_ = tr.AwaitRequest(context.Background())
			n, _, _, _ := tr.Test()
			got <- n
		}()
		<-waiting
		tr.BeginAwaiting()
		n, _, _, _ := tr.Test()
		select {
		case g := <-got:
			if g != n {
				t.Fatalf("the waiting call read test %d; want the next test, %d", g, n)
			}
		case <-waiting:
			tr.Give(GivenRequest(request("books")))
			<-got
			t.Fatalf("a call made in a test whose window closed waited again, for the next test's request")
		}
		if _, _, _, _, still := tr.Places(); !still {
			t.Fatalf("the next test no longer awaits its own request")
		}
	}
}

// A test's window that closes while the test still awaits its test case's
// request (End, the next Begin, NewSet), which the filter that opens it never
// brought, is said (OnUnreadClose), with how many calls waited for it: every
// value that changed in a call of the test is then refused, and the agent
// says so at its default log level. One whose request came says nothing.
func TestAWindowClosedWithoutItsRequestIsSaid(t *testing.T) {
	for _, row := range []struct {
		name  string
		close func(*Tracker)
		gives bool
		want  []int
	}{
		{"End, a call waiting", (*Tracker).End, false, []int{1}},
		{"the next test begins", (*Tracker).Begin, false, []int{1}},
		{"the next test begins awaiting its own", (*Tracker).BeginAwaiting, false, []int{1}},
		{"the next set begins", (*Tracker).NewSet, false, []int{1}},
		{"End, the request given", (*Tracker).End, true, nil},
	} {
		tr := NewTracker()
		var said []int
		tr.OnUnreadClose(func(n int) { said = append(said, n) })
		waiting := make(chan struct{}, 1)
		tr.OnAwait(func() {
			select {
			case waiting <- struct{}{}:
			default:
			}
		})
		tr.BeginAwaiting()
		done := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			_ = tr.AwaitRequest(ctx)
			close(done)
		}()
		<-waiting
		if row.gives {
			tr.Give(GivenRequest(&models.TestRequest{Test: "test-1"}))
		}
		row.close(tr)
		cancel()
		<-done
		if !slices.Equal(said, row.want) {
			t.Errorf("%s: said %v; want %v", row.name, said, row.want)
		}
	}
	// No call waited: said still, with none.
	tr := NewTracker()
	var said []int
	tr.OnUnreadClose(func(n int) { said = append(said, n) })
	tr.BeginAwaiting()
	tr.End()
	tr.End()
	if !slices.Equal(said, []int{0}) {
		t.Errorf("a window closed unread, no call waiting, then closed again: said %v; want [0]", said)
	}
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
		return []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{jsonType, {Name: "body", Recorded: `{"slots":["` + recorded + `"]}`, Sent: `{"slots":["` + sent + `"]}`}}})}
	}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("11:15"), shape("11:14"), at, req("11:14", "11:15"), nil, nil, nil)[0]; ln.Kind != KindVariable || !ln.From.Given || ln.From.Path != "body.slots[0]" {
		t.Errorf("F's pick in the request as sent: %s from %+v (%s); want a variable of the request's slot", ln.Kind, ln.From, ln.Why)
	}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("11:15"), shape("11:14"), at, req("09:00", "11:15"), nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "stands in test case test-1 at body.slots[0]") {
		t.Errorf("F's pick in the request as sent only: %s (%s); want it refused, naming the request's slot", ln.Kind, ln.Why)
	}
	rec := time.Date(2026, 10, 22, 11, 14, 17, 152e6, time.UTC)
	typed := func(at time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, time.Millisecond)}}
	}
	stamp := rec.Format("2006-01-02T15:04:05.000Z")
	if ln := ClassifyIn(clockedDrifts(), []string{p}, typed(rec.Add(FullRoundLead)), typed(rec), at, req(stamp, stamp), nil, nil, nil)[0]; ln.Kind != KindUnaccounted || ln.Why != requestHoldsWhy("test case test-1 at body.slots[0]", true) {
		t.Errorf("a typed value whose recorded moment the request holds: %s from %+v (%s); want it refused, naming the request's slot", ln.Kind, ln.From, ln.Why)
	}
	// A typed value that is the request's instant as recorded and as sent
	// (a template value): it drifted with the request's slot, a variable of
	// it, whole, found as a leaf that drifted with it before the echo reads
	// it; nothing is bound at the request (the adapter refuses it).
	sent := rec.Add(FullRoundLead).Format("2006-01-02T15:04:05.000Z")
	if ln := ClassifyIn(clockedDrifts(), []string{p}, typed(rec.Add(FullRoundLead)), typed(rec), at, req(stamp, sent), nil, nil, nil)[0]; ln.Kind != KindVariable || !ln.From.Given || !ln.From.Whole || ln.From.Echo || ln.From.Path != "body.slots[0]" {
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
			ln := ClassifyIn(clockedDrifts(), []string{p}, shape(live), shape("11:14"), at, read, nil, nil, w)[0]
			if ln.Kind != KindUnaccounted || ln.Why != requestHoldsWhy("test case test-1 at body.slots[1]", true) {
				t.Errorf("11:14 -> %q, %s: %s (%s); want it refused, naming the request's slot", live, wname, ln.Kind, ln.Why)
			}
			ln = ClassifyIn(clockedDrifts(), []string{p}, shape(live), shape("11:14"), at, []Earlier{GivenRequest(nil)}, nil, nil, w)[0]
			if ln.Kind != KindUnaccounted || ln.Why != unreadRequestWhy(unsentRequest) {
				t.Errorf("11:14 -> %q, %s, no request sent: %s (%s); want it refused for the request", live, wname, ln.Kind, ln.Why)
			}
		}
	}
	// One slot names both moments, by two rules.
	cut := []Earlier{GivenRequest(slotRequest("11:14:40.120"))}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("11:15"), shape("11:14"), at, cut, nil, nil, w)[0]; ln.Kind != KindUnaccounted || ln.Why != requestHoldsWhy("test case test-1 at body.slots[0]", true) {
		t.Errorf("a minute read in its window beside a slot of both moments: %s (%s); want it refused, naming the slot", ln.Kind, ln.Why)
	}
	// Where the request as sent gives F's moment by the rule that gives the
	// recorded one, it explains F's value: no pick the request no longer
	// lists (the echo's to read).
	both := []Earlier{GivenRequest(slotRequest("11:14:20.120", "11:15:20.120"))}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("11:15"), shape("11:14"), at, both, nil, nil, w)[0]; ln.Kind == KindNoise || strings.HasPrefix(ln.Why, "the moment it was recorded with stands in test case test-1 at body.slots[0], which") {
		t.Errorf("a slot for each moment, by one rule: %s (%s); want the echo's, not a pick the request no longer lists", ln.Kind, ln.Why)
	}
	// The part keploy test sent with template values: the request as
	// recorded held the moment, the reason says how keploy test sent it,
	// and the request as sent is not what decides (it holds neither).
	templated := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{jsonType, {Name: "body",
		Recorded: `{"slots":["2026-10-22T11:13:00Z","2026-10-22T11:14:00Z"]}`, Sent: `{"slots":["2026-10-22T11:20:00Z","2026-10-22T11:21:00Z"]}`}}})}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("none"), shape("11:14"), at, templated, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || ln.Why != requestHoldsWhy("test case test-1 at body.slots[1]", false) {
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
	dc := clockedDrifts()
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
				if !noiseOrRead(ln) {
					t.Fatalf("%q -> %q: %s (%s); want noise", tc.rec, tc.live, ln.Kind, ln.Why)
				}
			} else if ln.Kind != KindUnaccounted || !clientTestRefusal(ln.Why, tc.part, tc.w != nil, tc.rec, tc.live) {
				t.Fatalf("%q -> %q: %s (%s); want it refused, naming %s, saying keploy saw no read of the app's clock that matched it, and quoting no value", tc.rec, tc.live, ln.Kind, ln.Why, tc.part)
			}
			// In keploy test with no call window, a whole instant is a read
			// of the clock a replay checks, moved by the lead or not.
			if ln := ClassifyIn(clockedDrifts(), []string{p}, str(tc.live), str(tc.rec), rec, nil, nil, nil, nil)[0]; !noiseIn(ln, nil) {
				t.Fatalf("keploy test, %q -> %q: %s Read=%+v (%s); want noise, and of a whole instant a read of the clock", tc.rec, tc.live, ln.Kind, ln.Read, ln.Why)
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
	if ln := ClassifyIn(clientTestCache(), []string{p}, typed(tl), typed(tr), rec, nil, nil, nil, w)[0]; !noiseOrRead(ln) {
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
			if ln := ClassifyIn(clockedDrifts(), []string{p}, typed(c.l), typed(tr), rec, nil, nil, nil, nil)[0]; !noiseOrRead(ln) {
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
	// The call is in the minute the slot names: a read of the clock is no
	// later than its call.
	at := time.Date(2026, 10, 22, 11, 20, 16, 900_000_000, time.UTC)
	const p = "body.note"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	call := []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Payload: payloadOf("mock-1", at.Add(-time.Second)), Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.slot", Noise: "body.slot", Type: String, Value: "2026-10-22T11:20Z"}, Live: "2026-10-22T11:21Z", Place: "body.slot"}}}}
	if ln := ClassifyIn(clientTestCache(), []string{p}, str("book 2026-10-22T11:21Z"), str("book 2026-10-22T11:20Z"), at, call, nil, nil, nil)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Path != "body.slot" {
		t.Errorf("a client test: %s from %+v (%s); want a variable of mock-1's body.slot", ln.Kind, ln.From, ln.Why)
	}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, str("book 2026-10-22T11:21Z"), str("book 2026-10-22T11:20Z"), at, call, nil, nil, nil)[0]; ln.Kind != KindNoise {
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
	for name, dc := range map[string]*DriftCache{"keploy test": clockedDrifts(), "no cache": none} {
		if got := dc.ClockReads(rec, live); !reflect.DeepEqual(got, want) {
			t.Errorf("%s's clock evidence %v; want %v", name, got, want)
		}
	}
	at := time.Date(2026, 10, 22, 11, 14, 16, 473_000_000, time.UTC)
	r, l := InstantLeaf("p", "p", at, time.Millisecond), InstantLeaf("p", "p", at.Add(FullRoundLead), time.Millisecond)
	if clientTestCache().InstantClockRead(r, l, nil) {
		t.Error("a client test reads a typed instant the lead moved as a clock read")
	}
	if !clockedDrifts().InstantClockRead(r, l, nil) || !none.InstantClockRead(r, l, nil) || !InstantClockRead(r, l, nil) {
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

// A body keploy keeps as the base64 of its bytes is read as the text its
// bytes are, where its request declares a text media type: a pick from the
// words it holds is refused, naming the request, as from a text body. A body
// its request declares no text (a protobuf listing slots s17 and s18 as
// application/x-protobuf, its fields' tags and lengths printable or not,
// kept as its base64 or, as OSS records a body, as its bytes' text) is a
// request keploy could not read: every value
// that changed is refused, saying why, the pick a printable tag and length
// glue to its word ("BAs17") too, and a value no word of the request holds.
// Bytes that are no text are unread whatever the request declares.
func TestABinaryRequestBodyIsReadForItsText(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const p = "body.slot"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	requestAs := func(ct, part string, body []byte) []Earlier {
		v := string(body)
		if part == "binary" {
			v = base64.StdEncoding.EncodeToString(body)
		}
		parts := []models.TestRequestPart{{Name: part, Recorded: v, Sent: v}}
		if ct != "" {
			parts = append([]models.TestRequestPart{{Name: "header.Content-Type", Recorded: ct, Sent: ct}}, parts...)
		}
		return []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: parts})}
	}
	text := requestAs("text/plain; charset=utf-8", "binary", []byte("slots s17 s18"))
	if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("s18"), shape("s17"), at, text, nil, nil, nil)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "in test case test-1 at binary{text}") {
		t.Errorf("a pick from a binary request of text: %s from %+v (%s); want it refused, naming the request's text", ln.Kind, ln.From, ln.Why)
	}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("n-77c0d3"), shape("n-5f2a91"), at, text, nil, nil, nil)[0]; ln.Kind != KindNoise {
		t.Errorf("a value a binary request of text does not hold: %s (%s); want noise", ln.Kind, ln.Why)
	}
	// Field 8, length 65: "B" and "A", printable, before "s17"; every byte
	// printable.
	printable := append([]byte{0x42, 0x41}, []byte("s17 and more text here to make it sixty-five bytes long, all of i")...)
	for _, row := range []struct {
		name, ct, part string
		body           []byte
		why            string
	}{
		// Field 1, length 3, "s17"; field 1, length 3, "s18".
		{"tags and lengths no text, declared text", "text/plain", "binary", []byte{0x0a, 0x03, 's', '1', '7', 0x0a, 0x03, 's', '1', '8'}, binaryUnread},
		{"a protobuf, every byte printable, as base64", "application/x-protobuf", "binary", printable, mediaUnread("application/x-protobuf")},
		{"a protobuf, every byte printable, as its text", "application/x-protobuf", "body", printable, mediaUnread("application/x-protobuf")},
		{"a gRPC message as its text", "application/grpc+proto", "body", printable, mediaUnread("application/grpc+proto")},
	} {
		req := requestAs(row.ct, row.part, row.body)
		for _, v := range [][2]string{{"s17", "s18"}, {"n-5f2a91", "n-77c0d3"}} {
			ln := ClassifyIn(clockedDrifts(), []string{p}, shape(v[1]), shape(v[0]), at, req, nil, nil, nil)[0]
			if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, row.why) {
				t.Errorf("%s, %s: %s from %+v (%s); want it refused, the request unread", row.name, v[0], ln.Kind, ln.From, ln.Why)
			}
		}
	}
	for ct, want := range map[string]bool{
		"application/json": true, "application/vnd.api+json; charset=utf-8": true, "text/csv": true, "application/x-www-form-urlencoded": true, "application/soap+xml": true,
		"application/x-protobuf": false, "application/octet-stream": false, "multipart/form-data; boundary=x": false, "image/png": false, "": false, "not a type": false,
	} {
		if got := textMedia(ct); got != want {
			t.Errorf("textMedia(%q) = %v; want %v", ct, got, want)
		}
	}
	for b64, want := range map[string]struct {
		text string
		ok   bool
	}{
		"aGVsbG8gd29ybGQ=":     {"hello world", true},
		"aGVsbG8gd29ybGQ":      {"hello world", true},
		"bGluZSBvbmUKbGluZTI=": {"line one\nline2", true},
		"":                     {"", true},
		"CgNzMTcKA3MxOA==":     {"", false},
		"AAECAw==":             {"", false},
		"not base64!":          {"", false},
	} {
		if got, ok := binaryText(b64); got != want.text || ok != want.ok {
			t.Errorf("binaryText(%q) = %q, %v; want %q, %v", b64, got, ok, want.text, want.ok)
		}
	}
}

// A request body that declares no media type (no Content-Type) is read as
// text where its bytes are text (valid UTF-8 with no NUL or other control
// byte but a tab, a line feed and a carriage return), as its text or as the
// base64 of its bytes: a pick from its words is refused, naming the request,
// and a value it does not hold is noise. One whose bytes are no text (a
// protobuf's tags and lengths, a NUL, no UTF-8) is a request keploy could not
// read, the reason naming it: every value that changed is refused. Each side
// is read by what it declares: a sent body declared no text beside a recorded
// one declared JSON leaves the request unread, naming the sent side's type,
// and a recorded one declared no text beside a sent one declared JSON leaves
// it unread, naming the recorded side's; a side whose bytes are no text
// leaves it unread whichever side it is.
func TestARequestBodyOfNoDeclaredTypeIsReadWhereItsBytesAreText(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const p = "body.slot"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	request := func(parts ...models.TestRequestPart) []Earlier {
		return []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: parts})}
	}
	both := func(name, v string) models.TestRequestPart {
		return models.TestRequestPart{Name: name, Recorded: v, Sent: v}
	}
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	pick := func(req []Earlier) Learned {
		return ClassifyIn(clockedDrifts(), []string{p}, shape("s18"), shape("s17"), at, req, nil, nil, nil)[0]
	}
	other := func(req []Earlier) Learned {
		return ClassifyIn(clockedDrifts(), []string{p}, shape("n-77c0d3"), shape("n-5f2a91"), at, req, nil, nil, nil)[0]
	}
	for _, row := range []struct {
		name  string
		req   []Earlier
		where string
	}{
		{"its text", request(both("body", "slots s17 s18")), "in test case test-1 at body"},
		{"its text over lines and tabs", request(both("body", "slots:\r\n\ts17\n\ts18")), "in test case test-1 at body"},
		{"its bytes as base64", request(both("binary", b64([]byte("slots s17 s18")))), "in test case test-1 at binary{text}"},
	} {
		if e := row.req[0]; e.Unread != "" {
			t.Fatalf("%s: unread (%s); want it read as text", row.name, e.Unread)
		}
		if ln := pick(row.req); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, row.where) {
			t.Errorf("%s: a pick from it: %s (%s); want it refused, naming %q", row.name, ln.Kind, ln.Why, row.where)
		}
		if ln := other(row.req); ln.Kind != KindNoise {
			t.Errorf("%s: a value it does not hold: %s (%s); want noise", row.name, ln.Kind, ln.Why)
		}
	}
	// Field 1, length 3, "s17"; field 1, length 3, "s18".
	tagged := []byte{0x0a, 0x03, 's', '1', '7', 0x0a, 0x03, 's', '1', '8'}
	for _, row := range []struct {
		name string
		req  []Earlier
		why  string
	}{
		{"a protobuf's tags and lengths as its text", request(both("body", string(tagged))), undeclaredUnread},
		{"a protobuf as base64", request(both("binary", b64(tagged))), undeclaredUnread},
		{"a NUL in its text", request(both("body", "slots s17\x00s18")), undeclaredUnread},
		{"a DEL in its text", request(both("body", "slots s17\x7fs18")), undeclaredUnread},
		{"no UTF-8", request(both("body", "slots s17 \xffs18")), undeclaredUnread},
		{"recorded as text, sent as bytes", request(models.TestRequestPart{Name: "body", Recorded: "slots s17 s18", Sent: "slots s17\x01s18"}), undeclaredUnread},
		{"recorded as bytes, sent as text", request(models.TestRequestPart{Name: "body", Recorded: "slots s17\x01s18", Sent: "slots s17 s18"}), undeclaredUnread},
		{"recorded declared JSON, sent declared a protobuf",
			request(models.TestRequestPart{Name: "header.Content-Type", Recorded: "application/json", Sent: "application/x-protobuf"},
				models.TestRequestPart{Name: "body", Recorded: `{"slots":["s17","s18"]}`, Sent: "BAs17 s18"}),
			mediaUnread("application/x-protobuf")},
		{"recorded declared a protobuf, sent declared JSON",
			request(models.TestRequestPart{Name: "header.Content-Type", Recorded: "application/x-protobuf", Sent: "application/json"},
				models.TestRequestPart{Name: "body", Recorded: "BAs17 s18", Sent: `{"slots":["s17","s18"]}`}),
			mediaUnread("application/x-protobuf")},
	} {
		if e := row.req[0]; e.Unread != row.why {
			t.Errorf("%s: unread %q; want %q", row.name, e.Unread, row.why)
		}
		for _, ln := range []Learned{pick(row.req), other(row.req)} {
			if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, row.why) {
				t.Errorf("%s: %s (%s); want it refused, the request unread", row.name, ln.Kind, ln.Why)
			}
		}
	}
	for s, want := range map[string]bool{
		"": true, "slots s17 s18": true, "a\tb\r\nc": true, "日本語 ok": true,
		"a\x00b": false, "a\x01b": false, "a\x1bb": false, "a\x7fb": false, "a\u0085b": false, "a\vb": false, "a\fb": false, "\xff": false,
	} {
		if got := plainText(s); got != want {
			t.Errorf("plainText(%q) = %v; want %v", s, got, want)
		}
	}
}

// A request body that declares no media type, read as text by its bytes,
// may still be a protobuf whose tags and lengths are printable ("BA", field
// 8 of length 65, before "s17"): a word of what changed that it holds glued
// to the bytes around it is refused, the reason naming the body as one that
// declares no media type, never learned as noise, as the same bytes declared
// application/x-protobuf are refused too, and so is a body that one side
// declares and the other does not, whichever side it is. A body both sides
// declare text is read by its words, a glued word no word of it; the base64
// a binary part keeps frames no words; and a value the body does not hold is
// noise.
func TestAWordGluedInARequestBodyOfNoDeclaredTypeIsRefused(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const p = "body.slot"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	request := func(ct, part, body string) []Earlier {
		parts := []models.TestRequestPart{{Name: part, Recorded: body, Sent: body}}
		if ct != "" {
			// "recorded|sent" where the sides declare apart.
			rec, sent, apart := strings.Cut(ct, "|")
			if !apart {
				sent = rec
			}
			parts = append(parts, models.TestRequestPart{Name: "header.Content-Type", Recorded: rec, Sent: sent})
		}
		return []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: parts})}
	}
	v17, v18 := "s17"+strings.Repeat("x", 62), "s18"+strings.Repeat("x", 62)
	framed := "BA" + v17 + "BA" + v18
	const glued = "glued to the bytes around it, in a body that declares no media type (no Content-Type)"
	for _, row := range []struct {
		name, ct, part, body, rec, live string
		kind                            Kind
		why                             string
	}{
		{"a word glued to printable framing", "", "body", framed, "s17", "s18", KindUnaccounted, "in test case test-1 at body, " + glued},
		{"a whole field glued to printable framing", "", "body", framed, v17, v18, KindUnaccounted, "in test case test-1 at body, " + glued},
		{"an id's word glued to printable framing", "", "body", "BAa3f1c2d4-1111-2222-3333-444455556666" + strings.Repeat("y", 29),
			"a3f1c2d4-1111-2222-3333-444455556666", "b3f1c2d4-1111-2222-3333-444455556666", KindUnaccounted, "in test case test-1 at body, " + glued},
		{"its bytes as base64", "", "binary", base64.StdEncoding.EncodeToString([]byte(framed)), "s17", "s18", KindUnaccounted, "in test case test-1 at binary{text}, " + glued},
		{"a JSON leaf's word glued", "", "body", `{"slot":"BAs17"}`, "s17", "s18", KindUnaccounted, "in test case test-1 at body.slot, " + glued},
		{"the same bytes declared a protobuf", "application/x-protobuf", "body", framed, "s17", "s18", KindUnaccounted, mediaUnread("application/x-protobuf")},
		{"recorded declared text, sent declaring none", "text/plain|", "body", framed, "s17", "s18", KindUnaccounted, "in test case test-1 at body, " + glued},
		{"recorded declaring none, sent declared text", "|text/plain", "body", framed, "s17", "s18", KindUnaccounted, "in test case test-1 at body, " + glued},
		{"the same bytes declared text", "text/plain", "body", framed, "s17", "s18", KindNoise, ""},
		{"a value it does not hold", "", "body", framed, "n-5f2a91", "n-77c0d3", KindNoise, ""},
		{"a word of the base64 only", "", "binary", base64.StdEncoding.EncodeToString([]byte("text here")), "dGV4", "dGV5", KindNoise, ""},
	} {
		req := request(row.ct, row.part, row.body)
		ln := ClassifyIn(clockedDrifts(), []string{p}, shape(row.live), shape(row.rec), at, req, nil, nil, nil)[0]
		if ln.Kind != row.kind || !strings.Contains(ln.Why, row.why) {
			t.Errorf("%s: %s (%s); want %s naming %q", row.name, ln.Kind, ln.Why, row.kind, row.why)
		}
	}
}

// jsonType is a test case's request's Content-Type part declaring JSON.
var jsonType = models.TestRequestPart{Name: "header.Content-Type", Recorded: "application/json", Sent: "application/json"}

// harnessToken is a bearer JWT a test harness signs for its session: its
// auth_time (the harness's clock read when it signed it), a jti per token.
func harnessToken(authTime, jti string) string {
	return "Bearer " + testJWT(`{"alg":"HS256","typ":"JWT"}`, `{"sub":"u-1","sid":"e2e-go-session","auth_time":`+authTime+`,"jti":"`+jti+`"}`, "c2lnbmF0dXJlLW9uZQ")
}

// A header holding a JWT is read for its claims too, each a leaf of its own
// ahead of the header whole, as recorded and as sent: a value a claim holds
// whole is found there whole, and a reason names the claim. A header holding
// no token adds none.
func TestATokenTheTestRequestHoldsIsReadForItsClaims(t *testing.T) {
	g := GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{
		{Name: "header.Authorization", Recorded: harnessToken("1793373215", "j-1"), Sent: harnessToken("1793387031", "j-2")},
		{Name: "header.X-Trace", Recorded: "t-1", Sent: "t-1"},
	}})
	at := map[string]int{}
	for i, s := range g.Seen {
		at[s.Leaf.Path] = i
	}
	claim := g.Seen[at["header.Authorization{jwt}.auth_time"]]
	if claim.Leaf.Value != "1793373215" || claim.Live != "1793387031" {
		t.Fatalf("auth_time read as %q -> %q in %+v", claim.Leaf.Value, claim.Live, g.Seen)
	}
	if i, ok := at["header.Authorization{jwt}.jti"]; !ok || g.Seen[i].Live != "j-2" || at["header.Authorization{jwt}.auth_time"] > at["header.Authorization"] {
		t.Errorf("claims not read ahead of the header whole: %+v", g.Seen)
	}
	for p := range at {
		if strings.HasPrefix(p, "header.X-Trace{jwt}") {
			t.Errorf("a header holding no token read for claims: %s", p)
		}
	}
	// A call's token carries the claim on: a variable of the request's
	// claim, found whole.
	const p = "header.authorization{jwt}.auth_time"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: Number, Value: v}}}
	}
	call := time.Date(2026, 10, 31, 1, 2, 3, 0, time.UTC)
	if ln := ClassifyIn(clockedDrifts(), []string{p}, shape("1793387031"), shape("1793373215"), call, []Earlier{g}, nil, nil, nil)[0]; ln.Kind != KindVariable || !ln.From.Given || ln.From.Incoming || ln.From.Path != "header.Authorization{jwt}.auth_time" || !ln.From.Whole {
		t.Errorf("a claim the request gave: %s from %+v (%s); want a variable of the request's claim", ln.Kind, ln.From, ln.Why)
	}
}

// A client test's incoming requests, read in its window before the call, are
// its given place: a part each of them that holds it holds one value of, as
// recorded and in this run (the harness's auth_time in the token it signed),
// named by the first recorded request that sent it. A part two requests hold
// two values of (a jti per token, a request id) is no place; nor is a request
// recorded after the call, nor another test's. A call's token that carries
// the claim on is a variable of the incoming requests' claim.
func TestAClientTestsIncomingRequestsAreItsGivenPlace(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 1, 2, 0, 0, time.UTC)
	req := func(name string, at time.Time, authTime, jti, id string) IncomingRequest {
		return IncomingRequest{Name: name, At: at, Parts: []models.TestRequestPart{
			{Name: "url", Recorded: "http://localhost:8080/v1/apps/" + id},
			{Name: "header.Authorization", Recorded: harnessToken(authTime, jti)},
			{Name: "header.X-Request-Id", Recorded: "rq-" + id},
		}}
	}
	recorded := []IncomingRequest{
		req("test-9", t0.Add(40*time.Second), "1793373999", "j-9", "9"), // after the call
		req("test-4", t0.Add(2*time.Second), "1793373215", "j-4", "4"),
		req("test-3", t0.Add(time.Second), "1793373215", "j-3", "3"),
	}
	live := []IncomingRequest{req("", t0, "1793387031", "j-a", "a"), req("", t0, "1793387031", "j-b", "b")}
	call := t0.Add(5 * time.Second)
	e := IncomingGiven("TestRun", recorded, live, true, call)
	if e.Unread != "" || !e.Given || !e.Incoming || e.Mock != "incoming request test-3 of test TestRun" || !e.At.Equal(t0.Add(time.Second)) {
		t.Fatalf("place %+v", e)
	}
	seen := map[string]Seen{}
	for _, s := range e.Seen {
		seen[s.Leaf.Path] = s
	}
	if s := seen["header.Authorization{jwt}.auth_time"]; s.Leaf.Value != "1793373215" || s.Live != "1793387031" {
		t.Errorf("auth_time read as %q -> %q in %+v", s.Leaf.Value, s.Live, e.Seen)
	}
	for _, p := range []string{"header.Authorization{jwt}.jti", "header.X-Request-Id", "url.path.2", "header.Authorization"} {
		if _, ok := seen[p]; ok {
			t.Errorf("%s, two values among the requests, kept as a place: %+v", p, seen[p])
		}
	}
	if _, ok := seen["header.Authorization{jwt}.sid"]; !ok {
		t.Errorf("sid, one value in each, not kept: %+v", e.Seen)
	}
	const p = "header.authorization{jwt}.auth_time"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: Number, Value: v}}}
	}
	ln := ClassifyIn(clientTestCache(), []string{p}, shape("1793387031"), shape("1793373215"), call, []Earlier{e}, nil, nil, nil)[0]
	if ln.Kind != KindVariable || !ln.From.Given || !ln.From.Incoming || ln.From.Mock != "incoming request test-3 of test TestRun" || ln.From.Path != "header.Authorization{jwt}.auth_time" || !ln.From.Whole {
		t.Errorf("the harness's auth_time carried on: %s from %+v (%s); want a variable of the incoming requests' claim", ln.Kind, ln.From, ln.Why)
	}
	// Two values of auth_time among the recorded requests: no place holds it.
	split := append([]IncomingRequest(nil), recorded...)
	split[1] = req("test-4", t0.Add(2*time.Second), "1793373216", "j-4", "4")
	if e := IncomingGiven("TestRun", split, live, true, call); hasSeen(e, "header.Authorization{jwt}.auth_time") {
		t.Errorf("auth_time, two values as recorded, kept: %+v", e.Seen)
	}
	// None recorded before the call, none seen in this run, or this run's
	// not seen at all: no place, and it refuses nothing by itself.
	for name, e := range map[string]Earlier{
		"none recorded before": IncomingGiven("TestRun", recorded[:1], live, true, call),
		"none in this run":     IncomingGiven("TestRun", recorded, nil, true, call),
		"not seen":             IncomingGiven("TestRun", recorded, live, false, call),
	} {
		if e.Unread == "" || len(e.Seen) != 0 || !e.Incoming {
			t.Errorf("%s: %+v; want a place that could not be read", name, e)
		}
		if why := requestUnread([]Earlier{e}); why != "" {
			t.Errorf("%s: refuses every change (%s)", name, why)
		}
		if ln := ClassifyIn(clientTestCache(), []string{p}, shape("1793387031"), shape("1793373215"), call, []Earlier{e}, nil, nil, nil)[0]; ln.Kind == KindVariable {
			t.Errorf("%s: followed as a variable from %+v", name, ln.From)
		}
	}
	if e := IncomingGiven("TestRun", recorded, live, false, call); e.Unread != incomingUncaptured {
		t.Errorf("not seen: %q", e.Unread)
	}
}

// hasSeen reports whether e holds a leaf at path.
func hasSeen(e Earlier, path string) bool {
	for _, s := range e.Seen {
		if s.Leaf.Path == path {
			return true
		}
	}
	return false
}

// The tracker gives a client test (SetClientTests) its incoming requests as a
// place: those keploy mock recorded in its window (GiveIncoming, taken as
// the test begins), with those the application was sent while it ran
// (Incoming). A request sent while no test is open is no test's; the next
// test begins with none of the last one's; a test keploy mock gave no
// recorded requests, or a tracker of keploy test's tests, has no such place.
func TestTrackerGivesAClientTestItsIncomingRequests(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 1, 2, 0, 0, time.UTC)
	in := func(name, authTime string) IncomingRequest {
		return IncomingRequest{Name: name, At: t0, Parts: []models.TestRequestPart{{Name: "header.Authorization", Recorded: harnessToken(authTime, "j-"+name)}}}
	}
	incoming := func(tr *Tracker) (Earlier, bool) {
		_, earlier, _, _ := tr.Test()
		for _, e := range earlier {
			if e.Incoming {
				return e, true
			}
		}
		return Earlier{}, false
	}
	tr := NewTracker()
	tr.SetClientTests(true)
	tr.SetIncomingSeen(true)
	tr.Incoming(in("", "1793387031")) // before any test: no test's
	tr.GiveIncoming("TestRun", []IncomingRequest{in("test-3", "1793373215")})
	tr.Begin()
	if e, ok := incoming(tr); !ok || e.Unread == "" {
		t.Fatalf("no request in this run yet: %+v, %v; want a place that could not be read", e, ok)
	}
	tr.Incoming(in("", "1793387031"))
	e, ok := incoming(tr)
	if !ok || e.Unread != "" || e.Mock != "incoming request test-3 of test TestRun" || !hasSeen(e, "header.Authorization{jwt}.auth_time") {
		t.Fatalf("the test's place: %+v, %v", e, ok)
	}
	tr.End()
	tr.Incoming(in("", "1793387032")) // between tests: dropped
	tr.Begin()
	if e, ok := incoming(tr); ok {
		t.Fatalf("a test keploy mock gave no recorded requests has a place: %+v", e)
	}
	tr.GiveIncoming("TestNone", nil)
	tr.Begin()
	if e, ok := incoming(tr); ok {
		t.Fatalf("a test keploy mock gave an empty list of recorded requests has a place: %+v", e)
	}
	tr.GiveIncoming("TestNext", []IncomingRequest{in("test-7", "1793373215")})
	tr.Begin()
	if e, ok := incoming(tr); !ok || e.Unread == "" || len(e.Seen) != 0 {
		t.Fatalf("the next test holds the last one's requests: %+v", e)
	}
	tr.SetIncomingSeen(false)
	tr.Incoming(in("", "1793387031"))
	if e, ok := incoming(tr); !ok || e.Unread != incomingUncaptured {
		t.Fatalf("this run's not seen: %+v", e)
	}
	server := NewTracker()
	server.SetIncomingSeen(true)
	server.GiveIncoming("test-1", []IncomingRequest{in("test-1", "1793373215")})
	server.Begin()
	server.Incoming(in("", "1793387031"))
	if e, ok := incoming(server); ok {
		t.Fatalf("keploy test's tests have a place of incoming requests: %+v", e)
	}
}
