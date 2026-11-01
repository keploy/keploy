package learn

// The Clock of a changed instant that its call's window holds (the order's
// step 5), as a replay checks it. It replaces main's offsetReading on the
// one entry (Decide) alone: main's chain keeps its own reading until it is
// deleted, and every offset read here carries models.ClockOffset.Step,
// which main's chain never writes, so an old recording checks exactly as
// on main and a keploy that does not read Step holds nothing of a new one.

import (
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func init() {
	clockInWindow = offsetInWindow
	clockSinceBegin = sinceBeginV5
}

// offsetInWindow is the offset r and l, an instant as recorded and as F
// saw it, stand at from a read of each run's clock in its call's window w,
// with its Step and its Why (the learned line: what a replay passes), or
// why it is refused (a non-empty why with ok false), or none at all. It is
// main's offsetReading (the offsets d that put some read in each run's
// window, plus d, at its value cut to the cut both values show) with four
// changes:
//   - Where the two values stand a whole number of a coarser step apart
//     (models.CutOf), an application that cuts its value to that step and
//     adds an offset of no whole number of it writes such values too, so
//     the offset is read at the step where one holds both runs there, and
//     is never refused as ambiguous (founder decision, 2026-10-27: a cut is
//     checked, never refused). A look-back the values' own cut puts in both
//     runs is the exception, read there (ownLookBack): at the step it would
//     stand for the whole step and pass a dropped look-back.
//   - A look-back read at the step is locked to the phase both runs share
//     (phaseAt): the application cut its read to the step, then subtracted
//     the offset, so a replay passes only that remainder within the step.
//     A real cutter passes at every read; a dropped look-back, or one
//     changed by part of a step, fails at every read; an application that
//     does not cut and wrote two such values by chance (one recording in
//     some sixty, of seconds a whole minute apart) fails a replay where its
//     read falls at another remainder (a false red), and where its read
//     lands on the learned one (a read in sixty) passes whatever its
//     look-back, a dropped one too: never at every read.
//   - An offset cut to a second or a minute that no unit holds three of
//     (clock - 2s, an epoch's seconds: a debounce or a stale scan's cutoff)
//     is learned whatever its width and checked at the cut against the
//     whole cuts its range holds (models.ClockOffset.CutOffsets), as main
//     checks a day's or an hour's.
//   - A look-back whose window holds more than one whole cut of it, or a
//     read with no look-back too (load, or an idle poll: the window runs
//     from the previous call), is read as the read nearest its call
//     (nearestCall), as a window narrower than the cut reads it, so a
//     verdict never flips with load.
//
// It refuses what main's clockOffset refuses for its unit (Fine: an offset
// that reads in a second or finer, but one cut to the second; why) and for
// a window that no replay of its width could check (Checkable). An offset
// Fine at a cut of a second or coarser holds no whole cut, so no application
// that cuts its read and adds a whole cut wrote it: where the window from
// each run's test begin holds the value as a plain read (heldSinceBegin) it
// is left to the order's step 7, which a refusal here would keep from
// reading it; so is any offset of a cut that coarse that this protocol's
// replay does not compare (Window.ComparesOffsets), which the order could
// only refuse. A look-back Fine at a finer cut stays refused where it stands
// a second or more before its call in both runs (Min, the read nearest it):
// read from the test's begin it would pass a dropped look-back. Nearer than
// that it is a plain read's ordinary lead too, and is left to step 7, whose
// check goes red where the value is no read of the clock, and which passes
// a look-back under a second dropped or changed, stated. A time to live under a
// minute at the second is refused, as main refuses every one, where its
// window is as wide as its cut (ttlTooWide). The other guards (one no replay
// compares, one too wide to narrow, one near a wall time shown twice) are
// the order's, as for any reading.
//
// Since its test began (the 2026-10-31 ruling on a time to live):
//   - A time to live (Min above zero) that its call's window holds at a
//     cut of the second or the minute (CoarseCut: clock + 29d23h59m57s,
//     read three seconds before its call plus thirty days), or refuses
//     (Fine: clock + 29d23h59m56.3s to 56.9s; or not Checkable), measures
//     how long the application waited between its read and its call as
//     much as what it added, so a replay whose wait changed by two seconds
//     would fail it or pass it by where its reads fell in their seconds,
//     or no replay passes it. Where the window from each run's test begin
//     to its call
//     (Window.sinceBegin: client tests) holds it as the clock plus an
//     offset in both runs (clock + 30d), that offset is learned instead,
//     checked from the test's begin (models.SinceTestBegin): it holds the
//     application's value however long the replay waited.
//   - Read in that window itself (w.begun: the order's step 7, through
//     clockSinceBegin), only a time to live is learned: a look-back checked
//     from the test's begin would pass one changed by up to the test's span
//     (and a dropped one), so it is declined, never refused: the order
//     goes on as if there were no such read.
func offsetInWindow(path string, r, l typedInstant, w *Window, zone *time.Location) (models.ClockOffset, string, bool) {
	o, gap, near, ok := offsetV5(r, l, w, zone)
	if !ok || w.begun && o.Min <= 0 {
		return models.ClockOffset{}, "", false
	}
	enc := max(r.res, l.res)
	checkable := o.Checkable(enc, w.Rec.From, w.Rec.To) && o.Checkable(enc, w.F.From, w.F.To)
	if wb := w.sinceBegin(); wb != nil && o.Min > 0 && (o.Fine() || o.CoarseCut() || !checkable) {
		if ob, _, bok := offsetInWindow(path, r, l, wb, zone); bok {
			return ob, "", true
		}
	}
	switch {
	case (o.Fine() || !w.offsets) && (o.Res >= time.Second || o.Min > -time.Second) && heldSinceBegin(r, l, w, enc):
		return models.ClockOffset{}, "", false
	case o.Fine():
		return models.ClockOffset{}, fineV5Why(path, o, w, near), false
	case ttlTooWide(o, w):
		return models.ClockOffset{}, ttlWideWhy(path, o, w), false
	case !checkable:
		return models.ClockOffset{}, clockSeenV5(path, o) + ", but a replay in a window as wide as its call's (" + models.ShortDuration(max(w.Rec.To.Sub(w.Rec.From), w.F.To.Sub(w.F.From))) + ") could not tell " + offsetNamed(o) + " changed by " + aUnit(o.Unit()) + " from it, so keploy refuses it" + clockNoiseLine, false
	case w.begun:
		o.Since = models.SinceTestBegin
		o.Why = sinceLineV5(path, o)
		return o, "", true
	}
	o.Why = lineV5(path, o, enc, gap, nearLine(o, w, near))
	return o, "", true
}

// heldSinceBegin reports whether the window from each run's test begin to
// its call (Window.sinceBegin: client tests) holds r and l, written at
// enc, as plain reads of the clock.
func heldSinceBegin(r, l typedInstant, w *Window, enc time.Duration) bool {
	wb := w.sinceBegin()
	return wb != nil && wb.Rec.holds(r.at, enc) && wb.F.holds(l.at, enc)
}

// sinceBeginV5 is clockSinceBegin: ln, a Clock the order's step 7 found in
// the window from each run's test begin to its call, checked from its
// test's begin (models.SinceTestBegin): a plain read, or the clock plus an
// offset that lies after the call in both runs (a time to live added to an
// early read, which offsetInWindow reads in that window). A look-back is
// declined (ok false), as offsetInWindow declines one there: checked from
// the test's begin it would pass a look-back changed by up to the test's
// span. ok is false for anything else.
func sinceBeginV5(ln Learned) (Learned, bool) {
	if ln.Kind != KindClockOffset || ln.Offset == nil || !ln.Offset.Plain() && ln.Offset.Min <= 0 {
		return ln, false
	}
	ln.Offset.Since = models.SinceTestBegin
	return ln, true
}

// offsetV5 is offsetInWindow's reading, before its guards: the offset, with
// its Step, gap, the values' gap where it set the step the offset was read
// at (0 where their own cut did), and near, the kind of reading
// nearestCall replaced ("" where it did not).
func offsetV5(r, l typedInstant, w *Window, zone *time.Location) (o models.ClockOffset, gap time.Duration, near string, ok bool) {
	if w == nil || r.names != namesMoment || l.names != namesMoment || !inClockRange(r.at) || !inClockRange(l.at) || r.at.Equal(l.at) {
		return models.ClockOffset{}, 0, "", false
	}
	enc := max(r.res, l.res)
	cut := zonedRes(enc, r.at, l.at, false, zone)
	o, ok = offsetAt(r, l, w, cut)
	o.Step = cut
	n, nok := nearestCall(r, l, w, cut)
	if nok && cut >= time.Second {
		switch lo, hi := o.CutOffsets(); {
		case !ok:
			o, ok, near = n, true, noLookBack
		case hi > lo:
			o, near = n, "more whole "+unitWord(cut)+"s of look-back"
		}
	}
	g := l.at.Sub(r.at)
	if step := models.CutOf(int64(g), enc, 24*time.Hour); step > cut {
		so, sok := offsetAt(r, l, w, step)
		switch {
		case ownLookBack(o, ok, cut):
		case sok && so.Max > 0:
			// A time to live: the step's check holds both the writer that
			// cuts to the step and adds it, and one that does not cut.
			so.Step = step
			return so, g.Abs(), "", true
		default:
			if p, pok := phaseAt(r, l, w, step, cut, zone); pok {
				return p, g.Abs(), "", true
			}
		}
	}
	if nok && !ok {
		// A cut finer than a second that no step locks: the guards then
		// refuse it, as they refuse it in a quiet window.
		return n, 0, noLookBack, true
	}
	return o, 0, near, ok
}

// noLookBack is what a window that holds a value as a plain read too held
// (nearLine, nearFine).
const noLookBack = "a read with no look-back"

// nearestCall is the look-back r and l stand at, cut to cut, from the read
// of each run's clock nearest its call: the offsets from the greatest of v
// less each run's call (Min) up to one cut, or a second where the cut is
// finer, above it, so the one whole cut, or second, they hold is the latest
// one both runs' windows put a read in. ok only where they hold one and it
// is a look-back in both runs: one run's wide window beside the other's
// narrow one may leave no whole cut, and no read nearest its call stands
// there.
//
// The reading of a window holds every whole cut of look-back from its call
// back to its start, and a read with no look-back as well where it runs
// back past the look-back (dirtyAt <= now - 2s in a window 2.5 s wide). Two
// whole cuts already pass a dropped look-back at every read (the check runs
// a cut past the later one), and they come of a window 400 ms to 1.5 s wide
// as often as of a turn of the cut, which a window's width cannot tell
// apart, so wherever a reading holds more than one the read nearest its
// call is the one (hi > lo in offsetV5). An application that cuts its read
// and subtracts a look-back writes a value in the cut nearest its call,
// less the look-back, unless it read its clock a whole cut or more before
// its call in both runs, or a turn of the cut fell between its read and its
// call in both. The turn reads one cut longer, which the check's one cut of
// slack passes. The long read reads as a look-back so much longer, which a
// replay passes where the application waits as long again (a plain read
// made 1.2 s before its call reads as clock - 1s).
//
// Of a cut finer than a second a window that holds the value at all is
// read here (offsetV5's last step) only to ask what a quiet window would
// say: Fine (now - 2s at the millisecond: refused), or too wide for its
// unit. A value a second or more before its call in both runs is refused so
// in any window, a plain read made that long before its call too (the
// rule's stated cost, fail closed): read as a plain read, it fails the
// application's value on a quiet replay and passes a dropped look-back,
// and which of the two a recording got would turn on load. Under a second
// a look-back cannot be told from a plain read's ordinary lead: a window
// wider than it reads it as the plain read.
func nearestCall(r, l typedInstant, w *Window, cut time.Duration) (models.ClockOffset, bool) {
	rlo, rhi, rok := offsetBounds(r.at, cut, w.Rec)
	llo, lhi, lok := offsetBounds(l.at, cut, w.F)
	if !rok || !lok {
		return models.ClockOffset{}, false
	}
	grain := max(cut, time.Second)
	lo := max(rlo, llo)
	o := models.ClockOffset{Min: lo, Max: min(lo+grain, rhi, lhi), Res: cut, Layout: models.LayoutTyped, Step: cut}
	return o, o.Max < 0 && o.Unit() >= grain
}

// nearLine is the sentence a line of o, read by nearestCall where its
// window held near, inserts: "" where it was not.
func nearLine(o models.ClockOffset, w *Window, near string) string {
	if near == "" {
		return ""
	}
	u := unitWord(o.Res)
	return "; its call's window (" + windowsWide(w) + ") also held " + near + ", as a window widened by load or an idle poll, or one that holds a turn of the " + u + " in both runs, does, so keploy reads the look-back of the read nearest its call, the latest whole " + u + " both runs' windows hold, as a window narrower than " + aUnit(o.Res) + " reads it: a read the application made a whole " + u + " or more before its call in both runs is read that much longer, and a replay passes it only where the application waits about as long again"
}

// nearFine is the sentence a refusal of o inserts where its window held
// near: "" where it did not.
func nearFine(w *Window, near string) string {
	if near == "" {
		return ""
	}
	return ", and its call's window (" + windowsWide(w) + ") also held " + near + ", as a window widened by load or an idle poll does, but the value stands a second or more before its call in both runs, so keploy reads it as a quiet window reads it, a plain read made that long before its call too"
}

// windowsWide names how wide w is in each run.
func windowsWide(w *Window) string {
	return models.ShortDuration(w.Rec.To.Sub(w.Rec.From)) + " in the recording, " + models.ShortDuration(w.F.To.Sub(w.F.From)) + " in record's full round"
}

// ttlTooWide reports whether o is a time to live under a minute at the
// second in a window as wide as its cut in both runs (one run's narrow
// window narrows the range to its own). Main refuses every time to live that reads in seconds. A
// narrow window's check tells one from the same changed by two seconds, but
// here every whole second of the range stands for it: a replay would pass
// one changed by the window's width at every read, and no read nearest its
// call tells it, as it tells a look-back, because a time to live the
// application added to a read made a second earlier reads a second
// shorter. One of a minute or longer is read at its cut, and its line says
// how far a changed one passes (a verdict's width the window sets, on an
// offset far longer than the window).
func ttlTooWide(o models.ClockOffset, w *Window) bool {
	return o.Min > 0 && o.Max <= time.Minute && o.Res == time.Second && o.CoarseCut() && min(w.Rec.To.Sub(w.Rec.From), w.F.To.Sub(w.F.From)) >= o.Res
}

// ttlWideWhy is why ttlTooWide refuses o.
func ttlWideWhy(path string, o models.ClockOffset, w *Window) string {
	return clockSeenV5(path, o) + ", but it is a time to live under a minute, read in a window (" + windowsWide(w) + ") as wide as the second its values are cut to: every whole second of that range stands for it, and a replay would pass one changed by that many seconds at every read, so keploy refuses it" + clockNoiseLine
}

// ownLookBack reports whether o, the offset the values' own cut puts in
// both runs (ok), is a look-back read there, never at its values' step: at
// a millisecond's cut or finer, any look-back (its own unit then decides:
// below a second it is Fine, and refused, as main refuses every such
// look-back); at a second's cut or coarser, one that reads in its cut's
// unit, as an application that does not cut shows it (now - 90s: clock -
// 1m30s at the second). One that reads finer than its cut is the step
// writer's (cut_m(now) - 103s, both runs read at one phase of their
// minutes), read at the step.
func ownLookBack(o models.ClockOffset, ok bool, cut time.Duration) bool {
	return ok && o.Max <= 0 && (cut < time.Second || !o.Fine())
}

// phaseAt is the look-back r and l stand at from a read of each run's
// clock in w cut to step, then less the offset (the phase lock): the
// offsets v - k, k a whole step that some read in the run's window cuts
// to, common to both runs. Their values stand a whole number of steps
// apart, so they share their remainder within the step and the offsets of
// each run lie on one grid. Min is the least common offset and Max the
// greatest plus cut, the values' own cut; ok only where both runs share one
// and it lies below zero, a look-back in both.
//
// A minute's step or a finer one is one grid in every zone (zones' offsets
// are whole minutes). An hour's or a day's is not: an application in
// +05:30 that cuts to its own hour (Java's truncatedTo) keeps one remainder
// on its zone's grid, one in Go (Truncate counts from UTC) on UTC's. So
// where keploy names the application's zone (zone), the grid is read in
// UTC and in that zone, and AppZone names it, as a replay reads its value
// on both grids (checkReadIn), as it reads a plain read's cut (cutZones);
// where both hold a look-back, the shorter: right for any look-back
// shorter than the two grids stand apart (thirty minutes for +05:30's
// hour), which an application that subtracts more within the step may
// write a false red of, where a replay reads in the other part of it.
func phaseAt(r, l typedInstant, w *Window, step, cut time.Duration, zone *time.Location) (models.ClockOffset, bool) {
	grids, name := []*time.Location{time.UTC}, ""
	if step >= time.Hour && zone != nil {
		if name = models.ZoneNameOf(zone, r.at); name != "" {
			grids = append(grids, zone)
		}
	}
	var best models.ClockOffset
	found := false
	for _, g := range grids {
		rlo, rhi, rok := phaseRange(r.at, w.Rec, step, g)
		llo, lhi, lok := phaseRange(l.at, w.F, step, g)
		o := models.ClockOffset{Min: max(rlo, llo), Max: min(rhi, lhi) + cut, Res: cut, Layout: models.LayoutTyped, Step: step, AppZone: name}
		if rok && lok && o.Min < o.Max && o.Max <= 0 && (!found || o.Max > best.Max) {
			best, found = o, true
		}
	}
	return best, found
}

// phaseRange is the least and greatest of v less each whole step (on
// grid's clock) that a read in s cuts to.
func phaseRange(v time.Time, s Span, step time.Duration, grid *time.Location) (lo, hi time.Duration, ok bool) {
	if s.From.IsZero() || s.To.IsZero() || s.To.Before(s.From) || step <= 0 {
		return 0, 0, false
	}
	first, last := models.ReadCut(s.From, step, grid), models.ReadCut(s.To, step, grid)
	return v.Sub(last), v.Sub(first), true
}

// clockNoiseLine is the edit every refusal and line of a clock offset names.
const clockNoiseLine = "; if the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"

// clockSeenV5 is what the learn step saw: "expires_at = clock + 30m ...".
func clockSeenV5(path string, o models.ClockOffset) string {
	return path + " = " + o.String() + ": in the recording and in record's full round alike, it is a read of the application's clock in its call's window plus that offset"
}

// fineV5Why is why an offset Fine for its unit is refused (main's
// clockOffsetFineWhy, and that one cut to the second is checked).
func fineV5Why(path string, o models.ClockOffset, w *Window, near string) string {
	u := unitWord(o.Unit())
	return clockSeenV5(path, o) + nearFine(w, near) + ", but the offset reads in " + u + "s and its values at a resolution of " + models.ShortDuration(o.Res) + ": a read the application made outside its call's window, " + u + "s before or after it, shows such an offset too, which a replay would check to " + models.ShortDuration(o.Res) + " and fail as soon as that wait changed, and an application that cuts its value to the " + u + " makes " + offsetNamed(o) + " changed by " + aUnit(o.Unit()) + " look the same, so keploy refuses an offset that reads in a second or finer unless its values are cut to a second or a coarser unit, which a replay checks at the cut" + clockNoiseLine
}

// lineV5 is the Why of a Clock o learned in its call's window: what was
// seen, and what a replay passes (models.ClockOffset.Holds), truly. gap is
// the values' gap where it set the step o was read at; near, nearLine's
// sentence where nearestCall read o.
func lineV5(path string, o models.ClockOffset, enc, gap time.Duration, near string) string {
	seen := clockSeenV5(path, o)
	c := models.ShortDuration(o.Res)
	if o.Step > o.Res {
		st := models.ShortDuration(o.Step)
		return seen + ". Its two values stand " + models.ShortDuration(gap) + " apart, a whole number of " + st + ", and share their remainder within it, so keploy reads the application as cutting its read to " + st + " and then subtracting the look-back. A replay passes a value only where it is a whole " + st + " of its call's window (within " + c + " on either side) plus an offset from " + models.ShortDuration(o.Min) + " to " + models.ShortDuration(o.Max) + ": a look-back dropped, or changed by part of " + st + ", fails at every read, and so does one changed by a whole " + st + " unless the window holds a turn of the " + unitWord(o.Step) + "; an application that does not cut and wrote these two values by chance fails a replay wherever its value's remainder within " + st + " moves (a false red), and passes one where that remainder lands on the learned one, about one read in " + strconv.FormatInt(int64(o.Step/o.Res), 10) + ", whatever its look-back" + clockNoiseLine
	}
	via := ", cut to " + aUnit(o.Res)
	if gap > 0 && o.Res > enc {
		via = ", checked at " + aUnit(o.Res) + "'s cut, the step its two values stand " + models.ShortDuration(gap) + " apart (their digits show no such cut; an application that cuts its value to " + c + " and adds a time to live that is no whole number of it writes such values, and so, by chance, can one that does not cut)"
	}
	if lo, hi := o.CutOffsets(); o.CoarseCut() {
		w := unitWord(o.Res)
		dir := "between the application's read and its call (a longer one) or between the window's start and the read (a shorter one)"
		if o.Max <= 0 {
			dir = "between the window's start and the application's read (a longer one) or between the read and its call (a shorter one)"
		}
		changed := offsetNamed(o) + " changed by one " + w + " passes there at every read, and one changed by more " + w + "s"
		next, later := "one changed by two "+w+"s", "one changed by three "+w+"s or more"
		if hi > lo && hi != o.Max {
			changed = offsetNamed(o) + " from " + models.ShortDuration(lo-o.Res) + " to " + models.ShortDuration(hi+o.Res) + " passes at every read, and one further out"
			next, later = "one a "+w+" further out", "one two "+w+"s further out or more"
		}
		_, on := cutExceptions(o.Res)
		return seen + via + near + ". A replay checks it at the " + w + " against " + cutOffsetsV5(o, lo, hi) + ", as standing for its whole " + w + ", within one " + w + " of its call's window plus that offset on either side, whatever the window's width: " + changed + " only where the replay's call's window holds a turn of the " + w + " for each " + w + " more, " + dir + ", so in a window narrower than the " + w + " " + next + " passes only where a turn of the " + w + " falls in it, and " + later + " fails" + on + clockNoiseLine
	}
	// Holds checks a value from one Res before the cut that holds the
	// window's start plus Min up to one Res after its end plus Max: a read
	// r in the window plus d passes at every read for d from Min - Res to
	// Max + Res, at some reads (r late or early in the window and its cut)
	// down to one Res and the window's width below that or up to the
	// window's width above it, and at no read further out.
	lo, hi := models.ShortDuration(o.Min-o.Res), models.ShortDuration(o.Max+o.Res)
	return seen + via + near + ". A replay checks a value written as recorded at " + c + ": it passes one from one " + c + " before the start of the " + c + " that holds its call's window's start plus " + models.ShortDuration(o.Min) + " up to one " + c + " after the window's end plus " + models.ShortDuration(o.Max) + ". So a read in that window plus an offset from " + lo + " to " + hi + " passes at every read; plus an offset from one " + c + " and the window's width below " + lo + " up to it, or from " + hi + " up to the window's width above it, only where the read falls late or early enough in the window and its " + c + "; and plus any other offset (" + offsetNamed(o) + " changed by more) at no read; near a wall time the application's zone shows twice as its clocks go back a replay refuses the value" + clockNoiseLine
}

// cutOffsetsV5 names the whole cuts lo to hi a replay checks o against:
// "clock - 2s" for one, "clock plus each whole second from -3s to -2s" for
// two or more, o's range where it holds none.
func cutOffsetsV5(o models.ClockOffset, lo, hi time.Duration) string {
	switch {
	case hi == o.Max:
		return o.Range()
	case lo == hi && lo < 0:
		return "clock - " + models.ShortDuration(-lo)
	case lo == hi:
		return "clock + " + models.ShortDuration(lo)
	}
	return "clock plus each whole " + unitWord(o.Res) + " from " + models.ShortDuration(lo) + " to " + models.ShortDuration(hi)
}

// sinceLineV5 is the Why of a time to live o learned in the window from
// each run's test begin to its call (models.SinceTestBegin): what was
// seen, and what a replay passes (models.ClockOffset.Holds from the test's
// begin), truly.
func sinceLineV5(path string, o models.ClockOffset) string {
	c := models.ShortDuration(o.Res)
	lo, hi := o.Min, o.Max
	if o.CoarseChecked(o.Res) {
		lo, hi = o.CutOffsets()
	}
	return path + " = " + o.String() + ": in the recording and in record's full round alike, it is a read of the application's clock made after its test began and before its call, plus a time to live, cut to " + aUnit(o.Res) + ". Its call's window alone shows an offset that measures how long the application waited between its read and its call as much as what it added, so keploy checks it from the test's begin: a replay passes a value from one " + c + " before the " + c + " that holds its test's begin plus " + models.ShortDuration(lo) + ", up to one " + c + " after its call plus " + models.ShortDuration(hi) + ", checked at " + c + ", however long the application waited between its read and its call; so a time to live changed by less than the test's span plus " + models.ShortDuration(hi-lo) + " can pass too, and one changed by more fails" + clockNoiseLine
}
