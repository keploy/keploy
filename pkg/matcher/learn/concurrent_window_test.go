package learn

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// windowTracker is r's tracker (concurrentRun.tracker) told the test's
// recorded begin, as the agent tells it (RecordedBegin before a keploy mock
// test begins, RecordedTestBegin with a keploy test case's request), so a
// call's window in the recording knows where its test began there.
func (r concurrentRun) windowTracker(client bool) *Tracker {
	tr := NewTracker()
	tr.SetClientTests(client)
	tr.SetAppClock(r.clock.at)
	tr.SetAppClockRun(r.clock.began)
	tr.NewSetAt(r.prevTest.Add(-time.Second))
	tr.ServedMockAt(&models.Mock{Name: "mock-4", Kind: models.HTTP}, r.prevTest)
	tr.RecordedBegin(r.recBegin)
	tr.BeginAt(r.began)
	tr.ServedMockAt(&models.Mock{Name: "mock-7", Kind: models.Mongo}, r.a)
	tr.ServedMockAt(&models.Mock{Name: "mock-8", Kind: models.Mongo}, r.b)
	return tr
}

// windowRecording is where the upsert's recording stands in the recording,
// counted from the test's recorded begin: its previous recorded call (prev)
// and the upsert itself (call).
type windowRecording struct {
	what       string
	prev, call time.Duration
}

// recordings are the two recordings the window rows learn the upsert of:
// one whose previous recorded call is the previous test's (only record's
// full round served another goroutine's call after the read), and one
// where the consumer's call was recorded after the read too.
func (r concurrentRun) recordings() []windowRecording {
	return []windowRecording{
		{"the previous recorded call is the previous test's", -150 * time.Millisecond, 400 * time.Microsecond},
		{"the consumer's call was recorded after the read too", 1200 * time.Microsecond, 2 * time.Millisecond},
	}
}

// learnUpsert is the learn step's reading of the upsert's place p, recorded
// as rec and seen as live in record's full round, in the window of its
// call, recorded as wr says, in a server test or a client test.
func (r concurrentRun) learnUpsert(t *testing.T, client bool, wr windowRecording, p string, rec, live Leaf) (Learned, *Tracker) {
	t.Helper()
	return r.learnUpsertIn(t, client, wr, p, rec, live, nil)
}

// learnUpsertIn is learnUpsert in the call's window as edit gives it, where
// edit is not nil.
func (r concurrentRun) learnUpsertIn(t *testing.T, client bool, wr windowRecording, p string, rec, live Leaf, edit func(*Window) *Window) (Learned, *Tracker) {
	t.Helper()
	tr := r.windowTracker(client)
	paired := &models.Mock{Name: "mock-5", Kind: models.Mongo}
	paired.Spec.ReqTimestampMock = r.recBegin.Add(wr.call)
	w := tr.CallWindowFrom(r.recBegin.Add(wr.prev), paired, r.upTo)
	if w == nil {
		t.Fatalf("client %v, %s: no window for the upsert", client, wr.what)
	}
	if edit != nil {
		w = edit(w)
	}
	dc := clockedDrifts()
	dc.clientTests = client
	return ClassifyIn(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: live}}, Shape{Leaves: map[string]Leaf{p: rec}}, paired.Spec.ReqTimestampMock, nil, nil, nil, w)[0], tr
}

// The learn step reads a call's clock reads in its window from where its
// test began too, as the replay's check does: another goroutine's call that
// keploy serves after the test began (a consumer beside the test's handler)
// can be served between a read and the call that carries it, in record's
// full round and in the recording alike. The handler's upsert, whose
// created the app read after the test's begin moved its clock (.934 at a
// cut of a millisecond) and which reached keploy after the consumer's two
// serves, recorded as the .933 cut, is learned as a plain read of the
// clock, in a server test and in a client test, whether or not the
// consumer's call was recorded after the read too. What the learn step
// learned, the replay's check (record's confirming round, and keploy test)
// passes for the same value. A value outside the test's span in either run
// is refused as before: a read of the clock as it ran before the move, a
// value two cuts before the test's begin, and one more than a cut after
// the call.
func TestTheLearnStepReadsAReadFromItsTestsBeginPastAnotherGoroutinesServes(t *testing.T) {
	r := newConcurrentRun()
	const p = "body.docs.msgs[].u.{}.created"
	at := func(v time.Time) Leaf { return InstantLeaf(p, p, v, time.Millisecond) }
	recRead := r.recBegin.Add(300 * time.Microsecond)
	if got := at(recRead).Value; !strings.HasSuffix(got, ":33.933Z") {
		t.Fatalf("the recorded read writes %s; want the .933 cut", got)
	}
	for _, client := range []bool{false, true} {
		for _, wr := range r.recordings() {
			for _, c := range r.reads() {
				if at(c.v).Value == at(recRead).Value {
					continue // no drift: the read at the move's end writes the recorded cut
				}
				ln, tr := r.learnUpsert(t, client, wr, p, at(recRead), at(c.v))
				learned := ln.Kind == KindClockRead && ln.Read != nil
				if learned != c.ok {
					t.Errorf("client %v, %s: %s (%s): learned %s (%s); want a read of the clock %v", client, wr.what, c.what, at(c.v).Value, ln.Kind, ln.Why, c.ok)
					continue
				}
				if !learned {
					continue
				}
				s, ok := ClockSpanFrom(WithTracker(context.Background(), tr), r.upTo)
				if ok, why := CheckClockRead(p, *ln.Read, at(c.v), s, ok); !ok {
					t.Errorf("client %v, %s: %s (%s): learned as %s, which the replay's check refuses: %s", client, wr.what, c.what, at(c.v).Value, ln.Read, why)
				}
			}
		}
		// A recorded value outside the test's span in the recording is no
		// read the test made there.
		ln, _ := r.learnUpsert(t, client, r.recordings()[1], p, at(r.recBegin.Add(-2*time.Millisecond)), at(r.appRead))
		if ln.Kind == KindClockRead {
			t.Errorf("client %v: a recorded value two cuts before the test's recorded begin was learned as %s; want it refused", client, ln.Read)
		}
	}
}

// A read of the clock plus a constant offset (an expiry the handler stamps
// thirty minutes after its read) that its server test's call's window
// refuses is not learned from its test's begin: the learn step takes only a
// plain read there (Window.fromTestBegin). An offset's constant is fitted
// to the window it is read in, so a wider window widens the offsets it
// admits by as much as it widens the window, and a time to live shorter by
// up to that much would read as the same offset. The handler's expiry past
// the consumer's serves, and one recorded two cuts before the test's
// recorded begin, are learned as the call's window alone learns them, its
// refusal and its reason, with the consumer's call recorded before and
// after the read. A client test's window from its test's begin
// (Window.sinceBegin) takes no offset either, as on main.
func TestTheLearnStepTakesNoReadPlusAnOffsetFromItsTestsBegin(t *testing.T) {
	r := newConcurrentRun()
	const p = "body.docs.msgs[].u.{}.expiresAt"
	const ttl = 30 * time.Minute
	at := func(v time.Time) Leaf { return InstantLeaf(p, p, v.Add(ttl), time.Millisecond) }
	offsets := func(w *Window) *Window { return w.ComparesOffsets() }
	callsOwn := func(w *Window) *Window { // the call's window alone: no test begin to read from (TestFrom, Began)
		w = w.ComparesOffsets()
		w.Rec.TestFrom, w.F.TestFrom, w.Rec.Began, w.F.Began = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		return w
	}
	for _, wr := range r.recordings() {
		for _, c := range []struct {
			what string
			rec  time.Time
		}{
			{"the handler's expiry", r.recBegin.Add(300 * time.Microsecond)},
			{"an expiry recorded two cuts before the test's recorded begin", r.recBegin.Add(-2 * time.Millisecond)},
		} {
			for _, client := range []bool{false, true} {
				ln, _ := r.learnUpsertIn(t, client, wr, p, at(c.rec), at(r.appRead), offsets)
				if ln.Kind != KindUnaccounted {
					t.Errorf("client %v, %s: %s (recorded %s, %s in the full round): learned %s (%s); want it refused", client, wr.what, c.what, at(c.rec).Value, at(r.appRead).Value, ln.Kind, ln.Why)
					continue
				}
				own, _ := r.learnUpsertIn(t, client, wr, p, at(c.rec), at(r.appRead), callsOwn)
				if own.Kind != KindUnaccounted || own.Why == "" {
					t.Fatalf("client %v, %s: %s: the call's window alone learned %s (%q); want a refusal with its reason", client, wr.what, c.what, own.Kind, own.Why)
				}
				if ln.Why != own.Why {
					t.Errorf("client %v, %s: %s: refused with %q; want the call's window's reason %q", client, wr.what, c.what, ln.Why, own.Why)
				}
			}
		}
	}
}

// A word read (the millisecond count in an event's id) past another
// goroutine's serves is never refused by the learn step. In a client test
// it is learned from its test's begin as a whole value is: a plain read of
// that word, which the replay's check passes, the rest of the text still
// compared, and a word outside the test's span is refused. In a server test
// a changed word of a text is noise of that word whatever its call's window
// holds (only the changed word goes unchecked), in its call's window and
// outside it alike, so no window there refuses it.
func TestTheLearnStepReadsAWordReadFromItsTestsBeginPastAnotherGoroutinesServes(t *testing.T) {
	r := newConcurrentRun()
	const p = "body.msg"
	text := func(v time.Time) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: "evt-" + strconv.FormatInt(v.UnixMilli(), 10) + "-sqs"}
	}
	recRead := r.recBegin.Add(300 * time.Microsecond)
	for _, client := range []bool{false, true} {
		for _, wr := range r.recordings() {
			for _, c := range r.reads() {
				if text(c.v).Value == text(recRead).Value {
					continue // no drift: the read at the move's end writes the recorded cut
				}
				ln, tr := r.learnUpsert(t, client, wr, p, text(recRead), text(c.v))
				if !client {
					if ln.Kind != KindNoise || ln.Read != nil {
						t.Errorf("server test, %s: %s (%s): learned %s (%s); want noise of the word", wr.what, c.what, text(c.v).Value, ln.Kind, ln.Why)
					}
					continue
				}
				learned := ln.Kind == KindClockRead && ln.Read != nil
				if learned != c.ok {
					t.Errorf("client test, %s: %s (%s): learned %s (%s); want a read of the clock %v", wr.what, c.what, text(c.v).Value, ln.Kind, ln.Why, c.ok)
					continue
				}
				if !learned {
					continue
				}
				if ln.Read.Word.IsZero() {
					t.Errorf("client test, %s: %s: learned %s of the whole text; want a read of its one word", wr.what, c.what, ln.Read)
				}
				s, ok := ClockSpanFrom(WithTracker(context.Background(), tr), r.upTo)
				if ok, why := CheckClockRead(p, *ln.Read, text(c.v), s, ok); !ok {
					t.Errorf("client test, %s: %s (%s): learned as %s, which the replay's check refuses: %s", wr.what, c.what, text(c.v).Value, ln.Read, why)
				}
			}
		}
	}
}

// keploy test gives a server test's recorded begin with its test case's
// request, after the test began awaiting it (RecordedTestBegin): a call's
// window in the recording then carries it where a call recorded after it
// starts the span. One given before the test began (RecordedBegin) stands,
// and so does the first given; one given while no test is open is dropped,
// a call made once its test ended reads none, and the next test begins
// with none.
func TestATestsRecordedBeginGivenWithItsRequestBoundsItsWindowInTheRecording(t *testing.T) {
	r := newConcurrentRun()
	paired := &models.Mock{Name: "mock-5", Kind: models.Mongo}
	paired.Spec.ReqTimestampMock = r.recBegin.Add(2 * time.Millisecond)
	prevRec := r.recBegin.Add(1200 * time.Microsecond)
	window := func(tr *Tracker) time.Time {
		t.Helper()
		w := tr.CallWindowFrom(prevRec, paired, r.upTo)
		if w == nil {
			t.Fatal("no window for the upsert")
		}
		return w.Rec.TestFrom
	}
	server := func(told func(tr *Tracker)) *Tracker {
		tr := r.tracker(false)
		told(tr)
		return tr
	}
	other := r.recBegin.Add(-time.Millisecond)
	for _, c := range []struct {
		what string
		tr   *Tracker
		want time.Time
	}{
		{"given with the request", server(func(tr *Tracker) { tr.RecordedTestBegin(r.recBegin) }), r.recBegin},
		{"none given", server(func(tr *Tracker) {}), time.Time{}},
		{"given twice", server(func(tr *Tracker) { tr.RecordedTestBegin(r.recBegin); tr.RecordedTestBegin(other) }), r.recBegin},
		{"given before the test began", r.windowTracker(false), r.recBegin},
		{"given before, then with the request", func() *Tracker { tr := r.windowTracker(false); tr.RecordedTestBegin(other); return tr }(), r.recBegin},
		{"given after the test ended", server(func(tr *Tracker) { tr.End(); tr.RecordedTestBegin(r.recBegin) }), time.Time{}},
		{"the test ended after it was given", server(func(tr *Tracker) { tr.RecordedTestBegin(r.recBegin); tr.End() }), time.Time{}},
		{"given to the previous test", server(func(tr *Tracker) { tr.RecordedTestBegin(r.recBegin); tr.BeginAt(r.a) }), time.Time{}},
		{"a recorded begin after the previous recorded call", server(func(tr *Tracker) { tr.RecordedTestBegin(prevRec.Add(time.Microsecond)) }), time.Time{}},
	} {
		if got := window(c.tr); !got.Equal(c.want) {
			t.Errorf("%s: the recording's span carries the test's begin %v; want %v", c.what, got, c.want)
		}
	}
}
