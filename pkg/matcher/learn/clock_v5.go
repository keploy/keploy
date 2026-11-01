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
// window, plus d, at its value cut to the cut both values show) with three
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
//   - A look-back cut to a second or coarser whose window is wider than its
//     cut (load, or an idle poll: the window runs from the previous call)
//     is read as the read nearest its call (nearestCall), as a window
//     narrower than the cut reads it, so a verdict never flips with load.
//
// It refuses what main's clockOffset refuses for its unit (Fine: an offset
// that reads in a second or finer, but one cut to the second; why) and for
// a window that no replay of its width could check (Checkable). The other
// guards (one no replay compares, one too wide to narrow, one near a wall
// time shown twice) are the order's, as for any reading.
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
	case o.Fine():
		return models.ClockOffset{}, fineV5Why(path, o), false
	case !checkable:
		return models.ClockOffset{}, clockSeenV5(path, o) + ", but a replay in a window as wide as its call's (" + models.ShortDuration(max(w.Rec.To.Sub(w.Rec.From), w.F.To.Sub(w.F.From))) + ") could not tell " + offsetNamed(o) + " changed by " + aUnit(o.Unit()) + " from it, so keploy refuses it" + noiseEdit, false
	case w.begun:
		o.Since = models.SinceTestBegin
		o.Why = sinceLineV5(path, o)
		return o, "", true
	}
	o.Why = lineV5(path, o, enc, gap, nearLine(o, w, near))
	return o, "", true
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
	switch {
	case ln.Kind == KindClockRead && ln.Read != nil:
		ln.Read.Since = models.SinceTestBegin
	case ln.Kind == KindClockOffset && ln.Offset != nil && (ln.Offset.Plain() || ln.Offset.Min > 0):
		ln.Offset.Since = models.SinceTestBegin
	default:
		return ln, false
	}
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
	if n, nok := nearestCall(r, l, w, cut); nok {
		switch lo, hi := o.CutOffsets(); {
		case !ok:
			o, ok, near = n, true, "a read with no look-back"
		case o.Max <= 0 && hi-lo >= 2*cut:
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
	return o, 0, near, ok
}

// nearestCall is the look-back r and l stand at, cut to cut (a second or
// coarser), from the read of each run's clock nearest its call: the offsets
// from the greatest of v less each run's call (Min) up to one cut above it,
// so the one whole cut they hold is the latest one both runs' windows put
// a read in. ok only where that is a look-back in both runs. A window
// narrower than its cut holds that cut, or it and the next (a turn of the
// cut in both runs); a wider one (the window runs from the previous call,
// so load or an idle poll widens it) holds more, or a read with no
// look-back too (dirtyAt <= now - 2s in a window 2.5 s wide), which the
// reading would read as three or more cuts, checked as one, or leave to a
// plain read that a quiet replay's window fails. An application that cuts
// its read and subtracts a look-back writes a value in the cut nearest its
// call, less the look-back, unless it read its clock a whole cut or more
// before its call in both runs: that is read as a look-back so much
// longer, which a replay passes where the application waits as long again
// (a plain read made 1.2 s before its call reads as clock - 1s). Of a cut
// finer than a second the lead itself shows at the cut, so ok is false.
func nearestCall(r, l typedInstant, w *Window, cut time.Duration) (models.ClockOffset, bool) {
	rlo, rhi, rok := offsetBounds(r.at, cut, w.Rec)
	llo, lhi, lok := offsetBounds(l.at, cut, w.F)
	if cut < time.Second || !rok || !lok || rlo >= 0 || llo >= 0 {
		return models.ClockOffset{}, false
	}
	lo := max(rlo, llo)
	o := models.ClockOffset{Min: lo, Max: min(lo+cut, rhi, lhi), Res: cut, Layout: models.LayoutTyped, Step: cut}
	return o, o.Min < o.Max && o.Max < 0
}

// nearLine is the sentence a line of o, read by nearestCall where its
// window held near, inserts: "" where it was not.
func nearLine(o models.ClockOffset, w *Window, near string) string {
	if near == "" {
		return ""
	}
	u := unitWord(o.Res)
	return "; its call's window (" + models.ShortDuration(w.Rec.To.Sub(w.Rec.From)) + " in the recording, " + models.ShortDuration(w.F.To.Sub(w.F.From)) + " in record's full round) also held " + near + ", as a window widened by load or an idle poll does, so keploy reads the look-back of the read nearest its call, the latest whole " + u + " both runs' windows hold, as a window narrower than " + aUnit(o.Res) + " reads it: a read the application made a whole " + u + " or more before its call in both runs is read that much longer, and a replay passes it only where the application waits about as long again"
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

// noiseEdit is the edit every refusal and line of a clock offset names.
const noiseEdit = "; if the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"

// clockSeenV5 is what the learn step saw: "expires_at = clock + 30m ...".
func clockSeenV5(path string, o models.ClockOffset) string {
	return path + " = " + o.String() + ": in the recording and in record's full round alike, it is a read of the application's clock in its call's window plus that offset"
}

// fineV5Why is why an offset Fine for its unit is refused (main's
// clockOffsetFineWhy, and that one cut to the second is checked).
func fineV5Why(path string, o models.ClockOffset) string {
	u := unitWord(o.Unit())
	return clockSeenV5(path, o) + ", but the offset reads in " + u + "s and its values at a resolution of " + models.ShortDuration(o.Res) + ": a read the application made outside its call's window, " + u + "s before or after it, shows such an offset too, which a replay would check to " + models.ShortDuration(o.Res) + " and fail as soon as that wait changed, and an application that cuts its value to the " + u + " makes " + offsetNamed(o) + " changed by " + aUnit(o.Unit()) + " look the same, so keploy refuses an offset that reads in a second or finer unless its values are cut to a second or a coarser unit, which a replay checks at the cut" + noiseEdit
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
		return seen + ". Its two values stand " + models.ShortDuration(gap) + " apart, a whole number of " + st + ", and share their remainder within it, so keploy reads the application as cutting its read to " + st + " and then subtracting the look-back. A replay passes a value only where it is a whole " + st + " of its call's window (within " + c + " on either side) plus " + o.Range() + ": a look-back dropped, or changed by part of " + st + ", fails at every read, and so does one changed by a whole " + st + " unless the window holds a turn of the " + unitWord(o.Step) + "; an application that does not cut and wrote these two values by chance fails a replay wherever its value's remainder within " + st + " moves (a false red), and passes one where that remainder lands on the learned one, about one read in " + strconv.FormatInt(int64(o.Step/o.Res), 10) + ", whatever its look-back" + noiseEdit
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
		_, on := cutExceptions(o.Res)
		return seen + via + near + ". A replay checks it at the " + w + " against " + cutOffsetsV5(o, lo, hi) + ", as standing for its whole " + w + ", within one " + w + " of its call's window plus that offset on either side, whatever the window's width: " + offsetNamed(o) + " changed by one " + w + " passes there at every read, and one changed by more " + w + "s only where the replay's call's window holds a turn of the " + w + " for each " + w + " more, " + dir + ", so in a window narrower than the " + w + " one changed by two " + w + "s passes only where a turn of the " + w + " falls in it, and one changed by three " + w + "s or more fails" + on + noiseEdit
	}
	// Holds checks a value from one Res before the cut that holds the
	// window's start plus Min up to one Res after its end plus Max: a read
	// r in the window plus d passes at every read for d from Min - Res to
	// Max + Res, at some reads (r late or early in the window and its cut)
	// down to one Res and the window's width below that or up to the
	// window's width above it, and at no read further out.
	lo, hi := models.ShortDuration(o.Min-o.Res), models.ShortDuration(o.Max+o.Res)
	return seen + via + near + ". A replay checks a value written as recorded at " + c + ": it passes one from one " + c + " before the start of the " + c + " that holds its call's window's start plus " + models.ShortDuration(o.Min) + " up to one " + c + " after the window's end plus " + models.ShortDuration(o.Max) + ". So a read in that window plus an offset from " + lo + " to " + hi + " passes at every read; plus an offset from one " + c + " and the window's width below " + lo + " up to it, or from " + hi + " up to the window's width above it, only where the read falls late or early enough in the window and its " + c + "; and plus any other offset (" + offsetNamed(o) + " changed by more) at no read; near a wall time the application's zone shows twice as its clocks go back a replay refuses the value" + noiseEdit
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
	return path + " = " + o.String() + ": in the recording and in record's full round alike, it is a read of the application's clock made after its test began and before its call, plus a time to live, cut to " + aUnit(o.Res) + ". Its call's window alone shows an offset that measures how long the application waited between its read and its call as much as what it added, so keploy checks it from the test's begin: a replay passes a value from one " + c + " before the " + c + " that holds its test's begin plus " + models.ShortDuration(lo) + ", up to one " + c + " after its call plus " + models.ShortDuration(hi) + ", checked at " + c + ", however long the application waited between its read and its call; so a time to live changed by less than the test's span plus " + models.ShortDuration(hi-lo) + " can pass too, and one changed by more fails" + noiseEdit
}
