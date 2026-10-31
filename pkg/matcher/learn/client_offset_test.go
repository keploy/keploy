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
	// Two layouts the instant reading pairs, as one form of time: a moment
	// with a T and with a space, and epoch milliseconds and seconds. Each is
	// read as a read of the clock plus the same offset, but no layout holds
	// both, so no replay could read the value in its form: refused.
	for _, two := range []struct {
		name string
		r, l Leaf
	}{
		{"a T and a space", Leaf{Path: p, Noise: p, Type: String, Value: rec.Truncate(ms).Format("2006-01-02T15:04:05.000Z07:00")}, Leaf{Path: p, Noise: p, Type: String, Value: live.Truncate(ms).Format("2006-01-02 15:04:05.000Z07:00")}},
		{"epoch milliseconds and seconds", millisText(p, rec), Leaf{Path: p, Noise: p, Type: String, Value: strconv.FormatInt(live.Unix(), 10)}},
	} {
		if ln := c.classifyClient(p, two.r, two.l, c.w.ComparesOffsets()); ln.Kind != KindUnaccounted || ln.Offset != nil {
			t.Errorf("two layouts, %s: %s %+v (%s); want refused", two.name, ln.Kind, ln.Offset, ln.Why)
		}
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
// where the lead does not move it too (one it moves:
// TestAKeployTestTriesAValueBeforeItsWindowAsAnOffsetBelowZeroFirst):
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

// keploy test tries a value made before its call's window as an offset below
// zero first, and only then as a plain read: a cutoff two minutes before its
// call's window in both runs (createdAt $gte now - 2m), which moved by the
// lead record's full round runs ahead of the recording, as a read made no
// later than its call would, is learned as that offset, as its protocol
// types it (a $date) and as recorded (its millisecond text), and every
// replay checks it in its call's span: a read in the replay's window less
// two minutes holds; one less one or three minutes, or a plain read, fails.
// Where the offset rule refuses it (a read two seconds before the window,
// an offset that reads in seconds; a replay that compares no offset; the
// call's answer holding it), keploy test learns it as before: a plain read
// of the clock, or the answer's echo.
func TestAKeployTestTriesAValueBeforeItsWindowAsAnOffsetBelowZeroFirst(t *testing.T) {
	const p = "body.filter.createdAt.$gte"
	c := newClockOffsetCall()
	ms, min2 := time.Millisecond, 2*time.Minute
	text := func(rec, live time.Time, w *Window, resp ...Leaf) Learned {
		return ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: millisText(p, live)}}, Shape{Leaves: map[string]Leaf{p: millisText(p, rec)}}, c.call, nil, nil, resp, w)[0]
	}
	rec, live := c.read.Add(-min2).Truncate(ms), c.fread.Add(-min2).Truncate(ms)
	if !leadMoved(rec, live, ms, ms, false) {
		t.Fatalf("the cutoff %s, then %s, did not move by the lead", rec, live)
	}
	want := models.ClockOffset{Min: -min2 - 40*ms, Max: -min2 + 11*ms, Res: ms, Layout: models.LayoutTyped}
	ln := c.classify(p, rec, live, ms, c.w.ComparesOffsets())
	if ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != want || ln.Read != nil || ln.Why != clockOffsetEvidence(p, want) {
		t.Fatalf("a $date: %s %+v %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Read, ln.Why, want)
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
		{"a plain read", call.Add(-10 * ms), false},
	} {
		if got, why := CheckClockRead(p, o, InstantLeaf(p, p, tc.at.Truncate(ms), ms), s, true); got != tc.ok {
			t.Errorf("a $date %s: %v (%s); want %v", tc.name, got, why, tc.ok)
		}
	}
	millis := models.ClockOffset{Min: -min2 - 40*ms, Max: -min2 + 11*ms, Res: ms, Layout: "epoch-ms"}
	if ln := text(c.read.Add(-min2), c.fread.Add(-min2), c.w.ComparesOffsets()); ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != millis || ln.Read != nil || ln.Why != clockOffsetEvidence(p, millis) {
		t.Errorf("its millisecond text: %s %+v %+v (%s); want the offset %+v", ln.Kind, ln.Offset, ln.Read, ln.Why, millis)
	}

	answer := func(at time.Time) Leaf {
		return millisText("body.cursor.firstBatch[].createdAt", at)
	}
	for _, tc := range []struct {
		name      string
		rec, live time.Time
		w         *Window
		resp      []Leaf
		kind      Kind
	}{
		{"a read two seconds before the window", c.read.Add(-2 * time.Second), c.fread.Add(-2 * time.Second), c.w.ComparesOffsets(), nil, KindClockRead},
		{"no replay compares an offset", c.read.Add(-min2), c.fread.Add(-min2), c.w, nil, KindClockRead},
		{"the call's answer holds it", c.read.Add(-min2), c.fread.Add(-min2), c.w.ComparesOffsets(), []Leaf{answer(c.read.Add(-min2))}, KindEcho},
	} {
		r, l := tc.rec.Truncate(ms), tc.live.Truncate(ms)
		if !leadMoved(r, l, ms, ms, false) {
			t.Fatalf("%s: %s, then %s, did not move by the lead", tc.name, r, l)
		}
		var typedResp []Leaf
		for _, a := range tc.resp {
			typedResp = append(typedResp, InstantLeaf(a.Path, a.Path, r, ms))
		}
		for _, got := range []struct {
			form string
			ln   Learned
		}{
			{"a $date", c.classify(p, r, l, ms, tc.w, typedResp...)},
			{"its millisecond text", text(tc.rec, tc.live, tc.w, tc.resp...)},
		} {
			ln := got.ln
			switch {
			case ln.Kind != tc.kind || ln.Offset != nil:
				t.Errorf("%s, %s: %s %+v (%s); want %s as before", tc.name, got.form, ln.Kind, ln.Offset, ln.Why, tc.kind)
			case tc.kind == KindClockRead && (ln.Read == nil || !ln.Read.Plain()):
				t.Errorf("%s, %s: read %+v; want a plain read", tc.name, got.form, ln.Read)
			}
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
		o, ok := clockOffsetOf(at(tc.rec), at(tc.live), tc.w, nil)
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

// A reason names an offset as what it is to the application: one above
// zero a time to live, one below zero a look-back (a cutoff such as
// createdAt >= now - 2m), never a time to live; a range that holds zero an
// offset. Each reason the offset rule gives reads so.
func TestAnOffsetBelowZeroIsNamedALookBackNotATimeToLive(t *testing.T) {
	const p = "body.cmd.query.createdAt.{}"
	ms, min2 := time.Millisecond, 2*time.Minute
	for _, tc := range []struct {
		name string
		off  models.ClockOffset
		want string
	}{
		{"below zero", models.ClockOffset{Min: -min2 - 40*ms, Max: -min2 + 11*ms, Res: ms}, "a look-back"},
		{"above zero", models.ClockOffset{Min: min2 - 40*ms, Max: min2 + 11*ms, Res: ms}, "a time to live"},
		{"a range holding zero", models.ClockOffset{Min: -30 * time.Second, Max: 30 * time.Second, Res: time.Minute}, "an offset"},
	} {
		for name, why := range map[string]string{
			"evidence":   clockOffsetEvidence(p, tc.off),
			"fine":       clockOffsetFineWhy(p, tc.off),
			"wide":       clockOffsetWideWhy(p, tc.off, ms),
			"uncompared": clockOffsetUncomparedWhy(p, tc.off),
		} {
			if !strings.Contains(why, " "+tc.want+" changed by ") || tc.want != "a time to live" && strings.Contains(why, "time to live") {
				t.Errorf("%s, %s: %s; want it to name the offset %s, and no time to live", tc.name, name, why, tc.want)
			}
		}
		if tc.off.Min <= 0 && tc.off.Max >= 0 {
			continue
		}
		// A replay's refusal of a window too wide to tell the offset
		// changed by its unit names it so too.
		from := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
		wide := Span{From: from, To: from.Add(2 * time.Minute)}
		live := Leaf{Path: p, Type: String, Value: from.Add(tc.off.Min).Format(time.RFC3339Nano)}
		if ok, why := CheckClockRead(p, tc.off, live, wide, true); ok || !strings.Contains(why, " "+tc.want+" changed by ") || tc.want != "a time to live" && strings.Contains(why, "time to live") {
			t.Errorf("%s, a replay's window two minutes wide: %v %s; want refused naming the offset %s, and no time to live", tc.name, ok, why, tc.want)
		}
	}
	c := newClockOffsetCall()
	ln := c.classifyClient(p, millisText(p, c.read.Add(-min2)), millisText(p, c.fread.Add(-min2)), c.w.ComparesOffsets())
	if ln.Kind != KindClockOffset || !strings.Contains(ln.Why, ". A replay compares the offset: a look-back changed by a minute or more still fails; ") {
		t.Errorf("a client test's cutoff two minutes before: %s (%s); want its evidence to name a look-back", ln.Kind, ln.Why)
	}
}

// secondsText is t as an epoch's seconds, as recorded: a Mongo $numberLong
// the application writes from Go's Unix(), a read of its clock cut to the
// second.
func secondsText(p string, t time.Time) Leaf {
	return Leaf{Path: p, Noise: p, Type: String, Value: strconv.FormatInt(t.Unix(), 10)}
}

// api-server's endpoint claim loop, polled in keploy mock's client test
// TestGenerateTestSuitesOnClient, queries ptw_endpoints for dirtyAt <= now
// - 2s or firstDirtyAt <= now - 30s, each an epoch's seconds: a read of the
// application's clock cut to the second, less a look-back that reads in
// seconds. A cut is checked at the cut, never refused (founder decision,
// 2026-10-27: a clock read cut coarser than a millisecond): each is learned as that
// look-back at the second, never refused as an offset that reads in
// seconds, and its line names the cut. A replay checks it at the second:
// the read less the look-back, cut down, holds wherever the replay's read
// falls in its second; one changed by three seconds or more never does,
// nor does a plain read of the clock, and a replay whose window with the
// look-back's range is a second or more refuses the value, naming why. The
// same look-back written in milliseconds (an application that does not cut)
// stays refused: there a read made two seconds before its call's window
// shows it too (TestAClientTestsOffsetIsRefusedWhereNoComparedOffsetReads).
func TestAWholeSecondLookBackAtTheSecondsCutIsCheckedAtTheSecond(t *testing.T) {
	c := newClockOffsetCall()
	ms, sec := time.Millisecond, time.Second
	for _, tc := range []struct {
		p    string
		back time.Duration
		// From the recording's call less 155 ms up to F's less 632 ms: the
		// second the recording's value starts, before its window, and the
		// one F's ends, after its window.
		want models.ClockOffset
	}{
		{"body.cmd.query.$and[].$or[].dirtyAt.$lte", 2 * sec, models.ClockOffset{Min: -2*sec - 155*ms, Max: -2*sec + 368*ms, Res: sec, Layout: "epoch-s"}},
		{"body.cmd.query.$and[].$or[].firstDirtyAt.$lte", 30 * sec, models.ClockOffset{Min: -30*sec - 155*ms, Max: -30*sec + 368*ms, Res: sec, Layout: "epoch-s"}},
	} {
		rec, live := secondsText(tc.p, c.read.Add(-tc.back)), secondsText(tc.p, c.fread.Add(-tc.back))
		if lay := clockTextLayout(rec.Value); lay != tc.want.Layout {
			t.Fatalf("precondition: %q reads in the layout %q, want %q", rec.Value, lay, tc.want.Layout)
		}
		ln := c.classifyClient(tc.p, rec, live, c.w.ComparesOffsets())
		if ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != tc.want || ln.Why != clockOffsetEvidence(tc.p, tc.want) {
			t.Errorf("FALSE RED: clock - %s cut to the second (%s -> %s): %s %+v (%s); want the look-back %+v", tc.back, rec.Value, live.Value, ln.Kind, ln.Offset, ln.Why, tc.want)
			continue
		}
		if !strings.Contains(ln.Why, " = clock - "+models.ShortDuration(tc.back)+" (") || !strings.Contains(ln.Why, ", cut to a second. A replay checks the offset at the second") {
			t.Errorf("clock - %s: the learned line does not name the look-back and its cut: %s", tc.back, ln.Why)
		}
		o, ok := ln.Clock()
		if !ok {
			t.Fatalf("clock - %s: %s (%s): no clock to check", tc.back, ln.Kind, ln.Why)
		}
		// A replay two days on, its window 50 ms wide, its read 20 ms into
		// it, the window started at each 10 ms of a second.
		base := c.call.Add(48 * time.Hour).Truncate(sec)
		for step := time.Duration(0); step < sec; step += 10 * ms {
			from := base.Add(step)
			s, read := Span{From: from, To: from.Add(50 * ms)}, from.Add(20*ms)
			type row struct {
				name string
				at   time.Time
				ok   bool
			}
			rows := []row{
				{"the read less the look-back", read.Add(-tc.back), true},
				{"a look-back three seconds longer", read.Add(-tc.back - 3*sec), false},
				{"a look-back three seconds shorter", read.Add(-tc.back + 3*sec), false},
			}
			// A plain read is a look-back changed by all of it: by three
			// seconds or more, it never holds.
			if tc.back >= 3*sec {
				rows = append(rows, row{"a plain read", read, false})
			}
			for _, row := range rows {
				if got, why := CheckClockRead(tc.p, o, secondsText(tc.p, row.at), s, true); got != row.ok {
					t.Errorf("%s: clock - %s, a replay's read %s into its second, %s: %v (%s); want %v", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.ok], tc.back, read.Sub(read.Truncate(sec)), row.name, got, why, row.ok)
				}
			}
		}
		// A replay whose window with the look-back's range is a second or
		// more could not tell a look-back changed by seconds: refused,
		// naming the cut, never passed unchecked.
		wide := Span{From: base, To: base.Add(700 * ms)}
		if got, why := CheckClockRead(tc.p, o, secondsText(tc.p, base.Add(-tc.back)), wide, true); got || !strings.Contains(why, "which a replay checks at the second it is cut to against its call's window, but this call's window (700ms) with the offset's range is no narrower than the second") {
			t.Errorf("clock - %s, a replay's window 700 ms wide: %v (%s); want refused naming the second", tc.back, got, why)
		}
	}
}

// The look-back of TestAWholeSecondLookBackAtTheSecondsCutIsCheckedAtTheSecond
// (dirtyAt <= now - 2s, cut to the second), and a deadline at clock + 30s
// cut to the second, where record's full round's call ran in a wider window: the learn step learns it at the second only
// where a replay as wide as each run's window could check it there
// (models.ClockOffset.Checkable: the window and the offset's range together
// narrower than the second). F's window 200 ms wide puts the look-back from
// 2.155 s up to 1.492 s back (663 ms), which with either window stays under
// the second: learned. 300 ms wide puts it up to 1.392 s back (763 ms),
// which with that window is a second or more: refused, never learned as a
// check every replay that wide fails, which the offset's own check there
// shows. The deadline reads the same 155 ms before and 508 ms or 608 ms
// after thirty seconds. In keploy test the deadline's refusal names the
// window and the cut; as an epoch's seconds in keploy mock's client test,
// the client test's refusal stands (textClockOffset), as for every offset
// clockOffset refuses, and keploy test's lead shows a look-back as a plain
// read, which its fixture's caller checks none of.
func TestAWholeSecondLookBackIsRefusedWhereItsCallsWindowIsTooWideToCheckItAtTheSecond(t *testing.T) {
	const p = "body.cmd.query.$and[].$or[].dirtyAt.$lte"
	c := newClockOffsetCall()
	ms, sec := time.Millisecond, time.Second
	window := func(fw time.Duration) *Window {
		return (&Window{Rec: Span{From: c.call.Add(-50 * ms), To: c.call}, F: Span{From: c.fcall.Add(-fw), To: c.fcall}}).ComparesOffsets()
	}
	for _, form := range []struct {
		name   string
		d      time.Duration
		learn  func(rec, live time.Time, w *Window) Learned
		layout string
		named  bool
	}{
		{"a look-back as an epoch's seconds in a client test", -2 * sec, func(rec, live time.Time, w *Window) Learned {
			return c.classifyClient(p, secondsText(p, rec), secondsText(p, live), w)
		}, "epoch-s", false},
		{"a deadline typed to the second in keploy test", 30 * sec, func(rec, live time.Time, w *Window) Learned {
			return c.classify(p, rec.Truncate(sec), live.Truncate(sec), sec, w)
		}, models.LayoutTyped, true},
	} {
		t.Run(form.name, func(t *testing.T) {
			rec, live := c.read.Add(form.d), c.fread.Add(form.d)
			want := models.ClockOffset{Min: form.d - 155*ms, Max: form.d + 508*ms, Res: sec, Layout: form.layout}
			if ln := form.learn(rec, live, window(200*ms)); ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != want {
				t.Fatalf("FALSE RED: F's window 200 ms wide: %s %+v (%s); want the look-back %+v", ln.Kind, ln.Offset, ln.Why, want)
			}
			ln := form.learn(rec, live, window(300*ms))
			if ln.Kind != KindUnaccounted || ln.Offset != nil || !strings.Contains(ln.Why, "declare it noise in keploy.yml") {
				t.Fatalf("F's window 300 ms wide: %s %+v (%s); want refused", ln.Kind, ln.Offset, ln.Why)
			}
			if form.named && (!strings.Contains(ln.Why, p+" = ") || !strings.Contains(ln.Why, "a replay checks it at the second it is cut to against its call's window, and its call's window (300ms) with the offset's range is no narrower than the second")) {
				t.Fatalf("F's window 300 ms wide: %s; want the reason to name the field, the window and the second", ln.Why)
			}
			// The offset learned there would be refused by every replay that
			// wide, whatever it carried: a certain false red.
			o := models.ClockOffset{Min: form.d - 155*ms, Max: form.d + 608*ms, Res: sec, Layout: form.layout}
			if !o.Narrow() {
				t.Fatalf("precondition: %+v is narrower than the second", o)
			}
			base := c.call.Add(48 * time.Hour).Truncate(sec)
			s := Span{From: base, To: base.Add(300 * ms)}
			at := base.Add(100*ms + form.d)
			lf := secondsText(p, at)
			if form.named {
				lf = InstantLeaf(p, p, at.Truncate(sec), sec)
			}
			if got, why := CheckClockRead(p, o, lf, s, true); got || !strings.Contains(why, "no narrower than the second") {
				t.Fatalf("a replay 300 ms wide: %v (%s); want refused for its window", got, why)
			}
		})
	}
}
