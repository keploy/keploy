package learn

import (
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// F's order is read only where the application can have seen it: a serve made
// in a request that landed before the read, while no other request was aloft
// (Flight). Two requests on parallel connections aloft at one instant leave
// both their serves in an order keploy cannot tell, whichever the scheduler
// let F claim first and whether the second was claimed before or after the
// first's answer went out.
func TestFServesInRequestsOnParallelConnectionsAreUnordered(t *testing.T) {
	a, b := &models.Mock{Name: "mock-a"}, &models.Mock{Name: "mock-b"}
	type step func(tr *Tracker, fs map[string]*Flight)
	lift := func(c string) step { return func(tr *Tracker, fs map[string]*Flight) { fs[c] = tr.Lift() } }
	land := func(c string) step { return func(_ *Tracker, fs map[string]*Flight) { fs[c].Land() } }
	serve := func(m *models.Mock) step { return func(tr *Tracker, _ map[string]*Flight) { tr.ServedMock(m) } }
	for _, tc := range []struct {
		name       string
		steps      []step
		wantA, wan [2]int
	}{
		{"one after the other", []step{lift("1"), serve(a), land("1"), lift("2"), serve(b), land("2")}, [2]int{1, 1}, [2]int{2, 2}},
		{"one after the other, B first", []step{lift("2"), serve(b), land("2"), lift("1"), serve(a), land("1")}, [2]int{2, 2}, [2]int{1, 1}},
		{"both aloft, A claimed first", []step{lift("1"), lift("2"), serve(a), serve(b), land("1"), land("2")}, [2]int{}, [2]int{}},
		{"both aloft, B claimed first", []step{lift("1"), lift("2"), serve(b), serve(a), land("2"), land("1")}, [2]int{}, [2]int{}},
		{"B's request in before A's answer went out, claimed after", []step{lift("1"), serve(a), lift("2"), land("1"), serve(b), land("2")}, [2]int{}, [2]int{}},
		{"B lifted and landed inside A", []step{lift("1"), lift("2"), serve(b), land("2"), serve(a), land("1")}, [2]int{}, [2]int{}},
		{"A claimed in no request", []step{lift("1"), land("1"), serve(a), lift("2"), serve(b), land("2")}, [2]int{}, [2]int{2, 2}},
		{"A served again on a parallel request", []step{lift("1"), serve(a), land("1"), lift("2"), lift("3"), serve(a), land("2"), land("3"), lift("4"), serve(b), land("4")}, [2]int{}, [2]int{3, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			fs := map[string]*Flight{}
			for _, s := range tc.steps {
				s(tr, fs)
			}
			o := tr.Placed().Order
			if f, l := o.Of(a); [2]int{f, l} != tc.wantA {
				t.Fatalf("A at %d,%d; want %v", f, l, tc.wantA)
			}
			if f, l := o.Of(b); [2]int{f, l} != tc.wan {
				t.Fatalf("B at %d,%d; want %v", f, l, tc.wan)
			}
		})
	}
}

// A read sees a serve whose request is still aloft as in an order keploy
// cannot tell, and keeps that verdict after the request lands, or after
// another request crosses one it read as told: what the application sends
// after the read changes nothing it decided.
func TestAReadOfFsOrderKeepsItsVerdictWhateverTheApplicationSendsAfter(t *testing.T) {
	a, b := &models.Mock{Name: "mock-a"}, &models.Mock{Name: "mock-b"}
	tr := NewTracker()
	f1 := tr.Lift()
	tr.ServedMock(a)
	aloft := tr.Placed().Order
	if f, l := aloft.Of(a); f != 0 || l != 0 {
		t.Fatalf("A read while its request is aloft at %d,%d; want 0,0", f, l)
	}
	f1.Land()
	f1.Land() // landed once
	landed := tr.Placed().Order
	if f, l := landed.Of(a); f != 1 || l != 1 {
		t.Fatalf("A read once its request landed at %d,%d; want 1,1", f, l)
	}
	if f, l := aloft.Of(a); f != 0 || l != 0 {
		t.Fatalf("the read made while A was aloft now says %d,%d; want 0,0 still", f, l)
	}
	f2, f3 := tr.Lift(), tr.Lift()
	tr.ServedMock(b)
	f2.Land()
	f3.Land()
	if f, l := landed.Of(a); f != 1 || l != 1 {
		t.Fatalf("A at %d,%d after later requests crossed; want 1,1", f, l)
	}
	if f, l := tr.Placed().Order.Of(b); f != 0 || l != 0 {
		t.Fatalf("B, served while two requests were aloft, at %d,%d; want 0,0", f, l)
	}
	var none *Flight
	none.Land() // nothing
}

// A tracker no connection was ever lifted on reads F's claims as the order,
// as before flights; a protocol phase F served is in no request's order
// once one was; and the next set reads only its own serves' requests.
func TestFlightsBeginWithTheFirstConnectionAndFollowTheSet(t *testing.T) {
	a, b := &models.Mock{Name: "mock-a"}, &models.Mock{Name: "mock-b"}
	tr := NewTracker()
	tr.ServedMock(a)
	tr.ServedMock(b)
	if f, l := tr.Placed().Order.Of(b); f != 2 || l != 2 {
		t.Fatalf("B, with no connection lifted, at %d,%d; want 2,2", f, l)
	}
	f := tr.Lift()
	greeting := &models.Mock{Name: "mock-greeting", Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}}}
	if !greeting.IsProtocolPhase() {
		t.Fatal("a config mock is no protocol phase: the row reads none")
	}
	tr.ServedMock(greeting)
	f.Land()
	if f, l := tr.Placed().Order.Of(greeting); f != 0 || l != 0 {
		t.Fatalf("a protocol phase served in a request at %d,%d; want 0,0", f, l)
	}
	if f, l := tr.Placed().Order.Of(a); f != 0 || l != 0 {
		t.Fatalf("A, served before the first lift once flights are read, at %d,%d; want 0,0", f, l)
	}
	h := tr.Lift()
	tr.ServedMock(b) // the 4th serve of the set, in h
	h.Land()
	tr.NewSet()
	tr.ServedMock(a)
	tr.ServedMock(a)
	tr.ServedMock(a)
	tr.ServedMock(b) // the next set's 4th serve, in no request
	if f, l := tr.Placed().Order.Of(b); f != 0 || l != 0 {
		t.Fatalf("B in the next set, in no request, at %d,%d; want 0,0: the set before's 4th serve's request is not its", f, l)
	}
	tr.NewSet()
	g := tr.Lift()
	tr.ServedMock(b)
	g.Land()
	if f, l := tr.Placed().Order.Of(b); f != 1 || l != 1 {
		t.Fatalf("B in the set after at %d,%d; want 1,1", f, l)
	}
}

// interleavings are every merge of a and b that keeps each one's own order.
func interleavings[T any](a, b []T) [][]T {
	if len(a) == 0 {
		return [][]T{append([]T(nil), b...)}
	}
	if len(b) == 0 {
		return [][]T{append([]T(nil), a...)}
	}
	var out [][]T
	for _, rest := range interleavings(a[1:], b) {
		out = append(out, append([]T{a[0]}, rest...))
	}
	for _, rest := range interleavings(a, b[1:]) {
		out = append(out, append([]T{b[0]}, rest...))
	}
	return out
}

// flightStep is one event of a request: its lift, a serve in it (said:
// the parser says whose request it is, ServedMockIn; none: the parser says it
// is no request's, NoRequest; handshake: the parser says the handshake
// answers it and was chosen in it, ServedHandshakeIn; ssl: chosen in the
// request c+"1"), or its landing.
type flightStep struct {
	c    string
	what string // "lift", "said", "unsaid", "none", "handshake", "ssl", "land"
	m    *models.Mock
}

func runFlights(tr *Tracker, steps []flightStep) {
	fs := map[string]*Flight{}
	for _, s := range steps {
		switch s.what {
		case "lift":
			fs[s.c] = tr.Lift()
		case "said":
			tr.ServedMockIn(s.m, fs[s.c])
		case "unsaid":
			tr.ServedMock(s.m)
		case "none":
			tr.ServedMockIn(s.m, NoRequest())
		case "handshake":
			tr.ServedHandshakeIn(s.m, fs[s.c], fs[s.c])
		case "ssl":
			// Chosen in the connection's SSLRequest (its c+"1"), answering
			// its StartupMessage (c).
			tr.ServedHandshakeIn(s.m, fs[s.c+"1"], fs[s.c])
		case "land":
			fs[s.c].Land()
		}
	}
}

// A request that reads no recording (a driver's monitor heartbeat, a long
// poll no recording answers, a health check, one answered by a protocol
// phase) crosses no serve: a serve its parser said was made in A's request
// keeps its place in F's order whatever the scheduler interleaved with it.
func TestARequestThatReadsNoRecordingCrossesNoServe(t *testing.T) {
	a := &models.Mock{Name: "mock-a"}
	hello := &models.Mock{Name: "mock-hello", Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}}}
	served := []flightStep{{"A", "lift", nil}, {"A", "said", a}, {"A", "land", nil}}
	for name, heartbeat := range map[string][]flightStep{
		"idle":                            {{"H", "lift", nil}, {"H", "land", nil}},
		"a protocol phase it said":        {{"H", "lift", nil}, {"H", "said", hello}, {"H", "land", nil}},
		"a protocol phase it did not say": {{"H", "lift", nil}, {"H", "unsaid", hello}, {"H", "land", nil}},
	} {
		for i, steps := range interleavings(served, heartbeat) {
			tr := NewTracker()
			runFlights(tr, steps)
			if f, l := tr.Placed().Order.Of(a); f == 0 || f != l {
				t.Fatalf("%s heartbeat, interleaving %d %v: A at %d,%d; want its serve's number", name, i, steps, f, l)
			}
		}
	}
}

// Two serves their parsers said were made in requests aloft at one instant
// are both in an order keploy cannot tell, whichever was claimed first; two
// that were not aloft together keep F's order. A serve said in a request
// already landed is in none's order; and a serve no parser said, made while
// A was aloft with another, makes both serves (fail closed).
func TestServesSaidInRequestsAloftTogetherAreUnordered(t *testing.T) {
	a, b := &models.Mock{Name: "mock-a"}, &models.Mock{Name: "mock-b"}
	sa := []flightStep{{"A", "lift", nil}, {"A", "said", a}, {"A", "land", nil}}
	sb := []flightStep{{"B", "lift", nil}, {"B", "said", b}, {"B", "land", nil}}
	for i, steps := range interleavings(sa, sb) {
		pos := map[string]int{}
		for k, s := range steps {
			pos[s.c+s.what] = k
		}
		overlap := pos["Blift"] < pos["Aland"] && pos["Alift"] < pos["Bland"]
		tr := NewTracker()
		runFlights(tr, steps)
		o := tr.Placed().Order
		fa, _ := o.Of(a)
		fb, _ := o.Of(b)
		if told := fa != 0 && fb != 0; told == overlap {
			t.Fatalf("interleaving %d %v: A at %d, B at %d; overlapping %v", i, steps, fa, fb, overlap)
		}
	}
	tr := NewTracker()
	runFlights(tr, []flightStep{{"A", "lift", nil}, {"A", "land", nil}})
	f := tr.Lift()
	f.Land()
	tr.ServedMockIn(a, f)
	if fa, la := tr.Placed().Order.Of(a); fa != 0 || la != 0 {
		t.Fatalf("A, said in a request already landed, at %d,%d; want 0,0", fa, la)
	}
	tr = NewTracker()
	runFlights(tr, []flightStep{{"A", "lift", nil}, {"H", "lift", nil}, {"A", "said", a}, {"H", "unsaid", b}, {"H", "land", nil}, {"A", "land", nil}})
	if fa, la := tr.Placed().Order.Of(a); fa != 0 || la != 0 {
		t.Fatalf("A, crossed by a serve no parser said, at %d,%d; want 0,0", fa, la)
	}
	other := NewTracker()
	g := other.Lift()
	tr = NewTracker()
	fA, fH := tr.Lift(), tr.Lift()
	tr.ServedMockIn(a, g) // another tracker's request is no request of this one's
	g.Land()
	fH.Land()
	fA.Land()
	if fa, la := tr.Placed().Order.Of(a); fa != 0 || la != 0 {
		t.Fatalf("A, said in another tracker's request, at %d,%d; want 0,0", fa, la)
	}
}

// A read made once A's request landed, before the request that overlapped it
// was served, sees A in F's order (the application cannot have read the
// later answer), and keeps that verdict once it is served; a read made after
// sees both unordered.
func TestACrossingIsReadFromTheServeThatMadeIt(t *testing.T) {
	a, b := &models.Mock{Name: "mock-a"}, &models.Mock{Name: "mock-b"}
	tr := NewTracker()
	fa := tr.Lift()
	tr.ServedMockIn(a, fa)
	fb := tr.Lift()
	fa.Land()
	before := tr.Placed().Order
	if f, l := before.Of(a); f != 1 || l != 1 {
		t.Fatalf("A, read before B was served, at %d,%d; want 1,1", f, l)
	}
	tr.ServedMockIn(b, fb)
	fb.Land()
	if f, l := before.Of(a); f != 1 || l != 1 {
		t.Fatalf("the read made before B was served now says A at %d,%d; want 1,1 still", f, l)
	}
	after := tr.Placed().Order
	if f, l := after.Of(a); f != 0 || l != 0 {
		t.Fatalf("A, read after B was served, at %d,%d; want 0,0", f, l)
	}
	if f, l := after.Of(b); f != 0 || l != 0 {
		t.Fatalf("B at %d,%d; want 0,0", f, l)
	}
}

// A serve its parser says was made in no request of the application's
// (NoRequest: a server push nothing the application sent caused) makes no
// request aloft a serve: a request beside it, and a heartbeat aloft with that
// one, keep the request's own serve in F's order wherever the scheduler put
// the push, and the push itself is in an order keploy cannot tell. The same
// push told as no parser's (ServedMock) makes every request aloft a serve, or
// is the one aloft's.
func TestAServeMadeInNoRequestMakesNoRequestAServe(t *testing.T) {
	a, push := &models.Mock{Name: "mock-a"}, &models.Mock{Name: "mock-push"}
	served := []flightStep{{"A", "lift", nil}, {"A", "said", a}, {"A", "land", nil}}
	beside := []flightStep{{"H", "lift", nil}, {"P", "none", push}, {"H", "land", nil}}
	for i, steps := range interleavings(served, beside) {
		tr := NewTracker()
		runFlights(tr, steps)
		o := tr.Placed().Order
		if f, l := o.Of(a); f == 0 || f != l {
			t.Fatalf("interleaving %d %v: A at %d,%d; want its serve's number: the push was made in no request", i, steps, f, l)
		}
		if f, l := o.Of(push); f != 0 || l != 0 {
			t.Fatalf("interleaving %d %v: the push at %d,%d; want 0,0: it was made in no request", i, steps, f, l)
		}
	}
	tr := NewTracker()
	runFlights(tr, []flightStep{{"A", "lift", nil}, {"H", "lift", nil}, {"P", "unsaid", push}, {"A", "said", a}, {"H", "land", nil}, {"A", "land", nil}})
	if f, l := tr.Placed().Order.Of(a); f != 0 || l != 0 {
		t.Fatalf("A, beside a heartbeat and a push no parser said, at %d,%d; want 0,0", f, l)
	}
	tr = NewTracker()
	runFlights(tr, []flightStep{{"A", "lift", nil}, {"P", "unsaid", push}, {"A", "said", a}, {"A", "land", nil}})
	if f, l := tr.Placed().Order.Of(push); f != 1 || l != 1 {
		t.Fatalf("a push no parser said, made with A alone aloft, at %d,%d; want 1,1: A's", f, l)
	}
	NoRequest().Land()
	if NoRequest().LiftCaused() != NoRequest() || (*Flight)(nil).LiftCaused() != NoRequest() {
		t.Fatal("what no request, or a nil one, caused is not NoRequest")
	}
}

// A serve a request caused (Flight.LiftCaused: the push its PUBLISH let go)
// is told in that request whenever it lands: after the request's own serve,
// as F claimed them, and in no order keploy can tell beside a serve whose
// request was aloft at an instant from the cause's lift to the push's
// landing, whether it landed before the push was made or not, and whether
// the cause lifted in this set or before it began (an earlier set's serves
// are no serves of this one's). A request that serves nothing crosses no
// serve; one landed before the cause lifted, or lifted once the push landed,
// keeps F's order.
func TestAServeARequestCausedIsToldInItWheneverItLanded(t *testing.T) {
	a, b, push := &models.Mock{Name: "mock-a"}, &models.Mock{Name: "mock-b"}, &models.Mock{Name: "mock-push"}
	type step func(tr *Tracker, fs map[string]*Flight)
	lift := func(c string) step { return func(tr *Tracker, fs map[string]*Flight) { fs[c] = tr.Lift() } }
	land := func(c string) step { return func(_ *Tracker, fs map[string]*Flight) { fs[c].Land() } }
	said := func(c string, m *models.Mock) step {
		return func(tr *Tracker, fs map[string]*Flight) { tr.ServedMockIn(m, fs[c]) }
	}
	caused := func(c, by string) step {
		return func(_ *Tracker, fs map[string]*Flight) { fs[c] = fs[by].LiftCaused() }
	}
	newSet := func(tr *Tracker, _ map[string]*Flight) { tr.NewSet() }
	inNone := func(m *models.Mock) step {
		return func(tr *Tracker, _ map[string]*Flight) { tr.ServedMockIn(m, NoRequest()) }
	}
	x := &models.Mock{Name: "mock-x"}
	pushed := []step{caused("P", "C"), said("P", push), land("P")}
	cause := []step{lift("C"), said("C", a), land("C")}
	join := func(parts ...[]step) []step {
		var out []step
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	for _, tc := range []struct {
		name               string
		steps              []step
		wantA, wantB, want [2]int
	}{
		{"consumed once its cause landed", join(cause, pushed), [2]int{1, 1}, [2]int{}, [2]int{2, 2}},
		{"consumed while its cause is aloft", []step{lift("C"), said("C", a), caused("P", "C"), said("P", push), land("P"), land("C")}, [2]int{1, 1}, [2]int{}, [2]int{2, 2}},
		{"a request lifted and landed between the cause's landing and the push", join(cause, []step{lift("R"), said("R", b), land("R")}, pushed), [2]int{1, 1}, [2]int{}, [2]int{}},
		{"a request lifted once the push landed", join(cause, pushed, []step{lift("R"), said("R", b), land("R")}), [2]int{1, 1}, [2]int{3, 3}, [2]int{2, 2}},
		{"a request aloft when the cause lifted, landed before it", []step{lift("R"), lift("C"), said("R", b), land("R"), said("C", a), land("C"), caused("P", "C"), said("P", push), land("P")}, [2]int{}, [2]int{}, [2]int{}},
		{"a request lifted while the push is aloft", join(cause, []step{caused("P", "C"), said("P", push), lift("R"), said("R", b), land("R"), land("P")}), [2]int{1, 1}, [2]int{}, [2]int{}},
		{"a heartbeat between the cause and the push", join(cause, []step{lift("H"), land("H")}, pushed), [2]int{1, 1}, [2]int{}, [2]int{2, 2}},
		{"the cause lifted before the set began", join(cause, []step{newSet, lift("R"), said("R", b), land("R")}, pushed), [2]int{}, [2]int{}, [2]int{}},
		{"a request lifted before the set began, aloft at the push", join(cause, []step{lift("R"), newSet, said("R", b), caused("P", "C"), said("P", push), land("R"), land("P")}), [2]int{}, [2]int{}, [2]int{}},
		{"the push aloft at the read", join(cause, []step{caused("P", "C"), said("P", push)}), [2]int{1, 1}, [2]int{}, [2]int{}},
		{"a request landed before the cause lifted, in a later set", join([]step{newSet, lift("R"), said("R", b), land("R")}, cause, pushed), [2]int{2, 2}, [2]int{1, 1}, [2]int{3, 3}},
		{"the cause lifted before the set began, an earlier set's request after it", join(cause, []step{lift("R"), said("R", b), land("R"), newSet, inNone(x)}, pushed), [2]int{}, [2]int{}, [2]int{2, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			fs := map[string]*Flight{}
			for _, s := range tc.steps {
				s(tr, fs)
			}
			o := tr.Placed().Order
			for _, w := range []struct {
				m    *models.Mock
				want [2]int
			}{{a, tc.wantA}, {b, tc.wantB}, {push, tc.want}} {
				if f, l := o.Of(w.m); [2]int{f, l} != w.want {
					t.Fatalf("%s at %d,%d; want %v", w.m.Name, f, l, w.want)
				}
			}
		})
	}
	// A read made before the push was served keeps the request it read as
	// told after the push crossed it.
	tr := NewTracker()
	fc := tr.Lift()
	tr.ServedMockIn(a, fc)
	fc.Land()
	fr := tr.Lift()
	tr.ServedMockIn(b, fr)
	fr.Land()
	before := tr.Placed().Order
	fp := fc.LiftCaused()
	tr.ServedMockIn(push, fp)
	fp.Land()
	if f, l := before.Of(b); f != 2 || l != 2 {
		t.Fatalf("the read made before the push says B at %d,%d; want 2,2 still", f, l)
	}
	if f, l := tr.Placed().Order.Of(b); f != 0 || l != 0 {
		t.Fatalf("B, read after the push its span crossed, at %d,%d; want 0,0", f, l)
	}
}

// A request whose answer the application is not given ends untold
// (Flight.EndUntold): every serve made in it is in an order keploy cannot
// tell in each read made from then on, whether it ended aloft (no write of
// it will be made) or after it landed (the write that started failed); a read
// made before keeps its verdict. Ended aloft, it is in flight no more, so a
// request lifted after it is ordered once it lands, while one that
// overlapped it stays unordered (fail closed). It ends once, and a Land after
// it is nothing.
func TestARequestEndedUntoldLeavesItsServesUnordered(t *testing.T) {
	a, b := &models.Mock{Name: "mock-a"}, &models.Mock{Name: "mock-b"}
	of := func(o Order, m *models.Mock) [2]int {
		f, l := o.Of(m)
		return [2]int{f, l}
	}

	t.Run("ended aloft, never written", func(t *testing.T) {
		tr := NewTracker()
		f1 := tr.Lift()
		tr.ServedMockIn(a, f1)
		f1.EndUntold()
		f1.Land() // changes no read: it ended untold
		f1.EndUntold()
		f2 := tr.Lift()
		tr.ServedMockIn(b, f2)
		f2.Land()
		o := tr.Placed().Order
		if got := of(o, a); got != [2]int{} {
			t.Fatalf("A, served in a request whose answer was never written, at %v; want 0,0", got)
		}
		if got := of(o, b); got != [2]int{2, 2} {
			t.Fatalf("B, served in a request lifted after A's ended untold, at %v; want 2,2: the ended request is in flight no more", got)
		}
	})

	t.Run("landed, then its write failed", func(t *testing.T) {
		tr := NewTracker()
		f1 := tr.Lift()
		tr.ServedMockIn(a, f1)
		f1.Land()
		before := tr.Placed().Order
		f1.EndUntold()
		after := tr.Placed().Order
		if got := of(before, a); got != [2]int{1, 1} {
			t.Fatalf("a read made before the write failed now says A at %v; want 1,1 still", got)
		}
		if got := of(after, a); got != [2]int{} {
			t.Fatalf("A, whose write failed, at %v in a read made after; want 0,0", got)
		}
		f2 := tr.Lift()
		tr.ServedMockIn(b, f2)
		f2.Land()
		if got := of(tr.Placed().Order, b); got != [2]int{2, 2} {
			t.Fatalf("B, served in a request lifted after A's write failed, at %v; want 2,2", got)
		}
	})

	t.Run("a request that overlapped it stays unordered", func(t *testing.T) {
		tr := NewTracker()
		f1, f2 := tr.Lift(), tr.Lift()
		tr.ServedMockIn(a, f1)
		tr.ServedMockIn(b, f2)
		f1.EndUntold()
		f2.Land()
		o := tr.Placed().Order
		if got := of(o, a); got != [2]int{} {
			t.Fatalf("A at %v; want 0,0", got)
		}
		if got := of(o, b); got != [2]int{} {
			t.Fatalf("B, served while A's request was aloft, at %v; want 0,0", got)
		}
	})

	t.Run("a serve told in it after it ended", func(t *testing.T) {
		tr := NewTracker()
		f1 := tr.Lift()
		f1.EndUntold()
		tr.ServedMockIn(a, f1)
		if got := of(tr.Placed().Order, a); got != [2]int{} {
			t.Fatalf("A, told in a request that had ended untold, at %v; want 0,0", got)
		}
	})

	var none *Flight
	none.EndUntold()
	NoRequest().EndUntold()
	tr := NewTracker()
	tr.Lift().Land()
	tr.ServedMockIn(a, NoRequest())
	if got := of(tr.Placed().Order, a); got != [2]int{} {
		t.Fatalf("a serve in no request at %v after NoRequest's EndUntold; want 0,0 as before", got)
	}
}

// overlapped reports whether requests a and b were aloft at one instant in
// steps: neither landed before the other lifted.
func overlapped(steps []flightStep, a, b string) bool {
	at := func(c, what string) int {
		for i, s := range steps {
			if s.c == c && s.what == what {
				return i
			}
		}
		return -1
	}
	return !(at(a, "land") < at(b, "lift") || at(b, "land") < at(a, "lift"))
}

// handshakeMock is a connection's recorded start-up: a protocol phase.
func handshakeMock(name string) *models.Mock {
	return &models.Mock{Name: name, Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}}}
}

// A handshake its parser says answers a request (a Postgres start-up answers
// its connection's StartupMessage) has that request's place in F's order:
// told once it landed, in the order F served it. Two connections opened at
// once (their start-ups aloft at one instant) may get each other's start-ups,
// so both are in an order keploy cannot tell, whichever F claimed first. A
// pool's handshake beside a statement on another connection leaves the
// statement its place (the handshake is no serve of the application's
// request), and has none itself; a driver's heartbeat beside a handshake
// leaves the handshake its place.
func TestAHandshakeIsToldInTheRequestItAnswers(t *testing.T) {
	hsA, hsB := handshakeMock("mock-hs-a"), handshakeMock("mock-hs-b")
	q := &models.Mock{Name: "mock-q"}
	hello := &models.Mock{Name: "mock-hello", Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}}}
	open := func(c string, m *models.Mock) []flightStep {
		return []flightStep{{c, "lift", nil}, {c, "handshake", m}, {c, "land", nil}}
	}
	t.Run("two opens", func(t *testing.T) {
		for i, steps := range interleavings(open("A", hsA), open("B", hsB)) {
			tr := NewTracker()
			runFlights(tr, steps)
			o := tr.Placed().Order
			fa, la := o.Of(hsA)
			fb, lb := o.Of(hsB)
			if overlapped(steps, "A", "B") {
				if fa != 0 || fb != 0 {
					t.Fatalf("interleaving %d %v: A's start-up at %d,%d and B's at %d,%d; want both untold (0, 0): the opens were aloft together", i, steps, fa, la, fb, lb)
				}
				continue
			}
			if fa == 0 || fa != la || fb == 0 || fb != lb || fa == fb {
				t.Fatalf("interleaving %d %v: A's start-up at %d,%d and B's at %d,%d; want each its own serve's number", i, steps, fa, la, fb, lb)
			}
		}
	})
	t.Run("a pool's handshake beside a statement", func(t *testing.T) {
		stmt := []flightStep{{"S", "lift", nil}, {"S", "said", q}, {"S", "land", nil}}
		for i, steps := range interleavings(stmt, open("H", hsA)) {
			tr := NewTracker()
			runFlights(tr, steps)
			o := tr.Placed().Order
			if f, l := o.Of(q); f == 0 || f != l {
				t.Fatalf("interleaving %d %v: the statement at %d,%d; want its serve's number (a handshake beside it is no serve)", i, steps, f, l)
			}
			f, _ := o.Of(hsA)
			if want := !overlapped(steps, "S", "H"); (f != 0) != want {
				t.Fatalf("interleaving %d %v: the handshake at %d; told %v, want %v", i, steps, f, f != 0, want)
			}
		}
	})
	t.Run("a heartbeat beside a handshake", func(t *testing.T) {
		beat := []flightStep{{"P", "lift", nil}, {"P", "said", hello}, {"P", "land", nil}}
		for i, steps := range interleavings(beat, open("A", hsA)) {
			tr := NewTracker()
			runFlights(tr, steps)
			if f, l := tr.Placed().Order.Of(hsA); f == 0 || f != l {
				t.Fatalf("interleaving %d %v: the start-up at %d,%d; want its serve's number", i, steps, f, l)
			}
		}
	})
	for _, tc := range []struct {
		name  string
		steps []flightStep
		want  bool
	}{
		{"told before its request landed", open("A", hsA), true},
		{"told after its request landed", []flightStep{{"A", "lift", nil}, {"A", "land", nil}, {"A", "handshake", hsA}}, false},
		{"told in no request a connection lifted", []flightStep{{"A", "lift", nil}, {"Z", "handshake", hsA}, {"A", "land", nil}}, false},
		{"told as a protocol phase", []flightStep{{"A", "lift", nil}, {"A", "said", hsA}, {"A", "land", nil}}, false},
		{"told with no connection ever lifted", []flightStep{{"Z", "handshake", hsA}}, true},
		{"told in NoRequest", []flightStep{{"A", "lift", nil}, {"N", "none", hsA}, {"A", "land", nil}}, false},
		{"chosen in an earlier request", []flightStep{{"A1", "lift", nil}, {"A1", "land", nil}, {"A", "lift", nil}, {"A", "ssl", hsA}, {"A", "land", nil}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			runFlights(tr, tc.steps)
			if f, l := tr.Placed().Order.Of(hsA); (f != 0 && f == l) != tc.want {
				t.Fatalf("%v: the start-up at %d,%d; told %v, want %v", tc.steps, f, l, f != 0, tc.want)
			}
		})
	}
}

// A handshake chosen in one request and answering another (an SSLRequest's
// connection, whose start-up is picked as the SSLRequest arrives and answers
// the StartupMessage after it) has no place, whatever its requests
// overlapped: two such connections whose SSLRequests were aloft together may
// have swapped start-ups although their StartupMessages came one after the
// other. It still makes its request one that got a handshake, so a
// handshake chosen and answered in one request beside it has no place
// either.
func TestAHandshakeChosenInAnEarlierRequestHasNoPlace(t *testing.T) {
	hsA, hsB := handshakeMock("mock-hs-a"), handshakeMock("mock-hs-b")
	steps := []flightStep{
		{"A1", "lift", nil}, {"B1", "lift", nil}, {"A1", "land", nil}, {"B1", "land", nil},
		{"B", "lift", nil}, {"B", "ssl", hsB}, {"B", "land", nil},
		{"A", "lift", nil}, {"A", "ssl", hsA}, {"A", "land", nil},
	}
	tr := NewTracker()
	runFlights(tr, steps)
	o := tr.Placed().Order
	for _, m := range []*models.Mock{hsA, hsB} {
		if f, l := o.Of(m); f != 0 || l != 0 {
			t.Fatalf("%s at %d,%d; want no place (chosen in its SSLRequest)", m.Name, f, l)
		}
	}
	tr = NewTracker()
	runFlights(tr, []flightStep{{"A", "lift", nil}, {"B", "lift", nil}, {"A", "ssl", hsA}, {"B", "handshake", hsB}, {"A", "land", nil}, {"B", "land", nil}})
	if f, l := tr.Placed().Order.Of(hsB); f != 0 || l != 0 {
		t.Fatalf("B's start-up at %d,%d beside one with no place; want none", f, l)
	}
}

// A request that told two handshakes beside another that told one leaves
// every one unordered: each is a handshake serve, not only the first.
func TestEveryHandshakeOfARequestIsCheckedForACrossing(t *testing.T) {
	x, a, y := handshakeMock("mock-x"), handshakeMock("mock-a"), handshakeMock("mock-y")
	tr := NewTracker()
	runFlights(tr, []flightStep{{"A", "lift", nil}, {"B", "lift", nil}, {"A", "handshake", x}, {"B", "handshake", y}, {"A", "handshake", a}, {"A", "land", nil}, {"B", "land", nil}})
	for _, m := range []*models.Mock{x, a, y} {
		if f, l := tr.Placed().Order.Of(m); f != 0 || l != 0 {
			t.Fatalf("%s at %d,%d; want untold (A and B aloft together)", m.Name, f, l)
		}
	}
}

// A read compares its own set's serve numbers only: a request aloft across
// NewSet that becomes a serve, or gets a handshake, in the next set does
// not change a read of the set before, whatever that set's numbers are.
func TestAReadOfASetIgnoresTheNextSetsNumbers(t *testing.T) {
	hsA, hsB := handshakeMock("mock-hs-a"), handshakeMock("mock-hs-b")
	for _, pad := range []int{0, 3, 6} {
		tr := NewTracker()
		a := tr.Lift()
		b := tr.Lift()
		for i := 0; i < pad; i++ {
			tr.ServedMock(handshakeMock(fmt.Sprintf("mock-pad-%d", i)))
		}
		tr.ServedHandshakeIn(hsA, a, a)
		a.Land()
		read := tr.Placed().Order
		if f, _ := read.Of(hsA); f == 0 {
			t.Fatalf("pad %d: A's start-up has no place before B got one", pad)
		}
		tr.NewSet()
		tr.ServedHandshakeIn(hsB, b, b)
		b.Land()
		if f, l := read.Of(hsA); f == 0 || f != l {
			t.Fatalf("pad %d: the read of the earlier set changed after the next set's handshake: A at %d,%d", pad, f, l)
		}
	}
}
