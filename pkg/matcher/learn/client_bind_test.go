package learn

import (
	"testing"
	"time"
)

// A client test's harness makes a value of its own each run (api-server's
// e2e suite names its app `[E2E] app-crud-<UnixNano>` by the test's clock)
// and the application sends it in the test's Mongo calls. keploy sees no
// source of it and no read of the application's clock for the instant in
// it, so the call that first sends it is refused, as before, but marked
// Bindable: a caller that binds variables learns it there as the test's
// variable. A later call that sends the same value finds that place as its
// source (a variable, held to the bound value); so does a later call that
// sends another value in its place, whose caller's variable pass then
// refuses it (the bound value differs), and it is no binding place itself.
// keploy test (no client test) learns the first appearance as noise, as
// before, and marks nothing.
func TestAClientTestsGivenValueIsBindableWhereItIsFirstSent(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC) // the recorded call
	w := &Window{
		Rec: Span{From: at.Add(-900 * time.Millisecond), To: at},
		F:   Span{From: at.Add(60_500 * time.Millisecond), To: at.Add(61_400 * time.Millisecond)},
	}
	const (
		p       = "body.cmd.query.app.name"
		rec     = "[E2E] app-crud-1793224420036884439"
		live    = "[E2E] app-crud-1793311234567890123"
		another = "[E2E] app-crud-1793311299999999999"
		part    = "its characters 16 to 34"
	)
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	for _, win := range []*Window{w, nil} {
		ln := ClassifyIn(clientTestCache(), []string{p}, str(live), str(rec), at, nil, nil, nil, win)[0]
		if ln.Kind != KindUnaccounted || !clientTestRefusal(ln.Why, part, win != nil, rec, live) {
			t.Errorf("first sent, window %v: %s (%s); want the client test's refusal, naming %s", win != nil, ln.Kind, ln.Why, part)
		}
		if !ln.Bindable {
			t.Errorf("first sent, window %v: not bindable; want it bindable as the test's variable", win != nil)
		}
	}
	if ln := ClassifyIn(clockedDrifts(), []string{p}, str(live), str(rec), at, nil, nil, nil, nil)[0]; ln.Kind != KindNoise || ln.Bindable {
		t.Errorf("keploy test, first sent: %s bindable=%v (%s); want noise, not bindable", ln.Kind, ln.Bindable, ln.Why)
	}
	// Its own answer holds it, whole (an echo), or the word of it that
	// changed alone (an echo keploy cannot answer with this run's value):
	// never a binding place.
	const seq = "body.cursor.firstBatch.0.seq"
	for what, v := range map[string]string{"whole": rec, "the word that changed alone": "1793224420036884439"} {
		resp := []Leaf{{Path: seq, Noise: seq, Type: String, Value: v}}
		if ln := ClassifyIn(clientTestCache(), []string{p}, str(live), str(rec), at, nil, nil, resp, w)[0]; ln.Kind != KindUnaccounted || ln.Bindable {
			t.Errorf("first sent, its answer holding it %s: %s bindable=%v (%s); want refused, not bindable", what, ln.Kind, ln.Bindable, ln.Why)
		}
	}
	// Its own answer holds the instant in it in another layout, a typed
	// instant of the same moment: an echo (Echoes reads the moment a typed
	// instant holds, where echoedPart reads words, which a typed instant has
	// none of), so a value the answer gives back, never a binding place.
	createdAt := []Leaf{{Path: "body.cursor.firstBatch.0.createdAt", Noise: "body.cursor.firstBatch.0.createdAt", Type: Instant, Value: "2026-10-28T21:53:40.036884439Z"}}
	if ln := ClassifyIn(clientTestCache(), []string{p}, str(live), str(rec), at, nil, nil, createdAt, w)[0]; ln.Kind != KindUnaccounted || ln.Bindable {
		t.Errorf("first sent, its answer holding its instant typed: %s bindable=%v (%s); want refused, not bindable", ln.Kind, ln.Bindable, ln.Why)
	}
	// The insert that first sent it, an earlier call of the test.
	const insert = "body.cmd.documents.0.name"
	first := []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Payload: payloadOf("mock-1", at.Add(-time.Second)), Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: insert, Noise: insert, Type: String, Value: rec}, Live: live, Place: insert}}}}
	if ln := ClassifyIn(clientTestCache(), []string{p}, str(live), str(rec), at, first, nil, nil, w)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Path != insert || ln.Bindable {
		t.Errorf("sent again: %s from %+v bindable=%v (%s); want a variable of mock-1's %s", ln.Kind, ln.From, ln.Bindable, ln.Why, insert)
	}
	// Another value sent in its place: the same recorded value, so a use of
	// mock-1's variable (the caller's variable pass holds it to the value
	// mock-1's place bound, and the call misses there), never a binding
	// place of its own.
	if ln := ClassifyIn(clientTestCache(), []string{p}, str(another), str(rec), at, first, nil, nil, w)[0]; ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Live != live || ln.Bindable {
		t.Errorf("another value sent in its place: %s from %+v bindable=%v (%s); want a variable of mock-1's %s, bound to its value, not bindable", ln.Kind, ln.From, ln.Bindable, ln.Why, insert)
	}
}
