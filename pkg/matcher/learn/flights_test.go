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
