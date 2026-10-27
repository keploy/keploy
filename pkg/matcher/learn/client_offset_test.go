package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// classifyClient is ClassifyIn of p, recorded rec and seen live in record's
// full round, in one of keploy mock's client tests, in window w, with the
// call's recorded answer resp.
func (c clockOffsetCall) classifyClient(p string, rec, live Leaf, w *Window, resp ...Leaf) Learned {
	dc := clockedDrifts()
	dc.clientTests = true
	return ClassifyIn(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: live}}, Shape{Leaves: map[string]Leaf{p: rec}}, c.call, nil, nil, resp, w)[0]
}

// millisText is t as a Mongo $date's milliseconds, as recorded (the raw
// reading's text of it).
func millisText(p string, t time.Time) Leaf {
	return Leaf{Path: p, Noise: p, Type: String, Value: strconv.FormatInt(t.UnixMilli(), 10)}
}

// An expiry keploy mock's client test sends (an OAuth code's expires_at =
// now + 30 m, inserted into Mongo) is, in its call's window in both runs, a
// read of the application's clock plus that offset: learned as that offset,
// which every replay compares, both as its protocol types it (a $date) and
// as recorded (its millisecond text, or a time that names its zone), the
// text's offset read in the text's layout. Never the client test's refusal
// that no clock read matched it, and never noise.
func TestAClientTestsClockPlusAnOffsetIsLearnedAsThatOffset(t *testing.T) {
	const p = "body.docs.msgs.{}.expires_at"
	c := newClockOffsetCall()
	ms := time.Millisecond
	rec, live := c.read.Add(30*time.Minute).Truncate(ms), c.fread.Add(30*time.Minute).Truncate(ms)
	want := models.ClockOffset{Min: 30*time.Minute - 40*ms, Max: 30*time.Minute + 11*ms, Res: ms, Layout: models.LayoutTyped}

	ln := c.classifyClient(p, InstantLeaf(p, p, rec, ms), InstantLeaf(p, p, live, ms), c.w.ComparesOffsets())
	if ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != want || ln.Why != clockOffsetEvidence(p, want) {
		t.Errorf("a $date: %s %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Why, want)
	}

	text := func(name string, f func(time.Time) string, layout string) {
		t.Helper()
		r, l := Leaf{Path: p, Noise: p, Type: String, Value: f(rec)}, Leaf{Path: p, Noise: p, Type: String, Value: f(live)}
		ln := c.classifyClient(p, r, l, c.w.ComparesOffsets())
		w := want
		w.Layout = layout
		if ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != w || ln.Why != clockOffsetEvidence(p, w) {
			t.Errorf("%s %q -> %q: %s %+v (%s); want the offset %+v", name, r.Value, l.Value, ln.Kind, ln.Offset, ln.Why, w)
		}
		if _, ok := ln.Clock(); !ok {
			t.Errorf("%s: no clock a replay checks", name)
		}
	}
	text("the $date's millisecond text", func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }, "epoch-ms")
	text("a time that names its zone", func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }, clockTextLayout(rec.Format("2006-01-02T15:04:05.000Z07:00")))
}

// A replay checks such an offset as the learn step read it: the millisecond
// text of a read in the replay's call window plus 30 m holds; one whose time
// to live changed by a minute, or that is a plain read of the clock, fails.
func TestAClientTestsTextOffsetIsCheckedAtReplay(t *testing.T) {
	const p = "body.docs.msgs.{}.expires_at"
	c := newClockOffsetCall()
	ms := time.Millisecond
	ln := c.classifyClient(p, millisText(p, c.read.Add(30*time.Minute)), millisText(p, c.fread.Add(30*time.Minute)), c.w.ComparesOffsets())
	o, ok := ln.Clock()
	if !ok {
		t.Fatalf("%s (%s): no clock to check", ln.Kind, ln.Why)
	}
	call := c.call.Add(48 * time.Hour)
	s := Span{From: call.Add(-30 * ms), To: call}
	for _, tc := range []struct {
		name string
		at   time.Time
		ok   bool
	}{
		{"read in the window, plus 30m", call.Add(-10 * ms).Add(30 * time.Minute), true},
		{"a time to live of 31m", call.Add(-10 * ms).Add(31 * time.Minute), false},
		{"a time to live of 29m", call.Add(-10 * ms).Add(29 * time.Minute), false},
		{"a plain read", call.Add(-10 * ms), false},
	} {
		if got, why := CheckClockRead(p, o, millisText(p, tc.at), s, true); got != tc.ok {
			t.Errorf("%s: %v (%s); want %v", tc.name, got, why, tc.ok)
		}
	}
}

// The client test's refusal stands, saying how to correct a value the
// harness supplies, wherever no constant offset from a read in the call's
// window is learned: the replay does not compare offsets (an HTTP client
// test); the value is built on a read made two seconds before the window
// (its offset reads in seconds, which the offset rule refuses); a wall
// clock of no zone; two layouts; or the call's answer holds the value.
func TestAClientTestsOffsetIsRefusedWhereNoComparedOffsetReads(t *testing.T) {
	const p = "body.docs.msgs.{}.expires_at"
	c := newClockOffsetCall()
	ms := time.Millisecond
	rec, live := c.read.Add(30*time.Minute), c.fread.Add(30*time.Minute)
	refused := func(name string, ln Learned, why string) {
		t.Helper()
		if ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Read != nil || ln.Why != why {
			t.Errorf("%s: %s %+v (%s); want refused: %s", name, ln.Kind, ln.Offset, ln.Why, why)
		}
	}
	millis := clientTestWhy("its characters 1 to 13", true)
	refused("no replay compares an offset", c.classifyClient(p, millisText(p, rec), millisText(p, live), c.w), millis)
	refused("a read two seconds before the window", c.classifyClient(p, millisText(p, c.call.Add(-2*time.Second+30*time.Minute)), millisText(p, c.fcall.Add(-2*time.Second-5*ms+30*time.Minute)), c.w.ComparesOffsets()), millis)
	// The same as its protocol types it: the client test's refusal too.
	typed := func(at time.Time) Leaf { return InstantLeaf(p, p, at.Truncate(ms), ms) }
	refused("a typed read two seconds before the window", c.classifyClient(p, typed(c.call.Add(-2*time.Second+30*time.Minute)), typed(c.fcall.Add(-2*time.Second-5*ms+30*time.Minute)), c.w.ComparesOffsets()), clientTestWhy("", true))

	wall := func(t time.Time) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: t.Format("2006-01-02 15:04:05.000")}
	}
	refused("a wall clock of no zone", c.classifyClient(p, wall(rec), wall(live), c.w.ComparesOffsets()), clientTestWhy("its characters 1 to 23", true))

	iso := Leaf{Path: p, Noise: p, Type: String, Value: live.Truncate(ms).Format("2006-01-02T15:04:05.000Z07:00")}
	if ln := c.classifyClient(p, millisText(p, rec), iso, c.w.ComparesOffsets()); ln.Kind != KindUnaccounted || ln.Offset != nil {
		t.Errorf("two layouts: %s %+v (%s); want refused", ln.Kind, ln.Offset, ln.Why)
	}

	answer := millisText("body.cursor.firstBatch[].expires_at", rec)
	refused("the call's answer holds it", c.classifyClient(p, millisText(p, rec), millisText(p, live), c.w.ComparesOffsets(), answer), millis)
}

// A cutoff keploy mock's client test sends (a login code's lookup, createdAt
// $gte now - 120 s, in a Mongo findAndModify) is, in its call's window in
// both runs, a read of the application's clock less that time: learned as
// an offset below zero under the rules of one above it, both as recorded
// (its millisecond text) and as its protocol types it (a $date), and
// checked at every replay: a read in the replay's window less two minutes
// holds; one less one or three minutes, the same time after the read, or a
// plain read fail.
func TestAClientTestsClockLessAnOffsetIsLearnedAndChecked(t *testing.T) {
	const q = "body.cmd.query.createdAt.{}"
	c := newClockOffsetCall()
	ms, min2 := time.Millisecond, 2*time.Minute
	want := models.ClockOffset{Min: -min2 - 40*ms, Max: -min2 + 11*ms, Res: ms, Layout: "epoch-ms"}
	ln := c.classifyClient(q, millisText(q, c.read.Add(-min2)), millisText(q, c.fread.Add(-min2)), c.w.ComparesOffsets())
	if ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != want || ln.Why != clockOffsetEvidence(q, want) {
		t.Fatalf("its millisecond text: %s %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Why, want)
	}
	if !strings.Contains(ln.Why, q+" = clock - 2m (") {
		t.Errorf("the evidence does not read the offset below zero: %s", ln.Why)
	}
	typed := want
	typed.Layout = models.LayoutTyped
	rec, live := c.read.Add(-min2).Truncate(ms), c.fread.Add(-min2).Truncate(ms)
	if ln := c.classifyClient(q, InstantLeaf(q, q, rec, ms), InstantLeaf(q, q, live, ms), c.w.ComparesOffsets()); ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != typed {
		t.Errorf("a $date: %s %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Why, typed)
	}

	o, ok := ln.Clock()
	if !ok {
		t.Fatalf("%s (%s): no clock to check", ln.Kind, ln.Why)
	}
	call := c.call.Add(48 * time.Hour)
	s := Span{From: call.Add(-30 * ms), To: call}
	for _, tc := range []struct {
		name string
		at   time.Time
		ok   bool
	}{
		{"read in the window, less 2m", call.Add(-10 * ms).Add(-min2), true},
		{"less 1m", call.Add(-10 * ms).Add(-time.Minute), false},
		{"less 3m", call.Add(-10 * ms).Add(-3 * time.Minute), false},
		{"plus 2m", call.Add(-10 * ms).Add(min2), false},
		{"a plain read", call.Add(-10 * ms), false},
	} {
		if got, why := CheckClockRead(q, o, millisText(q, tc.at), s, true); got != tc.ok {
			t.Errorf("%s: %v (%s); want %v", tc.name, got, why, tc.ok)
		}
	}
}

// An offset below zero is learned only as one above it is: the client
// test's refusal stands where the two runs show no one offset (two minutes
// before at record, one minute before, or two minutes after, in record's
// full round), where the value was read two seconds before its call's
// window (an offset that reads in seconds), where the call's answer holds
// it, and where the replay compares no offset (an HTTP client test).
func TestAClientTestsClockLessAnOffsetIsRefusedWhereNoOneOffsetReads(t *testing.T) {
	const q = "body.cmd.query.createdAt.{}"
	c := newClockOffsetCall()
	ms, min2 := time.Millisecond, 2*time.Minute
	millis := clientTestWhy("its characters 1 to 13", true)
	for _, tc := range []struct {
		name      string
		rec, live time.Time
		w         *Window
		resp      []Leaf
	}{
		{"two minutes before, then one minute before", c.read.Add(-min2), c.fread.Add(-time.Minute), c.w.ComparesOffsets(), nil},
		{"two minutes before, then two minutes after", c.read.Add(-min2), c.fread.Add(min2), c.w.ComparesOffsets(), nil},
		{"a read two seconds before the window", c.call.Add(-2 * time.Second), c.fcall.Add(-2*time.Second - 5*ms), c.w.ComparesOffsets(), nil},
		{"the call's answer holds it", c.read.Add(-min2), c.fread.Add(-min2), c.w.ComparesOffsets(), []Leaf{millisText("body.cursor.firstBatch[].createdAt", c.read.Add(-min2))}},
		{"no replay compares an offset", c.read.Add(-min2), c.fread.Add(-min2), c.w, nil},
	} {
		ln := c.classifyClient(q, millisText(q, tc.rec), millisText(q, tc.live), tc.w, tc.resp...)
		if ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Read != nil || ln.Why != millis {
			t.Errorf("%s: %s %+v (%s); want refused: %s", tc.name, ln.Kind, ln.Offset, ln.Why, millis)
		}
		typed := func(at time.Time) Leaf { return InstantLeaf(q, q, at.Truncate(ms), ms) }
		if tc.resp != nil {
			continue
		}
		if ln := c.classifyClient(q, typed(tc.rec), typed(tc.live), tc.w); ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Read != nil || ln.Why != clientTestWhy("", true) {
			t.Errorf("%s, a $date: %s %+v (%s); want refused: %s", tc.name, ln.Kind, ln.Offset, ln.Why, clientTestWhy("", true))
		}
	}
}

// keploy test learns an offset below zero as keploy mock's client tests do
// where the lead does not read it as a plain read made before its call:
// record's full round made its call five seconds later past the lead than
// the recording, so a typed cutoff two minutes before its call's window in
// both runs moved by the lead and those five seconds, and is learned as that
// offset. Where the offset rule refuses one, the reason keploy test gave a
// value before its call's window before stands: a read two seconds before
// the window, or two minutes before at record and one minute before in
// record's full round, is no read of the clock, never an offset that reads
// in seconds.
func TestAKeployTestsClockLessAnOffsetIsLearnedAndAnEarlierReadKeepsItsReason(t *testing.T) {
	const p = "body.filter.createdAt.$gte"
	c := newClockOffsetCall()
	c.fcall = c.call.Add(FullRoundLead + 5*time.Second)
	c.fread = c.fcall.Add(-45 * time.Millisecond)
	c.w = &Window{Rec: c.w.Rec, F: Span{From: c.fcall.Add(-60 * time.Millisecond), To: c.fcall}}
	ms, min2 := time.Millisecond, 2*time.Minute
	rec, live := c.read.Add(-min2).Truncate(ms), c.fread.Add(-min2).Truncate(ms)
	// Res is the millisecond both values are written at: the 15 ms they share
	// by chance is no step of models.CutOf, and the learn step reads a cut
	// only at those steps (offsetReading), never a chance granularity.
	if g := sharedGrain(rec, live); g != 15*ms {
		t.Fatalf("the two values share %v, want the 15 ms this row pins", g)
	}
	want := models.ClockOffset{Min: -min2 - 40*ms, Max: -min2 + 11*ms, Res: ms, Layout: models.LayoutTyped}
	if ln := c.classify(p, rec, live, ms, c.w.ComparesOffsets()); ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != want || ln.Why != clockOffsetEvidence(p, want) {
		t.Errorf("two minutes before: %s %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Why, want)
	}
	for _, tc := range []struct {
		name      string
		rec, live time.Time
	}{
		{"a read two seconds before the window", c.call.Add(-2 * time.Second), c.fcall.Add(-2*time.Second - 5*ms)},
		{"two minutes before, then one minute before", c.read.Add(-min2), c.fread.Add(-time.Minute)},
	} {
		if ln := c.classify(p, tc.rec.Truncate(ms), tc.live.Truncate(ms), ms, c.w.ComparesOffsets()); ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Why != instantNotClock {
			t.Errorf("%s: %s %+v (%s); want refused: %s", tc.name, ln.Kind, ln.Offset, ln.Why, instantNotClock)
		}
	}
}

// clockOffsetOf shows an offset only where the value stands on one side of
// its call's window, the same side in both runs: after every read in it in
// both (above zero), or before every read in both (below zero). A value
// inside the recording's wide window that record's full round saw two
// minutes past, or two minutes before, its narrow window shows none, though
// the two runs' bounds meet at two minutes: the recording shows a read made
// in its window, not an offset.
func TestAClockOffsetIsOnOneSideOfItsWindowInBothRuns(t *testing.T) {
	ms, min2 := time.Millisecond, 2*time.Minute
	call := time.Date(2026, 10, 26, 11, 14, 17, 155e6, time.UTC)
	fcall := call.Add(FullRoundLead + 37*ms)
	at := func(t time.Time) typedInstant { return typedInstant{at: t, res: ms, names: namesMoment} }
	narrow := &Window{Rec: Span{From: call.Add(-50 * ms), To: call}, F: Span{From: fcall.Add(-60 * ms), To: fcall}}
	wide := &Window{Rec: Span{From: call.Add(-3 * time.Minute), To: call}, F: narrow.F}
	for _, tc := range []struct {
		name      string
		rec, live time.Time
		w         *Window
		sign      int
	}{
		{"after both windows", call.Add(-40*ms + min2), fcall.Add(-45*ms + min2), narrow, 1},
		{"before both windows", call.Add(-40*ms - min2), fcall.Add(-45*ms - min2), narrow, -1},
		{"in both windows", call.Add(-40 * ms), fcall.Add(-45 * ms), narrow, 0},
		{"in the recording's wide window, two minutes past the full round's", call.Add(-time.Minute), fcall.Add(-45*ms + min2), wide, 0},
		{"in the recording's wide window, two minutes before the full round's", call.Add(-time.Minute), fcall.Add(-45*ms - min2), &Window{Rec: Span{From: call.Add(-3 * time.Minute), To: call.Add(3 * time.Minute)}, F: narrow.F}, 0},
		{"before the recording's window, after the full round's", call.Add(-40*ms - min2), fcall.Add(-45*ms + min2), narrow, 0},
		// Its millisecond ends where the recording's two-minute window
		// starts: every offset that window holds it at is below zero.
		{"its cut ending at the recording's wide window's start, two minutes before the full round's", call.Add(-min2), fcall.Add(-45*ms - min2), &Window{Rec: Span{From: call.Add(-min2 + ms), To: call.Add(11 * ms)}, F: narrow.F}, -1},
		// Its millisecond holds the recording's window's start: a read
		// made there, no offset.
		{"its cut holding the recording's wide window's start", call.Add(-min2), fcall.Add(-45*ms - min2), &Window{Rec: Span{From: call.Add(-min2 + ms - 1), To: call.Add(11 * ms)}, F: narrow.F}, 0},
	} {
		o, ok := clockOffsetOf(at(tc.rec), at(tc.live), tc.w)
		switch {
		case tc.sign == 0 && ok:
			t.Errorf("%s: an offset %+v, want none", tc.name, o)
		case tc.sign > 0 && (!ok || o.Min <= 0):
			t.Errorf("%s: %+v %v, want one above zero", tc.name, o, ok)
		case tc.sign < 0 && (!ok || o.Max >= 0):
			t.Errorf("%s: %+v %v, want one below zero", tc.name, o, ok)
		}
	}
}
