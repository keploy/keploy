package learn

import (
	"strconv"
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
// (its offset reads in seconds, which the offset rule refuses); the cutoff
// is the clock less a time to live (createdAt $gte now - 120 s: before its
// call, as a read made earlier is, which no offset reads); a wall clock of
// no zone; two layouts; or the call's answer holds the value.
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

	const q = "body.cmd.query.createdAt.{}"
	refused("the clock less a time to live", c.classifyClient(q, millisText(q, c.read.Add(-120*time.Second)), millisText(q, c.fread.Add(-120*time.Second)), c.w.ComparesOffsets()), millis)

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
