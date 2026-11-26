package learn

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestDecideReadsServerTestPlainClockReadFromTestBeginPastConcurrentServe
// verifies that Decide (order.go) honors Window.fromTestBegin() for plain clock
// reads in server tests when another goroutine's serve lands between the
// handler's clock read and its dependency call — matching CheckClockRead
// (clockread.go) and legacy ClassifyIn (classify.go).
func TestDecideReadsServerTestPlainClockReadFromTestBeginPastConcurrentServe(t *testing.T) {
	r := newConcurrentRun()
	const p = "body.docs.msgs.0.u.$set.created.$date.$numberLong"
	at := func(v time.Time) Leaf { return InstantLeaf(p, p, v, time.Millisecond) }
	recRead := r.recBegin.Add(300 * time.Microsecond) // .933Z

	for _, wr := range r.recordings() {
		t.Run("typed/"+wr.what, func(t *testing.T) {
			tr := r.windowTracker(false)
			paired := &models.Mock{Name: "mock-13", Kind: models.Mongo}
			paired.Spec.ReqTimestampMock = r.recBegin.Add(wr.call)
			w := tr.CallWindowFrom(r.recBegin.Add(wr.prev), paired, r.upTo)
			if w == nil {
				t.Fatal("CallWindowFrom returned nil")
			}
			w = w.ComparesOffsets()

			for _, c := range r.reads() {
				if at(c.v).Value == at(recRead).Value {
					continue
				}
				dc := clockedDrifts()
				in := Input{
					Drifts: dc,
					Drift:  []string{p},
					Rec:    Shape{Leaves: map[string]Leaf{p: at(recRead)}},
					Live:   Shape{Leaves: map[string]Leaf{p: at(c.v)}},
					At:     paired.Spec.ReqTimestampMock,
					Window: w,
					Test:   "post-url-3",
					Mock:   paired.Name,
					Mode:   ModeOwn,
				}
				ds := Decide(in)
				if len(ds) != 1 {
					t.Fatalf("%s: got %d decisions, want 1", c.what, len(ds))
				}
				d := ds[0]
				learned := d.Outcome == OutcomeClock && d.Clock != nil
				if learned != c.ok {
					t.Errorf("%s (%s): outcome=%v clock=%v line=%q; want clock=%v", c.what, at(c.v).Value, d.Outcome, d.Clock, d.Line, c.ok)
					continue
				}
				if !learned {
					continue
				}
				if !d.Clock.Plain() || d.Clock.Since != "" {
					t.Errorf("%s: learned clock %+v; want plain read checked in call span (Since empty)", c.what, *d.Clock)
				}
				s, ok := ClockSpanFrom(WithTracker(context.Background(), tr), r.upTo)
				if ok, why := CheckClockRead(p, *d.Clock, at(c.v), s, ok); !ok {
					t.Errorf("%s: learned %s refused by CheckClockRead: %s", c.what, d.Clock, why)
				}
			}
		})
	}

	t.Run("only_record_had_concurrent_serve", func(t *testing.T) {
		recBegin := time.Date(2026, 11, 26, 15, 0, 43, 215_500_000, time.UTC)
		recRead := recBegin.Add(300 * time.Microsecond) // .215Z
		recPrev := recBegin.Add(700 * time.Microsecond) // .2162Z (> .2160Z)
		recCall := recBegin.Add(1500 * time.Microsecond)

		fBegin := recBegin.Add(FullRoundLead)
		fRead := fBegin.Add(400 * time.Microsecond)
		fCall := fBegin.Add(1200 * time.Microsecond)

		w := (&Window{
			Rec: Span{From: recPrev, To: recCall, TestFrom: recBegin},
			F:   Span{From: fBegin, To: fCall},
		}).ComparesOffsets()

		ds := Decide(Input{
			Drifts: clockedDrifts(),
			Drift:  []string{p},
			Rec:    Shape{Leaves: map[string]Leaf{p: at(recRead)}},
			Live:   Shape{Leaves: map[string]Leaf{p: at(fRead)}},
			At:     recCall,
			Window: w,
			Test:   "post-url-3",
			Mock:   "mock-13",
			Mode:   ModeOwn,
		})
		if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil || !ds[0].Clock.Plain() {
			t.Fatalf("record-only concurrent serve: got %+v; want plain OutcomeClock", ds)
		}
	})

	t.Run("text_iso_timestamp_past_concurrent_serve", func(t *testing.T) {
		const tp = "body.createdAt"
		tr := r.windowTracker(false)
		paired := &models.Mock{Name: "mock-14", Kind: models.HTTP}
		paired.Spec.ReqTimestampMock = r.recBegin.Add(r.recordings()[1].call)
		w := tr.CallWindowFrom(r.recBegin.Add(r.recordings()[1].prev), paired, r.upTo)
		if w == nil {
			t.Fatal("CallWindowFrom returned nil")
		}
		w = w.ComparesOffsets()
		iso := func(v time.Time) Leaf {
			return Leaf{Path: tp, Noise: tp, Type: String, Value: v.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")}
		}
		ds := Decide(Input{
			Drifts: clockedDrifts(),
			Drift:  []string{tp},
			Rec:    Shape{Leaves: map[string]Leaf{tp: iso(recRead)}},
			Live:   Shape{Leaves: map[string]Leaf{tp: iso(r.appRead)}},
			At:     paired.Spec.ReqTimestampMock,
			Window: w,
			Test:   "post-url-3",
			Mock:   paired.Name,
			Mode:   ModeOwn,
		})
		if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil || !ds[0].Clock.Plain() {
			t.Fatalf("text ISO instant past concurrent serve: got %+v; want plain OutcomeClock", ds)
		}
		s, ok := ClockSpanFrom(WithTracker(context.Background(), tr), r.upTo)
		if ok, why := CheckClockRead(tp, *ds[0].Clock, iso(r.appRead), s, ok); !ok {
			t.Fatalf("learned text clock %s refused by CheckClockRead: %s", ds[0].Clock, why)
		}
	})

	t.Run("outside_scope_coarse_answer_beside_concurrent_serve", func(t *testing.T) {
		tr := r.windowTracker(false)
		paired := &models.Mock{Name: "mock-13", Kind: models.Mongo}
		paired.Spec.ReqTimestampMock = r.recBegin.Add(r.recordings()[0].call)
		w := tr.CallWindowFrom(r.recBegin.Add(r.recordings()[0].prev), paired, r.upTo)
		if w == nil {
			t.Fatal("CallWindowFrom returned nil")
		}
		w = w.ComparesOffsets()

		outsideAt := r.recBegin.Add(-50 * time.Millisecond)
		outside := Earlier{
			Mock: "mock-other",
			At:   outsideAt,
			Run:  true,
			Seen: []Seen{{
				Side:  Response,
				Leaf:  Leaf{Path: "body.at", Noise: "body.at", Type: String, Value: recRead.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")},
				Live:  r.appRead.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z"),
				Place: "body.at",
			}},
		}
		ds := Decide(Input{
			Drifts:  clockedDrifts(),
			Drift:   []string{p},
			Rec:     Shape{Leaves: map[string]Leaf{p: at(recRead)}},
			Live:    Shape{Leaves: map[string]Leaf{p: at(r.appRead)}},
			At:      paired.Spec.ReqTimestampMock,
			Earlier: []Earlier{outside},
			Window:  w,
			Test:    "post-url-3",
			Mock:    paired.Name,
			Mode:    ModeOwn,
		})
		if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil || !ds[0].Clock.Plain() {
			t.Fatalf("outside coarse answer beside server test plain read: got %+v; want plain OutcomeClock", ds)
		}
	})

	t.Run("in_scope_answer_finer_instant_beside_concurrent_serve", func(t *testing.T) {
		const sp = "body.atSec"
		recBegin := time.Date(2026, 11, 26, 15, 0, 43, 0, time.UTC)
		fBegin := recBegin.Add(FullRoundLead)
		recRead := recBegin.Add(300 * time.Millisecond)
		fRead := fBegin.Add(400 * time.Millisecond)
		w := (&Window{
			Rec: Span{From: recBegin.Add(500 * time.Millisecond), To: recBegin.Add(800 * time.Millisecond), TestFrom: recBegin},
			F:   Span{From: fBegin.Add(600 * time.Millisecond), To: fBegin.Add(900 * time.Millisecond), TestFrom: fBegin},
		}).ComparesOffsets()
		sec := func(v time.Time) Leaf { return InstantLeaf(sp, sp, v, time.Second) }
		// An in-scope answer holding the same second only as a finer nanosecond
		// timestamp inside a name: cutOnly is true, so windowRead() must see
		// fromTestBegin() and keep the call's own plain clock read instead of
		// binding the answer.
		inScopeAns := Earlier{
			Mock: "mock-setup",
			At:   recBegin.Add(100 * time.Millisecond),
			Own:  true,
			Payload: RawOf(&models.Mock{
				Name: "mock-setup",
				Spec: models.MockSpec{
					ReqTimestampMock: recBegin.Add(100 * time.Millisecond),
					ResTimestampMock: recBegin.Add(150 * time.Millisecond),
				},
			}, true),
			Seen: []Seen{{
				Side:  Response,
				Leaf:  Leaf{Path: "body.tag", Noise: "body.tag", Type: String, Value: "job-" + recRead.UTC().Format("2006-01-02T15:04:05.000000000Z")},
				Live:  "job-" + fRead.UTC().Format("2006-01-02T15:04:05.000000000Z"),
				Place: "body.tag",
			}},
		}
		ds := Decide(Input{
			Drifts:  clockedDrifts(),
			Drift:   []string{sp},
			Rec:     Shape{Leaves: map[string]Leaf{sp: sec(recRead)}},
			Live:    Shape{Leaves: map[string]Leaf{sp: sec(fRead)}},
			At:      recBegin.Add(800 * time.Millisecond),
			Earlier: []Earlier{inScopeAns},
			Window:  w,
			Test:    "post-url-3",
			Mock:    "mock-13",
			Mode:    ModeOwn,
		})
		if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil || !ds[0].Clock.Plain() {
			t.Fatalf("in-scope finer answer beside server test plain read: got %+v; want plain OutcomeClock", ds)
		}
	})
}

// TestDecideServerTestFromTestBeginGuards verifies that Window.fromTestBegin()
// in Decide stays strictly fail-closed: it never accepts reads before TestFrom,
// never learns an offset from TestFrom, never weakens a >=1s look-back refusal,
// never overrides a valid narrow-window offset, and never overrides a same-test
// coarse copy.
func TestDecideServerTestFromTestBeginGuards(t *testing.T) {
	r := newConcurrentRun()
	const p = "body.docs.msgs.0.u.$set.created.$date.$numberLong"
	at := func(v time.Time) Leaf { return InstantLeaf(p, p, v, time.Millisecond) }

	t.Run("refuses_read_before_test_begin", func(t *testing.T) {
		tr := r.windowTracker(false)
		paired := &models.Mock{Name: "mock-13", Kind: models.Mongo}
		paired.Spec.ReqTimestampMock = r.recBegin.Add(r.recordings()[1].call)
		w := tr.CallWindowFrom(r.recBegin.Add(r.recordings()[1].prev), paired, r.upTo).ComparesOffsets()

		staleRec := r.recBegin.Add(-2 * time.Millisecond)
		ds := Decide(Input{
			Drifts: clockedDrifts(),
			Drift:  []string{p},
			Rec:    Shape{Leaves: map[string]Leaf{p: at(staleRec)}},
			Live:   Shape{Leaves: map[string]Leaf{p: at(r.appRead)}},
			At:     paired.Spec.ReqTimestampMock,
			Window: w,
			Test:   "post-url-3",
			Mock:   paired.Name,
			Mode:   ModeOwn,
		})
		if len(ds) != 1 || ds[0].Outcome != OutcomeRefused {
			t.Fatalf("read before recBegin: got %+v; want OutcomeRefused", ds)
		}
	})

	t.Run("refuses_ttl_offset_from_test_begin", func(t *testing.T) {
		tr := r.windowTracker(false)
		paired := &models.Mock{Name: "mock-13", Kind: models.Mongo}
		paired.Spec.ReqTimestampMock = r.recBegin.Add(r.recordings()[0].call)
		w := tr.CallWindowFrom(r.recBegin.Add(r.recordings()[0].prev), paired, r.upTo).ComparesOffsets()

		ttl := 30 * time.Minute
		ds := Decide(Input{
			Drifts: clockedDrifts(),
			Drift:  []string{p},
			Rec:    Shape{Leaves: map[string]Leaf{p: at(r.recBegin.Add(300*time.Microsecond + ttl))}},
			Live:   Shape{Leaves: map[string]Leaf{p: at(r.appRead.Add(ttl))}},
			At:     paired.Spec.ReqTimestampMock,
			Window: w,
			Test:   "post-url-3",
			Mock:   paired.Name,
			Mode:   ModeOwn,
		})
		if len(ds) != 1 || ds[0].Outcome != OutcomeRefused {
			t.Fatalf("TTL offset outside narrow window: got %+v; want OutcomeRefused", ds)
		}
	})

	t.Run("refuses_real_lookback_over_1s_even_with_TestFrom", func(t *testing.T) {
		recBegin := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
		fBegin := recBegin.Add(FullRoundLead)
		w := (&Window{
			Rec: Span{From: recBegin.Add(4 * time.Second), To: recBegin.Add(5 * time.Second), TestFrom: recBegin},
			F:   Span{From: fBegin.Add(4 * time.Second), To: fBegin.Add(5 * time.Second), TestFrom: fBegin},
		}).ComparesOffsets()
		// A real now-2s look-back at ms precision (both 2s before call, inside TestFrom).
		ds := Decide(Input{
			Drifts: clockedDrifts(),
			Drift:  []string{p},
			Rec:    Shape{Leaves: map[string]Leaf{p: at(recBegin.Add(2500 * time.Millisecond))}},
			Live:   Shape{Leaves: map[string]Leaf{p: at(fBegin.Add(2500 * time.Millisecond))}},
			At:     recBegin.Add(5 * time.Second),
			Window: w,
			Test:   "post-url-3",
			Mock:   "mock-13",
			Mode:   ModeOwn,
		})
		if len(ds) != 1 || ds[0].Outcome != OutcomeRefused {
			t.Fatalf("now-2s ms look-back with TestFrom: got %+v; want OutcomeRefused", ds)
		}
	})

	t.Run("keeps_valid_narrow_window_offset_over_TestFrom", func(t *testing.T) {
		recBegin := time.Date(2026, 10, 31, 8, 50, 0, 0, time.UTC)
		fBegin := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
		w := (&Window{
			Rec: Span{From: recBegin.Add(2*time.Minute + 500*time.Millisecond), To: recBegin.Add(2*time.Minute + 1500*time.Millisecond), TestFrom: recBegin},
			F:   Span{From: fBegin.Add(2*time.Minute + 600*time.Millisecond), To: fBegin.Add(2*time.Minute + 1400*time.Millisecond), TestFrom: fBegin},
		}).ComparesOffsets()
		ds := Decide(Input{
			Drifts: clockedDrifts(),
			Drift:  []string{p},
			Rec:    Shape{Leaves: map[string]Leaf{p: at(recBegin.Add(1037 * time.Millisecond))}},
			Live:   Shape{Leaves: map[string]Leaf{p: at(fBegin.Add(789 * time.Millisecond))}},
			At:     recBegin.Add(2*time.Minute + 1500*time.Millisecond),
			Window: w,
			Test:   "post-url-3",
			Mock:   "mock-13",
			Mode:   ModeOwn,
		})
		if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil || ds[0].Clock.Plain() {
			t.Fatalf("narrow-window 2m offset with TestFrom: got %+v; want non-plain offset Clock", ds)
		}
	})

	t.Run("keeps_same_test_coarse_copy_over_TestFrom", func(t *testing.T) {
		recBegin := time.Date(2026, 11, 26, 15, 0, 43, 0, time.UTC)
		fBegin := recBegin.Add(FullRoundLead)
		recRead := recBegin.Add(10 * time.Millisecond)
		fRead := fBegin.Add(12 * time.Millisecond)
		firstAt := recBegin.Add(20 * time.Millisecond)
		secondAt := recBegin.Add(40 * time.Millisecond)

		w := (&Window{
			Rec: Span{From: firstAt, To: secondAt, TestFrom: recBegin},
			F:   Span{From: fBegin.Add(22 * time.Millisecond), To: fBegin.Add(42 * time.Millisecond), TestFrom: fBegin},
		}).ComparesOffsets()
		earlier := []Earlier{{
			Mock:    "mock-first",
			At:      firstAt,
			Own:     true,
			Payload: payloadOf("mock-first", firstAt),
			Seen: []Seen{{
				Side:  Request,
				Leaf:  at(recRead),
				Live:  at(fRead).Value,
				Place: p,
			}},
		}}
		ds := Decide(Input{
			Drifts:  clockedDrifts(),
			Drift:   []string{p},
			Rec:     Shape{Leaves: map[string]Leaf{p: at(recRead)}},
			Live:    Shape{Leaves: map[string]Leaf{p: at(fRead)}},
			At:      secondAt,
			Earlier: earlier,
			Window:  w,
			Test:    "post-url-3",
			Mock:    "mock-second",
			Mode:    ModeOwn,
		})
		if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "mock-first" {
			t.Fatalf("same-test ms copy with TestFrom: got %+v; want OutcomeValue from mock-first", ds)
		}
	})

	t.Run("worker_call_omits_TestFrom_matching_LiveSpanOf", func(t *testing.T) {
		tr := r.windowTracker(false)
		workerMock := &models.Mock{Name: "mock-worker", Kind: models.Mongo}
		workerMock.Spec.ReqTimestampMock = r.recBegin.Add(r.recordings()[0].call)
		tr.SetServeOwnerOf(func(m *models.Mock) ServeOwner {
			if m != nil && m.Name == workerMock.Name {
				return OwnerOther
			}
			return OwnerTest
		})
		w := tr.CallWindowFrom(r.recBegin.Add(r.recordings()[0].prev), workerMock, r.upTo)
		if w == nil {
			t.Fatal("CallWindowFrom returned nil")
		}
		if !w.Rec.TestFrom.IsZero() || !w.F.TestFrom.IsZero() {
			t.Fatalf("OwnerOther CallWindowFrom kept TestFrom (Rec=%v F=%v); want zero matching LiveSpanOf", w.Rec.TestFrom, w.F.TestFrom)
		}
		liveSpan, ok := tr.LiveSpanOf(r.upTo, workerMock)
		if !ok {
			t.Fatal("LiveSpanOf returned !ok")
		}
		if !liveSpan.TestFrom.IsZero() {
			t.Fatalf("LiveSpanOf(worker) kept TestFrom=%v; want zero", liveSpan.TestFrom)
		}

		recRead := r.recBegin.Add(300 * time.Microsecond)
		liveRead := r.appRead
		// Neither Decide nor ClassifyIn may learn a plain read from test begin
		// when the recording belongs to OwnerOther (replay will not attach TestFrom).
		ds := Decide(Input{
			Drifts: clockedDrifts(),
			Drift:  []string{p},
			Rec:    Shape{Leaves: map[string]Leaf{p: at(recRead)}},
			Live:   Shape{Leaves: map[string]Leaf{p: at(liveRead)}},
			At:     workerMock.Spec.ReqTimestampMock,
			Window: w.ComparesOffsets(),
			Test:   "post-url-3",
			Mock:   workerMock.Name,
			Mode:   ModeOwn,
		})
		if len(ds) != 1 || ds[0].Outcome == OutcomeClock {
			t.Fatalf("Decide on OwnerOther worker call learned clock %+v; want refusal matching LiveSpanOf", ds)
		}
		lns := ClassifyIn(
			clockedDrifts(),
			[]string{p},
			Shape{Leaves: map[string]Leaf{p: at(liveRead)}},
			Shape{Leaves: map[string]Leaf{p: at(recRead)}},
			workerMock.Spec.ReqTimestampMock,
			nil, nil, nil,
			w.ComparesOffsets(),
		)
		if len(lns) != 1 || lns[0].Kind == KindClockRead || lns[0].Kind == KindClockOffset {
			t.Fatalf("ClassifyIn on OwnerOther worker call learned clock %+v; want non-clock matching LiveSpanOf", lns)
		}
		if ok, _ := CheckClockRead(p, models.ClockOffset{Res: time.Millisecond}, at(liveRead), liveSpan, true); ok {
			t.Fatal("CheckClockRead unexpectedly accepted read before narrow span on LiveSpanOf(worker)")
		}
	})
}
