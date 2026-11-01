package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// v5Read is one run's call: its window, from 61 ms before the call (a
// poll's, as api-server's recorded), and the application's read in it.
type v5Read struct{ from, to, read time.Time }

func v5Call(call time.Time, width, readBefore time.Duration) v5Read {
	return v5Read{from: call.Add(-width), to: call, read: call.Add(-readBefore)}
}

func v5Window(r, f v5Read) *Window {
	return (&Window{Rec: Span{From: r.from, To: r.to}, F: Span{From: f.from, To: f.to}}).ComparesOffsets()
}

func v5Instant(t time.Time, res time.Duration) typedInstant {
	return typedInstant{at: t, res: res, names: namesMoment}
}

// v5Check is a replay's check of o (learned at the text rec, whose layout
// the order sets on a text's Clock) against the value at, written as rec
// is, in the call's window s.
func v5Check(p string, o models.ClockOffset, rec string, at time.Time, s Span) (bool, string) {
	v := strconv.FormatInt(at.Unix(), 10)
	if !isDigits(rec) {
		v = at.UTC().Format(time.RFC3339)
	}
	o.Layout = clockTextLayout(rec)
	return CheckClockRead(p, o, Leaf{Path: p, Noise: p, Type: String, Value: v}, s, true)
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// api-server's ordinal reconciler claims an endpoint with a filter of
// dirtyAt <= now - DebounceSeconds (2 s) or firstDirtyAt <= now -
// MaxStaleSeconds (30 s), and its stale scan queries last_active < now - 15
// s, each written as an epoch's seconds (time.Now().Unix() less the
// offset): the call's own clock read, cut to the second, less a fixed
// look-back. Through the reading the order's step 5 calls (clockInWindow)
// and the guard it applies (Narrow), each is a Clock of exactly that
// look-back at the second, checked at the cut: a replay passes the
// application's value at every phase of the second, in each of the
// reconciler's recorded windows, and fails a stale value (the recorded
// one), a look-back changed by three seconds or more, and a plain read in
// its place. The same holds of the value written as an RFC 3339 text (an
// HTTP body), one rule across value forms. main's reading refuses each as
// too wide to narrow, so the claim binds to an earlier mark instead.
func TestAPollsLookBackCutToTheSecondIsTheCallsClockCheckedAtTheCut(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	call := time.Date(2026, 10, 26, 11, 14, 17, 155e6, time.UTC)
	rec, f := v5Call(call, 61*ms, 0), v5Call(call.Add(FullRoundLead-8*ms), 61*ms, 0)
	w := v5Window(rec, f)
	for _, back := range []time.Duration{2 * sec, 15 * sec, 30 * sec} {
		for _, form := range []string{"epoch-s", "rfc3339"} {
			p := "body.cmd.query.dirtyAt.$lte"
			rv, lv := rec.read.Truncate(sec).Add(-back), f.read.Truncate(sec).Add(-back)
			recText := strconv.FormatInt(rv.Unix(), 10)
			if form == "rfc3339" {
				recText = rv.Format(time.RFC3339)
			}
			o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), w, nil)
			named := "clock - " + models.ShortDuration(back)
			if !ok || why != "" || !o.Narrow() || o.Step != sec || o.Res != sec {
				t.Errorf("FALSE RED: %s %s: %+v %q %v, Narrow %v; want a Clock at the second that the order's guard keeps", named, form, o, why, ok, o.Narrow())
				continue
			}
			for _, part := range []string{" = " + named + " (", "cut to a second. A replay checks it at the second against " + named + ", ", "whatever the window's width"} {
				if !strings.Contains(o.Why, part) {
					t.Errorf("%s: the line does not say %q: %s", named, part, o.Why)
				}
			}
			base := call.Add(48 * time.Hour).Truncate(sec)
			for _, width := range []time.Duration{61 * ms, 75 * ms, 131 * ms} {
				for phase := time.Duration(0); phase < sec; phase += 10 * ms {
					s := Span{From: base.Add(phase), To: base.Add(phase + width)}
					read := s.To
					for _, row := range []struct {
						name string
						at   time.Time
						want bool
					}{
						{"the application's value", read.Truncate(sec).Add(-back), true},
						{"the recorded value, two days stale", rv, false},
						{"a look-back three seconds longer", read.Truncate(sec).Add(-back - 3*sec), false},
						{"a look-back three seconds shorter", read.Truncate(sec).Add(-back + 3*sec), false},
						{"a plain read", read.Truncate(sec), false},
					} {
						if got, why := v5Check(p, o, recText, row.at, s); got != row.want {
							t.Errorf("%s: %s %s, window %s at %s into its second, %s: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], named, form, width, phase, row.name, got, why)
						}
					}
				}
			}
		}
	}
}

// A look-back of an application that cuts its read to a minute and then
// subtracts 103 s, written in seconds (cut_m(now) - 103s), whose two runs
// read at different phases of their minutes, so no offset at the values'
// own second holds both, is read at the minute its two values stand a
// whole number of apart and locked to the remainder both share within it:
// clock - 1m43s, read cut to a minute. A replay passes the application's
// value at every phase of the minute, and fails at every phase a dropped
// look-back (the read cut to the second) and one changed by half a minute
// either way. Main's reading learns it as a whole minute's offset, which a
// dropped look-back passes.
func TestAMinuteCutLookBackLearnedAtItsStepIsLockedToItsPhase(t *testing.T) {
	sec, ms, min := time.Second, time.Millisecond, time.Minute
	cutter := func(t time.Time) time.Time { return t.Truncate(min).Add(-103 * sec) }
	p := "body.lastSeen.$lte"
	base := time.Date(2026, 10, 26, 11, 14, 0, 0, time.UTC)
	learned := 0
	for _, ph := range []struct{ r, f time.Duration }{{7 * sec, 21 * sec}, {50 * sec, 3 * sec}, {31*sec + 500*ms, 33 * sec}} {
		rec := v5Call(base.Add(ph.r), 61*ms, 20*ms)
		f := v5Call(base.Add(3*min+ph.f), 61*ms, 20*ms)
		rv, lv := cutter(rec.read), cutter(f.read)
		o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), v5Window(rec, f), nil)
		if !ok || o.Step != min || o.Min != -103*sec || o.Max != -102*sec || !o.Narrow() {
			t.Errorf("phases %v: %+v %q %v; want clock - 1m43s locked to the minute", ph, o, why, ok)
			continue
		}
		learned++
		recText := strconv.FormatInt(rv.Unix(), 10)
		for phase := time.Duration(0); phase < min; phase += 1200 * ms {
			s := Span{From: base.Add(48*time.Hour + phase), To: base.Add(48*time.Hour + phase + 61*ms)}
			read := s.To.Add(-20 * ms)
			for _, row := range []struct {
				name string
				at   time.Time
				want bool
			}{
				{"the application's value", cutter(read), true},
				{"a dropped look-back", read.Truncate(sec), false},
				{"half a minute shorter", cutter(read).Add(30 * sec), false},
				{"half a minute longer", cutter(read).Add(-30 * sec), false},
			} {
				if got, why := v5Check(p, o, recText, row.at, s); got != row.want {
					t.Errorf("%s: phases %v, a replay %s into its minute, %s: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], ph, phase, row.name, got, why)
				}
			}
		}
		for _, part := range []string{"read cut to 1m0s before the offset", "A replay passes a value only where it is a whole 1m of its call's window", "a look-back dropped, or changed by part of 1m, fails at every read", "a false red, never a false green"} {
			if !strings.Contains(o.Why, part) {
				t.Errorf("the line does not say %q: %s", part, o.Why)
			}
		}
	}
	if learned == 0 {
		t.Fatal("no recording learned at the step")
	}
}
