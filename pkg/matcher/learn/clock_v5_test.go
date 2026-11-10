package learn

import (
	"encoding/base64"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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

// The call's window runs from its previous data-plane call, so load (or an
// idle poll) can widen it to seconds: api-server's reconciler, recorded on
// boxes at 2 to 3.5 load per CPU, showed windows that held its dirtyAt <=
// now - 2s as a read with no look-back too, and in others three whole
// seconds of look-back, where a quiet window holds one. A look-back cut to
// the second is read there as the read nearest its call (the latest whole
// second both runs' windows put a read in, the one a quiet window reads),
// so a window widened by load learns the Clock a quiet one does, at every
// phase of the call's second: clock - 2s (and clock - 15s), checked at the
// cut, whose replay passes the application's value at every phase, in a
// quiet window and in one as wide, and fails a stale value and a plain
// read, and in a quiet window a look-back changed by three seconds. A
// plain read made just before its call in such a window is no look-back
// (the order's plain read stands); one the application made 1.2 s before
// its call in both runs is read as a look-back of the seconds between,
// which a replay passes where the application waits as long again.
func TestALookBackInAWindowWidenedByLoadIsReadAsTheReadNearestItsCall(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.cmd.query.$and.0.$or.0.dirtyAt.$lte.$numberLong"
	start := time.Date(2026, 11, 1, 15, 21, 40, 0, time.UTC)
	type row = struct {
		name string
		at   time.Time
		want bool
	}
	// check runs a replay of o at every phase of a quiet window and of one
	// 3.2 s wide, the application reading lead before its call.
	check := func(name string, o models.ClockOffset, recText string, lead time.Duration, rows func(read time.Time, quiet bool) []row) {
		t.Helper()
		for _, width := range []time.Duration{61 * ms, 3200 * ms} {
			for ph := time.Duration(0); ph < sec; ph += 70 * ms {
				s := Span{From: start.Add(48*time.Hour + ph), To: start.Add(48*time.Hour + ph + width)}
				for _, row := range rows(s.To.Add(-lead), width < sec) {
					if got, why := v5Check(p, o, recText, row.at, s); got != row.want {
						t.Errorf("%s: %s, a replay's window %s wide at %s into its second, %s: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], name, width, ph, row.name, got, why)
					}
				}
			}
		}
	}
	for _, back := range []time.Duration{2 * sec, 15 * sec} {
		for _, width := range []time.Duration{61 * ms, 400 * ms, 700 * ms, 900 * ms, 1100 * ms, 1500 * ms, 1900 * ms, 2500 * ms, 3200 * ms, 6 * sec} {
			for phase := 7 * ms; phase < sec; phase += 90 * ms {
				rec := v5Call(start.Add(phase), width, 4*ms)
				f := v5Call(rec.to.Add(FullRoundLead-8*ms), width, 4*ms)
				rv, lv := rec.read.Truncate(sec).Add(-back), f.read.Truncate(sec).Add(-back)
				o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), v5Window(rec, f), nil)
				named := "clock - " + models.ShortDuration(back)
				name := named + " in a window " + width.String() + " wide, the call " + phase.String() + " into its second"
				if lo, hi := o.CutOffsets(); !ok || why != "" || !o.Narrow() || o.Step != sec || o.Res != sec || lo != -back || hi != -back {
					t.Errorf("FALSE RED (flips with load): %s: %+v %q %v, CutOffsets %s..%s; want %s alone, as a quiet window learns it", name, o, why, ok, lo, hi, named)
					continue
				}
				parts := []string{" = " + named + " (", "A replay checks it at the second against " + named + ", "}
				if width >= sec { // a narrower window holds one whole second at most phases, and is read as it is there
					parts = append(parts, "the read nearest its call")
				}
				for _, part := range parts {
					if !strings.Contains(o.Why, part) {
						t.Errorf("%s: the line does not say %q: %s", name, part, o.Why)
					}
				}
				check(name, o, strconv.FormatInt(rv.Unix(), 10), 4*ms, func(read time.Time, quiet bool) []row {
					rows := []row{
						{"the application's value", read.Truncate(sec).Add(-back), true},
						{"the recorded value, two days stale", rv, false},
						{"a plain read", read.Truncate(sec), false},
					}
					if quiet {
						rows = append(rows, row{"a look-back three seconds longer", read.Truncate(sec).Add(-back - 3*sec), false}, row{"a look-back three seconds shorter", read.Truncate(sec).Add(-back + 3*sec), false})
					}
					return rows
				})
			}
		}
	}
	for _, lead := range []time.Duration{4 * ms, 1200 * ms} {
		for phase := 7 * ms; phase < sec; phase += 90 * ms {
			rec := v5Call(start.Add(phase), 3200*ms, lead)
			f := v5Call(rec.to.Add(FullRoundLead-8*ms), 3200*ms, lead)
			rv, lv := rec.read.Truncate(sec), f.read.Truncate(sec)
			o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), v5Window(rec, f), nil)
			name := "a plain read " + lead.String() + " before its call, in a window 3.2s wide, the call " + phase.String() + " into its second"
			if lead < sec {
				if ok || why != "" {
					t.Errorf("FALSE RED: %s: %+v %q %v; want no offset, the order's plain read", name, o, why, ok)
				}
				continue
			}
			if !ok || why != "" || o.Max >= 0 || o.Step != sec || !strings.Contains(o.Why, "the read nearest its call") {
				t.Errorf("%s: %+v %q %v; want a look-back of the seconds between its read and its call", name, o, why, ok)
				continue
			}
			check(name, o, strconv.FormatInt(rv.Unix(), 10), lead, func(read time.Time, _ bool) []row {
				return []row{{"the application's value, read as long before its call", read.Truncate(sec), true}}
			})
		}
	}
}

// A look-back of an application that cuts its read to a minute and then
// subtracts 103 s, written in seconds (cut_m(now) - 103s), whose two runs
// read at different phases of their minutes, so no offset at the values'
// own second holds both (or one does, of no whole second: reads 700 ms
// apart in their minutes, 7.5 s and 8.2 s), is read at the minute its two
// values stand a
// whole number of apart and locked to the remainder both share within it:
// clock - 1m43s, read cut to a minute. A replay passes the application's
// value at every phase of the minute, and fails at every phase a dropped
// look-back (the read cut to the second) and one changed by half a minute
// either way; a value a whole minute stale fails except where the window
// starts in its minute's first second (one Res of slack). Main's reading
// learns it as a whole minute's offset, which a dropped look-back passes.
func TestAMinuteCutLookBackLearnedAtItsStepIsLockedToItsPhase(t *testing.T) {
	sec, ms, min := time.Second, time.Millisecond, time.Minute
	cutter := func(t time.Time) time.Time { return t.Truncate(min).Add(-103 * sec) }
	p := "body.lastSeen.$lte"
	base := time.Date(2026, 10, 26, 11, 14, 0, 0, time.UTC)
	learned := 0
	for _, ph := range []struct{ r, f time.Duration }{{7 * sec, 21 * sec}, {50 * sec, 3 * sec}, {31*sec + 500*ms, 33 * sec}, {7*sec + 500*ms, 8*sec + 200*ms}} {
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
				{"a second shorter", cutter(read).Add(sec), false},
				{"a second longer", cutter(read).Add(-sec), false},
				// A minute stale is the cut of a read a minute before:
				// within one Res of the window only where it starts in
				// its minute's first second.
				{"a whole minute stale", cutter(read).Add(-min), phase < sec},
			} {
				if got, why := v5Check(p, o, recText, row.at, s); got != row.want {
					t.Errorf("%s: phases %v, a replay %s into its minute, %s: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], ph, phase, row.name, got, why)
				}
			}
		}
		// A replay whose read fell 500 ms before its window, in the minute
		// before the one its window starts 300 ms into: within one Res of
		// the window, its value passes.
		s := Span{From: base.Add(48*time.Hour + min + 300*ms), To: base.Add(48*time.Hour + min + 361*ms)}
		if got, why := v5Check(p, o, recText, cutter(s.From.Add(-500*ms)), s); !got {
			t.Errorf("FALSE RED: phases %v, a read 500 ms before a window that starts 300 ms into its minute: %s", ph, why)
		}
		for _, part := range []string{"read cut to 1m0s before the offset", "A replay passes a value only where it is a whole 1m of its call's window (within 1s on either side) plus an offset from -1m43s to -1m42s", "a look-back dropped, or changed by part of 1m, fails at every read", "fails a replay wherever its value's remainder within 1m moves (a false red), and passes one where that remainder lands on the learned one, about one read in 60, whatever its look-back"} {
			if !strings.Contains(o.Why, part) {
				t.Errorf("the line does not say %q: %s", part, o.Why)
			}
		}
	}
	if learned == 0 {
		t.Fatal("no recording learned at the step")
	}
}

// The same minute cutter (cut_m(now) - 103s), whose two runs read within
// a few hundred milliseconds of the same second of their minutes (7.1 s and
// 7.4 s in), looks exactly like an application that does not cut: one
// offset at the values' own second holds both runs (clock - 1m50s), and
// that is what is learned (the stated cost). A replay that reads in that
// same second, or within about a cut of it, passes the application's
// value; a replay that reads at another phase of the minute fails it (a
// false red: 114 of 120 phases here, the 95 % stated), and every replay
// fails a dropped look-back. So auto-replay's own replays, which read a
// third value at a phase of their own, fail such a test about 19 times in
// 20, and it is set aside there, never kept on a contract a later replay
// breaks.
func TestAMinuteCutterReadAtTheSameSecondInBothRunsFailsAThirdReadAtAnotherPhase(t *testing.T) {
	sec, ms, min := time.Second, time.Millisecond, time.Minute
	cutter := func(t time.Time) time.Time { return t.Truncate(min).Add(-103 * sec) }
	p := "body.lastSeen.$lte"
	base := time.Date(2026, 10, 26, 11, 14, 0, 0, time.UTC)
	rec := v5Call(base.Add(7*sec+100*ms), 61*ms, 20*ms)
	f := v5Call(base.Add(3*min+7*sec+400*ms), 61*ms, 20*ms)
	rv, lv := cutter(rec.read), cutter(f.read)
	o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), v5Window(rec, f), nil)
	if !ok || o.Step != sec || o.Res != sec || !strings.Contains(o.Why, " = clock - 1m50s (") {
		t.Fatalf("%+v %q %v; want clock - 1m50s at the values' own second", o, why, ok)
	}
	recText := strconv.FormatInt(rv.Unix(), 10)
	passed := 0
	for phase := time.Duration(0); phase < min; phase += 500 * ms {
		s := Span{From: base.Add(48*time.Hour + phase), To: base.Add(48*time.Hour + phase + 61*ms)}
		read := s.To.Add(-20 * ms)
		app, _ := v5Check(p, o, recText, cutter(read), s)
		if app {
			passed++
			if d := phase - 7*sec; d < -2*sec || d > 2*sec {
				t.Errorf("a replay %s into its minute passes the application's value; want it to fail there (a false red auto-replay sets aside)", phase)
			}
		}
		if got, why := v5Check(p, o, recText, read.Truncate(sec), s); got {
			t.Errorf("FALSE GREEN: a replay %s into its minute passes a dropped look-back (%s)", phase, why)
		}
	}
	if passed == 0 || passed > 6 {
		t.Errorf("the application's value passed %d of 120 replays; want only those reading within about a cut of 7 s into the minute", passed)
	}
}

// A client test whose runs know no test begin sends an expiry an hour
// ahead (expires_at = now + 1h), read 300 ms before its call when recorded
// and 200 ms before it in round F, outside the call's 61 ms window. Written
// as an epoch's seconds, the read cut to the second, both runs hold one
// offset in the call's window, and it is no Fine one: a Clock of an hour
// at the second (the order's step 5), which a replay passes where the
// application read its clock within about a second of its call, and fails
// an hour and a minute, fifty-nine minutes, and the recorded value. Written
// at the millisecond, the two waits show no one offset: no Clock and no
// refusal, so the order's step 8 binds it as the test's own value, as
// DESIGN pins (TestOutcomeAClientTestsValueMadeAfterItsBeginIsNotItsOwn's
// case of no begin known). Read 300 ms before its call in both runs, it
// shows one offset in a millisecond's unit (where the two values share
// no coarser step): refused as Fine, as main refuses it.
func TestAFutureInstantOfAClientTestWithNoBeginIsAClockWhereItsOffsetHolds(t *testing.T) {
	sec, ms, min := time.Second, time.Millisecond, time.Minute
	p := "body.expires_at"
	base := time.Date(2026, 10, 26, 11, 14, 0, 0, time.UTC)
	rec := v5Call(base.Add(400*ms), 61*ms, 300*ms)
	f := v5Call(base.Add(3*min+17*sec+700*ms), 61*ms, 200*ms)
	w := v5Window(rec, f)
	if w.sinceBegin() != nil {
		t.Fatal("the window knows a test begin")
	}
	exp := func(read time.Time) time.Time { return read.Add(time.Hour) }
	o, why, ok := clockInWindow(p, v5Instant(exp(rec.read).Truncate(sec), sec), v5Instant(exp(f.read).Truncate(sec), sec), w, nil)
	if !ok || o.Fine() || o.Step != sec || o.Min > time.Hour || o.Max <= time.Hour {
		t.Fatalf("FALSE RED: an hour ahead at the second: %+v %q %v; want a Clock of clock + 1h at the second", o, why, ok)
	}
	recText := strconv.FormatInt(exp(rec.read).Truncate(sec).Unix(), 10)
	for phase := time.Duration(0); phase < sec; phase += 100 * ms {
		s := Span{From: base.Add(48*time.Hour + phase), To: base.Add(48*time.Hour + phase + 61*ms)}
		for _, row := range []struct {
			name string
			at   time.Time
			want bool
		}{
			{"the application's value, read 250 ms before its call", exp(s.To.Add(-250 * ms)).Truncate(sec), true},
			{"the application's value, read in its window", exp(s.To.Add(-20 * ms)).Truncate(sec), true},
			{"an hour and a minute ahead", exp(s.To).Add(min).Truncate(sec), false},
			{"fifty-nine minutes ahead", exp(s.To).Add(-min).Truncate(sec), false},
			{"the recorded value", exp(rec.read).Truncate(sec), false},
		} {
			if got, why := v5Check(p, o, recText, row.at, s); got != row.want {
				t.Errorf("%s: a replay %s into its second, %s: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], phase, row.name, got, why)
			}
		}
	}
	if o, why, ok := clockInWindow(p, v5Instant(exp(rec.read), ms), v5Instant(exp(f.read), ms), w, nil); ok || why != "" {
		t.Errorf("an hour ahead at the millisecond, waits of 300 and 200 ms: %+v %q %v; want no Clock and no refusal (step 8's fresh)", o, why, ok)
	}
	same := v5Call(base.Add(3*min+17*sec+437*ms), 61*ms, 300*ms)
	if o, why, ok := clockInWindow(p, v5Instant(exp(rec.read), ms), v5Instant(exp(same.read), ms), v5Window(rec, same), nil); ok || !strings.Contains(why, "keploy refuses") {
		t.Errorf("an hour ahead at the millisecond, both waits 300 ms: %+v %q %v; want refused as Fine", o, why, ok)
	}
}

// An application in +05:30 that cuts its read to an hour, then looks back
// ten minutes, writes its value at the second (an epoch's seconds): a Java
// application cuts to its own zone's hour (truncatedTo), which starts at
// half past an hour in UTC; a Go one cuts to UTC's (Truncate counts from
// UTC). Recorded at 09:40 UTC or at 09:10 UTC (where both grids hold a
// look-back of the Java cutter's value: its own, the shorter, is kept) and
// replayed an hour and five minutes later,
// each is locked to its phase on its own grid: a replay passes the
// application's value at every phase of the hour, with keploy reading the
// application's zone from its process or from the learn step's name of it,
// and fails a dropped look-back and one changed by seven minutes at every
// phase. Read on UTC's grid alone, the zone's cutter is no Clock.
func TestAnHourCutLookBackInAZoneOffTheHourIsLockedToItsZonesPhase(t *testing.T) {
	sec, ms, min := time.Second, time.Millisecond, time.Minute
	ist := time.FixedZone("+05:30", 5*3600+1800)
	p := "body.cutoff.$lte"
	// Recorded at 09:40 UTC (15:10 in +05:30), one grid holds a look-back of
	// each cutter; at 09:10 UTC (14:40), both grids hold one of the Java
	// cutter's value (08:20 UTC: 40 minutes before UTC's hour, ten before
	// its zone's), and the shorter is its own.
	for _, base := range []time.Time{time.Date(2026, 10, 26, 9, 40, 0, 0, time.UTC), time.Date(2026, 10, 26, 9, 10, 0, 0, time.UTC)} {
		rec := v5Call(base, 61*ms, 20*ms)
		f := v5Call(base.Add(time.Hour+5*min), 61*ms, 20*ms)
		for _, c := range []struct {
			name string
			cut  func(time.Time) time.Time
			want time.Duration
		}{
			{"its zone's hour (Java)", func(t time.Time) time.Time { return models.ReadCut(t, time.Hour, ist).Add(-10 * min).Truncate(sec) }, -10 * min},
			{"UTC's hour (Go)", func(t time.Time) time.Time { return t.Truncate(time.Hour).Add(-10 * min) }, -10 * min},
		} {
			rv, lv := c.cut(rec.read), c.cut(f.read)
			o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), v5Window(rec, f), ist)
			if !ok || o.Step != time.Hour || o.Min != c.want || o.Max != c.want+min || o.AppZone != "+05:30" {
				t.Errorf("FALSE RED: recorded at %s, a cut to %s less 10m: %+v %q %v; want clock - 10m locked to the hour, in +05:30", base.Format("15:04"), c.name, o, why, ok)
				continue
			}
			recText := strconv.FormatInt(rv.Unix(), 10)
			for _, zone := range []struct {
				what string
				s    Span
			}{
				{"read from its process", Span{Zone: ist, Own: true}},
				{"named by the learn step", Span{}},
			} {
				for phase := time.Duration(0); phase < time.Hour; phase += 5*min + 7*sec {
					s := zone.s
					s.From = base.Add(48*time.Hour + phase)
					s.To = s.From.Add(61 * ms)
					read := s.To.Add(-20 * ms)
					for _, row := range []struct {
						name string
						at   time.Time
						want bool
					}{
						{"the application's value", c.cut(read), true},
						{"a dropped look-back", read.Truncate(sec), false},
						{"seven minutes shorter", c.cut(read).Add(7 * min), false},
						{"seven minutes longer", c.cut(read).Add(-7 * min), false},
					} {
						if got, why := v5Check(p, o, recText, row.at, s); got != row.want {
							t.Errorf("%s: recorded at %s, a cut to %s, the zone %s, a replay %s into its hour, %s: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], base.Format("15:04"), c.name, zone.what, phase, row.name, got, why)
						}
					}
				}
			}
		}
	}
}

// A time to live added to a read the application made earlier in its test,
// cut to the second (a token's expires_at = cut_s(now) + 30d, read a few
// seconds before its call), is learned from the test's begin, never as the
// offset its call's window shows (clock + 29d23h59m57s), which measures the
// wait between the read and the call as much as the time to live: through
// the order's step 5 (clockInWindow) where both runs' waits stand within a
// second of each other, and through its step 7 (clockInWindow in the
// window from each run's test begin, then clockSinceBegin) where they do
// not. A replay passes the application's value however long it waited
// between its read and its call (by up to a second more or less than
// either run, and more), at every phase of the second, and fails a time to
// live changed by an hour or a day, and a read with no time to live.
func TestATimeToLiveAddedToAnEarlyReadIsCheckedFromItsTestsBegin(t *testing.T) {
	sec, ms, day := time.Second, time.Millisecond, 24*time.Hour
	ttl := func(read time.Time) time.Time { return read.Truncate(sec).Add(30 * day) }
	p := "body.expires_at"
	call := time.Date(2026, 10, 26, 11, 14, 17, 155e6, time.UTC)
	run := func(call time.Time, wait time.Duration) (Span, time.Time) {
		return Span{From: call.Add(-61 * ms), To: call, Began: call.Add(-12 * sec)}, call.Add(-wait)
	}
	for _, c := range []struct {
		name         string
		recW, fW     time.Duration
		step5, step7 bool
	}{
		{"waits within a second (step 5)", 3200 * ms, 2900 * ms, true, false},
		{"waits equal, its call's window showing a whole second (step 5)", 3 * sec, 3 * sec, true, false},
		{"waits 2.7 s apart (step 7)", 3200 * ms, 5900 * ms, false, true},
	} {
		rs, rr := run(call, c.recW)
		fs, fr := run(call.Add(FullRoundLead-8*ms), c.fW) // values 62 s and 59 s apart: no whole minute
		w := (&Window{Rec: rs, F: fs}).ComparesOffsets()
		rv, lv := ttl(rr), ttl(fr)
		o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), w, nil)
		if ok != c.step5 {
			t.Errorf("%s: step 5 found %+v %q %v; want found %v", c.name, o, why, ok, c.step5)
			continue
		}
		if !ok {
			ob, _, bok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), w.sinceBegin(), nil)
			ln, sok := clockSinceBegin(Learned{Kind: KindClockOffset, Offset: &ob})
			if !bok || !sok || ln.Offset == nil {
				t.Errorf("%s: step 7 found %+v %v %v; want a time to live since the test began", c.name, ob, bok, sok)
				continue
			}
			o = *ln.Offset
		}
		if o.Since != models.SinceTestBegin || o.Min <= 0 || !o.Narrow() || o.Step != sec {
			t.Errorf("%s: %+v (Narrow %v); want clock + 30d at the second, read since the test began, that the order's guard keeps", c.name, o, o.Narrow())
			continue
		}
		for _, part := range []string{"made after its test began and before its call, plus a time to live", "however long the application waited between its read and its call", "a time to live changed by less than the test's span"} {
			if !strings.Contains(o.Why, part) {
				t.Errorf("%s: the line does not say %q: %s", c.name, part, o.Why)
			}
		}
		recText := strconv.FormatInt(rv.Unix(), 10)
		base := call.Add(48 * time.Hour)
		for phase := time.Duration(0); phase < sec; phase += 50 * ms {
			for _, wait := range []time.Duration{2 * sec, 2900 * ms, 3200 * ms, 4200 * ms, 6 * sec, 11 * sec} {
				s, read := run(base.Add(phase), wait)
				for _, row := range []struct {
					name string
					at   time.Time
					want bool
				}{
					{"the application's value", ttl(read), true},
					{"a time to live an hour longer", ttl(read).Add(time.Hour), false},
					{"a time to live a day shorter", ttl(read).Add(-day), false},
					{"no time to live", read.Truncate(sec), false},
				} {
					if got, why := v5Check(p, o, recText, row.at, s); got != row.want {
						t.Errorf("%s: %s: a replay %s into its second that waited %s, %s: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], c.name, phase, wait, row.name, got, why)
					}
				}
			}
		}
	}
}

// A look-back read since its test began is declined, never learned: checked
// from the test's begin, a poll's dirtyAt <= now - 30s would pass one the
// application shortened, or dropped, within the test's span. The order's
// step 7 (clockSinceBegin) declines one handed to it, and the reading in
// the window from each run's test begin finds none; a plain read and a time
// to live are taken.
func TestALookBackIsNeverCheckedFromItsTestsBegin(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	back := models.ClockOffset{Min: -31 * sec, Max: -29 * sec, Res: sec, Step: sec, Layout: models.LayoutTyped}
	if ln, ok := clockSinceBegin(Learned{Kind: KindClockOffset, Offset: &back}); ok {
		t.Errorf("FALSE GREEN: a look-back was taken since its test began: %+v", ln.Offset)
	}
	around := models.ClockOffset{Min: -sec, Max: sec, Res: sec, Step: sec, Layout: models.LayoutTyped}
	if ln, ok := clockSinceBegin(Learned{Kind: KindClockOffset, Offset: &around}); ok {
		t.Errorf("FALSE GREEN: an offset around zero, no time to live, was taken since its test began: %+v", ln.Offset)
	}
	for _, o := range []models.ClockOffset{{Res: sec}, {Min: 30 * 24 * time.Hour, Max: 30*24*time.Hour + 3*sec, Res: sec, Step: sec}} {
		if ln, ok := clockSinceBegin(Learned{Kind: KindClockOffset, Offset: &o}); !ok || ln.Offset.Since != models.SinceTestBegin {
			t.Errorf("FALSE RED: %+v was not taken since its test began", o)
		}
	}
	call := time.Date(2026, 10, 26, 11, 14, 17, 155e6, time.UTC)
	span := func(call time.Time) Span { return Span{From: call.Add(-61 * ms), To: call, Began: call.Add(-12 * sec)} }
	fcall := call.Add(FullRoundLead - 8*ms)
	w := (&Window{Rec: span(call), F: span(fcall)}).ComparesOffsets()
	rv, lv := call.Add(-3200*ms).Truncate(sec).Add(-30*sec), fcall.Add(-5400*ms).Truncate(sec).Add(-30*sec)
	if o, why, ok := clockInWindow("body.dirtyAt.$lte", v5Instant(rv, sec), v5Instant(lv, sec), w.sinceBegin(), nil); ok || why != "" {
		t.Errorf("FALSE GREEN: a look-back was read since its test began: %+v %q", o, why)
	}
}

// A look-back of an application that does not cut its read, whose two
// runs' values stand a whole number of a coarser step apart by chance
// (seconds now - 75s with values a whole minute apart; milliseconds now -
// 1500ms a whole second apart), is read at its values' own cut, never at
// that step, where it would stand for the whole step and pass a dropped
// look-back. Of seconds, it is learned at the second as the application's
// own look-back (clock - 1m15s) and a replay passes the application's value
// at every phase of the minute and fails a dropped look-back, and one
// changed by half a minute, at every phase. Of milliseconds, the offset
// reads below a second or in seconds at no cut, and is refused as main
// refuses every such look-back, with main's reason, even in client tests
// (whose window from the test's begin takes no look-back).
func TestALookBackOfAWriterThatDoesNotCutIsReadAtItsOwnCut(t *testing.T) {
	sec, ms, min := time.Second, time.Millisecond, time.Minute
	p := "body.lastSeen.$lte"
	for _, sh := range []struct {
		name       string
		back, step time.Duration
		res        time.Duration
	}{
		{"seconds, now - 75s", 75 * sec, min, sec},
		{"seconds, now - 90s", 90 * sec, min, sec},
		{"seconds, now - 2m30s", 150 * sec, min, sec},
		{"seconds, now - 5m30s", 330 * sec, min, sec},
		{"seconds, now - 2m (whole)", 120 * sec, min, sec},
		{"milliseconds, now - 1500ms", 1500 * ms, sec, ms},
		{"milliseconds, now - 2300ms", 2300 * ms, sec, ms},
		{"milliseconds, now - 45.5s", 45500 * ms, sec, ms},
		{"milliseconds, now - 2s (whole)", 2 * sec, sec, ms},
	} {
		value := func(read time.Time) time.Time { return read.Truncate(sh.res).Add(-sh.back) }
		seen := 0
		for rp := time.Duration(0); rp < sh.step; rp += sh.step / 7 {
			rec := v5Call(time.Date(2026, 10, 26, 11, 14, 0, 0, time.UTC).Add(rp+61*ms), 61*ms, 20*ms)
			// Round F reads within a fifth of a second of the recording's
			// phase, a whole step later, so its value stands a whole step
			// from the recording's.
			f := v5Call(rec.to.Add(3*sh.step+sh.res*7/10), 61*ms, 20*ms)
			rv, lv := value(rec.read), value(f.read)
			if lv.Sub(rv)%sh.step != 0 || rv.Truncate(sh.step).Equal(rv) && lv.Truncate(sh.step).Equal(lv) {
				continue
			}
			seen++
			for _, client := range []bool{false, true} {
				rs, fs := Span{From: rec.from, To: rec.to}, Span{From: f.from, To: f.to}
				if client {
					rs.Began, fs.Began = rec.from.Add(-12*sec), f.from.Add(-12*sec)
				}
				w := (&Window{Rec: rs, F: fs}).ComparesOffsets()
				o, why, ok := clockInWindow(p, v5Instant(rv, sh.res), v5Instant(lv, sh.res), w, nil)
				if sh.res == ms {
					if ok || !strings.Contains(why, "so keploy refuses an offset that reads in a second or finer") {
						t.Errorf("FALSE GREEN: %s (client tests %v, phase %s): %+v %q %v; want it refused with main's reason", sh.name, client, rp, o, why, ok)
					}
					continue
				}
				named := " = clock - " + models.ShortDuration(sh.back) + " ("
				if !ok || o.Step != sec || o.Res != sec || o.Since != "" || !o.Narrow() || !strings.Contains(o.Why, named) || !strings.Contains(o.Why, ", cut to a second. ") {
					t.Errorf("%s (client tests %v, phase %s): %+v %q %v; want %q read at its own cut, the line saying so", sh.name, client, rp, o, why, ok, named)
					continue
				}
				recText := strconv.FormatInt(rv.Unix(), 10)
				for ph := time.Duration(0); ph < sh.step; ph += 1300 * ms {
					s := Span{From: rec.from.Add(48*time.Hour + ph), To: rec.from.Add(48*time.Hour + ph + 61*ms)}
					read := s.To.Add(-20 * ms)
					for _, row := range []struct {
						name string
						at   time.Time
						want bool
					}{
						{"the application's value", value(read), true},
						{"a dropped look-back", read.Truncate(sec), false},
						{"half a step shorter", value(read).Add(sh.step / 2), false},
						{"half a step longer", value(read).Add(-sh.step / 2), false},
					} {
						if got, why := v5Check(p, o, recText, row.at, s); got != row.want {
							t.Errorf("%s: %s (client tests %v, phase %s), a replay %s into its step, %s: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], sh.name, client, rp, ph, row.name, got, why)
						}
					}
				}
			}
		}
		if seen == 0 {
			t.Errorf("%s: no recording stood a whole step apart", sh.name)
		}
	}
}

// A milliseconds writer that cuts its read to the second and subtracts
// 1500 ms (cut_s(now) - 1500ms, every value at .500) writes two values a
// whole second apart in every recording. Its bands, by how far round F's
// read stands from the recording's within their seconds: where both runs
// read within about their windows of one phase (61 ms each here), an
// offset at the millisecond holds both, reads in seconds at no cut, and is refused with
// main's reason (a false red, never a false green); everywhere else no
// offset at the millisecond holds both, and the look-back is read at the
// second, locked to the phase both runs share: a replay passes the
// application's value at every phase and fails a dropped look-back, and
// one changed by half a second, at every phase. Never learned at the
// millisecond, and never at the step unlocked.
func TestAMillisecondsSecondCutterIsLockedToItsPhaseOrRefused(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.until"
	value := func(read time.Time) time.Time { return read.Truncate(sec).Add(-1500 * ms) }
	rec := v5Call(time.Date(2026, 10, 26, 11, 14, 17, 155e6, time.UTC), 61*ms, 20*ms)
	refused, locked := 0, 0
	for d := time.Duration(0); d < sec; d += 10 * ms {
		f := v5Call(rec.to.Add(73*sec+d), 61*ms, 20*ms)
		rv, lv := value(rec.read), value(f.read)
		o, why, ok := clockInWindow(p, v5Instant(rv, ms), v5Instant(lv, ms), v5Window(rec, f), nil)
		near := d < 100*ms || d > sec-100*ms
		switch {
		case !ok && strings.Contains(why, "so keploy refuses an offset that reads in a second or finer"):
			refused++
			if !near {
				t.Errorf("FALSE RED: round F %s later in its second: refused (%s); want it locked to its phase", d, why)
			}
			continue
		case !ok || o.Step != sec || o.Res != ms || o.Min != -1500*ms || o.Max != -1499*ms || !o.Narrow():
			t.Errorf("round F %s later in its second: %+v %q %v (Narrow %v); want clock - 1.5s locked to the second, which the order's guard keeps, or main's refusal", d, o, why, ok, o.Narrow())
			continue
		}
		locked++
		if d < 40*ms || d > sec-40*ms {
			t.Errorf("round F %s later in its second: locked; want main's refusal where both runs read within their windows of one phase", d)
		}
		recText := strconv.FormatInt(rv.UnixMilli(), 10)
		for ph := time.Duration(0); ph < sec; ph += 70 * ms {
			s := Span{From: rec.from.Add(48*time.Hour + ph), To: rec.from.Add(48*time.Hour + ph + 61*ms)}
			read := s.To.Add(-20 * ms)
			for _, row := range []struct {
				name string
				at   time.Time
				want bool
			}{
				{"the application's value", value(read), true},
				{"a dropped look-back", read, false},
				{"half a second shorter", value(read).Add(500 * ms), false},
				{"half a second longer", value(read).Add(-500 * ms), false},
			} {
				o.Layout = clockTextLayout(recText)
				if got, why := CheckClockRead(p, o, Leaf{Path: p, Noise: p, Type: String, Value: strconv.FormatInt(row.at.UnixMilli(), 10)}, s, true); got != row.want {
					t.Errorf("%s: round F %s later, a replay %s into its second, %s: %v (%s) [%s]", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], d, ph, row.name, got, why, recText)
				}
			}
		}
	}
	t.Logf("cut_s(now) - 1500ms: %d of 100 recordings refused, %d locked to their phase", refused, locked)
	if refused == 0 || locked == 0 {
		t.Errorf("bands: %d refused, %d locked; want both", refused, locked)
	}
}

// api-server's TestStaffBlockAnOAuthClient: an OAuth grant's expires_at =
// now + 30d, a millisecond $date the application does not cut, whose two
// values stand a whole number of seconds apart by chance (1m49s), is
// learned at the second (clock + 30d, from 719h59m59.999s to 720h0m1.001s)
// and its line says exactly what a replay passes: a read in its call's
// window plus an offset from one second below Min to one second above Max
// passes at every read (a time to live changed by a second less, or two
// more), one up to one more second and the window's width below that or
// the window's width above it only at some reads, and one further out at
// no read (a time to live changed by three seconds, or a day). Before, the
// line said only that one changed by a day or more fails.
func TestAnOAuthGrantsThirtyDaysLineSaysWhatAReplayPasses(t *testing.T) {
	const p = "body.docs.msgs.{}.expires_at"
	ms, sec, day := time.Millisecond, time.Second, 24*time.Hour
	call := time.Date(2026, 10, 30, 19, 44, 12, 155e6, time.UTC)
	read := call.Add(-ms)
	fread := read.Add(109 * sec)
	w := (&Window{Rec: Span{From: call.Add(-2 * ms), To: call}, F: Span{From: fread.Add(-ms), To: fread.Add(ms)}}).ComparesOffsets()
	o, why, ok := clockInWindow(p, v5Instant(read.Add(30*day), ms), v5Instant(fread.Add(30*day), ms), w, nil)
	if !ok || why != "" || o.Min != 30*day-ms || o.Max != 30*day+sec+ms || o.Res != sec {
		t.Fatalf("FALSE RED: %+v %q %v; want clock + 30d from 719h59m59.999s to 720h0m1.001s at the second", o, why, ok)
	}
	for _, part := range []string{
		"A replay checks a value written as recorded at 1s: it passes one from one 1s before the start of the 1s that holds its call's window's start plus 719h59m59.999s up to one 1s after the window's end plus 720h0m1.001s.",
		"So a read in that window plus an offset from 719h59m58.999s to 720h0m2.001s passes at every read; plus an offset from one 1s and the window's width below 719h59m58.999s up to it, or from 720h0m2.001s up to the window's width above it, only where the read falls late or early enough in the window and its 1s; and plus any other offset (a time to live changed by more) at no read",
	} {
		if !strings.Contains(o.Why, part) {
			t.Errorf("the line does not say %q: %s", part, o.Why)
		}
	}
	if strings.Contains(o.Why, "a day or more fails") {
		t.Errorf("the line still says only that a day or more fails: %s", o.Why)
	}
	every, some := [2]time.Duration{o.Min - o.Res, o.Max + o.Res}, [2]time.Duration{o.Min - 2*o.Res, o.Max + o.Res}
	for _, width := range []time.Duration{2 * ms, 50 * ms, 400 * ms} {
		passes := map[time.Duration]int{}
		phases := 0
		for phase := time.Duration(0); phase < sec; phase += 10 * ms {
			from := time.Date(2026, 11, 2, 9, 0, 7, 0, time.UTC).Add(phase)
			to := from.Add(width)
			phases++
			for d := 30*day - 3*sec; d <= 30*day+3*sec; d += 250 * ms {
				for _, r := range []time.Time{from, from.Add(width / 2), to} {
					got := o.Holds(r.Add(d).Truncate(ms), ms, from, to)
					switch {
					case d >= every[0] && d <= every[1] && !got:
						t.Errorf("FALSE RED against the line: window %s, %s into its second, a read %s into it plus %s fails", width, phase, r.Sub(from), d)
					case (d <= some[0]-width || d > some[1]+width) && got:
						t.Errorf("FALSE GREEN against the line: window %s, %s into its second, a read %s into it plus %s passes", width, phase, r.Sub(from), d)
					}
					if got && r.Equal(to) {
						passes[d-30*day]++
					}
				}
			}
		}
		// The time to live as review measured it, the read at the window's end.
		if passes[2*sec] != phases || passes[-sec] != phases || passes[3*sec] != 0 || passes[-3*sec] != 0 {
			t.Errorf("window %s: of %d phases, a time to live changed by +2s passes %d, -1s %d, +3s %d, -3s %d; want all, all, none, none", width, phases, passes[2*sec], passes[-sec], passes[3*sec], passes[-3*sec])
		}
	}
}

// A look-back of an application that does not cut (lastSeen <= now - 90s,
// or - 75s, an epoch's seconds) whose read was made before its call's
// window, 100 ms before it in the recording and 1.5 s in record's full
// round (or 300 ms and 900 ms), and whose two values stand a whole minute
// apart by chance (about one recording in sixty). Where an offset at the
// values' own second holds both runs it is read there: the application's
// value passes at every replay read, and a dropped look-back, or one
// changed by half a minute, at none. Else it is read at the minute and
// locked to the remainder both runs share within it: the application's
// value passes at a replay whose read keeps that remainder and fails
// where it moves (a false red), and any value, a dropped look-back too,
// passes only where its remainder lands on the learned one (a read in
// sixty), never at every read; the line states both. An earlier reading
// at the step passed a dropped look-back at 70 to 100 % of reads, and
// main's reading refuses every such recording. Values that both show :00
// (one such recording in sixty) read as a minute's cut, whose line states
// that a look-back changed by a minute passes at every read.
func TestAnEarlyReadLookBackOfAWriterThatDoesNotCutNeverPassesADroppedLookBack(t *testing.T) {
	sec, ms, min := time.Second, time.Millisecond, time.Minute
	const p = "body.lastSeen.$lte"
	base := time.Date(2026, 10, 26, 11, 14, 0, 0, time.UTC)
	for _, back := range []time.Duration{75 * sec, 90 * sec} {
		for _, waits := range [][2]time.Duration{{100 * ms, 1500 * ms}, {300 * ms, 900 * ms}} {
			kinds := map[string]int{}
			falseRed, replays := 0, 0
			for k := 0; k < 44; k++ {
				rread := base.Add(time.Duration(k) * 1370 * ms)
				fread := rread.Truncate(sec).Add(3*min + time.Duration(k*37%1000)*ms)
				rec, f := v5Call(rread.Add(waits[0]), 61*ms, waits[0]), v5Call(fread.Add(waits[1]), 61*ms, waits[1])
				rv, lv := rread.Truncate(sec).Add(-back), fread.Truncate(sec).Add(-back)
				o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), v5Window(rec, f), nil)
				switch {
				case !ok:
					kinds["refused or none"]++
					continue
				case o.Step == min && o.Res == min:
					kinds["both at :00, at the minute's cut"]++
					if rv.Second() != 0 || !strings.Contains(o.Why, "a look-back changed by one minute passes there at every read") {
						t.Errorf("back %s, waits %v, k %d: %+v at a minute's cut: %s", back, waits, k, o, o.Why)
					}
					continue
				case o.Step == sec && o.Res == sec:
					kinds["at its own second"]++
				case o.Step == min && o.Res == sec:
					kinds["locked to its phase"]++
					if !strings.Contains(o.Why, "an application that does not cut and wrote these two values by chance fails a replay wherever its value's remainder within 1m moves (a false red), and passes one where that remainder lands on the learned one, about one read in 60, whatever its look-back") {
						t.Errorf("the line does not state what a replay of an application that does not cut passes: %s", o.Why)
					}
				default:
					t.Errorf("back %s, waits %v, k %d: %+v %q", back, waits, k, o, why)
					continue
				}
				recText := strconv.FormatInt(rv.Unix(), 10)
				// Replays three days later keep each run's remainder.
				for i, run := range []v5Read{rec, f} {
					s := Span{From: run.from.Add(72 * time.Hour), To: run.to.Add(72 * time.Hour)}
					if got, why := v5Check(p, o, recText, run.read.Add(72*time.Hour).Truncate(sec).Add(-back), s); !got {
						t.Errorf("FALSE RED: back %s, waits %v, k %d (%+v): a replay that keeps run %d's remainder fails: %s", back, waits, k, o, i, why)
					}
				}
				// Replays at every second of the minute, with either run's wait.
				greens, n := map[string]int{}, 0
				for phase := time.Duration(0); phase < min; phase += sec + 10*ms {
					for _, wait := range waits {
						call := base.Add(72*time.Hour + phase)
						s := Span{From: call.Add(-61 * ms), To: call}
						read := call.Add(-wait).Truncate(sec)
						n++
						for _, row := range []struct {
							name string
							at   time.Time
						}{
							{"a dropped look-back", read},
							{"half a minute shorter", read.Add(-back + 30*sec)},
							{"half a minute longer", read.Add(-back - 30*sec)},
						} {
							if got, _ := v5Check(p, o, recText, row.at, s); got {
								greens[row.name]++
							}
						}
						if got, _ := v5Check(p, o, recText, read.Add(-back), s); !got {
							greens["the application's value fails"]++
						}
					}
				}
				replays += n
				falseRed += greens["the application's value fails"]
				for name, g := range greens {
					switch {
					case name == "the application's value fails" && o.Step == sec && g > 0:
						t.Errorf("FALSE RED: back %s, waits %v, k %d (%+v): the application's value fails %d of %d replays", back, waits, k, o, g, n)
					case name == "the application's value fails":
					case o.Step == sec && g > 0, 20*g > n:
						t.Errorf("FALSE GREEN: back %s, waits %v, k %d (%+v): %s passes %d of %d replays", back, waits, k, o, name, g, n)
					}
				}
			}
			if kinds["at its own second"] == 0 && kinds["locked to its phase"] == 0 {
				t.Errorf("back %s, waits %v: nothing learned (%v)", back, waits, kinds)
			}
			t.Logf("back %s, waits %v: %v; the application's value fails %d of %d replays (the stated false red of a phase lock)", back, waits, kinds, falseRed, replays)
		}
	}
}

// A client test's read in Unix seconds made just after its test began, a
// second before its call's window in both runs (api-server's audit: the
// application reads its clock, makes other calls, then sends the audit):
// the window plus an offset of 1.1 s to 1.2 s holds it, Fine at the second
// (no whole second), refused in a keploy test. In a client test the window
// from each run's test begin holds it as a plain read, which the order's
// step 7 checks from the test's begin: the reading refuses nothing there,
// so the read is not kept from step 7. A whole-second look-back at the
// second is the application's rule and no such case, and a look-back in
// milliseconds stays refused in client tests
// (TestALookBackOfAWriterThatDoesNotCutIsReadAtItsOwnCut).
func TestAFineOffsetOfAReadSinceItsTestBeganIsLeftToItsBegin(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	r0, f0 := at.Add(6300*ms), at.Add(75800*ms)
	rec, f := v5Call(r0.Add(900*ms), 100*ms, 0), v5Call(f0.Add(1400*ms), 100*ms, 0)
	rv, lv := r0.Add(ms).Truncate(sec), f0.Add(100*ms).Truncate(sec)
	for _, client := range []bool{false, true} {
		rs, fs := Span{From: rec.from, To: rec.to}, Span{From: f.from, To: f.to}
		if client {
			rs.Began, fs.Began = r0, f0
		}
		o, why, ok := clockInWindow("body.auditedAt", v5Instant(rv, sec), v5Instant(lv, sec), (&Window{Rec: rs, F: fs}).ComparesOffsets(), nil)
		switch {
		case ok:
			t.Errorf("FALSE GREEN: client tests %v: %+v; want no Clock of the call's window", client, o)
		case client && why != "":
			t.Errorf("client tests: the reading refused %q, which keeps the order's step 7 from reading it since its test began", why)
		case !client && !strings.Contains(why, "so keploy refuses an offset that reads in a second or finer"):
			t.Errorf("keploy test: %q; want main's refusal of an offset that reads in a second or finer", why)
		}
	}
	// One run's begin after its value: no read since the test began in
	// that run, so the window's reading keeps its refusal.
	for _, lateIn := range []string{"the recording", "round F"} {
		rs, fs := Span{From: rec.from, To: rec.to, Began: r0}, Span{From: f.from, To: f.to, Began: f0}
		if lateIn == "round F" {
			fs.Began = lv.Add(sec)
		} else {
			rs.Began = rv.Add(sec)
		}
		o, why, ok := clockInWindow("body.auditedAt", v5Instant(rv, sec), v5Instant(lv, sec), (&Window{Rec: rs, F: fs}).ComparesOffsets(), nil)
		if ok || !strings.Contains(why, "so keploy refuses an offset that reads in a second or finer") {
			t.Errorf("a begin after the value in %s: %+v %q %v; want main's refusal, no read since the test began", lateIn, o, why, ok)
		}
	}
}

// The same read where its two runs show a whole second (clock - 1s at the
// second): a protocol whose replay compares offsets learns that Clock, and
// its replay fails where the application's wait changes by more than a
// second (the own cut's stated cost); one whose replay compares none could
// only refuse it, though the window from each run's test begin holds the
// value as a plain read it can check, so the reading leaves it to the
// order's step 7. In a keploy test, with no begin, the offset stands and
// the order's guard refuses it as one this protocol does not compare.
func TestAnOffsetAReplayCannotCompareOfAReadSinceItsTestBeganIsLeftToItsBegin(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	r0, f0 := at.Add(6300*ms), at.Add(75800*ms)
	rec, f := v5Call(r0.Add(900*ms), 100*ms, 0), v5Call(f0.Add(1400*ms), 100*ms, 0)
	rv, lv := r0.Add(ms).Truncate(sec), f0.Add(300*ms).Truncate(sec)
	for _, row := range []struct {
		name             string
		client, compares bool
		want             bool
	}{
		{"client test, compared", true, true, true},
		{"client test, not compared", true, false, false},
		{"keploy test, compared", false, true, true},
		{"keploy test, not compared", false, false, true},
	} {
		w := &Window{Rec: Span{From: rec.from, To: rec.to}, F: Span{From: f.from, To: f.to}}
		if row.client {
			w.Rec.Began, w.F.Began = r0, f0
		}
		if row.compares {
			w = w.ComparesOffsets()
		}
		o, why, ok := clockInWindow("body.auditedAt", v5Instant(rv, sec), v5Instant(lv, sec), w, nil)
		if why != "" || ok != row.want {
			t.Errorf("%s: %+v %q %v; want Clock %v and no refusal", row.name, o, why, ok, row.want)
		} else if lo, hi := o.CutOffsets(); ok && (lo != -sec || hi != -sec) {
			t.Errorf("%s: %+v, CutOffsets %s..%s; want clock - 1s", row.name, o, lo, hi)
		}
	}
}

// What the read nearest its call leaves alone. A value in milliseconds
// shows its read's lead at its cut, so a plain read in a window 3.2 s wide
// is no look-back of that lead, and now - 2s there is left to the order as
// it was (no offset, no refusal). A quiet window that holds a turn of the
// cut in both runs keeps the two whole seconds it holds: the turn, not the
// window's width, put them there. And where one run's window is wide and
// the other's narrow, the offsets both hold may hold no whole second: no
// read nearest its call stands there, and the reading stays silent rather
// than name a whole second one run's window does not hold, or refuse what
// a later step of the order may read.
func TestTheReadNearestItsCallLeavesAloneWhatNoLoadWidened(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.cmd.query.dirtyAt.$lte"
	s := time.Date(2026, 11, 1, 15, 21, 40, 0, time.UTC)
	rec, f := v5Call(s.Add(407*ms), 3200*ms, 4*ms), v5Call(s.Add(FullRoundLead+3*ms), 3200*ms, 4*ms)
	if o, why, ok := clockInWindow(p, v5Instant(rec.read, ms), v5Instant(f.read, ms), v5Window(rec, f), nil); ok || why != "" {
		t.Errorf("milliseconds, a plain read in a wide window: %+v %q %v; want no offset and no refusal", o, why, ok)
	}
	narrow, wide := v5Read{from: s, to: s.Add(100 * ms)}, v5Read{from: s.Add(57400 * ms), to: s.Add(60600 * ms)} // bounds -3.1 s to -2 s, and -2.6 s to 1.6 s
	nv, wv := v5Instant(s.Add(-3*sec), sec), v5Instant(s.Add(58*sec), sec)
	for _, row := range []struct {
		name   string
		w      *Window
		rv, lv typedInstant
	}{
		{"the recording's window narrow, round F's wide", v5Window(narrow, wide), nv, wv},
		{"the recording's window wide, round F's narrow", v5Window(wide, narrow), wv, nv},
	} {
		if o, why, ok := clockInWindow(p, row.rv, row.lv, row.w, nil); ok || why != "" {
			t.Errorf("%s: %+v %q %v; want no offset and no refusal: both windows hold no whole second", row.name, o, why, ok)
		}
	}
}

// A quiet window at a turn of the second (the application read 10 ms
// before it, its call came 20 ms after, in both runs) holds two whole
// seconds of look-back, as a window 900 ms wide does at most phases. Both
// are read as the one nearest the call: read as two, a replay's check would
// run up to a whole second past the look-back, and a dropped look-back
// would pass at every read. At the turn that one is a second longer than
// the application's, which the one cut of slack passes.
func TestAWindowAtATurnOfTheSecondReadsTheWholeSecondNearestItsCall(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.cmd.query.dirtyAt.$lte"
	s := time.Date(2026, 11, 1, 15, 21, 40, 0, time.UTC)
	turn := func(call time.Time) v5Read { return v5Call(call.Add(20*ms), 61*ms, 30*ms) }
	rec, f := turn(s), turn(s.Add(61*sec))
	rv := rec.read.Truncate(sec).Add(-2 * sec)
	o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(f.read.Truncate(sec).Add(-2*sec), sec), v5Window(rec, f), nil)
	if lo, hi := o.CutOffsets(); !ok || why != "" || lo != -3*sec || hi != -3*sec || !strings.Contains(o.Why, "the read nearest its call") {
		t.Fatalf("a quiet window at a turn of the second: %+v %q %v, CutOffsets %s..%s; want the one whole second nearest its call", o, why, ok, lo, hi)
	}
	text := strconv.FormatInt(rv.Unix(), 10)
	for ph := 7 * ms; ph < sec; ph += 70 * ms {
		to := s.Add(48*time.Hour + ph + 61*ms)
		sp, read := Span{From: to.Add(-61 * ms), To: to}, to.Add(-4*ms)
		if got, why := v5Check(p, o, text, read.Truncate(sec).Add(-2*sec), sp); !got {
			t.Errorf("FALSE RED: the application's value, %s into its second: %s", ph, why)
		}
		if got, _ := v5Check(p, o, text, read.Truncate(sec), sp); got {
			t.Errorf("FALSE GREEN: a dropped look-back, %s into its second", ph)
		}
	}
}

// A plain read in the last second before a call that ends on a whole second
// writes the second before the call's. Read from the call's end it stands
// at offsets from -1s up to, not including, zero, so an application that
// cuts its read and subtracts a second writes it only of a read at the
// call's very nanosecond, and Holds holds nothing for a range that ends at
// zero (no look-back in it is below zero at every offset). So the read
// nearest its call reads no look-back there, in either run or both, in a
// window of any width: the window's reading (a plain read in both runs)
// stands, and the order goes on to it. A value two seconds before the same
// call is the look-back nearest it.
func TestAValueOneSecondBeforeACallEndingOnAWholeSecondIsNoLookBack(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.cmd.query.dirtyAt.$lte"
	s := time.Date(2026, 11, 1, 15, 21, 40, 0, time.UTC)
	call := func(to, width time.Duration) v5Read { return v5Read{from: s.Add(to - width), to: s.Add(to)} }
	for _, row := range []struct {
		name string
		w    *Window
	}{
		{"the recording's call on a whole second, 2.5 s windows", v5Window(call(0, 2500*ms), call(63500*ms, 2500*ms))},
		{"round F's call on a whole second, 2.5 s windows", v5Window(call(500*ms, 2500*ms), call(63*sec, 2500*ms))},
		{"both on a whole second, 1.5 s windows", v5Window(call(0, 1500*ms), call(63*sec, 1500*ms))},
		{"both on a whole second, 900 ms windows", v5Window(call(0, 900*ms), call(63*sec, 900*ms))},
		{"both on a whole second, 61 ms windows", v5Window(call(0, 61*ms), call(63*sec, 61*ms))},
		{"the recording's call on a whole second, 6 s windows", v5Window(call(0, 6*sec), call(63500*ms, 6*sec))},
	} {
		r, l := v5Instant(s.Add(-sec), sec), v5Instant(s.Add(62*sec), sec)
		if o, why, ok := clockInWindow(p, r, l, row.w, nil); ok || why != "" {
			t.Errorf("%s, the value a second before the call: %+v %q %v; want no offset and no refusal: a plain read in its last second", row.name, o, why, ok)
		}
		r, l = v5Instant(s.Add(-2*sec), sec), v5Instant(s.Add(61*sec), sec)
		o, why, ok := clockInWindow(p, r, l, row.w, nil)
		if lo, hi := o.CutOffsets(); !ok || why != "" || lo != -2*sec || hi != -2*sec {
			t.Errorf("%s, the value two seconds before the call: %+v %q %v, CutOffsets %s..%s; want clock - 2s", row.name, o, why, ok, lo, hi)
		}
	}
}

// A look-back one cut below its step (the end of the last minute: an
// application's now.Truncate(time.Minute).Add(-time.Second)) has offsets
// from one cut below zero up to, not including, zero: Max 0 says no value
// past the whole step, and it is a look-back in both runs. A replay passes
// the application's value at every phase of the step and fails a dropped
// look-back wherever its remainder is not the learned one. A Clock that can
// never pass a replay is a false contract.
func TestALookBackOfOneCutBelowItsStepPassesTheApplicationsValue(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	start := time.Date(2026, 11, 1, 15, 21, 40, 400e6, time.UTC)
	for _, row := range []struct {
		name                 string
		step, res, back, gap time.Duration
	}{
		{"cut_m(now) - 1s", time.Minute, sec, -sec, FullRoundLead},
		{"cut_h(now) - 1s", time.Hour, sec, -sec, time.Hour + 7*sec},
		{"cut_s(now) - 1ms", sec, ms, -ms, 3400 * ms},
		{"cut_m(now) - 1m1s, the control", time.Minute, sec, -61 * sec, FullRoundLead},
	} {
		value := func(read time.Time) time.Time { return read.Truncate(row.step).Add(row.back) }
		rec, f := v5Call(start, 61*ms, 4*ms), v5Call(start.Add(row.gap), 61*ms, 4*ms)
		o, why, ok := clockInWindow("body.until", v5Instant(value(rec.read), row.res), v5Instant(value(f.read), row.res), v5Window(rec, f), nil)
		if !ok || why != "" || o.Step != row.step || o.Res != row.res || !strings.Contains(o.Why, "Its two values stand "+models.ShortDuration(value(f.read).Sub(value(rec.read)))+" apart") {
			t.Errorf("%s: %+v %q %v; want it locked to its step", row.name, o, why, ok)
			continue
		}
		for i := time.Duration(0); i < 40; i++ {
			to := start.Add(48 * time.Hour).Truncate(row.step).Add(row.step*i/40 + row.step/97)
			from, read := to.Add(-61*ms), to.Add(-4*ms)
			if !o.Holds(value(read), row.res, from, to) {
				t.Errorf("FALSE RED: %s: the application's value, %s into its step", row.name, to.Sub(to.Truncate(row.step)))
			}
			dropped := read.Truncate(row.res)
			if dropped.Add(-row.back).Truncate(row.step).Equal(dropped.Add(-row.back)) {
				continue // the one read in many whose remainder is the learned one
			}
			if o.Holds(dropped, row.res, from, to) {
				t.Errorf("FALSE GREEN: %s: a dropped look-back, %s into its step", row.name, to.Sub(to.Truncate(row.step)))
			}
		}
	}
}

// A look-back of a millisecond's value in a window that also holds it as a
// plain read (dirtyAt <= now - 2s, a BSON date) is refused as the quiet
// window refuses it, whatever the window's width: read as a plain read it
// fails the application's value on every quiet replay and passes a dropped
// look-back on every one; silent, it flips with load. The read nearest the
// call stands a second or more before it in both runs, which a plain read
// made that long before its call does too: that is the rule's stated cost
// (fail closed). A read under a second before its call in either run stays
// the order's plain read.
func TestAMillisecondsLookBackIsRefusedInEveryWindowAsAQuietOneRefusesIt(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.cmd.query.dirtyAt.$lte.$date"
	start := time.Date(2026, 11, 1, 15, 21, 40, 0, time.UTC)
	fine := "so keploy refuses an offset that reads in a second or finer"
	for _, back := range []time.Duration{2 * sec, 15 * sec} {
		for _, ws := range [][2]time.Duration{{61 * ms, 61 * ms}, {400 * ms, 400 * ms}, {1100 * ms, 1100 * ms}, {1900 * ms, 1900 * ms}, {2500 * ms, 2500 * ms}, {3200 * ms, 3200 * ms}, {6 * sec, 6 * sec}, {30 * sec, 30 * sec}, {61 * ms, 3200 * ms}, {3200 * ms, 61 * ms}} {
			for phase := 7 * ms; phase < sec; phase += 190 * ms {
				rec := v5Call(start.Add(phase), ws[0], 4*ms)
				f := v5Call(rec.to.Add(FullRoundLead-8*ms), ws[1], 4*ms)
				o, why, ok := clockInWindow(p, v5Instant(rec.read.Add(-back), ms), v5Instant(f.read.Add(-back), ms), v5Window(rec, f), nil)
				if ok || !strings.Contains(why, fine) {
					t.Errorf("now - %s, windows %s and %s: %+v %q %v; want refused as a quiet window refuses it", back, ws[0], ws[1], o, why, ok)
				}
				if ws[0] > back && ws[1] > back && !strings.Contains(why, "also held a read with no look-back") {
					t.Errorf("now - %s, windows %s and %s: the refusal does not say what the window held: %q", back, ws[0], ws[1], why)
				}
				if ws[0] < back && ws[1] < back && strings.Contains(why, "also held") {
					t.Errorf("now - %s, windows %s and %s, neither holding a read with no look-back: the refusal says the window held more: %q", back, ws[0], ws[1], why)
				}
			}
		}
	}
	for _, row := range []struct {
		name         string
		rLead, fLead time.Duration
		wantRefused  bool
	}{
		{"read 4 ms before its call in both runs", 4 * ms, 4 * ms, false},
		{"read 900 ms before its call in both runs", 900 * ms, 900 * ms, false},
		{"read 1.2 s before its call in one run", 1200 * ms, 4 * ms, false},
		{"read 1.2 s before its call in both runs", 1200 * ms, 1200 * ms, true},
	} {
		rec := v5Call(start.Add(407*ms), 3200*ms, row.rLead)
		f := v5Call(rec.to.Add(FullRoundLead), 3200*ms, row.fLead)
		o, why, ok := clockInWindow(p, v5Instant(rec.read, ms), v5Instant(f.read, ms), v5Window(rec, f), nil)
		switch {
		case row.wantRefused && (ok || !strings.Contains(why, fine)):
			t.Errorf("%s: %+v %q %v; want refused, the rule's stated cost", row.name, o, why, ok)
		case !row.wantRefused && (ok || why != ""):
			t.Errorf("%s: %+v %q %v; want no offset and no refusal: the order's plain read", row.name, o, why, ok)
		}
	}
}

// A time to live under a minute at the second is refused, as main refuses
// it, where its call's window is as wide as its cut: every whole second the
// range holds stands for it, and a replay would pass one changed by the
// window's width at every read, where a narrow window's check tells it
// from one changed by two seconds. One of a minute or longer is read at its
// cut and the line says how far a changed one passes, truly.
func TestATimeToLiveInAWindowAsWideAsItsCutIsRefusedUnderAMinute(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.expiresAt"
	start := time.Date(2026, 11, 1, 15, 21, 40, 0, time.UTC)
	read := func(width, phase time.Duration, ttl time.Duration) (models.ClockOffset, string, bool, time.Time) {
		rec := v5Call(start.Add(phase), width, 4*ms)
		f := v5Call(rec.to.Add(FullRoundLead-8*ms), width, 4*ms)
		rv := rec.read.Truncate(sec).Add(ttl)
		o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(f.read.Truncate(sec).Add(ttl), sec), v5Window(rec, f), nil)
		return o, why, ok, rv
	}
	for phase := 7 * ms; phase < sec; phase += 90 * ms {
		for _, width := range []time.Duration{1000 * ms, 1500 * ms, 3200 * ms, 6 * sec} {
			if o, why, ok, _ := read(width, phase, 30*sec); ok || !strings.Contains(why, "a time to live under a minute") || !strings.Contains(why, models.ShortDuration(width)) {
				t.Errorf("clock + 30s in a window %s wide: %+v %q %v; want refused, as main refuses it", width, o, why, ok)
			}
		}
		if o, why, ok, _ := read(61*ms, phase, 30*sec); !ok || why != "" {
			t.Errorf("clock + 30s in a quiet window, %s into its second: %+v %q %v; want the Clock", phase, o, why, ok)
		}
		o, why, ok, rv := read(3200*ms, phase, 90*sec)
		if !ok || why != "" || o.Step != sec || o.Res != sec {
			t.Errorf("clock + 90s in a window 3.2s wide: %+v %q %v; want read at the second", o, why, ok)
			continue
		}
		lo, hi := o.CutOffsets()
		sh := models.ShortDuration
		if want := "a time to live from " + sh(lo-sec) + " to " + sh(hi+sec) + " passes at every read"; !strings.Contains(o.Why, want) || !strings.Contains(o.Why, "each whole second from "+sh(lo)+" to "+sh(hi)) || !strings.Contains(o.Why, "one a second further out passes only where a turn of the second falls in it, and one two seconds further out or more fails") {
			t.Errorf("clock + 90s in a window 3.2s wide: the line does not say %q: %s", want, o.Why)
		}
		text := strconv.FormatInt(rv.Unix(), 10)
		for ph := 7 * ms; ph < sec; ph += 70 * ms {
			to := start.Add(48*time.Hour + ph)
			sp, app := Span{From: to.Add(-61 * ms), To: to}, to.Add(-4*ms).Truncate(sec)
			for d := lo - sec; d <= hi+sec; d += sec {
				if got, why := v5Check(p, o, text, app.Add(d), sp); !got {
					t.Errorf("a time to live of %s, in the line's range %s to %s: %s", d, lo-sec, hi+sec, why)
				}
			}
			for _, d := range []time.Duration{lo - 3*sec, hi + 3*sec} {
				if got, _ := v5Check(p, o, text, app.Add(d), sp); got {
					t.Errorf("FALSE GREEN: a time to live of %s, past the line's range %s to %s", d, lo-sec, hi+sec)
				}
			}
		}
	}
}

// v5Decide is the order's decision on the one changed value of a call,
// recorded as rec and seen in record's full round as live, in a window
// width wide in both runs, after the places earlier. Where the order is not
// on this tree (Decide then refuses every word) the row is skipped: it runs
// wherever the two meet.
func v5Decide(t *testing.T, path string, typ Type, rec, live string, rAt, fAt time.Time, width time.Duration, earlier ...Earlier) Decision {
	t.Helper()
	if reflect.ValueOf(decideBy).Pointer() == reflect.ValueOf(refuseAll).Pointer() {
		t.Skip("the order of a changed word is not on this tree")
	}
	leaf := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Noise: path, Type: typ, Value: v}}}
	}
	dc := NewDriftCache()
	dc.appClock = true
	ds := Decide(Input{
		Drifts: dc, Drift: []string{path}, Live: leaf(live), Rec: leaf(rec),
		At: rAt, Arrived: fAt, Test: "test-2", Mock: "mock-300", Mode: ModeOwn, Earlier: earlier,
		Window: v5Window(v5Call(rAt, width, 0), v5Call(fAt, width, 0)),
		Landed: Landed{Where: LandedBetween},
		Caps:   Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
	})
	if len(ds) != 1 {
		t.Fatalf("%q to %q: %d decisions; want one", rec, live, len(ds))
	}
	return ds[0]
}

// api-server's reconciler's claim end to end, through the order: dirtyAt <=
// now - 2s, and the stale scan's now - 15s, an epoch's seconds, is a Clock
// of exactly that look-back in a quiet window and in every width load gives
// it, never a plain read, and a replay passes the application's value and
// fails a dropped look-back; a plain read made just before its call stays a
// plain read. In the widest windows this waits for the order to ask the
// reading before it asks whether the window holds a plain read: a window
// wider than the look-back holds both.
func TestAPollsDebounceIsAClockThroughTheOrderInEveryWindow(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	const p = "body.filter.dirtyAt.$lte"
	at := time.Date(2026, 10, 31, 9, 0, 0, 400e6, time.UTC)
	fAt := at.Add(2*time.Hour + 17*time.Minute + 23*time.Second + 150*ms)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	for _, width := range []time.Duration{30 * ms, 900 * ms, 1100 * ms, 2500 * ms, 3200 * ms, 6 * sec} {
		for _, back := range []time.Duration{0, 2 * sec, 15 * sec} {
			rec, live := secs(at.Add(-4*ms-back)), secs(fAt.Add(-4*ms-back))
			d := v5Decide(t, p, Number, rec, live, at, fAt, width)
			if d.Clock == nil || d.Outcome != OutcomeClock {
				t.Errorf("window %s, now - %s: %+v; want a Clock", width, back, d)
				continue
			}
			if back == 0 {
				if !d.Clock.Plain() {
					t.Errorf("window %s, a plain read: %s; want a plain read", width, d.Clock)
				}
				continue
			}
			if lo, hi := d.Clock.CutOffsets(); lo != -back || hi != -back {
				t.Errorf("window %s, now - %s: %s, CutOffsets %s..%s; want exactly that look-back", width, back, d.Clock, lo, hi)
			}
			for ph := 7 * ms; ph < sec; ph += 70 * ms {
				to := at.Add(72*time.Hour + ph)
				sp := Span{From: to.Add(-61 * ms), To: to}
				leaf := func(v time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: secs(v)} }
				if got, why := CheckClockRead(p, *d.Clock, leaf(to.Add(-4*ms-back)), sp, true); !got {
					t.Errorf("FALSE RED: window %s, now - %s, %s into its second: %s", width, back, ph, why)
				}
				if got, _ := CheckClockRead(p, *d.Clock, leaf(to.Add(-4*ms)), sp, true); got {
					t.Errorf("FALSE GREEN: window %s, now - %s dropped, %s into its second", width, back, ph)
				}
			}
		}
	}
}

// A look-back whose moment a call of an earlier test sent before the call in
// both runs (a token's lastUsedAt, sent again nine seconds later as a login
// code's authTime) is that call's value carried, never a read of the
// clock: its offset is the gap between the two tests, which a replay moves.
// The order refuses it naming that call, in every window, where without
// the earlier send the same values are a Clock of exactly that look-back.
// The same send by the call's own test (a debounce beside the mark that set
// dirtyAt = now) stays a Clock, as the order reads a coarse copy after the
// clock; so does a plain read its own window holds beside an earlier test's
// send of the same second. It waits for the order's carried-value step
// (handed to the order's owner as a patch).
func TestALookBackAnEarlierTestSentIsItsValueCarriedThroughTheOrder(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	const p, sent = "body.cmd.documents.0.authTime", "body.cmd.updates.0.u.$set.lastUsedAt"
	at := time.Date(2026, 10, 31, 9, 0, 0, 400e6, time.UTC)
	fAt := at.Add(2*time.Hour + 17*time.Minute + 23*time.Second + 150*ms)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	sentBy := func(earlierTest bool, back time.Duration, rec, live string) Earlier {
		return Earlier{Mock: "mock-a", At: at.Add(-back), Own: !earlierTest, Run: earlierTest,
			Seen: []Seen{{Side: Request, Leaf: Leaf{Path: sent, Noise: sent, Type: Number, Value: rec}, Live: live}}}
	}
	for _, width := range []time.Duration{61 * ms, 1100 * ms, 3200 * ms} {
		for _, back := range []time.Duration{2 * sec, 9 * sec, 15 * sec} {
			rec, live := secs(at.Add(-4*ms-back)), secs(fAt.Add(-4*ms-back))
			d := v5Decide(t, p, Number, rec, live, at, fAt, width, sentBy(true, back, rec, live))
			if d.Clock != nil || d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalNoClockRead || !slices.Contains(d.Refusal.Places, "mock-a "+sent) {
				t.Errorf("window %s, an earlier test's value sent again %s later: %+v; want refused (no-clock-read) naming mock-a %s", width, back, d, sent)
			}
			if d := v5Decide(t, p, Number, rec, live, at, fAt, width); d.Clock == nil || d.Outcome != OutcomeClock {
				t.Errorf("window %s, now - %s with no earlier send: %+v; want a Clock", width, back, d)
			}
			if d := v5Decide(t, p, Number, rec, live, at, fAt, width, sentBy(false, back, rec, live)); d.Clock == nil || d.Outcome != OutcomeClock {
				t.Errorf("window %s, now - %s beside its own test's send: %+v; want a Clock", width, back, d)
			}
		}
		rec, live := secs(at.Add(-4*ms)), secs(fAt.Add(-4*ms))
		if d := v5Decide(t, p, Number, rec, live, at, fAt, width, sentBy(true, 10*ms, rec, live)); d.Clock == nil || !d.Clock.Plain() {
			t.Errorf("window %s, a plain read beside an earlier test's send of its second: %+v; want a plain read", width, d)
		}
	}
}

// api-server's device-code sign-in writes a refresh token's expiration as
// time.Now().Add(30 days).Unix(), epoch seconds, 86 ms after the test before
// it wrote an OAuth grant's expires_at as its own clock + 30d at the
// millisecond, which that call's learn step learned so: the two share a
// second in both runs. They are two reads of one time to live, not the
// grant's value carried: the expiration is a Clock of clock + 30d, and a
// replay passes the application's value and fails a dropped or shorter time
// to live. Each condition holds it apart from a carried value: a grant no
// learn step learned as a read, one learned at the expiration's own cut or
// with no cut, one no meet holds (two layouts, two zones, two steps, or an
// inverted range), one whose learned offset the expiration's window does not
// hold or only touches, and a plain read sent again as a look-back stay
// refused, naming the grant.
func TestATimeToLiveAnEarlierTestReadAtAFinerCutIsARead(t *testing.T) {
	sec, ms, µs, month := time.Second, time.Millisecond, time.Microsecond, 30*24*time.Hour
	const p, sent = "body.docs.msgs.0.expiration.$numberLong", "body.docs.msgs.0.expires_at.$date.$numberLong"
	at := time.Date(2026, 11, 9, 7, 2, 32, 979e6, time.UTC)
	fAt := at.Add(FullRoundLead + 9*sec + 82*ms)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	millis := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	// grant is the earlier test's send, its read 86 ms before the call plus
	// sentOff, written by value, learned as learned (none where nil).
	grant := func(sentOff time.Duration, value func(time.Time) string, learned *models.ClockOffset) Earlier {
		read := -86 * ms
		e := Earlier{Mock: "mock-1018", At: at.Add(read), Run: true,
			Seen: []Seen{{Side: Request, Leaf: Leaf{Path: sent, Noise: sent, Type: Number, Value: value(at.Add(read + sentOff))}, Live: value(fAt.Add(read + sentOff))}}}
		if learned == nil {
			return e
		}
		return Named(e, nil, map[string]models.ClockOffset{sent: *learned})
	}
	fine := models.ClockOffset{Min: month - 771*µs, Max: month + 379*µs, Res: ms}
	rec, live := secs(at.Add(month)), secs(fAt.Add(month))
	width := 20 * ms
	d := v5Decide(t, p, Number, rec, live, at, fAt, width, grant(month, millis, &fine))
	if d.Outcome != OutcomeClock || d.Clock == nil || d.Clock.Plain() || d.Clock.Min > month || d.Clock.Max < month {
		t.Fatalf("a refresh token's clock + 30d beside an earlier test's grant read at the millisecond: %+v; want a Clock of clock + 30d", d)
	}
	for ph := 7 * ms; ph < sec; ph += 70 * ms {
		to := at.Add(72*time.Hour + ph)
		sp := Span{From: to.Add(-width), To: to}
		if ok, why := v5Check(p, *d.Clock, rec, to.Add(-4*ms+month), sp); !ok {
			t.Errorf("FALSE RED: %s into its second: %s", ph, why)
		}
		for _, bad := range []time.Duration{0, month - 5*sec, month - 24*time.Hour} {
			if ok, _ := v5Check(p, *d.Clock, rec, to.Add(-4*ms+bad), sp); ok {
				t.Errorf("FALSE GREEN: clock + %s passes, %s into its second", bad, ph)
			}
		}
	}
	at0 := func(o models.ClockOffset) *models.ClockOffset { return &o }
	// touch is a grant learned at offsets from the expiration's greatest on:
	// the two ranges touch, and share no offset.
	touch := models.ClockOffset{Min: d.Clock.Max, Max: d.Clock.Max + ms, Res: ms}
	for _, row := range []struct {
		name string
		e    Earlier
		rec  string
		live string
	}{
		{"a grant no learn step learned as a read", grant(month, millis, nil), rec, live},
		{"a grant learned at the expiration's own cut", grant(month, secs, at0(models.ClockOffset{Min: month - 500*ms, Max: month + 500*ms, Res: sec})), rec, live},
		{"a grant learned with no cut", grant(month, millis, at0(models.ClockOffset{Min: fine.Min, Max: fine.Max})), rec, live},
		{"a grant two learnings in two layouts met", grant(month, millis, at0(models.ClockOffset{Min: fine.Min, Max: fine.Max, Res: ms, Layout: models.LayoutApart})), rec, live},
		{"a grant two learnings in two zones met", grant(month, millis, at0(models.ClockOffset{Min: fine.Min, Max: fine.Max, Res: ms, Zone: models.ZoneApart})), rec, live},
		{"a grant two learnings at two steps met", grant(month, millis, at0(models.ClockOffset{Min: fine.Min, Max: fine.Max, Res: ms, Step: models.StepApart})), rec, live},
		{"a grant learned at an inverted range", grant(month, millis, at0(models.ClockOffset{Min: fine.Max, Max: fine.Min, Res: ms})), rec, live},
		{"a grant learned at an offset the expiration's window does not hold", grant(month+100*ms, millis, at0(models.ClockOffset{Min: month + 100*ms - 771*µs, Max: month + 100*ms + 379*µs, Res: ms})), rec, live},
		{"a grant learned at offsets that only touch the expiration's", grant(touch.Min+ms/2, millis, &touch), rec, live},
		{"a plain read at the millisecond sent again as now - 9s", grant(86*ms-9*sec, millis, at0(models.ClockOffset{Res: ms})), secs(at.Add(-9 * sec)), secs(fAt.Add(-9 * sec))},
	} {
		d := v5Decide(t, p, Number, row.rec, row.live, at, fAt, width, row.e)
		if d.Clock != nil || d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalNoClockRead || !slices.Contains(d.Refusal.Places, "mock-1018 "+sent) {
			t.Errorf("%s: %+v; want refused (no-clock-read) naming mock-1018 %s", row.name, d, sent)
		}
		if d := v5Decide(t, p, Number, row.rec, row.live, at, fAt, width); d.Clock == nil || d.Outcome != OutcomeClock {
			t.Errorf("%s, with no earlier send: %+v; want a Clock", row.name, d)
		}
	}
}

// The same two reads of one time to live, the grant reaching the next test
// as keploy carries it there: served in test 1 with the reads its call's
// learn step learned (Tracker.Served of Named), kept for the tests after it
// as the run's when test 1 ends (ranCalls), and read by test 2's call
// (Tracker.Test). The expiration is a Clock of clock + 30d; the same grant
// served with no learned read is refused, naming it, so the run's place does
// reach test 2.
func TestATimeToLiveAnEarlierTestReadAtAFinerCutIsAReadThroughTheRun(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ms, µs, month := time.Millisecond, time.Microsecond, 30*24*time.Hour
	const p, sent = "body.docs.msgs.0.expiration.$numberLong", "body.docs.msgs.0.expires_at.$date.$numberLong"
	at := time.Date(2026, 11, 9, 7, 2, 32, 979e6, time.UTC)
	fAt := at.Add(FullRoundLead + 9*time.Second + 82*ms)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	millis := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	read := at.Add(-86 * ms)
	fine := models.ClockOffset{Min: month - 771*µs, Max: month + 379*µs, Res: ms, Why: sent + " = clock + 30d"}
	for _, row := range []struct {
		name   string
		clocks map[string]models.ClockOffset
		clock  bool
	}{
		{"the grant's learned read of the clock + 30d at the millisecond", map[string]models.ClockOffset{sent: fine}, true},
		{"the grant with no learned read", nil, false},
	} {
		tr := NewTracker()
		tr.Begin()
		n, _, _, _ := tr.Test()
		grant := Earlier{Mock: "mock-1018", At: read, Own: true, Seen: []Seen{{Side: Request, Place: sent,
			Leaf: Leaf{Path: sent, Noise: sent, Type: Number, Value: millis(read.Add(month))}, Live: millis(fAt.Add(-86*ms + month))}}}
		tr.Served(n, Named(grant, nil, row.clocks))
		tr.End()
		tr.Begin()
		_, earlier, _, _ := tr.Test()
		d := v5Decide(t, p, Number, secs(at.Add(month)), secs(fAt.Add(month)), at, fAt, 20*ms, earlier...)
		switch {
		case row.clock && (d.Outcome != OutcomeClock || d.Clock == nil || d.Clock.Min > month || d.Clock.Max < month):
			t.Errorf("%s: test 2's expiration is %+v; want a Clock of clock + 30d", row.name, d)
		case !row.clock && (d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalNoClockRead || !slices.Contains(d.Refusal.Places, "mock-1018 "+sent)):
			t.Errorf("%s: test 2's expiration is %+v; want refused (no-clock-read) naming mock-1018 %s", row.name, d, sent)
		}
	}
}

// api-server's jobEvents insert of a suite step's answer
// (TestSlowStepFailsOnlyItself) carries that answer's Date header, an HTTP
// date to the second, read from the clock in the call's window. Where the
// minute moves to the value the seconds were recorded at (07:06:07 to
// 07:07:09), or the seconds to the value the minute was (02:42:39 to
// 02:43:42), the word diff kept that word, pairing R's seconds with F's
// minute or R's minute with F's seconds, and keploy read the date's other
// words as one the text lost and one it gained (refused, changed-shape).
// These are the ten distinct pairs one day of the api-server suite refused.
// The date is one instant in both runs, aligned whole before any word is
// diffed, read by its moment: a Clock, a plain read at the second, as where
// no word repeats (07:06:17 to 07:07:29). A text of the same words that is
// no instant still changes shape, word by word.
func TestAnHTTPDateWhoseMinuteTakesItsSecondsIsReadWhole(t *testing.T) {
	const p, httpDate = "body.docs.msgs.0.event.response.headers.Date", "Mon, 02 Jan 2006 15:04:05 GMT"
	at := func(h, m, s int) time.Time { return time.Date(2026, 11, 9, h, m, s, 500e6, time.UTC) }
	for _, row := range []struct{ rec, live time.Time }{
		{at(1, 56, 58), at(1, 58, 1)},
		{at(2, 42, 39), at(2, 43, 42)},
		{at(3, 9, 10), at(3, 10, 13)},
		{at(3, 57, 59), at(3, 59, 2)},
		{at(6, 32, 33), at(6, 33, 35)},
		{at(6, 55, 56), at(6, 56, 58)},
		{at(7, 6, 7), at(7, 7, 9)},
		{at(9, 48, 45), at(9, 49, 48)},
		{at(13, 32, 33), at(13, 33, 35)},
		{at(15, 29, 30), at(15, 30, 32)},
		{at(7, 6, 17), at(7, 7, 29)},
	} {
		rec, live := row.rec.Format(httpDate), row.live.Format(httpDate)
		d := v5Decide(t, p, String, rec, live, row.rec, row.live, 20*time.Millisecond)
		if d.Outcome != OutcomeClock || d.Clock == nil || !d.Clock.Plain() || d.Clock.Res != time.Second {
			t.Errorf("%q to %q: %+v (%+v); want a Clock, a plain read at the second", rec, live, d, d.Refusal)
		}
	}
	d := v5Decide(t, p, String, "seq 07 06 07", "seq 07 07 09", at(7, 6, 7), at(7, 7, 9), 20*time.Millisecond)
	if d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalChangedShape {
		t.Errorf("a text of no instant whose word moves: %+v; want refused (changed-shape)", d)
	}
}

// api-server's sign-in calls find their grant by expires_at > now, a read
// of the clock in milliseconds, and the call's answer holds the grant's
// created_at, written by the test's insert a few milliseconds before. A
// created_at the read is a cut of (597 of a read of 600, rounded at 10 ms;
// 679 of 680) is another moment, no echo, and the read is a Clock. One that
// is the read cut (590 of 593, at 10 ms) is no echo either where the insert
// that wrote it holds it: the answer holds that place's value. An answer
// that holds the read, or the read cut, that no other place holds is the
// read's echo, refused: its text, its second as a DATETIME, the second it
// rounds to, its day written at the millisecond, and its 10 ms cut. So is
// one a leaf of the same request also sets to the read: that leaf is the
// read, and the answer stores it.
func TestAnExpiryFilterBesideItsGrantsCreatedAtIsNoEcho(t *testing.T) {
	if reflect.ValueOf(decideBy).Pointer() == reflect.ValueOf(refuseAll).Pointer() {
		t.Skip("the order of a changed word is not on this tree")
	}
	const p, ms = "body.cmd.query.expires_at.$gt.$date.$numberLong", time.Millisecond
	const set = "body.cmd.update.$set.updated_at.$date.$numberLong"
	created := func(v string) Leaf {
		return Leaf{Path: "resp.0.0.value.created_at.$date.$numberLong", Noise: "resp.0.0.value.created_at.$date.$numberLong", Type: String, Value: v}
	}
	stored := func(v string) Leaf {
		return Leaf{Path: "resp.0.0.value.created_at", Noise: "resp.0.0.value.created_at", Type: String, Value: v}
	}
	// insert is the test's insert of the grant, 3 ms before the call, that
	// wrote created_at, recorded and in round F.
	insert := func(rec, live string, rAt time.Time) Earlier {
		const at = "body.docs.msgs.0.created_at.$date.$numberLong"
		return Earlier{Mock: "mock-1001", At: rAt.Add(-3 * ms), Payload: payloadOf("mock-1001", rAt.Add(-3*ms)), Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: at, Noise: at, Type: String, Value: rec}, Live: live}}}
	}
	// decide is the call's decision, its read read; also: its request sets
	// the read again at set; grant: the insert wrote created_at grant.
	decide := func(read int64, answer Leaf, also bool, grant string) Decision {
		rAt := time.UnixMilli(read + 2).UTC()
		fAt := rAt.Add(2*time.Hour + 17*time.Minute + 23*time.Second + 150*ms)
		leaf := func(v int64) Shape {
			s := Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: strconv.FormatInt(v, 10)}}}
			if also {
				s.Leaves[set] = Leaf{Path: set, Noise: set, Type: String, Value: strconv.FormatInt(v, 10)}
			}
			return s
		}
		var earlier []Earlier
		if grant != "" {
			earlier = append(earlier, insert(grant, strconv.FormatInt(fAt.Add(-5*ms).UnixMilli(), 10), rAt))
		}
		dc := NewDriftCache()
		dc.appClock = true
		ds := Decide(Input{
			Drifts: dc, Drift: []string{p}, Live: leaf(fAt.Add(-2 * ms).UnixMilli()), Rec: leaf(read),
			At: rAt, Arrived: fAt, Test: "test-2", Mock: "mock-1007", Mode: ModeOwn, Resp: []Leaf{answer}, Earlier: earlier,
			Window: v5Window(v5Call(rAt, 20*ms, 0), v5Call(fAt, 20*ms, 0)), Landed: Landed{Where: LandedBetween},
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
		})
		if len(ds) != 1 {
			t.Fatalf("read %d beside %q: %d decisions; want one", read, answer.Value, len(ds))
		}
		return ds[0]
	}
	for _, c := range []struct {
		read           int64
		created, grant string
	}{
		{1794207702600, "1794207702597", ""},
		{1794207702680, "1794207702679", ""},
		{1794207702598, "1794207702597", ""},
		{1794207702593, "1794207702590", "1794207702590"},
	} {
		if d := decide(c.read, created(c.created), false, c.grant); d.Outcome != OutcomeClock || d.Clock == nil {
			t.Errorf("read %d beside created_at %s (inserted as %q): %+v; want a Clock", c.read, c.created, c.grant, d.Refusal)
		}
	}
	const echo = "its own call's recorded answer holds"
	for _, c := range []struct {
		read   int64
		answer Leaf
		also   bool
		grant  string
	}{
		{1794207702600, created("1794207702600"), false, ""},
		{1794207702600, stored("2026-11-09 07:01:42"), false, ""},
		{1794207702600, stored("2026-11-09 07:01:43"), false, ""},
		{1794207702600, stored("2026-11-09T00:00:00.000Z"), false, ""},
		{1794207702593, created("1794207702590"), false, ""},
		{1794207702593, created("1794207702590"), false, "1794207702593"},
		{1794207702600, stored("2026-11-09T07:01:42.600Z"), true, ""},
	} {
		if d := decide(c.read, c.answer, c.also, c.grant); d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalCannotCheck || !strings.Contains(d.Refusal.Why, echo) {
			t.Errorf("read %d echoed as %q (set again %v, inserted as %q): %v %+v; want refused, an echo", c.read, c.answer.Value, c.also, c.grant, d.Outcome, d.Refusal)
		}
	}
}

// api-server's device-code sign-in, as recorded: the approval (mock-1134)
// finds the pending code by expires_at > now, a read of milliseconds, and
// sets auth_time, the client's sign-in time in Unix seconds, 683 ms past
// that read's second; the next poll (mock-1135) finds it the same way 1 ms
// later and sets last_polled_at to its read. Each answer holds auth_time,
// the read's second, and last_polled_at 680, an earlier poll's read and
// the read cut at 10 ms. Neither is the read's echo: auth_time is the value
// the approval's request sets (and, for the poll, the approval's), and 680
// the earlier poll's, each a value made anew each run, written as the answer
// writes it: their sources. Each read is a Clock, the answers built as
// integrations' mongo/v2 builds them (a $date typed, a $numberLong a string)
// and with the read as a string too; so is it where the earlier poll's
// call learned its 680 as a read of the clock at the millisecond, finer
// than the 10 ms the answer cuts the read to, where the test's own request
// or a recording's request sets auth_time instead. With no source, the
// same answers are the reads' stored seconds and 10 ms cuts, refused: where
// the only place that holds them is recorded after the call, is an earlier
// test's, is an answer (a find's, answered before the call, at its instant,
// or at no time keploy knows), a recording's answer, a value the test gave
// keploy, a request sent at no time keploy knows or at the call's own
// instant, an exchange or a recording round F served only after the call,
// a recording that answers with the read, or a request that sets auth_time
// the same in both runs, in another form in round F, in another form than
// the answer's, or beside the read itself; and
// where the earlier poll's call learned its 680 as a read cut at 10 ms, or
// at no cut, or the approval's call learned auth_time as a read at the
// second: two reads that cut alike, two sources.
func TestASignInTimeTheRequestSetsBesideItsReadIsNoEcho(t *testing.T) {
	if reflect.ValueOf(decideBy).Pointer() == reflect.ValueOf(refuseAll).Pointer() {
		t.Skip("the order of a changed word is not on this tree")
	}
	const ms = time.Millisecond
	const p = "body.cmd.query.expires_at.$gt.$date.$numberLong"
	const authSet, polledSet = "body.cmd.update.$set.auth_time.$numberLong", "body.cmd.update.$set.last_polled_at.$date.$numberLong"
	const auth, fAuth = "1794207702", "1794207772"
	str := func(path, v string) Leaf { return Leaf{Path: path, Noise: path, Type: String, Value: v} }
	inst := func(path string, v int64) Leaf { return InstantLeaf(path, path, time.UnixMilli(v), ms) }
	answer := func(typed bool) []Leaf {
		leaf := str
		if typed {
			leaf = func(path, v string) Leaf { n, _ := strconv.ParseInt(v, 10, 64); return inst(path, n) }
		}
		return []Leaf{
			leaf("resp.0.0.value.created_at.$date.$numberLong", "1794207702679"),
			leaf("resp.0.0.value.expires_at.$date.$numberLong", "1794208302679"),
			leaf("resp.0.0.value.last_polled_at.$date.$numberLong", "1794207702680"),
			str("resp.0.0.value.auth_time.$numberLong", auth),
		}
	}
	// poll is an exchange of the test at at that sent leaves, each its
	// path, its recorded value and round F's.
	poll := func(mock string, at time.Time, leaves ...[3]string) Earlier {
		e := Earlier{Mock: mock, At: at, Payload: payloadOf(mock, at), Own: true}
		for _, l := range leaves {
			e.Seen = append(e.Seen, Seen{Side: Request, Leaf: str(l[0], l[1]), Live: l[2]})
		}
		return e
	}
	type call struct {
		name    string
		read    int64
		also    map[string][2]string
		earlier func(rAt, fAt time.Time) []Earlier
		clock   bool
		raw     func(rAt, fAt time.Time) Raw
	}
	msOf := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	firstPoll := func(rAt, fAt time.Time) Earlier {
		return poll("mock-1141", rAt.Add(-3*ms), [3]string{"body.cmd.query.expires_at.$gt.$date.$numberLong", "1794207702680", msOf(fAt.Add(-3 * ms))},
			[3]string{polledSet, "1794207702680", msOf(fAt.Add(-3 * ms))})
	}
	approval := func(rAt, fAt time.Time) Earlier {
		return poll("mock-1134", rAt.Add(-1*ms), [3]string{p, "1794207702683", msOf(fAt.Add(-1 * ms))}, [3]string{authSet, auth, fAuth})
	}
	both := func(f ...func(rAt, fAt time.Time) Earlier) func(rAt, fAt time.Time) []Earlier {
		return func(rAt, fAt time.Time) []Earlier {
			var out []Earlier
			for _, e := range f {
				out = append(out, e(rAt, fAt))
			}
			return out
		}
	}
	later := func(rAt, fAt time.Time) Earlier {
		e := approval(rAt, fAt)
		e.Mock, e.At, e.Payload = "mock-1136", rAt.Add(5*ms), payloadOf("mock-1136", rAt.Add(5*ms))
		return e
	}
	older := func(rAt, fAt time.Time) Earlier {
		e := approval(rAt, fAt)
		e.Mock, e.Run, e.Own, e.Payload = "mock-1099", true, false, payloadOf("mock-1099", e.At)
		return e
	}
	// readBack is a find of the test sent 2 ms before the call whose answer,
	// given at answered from the call's instant, holds auth_time.
	readBack := func(answered time.Duration) func(rAt, fAt time.Time) Earlier {
		return func(rAt, fAt time.Time) Earlier {
			at := rAt.Add(-2 * ms)
			m := &models.Mock{Name: "mock-1140", Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: rAt.Add(answered)}}
			return Earlier{Mock: m.Name, At: at, Payload: RawOf(m, true), Own: true,
				Seen: []Seen{{Side: Response, Leaf: str("resp.0.0.value.auth_time.$numberLong", auth), Live: fAuth}}}
		}
	}
	// noAnswerAt: that find, its answer at no time keploy knows.
	noAnswerTime := func(rAt, fAt time.Time) Earlier {
		e := readBack(-1*ms)(rAt, fAt)
		m := &models.Mock{Name: e.Mock, Spec: models.MockSpec{ReqTimestampMock: e.At}}
		e.Payload = RawOf(m, true)
		return e
	}
	// sets is the approval setting auth_time rec, live in round F, its call
	// having learned it as o (nil: as no read of the clock).
	sets := func(rec, live string, o *models.ClockOffset) func(rAt, fAt time.Time) Earlier {
		return func(rAt, fAt time.Time) Earlier {
			e := approval(rAt, fAt)
			e.Seen[1].Leaf.Value, e.Seen[1].Live, e.Seen[1].Offset = rec, live, o
			e.Seen[1].Clock = o != nil && o.Plain()
			return e
		}
	}
	// servedLate is the approval round F served only after the call.
	servedLate := func(rAt, fAt time.Time) Earlier {
		e := approval(rAt, fAt)
		e.Payload.servedAfter = true
		return e
	}
	untimed := func(rAt, fAt time.Time) Earlier {
		e := approval(rAt, fAt)
		e.At, e.Payload = time.Time{}, payloadOf(e.Mock, time.Time{})
		return e
	}
	atCall := func(rAt, fAt time.Time) Earlier {
		e := approval(rAt, fAt)
		e.At, e.Payload = rAt, payloadOf(e.Mock, rAt)
		return e
	}
	// learnedPoll is the earlier poll, its call having learned its leaves
	// as plain reads of the clock at res.
	learnedPoll := func(res time.Duration) func(rAt, fAt time.Time) Earlier {
		return func(rAt, fAt time.Time) Earlier {
			e := firstPoll(rAt, fAt)
			for i := range e.Seen {
				e.Seen[i].Clock, e.Seen[i].Offset = true, &models.ClockOffset{Res: res}
			}
			return e
		}
	}
	// given is the test's own request holding auth_time (gave: a value the
	// test gave keploy instead).
	given := func(gave bool) func(rAt, fAt time.Time) Earlier {
		return func(rAt, fAt time.Time) Earlier {
			return Earlier{Mock: "test-2", Given: true, Gave: gave, Seen: []Seen{{Side: Request, Leaf: str("body.auth_time", auth), Live: fAuth}}}
		}
	}
	// recording is the approval as a recording F served, sent at sent from
	// the call's instant, that sets auth_time in its request (else holds it
	// in its answer alone); answering, one that sets it and answers at, in
	// each run.
	answering := func(request bool, sent time.Duration, at [2]string) func(rAt, fAt time.Time) Raw {
		return func(rAt, fAt time.Time) Raw {
			mock := func(a, answer string, at time.Time) *models.Mock {
				req, resp := `{"auth_time":"`+a+`"}`, `{"ok":1}`
				if answer != "" {
					resp = `{"ok":1,"at":"` + answer + `"}`
				}
				if !request {
					req, resp = resp, req
				}
				return &models.Mock{Name: "mock-1134", Kind: models.HTTP, Spec: models.MockSpec{
					HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://auth.local/approve", Header: map[string]string{"Content-Type": "application/json"}, Body: req},
					HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: resp},
					ReqTimestampMock: at, ResTimestampMock: at.Add(ms / 2),
				}}
			}
			return RawOf(mock(auth, at[0], rAt.Add(sent)), true).ServedAs(1).ServedLive(RawOf(mock(fAuth, at[1], fAt.Add(sent)), true))
		}
	}
	recording := func(request bool, sent time.Duration) func(rAt, fAt time.Time) Raw {
		return answering(request, sent, [2]string{})
	}
	recordingServedLate := func(rAt, fAt time.Time) Raw {
		r := recording(true, -1*ms)(rAt, fAt)
		r.servedAfter = true
		return r
	}
	for _, c := range []call{
		{"mock-1134, the approval", 1794207702683, map[string][2]string{authSet: {auth, fAuth}}, both(firstPoll), true, nil},
		{"mock-1135, the next poll", 1794207702684, map[string][2]string{polledSet: {"1794207702684", ""}}, both(firstPoll, approval), true, nil},
		{"mock-1134 with no request that sets auth_time", 1794207702683, nil, both(firstPoll), false, nil},
		{"mock-1134 with no earlier poll", 1794207702683, map[string][2]string{authSet: {auth, fAuth}}, nil, false, nil},
		{"mock-1135 with no earlier call", 1794207702684, map[string][2]string{polledSet: {"1794207702684", ""}}, nil, false, nil},
		{"mock-1135, the approval recorded after it", 1794207702684, nil, both(firstPoll, later), false, nil},
		{"mock-1135, the approval an earlier test's", 1794207702684, nil, both(firstPoll, older), false, nil},
		{"mock-1135, a find answered before it that holds auth_time", 1794207702684, nil, both(firstPoll, readBack(-1*ms)), false, nil},
		{"mock-1135, a find answered at its instant that holds auth_time", 1794207702684, nil, both(firstPoll, readBack(0)), false, nil},
		{"mock-1135, a find answered at no time keploy knows that holds auth_time", 1794207702684, nil, both(firstPoll, noAnswerTime), false, nil},
		{"mock-1135, a find sent before it, answered after it, that holds auth_time", 1794207702684, nil, both(firstPoll, readBack(5*ms)), false, nil},
		{"mock-1135, the approval sent at no time keploy knows", 1794207702684, nil, both(firstPoll, untimed), false, nil},
		{"mock-1135, the approval round F served only after it", 1794207702684, nil, both(firstPoll, servedLate), false, nil},
		{"mock-1135, the approval sent at its own instant", 1794207702684, nil, both(firstPoll, atCall), false, nil},
		{"mock-1135, the approval setting auth_time the same in round F", 1794207702684, nil, both(firstPoll, sets(auth, auth, nil)), false, nil},
		{"mock-1135, the approval setting auth_time in another form in round F", 1794207702684, nil, both(firstPoll, sets(auth, "2026-11-09T07:02:52Z", nil)), false, nil},
		{"mock-1135, the approval setting auth_time in another form than the answer", 1794207702684, nil, both(firstPoll, sets("2026-11-09T07:01:42Z", "2026-11-09T07:02:52Z", nil)), false, nil},
		{"mock-1135, the approval setting auth_time beside the read itself", 1794207702684, nil, both(firstPoll, sets(auth+" 2026-11-09T07:01:42.684Z", fAuth+" 2026-11-09T07:02:50.000Z", nil)), false, nil},
		{"mock-1135, the approval's call learning auth_time as a read at the second", 1794207702684, nil, both(firstPoll, sets(auth, fAuth, &models.ClockOffset{Res: time.Second})), false, nil},
		{"mock-1135, the earlier poll's call learning 680 as a read at the millisecond", 1794207702684, nil, both(learnedPoll(ms), approval), true, nil},
		{"mock-1135, the earlier poll's call learning 680 as a read cut at 10 ms", 1794207702684, nil, both(learnedPoll(10*ms), approval), false, nil},
		{"mock-1135, the earlier poll's call learning 680 as a read at no cut", 1794207702684, nil, both(learnedPoll(0), approval), false, nil},
		{"mock-1135, the test's own request setting auth_time", 1794207702684, nil, both(firstPoll, given(false)), true, nil},
		{"mock-1135, a value the test gave keploy holding auth_time", 1794207702684, nil, both(firstPoll, given(true)), false, nil},
		{name: "mock-1135, a recording's request setting auth_time", read: 1794207702684, earlier: both(firstPoll), clock: true, raw: recording(true, -1*ms)},
		{name: "mock-1135, a recording's answer holding auth_time", read: 1794207702684, earlier: both(firstPoll), clock: false, raw: recording(false, -1*ms)},
		{name: "mock-1135, a recording's request setting auth_time, recorded at its instant", read: 1794207702684, earlier: both(firstPoll), clock: false, raw: recording(true, 0)},
		{name: "mock-1135, a recording's request setting auth_time, served in round F only after it", read: 1794207702684, earlier: both(firstPoll), clock: false, raw: recordingServedLate},
		{name: "mock-1135, a recording's request setting auth_time, its answer the read", read: 1794207702684, earlier: both(firstPoll), clock: false, raw: answering(true, -1*ms, [2]string{"1794207702684", "1794207772000"})},
	} {
		for _, typed := range []bool{true, false} {
			rAt := time.UnixMilli(c.read + 1).UTC()
			fAt := rAt.Add(70*time.Second + 150*ms)
			shape := func(read string, live bool) Shape {
				s := Shape{Leaves: map[string]Leaf{p: str(p, read)}}
				if typed {
					n, _ := strconv.ParseInt(read, 10, 64)
					s.Leaves[p] = inst(p, n)
				}
				for k, v := range c.also {
					// A leaf of no live value of its own sets the read.
					w := v[0]
					if live {
						w = v[1]
					}
					if w == "" {
						w = read
					}
					s.Leaves[k] = str(k, w)
				}
				return s
			}
			var earlier []Earlier
			if c.earlier != nil {
				earlier = c.earlier(rAt, fAt)
			}
			var raw []Raw
			if c.raw != nil {
				raw = []Raw{c.raw(rAt, fAt)}
			}
			dc := NewDriftCache()
			dc.appClock = true
			ds := Decide(Input{
				Drifts: dc, Drift: []string{p}, Live: shape(msOf(fAt.Add(-1*ms)), true), Rec: shape(strconv.FormatInt(c.read, 10), false),
				At: rAt, Arrived: fAt, Test: "test-2", Mock: "mock-1135", Mode: ModeOwn, Resp: answer(typed), Earlier: earlier, Raw: raw,
				Window: v5Window(v5Call(rAt, 20*ms, 0), v5Call(fAt, 20*ms, 0)), Landed: Landed{Where: LandedBetween},
				Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
			})
			if len(ds) != 1 {
				t.Fatalf("%s (typed %v): %d decisions; want one", c.name, typed, len(ds))
			}
			d := ds[0]
			switch {
			case c.clock && (d.Outcome != OutcomeClock || d.Clock == nil):
				t.Errorf("%s (typed %v): %+v; want a Clock", c.name, typed, d.Refusal)
			case !c.clock && (d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalCannotCheck || !strings.Contains(d.Refusal.Why, "its own call's recorded answer holds")):
				t.Errorf("%s (typed %v): %v %+v; want refused, an echo", c.name, typed, d.Outcome, d.Refusal)
			}
		}
	}
}

// A read of milliseconds (07:01:42.600) whose own call's answer holds its
// second, day or 10 ms is that read's stored form, refused, unless a source
// holds that instant (heldElsewhere): a place the application sent before
// the call, written as the answer writes it, made anew each run, and not
// another read of the clock its call learned at a cut no finer than the
// answer's. An answer of an earlier exchange (its HTTP Date), an earlier
// test's place, a Unix second written otherwise than the answer's
// DATETIME, a date or midnight the same in both runs or written otherwise in
// round F, and another read learned at the second are no source. One that
// is, a DATETIME second an earlier insert wrote and wrote anew in F, makes
// the read a Clock. An HTTP answer's Date header is the server's clock,
// never an echo of the read, whatever holds it.
func TestAClockReadsEchoIsExplainedOnlyByASourceOfTheAnswersInstant(t *testing.T) {
	if reflect.ValueOf(decideBy).Pointer() == reflect.ValueOf(refuseAll).Pointer() {
		t.Skip("the order of a changed word is not on this tree")
	}
	const p, ms = "body.cmd.query.expires_at.$gt.$date.$numberLong", time.Millisecond
	const read = int64(1794207702600) // 2026-11-09T07:01:42.600Z
	str := func(path, v string) Leaf { return Leaf{Path: path, Noise: path, Type: String, Value: v} }
	// answered is an exchange of the test sent 40 ms before the call, its
	// answer leaf path=v given 5 ms before the call.
	answered := func(path, v string) func(rAt time.Time) Earlier {
		return func(rAt time.Time) Earlier {
			at := rAt.Add(-40 * ms)
			m := &models.Mock{Name: "mock-990", Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: rAt.Add(-5 * ms)}}
			return Earlier{Mock: m.Name, At: at, Payload: RawOf(m, true), Own: true,
				Seen: []Seen{{Side: Response, Leaf: str(path, v), Live: "Mon, 09 Nov 2026 09:19:05 GMT"}}}
		}
	}
	// sent is a request of the test (another test's: other) sent 40 ms
	// before the call, that sent path=v, live in round F, its call having
	// learned it as o.
	sent := func(path, v, live string, other bool, o *models.ClockOffset) func(rAt time.Time) Earlier {
		return func(rAt time.Time) Earlier {
			at := rAt.Add(-40 * ms)
			return Earlier{Mock: "mock-991", At: at, Payload: payloadOf("mock-991", at), Own: !other, Run: other,
				Seen: []Seen{{Side: Request, Leaf: str(path, v), Live: live, Clock: o != nil && o.Plain(), Offset: o}}}
		}
	}
	typ := String // the type of the answer's leaf
	decide := func(apath, aval string, also map[string][2]string, earlier ...func(time.Time) Earlier) Decision {
		rAt := time.UnixMilli(read + 2).UTC()
		fAt := rAt.Add(2*time.Hour + 17*time.Minute + 23*time.Second + 150*ms)
		leaf := func(v int64, live bool) Shape {
			s := Shape{Leaves: map[string]Leaf{p: str(p, strconv.FormatInt(v, 10))}}
			for k, w := range also {
				x := w[0]
				if live {
					x = w[1]
				}
				if x == "" { // a leaf of no value of its own sets the read
					x = strconv.FormatInt(v, 10)
				}
				s.Leaves[k] = str(k, x)
			}
			return s
		}
		var es []Earlier
		for _, e := range earlier {
			es = append(es, e(rAt))
		}
		dc := NewDriftCache()
		dc.appClock = true
		ds := Decide(Input{
			Drifts: dc, Drift: []string{p}, Live: leaf(fAt.Add(-2*ms).UnixMilli(), true), Rec: leaf(read, false),
			At: rAt, Arrived: fAt, Test: "test-2", Mock: "mock-1007", Mode: ModeOwn, Resp: []Leaf{{Path: apath, Noise: apath, Type: typ, Value: aval}}, Earlier: es,
			Window: v5Window(v5Call(rAt, 20*ms, 0), v5Call(fAt, 20*ms, 0)), Landed: Landed{Where: LandedBetween},
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
		})
		if len(ds) != 1 {
			t.Fatalf("%d decisions; want one", len(ds))
		}
		return ds[0]
	}
	const upd, day, hdr = "resp.0.0.value.updated_at", "resp.0.0.value.day", "header.Date"
	const seen, link, token = "resp.0.0.value.seen", "resp.0.0.value.link", "resp.0.0.value.token"
	const second, fSecond = "2026-11-09 07:01:42", "2026-11-09 09:19:05"
	const isoSecond, fIsoSecond, httpSecond = "2026-11-09T07:01:42Z", "2026-11-09T09:19:05Z", "Mon, 09 Nov 2026 07:01:42 GMT"
	const encSecond = "at=2026-11-09T07%3A01%3A42Z"
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	const unixS, fUnixS = "1794207702", "1794215945"
	const filter = "body.cmd.pipeline.0.$match.expiration.$gt.$numberLong"
	setsRead := map[string][2]string{"body.cmd.update.$set.updated_at.$date.$numberLong": {"", ""}}
	for _, c := range []struct {
		name, path, val string
		also            map[string][2]string
		earlier         []func(time.Time) Earlier
		clock           bool
	}{
		{"a DATETIME second, held nowhere else", upd, second, nil, nil, false},
		{"a DATETIME second, an earlier answer's Date of it", upd, second, nil, []func(time.Time) Earlier{answered("header.Date", "Mon, 09 Nov 2026 07:01:42 GMT")}, false},
		{"a DATETIME second, an earlier answer of it", upd, second, nil, []func(time.Time) Earlier{answered("resp.0.0.value.updated_at", second)}, false},
		{"a DATETIME second, a request's Unix second of it", upd, second, nil, []func(time.Time) Earlier{sent(filter, unixS, fUnixS, false, nil)}, false},
		{"a DATETIME second, an earlier insert of it", upd, second, nil, []func(time.Time) Earlier{sent("body.docs.0.updated_at", second, fSecond, false, nil)}, true},
		{"a DATETIME second, an earlier test's insert of it", upd, second, nil, []func(time.Time) Earlier{sent("body.docs.0.updated_at", second, fSecond, true, nil)}, false},
		{"a DATETIME second, an insert of it the same in round F", upd, second, nil, []func(time.Time) Earlier{sent("body.docs.0.updated_at", second, second, false, nil)}, false},
		{"a DATETIME second, an insert of it its call learned as a read at the second", upd, second, nil, []func(time.Time) Earlier{sent("body.docs.0.updated_at", second, fSecond, false, &models.ClockOffset{Res: time.Second})}, false},
		{"a DATETIME second, an insert of it its call learned as a read at the millisecond", upd, second, nil, []func(time.Time) Earlier{sent("body.docs.0.updated_at", second, fSecond, false, &models.ClockOffset{Res: ms})}, true},
		{"a DATETIME second, a leaf of the request beside it", upd, second, map[string][2]string{"body.cmd.update.$set.seen": {second, fSecond}}, nil, true},
		{"a DATETIME second, the request setting the read", upd, second, setsRead, nil, false},
		{"a DATETIME second, the request setting the read, an earlier answer's Date of it", upd, second, setsRead, []func(time.Time) Earlier{answered("header.Date", "Mon, 09 Nov 2026 07:01:42 GMT")}, false},
		{"the read's date, a request's date the same in round F", day, "2026-11-09", nil, []func(time.Time) Earlier{sent("body.cmd.query.day", "2026-11-09", "2026-11-09", false, nil)}, false},
		{"the read's date, a request's date written otherwise in round F", day, "2026-11-09", nil, []func(time.Time) Earlier{sent("body.cmd.query.day", "2026-11-09", fUnixS, false, nil)}, false},
		{"the read's midnight, a request's midnight the same in round F", day, "2026-11-09T00:00:00.000Z", nil, []func(time.Time) Earlier{sent("body.cmd.query.from", "2026-11-09T00:00:00.000Z", "2026-11-09T00:00:00.000Z", false, nil)}, false},
		{"the read's midnight, a request's midnight written otherwise in round F", day, "2026-11-09T00:00:00.000Z", nil, []func(time.Time) Earlier{sent("body.cmd.query.from", "2026-11-09T00:00:00.000Z", fUnixS, false, nil)}, false},
		{"the read itself, a request of it", upd, "2026-11-09T07:01:42.600Z", nil, []func(time.Time) Earlier{sent("body.cmd.query.at", "2026-11-09T07:01:42.600Z", "2026-11-09T09:19:05.750Z", false, nil)}, false},
		{"an HTTP answer's Date of the read's second", hdr, "Mon, 09 Nov 2026 07:01:42 GMT", nil, nil, true},
		{"an HTTP answer's date header of the read's second", "header.date", "Mon, 09 Nov 2026 07:01:42 GMT", nil, nil, true},
		{"an HTTP answer's Date of the read's second, an earlier Date of it", hdr, "Mon, 09 Nov 2026 07:01:42 GMT", nil, []func(time.Time) Earlier{answered("header.Date", "Mon, 09 Nov 2026 07:01:42 GMT")}, true},
		{"an HTTP answer's Date of the read's second, the request holding it", hdr, "Mon, 09 Nov 2026 07:01:42 GMT", map[string][2]string{"header.X-Sent": {unixS, fUnixS}}, nil, true},
		{"an HTTP answer's Last-Modified of the read's second", "header.Last-Modified", "Mon, 09 Nov 2026 07:01:42 GMT", nil, nil, false},
		{"a stored answer's Date of the read's second, held nowhere else", "resp.0.0.value.response.header.Date", httpSecond, nil, nil, false},
		{"a Date the answer's body holds of the read's second, held nowhere else", "body.header.Date", httpSecond, nil, nil, false},
		// A source is read in the form the answer's instant was: as written,
		// or decoded and case-folded where the answer's was.
		{"an ISO second, an earlier insert of it", upd, isoSecond, nil, []func(time.Time) Earlier{sent("body.docs.0.updated_at", isoSecond, fIsoSecond, false, nil)}, true},
		{"an HTTP date of the second, an earlier insert of it", seen, httpSecond, nil, []func(time.Time) Earlier{sent("body.docs.0.seen", httpSecond, "Mon, 09 Nov 2026 09:19:05 GMT", false, nil)}, true},
		{"a percent-encoded ISO second, an earlier insert of it plain", link, "at=2026-11-09T07%3A01%3A42Z", nil, []func(time.Time) Earlier{sent("body.docs.0.updated_at", isoSecond, fIsoSecond, false, nil)}, true},
		{"a percent-encoded ISO second, an earlier request of it encoded", link, "at=2026-11-09T07%3A01%3A42Z", nil, []func(time.Time) Earlier{sent("body.cmd.query.link", "at=2026-11-09T07%3A01%3A42Z", "at=2026-11-09T09%3A19%3A05Z", false, nil)}, true},
		{"a percent-encoded DATETIME second, an earlier insert of it plain", link, "2026-11-09%2007%3A01%3A42", nil, []func(time.Time) Earlier{sent("body.docs.0.updated_at", second, fSecond, false, nil)}, true},
		{"a DATETIME second in a base64 envelope, held nowhere else", token, b64(second), nil, nil, false},
		{"a DATETIME second in a base64 envelope, an earlier request of the envelope", token, b64(second), nil, []func(time.Time) Earlier{sent("body.cmd.query.token", b64(second), b64(fSecond), false, nil)}, true},
		{"a DATETIME second in a base64 envelope, an earlier request of the envelope the same in round F", token, b64(second), nil, []func(time.Time) Earlier{sent("body.cmd.query.token", b64(second), b64(second), false, nil)}, false},
		{"an ISO second, an earlier insert of it as a DATETIME", upd, isoSecond, nil, []func(time.Time) Earlier{sent("body.docs.0.updated_at", second, fSecond, false, nil)}, false},
		{"an ISO second, an earlier request of it percent-encoded alone", upd, isoSecond, nil, []func(time.Time) Earlier{sent("body.cmd.query.link", "at=2026-11-09T07%3A01%3A42Z", "at=2026-11-09T09%3A19%3A05Z", false, nil)}, false},
		{"an ISO second, an insert of the second as a DATETIME beside its ISO text encoded", upd, isoSecond, nil, []func(time.Time) Earlier{sent("body.docs.0.at", second+" 2026-11-09T07%3A01%3A42Z", fSecond+" 2026-11-09T09%3A19%3A05Z", false, nil)}, false},
		{"a lower-case ISO second beside an escape, an earlier insert of it", link, "2026-11-09t07:01:42z p%20q", nil, []func(time.Time) Earlier{sent("body.docs.0.at", "2026-11-09t07:01:42z", "2026-11-09t09:19:05z", false, nil)}, true},
		{"a DATETIME second beside an escape, an insert of it beside the read in base64", link, second + " p%20q", nil, []func(time.Time) Earlier{sent("body.docs.0.at", second+" "+b64("2026-11-09 07:01:42.600"), fSecond+" "+b64("2026-11-09 09:19:05.750"), false, nil)}, false},
		{"a percent-encoded ISO second, an earlier insert of it as a DATETIME", link, encSecond, nil, []func(time.Time) Earlier{sent("body.docs.0.updated_at", second, fSecond, false, nil)}, false},
		// A source is made anew in one form: its recorded instant pairs with
		// one of round F read the same way, as written or in the same form.
		{"an ISO second, an insert of it plain in the recording and in base64 in round F", upd, isoSecond, nil, []func(time.Time) Earlier{sent("body.docs.0.at", isoSecond, b64(fIsoSecond), false, nil)}, false},
		{"an ISO second, an insert of it plain beside its base64, round F's in base64 alone", upd, isoSecond, nil, []func(time.Time) Earlier{sent("body.docs.0.at", isoSecond+" "+b64(isoSecond), "x "+b64(fIsoSecond), false, nil)}, false},
		{"a percent-encoded ISO second, an insert of it plain in the recording and in base64 in round F", link, encSecond, nil, []func(time.Time) Earlier{sent("body.docs.0.at", isoSecond, b64(fIsoSecond), false, nil)}, false},
		{"a percent-encoded ISO second, an insert of it plain in the recording, a DATETIME in base64 beside other text in round F", link, encSecond, nil, []func(time.Time) Earlier{sent("body.docs.0.at", isoSecond, "x "+b64(fSecond), false, nil)}, false},
		{"a percent-encoded ISO second, a request of it encoded in the recording and plain in round F", link, encSecond, nil, []func(time.Time) Earlier{sent("body.cmd.query.link", encSecond, "at="+fIsoSecond, false, nil)}, false},
		{"a percent-encoded ISO second, an insert of it plain beside another time in base64, round F's in base64 alone", link, encSecond, nil, []func(time.Time) Earlier{sent("body.docs.0.at", isoSecond+" "+b64("2026-11-08T01:02:03Z"), "x "+b64(fIsoSecond), false, nil)}, false},
		// A place that holds the read in a declared form holds the read,
		// however the answer's instant was read.
		{"a DATETIME second, an insert of it beside the read in base64", upd, second, nil, []func(time.Time) Earlier{sent("body.docs.0.at", second+" "+b64("2026-11-09 07:01:42.600"), fSecond+" "+b64("2026-11-09 09:19:05.750"), false, nil)}, false},
	} {
		d := decide(c.path, c.val, c.also, c.earlier...)
		switch {
		case c.clock && (d.Outcome != OutcomeClock || d.Clock == nil):
			t.Errorf("%s: %v %+v; want a Clock", c.name, d.Outcome, d.Refusal)
		case !c.clock && (d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalCannotCheck || !strings.Contains(d.Refusal.Why, "its own call's recorded answer holds")):
			t.Errorf("%s: %v %+v; want refused, an echo", c.name, d.Outcome, d.Refusal)
		}
	}
	// A typed instant of the answer (a BSON date) is read as written, and so
	// is its source: an insert that wrote the second plain is one, a request
	// that holds it percent-encoded alone, in a declared form, is none.
	typ = Instant
	if d := decide(upd, isoSecond, nil, sent("body.docs.0.updated_at", isoSecond, fIsoSecond, false, nil)); d.Outcome != OutcomeClock || d.Clock == nil {
		t.Errorf("a typed second, an earlier insert of it: %v %+v; want a Clock", d.Outcome, d.Refusal)
	}
	if d := decide(upd, isoSecond, nil, sent("body.cmd.query.link", "at=2026-11-09T07%3A01%3A42Z", "at=2026-11-09T09%3A19%3A05Z", false, nil)); d.Outcome != OutcomeRefused || d.Refusal == nil || !strings.Contains(d.Refusal.Why, "its own call's recorded answer holds") {
		t.Errorf("a typed second, an earlier request of it percent-encoded alone: %v %+v; want refused, an echo", d.Outcome, d.Refusal)
	}
}

// An answer of n instants, each the read's second as Unix seconds, that the
// last of n earlier inserts wrote, asks once whether a source holds that
// second (heldElsewhere), however large n is: what the call does for it is
// linear in its answers and places. An answer that writes the second in a
// second form asks once more, and where no insert writes that form, the
// read is refused at its first such answer.
func TestAnAnswerOfManyEchoesOfOneSecondAsksForItsSourceOnce(t *testing.T) {
	if reflect.ValueOf(decideBy).Pointer() == reflect.ValueOf(refuseAll).Pointer() {
		t.Skip("the order of a changed word is not on this tree")
	}
	const p, ms, n = "body.cmd.query.expires_at.$gt.$date.$numberLong", time.Millisecond, 3000
	const read = int64(1794207702600)
	str := func(path, v string) Leaf { return Leaf{Path: path, Noise: path, Type: String, Value: v} }
	rAt := time.UnixMilli(read + 2).UTC()
	fAt := rAt.Add(2*time.Hour + 17*time.Minute + 23*time.Second + 150*ms)
	var es []Earlier
	for i := range n {
		v := strconv.Itoa(1794207000 + i%600)
		if i == n-1 {
			v = "1794207702"
		}
		at := rAt.Add(-time.Duration(n-i) * ms)
		mock := "mock-" + strconv.Itoa(1000+i)
		es = append(es, Earlier{Mock: mock, At: at, Payload: payloadOf(mock, at), Own: true,
			Seen: []Seen{{Side: Request, Leaf: str("body.docs.msgs."+strconv.Itoa(i)+".created_at", v), Live: strconv.Itoa(1794215000 + i)}}})
	}
	for _, c := range []struct {
		what  string
		forms []string
		asks  int
		clock bool
	}{
		{"one form", []string{"1794207702"}, 1, true},
		{"two forms", []string{"1794207702", "2026-11-09 07:01:42"}, 2, false},
	} {
		var resp []Leaf
		for i := range n {
			resp = append(resp, str("resp.0.0.cursor.firstBatch."+strconv.Itoa(i)+".created_at", c.forms[i%len(c.forms)]))
		}
		in := Input{
			Drifts: NewDriftCache(), Drift: []string{p}, Live: oShape(p, strconv.FormatInt(fAt.Add(-2*ms).UnixMilli(), 10)), Rec: oShape(p, strconv.FormatInt(read, 10)),
			At: rAt, Arrived: fAt, Test: "test-2", Mock: "mock-9999", Mode: ModeOwn, Resp: resp, Earlier: es,
			Window: v5Window(v5Call(rAt, 20*ms, 0), v5Call(fAt, 20*ms, 0)), Landed: Landed{Where: LandedBetween},
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
		}
		in.Drifts.appClock = true
		x := newReaderIndex(in.Drifts)
		dc := newDecider(in, false, x)
		ds, _ := dc.decide()
		x.release()
		if len(ds) != 1 || (ds[0].Outcome == OutcomeClock) != c.clock {
			t.Errorf("%s: %s; want one decision, a Clock %v", c.what, oDescribe(ds), c.clock)
		}
		if dc.asked != c.asks {
			t.Errorf("%s: %d answers asked for their source %d times; want %d", c.what, n, dc.asked, c.asks)
		}
	}
}

// A client test's name made of its UnixNano (a CI identity
// "…repo-<UnixNano>" a Vault PUT sends) is the test's own value beside the
// answer's HTTP Date in the name's second, truncated or rounded up: the
// server's clock is no echo of it.
func TestAClientMadeNameBesideTheAnswersDateIsTheTestsOwnValue(t *testing.T) {
	const p = "body.data.name"
	recT := time.Date(2026, 11, 9, 3, 28, 6, 953971088, time.UTC)
	liveT := recT.Add(61*time.Second + 310*time.Millisecond)
	rAt, fAt := recT.Add(27*time.Millisecond), liveT.Add(27*time.Millisecond)
	recN, liveN := strconv.FormatInt(recT.UnixNano(), 10), strconv.FormatInt(liveT.UnixNano(), 10)
	str := func(path, v string) Leaf { return Leaf{Path: path, Noise: path, Type: String, Value: v} }
	created := map[string][2]string{"body.data.createdAt": {"1794194886", "1794194947"}, "body.data.updatedAt": {"2026-11-09T03:28:06Z", "2026-11-09T03:29:07Z"}}
	for _, c := range []struct {
		name string
		resp []Leaf
		also map[string][2]string
	}{
		{"the Date the name's second rounded up", []Leaf{str("header.Date", "Mon, 09 Nov 2026 03:28:07 GMT")}, nil},
		{"the Date the name's second truncated", []Leaf{str("header.Date", "Mon, 09 Nov 2026 03:28:06 GMT")}, nil},
		{"the Date rounded up beside the request's times", []Leaf{str("header.Date", "Mon, 09 Nov 2026 03:28:07 GMT"), str("body.data.created_time", "2026-11-09T03:28:07.016453Z")}, created},
	} {
		in := iCall(p, String, "CI identity: github octo-org/repo-"+recN, "CI identity: github octo-org/repo-"+liveN, rAt, fAt, nil, nil, nil)
		in.Drifts, in.Resp = oClientDrifts(), c.resp
		for k, v := range c.also {
			in.Rec.Leaves[k], in.Live.Leaves[k] = str(k, v[0]), str(k, v[1])
		}
		if d := oWord(t, Decide(in), recN, liveN); d.Outcome != OutcomeFresh {
			t.Errorf("%s: %s %+v; want the test's own value", c.name, oOutcomeName(d), d.Refusal)
		}
	}
}

// An answer echoes a clock read where it holds the read, or the read cut to
// a unit the answer writes, as the read is written or in the application's
// zone: a wall clock of no zone read in India (12:31:42.600) is the instant
// 07:01:42.600Z, which a database answers in UTC. Where keploy knows no
// zone, that answer is another moment, and so is a created_at 3 ms before
// the read. A date is the read's own day cut (the start of 9 November in
// New York, 9 November), never a date the read is a cut of: 10 November,
// read as midnight UTC, falls on 9 November in New York, so that read is
// 10 November cut to its day there, and no echo of it. A percent-encoded
// time is read as it opens: its date alone, as written, is no echo of the
// read's day where the time it opens to is another, and the time it opens
// to echoes the read's second.
func TestAnEchoOfAClockReadIsTheReadCutInItsOwnZoneOrTheApplications(t *testing.T) {
	ny := time.FixedZone("EST", -5*3600)
	for _, c := range []struct {
		name, read, answer string
		zone               *time.Location
		want               bool
	}{
		{"a wall clock read in India, answered in UTC", "2026-11-09 12:31:42.600", "2026-11-09T07:01:42.600Z", ist, true},
		{"the same, keploy knowing no zone", "2026-11-09 12:31:42.600", "2026-11-09T07:01:42.600Z", nil, false},
		{"a created_at 3 ms before the read", "2026-11-09 12:31:42.600", "2026-11-09T07:01:42.597Z", ist, false},
		{"the start of a day in New York, its date", "2026-11-09T00:00:00-05:00", "2026-11-09", ny, true},
		{"the start of a day in New York, the next date", "2026-11-09T00:00:00-05:00", "2026-11-10", ny, false},
		{"a percent-encoded time of the read's day, opened another time", "2026-11-09T10:30:00.123Z", "2026-11-09T11%3A45%3A00Z", time.UTC, false},
		{"a percent-encoded time, opened the read's second", "2026-11-09T10:30:00.123Z", "2026-11-09T10%3A30%3A00Z", time.UTC, true},
		{"the read's day written at the millisecond", "2026-11-09T10:30:00.123Z", "2026-11-09T00:00:00.000Z", time.UTC, true},
		{"the read's second in Unix seconds", "2026-11-09T07:01:42.683Z", "1794207702", time.UTC, true},
		{"the read's 10 ms in Unix milliseconds", "2026-11-09T07:01:42.683Z", "1794207702680", time.UTC, true},
		{"a millisecond the read rounds to at 10 ms", "2026-11-09T07:01:42.680Z", "1794207702679", time.UTC, false},
	} {
		dc := NewDriftCache()
		dc.zone = c.zone
		d := &decider{dr: dc, x: newReaderIndex(dc)}
		if got := d.echoesMoment(d.x.text(c.answer), momentOf(t, c.read), func(moment, time.Duration, string, bool) bool { return false }); got != c.want {
			t.Errorf("%s: %q beside the read %q echoes it %v; want %v", c.name, c.answer, c.read, got, c.want)
		}
	}
	// A source is asked about an echo at the finest unit either reading of
	// a wall clock read cuts to it: a read in its second's last 400 ms is,
	// in UTC, the next second rounded, and in India the next day rounded; a
	// read at midnight is, in UTC, the answer itself. A source a read at
	// the second could be is then two reads that cut alike (clockCut).
	for _, c := range []struct {
		name, read, answer string
		unit               time.Duration
	}{
		{"the next second in UTC, the next day in India", "2026-11-09 23:59:59.600", "2026-11-10T00:00:00Z", time.Second},
		{"the read in UTC, the next day in India", "2026-11-10 00:00:00.000", "2026-11-10T00:00:00Z", 0},
	} {
		dc := NewDriftCache()
		dc.zone = ist
		d := &decider{dr: dc, x: newReaderIndex(dc)}
		var asked []time.Duration
		got := d.echoesMoment(d.x.text(c.answer), momentOf(t, c.read), func(_ moment, unit time.Duration, _ string, _ bool) bool {
			asked = append(asked, unit)
			return false
		})
		if !got || !slices.Equal(asked, []time.Duration{c.unit}) {
			t.Errorf("%s: %q beside the read %q echoes it %v, a source asked at %v; want an echo, asked at %v", c.name, c.answer, c.read, got, asked, c.unit)
		}
	}
	// By its text, an instant of several words is held where the answer
	// holds its text, even glued to a word no instant begins inside.
	x := newReaderIndex(NewDriftCache())
	for _, c := range []struct {
		answer string
		want   bool
	}{
		{"x2026-11-09 07:01:42", true},
		{"x2026-11-09 07:01:43", false},
	} {
		if got := textHoldsInstant(x.text(c.answer), "2026-11-09 07:01:42", false); got != c.want {
			t.Errorf("%q holds the read 2026-11-09 07:01:42 by its text: %v; want %v", c.answer, got, c.want)
		}
	}
	// An instant of one word is held only as a word: inside a longer one
	// it is another value's digits.
	if textHoldsInstant(x.text("x1794207702683"), "1794207702683", false) {
		t.Errorf("x1794207702683 holds the read 1794207702683 by its text; want a word of one word held only as a word")
	}
	// An answer that holds the read's text where no instant of it is read
	// (glued to a word an instant would end inside) echoes it by that text
	// alone: replay would serve the recorded text.
	read := momentOf(t, "2026-11-09 07:01:42")
	for _, c := range []struct {
		answer, want string
	}{
		{"2026-11-09 07:01:42x", "resp.0.at"},
		{"2026-11-09 07:01:43x", ""},
	} {
		dc := NewDriftCache()
		d := &decider{dr: dc, x: newReaderIndex(dc), in: Input{Resp: []Leaf{{Path: "resp.0.at", Noise: "resp.0.at", Type: String, Value: c.answer}}}}
		if got := d.instantEchoed(changedInstant{r: "2026-11-09 07:01:42", rm: &read}); got != c.want {
			t.Errorf("an answer %q beside the read 2026-11-09 07:01:42 echoes it at %q; want %q", c.answer, got, c.want)
		}
	}
}

// A request that holds the answer's instant is no source of it where it
// holds the read too, read in the application's zone as well (a wall
// clock of no zone): it is the read; nor where round F holds another
// instant there but none written as the answer's was. A date is its
// midnight, so a date made anew is a source of the midnight the answer
// holds, and a time of day made anew of the time of day.
func TestARequestHoldingTheReadInTheAppsZoneIsNoSource(t *testing.T) {
	leaf := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{"body.q": {Path: "body.q", Noise: "body.q", Type: String, Value: v}}}
	}
	const day = 24 * time.Hour
	for _, c := range []struct {
		name, read, answer, text, rec, live string
		unit                                time.Duration
		want                                bool
	}{
		{"the answer's second alone, made anew", "2026-11-09 12:31:42.683", "1794207702", "1794207702", "1794207702", "1794207772", time.Second, true},
		{"the answer's second beside the read in India as Unix milliseconds", "2026-11-09 12:31:42.683", "1794207702", "1794207702", "1794207702 1794207702683", "1794207772 1794207772835", time.Second, false},
		{"the answer's second beside a date, round F holding the date alone", "2026-11-09 12:31:42.683", "1794207702", "1794207702", "1794207702 2026-11-01", "2026-11-02", time.Second, false},
		{"a typed midnight, its date made anew", "2026-11-09T10:30:00.123Z", "2026-11-09T00:00:00.000Z", "", "2026-11-09", "2026-11-10", day, true},
		{"a typed midnight, that midnight made anew", "2026-11-09T10:30:00.123Z", "2026-11-09T00:00:00.000Z", "", "2026-11-09T00:00:00.000Z", "2026-11-10T00:00:00.000Z", day, true},
		{"a time of day's second, made anew", "07:01:42.683", "07:01:42", "", "07:01:42", "07:02:52", time.Second, true},
	} {
		dc := NewDriftCache()
		dc.zone = ist
		d := &decider{dr: dc, x: newReaderIndex(dc), in: Input{Rec: leaf(c.rec), Live: leaf(c.live)}}
		if got := d.heldElsewhere(momentOf(t, c.answer), momentOf(t, c.read), c.unit, c.text, false); got != c.want {
			t.Errorf("%s: a request %q (round F %q) is a source of the answer's %q beside the read %q: %v; want %v", c.name, c.rec, c.live, c.answer, c.read, got, c.want)
		}
	}
}

// The same look-back as a word of a raw text (dirtyAt<2026-...Z&limit=10):
// one rule across value forms. It waits for the same change to the order.
func TestALookBackInARawTextIsAClockThroughTheOrder(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	const p = "url.query.q"
	at := time.Date(2026, 10, 31, 9, 0, 0, 400e6, time.UTC)
	fAt := at.Add(FullRoundLead + 13*ms)
	text := func(v time.Time) string { return "dirtyAt<" + v.UTC().Truncate(sec).Format(time.RFC3339) + "&limit=10" }
	for _, width := range []time.Duration{61 * ms, 3200 * ms} {
		d := v5Decide(t, p, String, text(at.Add(-4*ms-2*sec)), text(fAt.Add(-4*ms-2*sec)), at, fAt, width)
		if d.Clock == nil || d.Outcome != OutcomeClock || d.Clock.Word.IsZero() {
			t.Errorf("window %s: %+v; want a Clock on the text's word", width, d)
			continue
		}
		for ph := 7 * ms; ph < sec; ph += 70 * ms {
			to := at.Add(72*time.Hour + ph)
			sp := Span{From: to.Add(-61 * ms), To: to}
			leaf := func(v time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: text(v)} }
			if got, why := CheckClockRead(p, *d.Clock, leaf(to.Add(-4*ms-2*sec)), sp, true); !got {
				t.Errorf("FALSE RED: window %s, %s into its second: %s", width, ph, why)
			}
			if got, _ := CheckClockRead(p, *d.Clock, leaf(to.Add(-4*ms)), sp, true); got {
				t.Errorf("FALSE GREEN: window %s, a dropped look-back, %s into its second", width, ph)
			}
		}
	}
}

// An offset checked against its unit (not cut: clock + 30m at the
// millisecond) is refused where either run's call's window is too wide for
// it (Checkable reads both): a replay as wide would refuse it whatever it
// carried. The reason names the wider.
func TestAnOffsetIsRefusedWhereEitherRunsWindowIsTooWideForItsUnit(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.expiresAt"
	start := time.Date(2026, 11, 1, 15, 21, 40, 400e6, time.UTC)
	for _, ws := range []struct {
		name   string
		rw, fw time.Duration
	}{
		{"the recording's window a minute and a second wide", 61 * sec, 50 * ms},
		{"record's full round's window a minute and a second wide", 50 * ms, 61 * sec},
	} {
		rec, f := v5Call(start, ws.rw, 4*ms), v5Call(start.Add(FullRoundLead), ws.fw, 4*ms)
		o, why, ok := clockInWindow(p, v5Instant(rec.read.Add(30*time.Minute), ms), v5Instant(f.read.Add(30*time.Minute), ms), v5Window(rec, f), nil)
		if ok || !strings.Contains(why, "a replay in a window as wide as its call's (1m1s) could not tell a time to live changed by a minute from it") {
			t.Errorf("%s: %+v %q %v; want refused, naming the window", ws.name, o, why, ok)
		}
	}
}

// An offset cut to a unit whose range holds no whole cut (a cut hour plus
// 90 minutes, a cut minute less 90 seconds: the application cuts first and
// offsets after) is read as its range, never as "each whole hour from" a
// range that holds none, and is checked within one cut of it on either
// side, a look-back too.
func TestARangeHoldingNoWholeCutIsReadAsItsRange(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	for _, row := range []struct {
		o     models.ClockOffset
		named string
	}{
		{models.ClockOffset{Min: 89 * time.Minute, Max: 91 * time.Minute, Res: time.Hour, Step: time.Hour, Layout: models.LayoutTyped}, "clock plus an offset from 1h29m to 1h31m"},
		{models.ClockOffset{Min: -90*sec - 200*ms, Max: -90*sec + 300*ms, Res: time.Minute, Step: time.Minute, Layout: models.LayoutTyped}, "from -1m30.2s to -1m29.7s"},
	} {
		if !row.o.CoarseCut() {
			t.Fatalf("precondition: %+v CoarseCut false; want a cut", row.o)
		}
		lo, hi := row.o.CutOffsets()
		if lo != row.o.Min || hi != row.o.Max {
			t.Errorf("%+v: CutOffsets %s..%s; want its range", row.o, lo, hi)
		}
		why := lineV5("body.expiresAt", row.o, sec, 0, "")
		if !strings.Contains(why, "against "+row.o.Range()+", as standing for") || !strings.Contains(row.o.Range(), row.named) || !strings.Contains(why, "on either side, whatever the window's width") || !strings.Contains(why, "changed by one "+unitWord(row.o.Res)+" passes there at every read") {
			t.Errorf("%+v: the learned line does not read it as its range, on either side: %s", row.o, why)
		}
	}
}

// The learned line of an offset checked at its cut names which way a
// changed one passes: of a time to live, a longer one where turns of the
// cut fall between the application's read and its call, a shorter one
// between the window's start and the read; of a look-back, the other way
// round. A replay checks both within one cut on either side.
func TestALearnedCutLineNamesWhichWayAChangedOffsetPasses(t *testing.T) {
	hour, day := time.Hour, 24*time.Hour
	ttl := "between the application's read and its call (a longer one) or between the window's start and the read (a shorter one)"
	back := "between the window's start and the application's read (a longer one) or between the read and its call (a shorter one)"
	at := func(min, max, res time.Duration) models.ClockOffset {
		return models.ClockOffset{Min: min, Max: max, Res: res, Step: res, Layout: models.LayoutTyped}
	}
	for _, row := range []struct {
		name string
		o    models.ClockOffset
		want string
	}{
		{"clock + 30d at the day", at(30*day-3*hour, 30*day+20*hour, day), ttl},
		{"clock + 2h at the hour", at(2*hour-10*time.Minute, 2*hour+50*time.Minute, hour), ttl},
		{"now - 7d at the day", at(-7*day-3*hour, -7*day+20*hour, day), back},
		{"now - 2h at the hour", at(-2*hour-10*time.Minute, -2*hour+50*time.Minute, hour), back},
		{"now - 2s at the second", at(-2*time.Second-155*time.Millisecond, -2*time.Second+368*time.Millisecond, time.Second), back},
	} {
		if !row.o.CoarseCut() {
			t.Fatalf("precondition: %s (%+v) is no offset checked at its cut", row.name, row.o)
		}
		if why := lineV5("body.cutoff", row.o, time.Second, 0, ""); !strings.Contains(why, ", "+row.want+", ") || !strings.Contains(why, "on either side, whatever the window's width") {
			t.Errorf("%s: the learned line does not say %q, on either side: %s", row.name, row.want, why)
		}
	}
}

// A time to live its call's window cannot check (too wide for its unit, or
// Fine for it) is read from its test's begin where that window holds it.
func TestATimeToLiveItsWindowCannotCheckIsReadFromItsTestsBegin(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.expiresAt"
	day := 24 * time.Hour
	start := time.Date(2026, 11, 1, 15, 21, 40, 400e6, time.UTC)
	window := func(rec, f v5Read, began time.Duration) *Window {
		return (&Window{Rec: Span{From: rec.from, To: rec.to, Began: rec.to.Add(-began)}, F: Span{From: f.from, To: f.to, Began: f.to.Add(-began)}}).ComparesOffsets()
	}
	rec, f := v5Call(start, 61*sec, 4*ms), v5Call(start.Add(FullRoundLead), 61*sec, 4*ms)
	o, why, ok := clockInWindow(p, v5Instant(rec.read.Add(30*time.Minute), ms), v5Instant(f.read.Add(30*time.Minute), ms), window(rec, f, sec), nil)
	if !ok || why != "" || o.Since != models.SinceTestBegin || o.Unit() != time.Minute {
		t.Errorf("clock + 30m at the millisecond in a window a minute wide, its test a second old: %+v %q %v; want it read from its test's begin", o, why, ok)
	}
	rec, f = v5Call(start, 61*ms, 3200*ms), v5Call(start.Add(FullRoundLead), 61*ms, 3200*ms)
	o, why, ok = clockInWindow(p, v5Instant(rec.read.Add(30*day), ms), v5Instant(f.read.Add(30*day), ms), window(rec, f, 5*sec), nil)
	if !ok || why != "" || o.Since != models.SinceTestBegin || o.Unit() != day {
		t.Errorf("clock + 30d at the millisecond, read 3.2 s before its call: %+v %q %v; want it read from its test's begin", o, why, ok)
	}
	line := sinceLineV5(p, models.ClockOffset{Min: time.Hour + 10*time.Minute, Max: 2*time.Hour + 50*time.Minute, Res: time.Hour, Step: time.Hour, Layout: models.LayoutTyped, Since: models.SinceTestBegin})
	for _, part := range []string{"holds its test's begin plus 2h,", "after its call plus 2h,"} {
		if !strings.Contains(line, part) {
			t.Errorf("a time to live at the hour since its test began: the line does not say %q: %s", part, line)
		}
	}
}

// A time to live in milliseconds read 3.03 s before its call (clock +
// 30d), whose narrow window (61 ms) a replay could check, but which reads
// in seconds there (clock + 29d23h59m57s): Fine, refused in a keploy test
// with main's reason, and in a client test read from its test's begin,
// where it is clock + 30d, never refused for its call's window.
func TestAFineTimeToLiveItsWindowCouldCheckIsReadFromItsTestsBegin(t *testing.T) {
	sec, ms, day := time.Second, time.Millisecond, 24*time.Hour
	p := "body.expiresAt"
	start := time.Date(2026, 11, 1, 15, 21, 40, 400e6, time.UTC)
	rec, f := v5Call(start, 61*ms, 3030*ms), v5Call(start.Add(FullRoundLead), 61*ms, 3030*ms)
	r, l := v5Instant(rec.read.Add(30*day), ms), v5Instant(f.read.Add(30*day), ms)
	quiet := v5Window(rec, f)
	if o, _, _, ok := offsetV5(r, l, quiet, nil); !ok || !o.Fine() || !o.Checkable(ms, quiet.Rec.From, quiet.Rec.To) || !o.Checkable(ms, quiet.F.From, quiet.F.To) {
		t.Fatalf("precondition: %+v %v; want an offset Fine for its unit that its call's window could check", o, ok)
	}
	if o, why, ok := clockInWindow(p, r, l, quiet, nil); ok || !strings.Contains(why, "so keploy refuses an offset that reads in a second or finer") {
		t.Errorf("keploy test: %+v %q %v; want main's refusal", o, why, ok)
	}
	client := (&Window{Rec: Span{From: rec.from, To: rec.to, Began: rec.to.Add(-5 * sec)}, F: Span{From: f.from, To: f.to, Began: f.to.Add(-5 * sec)}}).ComparesOffsets()
	if o, why, ok := clockInWindow(p, r, l, client, nil); !ok || why != "" || o.Since != models.SinceTestBegin || o.Unit() != day {
		t.Errorf("client test: %+v %q %v; want clock + 30d read from its test's begin", o, why, ok)
	}
}

// A time to live of an application that cuts its read to the second and
// adds thirty minutes, written in milliseconds (every value at .000), whose
// two values stand 61 s apart (a whole number of seconds, its own cut, and
// no whole minute): read at its own cut, and its line says so; it never
// says its digits show no such cut, which they show.
func TestATimeToLiveAtItsOwnCutNeverNamesAStepItsDigitsShow(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.expiresAt"
	value := func(read time.Time) time.Time { return read.Truncate(sec).Add(30 * time.Minute) }
	start := time.Date(2026, 11, 1, 15, 21, 40, 20e6, time.UTC)
	rec, f := v5Call(start, 61*ms, 4*ms), v5Call(start.Add(61*sec), 61*ms, 4*ms)
	o, why, ok := clockInWindow(p, v5Instant(value(rec.read), ms), v5Instant(value(f.read), ms), v5Window(rec, f), nil)
	if !ok || o.Res != sec || o.Step != sec {
		t.Fatalf("%+v %q %v; want clock + 30m at the second", o, why, ok)
	}
	if !strings.Contains(o.Why, ", cut to a second.") || strings.Contains(o.Why, "show no such cut") {
		t.Errorf("the line does not read it at its own cut: %s", o.Why)
	}
}

// A look-back cut to a day, whose values stand a day apart, is locked to
// the day's phase, as one cut to the minute is to the minute's, and its
// line names that gap.
func TestADayCutLookBackIsLockedToItsDay(t *testing.T) {
	sec, ms, day := time.Second, time.Millisecond, 24*time.Hour
	value := func(read time.Time) time.Time { return read.Truncate(day).Add(-103 * sec) }
	rec := v5Call(time.Date(2026, 11, 1, 23, 59, 50, 400e6, time.UTC), 61*ms, 4*ms)
	f := v5Call(time.Date(2026, 11, 2, 0, 0, 52, 0, time.UTC), 61*ms, 4*ms)
	o, why, ok := clockInWindow("body.until", v5Instant(value(rec.read), sec), v5Instant(value(f.read), sec), v5Window(rec, f), nil)
	if !ok || why != "" || o.Step != day || o.Res != sec || o.Min != -103*sec || o.Max != -102*sec {
		t.Fatalf("cut_d(now) - 103s: %+v %q %v; want it locked to the day", o, why, ok)
	}
	if !strings.Contains(o.Why, "Its two values stand "+models.ShortDuration(day)+" apart") {
		t.Errorf("the line does not name the values' gap: %s", o.Why)
	}
	for h := time.Duration(0); h < 24; h++ {
		to := rec.to.Add(48 * time.Hour).Truncate(day).Add(h*time.Hour + 7*time.Minute)
		read := to.Add(-4 * ms)
		if !o.Holds(value(read), sec, to.Add(-61*ms), to) {
			t.Errorf("FALSE RED: the application's value, %d h into its day", h)
		}
		if o.Holds(read.Truncate(sec), sec, to.Add(-61*ms), to) {
			t.Errorf("FALSE GREEN: a dropped look-back, %d h into its day", h)
		}
	}
}

// A time to live whose two values stand a whole minute apart is read at the
// minute, which holds both the application that cuts to it and one that
// does not, at Step and Res the minute, and its line says so.
func TestATimeToLiveWhoseValuesStandAWholeMinuteApartIsReadAtTheMinute(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	start := time.Date(2026, 11, 1, 15, 21, 40, 400e6, time.UTC)
	rec, f := v5Call(start, 61*ms, 4*ms), v5Call(start.Add(time.Minute), 61*ms, 4*ms)
	o, why, ok := clockInWindow("body.expiresAt", v5Instant(rec.read.Truncate(sec).Add(90*sec), sec), v5Instant(f.read.Truncate(sec).Add(90*sec), sec), v5Window(rec, f), nil)
	if !ok || why != "" || o.Step != time.Minute || o.Res != time.Minute {
		t.Fatalf("%+v %q %v; want read at the minute", o, why, ok)
	}
	if !strings.Contains(o.Why, "the step its two values stand 1m apart") {
		t.Errorf("the line does not name the step: %s", o.Why)
	}
}

// A Go application in +05:30 cuts on UTC's hour (Truncate counts from
// UTC), and writes its value with its own offset: a replay reads it on
// UTC's grid whatever the offset it is written with.
func TestAnHourLockedLookBackWrittenWithItsOwnOffsetIsReadOnUTCsGrid(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.until"
	india := time.FixedZone("", 5*3600+1800)
	o := models.ClockOffset{Min: -20*time.Minute - 7*sec, Max: -20*time.Minute - 6*sec, Res: sec, Step: time.Hour}
	for ph := 7 * time.Minute; ph < time.Hour; ph += 7 * time.Minute {
		to := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC).Add(ph)
		v := to.Truncate(time.Hour).Add(-20*time.Minute - 7*sec)
		o.Layout = clockTextLayout(v.In(india).Format(time.RFC3339))
		if got, why := CheckClockRead(p, o, Leaf{Path: p, Noise: p, Type: String, Value: v.In(india).Format(time.RFC3339)}, Span{From: to.Add(-61 * ms), To: to}, true); !got {
			t.Errorf("FALSE RED: %s into its hour: %s", ph, why)
		}
	}
}

// An application that cuts to the second and subtracts 1500 ms, whose two
// runs' values happen to stand exactly sixty seconds apart, is read at the
// minute they stand a whole number of: the phase lock's stated cost. A
// replay passes its value only where its second of the minute lands on the
// learned one; anywhere else it is a false red, which auto-replay's own
// replays read and set aside. A dropped look-back fails at every read.
func TestASecondCutterWhoseValuesStandAWholeMinuteApartIsLockedToTheMinute(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	value := func(read time.Time) time.Time { return read.Truncate(sec).Add(-1500 * ms) }
	start := time.Date(2026, 11, 1, 15, 22, 0, 155e6, time.UTC) // the first second of a minute: the look-back crosses into the last one
	rec, f := v5Call(start, 61*ms, 20*ms), v5Call(start.Add(60*sec+300*ms), 61*ms, 20*ms)
	o, why, ok := clockInWindow("body.until", v5Instant(value(rec.read), ms), v5Instant(value(f.read), ms), v5Window(rec, f), nil)
	if !ok || why != "" || o.Step != time.Minute || o.Res != ms {
		t.Fatalf("%+v %q %v; want it locked to the minute", o, why, ok)
	}
	if !strings.Contains(o.Why, "fails a replay wherever its value's remainder within 1m moves (a false red)") {
		t.Errorf("the line does not state the cost: %s", o.Why)
	}
	passes := 0
	for s := time.Duration(0); s < 60; s++ {
		to := start.Add(48 * time.Hour).Truncate(time.Minute).Add(s*sec + 155*ms)
		read := to.Add(-20 * ms)
		if o.Holds(value(read), ms, to.Add(-61*ms), to) {
			passes++
		}
		if o.Holds(read.Truncate(ms), ms, to.Add(-61*ms), to) {
			t.Errorf("FALSE GREEN: a dropped look-back, %d s into its minute", s)
		}
	}
	if passes != 1 {
		t.Errorf("the application's value passes %d of the sixty seconds of a minute; want the learned one alone, the stated cost", passes)
	}
}

// A look-back of a millisecond's value that stands under a second before
// its call in both runs is a plain read's ordinary lead too: where the
// window from its test's begin holds it, the reading leaves it to the
// order's step 7. A second or more before its call it stays refused, read
// from the test's begin it would pass a dropped look-back. A time to live
// under a minute is refused in that window too, and one run's narrow window
// is no wide window.
func TestAMillisecondsLeadUnderASecondIsLeftToItsBegin(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.app.createdAt"
	start := time.Date(2026, 11, 1, 15, 21, 40, 400e6, time.UTC)
	window := func(rec, f v5Read) *Window {
		return (&Window{Rec: Span{From: rec.from, To: rec.to, Began: rec.to.Add(-5 * sec)}, F: Span{From: f.from, To: f.to, Began: f.to.Add(-5 * sec)}}).ComparesOffsets()
	}
	for _, row := range []struct {
		name    string
		lead    time.Duration
		refused bool
	}{
		{"500 ms", 500 * ms, false},
		{"900 ms", 900 * ms, false},
		{"1.5 s", 1500 * ms, true},
		{"2 s", 2 * sec, true},
	} {
		rec, f := v5Call(start, 50*ms, row.lead), v5Call(start.Add(FullRoundLead), 50*ms, row.lead)
		o, why, ok := clockInWindow(p, v5Instant(rec.read, ms), v5Instant(f.read, ms), window(rec, f), nil)
		if ok || (why != "") != row.refused {
			t.Errorf("read %s before its call: %+v %q %v; want refused %v", row.name, o, why, ok, row.refused)
		}
	}
	rec, f := v5Call(start, 61*ms, 4*ms), v5Call(start.Add(FullRoundLead), 3200*ms, 4*ms)
	rv, lv := rec.read.Truncate(sec).Add(30*sec), f.read.Truncate(sec).Add(30*sec)
	if o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), v5Window(rec, f), nil); !ok || why != "" {
		t.Errorf("clock + 30s, one run's window narrow: %+v %q %v; want the Clock its narrow window reads", o, why, ok)
	}
	rec, f = v5Call(start, 61*ms, 4*ms), v5Call(start.Add(FullRoundLead), 61*ms, 4*ms)
	rv, lv = rec.read.Truncate(sec).Add(30*sec), f.read.Truncate(sec).Add(30*sec)
	if o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), window(rec, f).sinceBegin(), nil); ok || !strings.Contains(why, "a time to live under a minute") {
		t.Errorf("clock + 30s from a test's begin five seconds back: %+v %q %v; want refused, as main refuses it", o, why, ok)
	}
}

// The same through the order: the reading's refusal stands where the window
// holds the value as a plain read too, so a BSON date's now - 2s is refused
// in every window a quiet one refuses it in, never a plain read that fails
// the application's value on a quiet replay and passes a dropped look-back.
// A plain read made just before its call stays one.
func TestAMillisecondsLookBackIsRefusedThroughTheOrderInEveryWindow(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	const p = "body.filter.dirtyAt.$lte"
	at := time.Date(2026, 10, 31, 9, 0, 0, 400e6, time.UTC)
	fAt := at.Add(2*time.Hour + 17*time.Minute + 23*time.Second + 153*ms)
	millis := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	for _, width := range []time.Duration{30 * ms, 1100 * ms, 2500 * ms, 3200 * ms, 6 * sec} {
		d := v5Decide(t, p, Number, millis(at.Add(-4*ms-2*sec)), millis(fAt.Add(-4*ms-2*sec)), at, fAt, width)
		if d.Clock != nil || d.Outcome != OutcomeRefused || d.Refusal == nil || !strings.Contains(d.Refusal.Why, "so keploy refuses an offset that reads in a second or finer") {
			t.Errorf("now - 2s in a window %s wide: %+v; want refused, as a quiet window refuses it", width, d)
		}
		d = v5Decide(t, p, Number, millis(at.Add(-4*ms)), millis(fAt.Add(-4*ms)), at, fAt, width)
		if d.Clock == nil || !d.Clock.Plain() {
			t.Errorf("a plain read in a window %s wide: %+v; want a plain read", width, d)
		}
	}
}

// A phase lock holds the remainder both runs share within the step only
// where one whole step of look-back is common to both runs' windows. Where
// each run's window holds a turn of the step (one widened by load to 1.1 s
// beside one of 30 s or 3m20s, in either order, or a 61 ms window at a turn
// beside a 30 s one), two phases hold both runs, every remainder of the
// step stands in the range, and a lock there would pass a look-back
// changed by any part of the step at every read: the reading declines the
// lock, and the offset is refused at the values' own cut, as a quiet window
// refuses it (fail closed). The same of a minute's step. Wherever a lock or
// any other offset is learned, a replay passes the application's value at
// every phase and fails a dropped look-back and one changed by half a
// step at every phase.
func TestAPhaseLockIsLearnedOnlyWhereBothRunsShareOneWholeStep(t *testing.T) {
	sec, ms, min := time.Second, time.Millisecond, time.Minute
	const p = "body.filter.until"
	for _, c := range []struct {
		name      string
		res, step time.Duration
		back      time.Duration
		rw, fw    time.Duration
		recAt     time.Time
	}{
		{"cut_s(now) - 1500ms, 1.1s beside 30s", ms, sec, 1500 * ms, 1100 * ms, 30 * sec, time.Date(2026, 10, 31, 9, 0, 0, 400e6, time.UTC)},
		{"cut_s(now) - 1500ms, 30s beside 1.1s", ms, sec, 1500 * ms, 30 * sec, 1100 * ms, time.Date(2026, 10, 31, 9, 0, 0, 400e6, time.UTC)},
		{"cut_s(now) - 1500ms, 1.1s beside 3m20s", ms, sec, 1500 * ms, 1100 * ms, 200 * sec, time.Date(2026, 10, 31, 9, 0, 0, 400e6, time.UTC)},
		{"cut_s(now) - 1500ms, 3m20s beside 1.1s", ms, sec, 1500 * ms, 200 * sec, 1100 * ms, time.Date(2026, 10, 31, 9, 0, 0, 400e6, time.UTC)},
		{"cut_s(now) - 1500ms, 61ms at a turn beside 30s", ms, sec, 1500 * ms, 61 * ms, 30 * sec, time.Date(2026, 10, 31, 9, 0, 0, 30e6, time.UTC)},
		{"cut_s(now) - 1500ms, 30s beside 61ms at a turn", ms, sec, 1500 * ms, 30 * sec, 61 * ms, time.Date(2026, 10, 31, 9, 0, 0, 30e6, time.UTC)},
		{"cut_m(now) - 103s, 61ms at a turn beside 3m20s", sec, min, 103 * sec, 61 * ms, 200 * sec, time.Date(2026, 10, 31, 9, 0, 0, 30e6, time.UTC)},
		{"cut_m(now) - 103s, 3m20s beside 1.1s at a turn", sec, min, 103 * sec, 200 * sec, 1100 * ms, time.Date(2026, 10, 31, 9, 0, 0, 500e6, time.UTC)},
	} {
		value := func(read time.Time) time.Time { return read.Truncate(c.step).Add(-c.back) }
		text := func(v time.Time) string {
			if c.res < sec {
				return strconv.FormatInt(v.UnixMilli(), 10)
			}
			return strconv.FormatInt(v.Unix(), 10)
		}
		rec := v5Call(c.recAt, c.rw, 4*ms)
		spans, learned := 0, 0
		for d := time.Duration(0); d < c.step; d += c.step / 20 {
			// Round F 17 steps and d after a whole number of hours: its
			// value stands a whole number of steps, never of hours, from
			// the recording's.
			f := v5Call(c.recAt.Add(73*time.Hour+17*c.step+d), c.fw, 4*ms)
			rv, lv := value(rec.read), value(f.read)
			o, why, ok := clockInWindow(p, v5Instant(rv, c.res), v5Instant(lv, c.res), v5Window(rec, f), nil)
			if !ok {
				continue
			}
			learned++
			if o.Step > o.Res && o.Max-o.Min >= o.Step {
				spans++
				t.Errorf("FALSE GREEN: %s, round F %s later in its step: %s (%q), a lock whose range spans its step; want none", c.name, d, o, why)
				continue
			}
			o.Layout = clockTextLayout(text(rv))
			for ph := 7 * ms; ph < c.step; ph += c.step / 13 {
				to := c.recAt.Add(96*time.Hour + ph)
				s := Span{From: to.Add(-61 * ms), To: to}
				read := to.Add(-4 * ms)
				for _, row := range []struct {
					name string
					at   time.Time
					want bool
				}{
					{"the application's value", value(read), true},
					{"a dropped look-back", read.Truncate(c.res), false},
					{"half a step shorter", value(read).Add(c.step / 2), false},
					{"half a step longer", value(read).Add(-c.step / 2), false},
				} {
					if row.want && o.Step == o.Res && c.step == min {
						// A minute cutter read at its own second where
						// both runs read at about one phase of their
						// minutes: the stated false red of the minute
						// cutter (TestAMinuteCutterReadAtTheSameSecondInBothRunsFailsAThirdReadAtAnotherPhase).
						continue
					}
					leaf := Leaf{Path: p, Noise: p, Type: String, Value: text(row.at)}
					if got, why := CheckClockRead(p, o, leaf, s, true); got != row.want {
						t.Errorf("%s: %s, round F %s later, learned %s, a replay %s into its step, %s: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], c.name, d, o, ph, row.name, got, why)
					}
				}
			}
		}
		t.Logf("%s: %d of 20 recordings learned, %d spanning their step", c.name, learned, spans)
	}
}

// v5DecideIn is v5Decide in the window w.
func v5DecideIn(t *testing.T, path string, typ Type, rec, live string, rAt, fAt time.Time, w *Window) Decision {
	t.Helper()
	if reflect.ValueOf(decideBy).Pointer() == reflect.ValueOf(refuseAll).Pointer() {
		t.Skip("the order of a changed word is not on this tree")
	}
	leaf := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Noise: path, Type: typ, Value: v}}}
	}
	dc := NewDriftCache()
	dc.appClock = true
	ds := Decide(Input{
		Drifts: dc, Drift: []string{path}, Live: leaf(live), Rec: leaf(rec),
		At: rAt, Arrived: fAt, Test: "test-2", Mock: "mock-300", Mode: ModeOwn,
		Window: w, Landed: Landed{Where: LandedBetween},
		Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
	})
	if len(ds) != 1 {
		t.Fatalf("%q to %q: %d decisions; want one", rec, live, len(ds))
	}
	return ds[0]
}

// The milliseconds second-cutter in a window of 1.1 s beside one of 30 s,
// through the order: whatever it decides, its line never claims what a
// replay does not check. Where it is a Clock, a replay passes the
// application's value and fails a look-back of 1 s in place of 1.5 s and a
// dropped one at every phase.
func TestAPhaseLockSpanningItsStepIsNoClockThroughTheOrder(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	const p = "body.filter.until"
	at := time.Date(2026, 10, 31, 9, 0, 0, 400e6, time.UTC)
	val := func(r time.Time, b time.Duration) time.Time { return r.Truncate(sec).Add(-b) }
	text := func(v time.Time) string { return strconv.FormatInt(v.UnixMilli(), 10) }
	for _, ws := range [][2]time.Duration{{1100 * ms, 30 * sec}, {30 * sec, 1100 * ms}, {1100 * ms, 200 * sec}} {
		for d := time.Duration(0); d < sec; d += 130 * ms {
			fAt := at.Add(FullRoundLead + d)
			w := (&Window{Rec: Span{From: at.Add(-ws[0]), To: at}, F: Span{From: fAt.Add(-ws[1]), To: fAt}}).ComparesOffsets()
			dd := v5DecideIn(t, p, Number, text(val(at.Add(-4*ms), 1500*ms)), text(val(fAt.Add(-4*ms), 1500*ms)), at, fAt, w)
			if dd.Clock == nil {
				continue
			}
			for ph := 7 * ms; ph < sec; ph += 50 * ms {
				to := at.Add(72*time.Hour + ph)
				sp := Span{From: to.Add(-61 * ms), To: to}
				lf := func(v time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: text(v)} }
				if ok, why := CheckClockRead(p, *dd.Clock, lf(val(to.Add(-4*ms), 1500*ms)), sp, true); !ok {
					t.Errorf("FALSE RED: windows %s and %s, round F %s later, %s, a replay %s into its second: %s", ws[0], ws[1], d, dd.Clock, ph, why)
				}
				for _, wrong := range []time.Time{val(to.Add(-4*ms), sec), to.Add(-4 * ms)} {
					if ok, _ := CheckClockRead(p, *dd.Clock, lf(wrong), sp, true); ok {
						t.Errorf("FALSE GREEN: windows %s and %s, round F %s later, %s (%s), a replay %s into its second passes %s", ws[0], ws[1], d, dd.Clock, dd.Clock.Why, ph, text(wrong))
					}
				}
			}
		}
	}
}

// A call whose replay does not compare a read of the clock plus an offset
// (a window without ComparesOffsets: HTTP's learn path today), in a client
// test whose begin comes before the value: a whole-second look-back
// (dirtyAt <= now - 2s, last_active < now - 15s) is refused, naming that
// the replay does not compare it, never handed to the window from the
// test's begin, where a plain read since the begin would pass a dropped
// look-back at every replay. A plain read made in its call's window is
// left to the plain read, as before.
func TestALookBackAReplayDoesNotCompareIsRefusedNeverReadFromItsTestsBegin(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	const p = "body.filter.dirtyAt.$lte"
	call := time.Date(2026, 10, 31, 9, 0, 0, 400e6, time.UTC)
	fcall := call.Add(FullRoundLead + 130*ms)
	span := func(at time.Time) Span { return Span{From: at.Add(-61 * ms), To: at, Began: at.Add(-20 * sec)} }
	w := &Window{Rec: span(call), F: span(fcall)}
	for _, back := range []time.Duration{2 * sec, 15 * sec} {
		rv, lv := call.Add(-4*ms).Truncate(sec).Add(-back), fcall.Add(-4*ms).Truncate(sec).Add(-back)
		o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), w, nil)
		if ok || !strings.Contains(why, "this protocol's replay does not compare such an offset yet") {
			t.Errorf("FALSE GREEN: now - %s where a replay does not compare offsets: %+v %q %v; want it refused, naming that", back, o, why, ok)
		}
	}
	rv, lv := call.Add(-4*ms).Truncate(sec), fcall.Add(-4*ms).Truncate(sec)
	if o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), w, nil); ok || why != "" {
		t.Errorf("a plain read in its call's window where a replay does not compare offsets: %+v %q %v; want it left to the plain read", o, why, ok)
	}
}

// A look-back of whole hours from a read cut to its zone's hour (a Java
// application in +05:30 truncating to its own hour, less two or three
// hours) is learned at its own cut, the hour (Step == Res), and a replay
// that reads the application's zone from its process checks it at that
// cut, never on a step's grids as a phase lock is. The recording shows
// where that hour falls within UTC's (its values' minutes past UTC's whole
// hour: 30m in +05:30, 15m in +05:45, none in a whole-hour zone), and a
// replay passes a value only on that phase: the application's value at
// every phase of the hour; one 1h15m or 1h30m earlier (on UTC's hour, or
// another zone's, or on none), and a dropped look-back, at none. Without
// the phase, a value moved onto UTC's hour was read in UTC as a value whole
// there, within the own cut's slack: in +05:45 one 1h15m earlier passed 9
// of the 12 phases, in +05:30 one 1h30m earlier 6.
func TestAnOwnCutHourInTheAppsZoneIsCheckedOnlyAtThatZonesHour(t *testing.T) {
	sec, ms, min := time.Second, time.Millisecond, time.Minute
	const p = "body.cutoff.$lte"
	for _, zn := range []*time.Location{time.FixedZone("+05:30", 5*3600+1800), time.FixedZone("+05:45", 5*3600+2700), time.FixedZone("+02:00", 2*3600)} {
		for _, back := range []time.Duration{2 * time.Hour, 3 * time.Hour} {
			cut := func(x time.Time) time.Time { return models.ReadCut(x, time.Hour, zn).Add(-back) }
			for _, base := range []time.Time{time.Date(2026, 10, 26, 9, 40, 0, 0, time.UTC), time.Date(2026, 10, 26, 9, 10, 0, 0, time.UTC)} {
				rec := v5Call(base, 61*ms, 20*ms)
				f := v5Call(base.Add(3*time.Hour+5*min), 61*ms, 20*ms)
				o, why, ok := clockInWindow(p, v5Instant(cut(rec.read), sec), v5Instant(cut(f.read), sec), v5Window(rec, f), zn)
				if !ok || o.Step != time.Hour || o.Res != time.Hour {
					t.Errorf("FALSE RED: recorded at %s in %s, a cut to its hour less %s: %+v %q %v; want it learned at its own cut, the hour", base.Format("15:04"), zn, back, o, why, ok)
					continue
				}
				recText := strconv.FormatInt(cut(rec.read).Unix(), 10)
				for phase := time.Duration(0); phase < time.Hour; phase += 5*min + 7*sec {
					s := Span{Zone: zn, Own: true, From: base.Add(48*time.Hour + phase)}
					s.To = s.From.Add(61 * ms)
					read := s.To.Add(-20 * ms)
					for _, row := range []struct {
						name string
						at   time.Time
						want bool
					}{
						{"the application's value", cut(read), true},
						{"1h15m earlier", cut(read).Add(-75 * min), false},
						{"1h30m earlier", cut(read).Add(-90 * min), false},
						{"a dropped look-back", read.Truncate(sec), false},
					} {
						got, why := v5Check(p, o, recText, row.at, s)
						if got != row.want {
							t.Errorf("%s: recorded at %s in %s, a cut to its hour less %s, a replay %s into its hour, %s: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], base.Format("15:04"), zn, back, phase, row.name, got, why)
						}
						if ph := row.at.Sub(row.at.Truncate(time.Hour)); !got && ph != o.Phase && !strings.Contains(why, ", off the hour the learned values sat on, ") {
							t.Errorf("recorded in %s, %s: refused as %q; want it to name that the value sits off the learned hour", zn, row.name, why)
						}
					}
				}
			}
		}
	}
}

// Two values cut to the hour, each written in a zone that shows it a whole
// hour, that sit at two places within UTC's hour (the recording's on
// +05:30's hour, 30m past UTC's, record's full round's on +05:45's, 15m past
// it): keploy cannot tell which hour a replay's value must sit on, so it
// refuses the offset (fail closed), naming both places. Read without the
// phase, it was learned at the hour, and a replay held a value on either
// zone's hour within the cut's slack.
func TestAnOwnCutHourWhoseValuesSitOnTwoPhasesIsRefused(t *testing.T) {
	ms := time.Millisecond
	const p = "body.cutoff.$lte"
	ist, npt := time.FixedZone("+05:30", 5*3600+1800), time.FixedZone("+05:45", 5*3600+2700)
	base := time.Date(2026, 10, 26, 9, 40, 0, 0, time.UTC)
	rec, f := v5Call(base, 61*ms, 20*ms), v5Call(base.Add(3*time.Hour+5*time.Minute), 61*ms, 20*ms)
	rv := models.ReadCut(rec.read, time.Hour, ist).Add(-2 * time.Hour).In(ist)
	lv := models.ReadCut(f.read, time.Hour, npt).Add(-2 * time.Hour).In(npt)
	o, why, ok := clockInWindow(p, v5Instant(rv, time.Second), v5Instant(lv, time.Second), v5Window(rec, f), nil)
	if ok || !strings.Contains(why, "two places within UTC's hour (30m past UTC's whole hour in the recording, 15m past UTC's whole hour in record's full round)") {
		t.Errorf("FALSE GREEN: values on +05:30's hour and on +05:45's: %+v %q %v; want it refused, naming both phases", o, why, ok)
	}
	// One zone's hour in both runs is learned on its phase.
	lv = models.ReadCut(f.read, time.Hour, ist).Add(-2 * time.Hour).In(ist)
	if o, why, ok := clockInWindow(p, v5Instant(rv, time.Second), v5Instant(lv, time.Second), v5Window(rec, f), nil); !ok || o.Phase != 30*time.Minute || o.Step != time.Hour || !strings.Contains(o.Why, "(30m past UTC's whole hour)") {
		t.Errorf("FALSE RED: values on +05:30's hour in both runs: %+v %q %v; want it learned at the hour on its phase, 30m past UTC's, its line naming it", o, why, ok)
	}
}

// A whole-second look-back of more than a second (dirtyAt <= now - 2s,
// last_active < now - 15s) in a client test whose begin comes before the
// value, in a call whose replay compares offsets (Window.ComparesOffsets):
// learned as the clock less that look-back at its own cut, checked by the
// replay, which passes the application's value and fails a dropped
// look-back (but in a window that holds a turn of the second, the cut's
// stated cost). Only a call whose replay does not compare offsets refuses it
// (TestALookBackAReplayDoesNotCompareIsRefusedNeverReadFromItsTestsBegin).
func TestALookBackAReplayComparesInAClientTestIsLearnedAtItsCut(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	const p = "body.filter.dirtyAt.$lte"
	call := time.Date(2026, 10, 31, 9, 0, 0, 400e6, time.UTC)
	fcall := call.Add(FullRoundLead + 130*ms)
	span := func(at time.Time) Span { return Span{From: at.Add(-61 * ms), To: at, Began: at.Add(-20 * sec)} }
	w := (&Window{Rec: span(call), F: span(fcall)}).ComparesOffsets()
	for _, back := range []time.Duration{2 * sec, 15 * sec} {
		rv, lv := call.Add(-4*ms).Truncate(sec).Add(-back), fcall.Add(-4*ms).Truncate(sec).Add(-back)
		o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), w, nil)
		if lo, hi := o.CutOffsets(); !ok || why != "" || o.Step != sec || o.Res != sec || o.Since != "" || lo != -back || hi != -back {
			t.Errorf("FALSE RED: now - %s where a replay compares offsets: %+v %q %v; want clock - %s at the second", back, o, why, ok, back)
			continue
		}
		recText := strconv.FormatInt(rv.Unix(), 10)
		for ph := time.Duration(0); ph < sec; ph += 150 * ms {
			s := Span{From: call.Add(time.Hour + ph - 61*ms), To: call.Add(time.Hour + ph)}
			read := s.To.Add(-4 * ms)
			if got, why := v5Check(p, o, recText, read.Truncate(sec).Add(-back), s); !got {
				t.Errorf("FALSE RED: now - %s, a replay %s into its second, the application's value: %s", back, ph, why)
			}
			// A window that holds a turn of the second passes a look-back
			// changed by two seconds (the cut's stated cost).
			if got, _ := v5Check(p, o, recText, read.Truncate(sec), s); got && s.From.Truncate(sec).Equal(s.To.Truncate(sec)) {
				t.Errorf("FALSE GREEN: now - %s, a replay %s into its second, a dropped look-back passes", back, ph)
			}
		}
	}
}

// A phase lock's range is never a whole step wide: such a range holds every
// remainder within the step, so it would pass a look-back changed by any
// part of it. Steps of one length never make one (a turn of the step in
// each run's window makes the range a step and a cut wide), but a day's
// step in a zone whose clocks change does: a window holding the turn after
// New York's 23-hour day of 2026-03-08 in the recording, one holding a
// 24-hour day's turn in record's full round, an application cutting to its
// zone's day less 2d3h at the hour, read after the turn in both, puts the
// range on New York's grid from -51h to -27h, exactly a day. The lock is
// read on UTC's grid instead, one hour wide.
func TestAPhaseLockIsNeverAWholeStepWide(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	m9, m20 := time.Date(2026, 3, 9, 0, 0, 0, 0, ny), time.Date(2026, 3, 20, 0, 0, 0, 0, ny)
	if d := m9.Sub(time.Date(2026, 3, 8, 0, 0, 0, 0, ny)); d != 23*time.Hour {
		t.Fatalf("New York's 2026-03-08 is %s long; the row needs its 23-hour day", d)
	}
	back := 51 * time.Hour
	rec := v5Read{from: m9.Add(-10 * time.Minute), to: m9.Add(10 * time.Minute), read: m9.Add(5 * time.Minute)}
	f := v5Read{from: m20.Add(-10 * time.Minute), to: m20.Add(10 * time.Minute), read: m20.Add(5 * time.Minute)}
	rv, lv := m9.Add(-back), m20.Add(-back)
	o, ok := phaseAt(v5Instant(rv, time.Second), v5Instant(lv, time.Second), v5Window(rec, f), 24*time.Hour, time.Hour, ny)
	if ok && o.Max-o.Min >= 24*time.Hour {
		t.Errorf("FALSE GREEN: a phase lock a whole day wide: %+v; want none that wide", o)
	}
	if !ok || o.Max-o.Min != time.Hour || o.AppZone == "" {
		t.Errorf("the lock on UTC's grid: %+v %v; want one an hour wide", o, ok)
	}
}

// A reconciler's claimedAt <= now - 60 s, an epoch's seconds, carried to an
// idle recording whose window runs two minutes back to the recording before
// it: whether keploy learns it or refuses it must not turn on where in its
// second each run's call fell. A read that fell in the second before its
// call (the call a fraction of a millisecond past a whole second) reads one
// second longer, clock - 1m1s, a range of seconds no whole minute lies in,
// and is learned at the cut in any window; any other phase reads clock -
// 1m, a range a whole minute lies in, and a window two minutes wide refuses
// it. The same application, two verdicts.
func TestAWholeMinuteLookBackInAWideWindowGetsOneVerdictAtEveryPhase(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "body.cmd.query.$and.1.$or.1.claimedAt.$lte.$numberLong"
	f := v5Call(time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC), 5*ms, 2*ms)
	base := time.Date(2026, 11, 2, 3, 53, 31, 0, time.UTC)
	verdicts := map[bool][]time.Duration{}
	for _, phase := range []time.Duration{343 * time.Microsecond, 900 * time.Microsecond, 1500 * time.Microsecond, 250 * ms, 500 * ms, 999 * ms} {
		rec := v5Call(base.Add(phase), 2*time.Minute, ms)
		rv, lv := rec.read.Truncate(sec).Add(-time.Minute), f.read.Truncate(sec).Add(-time.Minute)
		o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), v5Window(rec, f), nil)
		t.Logf("call %v into its second: %v ok=%v why=%.80q", phase, o, ok, why)
		verdicts[ok] = append(verdicts[ok], phase)
		if !ok {
			continue
		}
		// Checked at the cut: in a replay's window as wide, the
		// application's value passes at every phase and a dropped
		// look-back at none.
		recText := strconv.FormatInt(rv.Unix(), 10)
		for ph := time.Duration(0); ph < sec; ph += 125 * ms {
			s := Span{From: base.Add(72*time.Hour + ph - 2*time.Minute), To: base.Add(72*time.Hour + ph)}
			read := s.To.Add(-ms)
			if got, why := v5Check(p, o, recText, read.Truncate(sec).Add(-time.Minute), s); !got {
				t.Errorf("FALSE RED: learned at a call %v into its second, a replay %v into its second, the application's value: %s", phase, ph, why)
			}
			if got, _ := v5Check(p, o, recText, read.Truncate(sec), s); got {
				t.Errorf("FALSE GREEN: learned at a call %v into its second, a replay %v into its second passes a dropped look-back", phase, ph)
			}
		}
	}
	if len(verdicts) > 1 || len(verdicts[false]) > 0 {
		t.Errorf("the verdict turns on the phase of the recording's call, or refuses it: learned at %v, refused at %v; want it learned at every phase, checked at the cut", verdicts[true], verdicts[false])
	}
}

// An offset main's reading learned (no Step, so no Phase), of a +05:30
// application that writes the start of its own hour plus two hours: its
// values sit 30m past UTC's hour. A replay of the application's value
// passes, and one whose time to live changed fails with main's reason,
// word for word, naming no place within UTC's hour: the phase is a rule only
// of offsets learned at a Step, and an old recording never learned where its
// values sat.
func TestAnOldRecordingsHourCutFailsNamingNoPhaseItNeverLearned(t *testing.T) {
	ms, sec := time.Millisecond, time.Second
	ist := time.FixedZone("+05:30", 5*3600+1800)
	const p = "body.expires_at"
	// Main's reason (CheckClockRead on main, the same offset and value),
	// by the learned range each recording time gives.
	mainWhy := func(learned string) string {
		return p + " was learned as clock + 2h (" + learned + "), written as a number of seconds since the epoch, whole or with a fraction, a read of the application's clock plus that offset, and the value this call carries is not a read of the application's clock plus that offset made in this call's window: it changed (checked as standing for the whole 1h the learn step read it cut to, read here in UTC+05:30)"
	}
	want := map[string]string{"10:29": mainWhy("from 1h59m28.455s to 2h0m30.061s"), "11:29": mainWhy("from 1h59m8.455s to 2h0m10.061s")}
	val := func(x time.Time, ttl time.Duration) time.Time { return models.ReadCut(x, time.Hour, ist).Add(ttl) }
	for _, base := range []time.Time{time.Date(2026, 10, 26, 10, 29, 30, 0, time.UTC), time.Date(2026, 10, 26, 11, 29, 50, 0, time.UTC)} {
		rec, f := v5Call(base, 61*ms, 20*ms), v5Call(base.Add(FullRoundLead), 61*ms, 20*ms)
		rv, fv := val(rec.read, 2*time.Hour), val(f.read, 2*time.Hour)
		o, why, ok := offsetReading(p, v5Instant(rv, sec), v5Instant(fv, sec), v5Window(rec, f), ist)
		if !ok || o.Step != 0 || o.Phase != 0 || o.Res != time.Hour {
			t.Fatalf("main's reading of cut_hour_IST(now) + 2h at %s: %+v %q %v; want it learned at the hour, with no Step", base.Format("15:04"), o, why, ok)
		}
		recText := strconv.FormatInt(rv.Unix(), 10)
		for _, ttl := range []time.Duration{2 * time.Hour, 5 * time.Hour} {
			s := Span{Zone: ist, Own: true, From: base.Add(48*time.Hour + 7*time.Minute)}
			s.To = s.From.Add(61 * ms)
			got, why := v5Check(p, o, recText, val(s.To.Add(-20*ms), ttl), s)
			if got != (ttl == 2*time.Hour) {
				t.Errorf("recorded at %s, a replay of a time to live of %s: %v (%s); want it to pass only the recorded time to live", base.Format("15:04"), ttl, got, why)
			}
			if strings.Contains(why, "UTC's whole hour") {
				t.Errorf("FALSE REASON: recorded at %s, an old recording's replay of a time to live of %s names a place within UTC's hour it never learned: %q", base.Format("15:04"), ttl, why)
			}
			if ttl != 2*time.Hour && why != want[base.Format("15:04")] {
				t.Errorf("recorded at %s, an old recording's replay of a time to live of %s fails with\n%q\nwant main's reason\n%q", base.Format("15:04"), ttl, why, want[base.Format("15:04")])
			}
		}
	}
}

// A +05:30 application that cuts its read to its own day and subtracts 5h,
// recorded across its midnight: its values stand a day apart and are cut
// to the hour, 30m past UTC's, so it is a phase lock at the day's step,
// read at the hour on that phase. A replay passes the application's value
// at every phase of its day, and fails one moved onto UTC's day (cut to
// UTC's midnight, less 5h), which a lock read off its phase passed at every
// replay; the failure names where the moved value sits and the hour the
// learned values sat on.
func TestADayLockCutToTheHourInAZoneOffTheHourHoldsOnlyItsPhase(t *testing.T) {
	ms, sec := time.Millisecond, time.Second
	ist := time.FixedZone("+05:30", 5*3600+1800)
	const p = "body.since"
	app := func(x time.Time) time.Time { return models.ReadCut(x, 24*time.Hour, ist).Add(-5 * time.Hour) }
	moved := func(x time.Time) time.Time { return models.ReadCut(x, 24*time.Hour, time.UTC).Add(-5 * time.Hour) }
	call := time.Date(2026, 10, 26, 18, 29, 50, 0, time.UTC) // 23:59:50 in +05:30
	rec, f := v5Call(call, 61*ms, 20*ms), v5Call(call.Add(FullRoundLead), 61*ms, 20*ms)
	o, why, ok := clockInWindow(p, v5Instant(app(rec.read), sec), v5Instant(app(f.read), sec), v5Window(rec, f), ist)
	if !ok || o.Step != 24*time.Hour || o.Res != time.Hour || o.Phase != 30*time.Minute {
		t.Fatalf("cut_day_IST(now) - 5h across its midnight: %+v %q %v; want a lock at the day's step, read at the hour, 30m past UTC's", o, why, ok)
	}
	recText := strconv.FormatInt(app(rec.read).Unix(), 10)
	const movedTail = ": it changed (checked as standing for the whole 1h the learn step read it cut to, read here in UTC) (it sits on UTC's whole hour, off the hour the learned values sat on, 30m past UTC's whole hour)"
	for ph := time.Duration(0); ph < 24*time.Hour; ph += 37*time.Minute + 11*sec {
		s := Span{Zone: ist, Own: true, From: call.Add(72*time.Hour + ph)}
		s.To = s.From.Add(61 * ms)
		read := s.To.Add(-20 * ms)
		if got, why := v5Check(p, o, recText, app(read), s); !got {
			t.Errorf("FALSE RED: a replay %s into the day, the application's value: %s", ph, why)
		}
		got, why := v5Check(p, o, recText, moved(read), s)
		if got {
			t.Errorf("FALSE GREEN: a replay %s into the day passes a value moved onto UTC's day, %s (%s)", ph, moved(read).UTC().Format(time.RFC3339), why)
		}
		if !strings.HasSuffix(why, movedTail) {
			t.Errorf("a replay %s into the day fails a value moved onto UTC's day with\n%q\nwant it to end naming both hours:\n%q", ph, why, movedTail)
		}
	}
}

// An expiry at the start of the application's own day plus 30 days, in
// +05:30, whose two runs fell on two of its days: learned at its own cut,
// the day (Step and Res a day), checked in the application's zone. Its
// values sit 30m past UTC's hour, which an own cut of a day never holds a
// value to (only an hour's carries a Phase): a replay passes the
// application's value at every phase of its day and fails a time to live
// changed by three days and a dropped one (one changed by a day is within
// the cut's stated slack), with a reason that names no place within UTC's
// hour, since an own cut of a day learned none.
func TestAnOwnCutDayInAZoneOffTheHourPassesItsValueAtEveryPhase(t *testing.T) {
	ms, sec, day := time.Millisecond, time.Second, 24*time.Hour
	ist := time.FixedZone("+05:30", 5*3600+1800)
	const p = "body.expires_at"
	val := func(x time.Time, ttl time.Duration) time.Time { return models.ReadCut(x, day, ist).Add(ttl) }
	const failTail = ": it changed (checked as standing for the whole 1d the learn step read it cut to, read here in UTC+05:30)"
	for _, base := range []time.Time{time.Date(2026, 10, 26, 9, 40, 0, 0, time.UTC), time.Date(2026, 10, 26, 18, 10, 0, 0, time.UTC)} {
		rec, f := v5Call(base, 61*ms, 20*ms), v5Call(base.Add(day+3*time.Hour), 61*ms, 20*ms)
		o, why, ok := clockInWindow(p, v5Instant(val(rec.read, 30*day), sec), v5Instant(val(f.read, 30*day), sec), v5Window(rec, f), ist)
		if !ok || o.Step != day || o.Res != day || o.Phase != 0 {
			t.Errorf("recorded at %s, the start of its day in +05:30 plus 30 days: %+v %q %v; want it learned at its own cut, the day, with no phase", base.Format("15:04"), o, why, ok)
			continue
		}
		recText := strconv.FormatInt(val(rec.read, 30*day).Unix(), 10)
		for ph := time.Duration(0); ph < day; ph += 37*time.Minute + 11*sec {
			s := Span{Zone: ist, Own: true, From: base.Add(72*time.Hour + ph)}
			s.To = s.From.Add(61 * ms)
			read := s.To.Add(-20 * ms)
			if got, why := v5Check(p, o, recText, val(read, 30*day), s); !got {
				t.Errorf("FALSE RED: recorded at %s, a replay %s later, the application's value: %s", base.Format("15:04"), ph, why)
			}
			for _, ttl := range []time.Duration{27 * day, 33 * day, 0} {
				got, why := v5Check(p, o, recText, val(read, ttl), s)
				if got {
					t.Errorf("FALSE GREEN: recorded at %s, a replay %s later passes a time to live of %s", base.Format("15:04"), ph, ttl)
				}
				if !strings.HasSuffix(why, failTail) || strings.Contains(why, "UTC's whole hour") {
					t.Errorf("recorded at %s, a replay %s later fails a time to live of %s with\n%q\nwant it to end %q, naming no place within UTC's hour", base.Format("15:04"), ph, ttl, why, failTail)
				}
			}
		}
	}
}

// The whole-minute look-back's rule one unit up: claimedAt <= cut_m(now) -
// 1h, an epoch's seconds cut to the minute, carried to an idle recording
// whose window runs two hours back. Whether keploy learns it must not turn
// on where in its minute the call fell, and where it is learned a replay as
// wide passes the application's value at every phase of the minute and a
// dropped look-back at none.
func TestAWholeHourLookBackAtTheMinuteInAWideWindowGetsOneVerdictAtEveryPhase(t *testing.T) {
	sec, ms, min := time.Second, time.Millisecond, time.Minute
	const p = "body.claimedAt.$lte"
	f := v5Call(time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC), 5*ms, 2*ms)
	base := time.Date(2026, 11, 2, 3, 53, 0, 0, time.UTC)
	verdicts := map[bool][]time.Duration{}
	for _, phase := range []time.Duration{343 * time.Microsecond, 900 * time.Microsecond, 1500 * time.Microsecond, 250 * ms, 15 * sec, 30 * sec, 59 * sec} {
		rec := v5Call(base.Add(phase), 2*time.Hour, ms)
		rv, lv := rec.read.Truncate(min).Add(-time.Hour), f.read.Truncate(min).Add(-time.Hour)
		o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), v5Window(rec, f), nil)
		verdicts[ok] = append(verdicts[ok], phase)
		if !ok {
			t.Logf("call %v into its minute: refused, %.120q", phase, why)
			continue
		}
		recText := strconv.FormatInt(rv.Unix(), 10)
		for ph := time.Duration(0); ph < min; ph += 7*sec + 500*ms {
			s := Span{From: base.Add(72*time.Hour + ph - 2*time.Hour), To: base.Add(72*time.Hour + ph)}
			read := s.To.Add(-ms)
			if got, why := v5Check(p, o, recText, read.Truncate(min).Add(-time.Hour), s); !got {
				t.Errorf("FALSE RED: learned at a call %v into its minute, a replay %v into its minute, the application's value: %s", phase, ph, why)
			}
			if got, _ := v5Check(p, o, recText, read.Truncate(min), s); got {
				t.Errorf("FALSE GREEN: learned at a call %v into its minute, a replay %v into its minute passes a dropped look-back", phase, ph)
			}
		}
	}
	if len(verdicts) > 1 || len(verdicts[false]) > 0 {
		t.Errorf("the verdict turns on where in its minute the recording's call fell, or refuses it: learned at %v, refused at %v; want it learned at every phase, checked at the cut", verdicts[true], verdicts[false])
	}
}

// The uncompared look-back's refusal at its exact bound: in two calls whose
// windows start on a whole second, now - 2s (an epoch's seconds) stands
// from 2.061 s before its call up to, not including, exactly one second
// before its window's start: a range that ends exactly one cut back and
// holds only the whole cut of 2 s, more than one cut. Where the call's
// replay does not compare offsets it is refused, naming that, never left
// to the test's begin.
func TestALookBackOfTwoCutsEndingExactlyOneCutBackIsRefusedWhereNotCompared(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	const p = "body.filter.dirtyAt.$lte"
	call := time.Date(2026, 10, 31, 9, 0, 0, 61e6, time.UTC)
	fcall := call.Add(62 * sec)
	span := func(at time.Time) Span { return Span{From: at.Add(-61 * ms), To: at, Began: at.Add(-20 * sec)} }
	rv, lv := call.Add(-4*ms).Truncate(sec).Add(-2*sec), fcall.Add(-4*ms).Truncate(sec).Add(-2*sec)
	compared := (&Window{Rec: span(call), F: span(fcall)}).ComparesOffsets()
	if o, _, _, ok := offsetV5(v5Instant(rv, sec), v5Instant(lv, sec), compared, nil); !ok || o.Max != -sec || cutHi(o) != -2*sec {
		t.Fatalf("the look-back's range: %+v %v; want it up to, not including, -1s, its one whole cut -2s", o, ok)
	}
	o, why, ok := clockInWindow(p, v5Instant(rv, sec), v5Instant(lv, sec), &Window{Rec: span(call), F: span(fcall)}, nil)
	if ok || !strings.Contains(why, "this protocol's replay does not compare such an offset yet") {
		t.Errorf("FALSE GREEN: now - 2s ending exactly one cut back, where a replay does not compare offsets: %+v %q %v; want it refused, naming that", o, why, ok)
	}
}
