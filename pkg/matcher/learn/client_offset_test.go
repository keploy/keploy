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
	// An offset the learn step refused: the client test's refusal names it
	// and why (learning legible), and never binds it (bound, every replay
	// would bind it and never compare it: a clock read left unchecked).
	named := func(name string, ln Learned, why string) {
		t.Helper()
		if !offsetRefused(ln, p, why) {
			t.Errorf("%s: %s %+v bindable=%v (%s); want refused, naming the offset and %q, never bound", name, ln.Kind, ln.Offset, ln.Bindable, ln.Why, why)
		}
	}
	const fine = "but the offset reads in seconds and its values at a resolution of 1ms"
	named("no replay compares an offset", c.classifyClient(p, millisText(p, rec), millisText(p, live), c.w), "this protocol's replay does not compare such an offset yet")
	named("a read two seconds before the window", c.classifyClient(p, millisText(p, c.call.Add(-2*time.Second+30*time.Minute)), millisText(p, c.fcall.Add(-2*time.Second-5*ms+30*time.Minute)), c.w.ComparesOffsets()), fine)
	// The same as its protocol types it: named too.
	typed := func(at time.Time) Leaf { return InstantLeaf(p, p, at.Truncate(ms), ms) }
	named("a typed read two seconds before the window", c.classifyClient(p, typed(c.call.Add(-2*time.Second+30*time.Minute)), typed(c.fcall.Add(-2*time.Second-5*ms+30*time.Minute)), c.w.ComparesOffsets()), fine)

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
	named("the call's answer holds it", c.classifyClient(p, millisText(p, rec), millisText(p, live), c.w.ComparesOffsets(), answer), "the call's recorded answer holds the moment it was recorded with")
}

// offsetRefused reports whether ln is keploy mock's client test's refusal of
// an instant its call's window holds as a read of the clock plus an offset
// the learn step refused for why (a phrase of clockOffset's reason): it
// names the field, the offset and that reason, with the client test's own
// correction (clientOffsetRefusedWhy), never only that no clock read
// matched it, and it is never bound (Learned.Bindable): bound, every replay
// would bind it and compare nothing.
func offsetRefused(ln Learned, p, why string) bool {
	return ln.Kind == KindUnaccounted && ln.Offset == nil && ln.Read == nil && !ln.Bindable && strings.HasPrefix(ln.Why, p+" = clock") && strings.Contains(ln.Why, why) && strings.HasSuffix(ln.Why, ". If your test supplies this value, have it supply the same value in every run, and keploy keeps comparing it")
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
		// Where the window shows one offset the learn step refused, the
		// refusal names it and why; where it shows none, the client test's.
		why := map[string]string{
			"a read two seconds before the window": "but the offset reads in seconds and its values at a resolution of 1ms",
			"the call's answer holds it":           "the call's recorded answer holds the moment it was recorded with",
			"no replay compares an offset":         "this protocol's replay does not compare such an offset yet",
		}[tc.name]
		ln := c.classifyClient(q, millisText(q, tc.rec), millisText(q, tc.live), tc.w, tc.resp...)
		if why != "" && !offsetRefused(ln, q, why) || why == "" && (ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Read != nil || ln.Why != millis) {
			t.Errorf("%s: %s %+v (%s); want refused: %s", tc.name, ln.Kind, ln.Offset, ln.Why, map[bool]string{true: why, false: millis}[why != ""])
		}
		typed := func(at time.Time) Leaf { return InstantLeaf(q, q, at.Truncate(ms), ms) }
		if tc.resp != nil {
			continue
		}
		ln = c.classifyClient(q, typed(tc.rec), typed(tc.live), tc.w)
		if why != "" && !offsetRefused(ln, q, why) || why == "" && (ln.Kind != KindUnaccounted || ln.Offset != nil || ln.Read != nil || ln.Why != clientTestWhy("", true)) {
			t.Errorf("%s, a $date: %s %+v (%s); want refused: %s", tc.name, ln.Kind, ln.Offset, ln.Why, map[bool]string{true: why, false: clientTestWhy("", true)}[why != ""])
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

// api-server's endpoint poll (dirtyAt <= now - 2s, firstDirtyAt <= now -
// 30s, written as an epoch's seconds: an application that cuts its look-back
// to the second; founder decision, 2026-10-27: a clock read cut coarser
// than a millisecond is checked at the cut, never refused): each is learned
// as that look-back at the second, its line naming the cut, the offset a
// replay checks (read as clock - 2s) and the fall-back exception. A replay
// checks the value at the second against the call's window less exactly
// that look-back, from the start of the second that holds the window's
// start less it up to one second after the call less it, in any window:
// the read less the look-back holds at every read, and so does one a
// second shorter (the cut's stated cost: a writer that rounds); one made
// k seconds shorter holds only where the replay's window holds k-1 turns
// of the second between the application's read and its call, and one made
// k seconds longer only where it holds k turns between the window's start
// and the read. So a plain read in place of clock - 2s fails at every
// replay whose application reads its clock at its call (api-server's poll,
// its windows 61, 75 and 131 ms as recorded), and in a window it reads
// inside, only where a turn of the second falls between that read and the
// call; a 4 s look-back only where two turns fall between the window's
// start and the read (in a window 2 s wide), never in one narrower than a
// second. The cost of that: a value of the right look-back that the
// application read before its call's window passes only where no turn of
// the second falls between that read and the window's start. No window
// refuses the right value read in it: 700 ms and 2 s wide, it holds. The
// same look-back written in milliseconds (an
// application that does not cut) stays refused: there a read made two
// seconds before its call's window shows it too
// (TestAClientTestsOffsetIsRefusedWhereNoComparedOffsetReads).
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
		named := "clock - " + models.ShortDuration(tc.back)
		for _, part := range []string{" = " + named + " (", ", cut to a second. A replay checks the offset at the second", ", from the start of the second that holds its call's window's start plus the offset, read as " + named + ", up to one second after its call plus it, whatever the window's width", "one made two seconds longer or more fails, as does a value the application read before its call's window (kept across an earlier call) where a turn of the second falls between that read and the window's start", "near a wall time the application's zone shows twice as its clocks go back a replay refuses the value"} {
			if !strings.Contains(ln.Why, part) {
				t.Errorf("%s: the learned line does not say %q: %s", named, part, ln.Why)
			}
		}
		o, ok := ln.Clock()
		if !ok {
			t.Fatalf("%s: %s (%s): no clock to check", named, ln.Kind, ln.Why)
		}
		// A replay two days on, its window started at each 10 ms of a
		// second: 50 ms wide, its read 20 ms in; api-server's poll's windows
		// as recorded, its read at the call; 700 ms and 2 s wide.
		base := c.call.Add(48 * time.Hour).Truncate(sec)
		for _, win := range []struct{ width, read time.Duration }{{50 * ms, 20 * ms}, {61 * ms, 61 * ms}, {75 * ms, 75 * ms}, {131 * ms, 131 * ms}, {700 * ms, 350 * ms}, {2 * sec, sec}} {
			for step := time.Duration(0); step < sec; step += 10 * ms {
				from := base.Add(step)
				s, read := Span{From: from, To: from.Add(win.width)}, from.Add(win.read)
				// Turns of the second between the read and the call, and
				// between the window's start and the read.
				after, before := s.To.Truncate(sec).Sub(read.Truncate(sec))/sec, read.Truncate(sec).Sub(from.Truncate(sec))/sec
				type row struct {
					name string
					at   time.Time
					ok   bool
				}
				rows := []row{
					{"the read less the look-back", read.Add(-tc.back), true},
					{"a look-back a second shorter", read.Add(-tc.back + sec), true},
					{"a look-back a second longer", read.Add(-tc.back - sec), before >= 1},
					{"a look-back two seconds shorter", read.Add(-tc.back + 2*sec), after >= 1},
					{"a look-back two seconds longer", read.Add(-tc.back - 2*sec), before >= 2},
					{"a look-back three seconds shorter", read.Add(-tc.back + 3*sec), after >= 2},
					{"a look-back three seconds longer", read.Add(-tc.back - 3*sec), before >= 3},
					{"a plain read", read, time.Duration(after) >= tc.back/sec-1},
				}
				// The right look-back of a read made 100, 500 and 900 ms
				// before the window (the application kept it across an
				// earlier call): it holds only where no turn of the second
				// falls between that read and the window's start.
				for _, stale := range []time.Duration{100 * ms, 500 * ms, 900 * ms} {
					early := from.Add(-stale)
					rows = append(rows, row{"the look-back of a read " + stale.String() + " before the window", early.Add(-tc.back), early.Truncate(sec).Equal(from.Truncate(sec))})
				}
				for _, row := range rows {
					if got, why := CheckClockRead(tc.p, o, secondsText(tc.p, row.at), s, true); got != row.ok {
						t.Errorf("%s: %s, a replay's window %s wide, %s into its second, its read %s in, %s: %v (%s); want %v", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.ok], named, win.width, step, win.read, row.name, got, why, row.ok)
					}
				}
			}
		}
	}
}

// The look-back of TestAWholeSecondLookBackAtTheSecondsCutIsCheckedAtTheSecond
// (dirtyAt <= now - 2s, cut to the second), and a deadline at clock + 30s
// cut to the second, where one run's call ran in a wider window: learned at
// the second whatever the window (founder decision, 2026-10-27: a clock
// read cut coarser than a millisecond is checked at the cut, never
// refused), as the one whole second its range holds, which the line names
// (read as clock - 2s, clock + 30s): F's window 200 ms wide puts the
// look-back from 2.155 s up to 1.492 s back; 300 ms or 700 ms wide, or the
// recording's 300 ms wide (its window across a turn of the second, F's
// not), the range is wider, and still holds the one whole second. A replay
// 300 ms or 2 s wide holds the right value at every phase: no window, at
// learn or at replay, refuses it.
func TestAWholeSecondLookBackIsLearnedWhateverItsCallsWindow(t *testing.T) {
	const p = "body.cmd.query.$and[].$or[].dirtyAt.$lte"
	c := newClockOffsetCall()
	ms, sec := time.Millisecond, time.Second
	window := func(rw, fw time.Duration) *Window {
		return (&Window{Rec: Span{From: c.call.Add(-rw), To: c.call}, F: Span{From: c.fcall.Add(-fw), To: c.fcall}}).ComparesOffsets()
	}
	for _, form := range []struct {
		name   string
		d      time.Duration
		learn  func(rec, live time.Time, w *Window) Learned
		layout string
		named  string
	}{
		{"a look-back as an epoch's seconds in a client test", -2 * sec, func(rec, live time.Time, w *Window) Learned {
			return c.classifyClient(p, secondsText(p, rec), secondsText(p, live), w)
		}, "epoch-s", "clock - 2s"},
		{"a deadline typed to the second in keploy test", 30 * sec, func(rec, live time.Time, w *Window) Learned {
			return c.classify(p, rec.Truncate(sec), live.Truncate(sec), sec, w)
		}, models.LayoutTyped, "clock + 30s"},
	} {
		t.Run(form.name, func(t *testing.T) {
			rec, live := c.read.Add(form.d), c.fread.Add(form.d)
			want := models.ClockOffset{Min: form.d - 155*ms, Max: form.d + 508*ms, Res: sec, Layout: form.layout}
			if ln := form.learn(rec, live, window(50*ms, 200*ms)); ln.Kind != KindClockOffset || ln.Offset == nil || *ln.Offset != want {
				t.Fatalf("FALSE RED: F's window 200 ms wide: %s %+v (%s); want %+v", ln.Kind, ln.Offset, ln.Why, want)
			}
			base := c.call.Add(48 * time.Hour).Truncate(sec)
			for _, ws := range []struct{ rw, fw time.Duration }{{50 * ms, 300 * ms}, {50 * ms, 700 * ms}, {300 * ms, 50 * ms}} {
				ln := form.learn(rec, live, window(ws.rw, ws.fw))
				if ln.Kind != KindClockOffset || ln.Offset == nil || !strings.Contains(ln.Why, ", read as "+form.named+", ") || !strings.Contains(ln.Why, ", whatever the window's width") {
					t.Errorf("FALSE RED: windows %s and %s wide: %s %+v (%s); want learned, read as %s", ws.rw, ws.fw, ln.Kind, ln.Offset, ln.Why, form.named)
					continue
				}
				for _, width := range []time.Duration{300 * ms, 2 * sec} {
					for step := time.Duration(0); step < sec; step += 50 * ms {
						from := base.Add(step)
						at := from.Add(width/2 + form.d)
						lf := secondsText(p, at)
						if form.layout == models.LayoutTyped {
							lf = InstantLeaf(p, p, at.Truncate(sec), sec)
						}
						if got, why := CheckClockRead(p, *ln.Offset, lf, Span{From: from, To: from.Add(width)}, true); !got {
							t.Errorf("FALSE RED: learned in windows %s and %s wide, a replay %s wide %s into its second: %v (%s)", ws.rw, ws.fw, width, step, got, why)
						}
					}
				}
			}
		})
	}
}

// An offset checked against its unit (not cut: clock + 30m at the
// millisecond) is refused where either run's call's window, with its range
// and three of its resolution, is its unit or more (checkableIn reads both
// runs' windows: a replay as wide would refuse it whatever it carried),
// the reason naming that window.
func TestAnOffsetIsRefusedWhereEitherRunsWindowIsTooWideForItsUnit(t *testing.T) {
	const p = "body.expiresAt"
	c := newClockOffsetCall()
	ms := time.Millisecond
	for _, ws := range []struct {
		name   string
		rw, fw time.Duration
	}{{"the recording's window a minute and a second wide", 61 * time.Second, 50 * ms}, {"record's full round's window a minute and a second wide", 50 * ms, 61 * time.Second}} {
		w := (&Window{Rec: Span{From: c.call.Add(-ws.rw), To: c.call}, F: Span{From: c.fcall.Add(-ws.fw), To: c.fcall}}).ComparesOffsets()
		ln := c.classify(p, c.read.Add(30*time.Minute).Truncate(ms), c.fread.Add(30*time.Minute).Truncate(ms), ms, w)
		if ln.Kind != KindUnaccounted || ln.Offset != nil || !strings.Contains(ln.Why, "its call's window (1m1s) with the offset's range and three of its cut is no narrower than the minute the offset reads in") {
			t.Errorf("%s: %s %+v (%s); want refused, naming the window", ws.name, ln.Kind, ln.Offset, ln.Why)
		}
	}
}

// An application that reads its clock once, early in its test, and writes
// that read, cut to the second, at a call 2.5 to 9 seconds later (a login's
// authTime stored by a later insert, a createdAt taken at its handler's
// start): in its call's window, it reads as clock - Ns at the second, a
// look-back, but the window from each run's test begin holds it as a plain
// read in both runs, so it is learned as that read since its test began,
// never as the offset, in every recording (both runs' phases over their
// second, record's full round 150 ms slower). A replay checks it from its
// test's begin to its call, so the application's value passes whatever the
// replay's wait between its read and its call: the same, two seconds
// shorter (the slow calls in between are mocks that answer at once), or
// three seconds longer. A value read before the replay's test began fails.
func TestAnEarlyReadCutToTheSecondIsLearnedAsAReadSinceItsTestBegan(t *testing.T) {
	ms, sec := time.Millisecond, time.Second
	const p = "body.documents[].authTime"
	for _, gap := range []time.Duration{2500 * ms, 3 * sec, 9 * sec} {
		for rph := time.Duration(0); rph < sec; rph += 100 * ms {
			for fph := time.Duration(0); fph < sec; fph += 100 * ms {
				rcall := time.Date(2026, 10, 31, 10, 42, 14, 0, time.UTC).Add(rph)
				fcall := rcall.Add(FullRoundLead).Truncate(sec).Add(fph)
				rread, fread := rcall.Add(-gap), fcall.Add(-gap-150*ms)
				c := clockOffsetCall{call: rcall, read: rread, fcall: fcall, fread: fread}
				w := (&Window{Rec: Span{From: rcall.Add(-50 * ms), To: rcall, Began: rread.Add(-500 * ms)}, F: Span{From: fcall.Add(-50 * ms), To: fcall, Began: fread.Add(-500 * ms)}}).ComparesOffsets()
				ln := c.classifyClient(p, secondsText(p, rread), secondsText(p, fread), w)
				o, ok := ln.Clock()
				if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin {
					t.Fatalf("read %s before its call, phases %v and %v: %s %+v (%s); want a plain read since its test began", gap, rph, fph, ln.Kind, ln.Offset, ln.Why)
				}
				for _, extra := range []time.Duration{-2 * sec, -1500 * ms, -1 * sec, 0, 1200 * ms, 2 * sec, 3 * sec} {
					for ph := time.Duration(0); ph < sec; ph += 50 * ms {
						to := rcall.Add(72 * time.Hour).Truncate(sec).Add(ph)
						read := to.Add(-gap - extra)
						s := Span{From: to.Add(-50 * ms), To: to, Began: read.Add(-500 * ms)}
						if got, why := CheckClockRead(p, o, secondsText(p, read), s, true); !got {
							t.Fatalf("FALSE RED: read %s before its call, a replay %+v longer, %v into its second: %s", gap, extra, ph, why)
						}
						if got, _ := CheckClockRead(p, o, secondsText(p, read.Add(-3*sec)), s, true); got {
							t.Fatalf("FALSE GREEN: read %s before its call, a replay %+v longer: a value read 2.5 s before its test began passes", gap, extra)
						}
					}
				}
			}
		}
	}
}

// An application that reads its clock once, early in its test, and writes
// that read plus a time to live, cut to the second (an OAuth grant's
// expires_at = now + 30 days, its now taken at its handler's start), at a
// call 2.5 to 9 seconds later: in its call's window it reads as clock +
// 29d23h59m57s at the second, an offset that measures the wait as much as
// the time to live, so a replay whose wait changed by a second would fail
// it, or pass, by where its reads fell in their seconds. The window from
// each run's test begin holds it as clock + 30d in both runs, and the value
// lies after its call (a time to live, never a look-back), so it is learned
// as that offset since its test began, in every recording (both runs'
// phases over their second, record's full round 150 ms slower). A replay
// checks it from its test's begin to its call: the application's value
// passes at every phase whatever the replay's wait, up to a second shorter
// or longer (and two shorter, three longer), and a time to live changed by
// a minute, an hour or a day fails. One changed by less than the test's
// span before its call passes (the line says so).
func TestAnEarlyReadPlusATimeToLiveIsLearnedAsThatOffsetSinceItsTestBegan(t *testing.T) {
	ms, sec := time.Millisecond, time.Second
	const p = "body.documents[].expires_at"
	const ttl = 30 * 24 * time.Hour
	for _, gap := range []time.Duration{2500 * ms, 3 * sec, 9 * sec} {
		for rph := time.Duration(0); rph < sec; rph += 100 * ms {
			for fph := time.Duration(0); fph < sec; fph += 100 * ms {
				rcall := time.Date(2026, 10, 31, 10, 42, 14, 0, time.UTC).Add(rph)
				fcall := rcall.Add(FullRoundLead).Truncate(sec).Add(fph)
				rread, fread := rcall.Add(-gap), fcall.Add(-gap-150*ms)
				c := clockOffsetCall{call: rcall, read: rread, fcall: fcall, fread: fread}
				w := (&Window{Rec: Span{From: rcall.Add(-50 * ms), To: rcall, Began: rread.Add(-500 * ms)}, F: Span{From: fcall.Add(-50 * ms), To: fcall, Began: fread.Add(-500 * ms)}}).ComparesOffsets()
				ln := c.classifyClient(p, secondsText(p, rread.Add(ttl)), secondsText(p, fread.Add(ttl)), w)
				o, ok := ln.Clock()
				// Where the two runs' values stand a whole minute apart
				// (record's full round read its clock 0.55 s to 2.55 s
				// sooner after the lead than the recording did), the offset
				// is read at the minute in its call's window (offsetReading:
				// a writer that cuts to the minute and adds a time to live
				// writes such values too), which a replay's wait passes as
				// well, and a time to live changed by a minute can pass.
				apart := (fread.Add(ttl).Unix()-rread.Add(ttl).Unix())%60 == 0
				switch {
				case apart:
					if ln.Kind != KindClockOffset || !ok || o.Since != "" || o.Res != time.Minute || o.Min > ttl || o.Max < ttl-sec {
						t.Fatalf("read %s before its call plus 30 days, phases %v and %v, values a minute apart: %s %v (%s); want clock + 30d at the minute in its call's window", gap, rph, fph, ln.Kind, o, ln.Why)
					}
				case ln.Kind != KindClockOffset || !ok || o.Since != models.SinceTestBegin || o.Unit() != 24*time.Hour || o.Min > ttl || o.Max < ttl-sec:
					t.Fatalf("read %s before its call plus 30 days, phases %v and %v: %s %v (%s); want clock + 30d since its test began", gap, rph, fph, ln.Kind, o, ln.Why)
				default:
					if s := o.String(); !strings.HasPrefix(s, "clock + 30d") || !strings.Contains(s, "read after its test began and before its call") || !strings.Contains(s, "a time to live changed by less than that span passes too") {
						t.Fatalf("read %s before its call plus 30 days: the learned line %q does not name the offset, its span and what that span lets pass", gap, s)
					}
				}
				for _, extra := range []time.Duration{-2 * sec, -1 * sec, -500 * ms, -100 * ms, 0, 100 * ms, 500 * ms, 1 * sec, 3 * sec} {
					for ph := time.Duration(0); ph < sec; ph += 50 * ms {
						to := rcall.Add(72 * time.Hour).Truncate(sec).Add(ph)
						read := to.Add(-gap - extra)
						s := Span{From: to.Add(-50 * ms), To: to, Began: read.Add(-500 * ms)}
						if got, why := CheckClockRead(p, o, secondsText(p, read.Add(ttl)), s, true); !got {
							t.Fatalf("FALSE RED: read %s before its call plus 30 days, a replay %+v longer, %v into its second: %s", gap, extra, ph, why)
						}
						for _, d := range []time.Duration{-24 * time.Hour, -time.Hour, -time.Minute, time.Minute, time.Hour, 24 * time.Hour} {
							if apart && d.Abs() == time.Minute {
								continue
							}
							if got, _ := CheckClockRead(p, o, secondsText(p, read.Add(ttl+d)), s, true); got {
								t.Fatalf("FALSE GREEN: read %s before its call, a replay %+v longer, %v into its second: a time to live changed by %s passes", gap, extra, ph, d)
							}
						}
					}
				}
			}
		}
	}
}

// api-server's endpoint poll (dirtyAt <= now - 2s, firstDirtyAt <= now -
// 30s, an epoch's seconds), read at its call, in a test begun a second
// before it: each value lies before the test's begin in both runs, which no
// read since the test began explains, so it is learned as the look-back it
// names, checked at the second in its call's window, and a replay that drops
// the look-back (a plain read) fails at every phase. In a test begun five
// seconds before its poll, now - 2s lies after the begin in both runs: it is
// learned as a read since its test began, which passes a replay's look-back
// wherever its test began two seconds or more before it, and a plain read
// too (the cost of the rule that never lets a verdict turn on load: an
// application's early read and a look-back look the same there); now - 30s
// is still the look-back.
func TestAPollsLookBackBeforeItsTestBeganIsLearnedAsTheOffset(t *testing.T) {
	ms, sec := time.Millisecond, time.Second
	const p = "body.cmd.query.$and[].$or[].dirtyAt.$lte"
	for _, row := range []struct {
		back, began time.Duration
		since       bool // learned as a read since its test began
	}{
		{2 * sec, sec, false},
		{30 * sec, sec, false},
		{2 * sec, 5 * sec, true},
		{30 * sec, 5 * sec, false},
	} {
		for rph := time.Duration(0); rph < sec; rph += 100 * ms {
			rcall := time.Date(2026, 10, 31, 10, 42, 14, 0, time.UTC).Add(rph)
			fcall := rcall.Add(FullRoundLead + 37*ms)
			c := clockOffsetCall{call: rcall, read: rcall.Add(-ms), fcall: fcall, fread: fcall.Add(-ms)}
			w := (&Window{Rec: Span{From: rcall.Add(-61 * ms), To: rcall, Began: rcall.Add(-row.began)}, F: Span{From: fcall.Add(-131 * ms), To: fcall, Began: fcall.Add(-row.began)}}).ComparesOffsets()
			ln := c.classifyClient(p, secondsText(p, c.read.Add(-row.back)), secondsText(p, c.fread.Add(-row.back)), w)
			o, ok := ln.Clock()
			if row.since {
				if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin {
					t.Fatalf("now - %s, begun %s before, phase %v: %s %+v (%s); want a read since its test began", row.back, row.began, rph, ln.Kind, ln.Offset, ln.Why)
				}
				// The limit it carries, stated in its learned line.
				if s := o.String(); !strings.Contains(s, "a look-back the application shortened or dropped within it passes too") {
					t.Fatalf("now - %s, begun %s before: the learned line %q does not say a dropped look-back passes", row.back, row.began, s)
				}
				continue
			}
			if lo, hi := o.CutOffsets(); ln.Kind != KindClockOffset || !ok || o.Since != "" || lo > -row.back || hi < -row.back {
				t.Fatalf("now - %s, begun %s before, phase %v: %s %+v (%s); want the look-back of %s in its call's window", row.back, row.began, rph, ln.Kind, ln.Offset, ln.Why, row.back)
			}
			for _, rw := range []time.Duration{61 * ms, 131 * ms, 700 * ms} {
				for ph := time.Duration(0); ph < sec; ph += 10 * ms {
					from := rcall.Add(72 * time.Hour).Truncate(sec).Add(ph)
					s := Span{From: from, To: from.Add(rw), Began: from.Add(-row.began)}
					if got, why := CheckClockRead(p, o, secondsText(p, s.To.Add(-row.back)), s, true); !got {
						t.Fatalf("FALSE RED: now - %s, a replay's window %s, %v into its second: %s", row.back, rw, ph, why)
					}
					if got, _ := CheckClockRead(p, o, secondsText(p, s.To), s, true); got {
						t.Fatalf("FALSE GREEN: now - %s, a replay's window %s, %v into its second: a plain read passes", row.back, rw, ph)
					}
				}
			}
		}
	}
}

// The poll's look-backs at the recording's real phase (.680), its windows
// 61 ms, with record's full round reaching its poll 1.3 to 2 s sooner after
// its begin than the recording did: the two values of now - 2s, and of now
// - 30s, stand a whole minute apart. At the minute no offset holds both
// runs (each value stands within a minute of its read, on both sides of
// it), so the offset is read at the values' own cut, the second, and
// learned as the look-back it names, never refused for the two cuts. A
// replay passes the application's value and fails a plain read.
func TestAPollsValuesAWholeMinuteApartAreLearnedAtTheSecond(t *testing.T) {
	ms, sec := time.Millisecond, time.Second
	const p = "body.cmd.query.$and[].$or[].dirtyAt.$lte"
	for _, back := range []time.Duration{2 * sec, 30 * sec} {
		apart := 0
		for d := -2 * sec; d <= -1300*ms; d += 100 * ms {
			rcall := time.Date(2026, 10, 31, 10, 42, 14, 680e6, time.UTC)
			fcall := rcall.Add(FullRoundLead + d)
			c := clockOffsetCall{call: rcall, read: rcall.Add(-ms), fcall: fcall, fread: fcall.Add(-ms)}
			w := (&Window{Rec: Span{From: rcall.Add(-61 * ms), To: rcall}, F: Span{From: fcall.Add(-61 * ms), To: fcall}}).ComparesOffsets()
			rv, fv := c.read.Add(-back).Truncate(sec), c.fread.Add(-back).Truncate(sec)
			if fv.Sub(rv)%time.Minute != 0 {
				continue
			}
			apart++
			ln := c.classifyClient(p, secondsText(p, rv), secondsText(p, fv), w)
			o, ok := ln.Clock()
			if lo, hi := o.CutOffsets(); ln.Kind != KindClockOffset || !ok || o.Res != sec || lo > -back || hi < -back {
				t.Fatalf("now - %s, record's full round %v sooner, its values %s apart: %s %+v (%s); want the look-back of %s at the second", back, -d, fv.Sub(rv), ln.Kind, ln.Offset, ln.Why, back)
			}
			for ph := time.Duration(0); ph < sec; ph += 10 * ms {
				from := rcall.Add(72 * time.Hour).Truncate(sec).Add(ph)
				s := Span{From: from, To: from.Add(61 * ms)}
				if got, why := CheckClockRead(p, o, secondsText(p, s.To.Add(-back)), s, true); !got {
					t.Fatalf("FALSE RED: now - %s, %v into its second: %s", back, ph, why)
				}
				if got, _ := CheckClockRead(p, o, secondsText(p, s.To), s, true); got {
					t.Fatalf("FALSE GREEN: now - %s, %v into its second: a plain read passes", back, ph)
				}
			}
		}
		if apart == 0 {
			t.Fatalf("now - %s: no timing put its two values a whole minute apart", back)
		}
	}
}
