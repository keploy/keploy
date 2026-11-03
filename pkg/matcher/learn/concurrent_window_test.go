package learn

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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

// serverStampRun is one run of a server test whose handler read its clock
// for a .NET stamp (DateTime.UtcNow.ToString("O")) about a second after the
// test began, after which another goroutine's call was served (prev) before
// the handler's call reached keploy (call).
type serverStampRun struct{ began, read, prev, call time.Time }

// serverStampWindow is the handler's call's window in rec and in record's
// full round f, each carrying where its test began (Span.TestFrom), as a
// server test's window does.
func serverStampWindow(rec, f serverStampRun) *Window {
	return &Window{
		Rec: Span{From: rec.prev, To: rec.call, TestFrom: rec.began},
		F:   Span{From: f.prev, To: f.call, TestFrom: f.began},
	}
}

// serverStampRuns are the recording and record's full round of a server
// test whose handler read its clock for a .NET stamp a second after the test
// began, after which another goroutine's call was served before the
// handler's call reached keploy (serverStampRun). The timings are
// illustrative.
func serverStampRuns() (rec, f serverStampRun) {
	rec = serverStampRun{
		began: time.Date(2026, 11, 2, 12, 48, 54, 330_000_000, time.UTC),
		read:  time.Date(2026, 11, 2, 12, 48, 55, 350_537_100, time.UTC),
		prev:  time.Date(2026, 11, 2, 12, 48, 55, 423_433_000, time.UTC),
		call:  time.Date(2026, 11, 2, 12, 48, 55, 606_243_000, time.UTC),
	}
	f = serverStampRun{
		began: time.Date(2026, 11, 2, 12, 49, 57, 215_000_000, time.UTC),
		read:  time.Date(2026, 11, 2, 12, 49, 58, 275_116_400, time.UTC),
		prev:  time.Date(2026, 11, 2, 12, 49, 58, 391_020_000, time.UTC),
		call:  time.Date(2026, 11, 2, 12, 49, 58, 570_310_000, time.UTC),
	}
	return rec, f
}

// dotNetStamp is v as .NET writes DateTime.UtcNow.ToString("O").
func dotNetStamp(v time.Time) string { return v.UTC().Format("2006-01-02T15:04:05.0000000Z") }

// startUpLookUp is the Pulsar topic look-up the application's start-up made
// three seconds before rec's test began, as recorded: an opaque payload no
// call of the test binds.
func startUpLookUp(rec serverStampRun) []Raw {
	at := rec.began.Add(-3 * time.Second)
	return []Raw{RawOf(&models.Mock{Name: "mock-1", Kind: "Pulsar", Spec: models.MockSpec{
		Metadata:         map[string]string{"commandType": "LOOKUP", "type": "config"},
		GenericRequests:  []models.Payload{{Origin: "client", Message: []models.OutputBinary{{Type: "json", Data: `{"commandType":"LOOKUP","requestId":0,"topic":"persistent://public/default/lead-events","body":{"authoritative":false,"topic":"persistent://public/default/lead-events"}}`}}}},
		GenericResponses: []models.Payload{{Origin: "server", Message: []models.OutputBinary{{Type: "json", Data: `{"commandType":"LOOKUP_RESPONSE","requestId":0,"body":{"authoritative":true,"brokerServiceUrl":"pulsar://localhost:6650","proxyThroughServiceUrl":true,"response":"Connect"}}`}}}},
		ReqTimestampMock: at, ResTimestampMock: at.Add(211 * time.Millisecond),
	}}, false)}
}

// A server test's clock read made before another goroutine's call is
// learned the same way whatever digits the clock gave it: as a plain read
// of the clock that its window from the test's begin holds, which the
// replay's check passes in the call's span carrying that begin. Where only
// its call's window is known, that window shows no read, so the value's
// changed words are searched as data, and whether one of them, or a decoded
// or case-folded form of one, stands as a token in an opaque payload is
// chance: "…55.3505371Z" ends in
// "3505371Z", which decodes as base64 to a text whose word, case-folded, is
// "y", a token a start-up Pulsar look-up's JSON holds; "…55.3805371Z"
// decodes to no text. Read as a variable of the look-up, which no call can
// bind, the first would be refused, so the same test's call would be kept
// in one record and refused in the next. Where the window from the test's
// begin does not hold the read (the recording's read made before its test
// began, or its begin unknown there), the part found stays the value's
// place. So does a part an earlier call F served field by field drifted
// with, and a payload that holds the value whole (the application sent the
// stamp in a Pulsar message before the call) stays its source, as the copy
// rules read it, even where the window from the test's begin sets that
// payload aside (under the proven flow, a payload F had not served before
// the call is no source of a read that window holds).
func TestAServerTestsReadPastAnotherGoroutinesServeIsLearnedWhateverPartAPayloadHoldsByChance(t *testing.T) {
	recRun, fRun := serverStampRuns()
	lookUp := startUpLookUp(recRun)
	stamp := dotNetStamp
	const p = "body.Item.CreatedAt.S"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	live := stamp(fRun.read)
	learnAfter := func(earlier []Earlier, rec time.Time, w *Window, raw []Raw) Learned {
		return ClassifyIn(clockedDrifts(), []string{p}, str(live), str(stamp(rec)), recRun.call, earlier, raw, nil, w)[0]
	}
	learn := func(rec time.Time, w *Window, raw []Raw) Learned { return learnAfter(nil, rec, w, raw) }
	w := serverStampWindow(recRun, fRun)
	if ln := learn(recRun.read, &Window{Rec: Span{From: recRun.prev, To: recRun.call}, F: Span{From: fRun.prev, To: fRun.call}}, lookUp); !foundAsAPart(ln) || ln.From.Mock != "mock-1" {
		t.Fatalf("the call's window alone: %s from %q held %q (%s); want a part of the value the look-up holds", ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
	}
	// The recording's read in its call's window (no serve came between them
	// there) and its test's begin not known, record's full round's read
	// before another goroutine's serve: the window from the test's begin
	// moves record's full round's span alone, and holds the read in both.
	recInCall := serverStampWindow(recRun, fRun)
	recInCall.Rec = Span{From: recRun.read.Add(-10 * time.Millisecond), To: recRun.call}
	for _, c := range []struct {
		what string
		rec  time.Time
		raw  []Raw
		w    *Window
	}{
		{"a decoded word of it stands in the start-up look-up", recRun.read, lookUp, w},
		{"no word of it stands in the look-up", recRun.read.Add(30 * time.Millisecond), lookUp, w},
		{"no payload", recRun.read, nil, w},
		{"the recording's read in its call's window, its begin unknown, the look-up", recRun.read, lookUp, recInCall},
		{"the recording's read in its call's window, its begin unknown, no payload", recRun.read, nil, recInCall},
	} {
		ln := learn(c.rec, c.w, c.raw)
		if ln.Kind != KindClockRead || ln.Read == nil || ln.Read.Since == models.SinceTestBegin {
			t.Errorf("%s: %s from %q held %q (%s); want a plain read of the clock checked in its call's span", c.what, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
			continue
		}
		s := Span{From: fRun.prev, To: fRun.call, TestFrom: fRun.began}
		if ok, why := CheckClockRead(p, *ln.Read, str(live).Leaves[p], s, true); !ok {
			t.Errorf("%s: learned as %s, which the replay's check refuses: %s", c.what, ln.Read, why)
		}
	}
	early := recRun
	early.began = recRun.read.Add(50 * time.Millisecond) // after the read, before the other goroutine's serve
	fOnly := serverStampWindow(recRun, fRun)
	fOnly.Rec.TestFrom = time.Time{}
	for _, c := range []struct {
		what string
		w    *Window
	}{
		{"the recording's read made before its test began", serverStampWindow(early, fRun)},
		{"the test's begin known in record's full round alone", fOnly},
	} {
		if ln := learn(recRun.read, c.w, lookUp); !foundAsAPart(ln) || ln.From.Mock != "mock-1" {
			t.Errorf("%s: %s from %q held %q Read=%+v (%s); want the look-up's part to stay the value's place", c.what, ln.Kind, ln.From.Mock, ln.From.Held, ln.Read, ln.Why)
		}
	}
	// An earlier call of the test that F served field by field (its
	// adapter gives its payload), whose field drifted from the value's
	// minute to F's (the application sent its clock's minute there): the
	// minute is a word of a read the window from the test's begin holds, so
	// the value is learned as it is where no serve came between the read and
	// its call, a plain read checked in its call's span. Where that window
	// does not hold the read, the field F saw move with it stays its place.
	minute := []Earlier{{Mock: "mock-8", At: recRun.prev, Payload: payloadOf("mock-8", recRun.prev), Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.minute", Noise: "body.minute", Type: String, Value: "48"}, Live: "49", Place: "body.minute"},
	}}}
	inSpan := &Window{Rec: Span{From: recRun.began, To: recRun.call}, F: Span{From: fRun.began, To: fRun.call}}
	for _, mw := range []*Window{w, inSpan} {
		if ln := learnAfter(minute, recRun.read, mw, nil); ln.Kind != KindClockRead || ln.Read == nil || ln.From.Mock != "" {
			t.Errorf("a served call's field that drifted with it, window %+v: %s from %q %s whole=%v Read=%+v (%s); want a plain read of the clock", *mw, ln.Kind, ln.From.Mock, ln.From.Side, ln.From.Whole, ln.Read, ln.Why)
		}
	}
	if ln := learnAfter(minute, recRun.read, serverStampWindow(early, fRun), nil); ln.Kind != KindVariable || ln.From.Mock != "mock-8" || ln.From.Side != Request || ln.From.Whole {
		t.Errorf("a served call's field that drifted with it, the recording's read made before its test began: %s from %q %s whole=%v Read=%+v (%s); want the variable of mock-8's request, a part", ln.Kind, ln.From.Mock, ln.From.Side, ln.From.Whole, ln.Read, ln.Why)
	}
	sent := func(served bool) []Raw {
		at := recRun.read.Add(40 * time.Millisecond)
		return []Raw{RawOf(&models.Mock{Name: "mock-9", Kind: "Pulsar", Spec: models.MockSpec{
			GenericRequests:  []models.Payload{{Origin: "client", Message: []models.OutputBinary{{Type: "json", Data: `{"commandType":"SEND","payload":{"type":"lead.created","at":"` + stamp(recRun.read) + `"}}`}}}},
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		}}, served)}
	}
	for _, c := range []struct {
		what   string
		raw    []Raw
		proven bool
	}{
		{"sent whole in a payload F served before the call", sent(true), false},
		{"sent whole in a payload F had not served before the call", sent(false), false},
		{"the same, under the proven flow", sent(false), true},
		{"served before the call, under the proven flow", sent(true), true},
		{"sent whole in a payload F had not served, after the look-up's chance part", append(append([]Raw{}, lookUp...), sent(false)...), false},
		{"sent whole in a payload F had not served, after the look-up's chance part, under the proven flow", append(append([]Raw{}, lookUp...), sent(false)...), true},
		{"sent whole in a payload F served, after the look-up's chance part", append(append([]Raw{}, lookUp...), sent(true)...), false},
		{"sent whole in a payload F served, after the look-up's chance part, under the proven flow", append(append([]Raw{}, lookUp...), sent(true)...), true},
	} {
		t.Run(c.what, func(t *testing.T) {
			if c.proven {
				t.Setenv(provenswitch.Env, "1")
			}
			if ln := learn(recRun.read, w, c.raw); ln.Kind != KindVariable || ln.From.Mock != "mock-9" || ln.From.Held != stamp(recRun.read) {
				t.Fatalf("%s from %q held %q Read=%+v (%s); want the variable of mock-9 holding the stamp whole", ln.Kind, ln.From.Mock, ln.From.Held, ln.Read, ln.Why)
			}
		})
	}
	// Fail closed: only a payload that holds the value whole (or its echo:
	// the next row), made or served before the call, outranks the look-up's
	// part. One that holds another stamp of the same minute (a part again),
	// or the value whole but recorded after the call and not served before
	// it, leaves the earliest part the value's place: where the window from
	// the test's begin does not hold the read, its variable stands, and where
	// it does, the value is learned as the plain read the replay's check
	// passes, as with the look-up alone.
	other := func(v string, at time.Time, served bool) Raw {
		return RawOf(&models.Mock{Name: "mock-10", Kind: "Pulsar", Spec: models.MockSpec{
			GenericRequests:  []models.Payload{{Origin: "client", Message: []models.OutputBinary{{Type: "json", Data: `{"commandType":"SEND","payload":{"type":"lead.updated","at":"` + v + `"}}`}}}},
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		}}, served)
	}
	for _, c := range []struct {
		what string
		x    Raw
	}{
		{"another stamp of the minute", other(stamp(recRun.read.Add(-2*time.Second)), recRun.read.Add(40*time.Millisecond), true)},
		{"the stamp whole, recorded after the call, not served before it", other(stamp(recRun.read), recRun.call.Add(5*time.Millisecond), false)},
	} {
		for _, proven := range []bool{false, true} {
			t.Run(c.what+", proven "+strconv.FormatBool(proven), func(t *testing.T) {
				if proven {
					t.Setenv(provenswitch.Env, "1")
				}
				raw := append(append([]Raw{}, lookUp...), c.x)
				if ln := learn(recRun.read, serverStampWindow(early, fRun), raw); !foundAsAPart(ln) || ln.From.Mock != "mock-1" {
					t.Fatalf("%s from %q held %q Read=%+v (%s); want the look-up's part to stay the value's place", ln.Kind, ln.From.Mock, ln.From.Held, ln.Read, ln.Why)
				}
				ln := learn(recRun.read, w, raw)
				if ln.Kind != KindClockRead || ln.Read == nil || ln.Read.Since == models.SinceTestBegin {
					t.Fatalf("its test's begin known: %s from %q held %q (%s); want a plain read of the clock", ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
				}
				if ok, why := CheckClockRead(p, *ln.Read, str(live).Leaves[p], Span{From: fRun.prev, To: fRun.call, TestFrom: fRun.began}, true); !ok {
					t.Fatalf("its test's begin known: learned as %s, which the replay's check refuses: %s", ln.Read, why)
				}
			})
		}
	}
}

// A server test's clock read written as one word of a text ("lead <.NET
// stamp> created") is noise of that word whatever another goroutine served
// between the read and its call, and whatever digits the clock gave it, as
// it is where no serve came between them: the replay's check passes a read
// in its call's span from the test's begin, so the learn step reads which
// words of the value are the clock's in that window too, and none of them
// is read as data. Read as data, the stamp's fraction "3505371Z" decodes as
// base64 to a text whose word, case-folded, is "y", a token the start-up
// Pulsar look-up holds, so the text was the look-up's variable, which no call
// can bind; with no payload, the decoded text, of a script keploy does not
// split into words, was refused; a read 30 ms later decodes to no text and
// was noise. A read the window from the test's begin does not hold (made
// before the test began, or the recording's begin unknown) keeps today's
// answer, and an all-ASCII text is never refused for text of a script it
// does not hold: the reason names the decoded text.
func TestAServerTestsStampWordOfATextPastAnotherGoroutinesServeIsNoiseOfThatWord(t *testing.T) {
	recRun, fRun := serverStampRuns()
	lookUp := startUpLookUp(recRun)
	const p = "body.Item.Note.S"
	text := func(v time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "lead " + dotNetStamp(v) + " created"}}}
	}
	learn := func(rec time.Time, w *Window, raw []Raw) Learned {
		return ClassifyIn(clockedDrifts(), []string{p}, text(fRun.read), text(rec), recRun.call, nil, raw, nil, w)[0]
	}
	inSpan := &Window{Rec: Span{From: recRun.began, To: recRun.call}, F: Span{From: fRun.began, To: fRun.call}}
	// The text percent-escaped ("lead%202026-11-02T12%3A48%3A55.3505371Z%20created"
	// in a query): its decoded form's words are read the same way.
	escaped := func(v time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: strings.NewReplacer(" ", "%20", ":", "%3A").Replace("lead " + dotNetStamp(v) + " created")}}}
	}
	for _, rec := range []time.Time{recRun.read, recRun.read.Add(30 * time.Millisecond)} {
		for _, raw := range [][]Raw{lookUp, nil} {
			for _, w := range []*Window{serverStampWindow(recRun, fRun), inSpan} {
				ln := ClassifyIn(clockedDrifts(), []string{p}, escaped(fRun.read), escaped(rec), recRun.call, nil, raw, nil, w)[0]
				if ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
					t.Errorf("escaped, read at %s, look-up %v, window %+v: %s from %q held %q (%s); want noise of the word", rec.Format("05.000000"), raw != nil, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
				}
			}
		}
	}
	// The text base64-encoded in a token ("tok <base64 of the text> ok"):
	// the words of its encoded run's text are read in the window from the
	// test's begin too, and the clock read written there is read as the
	// clock it is before that text's own runs are decoded again (b64Pair),
	// so neither another goroutine's serve between the read and its call
	// nor the digits the clock gave it changes the answer: noise of the
	// word, under the proven flow too. On main, and before the decoded text
	// was read for clock words, the stamp's fraction there was decoded as
	// data, so its digits decided: "3505371Z" and "3609071Z" decode to text
	// whose word, case-folded, is a token the look-up holds (its part), with
	// no payload a text of a script keploy does not split into words
	// (refused), "3510556Z" to such text with the look-up too (refused), and
	// "3805371Z" to no text (noise); so did record's full round's: its read
	// "2754275Z" decodes to such text (refused).
	encodedText := func(v time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: "tok " + base64.StdEncoding.EncodeToString([]byte("lead "+dotNetStamp(v)+" created")) + " ok"}}}
	}
	learnEncodedOf := func(dc *DriftCache, rec, f time.Time, w *Window, raw []Raw) Learned {
		return ClassifyIn(dc, []string{p}, encodedText(f), encodedText(rec), recRun.call, nil, raw, nil, w)[0]
	}
	learnEncoded := func(dc *DriftCache, rec time.Time, w *Window, raw []Raw) Learned {
		return learnEncodedOf(dc, rec, fRun.read, w, raw)
	}
	// Each read's lag after the recording's read and record's full round's.
	layouts := []struct{ rec, f time.Duration }{
		{0, 0},
		{518_500 * time.Nanosecond, 0},
		{10_370 * time.Microsecond, 0},
		{30 * time.Millisecond, 0},
		{30 * time.Millisecond, 311_100 * time.Nanosecond},
	}
	for _, proven := range []bool{false, true} {
		t.Run("encoded, proven "+strconv.FormatBool(proven), func(t *testing.T) {
			if proven {
				t.Setenv(provenswitch.Env, "1")
			}
			for _, d := range layouts {
				rec, f := recRun.read.Add(d.rec), fRun.read.Add(d.f)
				// The stamp the text carries, sent whole by the application
				// in a payload after the read (a SEND record's full round
				// served, or had not served, before the call), with the
				// start-up look-up or without it.
				sent := func(served bool) Raw {
					at := rec.Add(40 * time.Millisecond)
					return RawOf(&models.Mock{Name: "mock-9", Kind: "Pulsar", Spec: models.MockSpec{
						GenericRequests:  []models.Payload{{Origin: "client", Message: []models.OutputBinary{{Type: "json", Data: `{"commandType":"SEND","payload":{"type":"lead.created","at":"` + dotNetStamp(rec) + `"}}`}}}},
						ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
					}}, served)
				}
				// The stamp sent in a payload record's full round served
				// whose adapter stated no view of what it gave, or one it
				// served changed with a view keploy could not read, and an
				// answer so served, of the call's own test or another
				// test's: the encoded run is a changed word, yet one read as
				// the text it decodes to, so no such payload may hold it, as
				// the same text unencoded holds none: whether another
				// goroutine's call was served before this one does not
				// change the answer.
				for _, rc := range []struct {
					what string
					raw  []Raw
				}{
					{"the look-up", lookUp},
					{"no payload", nil},
					{"the stamp sent, not served", []Raw{sent(false)}},
					{"the stamp sent, served", []Raw{sent(true)}},
					{"the look-up and the stamp sent, served", append(append([]Raw{}, lookUp...), sent(true))},
					{"the stamp sent, served changed with a view keploy could not read", []Raw{sent(true).ServedChanged()}},
					{"an answer served", []Raw{answerServed(rec, false)}},
					{"an answer served changed with a view keploy could not read", []Raw{answerServed(rec, false).ServedChanged()}},
					{"another test's answer served", []Raw{answerServed(rec, true)}},
					{"another test's answer served changed with a view keploy could not read", []Raw{answerServed(rec, true).ServedChanged()}},
				} {
					for _, w := range []*Window{serverStampWindow(recRun, fRun), inSpan} {
						if ln := learnEncodedOf(clockedDrifts(), rec, f, w, rc.raw); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
							t.Errorf("encoded, read at %s and in record's full round at %s, %s, window %+v: %s from %q held %q (%s); want noise of the word", rec.Format("05.0000000"), f.Format("05.0000000"), rc.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
						}
					}
				}
			}
			// Fail closed where no window of the value holds the read: made
			// after its call (no read of the clock for it), made before the
			// test began, its recording's begin unknown, the call's window
			// alone, or a client test's call's window, which carries no
			// test's begin and whose read made before another goroutine's
			// serve is no read in it. Its digits are decoded as data, as on
			// main: the look-up's part, or a refusal of the decoded text.
			late := recRun
			late.began = recRun.read.Add(50 * time.Millisecond) // after the read, before the other goroutine's serve
			unknownRec := serverStampWindow(recRun, fRun)
			unknownRec.Rec.TestFrom = time.Time{}
			callOnly := &Window{Rec: Span{From: recRun.prev, To: recRun.call}, F: Span{From: fRun.prev, To: fRun.call}}
			clientDrifts := func() *DriftCache {
				dc := clockedDrifts()
				dc.clientTests = true
				return dc
			}
			for _, c := range []struct {
				what string
				dc   func() *DriftCache
				rec  time.Duration
				w    *Window
				part bool
			}{
				{"read after its call", clockedDrifts, 291_708_100 * time.Nanosecond, serverStampWindow(recRun, fRun), false},
				{"read after its call, no serve between", clockedDrifts, 291_708_100 * time.Nanosecond, inSpan, false},
				{"read before its test began", clockedDrifts, 0, serverStampWindow(late, fRun), true},
				{"the recording's begin unknown", clockedDrifts, 0, unknownRec, true},
				{"the call's window alone", clockedDrifts, 0, callOnly, true},
				{"a client test's call's window", clientDrifts, 0, callOnly, true},
			} {
				rec := recRun.read.Add(c.rec)
				ln := learnEncoded(c.dc(), rec, c.w, lookUp)
				switch {
				case c.part && (!foundAsAPart(ln) || ln.From.Mock != "mock-1"):
					t.Errorf("%s, read at %s, the look-up: %s from %q held %q (%s); want the look-up's part to stay the value's place", c.what, rec.Format("05.0000000"), ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
				case !c.part && (ln.Kind != KindUnaccounted || ln.Why != encodedScriptWhy):
					t.Errorf("%s, read at %s, the look-up: %s from %q (%s); want it refused as %q", c.what, rec.Format("05.0000000"), ln.Kind, ln.From.Mock, ln.Why, encodedScriptWhy)
				}
				if ln := learnEncoded(c.dc(), rec, c.w, nil); ln.Kind != KindUnaccounted || ln.Why != encodedScriptWhy {
					t.Errorf("%s, read at %s, no payload: %s (%s); want it refused as %q", c.what, rec.Format("05.0000000"), ln.Kind, ln.Why, encodedScriptWhy)
				}
			}
		})
	}
	for _, c := range []struct {
		what string
		rec  time.Time
		raw  []Raw
	}{
		{"a decoded word of it stands in the start-up look-up", recRun.read, lookUp},
		{"no payload", recRun.read, nil},
		{"no word of it stands in the look-up (read 30 ms later)", recRun.read.Add(30 * time.Millisecond), lookUp},
	} {
		for _, wc := range []struct {
			what string
			w    *Window
		}{
			{"another goroutine served between the read and its call", serverStampWindow(recRun, fRun)},
			{"no serve between the read and its call", inSpan},
		} {
			if ln := learn(c.rec, wc.w, c.raw); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
				t.Errorf("%s, %s: %s from %q held %q (%s); want noise of the word", c.what, wc.what, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
			}
		}
	}
	// The stamp the text carries, sent by the application earlier in the
	// test in a payload (a SEND F served, or had not served, before the
	// call), with the start-up look-up or without it: the text gets the
	// answer it gets where no serve came between the read and its call,
	// noise of the word, under the proven flow too. Where the window from
	// the test's begin does not hold the read, a part found in a payload
	// stays the value's place, as before.
	sent := func(served bool) []Raw {
		at := recRun.read.Add(40 * time.Millisecond)
		return []Raw{RawOf(&models.Mock{Name: "mock-9", Kind: "Pulsar", Spec: models.MockSpec{
			GenericRequests:  []models.Payload{{Origin: "client", Message: []models.OutputBinary{{Type: "json", Data: `{"commandType":"SEND","payload":{"type":"lead.created","at":"` + dotNetStamp(recRun.read) + `"}}`}}}},
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		}}, served)}
	}
	early := recRun
	early.began = recRun.read.Add(50 * time.Millisecond) // after the read, before the other goroutine's serve
	for _, proven := range []bool{false, true} {
		for _, served := range []bool{true, false} {
			for _, withLookUp := range []bool{false, true} {
				raw := sent(served)
				if withLookUp {
					raw = append(append([]Raw{}, lookUp...), raw...)
				}
				name := fmt.Sprintf("sent in a payload, served %v, look-up %v, proven %v", served, withLookUp, proven)
				t.Run(name, func(t *testing.T) {
					if proven {
						t.Setenv(provenswitch.Env, "1")
					}
					for _, w := range []*Window{serverStampWindow(recRun, fRun), inSpan} {
						if ln := learn(recRun.read, w, raw); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
							t.Errorf("window %+v: %s from %q held %q (%s); want noise of the word", *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
						}
					}
					want := "mock-9"
					if withLookUp {
						want = "mock-1"
					}
					if ln := learn(recRun.read, serverStampWindow(early, fRun), raw); !foundAsAPart(ln) || ln.From.Mock != want {
						t.Errorf("the recording's read made before its test began: %s from %q held %q (%s); want a part found in %s to stay the value's place", ln.Kind, ln.From.Mock, ln.From.Held, ln.Why, want)
					}
				})
			}
		}
	}
	// Fail closed where the window from the test's begin does not hold the
	// read: the look-up's part stays the value's place, and with no payload
	// the decoded text is refused, named so.
	unknown := serverStampWindow(recRun, fRun)
	unknown.Rec.TestFrom = time.Time{}
	for _, wc := range []struct {
		what string
		w    *Window
	}{
		{"the recording's read made before its test began", serverStampWindow(early, fRun)},
		{"the recording's begin unknown", unknown},
		{"the call's window alone", &Window{Rec: Span{From: recRun.prev, To: recRun.call}, F: Span{From: fRun.prev, To: fRun.call}}},
	} {
		if ln := learn(recRun.read, wc.w, lookUp); !foundAsAPart(ln) || ln.From.Mock != "mock-1" {
			t.Errorf("%s, the look-up: %s from %q held %q (%s); want the look-up's part to stay the value's place", wc.what, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
		}
		ln := learn(recRun.read, wc.w, nil)
		if ln.Kind != KindUnaccounted || ln.Why != encodedScriptWhy {
			t.Errorf("%s, no payload: %s (%s); want it refused as %q", wc.what, ln.Kind, ln.Why, encodedScriptWhy)
		}
		// The text percent-escaped: the same answers.
		learnEscaped := func(raw []Raw) Learned {
			return ClassifyIn(clockedDrifts(), []string{p}, escaped(fRun.read), escaped(recRun.read), recRun.call, nil, raw, nil, wc.w)[0]
		}
		if ln := learnEscaped(lookUp); !foundAsAPart(ln) || ln.From.Mock != "mock-1" {
			t.Errorf("%s, escaped, the look-up: %s from %q held %q (%s); want the look-up's part to stay the value's place", wc.what, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
		}
		if ln := learnEscaped(nil); ln.Kind != KindUnaccounted || ln.Why != encodedScriptWhy {
			t.Errorf("%s, escaped, no payload: %s (%s); want it refused as %q", wc.what, ln.Kind, ln.Why, encodedScriptWhy)
		}
	}
	// One cache, one value, one call's window bounds: the window from the
	// test's begin decides which words are the clock's, so a window that
	// carries it is never read as one that does not.
	fUnknown := serverStampWindow(recRun, fRun)
	fUnknown.F.TestFrom = time.Time{}
	dc := clockedDrifts()
	for _, c := range []struct {
		what  string
		w     *Window
		noise bool
	}{
		{"the recording's begin unknown", unknown, false},
		{"both begins known", serverStampWindow(recRun, fRun), true},
		{"the recording's begin unknown, again", unknown, false},
		{"record's full round's begin unknown", fUnknown, false},
		{"both begins known, again", serverStampWindow(recRun, fRun), true},
	} {
		ln := ClassifyIn(dc, []string{p}, text(fRun.read), text(recRun.read), recRun.call, nil, lookUp, nil, c.w)[0]
		if (ln.Kind == KindNoise) != c.noise {
			t.Errorf("one cache, %s: %s from %q (%s); want noise %v", c.what, ln.Kind, ln.From.Mock, ln.Why, c.noise)
		}
	}
}

// A clock read written in the text an encoded run of a value decodes to
// ("tok <base64 of 'lead <stamp> created'> ok") is not decoded again where
// the value's drift reads it as the clock's: moved by the lead, in a drift
// bounded by no call (the one the set's tests share), or a read in its
// call's window, with the lead or without it (a client test). Where neither
// reads it, it is decoded as data, as on main: in a client test's drift
// bounded by no call, a client test's read made before another call's serve
// (outside its call's window), and an expiry thirty minutes after a read its
// window holds (an offset, which no window reads). Its fraction "3505371Z"
// decodes as base64 to a text of its own, which shows where it was decoded.
func TestAnEncodedTextsStampIsDecodedAgainOnlyWhereNoReadingOfTheClockHoldsIt(t *testing.T) {
	recRun, fRun := serverStampRuns()
	enc := func(v time.Time) string {
		return "tok " + base64.StdEncoding.EncodeToString([]byte("lead "+dotNetStamp(v)+" created")) + " ok"
	}
	frac := strings.TrimPrefix(dotNetStamp(recRun.read), dotNetStamp(recRun.read)[:20])
	again := b64Text(frac, nil)
	if frac != "3505371Z" || again == "" {
		t.Fatalf("the recorded fraction %q decodes to %q: this row needs one that decodes to text", frac, again)
	}
	byLead := recRun.read.Add(FullRoundLead + 400*time.Microsecond)
	spans := &Window{Rec: Span{From: recRun.began, To: recRun.call}, F: Span{From: fRun.began, To: fRun.call}}
	afterServe := &Window{Rec: Span{From: recRun.prev, To: recRun.call}, F: Span{From: fRun.prev, To: fRun.call}}
	const expiry = 30 * time.Minute
	for _, c := range []struct {
		what      string
		rec, live time.Time
		w         *Window
		lead      bool
		call      time.Time
		decoded   bool
	}{
		{"moved by the lead, bounded by no call", recRun.read, byLead, nil, true, time.Time{}, false},
		{"a read in its call's window", recRun.read, fRun.read, spans, true, recRun.call, false},
		{"a client test's read in its call's window", recRun.read, fRun.read, spans, false, recRun.call, false},
		{"moved by the lead, a client test's drift bounded by no call", recRun.read, byLead, nil, false, time.Time{}, true},
		{"a client test's read before another call's serve", recRun.read, fRun.read, afterServe, false, recRun.call, true},
		{"an expiry thirty minutes after a read its window holds", recRun.read.Add(expiry), fRun.read.Add(expiry), spans.ComparesOffsets(), true, recRun.call, true},
	} {
		d := newDrift(enc(c.rec), enc(c.live), c.w, c.lead, c.call, nil)
		if !strings.Contains(d.encodedRec, "lead "+dotNetStamp(c.rec)+" created") {
			t.Fatalf("%s: the encoded text %q was not decoded", c.what, d.encodedRec)
		}
		if got := strings.Contains(d.encodedRec, again); got != c.decoded {
			t.Errorf("%s: the stamp's fraction decoded again %v (%q); want %v", c.what, got, d.encodedRec, c.decoded)
		}
	}
	// The recording's text encoded twice, record's full round's once: the
	// recorded stamp's text comes out of the second decode, where the text
	// both sides decoded so far pairs it with the live one, so its fraction
	// is not decoded. Record's full round's fraction ("2754275Z", which
	// decodes to text) is decoded in that same second decode, a run of the
	// text its first decode gave, before the recorded stamp was there to
	// pair it with: the limit b64Pair states.
	fLate := fRun.read.Add(311_100 * time.Nanosecond)
	liveFrac := strings.TrimPrefix(dotNetStamp(fLate), dotNetStamp(fLate)[:20])
	liveAgain := b64Text(liveFrac, nil)
	if liveFrac != "2754275Z" || liveAgain == "" {
		t.Fatalf("record's full round's fraction %q decodes to %q: this row needs one that decodes to text", liveFrac, liveAgain)
	}
	twice := "tok " + base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString([]byte("lead "+dotNetStamp(recRun.read)+" created")))) + " ok"
	d := newDrift(twice, enc(fLate), spans, true, recRun.call, nil)
	if !strings.Contains(d.encodedRec, "lead "+dotNetStamp(recRun.read)+" created") || strings.Contains(d.encodedRec, again) {
		t.Errorf("encoded twice: the recorded text %q; want the stamp's text decoded and its fraction not decoded again", d.encodedRec)
	}
	liveDecoded := false
	for _, f := range d.forms {
		if strings.Contains(f[1], "lead "+dotNetStamp(fLate)+" created") && strings.Contains(f[1], liveAgain) {
			liveDecoded = true
		}
	}
	if !liveDecoded {
		t.Errorf("encoded once in record's full round: its fraction was not decoded at the first level (forms %q); the stated limit no longer holds, so say so", d.forms)
	}
}

// A value whose text of a script keploy reads only whole changed is refused
// naming where that text stands: in the value's own text, in the text its
// escapes write, or in the text an encoded run of it decodes to. A value all
// in ASCII is never said to hold such text itself.
func TestARefusalForTextOfAScriptNamesWhereThatTextStands(t *testing.T) {
	esc := func(s string) string { return strings.ReplaceAll(url.PathEscape(s), "%20", " ") }
	for _, c := range []struct {
		what, rec, live, want string
	}{
		{"its own text", "день понедельник", "день вторник", scriptWhy},
		{"its own text, escaped too", "понедельник " + esc("ок"), "вторник " + esc("да"), scriptWhy},
		{"the text its escapes write", "day " + esc("понедельник"), "day " + esc("вторник"), escapedScriptWhy},
		{"the text an encoded run decodes to", "lead 3505371Z created", "lead 2751164Z created", encodedScriptWhy},
		{"the text an encoded run decodes to, escaped", "lead%203505371Z%20created", "lead%202751164Z%20created", encodedScriptWhy},
		{"the text its quoted-printable escapes write", "day =D0=BF=D1=80", "day =D0=BF=D1=81", escapedScriptWhy},
	} {
		if got := NewDriftCache().of(c.rec, c.live).unparsed; got != c.want {
			t.Errorf("%s (%q -> %q): %q; want %q", c.what, c.rec, c.live, got, c.want)
		}
	}
}

// A payload that holds the moment a value was recorded with (the echo: a
// departure picked from a timetable the application was given) outranks
// an earlier payload that holds only a word of the value by chance (a
// slot "11:14" in a start-up's config): the echo names the value's place
// whichever payload came first. A timetable recorded after the call that F
// had not served before it is no place, and the earlier part stays.
func TestAnEchoInALaterPayloadOutranksAnEarlierPayloadsPart(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 14, 20, 0, time.UTC)
	const p = "body.departure"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	w := &Window{Rec: Span{From: at.Add(-time.Second), To: at}, F: Span{From: at.Add(time.Minute - time.Second), To: at.Add(time.Minute)}}
	answer := func(name, body string, when time.Time, served bool) Raw {
		return RawOf(&models.Mock{Name: name, Kind: "Pulsar", Spec: models.MockSpec{
			GenericResponses: []models.Payload{{Origin: "server", Message: []models.OutputBinary{{Type: "json", Data: body}}}},
			ReqTimestampMock: when, ResTimestampMock: when,
		}}, served)
	}
	config := answer("mock-1", `{"slot":"11:14","n":7}`, at.Add(-3*time.Second), true)
	timetable := func(when time.Time, served bool) Raw {
		return answer("mock-2", `{"cursor":[{"at":"2026-10-22T11:14:00Z"},{"at":"2026-10-22T11:15:00Z"}]}`, when, served)
	}
	learn := func(raw ...Raw) Learned {
		return ClassifyIn(clockedDrifts(), []string{p}, str("2026-10-22T11:15:00Z"), str("2026-10-22T11:14:00Z"), at, nil, raw, nil, w)[0]
	}
	for what, raw := range map[string][]Raw{
		"the timetable alone":            {timetable(at.Add(-time.Second), true)},
		"after the config's chance word": {config, timetable(at.Add(-time.Second), true)},
	} {
		if ln := learn(raw...); ln.Kind != KindVariable || ln.From.Mock != "mock-2" || !ln.From.Echo {
			t.Errorf("%s: %s from %q held %q echo %v (%s); want the echo's variable of mock-2", what, ln.Kind, ln.From.Mock, ln.From.Held, ln.From.Echo, ln.Why)
		}
	}
	if ln := learn(config, timetable(at.Add(time.Second), false)); !foundAsAPart(ln) || ln.From.Mock != "mock-1" || ln.From.Held != "14" {
		t.Errorf("a timetable recorded after the call, not served before it: %s from %q held %q echo %v (%s); want the config's part to stay the value's place", ln.Kind, ln.From.Mock, ln.From.Held, ln.From.Echo, ln.Why)
	}
}

// A stamp its call's window holds, followed by a changed word with no ASCII
// in it (a Russian weekday, the lead crossing midnight): the search reads
// that word as data, as a part, since the clock's words are the ASCII ones.
// A payload holding the weekday by chance (a locale table) comes first; a
// later payload made before the call holds the value whole.
// The whole copy names the value's place; with no whole copy, or one
// recorded after the call that F had not served, the weekday's part stays.
func TestAStampWholeInALaterPayloadOutranksAnEarlierNonASCIIPart(t *testing.T) {
	const p = "body.at"
	rec, live := "2026-11-02T23:59:58Z понедельник", "2026-11-03T00:01:00Z вторник"
	recAt := time.Date(2026, 11, 2, 23, 59, 58, 400_000_000, time.UTC)
	fAt := time.Date(2026, 11, 3, 0, 1, 0, 300_000_000, time.UTC)
	w := &Window{Rec: Span{From: recAt.Add(-time.Second), To: recAt}, F: Span{From: fAt.Add(-time.Second), To: fAt}}
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	payload := func(name, body string, at time.Time, served bool) Raw {
		return RawOf(&models.Mock{Name: name, Kind: "Pulsar", Spec: models.MockSpec{
			GenericRequests:  []models.Payload{{Origin: "client", Message: []models.OutputBinary{{Type: "json", Data: body}}}},
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		}}, served)
	}
	locale := payload("mock-1", `{"locale":"ru","days":["воскресенье","понедельник","вторник"]}`, recAt.Add(-5*time.Second), true)
	sent := func(at time.Time, served bool) Raw {
		return payload("mock-9", `{"commandType":"SEND","payload":{"at":"`+rec+`"}}`, at, served)
	}
	learn := func(raw ...Raw) Learned {
		return ClassifyIn(clockedDrifts(), []string{p}, str(live), str(rec), recAt, nil, raw, nil, w)[0]
	}
	if ln := learn(locale); !foundAsAPart(ln) || ln.From.Mock != "mock-1" || ln.From.Held != "понедельник" {
		t.Fatalf("the locale table alone: %s from %q held %q (%s); want the weekday's part of mock-1", ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
	}
	if ln := learn(locale, sent(recAt.Add(-time.Second), true)); ln.Kind != KindVariable || ln.From.Mock != "mock-9" || ln.From.Held != rec {
		t.Errorf("a later payload holding the value whole: %s from %q held %q (%s); want the variable of mock-9 holding it whole", ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
	}
	if ln := learn(locale, sent(recAt.Add(time.Second), false)); !foundAsAPart(ln) || ln.From.Mock != "mock-1" || ln.From.Held != "понедельник" {
		t.Errorf("the whole copy recorded after the call, not served before it: %s from %q held %q (%s); want the weekday's part of mock-1", ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
	}
}

// foundAsAPart reports whether ln is a variable whose place is in a raw
// mock's payload (Place.Side "": appearedIn reads its words as tokens there)
// and holds only a part of the recorded value (a word, a piece, or a decoded
// or case-folded form of one), never an echo's instant.
func foundAsAPart(ln Learned) bool {
	return ln.Kind == KindVariable && ln.From.Side == "" && !ln.From.Echo && ln.From.Held != ln.Recorded.Value
}

// answerServed is a recording record's full round served before the call,
// whose adapter stated no view of what it gave the application there: an
// answer (a Pulsar MESSAGE) recorded 20 ms after rec, a read of the clock,
// that holds no changed word of the value; of another test (OfAnotherTest)
// where other holds. keploy cannot tell what record's full round gave the
// application there, so a changed word that is no clock word may stand
// there (unseenWhy, servedWhy).
func answerServed(rec time.Time, other bool) Raw {
	x := RawOf(answerMock(rec, `{"type":"lead.seen","count":3}`), true)
	if other {
		x = x.OfAnotherTest()
	}
	return x
}

// answerMock is answerServed's recording, its MESSAGE's payload body (a
// view of it F's serve gave, where it differs).
func answerMock(rec time.Time, body string) *models.Mock {
	at := rec.Add(20 * time.Millisecond)
	return &models.Mock{Name: "mock-11", Kind: "Pulsar", Spec: models.MockSpec{
		GenericRequests:  []models.Payload{{Origin: "client", Message: []models.OutputBinary{{Type: "json", Data: `{"commandType":"FLOW","consumerId":7}`}}}},
		GenericResponses: []models.Payload{{Origin: "server", Message: []models.OutputBinary{{Type: "json", Data: `{"commandType":"MESSAGE","payload":` + body + `}`}}}},
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
	}}
}

// A server test's text holding a clock read ("ok? lead <.NET stamp>
// created") gets one answer, plain or encoded in a token (base64 once or
// twice, with its '/', '+' and '=' percent-escaped, URL-safe and
// percent-escaped, hex after "0x" or "0X"), whether or not record's full
// round served before the call a payload whose view keploy cannot read (no
// adapter stated one, or one could not be read), of the call's own test or
// another test's: noise of the word, whatever another goroutine's serve
// came between the read and its call, whatever digits the clock gave the
// read, under the proven flow too. An encoded run is read as the text it
// decodes to, whose words are searched themselves, so it is no word of its
// own such a payload may hold, as the plain text holds none (plainRunsOf);
// before, the encoded text alone was refused where one had been served
// before the call, which another goroutine's serve decides. Under such a
// payload so are the text with only its padding escaped, a text that lost
// words the recording held, one whose run record's full round spells in
// the other base64 alphabet, and one split over two runs: noise, as the
// plain text is. It fails closed otherwise, plain and encoded alike: a data
// word of the text changed, a read no window of the call holds (made before
// its test began, its begin unknown, the call's window alone, a client
// test's call), an expiry thirty minutes after the read, a word of a run
// that stands outside the runs too or in a longer run that does not decode,
// a second copy of a decoded run, a case-folded copy that does not decode,
// a word after a hex run, a hash after "0x", an escape on one side only
// that leaves a word of its own or one that changed, and a run whose
// case-folded form a shorter capital letter moves are refused there, and a
// view of what record's full round served that holds the encoded run whole
// refuses it.
func TestAnEncodedTextGetsThePlainTextsAnswerWhateverUnreadPayloadWasServedBeforeItsCall(t *testing.T) {
	recRun, fRun := serverStampRuns()
	const p = "body.Item.Note.S"
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	text := func(v time.Time, word string) string { return "ok? lead " + dotNetStamp(v) + " " + word }
	coop := func(v time.Time, word string) string { return "ok coöperate lead " + dotNetStamp(v) + " " + word }
	escape := strings.NewReplacer(" ", "%20", "/", "%2F", "+", "%2B", "=", "%3D").Replace
	shapes := []struct {
		what string
		of   func(v time.Time, word string) string
	}{
		{"plain", text},
		{"encoded", func(v time.Time, word string) string { return "tok " + enc(text(v, word)) + " ok" }},
		// " a>b" gives the run a '+' and its padding, both escaped too.
		{"encoded, percent-escaped", func(v time.Time, word string) string {
			return escape("tok " + enc(text(v, word)+" a>b") + " ok")
		}},
		// The URL-safe alphabet's '-' and '_' need no escape: its runs
		// stand glued to the "%20" before them in the readings that keep
		// '-' and '_' inside a word.
		{"encoded in the URL-safe alphabet, percent-escaped", func(v time.Time, word string) string {
			return strings.NewReplacer(" ", "%20", "=", "%3D").Replace("tok " + base64.URLEncoding.EncodeToString([]byte(text(v, word)+" a>b")) + " ok")
		}},
		{"encoded twice", func(v time.Time, word string) string { return "tok " + enc("note "+enc(text(v, word))+" end") + " ok" }},
		// The run encoded twice decoded from the second of two runs, so the
		// text it stands in is not the first the reader decoded (the first
		// run under 16 characters, which is not read as wrapped base64).
		{"encoded twice, after another run", func(v time.Time, word string) string {
			return "tok " + enc("a note") + " " + enc("note "+enc(text(v, word))+" end") + " ok"
		}},
		{"hex-encoded after 0x", func(v time.Time, word string) string {
			return "tok 0x" + hex.EncodeToString([]byte(text(v, word))) + " ok"
		}},
		{"hex-encoded after 0X", func(v time.Time, word string) string {
			return "tok 0X" + hex.EncodeToString([]byte(text(v, word))) + " ok"
		}},
		// A JSON writer that escapes '/' (PHP's, by default) writes the
		// run's '/' as "\/", here one that does not end a group of four,
		// so the run's parts after it do not decode on their own; and the
		// text it encodes, plain.
		{"encoded, its '/' JSON-escaped", func(v time.Time, word string) string {
			return "tok " + strings.ReplaceAll(enc(coop(v, word)), "/", `\/`) + " ok"
		}},
		{"plain, the JSON-escaped run's text", coop},
	}
	for _, word := range []string{"created", "deleted", "now then created"} {
		if e := enc(coop(fRun.read, word)); !regexp.MustCompile(`^(.{4})*.{0,2}/`).MatchString(e) {
			t.Fatalf("%q holds no '/' that does not end a group of four, so the JSON-escaped rows tell nothing", e)
		}
		if e := shapes[2].of(fRun.read, word); !strings.Contains(e, "%2F") || !strings.Contains(e, "%2B") || !strings.Contains(e, "%3D") {
			t.Fatalf("%q holds no escaped '/', '+' and '=' inside its run, so the escaped rows tell nothing", e)
		}
		if e := shapes[3].of(fRun.read, word); !strings.Contains(e, "%20b2s_") || !strings.Contains(e, "-") || !strings.Contains(e, "%3D") {
			t.Fatalf("%q holds no URL-safe run glued to an escape, so the URL-safe rows tell nothing", e)
		}
	}
	learn := func(dc *DriftCache, of func(time.Time, string) string, rec, f time.Time, recWord, fWord string, w *Window, raw []Raw) Learned {
		leaf := func(v string) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
		}
		return ClassifyIn(dc, []string{p}, leaf(of(f, fWord)), leaf(of(rec, recWord)), recRun.call, nil, raw, nil, w)[0]
	}
	inSpan := &Window{Rec: Span{From: recRun.began, To: recRun.call}, F: Span{From: fRun.began, To: fRun.call}}
	unread := []struct {
		what string
		raw  []Raw
	}{
		{"an answer served, no view stated", []Raw{answerServed(recRun.read, false)}},
		{"an answer served changed, a view keploy could not read", []Raw{answerServed(recRun.read, false).ServedChanged()}},
		{"another test's answer served, no view stated", []Raw{answerServed(recRun.read, true)}},
		{"another test's answer served changed, a view keploy could not read", []Raw{answerServed(recRun.read, true).ServedChanged()}},
	}
	payloads := append([]struct {
		what string
		raw  []Raw
	}{{"no payload", nil}, {"the answer not served", []Raw{answerServed(recRun.read, false).WithServed(false)}}}, unread...)
	layouts := []struct{ rec, f time.Duration }{
		{0, 0},
		{518_500 * time.Nanosecond, 0},
		{10_370 * time.Microsecond, 0},
		{30 * time.Millisecond, 0},
		{30 * time.Millisecond, 311_100 * time.Nanosecond},
	}
	for _, proven := range []bool{false, true} {
		t.Run("proven "+strconv.FormatBool(proven), func(t *testing.T) {
			if proven {
				t.Setenv(provenswitch.Env, "1")
			}
			for _, sh := range shapes {
				for _, d := range layouts {
					rec, f := recRun.read.Add(d.rec), fRun.read.Add(d.f)
					for _, pl := range payloads {
						for _, w := range []*Window{serverStampWindow(recRun, fRun), inSpan} {
							if ln := learn(clockedDrifts(), sh.of, rec, f, "created", "created", w, pl.raw); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
								t.Errorf("%s, read at %s and in record's full round at %s, %s, window %+v: %s from %q held %q (%s); want noise of the word", sh.what, rec.Format("05.0000000"), f.Format("05.0000000"), pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
							}
						}
					}
				}
				// Fail closed: a data word of the text changed is refused
				// as one such a payload may hold.
				for _, pl := range unread {
					for _, w := range []*Window{serverStampWindow(recRun, fRun), inSpan} {
						if ln := learn(clockedDrifts(), sh.of, recRun.read, fRun.read, "created", "deleted", w, pl.raw); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
							t.Errorf("%s, a data word changed, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", sh.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
						}
					}
				}
				// Fail closed where no window of the call holds the read, and
				// for an expiry thirty minutes after it: refused.
				early := recRun
				early.began = recRun.read.Add(50 * time.Millisecond)
				unknown := serverStampWindow(recRun, fRun)
				unknown.Rec.TestFrom = time.Time{}
				callOnly := &Window{Rec: Span{From: recRun.prev, To: recRun.call}, F: Span{From: fRun.prev, To: fRun.call}}
				clientDrifts := func() *DriftCache {
					dc := clockedDrifts()
					dc.clientTests = true
					return dc
				}
				for _, c := range []struct {
					what string
					dc   func() *DriftCache
					rec  time.Time
					w    *Window
				}{
					{"read before its test began", clockedDrifts, recRun.read, serverStampWindow(early, fRun)},
					{"the recording's begin unknown", clockedDrifts, recRun.read, unknown},
					{"the call's window alone", clockedDrifts, recRun.read, callOnly},
					{"a client test's call's window", clientDrifts, recRun.read, callOnly},
					{"an expiry thirty minutes after the read", clockedDrifts, recRun.read.Add(30 * time.Minute), serverStampWindow(recRun, fRun)},
				} {
					for _, pl := range unread {
						f := fRun.read
						if c.rec.After(recRun.read) {
							f = f.Add(30 * time.Minute)
						}
						if ln := learn(c.dc(), sh.of, c.rec, f, "created", "created", c.w, pl.raw); ln.Kind != KindUnaccounted {
							t.Errorf("%s, %s, %s: %s from %q held %q (%s); want it refused", sh.what, c.what, pl.what, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
						}
					}
				}
			}
			// Words the recording held, gone from F's text, are no word F
			// gave: noise, plain and encoded alike. (Nine bytes, so each
			// level's run is padded alike on both sides: a run padded on one
			// side only is read joined with the word after it as wrapped
			// base64, a limit of its own.)
			for _, sh := range shapes {
				for _, pl := range unread {
					for _, w := range []*Window{serverStampWindow(recRun, fRun), inSpan} {
						if ln := learn(clockedDrifts(), sh.of, recRun.read, fRun.read, "now then created", "created", w, pl.raw); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
							t.Errorf("%s, a recorded word gone, %s, window %+v: %s from %q held %q (%s); want noise of the word", sh.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
						}
					}
				}
			}
			// Fail closed: a view of what F served that holds F's encoded
			// run whole refuses it, as a view refuses any word it holds,
			// of the call's own test or another test's.
			liveEnc := shapes[1].of(fRun.read, "created")
			run := strings.Fields(liveEnc)[1]
			seen := func() Raw { return RawOf(answerMock(recRun.read, `{"type":"lead.seen","count":3}`), false) }
			holding := func() Raw { return RawOf(answerMock(recRun.read, `{"type":"lead.seen","note":"`+run+`"}`), false) }
			for _, v := range []struct {
				what string
				x    Raw
			}{
				{"served changed, one of two views holds the run", RawOf(answerMock(recRun.read, `{"type":"lead.seen","count":3}`), true).ServedChanged(seen(), holding())},
				{"another test's, served changed, one of two views holds the run", RawOf(answerMock(recRun.read, `{"type":"lead.seen","count":3}`), true).ServedChanged(seen(), holding()).OfAnotherTest()},
				{"another test's, served with a view that holds the run", RawOf(answerMock(recRun.read, `{"type":"lead.seen","count":3}`), true).ServedLive(holding()).OfAnotherTest()},
			} {
				for _, w := range []*Window{serverStampWindow(recRun, fRun), inSpan} {
					if ln := learn(clockedDrifts(), shapes[1].of, recRun.read, fRun.read, "created", "created", w, []Raw{v.x}); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "mock-11") || !strings.Contains(ln.Why, "record's full round gave it") {
						t.Errorf("encoded, %s, window %+v: %s from %q (%s); want it refused as a word F's view of mock-11 holds", v.what, *w, ln.Kind, ln.From.Mock, ln.Why)
					}
				}
			}
			// The run's padding percent-escaped and its '/' not (a query
			// escaped by a library that keeps '/'): a word of the run stands
			// in the value as written, glued to no escape, and is read as
			// the run's, so the text is noise, as the plain text is.
			escapePadding := strings.NewReplacer(" ", "%20", "=", "%3D").Replace
			padded := func(v time.Time) string { return "tok " + enc(text(v, "created.")) + " ok" }
			if e := escapePadding(padded(fRun.read)); !strings.Contains(e, "%3D") || !strings.Contains(e, "/") {
				t.Fatalf("%q holds no escaped padding beside an unescaped '/', so this row tells nothing", e)
			}
			// Fail closed: a word of record's full round's run that stands
			// outside the runs too ("eHk" of "YWI/eHk/", the run of
			// "ab?xy?", its text the recording's but for a word it lost) is
			// searched as any word of the value, and refused there, as the
			// plain word is; so is one that stands in a longer run that
			// holds the decoded run whole and does not decode
			// ("YWI/eHk/x"); and so is one of a value with no capital
			// letter, which has no case-folded form ("enk" of "eng/enk/",
			// the run of "zx?zy?").
			outside := func(rec bool) string {
				if rec {
					return "tok " + enc("ab?cd? xy") + " ok zzz"
				}
				return "tok " + enc("ab?xy?") + " ok eHk"
			}
			inLonger := func(rec bool) string {
				if rec {
					return "tok " + enc("ab?xy?") + " ok ref:YWI/zzz/x"
				}
				return "tok " + enc("ab?xy?") + " ok ref:YWI/eHk/x"
			}
			lower := func(rec bool) string {
				if rec {
					return "tok " + enc("zx{zx{zy") + " ok zzz"
				}
				return "tok " + enc("zx?zy?") + " ok enk"
			}
			// A second copy of a decoded run, which the reader does not
			// decode again ("YWI/eHk/" twice, the second once "YWI/Y2Q/"),
			// and a run the run's case-folding spells that does not decode
			// ("ywi/ehk/"): searched where they stand, and refused.
			second := func(rec bool) string {
				if rec {
					return "tok " + enc("ab?xy?") + " ok ref:" + enc("ab?cd?")
				}
				return "tok " + enc("ab?xy?") + " ok ref:" + enc("ab?xy?")
			}
			folded := func(rec bool) string {
				if rec {
					return "tok " + enc("ab?xy?") + " ok ywi/zzz/"
				}
				return "tok " + enc("ab?xy?") + " ok ywi/ehk/"
			}
			// A decoded run's words that stand, at the place it stands in
			// the text it decodes from, in the value itself inside a longer
			// run that does not decode ("YWI/eHk/x"): searched there.
			deepPlace := func(rec bool) string {
				if rec {
					return "YWI/zzz/x " + enc("YWI/eHk/ text")
				}
				return "YWI/eHk/x " + enc("YWI/eHk/ text")
			}
			// A word after a run of hex after "0x" is no word of that run.
			afterHex := func(rec bool) string {
				if rec {
					return "tok 0x" + hex.EncodeToString([]byte("ab?xy? text")) + " zzz"
				}
				return "tok 0x" + hex.EncodeToString([]byte("ab?xy? text")) + " eHk"
			}
			if !strings.Contains(outside(false), "/eHk/") || !strings.Contains(inLonger(false), "tok YWI/eHk/ ok") || lower(false) != strings.ToLower(lower(false)) || lower(true) != strings.ToLower(lower(true)) || !strings.Contains(lower(false), "eng/enk/") {
				t.Fatalf("%q, %q, %q: the runs are not as this row reads them, so it tells nothing", outside(false), inLonger(false), lower(false))
			}
			// Each with escapes on both sides, on one side only (the
			// recorded value's or record's full round's), or none: the raw
			// form of a value with escapes is read apart, and the decoded
			// form of the side with none is still read for the words outside
			// the runs. An escape only record's full round's value holds is
			// a change of its own, glued to a run's word too, as in a plain
			// text: refused.
			same := func(v string) string { return v }
			escapeOne := func(v string) string { return strings.Replace(v, " ", "%20", 1) }
			escapeBoth := func(v string) string { return strings.ReplaceAll(v, " ", "%20") }
			for _, esc := range []struct {
				what           string
				rec, live      func(string) string
				padRec, padLiv func(string) string
				oneSide        bool
				liveOnly       bool
			}{
				{"", same, same, escapePadding, escapePadding, false, false},
				{", both escaped", escapeBoth, escapeBoth, escapePadding, escapePadding, false, false},
				{", the recorded value's escaped", escapeOne, same, escapePadding, same, true, false},
				{", record's full round's escaped", same, escapeOne, same, escapePadding, true, true},
			} {
				for _, pl := range unread {
					for _, w := range []*Window{serverStampWindow(recRun, fRun), inSpan} {
						leaf := func(v string) Shape {
							return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
						}
						learnOf := func(rec, live string) Learned {
							return ClassifyIn(clockedDrifts(), []string{p}, leaf(live), leaf(rec), recRun.call, nil, pl.raw, nil, w)[0]
						}
						// Escapes on one side only that leave a word of their
						// own (the padding's "%3D" and the spaces' "%20" on
						// record's full round's side, or "ok" where the
						// recorded side's "%20ok" glued it) change the value,
						// as an escape a plain text gained does
						// ("created%2E"): refused.
						if ln := learnOf(esc.padRec(padded(recRun.read)), esc.padLiv(padded(fRun.read))); esc.oneSide && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11")) {
							t.Errorf("encoded, its padding escaped%s, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", esc.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
						} else if !esc.oneSide && (ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "") {
							t.Errorf("encoded, its padding escaped%s, %s, window %+v: %s from %q held %q (%s); want noise of the word", esc.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
						}
						if esc.what == "" {
							if ln := learnOf(text(recRun.read, "created."), text(fRun.read, "created%2E")); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
								t.Errorf("plain, an escape gained, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
							}
							// An escape inside the run alone (its padding's
							// '=' or its '/', the spaces around it raw):
							// written so on both sides it is the run's
							// spelling, noise; gained by record's full
							// round's value, the plain text's answer for
							// that escape gained ("x=y" to "x%3Dy"): refused
							// where it leaves a word of its own ("3Dy"), and
							// noise for a JSON "\/", which leaves none. Lost
							// by it, record's full round's run is written as
							// itself, every word of it the run's, which F
							// reads as its text: noise, as a run respelled
							// in its padding is (a plain text's word after an
							// escape it lost, "y" of "x%3Dy", is a word of
							// the value, not of a run).
							for _, in := range []struct {
								what, from, to string
								refused        bool
							}{{"its padding", "=", "%3D", true}, {"its '/'", "/", "%2F", true}, {"its '/' JSON-escaped", "/", `\/`, false}} {
								escIn := func(v string) string { return strings.ReplaceAll(v, in.from, in.to) }
								gained := learnOf(text(recRun.read, "x"+in.from+"y"), text(fRun.read, "x"+in.to+"y"))
								if refused := gained.Kind == KindUnaccounted && strings.Contains(gained.Why, "may stand in mock-11"); refused != in.refused {
									t.Errorf("plain, %s gained, %s, window %+v: %s (%s); want it refused %v", in.what, pl.what, *w, gained.Kind, gained.Why, in.refused)
								}
								if e := escIn(padded(fRun.read)); !strings.Contains(e, in.to) || strings.Contains(e, "%20") || !strings.Contains(e, " ") {
									t.Fatalf("%q holds no %s escaped beside raw spaces, so this row tells nothing", e, in.what)
								}
								if ln := learnOf(escIn(padded(recRun.read)), escIn(padded(fRun.read))); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
									t.Errorf("encoded, %s escaped on both sides, %s, window %+v: %s from %q held %q (%s); want noise of the word", in.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
								}
								if ln := learnOf(padded(recRun.read), escIn(padded(fRun.read))); ln.Kind != gained.Kind || ln.Kind == KindUnaccounted && !strings.Contains(ln.Why, "may stand in mock-11") {
									t.Errorf("encoded, %s escaped by record's full round's value only, %s, window %+v: %s from %q (%s); want the plain text's answer for that escape gained, %s", in.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.Why, gained.Kind)
								}
								if ln := learnOf(escIn(padded(recRun.read)), padded(fRun.read)); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
									t.Errorf("encoded, %s escaped by the recorded value only, %s, window %+v: %s from %q held %q (%s); want noise of the word", in.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
								}
							}
							// Both sides escape one of the run's two '/' but
							// not the same one ("?" before the word puts a
							// second '/' in the run): a change of its own,
							// refused, as "a/b%2Fc" to "a%2Fb/c" is.
							twoSlashes := func(v time.Time, nth int) string {
								e := enc(text(v, "? created"))
								if strings.Count(e, "/") != 2 {
									t.Fatalf("%q holds no two '/', so this row tells nothing", e)
								}
								i := strings.Index(e, "/")
								if nth == 2 {
									i += 1 + strings.Index(e[i+1:], "/")
								}
								return "tok " + e[:i] + "%2F" + e[i+1:] + " ok"
							}
							if ln := learnOf(twoSlashes(recRun.read, 1), twoSlashes(fRun.read, 2)); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
								t.Errorf("encoded, another of its '/' escaped, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
							}
							if ln := learnOf(text(recRun.read, "a/b%2Fc"), text(fRun.read, "a%2Fb/c")); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
								t.Errorf("plain, another '/' escaped, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
							}
							// An escape gained at the end of a run, right
							// before the next run ("Lg=%3D<run>"), glues its
							// remnant to the next run's first word: it is
							// the first run's, which changed its escapes,
							// not the next run's: refused.
							adjacent := func(v time.Time, last string) string {
								return "tok " + strings.TrimSuffix(enc(text(v, "created.")), "=") + last + enc("lead "+dotNetStamp(v)+" created") + " ok"
							}
							if ln := learnOf(adjacent(recRun.read, "="), adjacent(fRun.read, "%3D")); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
								t.Errorf("encoded, an escape gained at the end of a run right before another, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
							}
							// A run pairs by rank only where the text before
							// it, from the run before it, is the recorded
							// one's: record's full round's value that dropped
							// the recorded value's first run ("a <R0> ") has
							// its '/' escaped where the recorded run of its
							// rank, R1, holds it raw. That R0 escapes its
							// '/' the same way says nothing: refused, as
							// the same value is without R0.
							slashOnly := func(e string) string { return strings.ReplaceAll(e, "/", "%2F") }
							r0 := slashOnly(strings.TrimRight(enc(text(recRun.read.Add(-time.Second), "x")), "="))
							r1, l1 := enc(text(recRun.read, "created")), slashOnly(enc(text(fRun.read, "created")))
							if !strings.Contains(r0, "%2F") || !strings.Contains(l1, "%2F") || !strings.Contains(r1, "/") {
								t.Fatalf("%q, %q or %q holds no '/', so this row tells nothing", r0, r1, l1)
							}
							for _, rec := range []string{"a " + r0 + " b " + r1 + " ok", "b " + r1 + " ok"} {
								if ln := learnOf(rec, "b "+l1+" ok"); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
									t.Errorf("encoded, its '/' escaped by record's full round's value only, recorded %q, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", rec[:2], pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
								}
							}
							// Each byte is compared by the one spelling all
							// its copies share, so a run whose length moved
							// its padding and its '?' (Go's stamp trims a
							// trailing 0: 41 bytes, one '=' and no '/',
							// recorded; 40, "==" and a '/', live) is
							// escaped the same way: noise, as its text is
							// plain. A byte only one side holds ('/') is
							// written so where the recorded run escapes
							// another the same way ("%3D"); where it
							// escapes none, nothing says how it would write
							// it: refused (fail closed).
							goText := func(v time.Time, tail string) string {
								return enc("lead " + v.UTC().Format(time.RFC3339Nano) + " ok? yes" + tail)
							}
							queryEsc := strings.NewReplacer("/", "%2F", "=", "%3D", "+", "%2B").Replace
							recE, liveE := goText(recRun.read, ""), goText(fRun.read.Truncate(time.Microsecond), "")
							if strings.ContainsAny(recE, "/+") || strings.Count(recE, "=") != 1 || !strings.Contains(liveE, "/") || !strings.HasSuffix(liveE, "==") || strings.Contains(liveE, "+") {
								t.Fatalf("%q and %q are not one '=' and no '/' against a '/' and \"==\", so this row tells nothing", recE, liveE)
							}
							if ln := learnOf("tok "+queryEsc(recE)+" ok", "tok "+queryEsc(liveE)+" ok"); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
								t.Errorf("encoded, escaped the same way, its length moving a '/' and its padding, %s, window %+v: %s from %q held %q (%s); want noise of the word", pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
							}
							if ln := learnOf("tok "+recE+" ok", "tok "+liveE+" ok"); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
								t.Errorf("encoded raw, its length moving a '/' and its padding, %s, window %+v: %s from %q held %q (%s); want noise of the word", pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
							}
							recBare := goText(recRun.read, "!")
							if strings.ContainsAny(recBare, "/+=") {
								t.Fatalf("%q holds a byte an escape writes, so this row tells nothing", recBare)
							}
							if ln := learnOf("tok "+queryEsc(recBare)+" ok", "tok "+queryEsc(goText(fRun.read.Truncate(time.Microsecond), "!"))+" ok"); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
								t.Errorf("encoded, escaped by record's full round's value only where the recorded run escapes nothing, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
							}
							// Another kind of escape for the byte only one
							// side holds ('/' escaped twice, "%252F", beside
							// the recorded run's "%3D"): refused.
							twice := strings.ReplaceAll(queryEsc(liveE), "%2F", "%252F")
							if ln := learnOf("tok "+queryEsc(recE)+" ok", "tok "+twice+" ok"); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
								t.Errorf("encoded, a byte only record's full round's run holds escaped twice, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
							}
							// A clock read in the text before the run
							// ("ts <stamp> tok <run>") does not unpair it:
							// the run escaped the same way on both sides
							// is noise, wherever the clock moved before it.
							stamped := func(v time.Time) string {
								return "ts " + strings.ReplaceAll(dotNetStamp(v), ":", "%3A") + " tok " + escapePadding(slashOnly(enc(text(v, "created.")))) + " ok"
							}
							if s := stamped(fRun.read); !strings.Contains(s, "%2F") || !strings.Contains(s, "%3D") || strings.Contains(s, "%20") {
								t.Fatalf("%q holds no escaped '/' or padding, so this row tells nothing", s)
							}
							if ln := learnOf(stamped(recRun.read), stamped(fRun.read)); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
								t.Errorf("encoded after a clock read, escaped the same way, %s, window %+v: %s from %q held %q (%s); want noise of the word", pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
							}
							// The stated limit: where the recorded text holds
							// a stamp outside the test's span there (two
							// days stale, its fraction 0000000Z, which
							// decodes to no text), it is no clock read on
							// the recorded side, so the run after it pairs
							// with none and its escapes' words are the
							// value's own: refused, red, where the same
							// text with a plain run is noise.
							staleAt := recRun.read.Add(-48 * time.Hour).Truncate(time.Second)
							if _, ok := decodeText(strings.TrimPrefix(dotNetStamp(staleAt), dotNetStamp(staleAt)[:20])); ok || !strings.HasSuffix(dotNetStamp(staleAt), ".0000000Z") {
								t.Fatalf("%q's fraction decodes to text, so this row tells nothing", dotNetStamp(staleAt))
							}
							staleBefore := func(ts string, run string) string {
								return "ts " + strings.ReplaceAll(ts, ":", "%3A") + " tok " + run + " ok"
							}
							if ln := learnOf(staleBefore(dotNetStamp(staleAt), escapePadding(slashOnly(enc(text(recRun.read, "created."))))), stamped(fRun.read)); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
								t.Errorf("encoded after a stamp the recording holds stale, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
							}
							if ln := learnOf(staleBefore(dotNetStamp(staleAt), text(recRun.read, "created.")), staleBefore(dotNetStamp(fRun.read), text(fRun.read, "created."))); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
								t.Errorf("plain after a stamp the recording holds stale, %s, window %+v: %s from %q held %q (%s); want noise of the word", pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
							}
							// An escape record's full round's value lost is
							// no word of its own, while the others are
							// written as recorded: the recorded run's '/'
							// and '=' escaped, the live run's '/' only:
							// noise, as when it lost them all.
							partLost := strings.NewReplacer("/", "%2F").Replace
							if pe := padded(fRun.read); strings.Count(pe, "=") == 0 || !strings.Contains(pe, "/") {
								t.Fatalf("%q holds no '=' and '/', so this row tells nothing", pe)
							}
							if ln := learnOf(strings.ReplaceAll(partLost(padded(recRun.read)), "=", "%3D"), partLost(padded(fRun.read))); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
								t.Errorf("encoded, record's full round's value lost one of two escapes, %s, window %+v: %s from %q held %q (%s); want noise of the word", pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
							}
							// An escape gained in the run where the recorded
							// run escapes another byte the same way ('/'
							// written "%2F" beside a "%3D" both sides write):
							// the recorded run holds that '/' raw, so it is
							// the plain text's escape gained ("x/y" to
							// "x%2Fy"): refused.
							if ln := learnOf(escapePadding(padded(recRun.read)), escapePadding(slashOnly(padded(fRun.read)))); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
								t.Errorf("encoded, its padding escaped on both sides and its '/' by record's full round's value only, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
							}
							// The escape that ends a run right before the
							// next ("Lg=%3D<run>"), written so on both sides:
							// the word its remnant glues to the next run
							// ("3DbGVh...") is the runs', so the text is
							// noise, as it is with that '=' raw. So it is
							// where record's full round's value lost the
							// other escape of that run's padding ("=%3D"
							// against the recorded "%3D%3D"), whatever was
							// served before the call, as the plain text
							// is: a lost escape leaves no word, and the
							// glued word is the recorded one. Where it
							// gained that escape ("%3D%3D" against "=%3D"),
							// the escape leaves a word of its own ("3D"):
							// refused.
							if ln := learnOf(adjacent(recRun.read, "%3D"), adjacent(fRun.read, "%3D")); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
								t.Errorf("encoded, an escape ending a run right before another, written so on both sides, %s, window %+v: %s from %q held %q (%s); want noise of the word", pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
							}
							respelled := func(v time.Time, last string) string {
								return strings.Replace(adjacent(v, "%3D"), "=%3D", last, 1)
							}
							if r, l := respelled(recRun.read, "%3D%3D"), respelled(fRun.read, "=%3D"); !strings.Contains(r, "%3D%3D") || !strings.Contains(l, "=%3D") || strings.Contains(r, "=%3D") {
								t.Fatalf("%q and %q do not end the first run in \"%%3D%%3D\" and \"=%%3D\", so this row tells nothing", r, l)
							}
							plainAdjacent := func(v time.Time) string {
								return "tok " + text(v, "created.") + " lead " + dotNetStamp(v) + " created ok"
							}
							for _, py := range []struct {
								what string
								raw  []Raw
							}{payloads[0], payloads[1], pl} {
								learnIn := func(rec, live string) Learned {
									return ClassifyIn(clockedDrifts(), []string{p}, leaf(live), leaf(rec), recRun.call, nil, py.raw, nil, w)[0]
								}
								for _, c := range []struct{ what, rec, live string }{
									{"encoded, an escape ending a run right before another, the other escape of that run's padding lost", respelled(recRun.read, "%3D%3D"), respelled(fRun.read, "=%3D")},
									{"plain, the two runs' text", plainAdjacent(recRun.read), plainAdjacent(fRun.read)},
								} {
									if ln := learnIn(c.rec, c.live); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
										t.Errorf("%s, %s, window %+v: %s from %q held %q (%s); want noise of the word", c.what, py.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
									}
								}
							}
							if ln := learnOf(respelled(recRun.read, "=%3D"), respelled(fRun.read, "%3D%3D")); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
								t.Errorf("encoded, an escape ending a run right before another, the other escape of that run's padding gained, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
							}
							// An escape of a run's padding that record's full
							// round's value gained ("%3D%3D" or "=%3D"
							// against "==", "%3D%3D" against "=%3D") or
							// respelled ("%3D" against "%3d") leaves a word
							// of its own ("3D") where it stands, side
							// by side with the next run or apart from it,
							// whether or not another run that kept its
							// escapes spells the same word ("%3D" ending a
							// run after it): refused. Each escape is decided
							// where it stands, not by its remnant's spelling.
							g0 := func(v time.Time) string { return strings.TrimSuffix(enc(text(v, "created.")), "==") }
							g1 := func(v time.Time) string { return strings.TrimSuffix(enc(text(v, "updated.")), "==") }
							g2 := func(v time.Time) string { return strings.TrimSuffix(enc("lead "+dotNetStamp(v)+" created"), "=") }
							for _, v := range []time.Time{recRun.read, fRun.read} {
								if !strings.HasSuffix(enc(text(v, "created.")), "==") || !strings.HasSuffix(enc(text(v, "updated.")), "==") || strings.HasSuffix(enc("lead "+dotNetStamp(v)+" created"), "==") || !strings.HasSuffix(enc("lead "+dotNetStamp(v)+" created"), "=") {
									t.Fatalf("the runs at %v are not padded as this row reads them, so it tells nothing", v)
								}
							}
							two := func(between string) func(v time.Time, a, _, l string) string {
								return func(v time.Time, a, _, l string) string { return "tok " + g0(v) + a + between + g2(v) + l + " ok" }
							}
							three := func(between string) func(v time.Time, a, b, l string) string {
								return func(v time.Time, a, b, l string) string {
									return "tok " + g0(v) + a + between + g1(v) + b + between + g2(v) + l + " ok"
								}
							}
							for _, c := range []struct {
								what      string
								of        func(v time.Time, a, b, l string) string
								rec, live [3]string
							}{
								{"two runs side by side, the next one's padding raw", two(""), [3]string{"=%3D", "", "="}, [3]string{"%3D%3D", "", "="}},
								{"two runs side by side, the next one's padding escaped alike", two(""), [3]string{"=%3D", "", "%3D"}, [3]string{"%3D%3D", "", "%3D"}},
								{"two runs with \"%20\" between, the next one's padding escaped alike", two("%20"), [3]string{"=%3D", "", "%3D"}, [3]string{"%3D%3D", "", "%3D"}},
								{"two runs with a space between, the next one's padding escaped alike", two(" "), [3]string{"=%3D", "", "%3D"}, [3]string{"%3D%3D", "", "%3D"}},
								{"two runs with a space between, the next one's padding raw", two(" "), [3]string{"=%3D", "", "="}, [3]string{"%3D%3D", "", "="}},
								{"three runs side by side, the middle one's gained, the first one's alike", three(""), [3]string{"=%3D", "=%3D", "="}, [3]string{"=%3D", "%3D%3D", "="}},
								{"three runs with \"%20\" between, the middle one's gained, the first one's alike", three("%20"), [3]string{"=%3D", "=%3D", "="}, [3]string{"=%3D", "%3D%3D", "="}},
								{"three runs with \"%20\" between, the middle one's gained, the first one's raw", three("%20"), [3]string{"==", "=%3D", "="}, [3]string{"==", "%3D%3D", "="}},
								{"both its '=' gained, a run after words escaping its padding alike", two(" ok "), [3]string{"==", "", "%3D"}, [3]string{"%3D%3D", "", "%3D"}},
								{"both its '=' gained, a run after words with its padding raw", two(" ok "), [3]string{"==", "", "="}, [3]string{"%3D%3D", "", "="}},
								{"its last '=' gained, a run after words escaping its padding alike", two(" ok "), [3]string{"==", "", "%3D"}, [3]string{"=%3D", "", "%3D"}},
								{"its first '=' gained, a run after words escaping its padding alike", two(" ok "), [3]string{"=%3D", "", "%3D"}, [3]string{"%3D%3D", "", "%3D"}},
								{"its last '=' respelled, a run after words escaping its padding alike", two(" ok "), [3]string{"=%3d", "", "%3D"}, [3]string{"=%3D", "", "%3D"}},
								{"both its '=' respelled, a run after words escaping its padding alike", two(" ok "), [3]string{"%3d%3d", "", "%3D"}, [3]string{"%3D%3D", "", "%3D"}},
							} {
								rec, live := c.of(recRun.read, c.rec[0], c.rec[1], c.rec[2]), c.of(fRun.read, c.live[0], c.live[1], c.live[2])
								if ln := learnOf(rec, live); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
									t.Errorf("encoded, a padding escape gained, %s, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", c.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
								}
							}
							// Three runs side by side, each join its padding:
							// an escape written alike on both sides, or one
							// that record's full round's value lost, leaves
							// no word of its own: noise, as the plain text
							// is. One it gained is refused, except where
							// another run lost one, so the same word stands
							// on both sides: noise, as the plain text is.
							for _, c := range []struct {
								what                     string
								rec0, live0, rec1, live1 string
								noise                    bool
							}{
								{"all alike", "=%3D", "=%3D", "=%3D", "=%3D", true},
								{"the first raw, the second escape-ended alike", "==", "==", "=%3D", "=%3D", true},
								{"the first escape-ended alike, the second raw", "=%3D", "=%3D", "==", "==", true},
								{"the first one's escape lost, the second alike", "%3D%3D", "=%3D", "=%3D", "=%3D", true},
								{"the first alike, the second one's escape lost", "=%3D", "=%3D", "%3D%3D", "=%3D", true},
								{"the first raw, the second one's escape lost", "==", "==", "%3D%3D", "=%3D", true},
								{"all both escaped", "%3D%3D", "%3D%3D", "%3D%3D", "%3D%3D", true},
								{"the first one's escapes lost, the second one's first gained, so no word changed", "%3D%3D", "==", "=%3D", "%3D%3D", true},
								{"the first alike, the second one's escape gained", "=%3D", "=%3D", "==", "=%3D", false},
								{"the first one's escape gained, the second alike", "==", "=%3D", "=%3D", "=%3D", false},
							} {
								of := three("")
								ln := learnOf(of(recRun.read, c.rec0, c.rec1, "="), of(fRun.read, c.live0, c.live1, "="))
								if c.noise && (ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "") {
									t.Errorf("encoded, three runs side by side, %s, %s, window %+v: %s from %q held %q (%s); want noise of the word", c.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
								} else if !c.noise && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11")) {
									t.Errorf("encoded, three runs side by side, %s, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", c.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
								}
							}
							// An escape inside a run is decided where it
							// stands too: one record's full round's value
							// gained ("%2F" or "\u002F" for the '/' of
							// "b2s/IGE+Yi") leaves the word it glues to the
							// run's next bytes ("2FIGE"), refused even where
							// another run that kept that escape spells the
							// same word, beside it or after words. Written
							// alike on both sides it is noise, as the plain
							// text is, and so is a gained "\/", which leaves
							// no word.
							mid := func(v time.Time, word, slash string) string {
								return strings.ReplaceAll(enc("ok? a>b lead "+dotNetStamp(v)+" "+word), "/", slash)
							}
							for _, v := range []time.Time{recRun.read, fRun.read} {
								if !strings.HasPrefix(mid(v, "created.", "/"), "b2s/IGE+") || !strings.HasPrefix(mid(v, "updated.", "/"), "b2s/IGE+") {
									t.Fatalf("the runs at %v do not start \"b2s/IGE+\", so this row tells nothing", v)
								}
							}
							for _, sp := range []string{"%2F", `\u002F`} {
								for _, c := range []struct {
									what                     string
									between                  string
									rec0, live0, rec1, live1 string
									noise                    bool
								}{
									{"gained in the second run, the first beside it kept it", " ", sp, sp, "/", sp, false},
									{"gained in the second run, the first after words kept it", " ok ", sp, sp, "/", sp, false},
									{"gained in the first run, the second kept it", " ", "/", sp, sp, sp, false},
									{"gained in the one run of two", " ", "/", "/", "/", sp, false},
									{"written alike in both runs", " ", sp, sp, sp, sp, true},
									{"written alike in one run of two", " ok ", sp, sp, "/", "/", true},
									{`a "\/" gained beside a run that kept it`, " ", sp, sp, "/", `\/`, true},
								} {
									of := func(v time.Time, a, b string) string {
										return "tok " + mid(v, "created.", a) + c.between + mid(v, "updated.", b) + " ok"
									}
									ln := learnOf(of(recRun.read, c.rec0, c.rec1), of(fRun.read, c.live0, c.live1))
									if c.noise && (ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "") {
										t.Errorf("encoded, an escape inside a run %q, %s, %s, window %+v: %s from %q held %q (%s); want noise of the word", sp, c.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
									} else if !c.noise && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11")) {
										t.Errorf("encoded, an escape inside a run %q, %s, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", sp, c.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
									}
								}
							}
							// The stated limit, so too inside a run: a run
							// after text that changed in more than clock
							// reads (a ':' made ';') pairs with none, so no
							// recorded run of its rank tells how it writes
							// its escapes, and the word its "%2F" leaves is
							// the value's own even where the run before it,
							// which kept that escape, spells the same word:
							// refused, red, where the plain text is noise.
							// With nothing changed before it, it is noise.
							slashRun := func(v time.Time, word string) string {
								return strings.ReplaceAll(enc("ok? lead "+dotNetStamp(v)+" abc>d "+word), "/", "%2F")
							}
							for _, v := range []time.Time{recRun.read, fRun.read} {
								if r := slashRun(v, "created."); !strings.HasPrefix(r, "b2s%2FIGxlYWQg") || strings.Count(r, "%2F") != 1 || !strings.HasPrefix(slashRun(v, "updated."), r[:strings.IndexByte(r, '+')+1]) {
									t.Fatalf("the runs at %v do not share the word their \"%%2F\" leaves, so this row tells nothing: %q", v, r)
								}
							}
							for _, before := range [][2]string{{":", ";"}, {":", ":"}} {
								of := func(v time.Time, sep string) string {
									return "tok " + slashRun(v, "created.") + sep + slashRun(v, "updated.") + " ok"
								}
								if ln := learnOf(of(recRun.read, before[0]), of(fRun.read, before[1])); before[0] != before[1] && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11")) {
									t.Errorf("encoded, an escape inside a run after %q made %q, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", before[0], before[1], pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
								} else if before[0] == before[1] && (ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "") {
									t.Errorf("encoded, an escape inside a run after %q, %s, window %+v: %s from %q held %q (%s); want noise of the word", before[0], pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
								}
								plainOf := func(v time.Time, sep string) string {
									return "tok ok? lead " + dotNetStamp(v) + " abc>d created." + sep + "ok? lead " + dotNetStamp(v) + " abc>d updated. ok"
								}
								if ln := learnOf(plainOf(recRun.read, before[0]), plainOf(fRun.read, before[1])); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
									t.Errorf("plain, the runs' text after %q made %q, %s, window %+v: %s from %q held %q (%s); want noise of the word", before[0], before[1], pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
								}
							}
							// So too in a run of the URL-safe alphabet whose
							// encoder escapes its '_' ("%5F") and not its
							// '-': the word the first "%5F" leaves runs on
							// past the '-' in the reading that keeps '-' and
							// '_' inside a word ("5FIGE-YiBs..." up to the
							// next "%5F", the read's digits in it), and
							// stops at the '-' in the others ("5FIGE", the
							// same on both sides). That longer word, which
							// the run before it spells too, is searched
							// where the escape stands: refused after a ':'
							// made ';', noise with the ':' kept.
							urlRun := func(v time.Time, word string) string {
								return strings.ReplaceAll(base64.URLEncoding.EncodeToString([]byte("ok? a>b lead "+dotNetStamp(v)+" ab? "+word)), "_", "%5F")
							}
							for _, v := range []time.Time{recRun.read, fRun.read} {
								r := urlRun(v, "created.")
								end := strings.LastIndex(r, "%5F")
								if !strings.HasPrefix(r, "b2s%5FIGE-") || strings.Count(r, "%5F") != 2 || strings.Count(r, "-") != 1 || strings.ContainsAny(r, "=+/") || !strings.HasPrefix(urlRun(v, "updated."), r[:end+3]) {
									t.Fatalf("the URL-safe runs at %v do not share a word that runs past their '-', so this row tells nothing: %q", v, r)
								}
							}
							if a, b := urlRun(recRun.read, "created."), urlRun(fRun.read, "created."); a[:strings.LastIndex(a, "%5F")] == b[:strings.LastIndex(b, "%5F")] {
								t.Fatalf("the word past the '-' holds none of the read's digits, the same on both sides, so this row tells nothing: %q", a)
							}
							for _, before := range [][2]string{{":", ";"}, {":", ":"}} {
								of := func(v time.Time, sep string) string {
									return "tok " + urlRun(v, "created.") + sep + urlRun(v, "updated.") + " ok"
								}
								if ln := learnOf(of(recRun.read, before[0]), of(fRun.read, before[1])); before[0] != before[1] && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11")) {
									t.Errorf("encoded in the URL-safe alphabet, its '_' escaped, after %q made %q, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", before[0], before[1], pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
								} else if before[0] == before[1] && (ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "") {
									t.Errorf("encoded in the URL-safe alphabet, its '_' escaped, after %q, %s, window %+v: %s from %q held %q (%s); want noise of the word", before[0], pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
								}
								plainOf := func(v time.Time, sep string) string {
									return "tok ok? a>b lead " + dotNetStamp(v) + " ab? created." + sep + "ok? a>b lead " + dotNetStamp(v) + " ab? updated. ok"
								}
								if ln := learnOf(plainOf(recRun.read, before[0]), plainOf(fRun.read, before[1])); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
									t.Errorf("plain, the URL-safe runs' text after %q made %q, %s, window %+v: %s from %q held %q (%s); want noise of the word", before[0], before[1], pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
								}
							}
							plainMid := func(v time.Time) string {
								return "tok ok? a>b lead " + dotNetStamp(v) + " created. ok? a>b lead " + dotNetStamp(v) + " updated. ok"
							}
							if ln := learnOf(plainMid(recRun.read), plainMid(fRun.read)); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
								t.Errorf("plain, the two runs' text with a '/' inside, %s, window %+v: %s from %q held %q (%s); want noise of the word", pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
							}
							// The stated limit: a run after text that changed
							// in more than clock reads (a ':' made ';', a
							// space gained before the escape before it)
							// pairs with none, so the word that escape
							// glues to it is the value's own: refused,
							// red, where the plain text laid out the same
							// way is noise. With nothing changed before it,
							// the run is noise.
							oneRun := func(v time.Time) string { return enc("lead " + dotNetStamp(v) + " created") }
							plainRun := func(v time.Time) string {
								return strings.ReplaceAll("lead "+dotNetStamp(v)+" created", " ", "%20")
							}
							for _, before := range [][2]string{{":", ";"}, {"", " "}, {":", ":"}} {
								rec, live := "tok"+before[0]+"%20", "tok"+before[1]+"%20"
								if ln := learnOf(rec+oneRun(recRun.read)+"%20ok", live+oneRun(fRun.read)+"%20ok"); before[0] != before[1] && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11")) {
									t.Errorf("encoded, %q before the escape before it made %q, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", before[0], before[1], pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
								} else if before[0] == before[1] && (ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "") {
									t.Errorf("encoded, %q before the escape before it, %s, window %+v: %s from %q held %q (%s); want noise of the word", before[0], pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
								}
								if ln := learnOf(rec+plainRun(recRun.read)+"%20ok", live+plainRun(fRun.read)+"%20ok"); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
									t.Errorf("plain, %q before the escape before it made %q, %s, window %+v: %s from %q held %q (%s); want noise of the word", before[0], before[1], pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
								}
							}
						}
						if ln := learnOf(esc.rec(shapes[1].of(recRun.read, "created")), esc.live(shapes[1].of(fRun.read, "created"))); esc.liveOnly && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11")) {
							t.Errorf("encoded%s, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", esc.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
						} else if !esc.liveOnly && (ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "") {
							t.Errorf("encoded%s, %s, window %+v: %s from %q held %q (%s); want noise of the word", esc.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
						}
						// The same escape before a run whose first word
						// changed (a text with no '/' or '+' in its base64,
						// one word "bGVhZC..."): the word the escape's
						// remnant glues to it ("20bGVhZC...") is the run's,
						// so the text is noise, as the plain text is.
						if esc.what == ", both escaped" {
							oneWord := func(v time.Time) string { return "tok " + enc("lead "+dotNetStamp(v)+" created") + " ok" }
							if strings.ContainsAny(enc("lead "+dotNetStamp(recRun.read)+" created")+enc("lead "+dotNetStamp(fRun.read)+" created"), "+/") {
								t.Fatalf("the one-word run holds a '+' or '/', so this row tells nothing")
							}
							if ln := learnOf(esc.rec(oneWord(recRun.read)), esc.live(oneWord(fRun.read))); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
								t.Errorf("encoded in one word, both escaped, %s, window %+v: %s from %q held %q (%s); want noise of the word", pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
							}
							// So with a JSON string's "\n" before the run.
							jsonLine := func(v time.Time) string { return strings.Replace(oneWord(v), " ", `\n`, 1) }
							if ln := learnOf(jsonLine(recRun.read), jsonLine(fRun.read)); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
								t.Errorf("encoded in one word after a JSON \\n, %s, window %+v: %s from %q held %q (%s); want noise of the word", pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
							}
							// Two runs that each changed, the second's first
							// word glued to the escape before it: the text
							// from the first run to it is the same, so that
							// word is the second run's too.
							twoRuns := func(v time.Time) string { return "tok " + enc(text(v, "created")) + " ok " + oneWord(v) }
							if ln := learnOf(esc.rec(twoRuns(recRun.read)), esc.live(twoRuns(fRun.read))); ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
								t.Errorf("encoded in two runs that both changed, both escaped, %s, window %+v: %s from %q held %q (%s); want noise of the word", pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
							}
							// A letter beyond ASCII glued to the run with no
							// escape between them ("é<run>") is no escape's
							// remnant: the word that holds both is the
							// value's own, in the raw value as in its
							// decoded form, and refused (fail closed), with
							// an escape after the run or none at all, in
							// base64 and in hex after "0x" (no capital
							// letter, so its case-folded form is itself).
							for _, after := range []string{"%20ok", " ok"} {
								for _, encode := range []func(string) string{enc, func(s string) string { return "0x" + hex.EncodeToString([]byte(s)) }} {
									glued := func(at time.Time) string { return "tok é" + encode("lead "+dotNetStamp(at)+" created") + after }
									if ln := learnOf(glued(recRun.read), glued(fRun.read)); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
										t.Errorf("encoded in one word glued to a letter beyond ASCII (%q), %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", glued(fRun.read), pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
									}
								}
							}
						}
						// The same escape changed before the run ("%20" to
						// "%3A", or a JSON string's "\n" to "\t") is a change
						// of its own too: refused.
						if esc.what == ", both escaped" {
							encRec, encLive := shapes[1].of(recRun.read, "created"), shapes[1].of(fRun.read, "created")
							runA := func(v time.Time) string { return enc(text(v, "created")) }
							runB := enc("second note text")
							for _, ch := range []struct{ what, rec, live string }{
								{"a percent escape", esc.rec(encRec), strings.Replace(esc.live(encLive), "%20", "%3A", 1)},
								{"a backslash escape", strings.Replace(encRec, " ", `\n`, 1), strings.Replace(encLive, " ", `\t`, 1)},
								// The escape record's full round's value has
								// before the run stands elsewhere in the
								// recorded value, not before its run.
								{"a percent escape, the recorded value holding it elsewhere", "a%3Ab " + esc.rec(encRec), "a%3Ab " + strings.Replace(esc.live(encLive), "%20", "%3A", 1)},
								{"a backslash escape, the recorded value holding it elsewhere", strings.Replace(encRec, " ", `\n`, 1) + `\tend`, strings.Replace(encLive, " ", `\t`, 1) + `\tend`},
								// The escape record's full round's value has
								// before the run stands before another run
								// of the recorded value, which did not
								// change: only the escape before the run of
								// the same rank, with the same text from
								// the run before it, makes the glued word
								// the run's.
								{"a percent escape, the recorded value holding it before another run", "tok%20" + runA(recRun.read) + "%20ok%3A" + runB + "%20end", "tok%3A" + runA(fRun.read) + "%20ok%3A" + runB + "%20end"},
								{"a percent escape gained, the recorded value holding it before another run", "x " + runA(recRun.read) + " y%20" + runB, "x%20" + runA(fRun.read) + " y%20" + runB},
								{"a backslash escape, the recorded value holding it before another run", `tok\n` + runA(recRun.read) + ` ok\t` + runB, `tok\t` + runA(fRun.read) + ` ok\t` + runB},
								{"a backslash escape gained, the recorded value holding it before another run", "x " + runA(recRun.read) + ` y\n` + runB, `x\n` + runA(fRun.read) + ` y\n` + runB},
							} {
								if ln := learnOf(ch.rec, ch.live); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
									t.Errorf("encoded, %s before the run changed, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", ch.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
								}
							}
						}
						for _, c := range []struct {
							what string
							of   func(rec bool) string
						}{
							{"a word of the run outside it too", outside},
							{"a word of the run in a longer run that does not decode", inLonger},
							{"a word of the run outside it, no capital letter", lower},
							{"a second copy of the run changed", second},
							{"a word of the run case-folded, outside it", folded},
							{"a word after a run of hex", afterHex},
							{"a word of a run decoded deeper, at its place in the value", deepPlace},
						} {
							if ln := learnOf(esc.rec(c.of(true)), esc.live(c.of(false))); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
								t.Errorf("%s%s, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", c.what, esc.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
							}
						}
						// A hash after "0x" (hex that decodes to no text)
						// changed is refused, as the plain hash is.
						hash := func(v string) string {
							return "tok 0x" + hex.EncodeToString([]byte(v+"\x00\x01\x02\x03\x04\x05\x06\x07")) + " ok"
						}
						if ln := learnOf(esc.rec(hash("recorded")), esc.live(hash("replayed"))); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
							t.Errorf("a hash after 0x%s, %s, window %+v: %s from %q (%s); want it refused as a word that may stand in mock-11", esc.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.Why)
						}
					}
				}
			}
			// A capital letter whose case-folding is shorter ("ẞ", "ß")
			// before the run shifts the case-folded form, where the run then
			// does not stand where the reader read it: it is put out nowhere
			// there, so its case-folded words are searched (fail closed),
			// and nothing reads past the form's end.
			for _, pl := range unread {
				for _, after := range []string{"", " ok"} {
					shifted := func(v time.Time) string { return "ẞ tok " + enc(text(v, "created")) + after }
					func() {
						defer func() {
							if r := recover(); r != nil {
								t.Errorf("a shorter case-folding before the run, then %q, %s: panic %v", after, pl.what, r)
							}
						}()
						leaf := func(v string) Shape {
							return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
						}
						ln := ClassifyIn(clockedDrifts(), []string{p}, leaf(shifted(fRun.read)), leaf(shifted(recRun.read)), recRun.call, nil, pl.raw, nil, serverStampWindow(recRun, fRun))[0]
						if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "may stand in mock-11") {
							t.Errorf("a shorter case-folding before the run, then %q, %s: %s from %q (%s); want it refused as a word that may stand in mock-11", after, pl.what, ln.Kind, ln.From.Mock, ln.Why)
						}
					}()
				}
			}
			// A run F's side spells in the other base64 alphabet, and the
			// same text split over two runs, beside the stamp's run: their
			// text, which is all a run holds, is the recording's, so they
			// are noise, as the plain text is.
			fixed := "ab?ab?ab?" // "YWI/YWI/YWI/", "YWI_YWI_YWI_" in the URL-safe alphabet
			for _, c := range []struct {
				what      string
				rec, live func(v time.Time) string
			}{
				{"a run respelled", func(v time.Time) string { return "tok " + enc(text(v, "created")) + " ok " + enc(fixed) }, func(v time.Time) string {
					return "tok " + enc(text(v, "created")) + " ok " + base64.URLEncoding.EncodeToString([]byte(fixed))
				}},
				{"a text split over two runs", func(v time.Time) string {
					return "tok " + enc(text(v, "created")) + " ok " + enc("lead created by the scheduler\nsecond line of the note") + " end"
				}, func(v time.Time) string {
					return "tok " + enc(text(v, "created")) + " ok " + enc("lead created by the scheduler") + " " + enc("second line of the note") + " end"
				}},
			} {
				for _, pl := range payloads {
					for _, w := range []*Window{serverStampWindow(recRun, fRun), inSpan} {
						leaf := func(v string) Shape {
							return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
						}
						if ln := ClassifyIn(clockedDrifts(), []string{p}, leaf(c.live(fRun.read)), leaf(c.rec(recRun.read)), recRun.call, nil, pl.raw, nil, w)[0]; ln.Kind != KindNoise || ln.Read != nil || ln.From.Mock != "" {
							t.Errorf("%s, %s, window %+v: %s from %q held %q (%s); want noise of the word", c.what, pl.what, *w, ln.Kind, ln.From.Mock, ln.From.Held, ln.Why)
						}
					}
				}
			}
		})
	}
}
