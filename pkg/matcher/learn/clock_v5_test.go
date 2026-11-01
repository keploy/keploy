package learn

import (
	"reflect"
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
		for _, part := range []string{"read cut to 1m0s before the offset", "A replay passes a value only where it is a whole 1m of its call's window", "a look-back dropped, or changed by part of 1m, fails at every read", "fails a replay wherever its value's remainder within 1m moves (a false red), and passes one where that remainder lands on the learned one, about one read in 60, whatever its look-back"} {
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
		case !ok || o.Step != sec || o.Res != ms || o.Min != -1500*ms || o.Max != -1499*ms:
			t.Errorf("round F %s later in its second: %+v %q %v; want clock - 1.5s locked to the second, or main's refusal", d, o, why, ok)
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
// range holds stands for it, so a replay would pass one changed by the
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
		if want := "a time to live from " + sh(lo-sec) + " to " + sh(hi+sec) + " passes at every read"; !strings.Contains(o.Why, want) || !strings.Contains(o.Why, "each whole second from "+sh(lo)+" to "+sh(hi)) {
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
// width wide in both runs. Where the order is not on this tree (Decide then
// refuses every word) the row is skipped: it runs wherever the two meet.
func v5Decide(t *testing.T, path string, typ Type, rec, live string, rAt, fAt time.Time, width time.Duration) Decision {
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
		if !strings.Contains(why, "against "+row.o.Range()+", as standing for") || !strings.Contains(row.o.Range(), row.named) || !strings.Contains(why, "on either side, whatever the window's width") {
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
