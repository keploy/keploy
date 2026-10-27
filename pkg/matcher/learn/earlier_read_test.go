package learn

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"gopkg.in/yaml.v3"
)

// earlierReadRun is one run of a client test whose application reads its
// clock once for a request and writes it at a later call (an audit row's
// createdAt and auditedAtNanos, read before the app looks the row's
// subject up): the test's begin, the previous data-plane call, the call
// that carries the value, and the read, on that run's clock.
type earlierReadRun struct{ began, prev, call, read time.Time }

// earlierReadWindow is the window of the call of rec and f, two runs of
// one client test, as the tracker builds it: each run's span from its
// previous call to the call, carrying its test's begin (Span.Began).
func earlierReadWindow(rec, f earlierReadRun) *Window {
	return &Window{
		Rec: Span{From: rec.prev, To: rec.call, Began: rec.began},
		F:   Span{From: f.prev, To: f.call, Began: f.began},
	}
}

// The api-server suite's shape: the recording's test began at 18:15:20.0,
// the app read its clock at 18:15:20.4 (an audit event's createdAt), then
// looked the app up (the previous call, 18:15:22.1) and wrote the audit
// row (18:15:22.5). Record's full round ran the test a minute later and
// slower: the read at 1.3 s after its begin, the look-up at 9.6 s.
var (
	earlierRec = earlierReadRun{
		began: time.Date(2026, 10, 26, 18, 15, 20, 0, time.UTC),
		read:  time.Date(2026, 10, 26, 18, 15, 20, 400_000_000, time.UTC),
		prev:  time.Date(2026, 10, 26, 18, 15, 22, 100_000_000, time.UTC),
		call:  time.Date(2026, 10, 26, 18, 15, 22, 500_000_000, time.UTC),
	}
	earlierF = earlierReadRun{
		began: time.Date(2026, 10, 26, 18, 16, 20, 0, time.UTC),
		read:  time.Date(2026, 10, 26, 18, 16, 21, 300_000_000, time.UTC),
		prev:  time.Date(2026, 10, 26, 18, 16, 29, 600_000_000, time.UTC),
		call:  time.Date(2026, 10, 26, 18, 16, 30, 100_000_000, time.UTC),
	}
)

func nanosText(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }
func secsText(t time.Time) string  { return strconv.FormatInt(t.Unix(), 10) }

// In keploy mock's client tests a value that is one instant whole in both
// runs, read after its test began and before its call's window (the
// previous call), is a read of the application's clock all the same: it is
// learned as one (KindClockRead), never as noise, and its read says a
// replay checks it from its test's begin (models.SinceTestBegin). Read
// before the test began in either run, or with no begin known, it is
// refused as before; and a changed instant inside a longer value, which the
// word rule would learn as noise, is never learned from the wider window.
func TestAClientTestLearnsAReadMadeBeforeItsCallsWindowAsAReadSinceItsTestBegan(t *testing.T) {
	const p = "body.docs.msgs.{}.auditedAtNanos"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	w := earlierReadWindow(earlierRec, earlierF)
	noBegin := &Window{Rec: Span{From: earlierRec.prev, To: earlierRec.call}, F: Span{From: earlierF.prev, To: earlierF.call}}
	before := func(r earlierReadRun) earlierReadRun { r.read = r.began.Add(-300 * time.Millisecond); return r }
	for _, tc := range []struct {
		name      string
		rec, live string
		w         *Window
		read      bool   // learned as a read since the test began
		part      string // else refused, naming this part
	}{
		{"epoch nanoseconds read before the previous call", nanosText(earlierRec.read), nanosText(earlierF.read), w, true, ""},
		{"epoch seconds read before the previous call", secsText(earlierRec.read), secsText(earlierF.read), w, true, ""},
		{"an ISO time read before the previous call", earlierRec.read.Format(time.RFC3339Nano), earlierF.read.Format(time.RFC3339Nano), w, true, ""},
		{"no begin known", nanosText(earlierRec.read), nanosText(earlierF.read), noBegin, false, "its characters 1 to 19"},
		{"no begin known in the recording", nanosText(earlierRec.read), nanosText(earlierF.read), &Window{Rec: noBegin.Rec, F: w.F}, false, "its characters 1 to 19"},
		{"no begin known in full round", nanosText(earlierRec.read), nanosText(earlierF.read), &Window{Rec: w.Rec, F: noBegin.F}, false, "its characters 1 to 19"},
		// One run read in its call's window, the other before it, where the
		// other's begin is not known: no window from both begins holds it.
		{"no begin known in full round, read in its call's window there", nanosText(earlierRec.read), nanosText(earlierF.prev.Add(100 * time.Millisecond)), &Window{Rec: w.Rec, F: noBegin.F}, false, "its characters 1 to 19"},
		{"no begin known in the recording, read in its call's window there", nanosText(earlierRec.prev.Add(100 * time.Millisecond)), nanosText(earlierF.read), &Window{Rec: noBegin.Rec, F: w.F}, false, "its characters 1 to 19"},
		{"read before the recording's test began", nanosText(before(earlierRec).read), nanosText(earlierF.read), w, false, "its characters 1 to 19"},
		{"read before full round's test began", nanosText(earlierRec.read), nanosText(before(earlierF).read), w, false, "its characters 1 to 19"},
		{"inside a longer value", "audited " + nanosText(earlierRec.read), "audited " + nanosText(earlierF.read), w, false, "its characters 9 to 27"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := ClassifyIn(clientTestCache(), []string{p}, str(tc.live), str(tc.rec), earlierRec.call, nil, nil, nil, tc.w)[0]
			if !tc.read {
				if ln.Kind != KindUnaccounted || !clientTestRefusal(ln.Why, tc.part, true, tc.rec, tc.live) {
					t.Fatalf("%q -> %q: %s Read=%+v (%s); want it refused, naming %s", tc.rec, tc.live, ln.Kind, ln.Read, ln.Why, tc.part)
				}
				return
			}
			o, ok := ln.Clock()
			if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin {
				t.Fatalf("%q -> %q: %s Read=%+v (%s); want a plain read of the clock a replay checks since its test began", tc.rec, tc.live, ln.Kind, ln.Read, ln.Why)
			}
			// Read in its call's window it is a read as before, checked
			// from its call's window.
			in := earlierReadWindow(earlierRec, earlierF)
			in.Rec.From, in.F.From = earlierRec.began, earlierF.began
			ln = ClassifyIn(clientTestCache(), []string{p}, str(tc.live), str(tc.rec), earlierRec.call, nil, nil, nil, in)[0]
			if o, ok := ln.Clock(); ln.Kind != KindClockRead || !ok || o.Since != "" {
				t.Fatalf("in its call's window: %s Read=%+v (%s); want a read checked from its call's window", ln.Kind, ln.Read, ln.Why)
			}
		})
	}
	// A protocol's number (a BSON int64 of epoch nanoseconds, epoch
	// seconds), as the Mongo parser hands it: the same.
	for _, f := range []func(time.Time) string{nanosText, secsText} {
		num := func(v string) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: Number, Value: v}}}
		}
		rv, lv := f(earlierRec.read), f(earlierF.read)
		ln := ClassifyIn(clientTestCache(), []string{p}, num(lv), num(rv), earlierRec.call, nil, nil, nil, w)[0]
		if o, ok := ln.Clock(); ln.Kind != KindClockRead || !ok || o.Since != models.SinceTestBegin {
			t.Fatalf("a number %s -> %s: %s Read=%+v (%s); want a read since its test began", rv, lv, ln.Kind, ln.Read, ln.Why)
		}
	}
	// A typed instant of its protocol (a BSON datetime): the same, and
	// refused where it was read before the test began.
	typed := func(at time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, time.Millisecond)}}
	}
	ms := func(t time.Time) time.Time { return t.Truncate(time.Millisecond) }
	ln := ClassifyIn(clientTestCache(), []string{p}, typed(ms(earlierF.read)), typed(ms(earlierRec.read)), earlierRec.call, nil, nil, nil, w)[0]
	if o, ok := ln.Clock(); ln.Kind != KindClockRead || !ok || o.Since != models.SinceTestBegin || o.Layout != models.LayoutTyped {
		t.Fatalf("a typed instant read before its call's window: %s Read=%+v (%s); want a typed read since its test began", ln.Kind, ln.Read, ln.Why)
	}
	ln = ClassifyIn(clientTestCache(), []string{p}, typed(ms(earlierF.read)), typed(ms(before(earlierRec).read)), earlierRec.call, nil, nil, nil, w)[0]
	if ln.Kind != KindUnaccounted || !clientTestRefusal(ln.Why, "", true) {
		t.Fatalf("a typed instant read before its test began: %s Read=%+v (%s); want it refused", ln.Kind, ln.Read, ln.Why)
	}
	// The call's own answer holds the recorded stamp: in the wider window
	// it is an echo, which the earlier-read window never takes (only a read
	// of the clock it learns there is): it stays refused.
	resp := []Leaf{{Path: "body.received", Noise: "body.received", Type: String, Value: nanosText(earlierRec.read)}}
	ln = ClassifyIn(clientTestCache(), []string{p}, str(nanosText(earlierF.read)), str(nanosText(earlierRec.read)), earlierRec.call, nil, nil, resp, w)[0]
	if ln.Kind != KindUnaccounted || !clientTestRefusal(ln.Why, "its characters 1 to 19", true) {
		t.Fatalf("an echoed stamp read before its call's window: %s (%s); want it refused", ln.Kind, ln.Why)
	}
}

// A replay checks a read learned since its test began (models.SinceTestBegin)
// from where its test began on the application's clock (Span.Began) to its
// call: a value read there passes, one read before the test began fails,
// naming the span; where the replay cannot tell its test's begin it refuses
// the value, whatever it is. A read learned in its call's window is checked
// there as before, and a read of a Since this keploy does not read is
// refused whatever the call carries.
func TestAReplayChecksAReadLearnedSinceItsTestBeganFromItsTestsBegin(t *testing.T) {
	const path = "body.docs.msgs.{}.auditedAtNanos"
	began := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	s := Span{From: began.Add(2 * time.Second), To: began.Add(2500 * time.Millisecond), Began: began}
	leaf := func(at time.Time) Leaf { return Leaf{Path: path, Noise: path, Type: String, Value: nanosText(at)} }
	since := models.ClockOffset{Res: time.Nanosecond, Layout: "epoch-ns", Since: models.SinceTestBegin}
	call := since
	call.Since = ""
	early, inCall, beforeBegin := began.Add(700*time.Millisecond), began.Add(2200*time.Millisecond), began.Add(-time.Millisecond)
	for _, tc := range []struct {
		name string
		o    models.ClockOffset
		at   time.Time
		s    Span
		ok   bool
		why  string
	}{
		{"since the test began, read before the previous call", since, early, s, true, ""},
		{"since the test began, read in the call's window", since, inCall, s, true, ""},
		{"since the test began, read before it began", since, beforeBegin, s, false, "made after this test began and no later than this call: it changed"},
		{"since the test began, after the call", since, s.To.Add(time.Millisecond), s, false, "made after this test began and no later than this call: it changed"},
		{"since the test began, no begin known", since, early, Span{From: s.From, To: s.To}, false, "which a replay checks from where its test began on the application's clock, but keploy cannot tell that at this call"},
		{"in its call's window, read before the previous call", call, early, s, false, "made in this call's window: it changed"},
		{"in its call's window, read in it", call, inCall, s, true, ""},
		{"a start this keploy does not read", models.ClockOffset{Res: time.Nanosecond, Layout: "epoch-ns", Since: "set"}, inCall, s, false, "rather than check it in a span its learn step did not read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := CheckClockRead(path, tc.o, leaf(tc.at), tc.s, true)
			if ok != tc.ok || !strings.Contains(why, tc.why) || ok && why != "" {
				t.Fatalf("CheckClockRead = %v, %q; want %v, naming %q", ok, why, tc.ok, tc.why)
			}
			if !tc.ok && !strings.HasPrefix(why, path+" was learned as ") {
				t.Fatalf("reason %q names no place", why)
			}
			if h := ClockReadHolds(tc.o, leaf(tc.at), tc.s); h != tc.ok {
				t.Fatalf("ClockReadHolds = %v; want %v", h, tc.ok)
			}
		})
	}
}

// A client test's replay span carries its test's begin on the app's clock
// (Span.Began) where the clock ran as one from the begin to the call; keploy
// test's carries none, and neither does one whose clock the begin moved
// after the test began.
func TestALiveSpanCarriesItsClientTestsBegin(t *testing.T) {
	base := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	moved := base.Add(time.Second)
	off := 42 * time.Second
	clock := func(r time.Time) (time.Time, bool) {
		if r.Before(moved) {
			return r, true
		}
		return r.Add(off), true
	}
	served, arrival := moved.Add(2*time.Second), moved.Add(3*time.Second)
	for _, tc := range []struct {
		name   string
		client bool
		began  time.Time
		want   time.Time // Began on the app's clock, zero for none
	}{
		{"a client test", true, moved.Add(time.Millisecond), moved.Add(time.Millisecond + off)},
		{"a client test whose clock moved after its begin", true, moved.Add(-time.Millisecond), time.Time{}},
		{"keploy test", false, moved.Add(time.Millisecond), time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			tr.SetClientTests(tc.client)
			tr.SetAppClock(clock)
			tr.BeginAt(tc.began)
			tr.ServedMockAt(&models.Mock{Name: "mock-0", Kind: models.HTTP}, served)
			s, ok := tr.LiveSpan(arrival)
			if !ok || !s.From.Equal(served.Add(off)) || !s.To.Equal(arrival.Add(off)) || !s.Began.Equal(tc.want) {
				t.Fatalf("LiveSpan = %+v, %v; want from the serve, Began %v", s, ok, tc.want)
			}
		})
	}
}

// Two learnings of one place meet from the test's begin where either was
// learned so (it holds every read its call's window does), and as a start
// this keploy does not read where either is one, which holds nothing; the
// read names its start, and Since is written only where set.
func TestAClockReadsSinceMeetsAndIsWrittenOnlyWhereSet(t *testing.T) {
	o := models.ClockOffset{Res: time.Second}
	test := o
	test.Since = models.SinceTestBegin
	odd := o
	odd.Since = "set"
	for _, tc := range []struct {
		a, b models.ClockOffset
		want string
	}{
		{o, o, ""}, {o, test, models.SinceTestBegin}, {test, o, models.SinceTestBegin}, {test, test, models.SinceTestBegin},
		{test, odd, "set"}, {odd, test, "set"}, {o, odd, "set"}, {odd, o, "set"},
	} {
		if got := tc.a.Meet(tc.b).Since; got != tc.want {
			t.Errorf("%q meets %q as %q; want %q", tc.a.Since, tc.b.Since, got, tc.want)
		}
	}
	if !o.SinceKnown() || !test.SinceKnown() || odd.SinceKnown() {
		t.Fatalf("SinceKnown: %v %v %v; want true true false", o.SinceKnown(), test.SinceKnown(), odd.SinceKnown())
	}
	if s := test.String(); !strings.Contains(s, "read after its test began and before its call") {
		t.Errorf("String = %q; want it to name the read's start", s)
	}
	if s := o.String(); strings.Contains(s, "test began") {
		t.Errorf("String = %q; want no start named", s)
	}
	if s := odd.String(); !strings.Contains(s, "a start this keploy does not read") {
		t.Errorf("String = %q; want it to say keploy does not read the start", s)
	}
	for _, c := range []struct {
		o    models.ClockOffset
		want bool
	}{{o, false}, {test, true}} {
		j, _ := json.Marshal(c.o)
		y, _ := yaml.Marshal(c.o)
		if strings.Contains(string(j), `"since"`) != c.want || strings.Contains(string(y), "since:") != c.want {
			t.Errorf("%+v written as %s / %s; want since written: %v", c.o, j, y, c.want)
		}
		var back models.ClockOffset
		if err := yaml.Unmarshal(y, &back); err != nil || back != c.o {
			t.Errorf("%+v read back as %+v (%v)", c.o, back, err)
		}
	}
}
