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
// own second holds both (or one does, of no whole second: reads 700 ms
// apart in their minutes, 7.5 s and 8.2 s), is read at the minute its two
// values stand a
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
