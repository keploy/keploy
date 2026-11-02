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
// test begins, GiveRequest with a keploy test case's request), so a
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
// request, after the test began awaiting it (GiveRequest): a call's
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
	// give hands tr the test case's request, recorded at at.
	give := func(tr *Tracker, at time.Time) { tr.GiveRequest(&models.TestRequest{Test: "test-2", RecordedAt: at}) }
	for _, c := range []struct {
		what string
		tr   *Tracker
		want time.Time
	}{
		{"given with the request", server(func(tr *Tracker) { give(tr, r.recBegin) }), r.recBegin},
		{"none given", server(func(tr *Tracker) {}), time.Time{}},
		{"given twice", server(func(tr *Tracker) { give(tr, r.recBegin); give(tr, other) }), r.recBegin},
		{"given before the test began", r.windowTracker(false), r.recBegin},
		{"given before, then with the request", func() *Tracker { tr := r.windowTracker(false); give(tr, other); return tr }(), r.recBegin},
		{"given after the test ended", server(func(tr *Tracker) { tr.End(); give(tr, r.recBegin) }), time.Time{}},
		{"the test ended after it was given", server(func(tr *Tracker) { give(tr, r.recBegin); tr.End() }), time.Time{}},
		{"given to the previous test", server(func(tr *Tracker) { give(tr, r.recBegin); tr.BeginAt(r.a) }), time.Time{}},
		{"a recorded begin after the previous recorded call", server(func(tr *Tracker) { give(tr, prevRec.Add(time.Microsecond)) }), time.Time{}},
	} {
		if got := window(c.tr); !got.Equal(c.want) {
			t.Errorf("%s: the recording's span carries the test's begin %v; want %v", c.what, got, c.want)
		}
	}
	// A client test's window in the recording starts at its test's recorded
	// begin where that came after the previous recorded call: one given once
	// the test ended is dropped there too, so a call made after the test
	// reads the recording's span from the previous recorded call.
	ct := r.tracker(true)
	ct.End()
	give(ct, prevRec.Add(time.Microsecond))
	if w := ct.CallWindowFrom(prevRec, paired, r.upTo); w == nil || !w.Rec.From.Equal(prevRec) || !w.Rec.Began.IsZero() {
		t.Errorf("a client test's call once its test ended, a recorded begin given then: window %+v; want the recording's span from the previous recorded call, with no begin", w)
	}
}

// A client test's learn step reads a plain value its call's window refuses
// from each run's test begin, in both runs at once (Window.sinceBegin): a
// server test's window widened from its test's begin
// (Window.fromTestBegin), which keeps the full round's span from a serve
// made before the test began, is not a client test's. On a clock that ran
// as one from a serve made before its test began, a value the full round
// read before its test began, recorded inside its test before the call
// recorded before the upsert, is refused; read after its test began, the
// same recorded value is learned as a read since its test began.
func TestAClientTestsWindowTakesNoServerTestsSpanFromItsBegin(t *testing.T) {
	base := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	recBegin := time.Date(2026, 10, 31, 8, 53, 33, 933_485_077, time.UTC)
	clock := &runClock{}
	tr := NewTracker()
	tr.SetClientTests(true)
	tr.SetAppClock(clock.at)
	tr.SetAppClockRun(clock.began)
	tr.NewSetAt(base.Add(-time.Second))
	tr.ServedMockAt(&models.Mock{Name: "mock-4", Kind: models.HTTP}, base)
	tr.RecordedBegin(recBegin)
	tr.BeginAt(base.Add(5 * time.Millisecond))
	paired := &models.Mock{Name: "mock-5", Kind: models.Mongo}
	paired.Spec.ReqTimestampMock = recBegin.Add(4 * time.Millisecond)
	w := tr.CallWindowFrom(recBegin.Add(2*time.Millisecond), paired, base.Add(8*time.Millisecond))
	if w == nil {
		t.Fatal("no window for the upsert")
	}
	if !w.F.From.Equal(base) || !w.F.Began.Equal(base.Add(5*time.Millisecond)) || !w.Rec.From.Equal(recBegin.Add(2*time.Millisecond)) || !w.Rec.Began.Equal(recBegin) {
		t.Fatalf("window %+v; want the full round's span from a serve made before its test's begin, and the recording's from the call recorded before the upsert, after its test's", w)
	}
	const p = "body.docs.msgs[].u.{}.created"
	at := func(v time.Time) Leaf { return InstantLeaf(p, p, v, time.Millisecond) }
	rec := at(recBegin.Add(time.Millisecond))
	for _, c := range []struct {
		what string
		live time.Time
		ok   bool
	}{
		{"read before its test began", base.Add(2 * time.Millisecond), false},
		{"read after its test began", base.Add(6 * time.Millisecond), true},
	} {
		dc := clockedDrifts()
		dc.clientTests = true
		ln := ClassifyIn(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: at(c.live)}}, Shape{Leaves: map[string]Leaf{p: rec}}, paired.Spec.ReqTimestampMock, nil, nil, nil, w)[0]
		learned := ln.Kind == KindClockRead && ln.Read != nil
		if learned != c.ok || !c.ok && ln.Kind != KindUnaccounted {
			t.Errorf("a client test's value %s (%s, recorded %s): learned %s (%s); want a read of the clock %v, else a refusal", c.what, at(c.live).Value, rec.Value, ln.Kind, ln.Why, c.ok)
		} else if learned && ln.Read.Since != models.SinceTestBegin {
			t.Errorf("a client test's value %s (%s, recorded %s): learned %s; want a read since its test began", c.what, at(c.live).Value, rec.Value, ln.Read)
		}
	}
}

// A value its server test's call's window learns as anything but a refusal
// keeps what that window learned: the learn step reads the window from the
// test's begin (Window.fromTestBegin) only for a value the call's window
// refuses, and only for a plain read there. In a test that ran two minutes
// before another goroutine's call was served, the handler's created, read
// about a second after the test began in each run, reached keploy past that
// serve: the call's window, which compares offsets, learns it as a read
// made two minutes before the call (a cutoff such as createdAt >= now -
// 2m), and the window from the test's begin holds it as a plain read. It is
// learned as the offset, checked so at every replay, never as the plain
// read. A copy of what the consumer's call was answered, the moment in both
// runs, is learned as that call's variable, as the call's window learns it.
func TestAServerTestsCallKeepsWhatItsWindowLearnedPastAnotherGoroutinesServes(t *testing.T) {
	recBegin := time.Date(2026, 10, 31, 8, 50, 0, 0, time.UTC)
	began := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, true })
	tr.NewSetAt(began.Add(-time.Minute))
	tr.ServedMockAt(&models.Mock{Name: "mock-4", Kind: models.HTTP}, began.Add(-time.Second))
	tr.RecordedBegin(recBegin)
	tr.BeginAt(began)
	serve := began.Add(2*time.Minute + 600*time.Millisecond)
	tr.ServedMockAt(&models.Mock{Name: "mock-7", Kind: models.Mongo}, serve)
	paired := &models.Mock{Name: "mock-5", Kind: models.Mongo}
	paired.Spec.ReqTimestampMock = recBegin.Add(2*time.Minute + 1500*time.Millisecond)
	prevRec := recBegin.Add(2*time.Minute + 500*time.Millisecond)
	arrival := began.Add(2*time.Minute + 1400*time.Millisecond)
	w := tr.CallWindowFrom(prevRec, paired, arrival)
	if w == nil {
		t.Fatal("no window for the upsert")
	}
	if !w.F.From.Equal(serve) || !w.F.TestFrom.Equal(began) || !w.Rec.From.Equal(prevRec) || !w.Rec.TestFrom.Equal(recBegin) {
		t.Fatalf("window %+v; want each run's span from the consumer's call, carrying its test's begin", w)
	}
	w = w.ComparesOffsets()
	const p = "body.docs.msgs[].u.{}.created"
	at := func(v time.Time) Leaf { return InstantLeaf(p, p, v, time.Millisecond) }
	rec, live := at(recBegin.Add(1037*time.Millisecond)), at(began.Add(789*time.Millisecond))
	learn := func(earlier []Earlier, w *Window) Learned {
		return ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: live}}, Shape{Leaves: map[string]Leaf{p: rec}}, paired.Spec.ReqTimestampMock, earlier, nil, nil, w)[0]
	}
	if wide := learn(nil, w.fromTestBegin()); wide.Kind != KindClockRead || wide.Read == nil {
		t.Fatalf("the window from the test's begin learned %s (%s); want a plain read of the clock, which the call's window must not take", wide.Kind, wide.Why)
	}
	ln := learn(nil, w)
	if ln.Kind != KindClockOffset || ln.Offset == nil || ln.Offset.Min > -2*time.Minute || ln.Offset.Max <= -2*time.Minute {
		t.Errorf("the handler's created read about a second after the test began: learned %s (%s, offset %v); want the read two minutes before the call its window learns", ln.Kind, ln.Why, ln.Offset)
	}
	consumer := []Earlier{{Mock: "mock-7", At: prevRec, Own: true, Seen: []Seen{{Side: Response, Leaf: rec, Live: live.Value, Place: p}}}}
	if ln := learn(consumer, w); ln.Kind != KindVariable || ln.From.Mock != "mock-7" {
		t.Errorf("a copy of what the consumer's call was answered: learned %s (%s, from %+v); want that call's variable", ln.Kind, ln.Why, ln.From)
	}
}

// A server test's call that waits for its test case's request
// (AwaitRequest) is released by GiveRequest, and reads its window in the
// recording with the test's recorded begin the request carries. A request
// with no recorded time releases it with none. GiveRequest takes the begin
// in the same hold of the tracker's lock that releases the call; no row
// pins that order within the hold, which would need a hook between the two.
func TestACallTheRequestReleasesReadsItsWindowWithItsRecordedBegin(t *testing.T) {
	r := newConcurrentRun()
	paired := &models.Mock{Name: "mock-5", Kind: models.Mongo}
	paired.Spec.ReqTimestampMock = r.recBegin.Add(2 * time.Millisecond)
	prevRec := r.recBegin.Add(1200 * time.Microsecond)
	for _, c := range []struct {
		what string
		req  *models.TestRequest
		want time.Time
	}{
		{"a request recorded at the test's begin", &models.TestRequest{Test: "test-2", RecordedAt: r.recBegin}, r.recBegin},
		{"a request with no recorded time", &models.TestRequest{Test: "test-2"}, time.Time{}},
	} {
		tr := r.tracker(false)
		waiting, got := make(chan struct{}, 1), make(chan *Window, 1)
		tr.OnAwait(func() { waiting <- struct{}{} })
		tr.Await()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		go func() {
			if err := tr.AwaitRequest(ctx); err != nil {
				got <- nil
				return
			}
			got <- tr.CallWindowFrom(prevRec, paired, r.upTo)
		}()
		select {
		case <-waiting:
		case w := <-got:
			cancel()
			t.Fatalf("%s: the call did not wait for the request (window %+v)", c.what, w)
		}
		tr.GiveRequest(c.req)
		w := <-got
		cancel()
		switch {
		case w == nil:
			t.Errorf("%s: the call awaiting it was not released, or read no window", c.what)
		case !w.Rec.TestFrom.Equal(c.want):
			t.Errorf("%s: the released call's window in the recording carries the test's begin %v; want %v", c.what, w.Rec.TestFrom, c.want)
		}
	}
}
