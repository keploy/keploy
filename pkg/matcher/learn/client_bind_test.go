package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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
// keploy test (no client test) reads the instant in its first appearance
// as it reads the instant whole: in a call with no window, a plain read of
// the clock at its word, which every replay checks; it marks nothing.
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
	if ln := ClassifyIn(clockedDrifts(), []string{p}, str(live), str(rec), at, nil, nil, nil, nil)[0]; !readAtWord(ln, "[E2E] app-crud-", "") || ln.Bindable {
		t.Errorf("keploy test, first sent: %s Read=%+v bindable=%v (%s); want a plain read of the clock at its word, not bindable", ln.Kind, ln.Read, ln.Bindable, ln.Why)
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

// A client test's value that changed, with an instant keploy saw no read
// of, is the test's own value only where nothing earlier may be its
// source. Where the word rule let a word of it go at an earlier place
// (follows.first: 48d1 of the id mock-8's insert sent, which record's full
// round gave the application in another form the value took none of), or
// where the search for its source went past an earlier place (follows.
// refuted: the recorded name stands whole in mock-s's answer, which record's
// full round gave the application byte for byte while the name changed),
// it is refused as before and never bindable: binding it would hold every
// replay to the value sent here, whatever that earlier place gives.
func TestAClientTestsValueBesideAnEarlierWordIsNeverBindable(t *testing.T) {
	const (
		p       = "bind.$8"
		recName = "[E2E] app-crud-1793224420036884439"
		liveNm  = "[E2E] app-crud-1793311234567890123"
	)
	w := &Window{
		Rec: Span{From: eRecAt.Add(-900 * time.Millisecond), To: eRecAt},
		F:   Span{From: eRecAt.Add(60_500 * time.Millisecond), To: eRecAt.Add(61_400 * time.Millisecond)},
	}
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: v}}}
	}
	before := eRecAt.Add(-50 * time.Millisecond)
	insert := []Earlier{{Mock: "mock-8", At: before, Payload: eLeaf(before, 6, "", "").Payload, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.cmd.documents.0._id", Type: String, Value: chanceSrcRec}, Live: chanceSrcLive},
	}}}
	answered := append(eStartup(), ePg("mock-s", 150*time.Millisecond, "SELECT name FROM apps", []models.PostgresV3Cells{{{Value: recName}}}, "SELECT 1").ServedAs(6).ServedAsRecorded())
	for _, tc := range []struct {
		name      string
		rec, live string
		earlier   []Earlier
		raw       []Raw
		noiseWhy  string // what keploy test's noise, or its read's Refuted, names
		read      bool   // keploy test reads its one changed instant at its word
	}{
		{"a word the word rule let go", recName + " 48d1", liveNm + " dddd", insert, eStartup(), "stands in mock-8 at body.cmd.documents.0._id", false},
		{"a source the search went past", recName, liveNm, nil, answered, "its recorded value stands whole in mock-s at", true},
	} {
		// keploy test: noise, naming the earlier place (the row's shape is
		// the word rule's, not a client test's own value); a text whose one
		// changed instant is, whole, a read of the clock, that read at its
		// word, naming the place set aside (Refuted), as the instant whole
		// is.
		ln := ClassifyFollowing(clockedDrifts(), []string{p}, str(tc.live), str(tc.rec), eRecAt, tc.earlier, tc.raw, nil, nil, nil)[0]
		if tc.read && (!readAtWord(ln, "[E2E] app-crud-", "") || ln.Bindable || !strings.Contains(ln.Refuted, tc.noiseWhy)) {
			t.Fatalf("keploy test, %s: %s Read=%+v bindable=%v (%q, refuted %q); want a read at its word naming %q", tc.name, ln.Kind, ln.Read, ln.Bindable, ln.Why, ln.Refuted, tc.noiseWhy)
		}
		if !tc.read && (ln.Kind != KindNoise || ln.Bindable || !strings.Contains(ln.Why, tc.noiseWhy)) {
			t.Fatalf("keploy test, %s: %s bindable=%v (%q); want noise naming %q", tc.name, ln.Kind, ln.Bindable, ln.Why, tc.noiseWhy)
		}
		// Without the earlier place, the client test's own value: bindable.
		if ln := ClassifyFollowing(clientTestCache(), []string{p}, str(tc.live), str(tc.rec), eRecAt, nil, eStartup(), nil, w, nil)[0]; ln.Kind != KindUnaccounted || !ln.Bindable {
			t.Fatalf("client test, %s, no earlier place: %s bindable=%v (%q); want refused and bindable", tc.name, ln.Kind, ln.Bindable, ln.Why)
		}
		for _, win := range []*Window{w, nil} {
			ln := ClassifyFollowing(clientTestCache(), []string{p}, str(tc.live), str(tc.rec), eRecAt, tc.earlier, tc.raw, nil, win, nil)[0]
			if ln.Kind != KindUnaccounted || !clientTestRefusal(ln.Why, "its characters 16 to 34", win != nil, tc.rec, tc.live) {
				t.Errorf("client test, %s, window %v: %s (%q); want the client test's refusal", tc.name, win != nil, ln.Kind, ln.Why)
			}
			if ln.Bindable {
				t.Errorf("FALSE GREEN? client test, %s, window %v: bindable; want it never bound as the test's variable", tc.name, win != nil)
			}
		}
	}
}
