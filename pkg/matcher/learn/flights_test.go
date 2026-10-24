package learn

import (
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
// is no request's, NoRequest), or its landing.
type flightStep struct {
	c    string
	what string // "lift", "said", "unsaid", "none", "land"
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
