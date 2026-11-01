package learn

import (
	"context"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A value the learn step learns as a read of the application's clock (a
// plain read, Learned.Read, or one plus a constant offset, KindClockOffset)
// is still compared at a replay: a caller writes it to models.MockSpec.Clocks
// (Learned.Clock), and its replay checks the value its call carries there is
// such a read in the call's own span on the application's clock
// (ClockSpanFrom, CheckClockRead), refusing it otherwise with a reason.
// A replay that cannot tell the application's clock at the call refuses it
// too: a value checked against no clock would pass whatever it is.

// Clock is what a replay checks of ln's leaf, as models.MockSpec.Clocks
// holds it: the offset of a KindClockOffset, the plain read of a
// KindClockRead (Read). ok is false for any other leaf: a KindNoise never
// carries a read, so a caller that writes its noise leaves to the noise a
// replay skips never stops comparing a read of the clock.
func (ln Learned) Clock() (models.ClockOffset, bool) {
	switch {
	case ln.Kind == KindClockOffset && ln.Offset != nil:
		return *ln.Offset, true
	case ln.Kind == KindClockRead && ln.Read != nil:
		return *ln.Read, true
	}
	return models.ClockOffset{}, false
}

// clockReadUncheckedWhy is a KindClockRead's Why (read after "<field>
// changed, and "): what a caller whose replay checks no read of the clock
// refuses it with, where keploy learned it as noise before it learned a read
// (the lead moved it, or a window held it). One keploy refused before it
// learned it as a read keeps that refusal's reason instead (classifyInstant).
const clockReadUncheckedWhy = "keploy learns it as a read of the application's clock, which a replay must check is a read made in its call's own span, and the replay of this call's protocol checks no such read: learned as noise it would let any value pass at every replay, so keploy refuses it. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"

// noAppClockWhy is why an instant whole in both runs that neither the lead
// nor a window shows a read of the application's clock is refused, in a
// call with no window, where keploy cannot tell which clock the application
// reads (DriftCache.appClock): it would be a plain read of the clock
// (noWindowRead, classifyRead), but no replay can check one there
// (Tracker.LiveSpan), so it is refused at learn, by one rule wherever the
// recording fell in its second.
const noAppClockWhy = "nothing shows it is a read of the application's clock: keploy cannot tell which clock the application reads in this run (the agent's time hooks report none), so its call has no window on that clock, and no replay can check a read of it there. Learned as noise it would let any value pass at every replay, so keploy refuses it. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"

// outsideWindowWhy is why a value that is one instant whole in both runs is
// refused where its call had a window on the application's clock that does
// not hold it in both runs, and the lead did not show it a read of the clock
// (read after "<field> changed, and "). A call with no window learns it as a
// plain read of the clock instead (Learned.Read), which a replay checks.
const outsideWindowWhy = "it is one instant whole that its call's window on the application's clock does not hold in both runs, a clock read before its call's window or no plain read of the clock, and it did not move by the lead that record's full round runs ahead of the recording: nothing shows it is a read of the application's clock made for its call, which a replay would check in the call's window, and noise there would let any value pass at every replay, so keploy refuses it. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"

// windowHolds reports whether w, the window of the call that carries v, holds a
// read of the application's clock in both runs at one of the instants v
// changed (Window.holdsRead), in UTC or in the application's zone.
func (dc *DriftCache) windowHolds(v *valueDrift, w *Window) bool {
	for _, p := range v.instants {
		if w.holdsRead(p.rec, p.live, dc.zone) {
			return true
		}
	}
	return false
}

// readClockText reads s as one instant whole, by the clock layouts the word
// rule reads (clockLayouts): its instant and the resolution its text writes.
// ok only where one layout's pattern matches all of s and reads it, and the
// layout names a day (a time of day does not: no window holds it).
func readClockText(s string) (time.Time, time.Duration, bool) {
	t, res, _, ok := readClockTextWall(s)
	return t, res, ok
}

// readClockTextWall is readClockText, and whether s is a wall clock of no
// zone (namesZone: a date, or a date and time, its text names no zone of),
// whose instant it reads in UTC.
func readClockTextWall(s string) (time.Time, time.Duration, bool, bool) {
	if s == "" {
		return time.Time{}, 0, false, false
	}
	for k, lay := range clockLayouts {
		if lay.wraps {
			continue
		}
		loc := lay.re.FindStringIndex(s)
		if loc == nil || loc[0] != 0 || loc[1] != len(s) {
			continue
		}
		t, res, ok := lay.read(s)
		if !ok || !inClockRange(t) {
			return time.Time{}, 0, false, false
		}
		return t, res, !namesZone(k, strings.ToUpper(s)), true
	}
	return time.Time{}, 0, false, false
}

// clockTextLayout is the layout (models.ClockOffset.Layout, one of
// models.ClockLayoutNames) of s, a text or a number that is one instant
// whole (readClockTextWall), which names the resolution its form writes
// but no fraction of a second and no zone; "" where s is no such instant.
// Its words are the clock layout's (clockLayouts) that reads all of s,
// refined by what its resolution and form are: an ISO 8601 date, its hour,
// its time to the minute or to the second (with a space or a "T" before the
// time), an epoch's unit by its digits, a UUID's version.
func clockTextLayout(s string) string {
	if _, _, _, ok := readClockTextWall(s); !ok {
		return ""
	}
	for k, lay := range clockLayouts {
		if lay.wraps {
			continue
		}
		if loc := lay.re.FindStringIndex(s); loc == nil || loc[0] != 0 || loc[1] != len(s) {
			continue
		}
		switch k {
		case 0:
			return "rfc1123"
		case 1:
			return isoLayout(s)
		case 2:
			return "compact"
		case 3:
			return "uuid" + s[14:15]
		case 4:
			digits := s
			if i := strings.IndexByte(s, '.'); i >= 0 {
				digits = s[:i]
			}
			switch len(digits) {
			case 10:
				return "epoch-s"
			case 13:
				return "epoch-ms"
			case 16:
				return "epoch-us"
			case 19:
				return "epoch-ns"
			}
		}
		return ""
	}
	return ""
}

// isoLayout is clockTextLayout of s, an ISO 8601 text clockLayouts reads
// whole: its date (the first ten bytes), and after a "T" or a space and a
// digit, its hour, minute or second; whatever follows (a fraction, a zone)
// is no part of it.
func isoLayout(s string) string {
	if len(s) < 13 || (s[10] != 'T' && s[10] != ' ') || s[11] < '0' || s[11] > '9' {
		return "iso-date"
	}
	space := ""
	if s[10] == ' ' {
		space = "-space"
	}
	switch {
	case len(s) < 14 || s[13] != ':':
		return "iso-hour"
	case len(s) < 17 || s[16] != ':':
		return "iso-minute" + space
	}
	return "iso-second" + space
}

// wallZone is the zone a replay reads a wall clock of no zone in
// (models.ClockOffset.Zone) that the learn step learns as a plain read of
// the application's clock, rt and lt its readings in UTC as recorded and as
// record's full round saw it, each at the resolution its encoding writes
// (rres, lres), in the call recorded at call whose window is w (nil: none):
// models.ZoneUTC where the learn step reads such a clock in UTC alone (the
// cache's zone is nil). Otherwise the zone the recording tells at the
// value's resolution: of a call with a window, the zone in which w holds
// the reading in both runs; with none, the zone whose reading, a unit of
// the value long, holds the call (a date: the day of the call in that
// zone). Where both do (the call falls in one day of a date in UTC and in
// the application's zone), or the two readings are one instant (the
// application's zone is UTC there), the recording cannot tell the two
// apart: models.ZoneEither, which a replay passes read in either, so its
// verdict on the application's own value is one at every hour of the day,
// whichever of the two zones the application writes. Where neither does (a
// read no window holds that the lead showed, or one made before its call),
// the zone of its recorded reading nearest the call (nearestZone). The
// replay checks the value in that zone alone (clockReadIn), so a value read
// in another fails there, named.
func (dc *DriftCache) wallZone(rt, lt time.Time, rres, lres time.Duration, w *Window, call time.Time) string {
	if dc.zone == nil {
		return models.ZoneUTC
	}
	rz, lz := wallIn(rt.UnixNano(), dc.zone), wallIn(lt.UnixNano(), dc.zone)
	if rz.Equal(rt) && lz.Equal(lt) {
		return models.ZoneEither
	}
	in := func(r, l time.Time) bool {
		if w != nil {
			return w.Rec.holds(r, rres) && w.F.holds(l, lres)
		}
		return !call.IsZero() && !r.After(call) && call.Before(r.Add(rres))
	}
	switch utc, app := in(rt, lt), in(rz, lz); {
	case utc && app:
		return models.ZoneEither
	case utc:
		return models.ZoneUTC
	case app:
		return models.ZoneApp
	}
	return nearestZone(rt, call, dc.zone)
}

// nearestZone is the zone, UTC (models.ZoneUTC) or zone (models.ZoneApp), in
// which rt, a wall clock of no zone as recorded read in UTC, is the reading
// nearest the call recorded at call: of two readings no later than call the
// later, and of one, that one; of none, the earlier. ZoneUTC where the two
// readings are one instant (zone is UTC there) or call is not known (zero),
// as the learn step read such a value before it carried a zone.
func nearestZone(rt, call time.Time, zone *time.Location) string {
	if call.IsZero() {
		return models.ZoneUTC
	}
	z := wallIn(rt.UnixNano(), zone)
	uOK, zOK := !rt.After(call), !z.After(call)
	switch {
	case uOK && zOK:
		if z.After(rt) {
			return models.ZoneApp
		}
		return models.ZoneUTC
	case uOK:
		return models.ZoneUTC
	case zOK:
		return models.ZoneApp
	}
	if z.Before(rt) {
		return models.ZoneApp
	}
	return models.ZoneUTC
}

// textReadZone is the Zone of textClockRead's read of a text recorded as rec
// and seen in record's full round as live, in the call recorded at call
// whose window is w (nil: none): of a wall clock of no zone in both
// (readClockTextWall), the zone wallZone reads it in; "" where either names
// its zone, read as it names it (where only one does, the learn step
// refuses it: textFormChangedWhy).
func (dc *DriftCache) textReadZone(rec, live string, w *Window, call time.Time) string {
	rt, rres, rwall, okR := readClockTextWall(rec)
	lt, lres, lwall, okL := readClockTextWall(live)
	if !okR || !okL || !rwall || !lwall {
		return ""
	}
	return dc.wallZone(rt, lt, rres, lres, w, call)
}

// textClockRead is Learned.Read of a text or number leaf recorded as rec
// and seen in record's full round as live, whose every changed word the
// learn step read as a read of the clock: a plain read at the
// resolution textReadRes reads from the two values' (the finer of the two,
// as a fraction's digits bound its writer's from above only), or the cut the
// two show, where
// each is one instant whole (readClockText), written in the layout both are
// (clockTextLayout), or in models.LayoutApart where the two differ, which
// the learn step refuses (textLayoutChangedWhy); nil where either is not.
func textClockRead(rec, live string) *models.ClockOffset {
	return textClockReadIn(rec, live, nil)
}

// textClockReadIn is textClockRead with the cut the two values show read in
// zone, the application's, too (nil: not known), beside UTC and the offset
// each writes (zonedRes): what the learn step reads (DriftCache.zone).
func textClockReadIn(rec, live string, zone *time.Location) *models.ClockOffset {
	rt, rres, rwall, ok := readClockTextWall(rec)
	if !ok {
		return nil
	}
	lt, lres, lwall, ok := readClockTextWall(live)
	if !ok {
		return nil
	}
	return &models.ClockOffset{Res: zonedRes(min(rres, lres), rt, lt, rwall || lwall, zone), Layout: models.ClockOffset{Layout: clockTextLayout(rec)}.MeetLayout(clockTextLayout(live))}
}

// textReadRes is the resolution a plain read of the clock written as a text
// or a number (textClockRead, todRead) is learned at, from rres and lres,
// the resolutions its two values' forms write, and grain, the granularity
// the two share: readRes of the finer of rres and lres, never the coarser.
// A fraction's digits bound the resolution its writer writes from above
// only: a writer that drops a fraction's trailing zeros (Go's RFC3339Nano,
// Java's Instant, a Python float) writes ".495123" where its read fell on a
// whole microsecond, so one value that happens to end in zeros must not
// coarsen it. Two values of a layout that fixes its resolution share it.
// Where the two are both whole multiples of a step above the finer (a unit,
// or a decimal step between units: models.CutOf), the coarsest such step
// (readRes): a cut a replay reads by its own value (CheckClockRead), in
// either value's form.
func textReadRes(rres, lres, grain time.Duration) time.Duration {
	return readRes(min(rres, lres), grain)
}

// textLayoutChangedWhy is why the learn step refuses a text or a number
// recorded as rec and seen in record's full round as live, each one instant
// whole, written in two layouts (clockTextLayout), read after "<field>
// changed, and ": a replay checks a read of the clock in the layout its
// learn step saw it in, and one that saw two cannot tell which.
func textLayoutChangedWhy(rec, live string) string {
	return "it is a read of the application's clock written as " + models.LayoutName(textLayout(rec)) + " as recorded and as " + models.LayoutName(textLayout(live)) + " in record's full round: a replay checks such a read in the form its learn step saw it written in, and this one saw two, so keploy refuses it rather than pass either form unchecked. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}

// textFormChangedWhy is why the learn step refuses a text recorded as rec
// and seen in record's full round as live, each one instant whole
// (readClockTextWall), where one names its moment and the other is a wall
// clock of no zone, read after "<field> changed, and ": a replay checks a
// read in the form its learn step saw it written in, and one that saw two
// cannot tell which zone to read it in; "" where both are one form (or
// either is no instant).
func textFormChangedWhy(rec, live string) string {
	_, _, rwall, okR := readClockTextWall(rec)
	_, _, lwall, okL := readClockTextWall(live)
	if !okR || !okL || rwall == lwall {
		return ""
	}
	form := func(wall bool) string {
		if wall {
			return namesWall.what()
		}
		return namesMoment.what()
	}
	return "it is a read of the application's clock written as " + form(rwall) + " as recorded and as " + form(lwall) + " in record's full round: a replay checks such a read in the form its learn step saw it written in, and this one saw two, so keploy refuses it rather than pass either form unchecked. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}

// textLayout is the layout of s, a text or a number that is one instant
// whole (clockTextLayout) or one time of day whole (textTimeOfDay); "" for
// any other.
func textLayout(s string) string {
	if lay := clockTextLayout(s); lay != "" {
		return lay
	}
	_, _, lay, _ := textTimeOfDay(s)
	return lay
}

// textTodRead is Learned.Read of a text recorded as rec and seen in
// record's full round as live, each one time of day whole (textTimeOfDay),
// in the call recorded at call whose window is w (todRead); nil where
// either is not.
func (dc *DriftCache) textTodRead(rec, live string, w *Window, call time.Time) *models.ClockOffset {
	rtod, rres, rlay, ok := textTimeOfDay(rec)
	if !ok {
		return nil
	}
	ltod, lres, llay, ok := textTimeOfDay(live)
	if !ok {
		return nil
	}
	return dc.todRead(rtod, ltod, rres, lres, rlay, llay, w, call)
}

// leafLayout is the layout l, a leaf of a replay's call, is written in
// (models.ClockOffset.Layout): models.LayoutTyped for an instant its
// protocol types, clockTextLayout of a text or a number. A time of day is
// none clockReadIn reads here: todReadIn reads its layout (timeOfDayOf).
func leafLayout(l Leaf) string {
	if l.Type == Instant {
		return models.LayoutTyped
	}
	return clockTextLayout(l.Value)
}

// timeOfDayOf reads l, a leaf of a call, as a time of day whole: one its
// protocol types (TimeOfDayLeaf), or a text the clock layouts read as one
// whole (textTimeOfDay): its time into its day (a typed 24:00, which
// Postgres writes, is the next day's 00:00; a text 24:00 reads as no time of
// day), the resolution its form writes, and its layout
// (models.LayoutTimeOfDay). ok is false for any other value.
func timeOfDayOf(l Leaf) (tod, res time.Duration, layout string, ok bool) {
	if l.Type == Instant {
		ti, ok := readTyped(l)
		if !ok || ti.names != namesTimeOfDay {
			return 0, 0, "", false
		}
		return ti.at.Sub(timeOfDayDay) % (24 * time.Hour), ti.res, models.LayoutTypedTimeOfDay, true
	}
	return textTimeOfDay(l.Value)
}

// textTimeOfDay reads s as one time of day whole, by the clock layout of a
// time of day (clockLayouts, wraps): "10:00", "10:00:41", "10:00:41.250":
// its time into its day, the resolution its text writes, and its layout,
// "time-minute" or "time-second" (models.ClockLayoutNames; a fraction of a
// second is no part of a layout, as it is none of a date's). ok only where
// that layout's pattern matches all of s and reads it.
func textTimeOfDay(s string) (tod, res time.Duration, layout string, ok bool) {
	for _, lay := range clockLayouts {
		if !lay.wraps {
			continue
		}
		if loc := lay.re.FindStringIndex(s); loc == nil || loc[0] != 0 || loc[1] != len(s) {
			continue
		}
		t, res, ok := lay.read(s)
		if !ok {
			return 0, 0, "", false
		}
		layout = "time-second"
		if res >= time.Minute {
			layout = "time-minute"
		}
		return t.Sub(timeOfDayDay), res, layout, true
	}
	return 0, 0, "", false
}

// todAtOrBefore is the latest instant no later than ref whose time of day,
// read in loc, is tod (a time into its day, below a day): on ref's own day
// in loc, or on the day before where that is later than ref. A read of the
// clock whose time of day a value writes at resolution res (which divides a
// day, so the value's cell lies in one day) is checked at this instant, or
// the one a day after it where the value is ahead of ref (todReadIn), within
// one res of the span on either side: the span's time of day read modulo a
// day, the midnight it may cross included.
func todAtOrBefore(tod time.Duration, ref time.Time, loc *time.Location) time.Time {
	y, m, d := ref.In(loc).Date()
	h, mi := int(tod/time.Hour), int(tod%time.Hour/time.Minute)
	sec, ns := int(tod%time.Minute/time.Second), int(tod%time.Second)
	t := time.Date(y, m, d, h, mi, sec, ns, loc)
	if t.After(ref) {
		t = time.Date(y, m, d-1, h, mi, sec, ns, loc)
	}
	return t
}

// todZone is the Zone (models.ClockOffset.Zone) a read of the clock that
// wrote a time of day is learned in: rtod as recorded and ltod as record's
// full round saw it, at their encodings' resolutions (rres, lres), in the
// call recorded at call whose window is w (nil: none). A time of day names
// no zone, as a wall clock of no zone does not (wallZone): models.ZoneUTC
// where the learn step reads such a clock in UTC alone (the cache's zone is
// nil); models.ZoneEither where the application's zone is UTC there, or a
// window holds the reading in both runs in each zone; the one zone in which
// w holds it in both runs; and otherwise the zone whose recorded reading is
// the nearest one no later than the call (of w's end where call is not
// known; models.ZoneUTC where neither is), as nearestZone reads a wall
// clock's. A replay checks the value in that zone alone (clockReadIn).
func (dc *DriftCache) todZone(rtod, ltod, rres, lres time.Duration, w *Window, call time.Time) string {
	if dc.zone == nil {
		return models.ZoneUTC
	}
	ref := call
	if ref.IsZero() && w != nil {
		ref = w.Rec.To
	}
	if ref.IsZero() {
		return models.ZoneUTC
	}
	ru, rz := todAtOrBefore(rtod, ref, time.UTC), todAtOrBefore(rtod, ref, dc.zone)
	if ru.Equal(rz) {
		return models.ZoneEither
	}
	in := func(loc *time.Location) bool { return w.holdsTod(rtod, ltod, rres, lres, loc) }
	switch utc, app := in(time.UTC), in(dc.zone); {
	case utc && app:
		return models.ZoneEither
	case utc:
		return models.ZoneUTC
	case app:
		return models.ZoneApp
	}
	if rz.After(ru) {
		return models.ZoneApp
	}
	return models.ZoneUTC
}

// todGrain is the granularity two times of day, a and b, share
// (sharedGrain), no coarser than a day: a time of day of midnight is a
// whole number of every unit, as an instant on a day's start is of each
// unit up to a day, so it shares the other's grain read on that day alone.
func todGrain(a, b time.Duration) time.Duration {
	at := func(d time.Duration) time.Time { return time.Unix(0, int64(d)) }
	return sharedGrain(at(sharedGrain(at(a), at(b))), at(24*time.Hour))
}

// todRead is Learned.Read of a time of day whole (timeOfDayOf) recorded as
// rtod and seen in record's full round as ltod, each at its encoding's
// resolution, written in layouts rlay and llay, in the call recorded at
// call whose window is w: a plain read at the resolution textReadRes reads
// from the two (a typed time of day's are its type's, one and the same), or
// the cut the two times show, in the layout both
// are (models.LayoutApart where they differ, which the learn step refuses),
// in the zone todZone reads it in. A replay checks it modulo a day in its
// call's span (clockReadIn), so a time of day the lead moved is compared
// at every replay, never noise that lets any value pass.
func (dc *DriftCache) todRead(rtod, ltod, rres, lres time.Duration, rlay, llay string, w *Window, call time.Time) *models.ClockOffset {
	return &models.ClockOffset{
		Res:    textReadRes(rres, lres, todGrain(rtod, ltod)),
		Zone:   dc.todZone(rtod, ltod, rres, lres, w, call),
		Layout: models.ClockOffset{Layout: rlay}.MeetLayout(llay),
	}
}

// holdsTod reports whether w (nil: none) holds rtod and ltod, a time of day
// as recorded and as record's full round saw it, at their encodings'
// resolutions (rres, lres), as reads of their runs' clocks in loc modulo a
// day: the instant each names last, no later than its span's end
// (todAtOrBefore), lies in that span, as a replay reads one in its call's
// span (todReadIn).
func (w *Window) holdsTod(rtod, ltod, rres, lres time.Duration, loc *time.Location) bool {
	return w != nil && w.Rec.holds(todAtOrBefore(rtod, w.Rec.To, loc), rres) && w.F.holds(todAtOrBefore(ltod, w.F.To, loc), lres)
}

// readsOwn reports whether a value recorded as r and seen in record's full
// round as l, in the call recorded at call, may be that call's own read of
// the clock in w, a window of it, by the rules the leaf learns such a read
// by: w holds an instant of it in both runs (windowHolds); it is one time of
// day whole that w holds modulo a day (holdsTod), in UTC or in the
// application's zone, as todZone reads one; or keploy knows no window of the
// call (w nil), and the leaf learns a read of the clock there in its call's
// own span, which every replay checks. Whether it is a read, and of which
// form and zone, the leaf decides.
func (dc *DriftCache) readsOwn(r, l Leaf, w *Window, call time.Time) bool {
	if w == nil || dc.windowHolds(dc.ofIn(r.Value, l.Value, w, call), w) {
		return true
	}
	rtod, rres, _, rok := timeOfDayOf(r)
	ltod, lres, _, lok := timeOfDayOf(l)
	return rok && lok && (w.holdsTod(rtod, ltod, rres, lres, time.UTC) || dc.zone != nil && w.holdsTod(rtod, ltod, rres, lres, dc.zone))
}

// clockReadOf reads l, a leaf of a replay's call, as the instant a clock
// read wrote there: an instant its protocol types that names its moment or
// is a wall clock of no zone (WallClockLeaf), or a text or a number that is
// one instant whole (readClockText), with the resolution its form writes,
// and whether it is a wall clock of no zone (wall), whose instant it reads in
// UTC. ok is false for any other value: a time of day too, which names no
// day.
func clockReadOf(l Leaf) (at time.Time, res time.Duration, wall, ok bool) {
	if l.Type == Instant {
		ti, ok := readTyped(l)
		if !ok || ti.names == namesTimeOfDay {
			return time.Time{}, 0, false, false
		}
		return ti.at, ti.res, ti.names == namesWall, true
	}
	return readClockTextWall(l.Value)
}

// clockReadIn reads live, the leaf a replay's call carries at a place the
// learn step learned as o, as the instants of the clock read o's Zone says
// it is (models.ClockOffset.Zone), in zone, the application's
// (Span.Zone): a wall clock of no zone in UTC (models.ZoneUTC), as the
// wall clock of zone (models.ZoneApp, wallIn), or both, each a reading the
// replay passes where it holds (models.ZoneEither); of a Zone of "", which
// the learn step gives only an instant that names its moment (or an offset
// from one), as the value names its moment. Otherwise why it reads none,
// after "<path> was learned as <o>, <read>": the value is no instant keploy
// reads, it names its zone where the learn step read a wall clock of no
// zone, it is a wall clock of no zone where the learn step read an instant
// that names its moment (Zone ""), or o reads in the application's zone
// (alone or as one of the two) and the replay cannot tell it (zone nil). zone is s.Zone, s
// the call's span. Of a place learned as a time of day
// (models.LayoutTimeOfDay), todReadIn reads it, in s.
func clockReadIn(o models.ClockOffset, live Leaf, s Span) ([]time.Time, time.Duration, string) {
	if models.LayoutTimeOfDay(o.Layout) {
		return todReadIn(o, live, s)
	}
	zone := s.Zone
	at, res, wall, ok := clockReadOf(live)
	if !ok {
		return nil, 0, ", and the value this call carries is no instant keploy reads: it changed"
	}
	if lay := leafLayout(live); o.Layout != "" && lay != o.Layout {
		return nil, 0, ", and the value this call carries is written as " + models.LayoutName(lay) + ", in another form than the learn step saw: it changed"
	}
	switch o.Zone {
	case "":
		if wall {
			return nil, 0, ", and the value this call carries is a wall clock that names no zone, where the learn step read an instant that names its moment: it changed"
		}
	case models.ZoneUTC, models.ZoneApp, models.ZoneEither:
		if !wall {
			return nil, 0, ", and the value this call carries names its zone, where the learn step read a wall clock of no zone: it changed"
		}
		if o.Zone == models.ZoneUTC {
			break
		}
		if zone == nil {
			return nil, 0, ", which a replay reads in the application's zone, but keploy cannot tell the application's zone at this call, so it refuses the value rather than read it in another zone"
		}
		if o.Zone == models.ZoneEither {
			return []time.Time{at, wallIn(at.UnixNano(), zone)}, res, ""
		}
		at = wallIn(at.UnixNano(), zone)
	}
	return []time.Time{at}, res, ""
}

// todReadIn is clockReadIn of a place learned as a time of day (o, of a
// models.LayoutTimeOfDay layout): live, the leaf a replay's call carries
// there, read as a time of day whole in o's layout (timeOfDayOf), as the
// instant nearest no later than the end of s, the call's span, that writes
// it in each zone o's Zone names (todAtOrBefore: UTC, the application's,
// s.Zone, or both under models.ZoneEither), and the one a day after it, at
// the resolution its form writes: Holds of a plain read then holds it
// within one of its cuts of s on either side, read modulo a day, a span
// that crosses midnight too. Otherwise why it reads
// none, as clockReadIn says it: the value is no time of day keploy reads,
// or one in another layout; o is a time of day plus an offset, or of no
// zone (no learn step writes either); or o reads it in the application's
// zone, which s does not carry.
func todReadIn(o models.ClockOffset, live Leaf, s Span) ([]time.Time, time.Duration, string) {
	tod, res, lay, ok := timeOfDayOf(live)
	if !ok {
		return nil, 0, ", and the value this call carries is no time of day keploy reads: it changed"
	}
	if lay != o.Layout {
		return nil, 0, ", and the value this call carries is written as " + models.LayoutName(lay) + ", in another form than the learn step saw: it changed"
	}
	if !o.Plain() {
		return nil, 0, ", a time of day plus an offset, which keploy does not check: it refuses the value whatever it is. Record the test again with this keploy"
	}
	var locs []*time.Location
	switch o.Zone {
	case models.ZoneUTC:
		locs = []*time.Location{time.UTC}
	case models.ZoneApp, models.ZoneEither:
		if s.Zone == nil {
			return nil, 0, ", which a replay reads in the application's zone, but keploy cannot tell the application's zone at this call, so it refuses the value rather than read it in another zone"
		}
		locs = []*time.Location{s.Zone}
		if o.Zone == models.ZoneEither {
			locs = append(locs, time.UTC)
		}
	default:
		return nil, 0, ", a time of day read in no zone, which names none: keploy refuses the value whatever it is rather than read it in a zone it was not read in. Record the test again with this keploy"
	}
	ats := make([]time.Time, 0, 2*len(locs))
	for _, loc := range locs {
		// The instant no later than the span's end, and the next one: a
		// value up to one cut ahead of the span holds (a writer that rounds,
		// models.ClockOffset.holdsRead).
		at := todAtOrBefore(tod, s.To, loc)
		ats = append(ats, at, at.AddDate(0, 0, 1))
	}
	return ats, res, ""
}

// holdsAny reports whether o holds one of ats, the readings clockReadIn
// gives a replay's value, at resolution res in the span from from to to
// (models.ClockOffset.Holds).
func holdsAny(o models.ClockOffset, ats []time.Time, res time.Duration, from, to time.Time) bool {
	for _, at := range ats {
		if o.Holds(at, res, from, to) {
			return true
		}
	}
	return false
}

// ClockReadHolds reports whether live, the leaf a replay's call carries at a
// place the learn step learned a read of the application's clock at (o, as
// models.MockSpec.Clocks holds it), is such a read: an instant made in s,
// the call's span on the application's clock (ClockSpanFrom), plus o's
// offset, read at the resolution its own form writes and in the zone o's
// Zone says, or in either of the two it names (clockReadIn;
// models.ClockOffset.Holds; of a plain read, a read in s itself, at the cut
// the value shows where o.Res is coarser than its form: CheckClockRead).
// False for
// a live value that reads as no instant (another type, a text that is not
// one instant whole), one clockReadIn reads in no zone, and for every value
// where s is no span (its From is zero): it fails closed.
//
// A read learned before its call's window (o.Since, models.SinceTestBegin)
// is read in s from the running test's begin (Span.sinceBegin): false where
// s knows no begin, and for an o of a Since this keploy does not read.
func ClockReadHolds(o models.ClockOffset, live Leaf, s Span) bool {
	ok, _ := CheckClockRead("", o, live, s, true)
	return ok
}

// spanOf is the span a replay checks o, a learned read of the clock, in: s,
// its call's span, or, for a read learned before its call's window
// (models.SinceTestBegin), s from the running test's begin (Span.sinceBegin).
// Otherwise why it checks none (read after "<path> was learned as <o>, a
// read of the application's clock"): s knows no begin of its test, or o's
// Since is one this keploy does not read.
func spanOf(o models.ClockOffset, s Span) (Span, string) {
	switch {
	case !o.SinceKnown():
		return Span{}, ": keploy refuses the value whatever it is rather than check it in a span its learn step did not read. Record the test again with this keploy"
	case o.Since != models.SinceTestBegin:
		return s, ""
	}
	b, ok := s.sinceBegin()
	if !ok {
		return Span{}, ", which a replay checks from where its test began on the application's clock, but keploy cannot tell that at this call (no test began, or the application's clock did not run as one from the test's begin to the call), so it refuses the value rather than pass it unchecked"
	}
	return b, ""
}

// CheckClockRead is a replay's check of live, the leaf its call carries at
// path, a place the learn step learned as o (models.MockSpec.Clocks), in s,
// the call's span on the application's clock, which known says the replay
// could tell (ClockSpanFrom): ok where live is a read of the clock made in s
// plus o's offset (ClockReadHolds). Otherwise why it refuses the call, which
// names path and o and says what failed: keploy could not tell the
// application's clock at the call; the value is no instant keploy reads in
// o's zone (clockReadIn: one that names its zone where the learn step read a
// wall clock of no zone, or one of the application's zone, which s does not
// carry); or it is no such read: it changed. A plain read is checked at a
// resolution: a value holds within one of it of the span on either side
// (founder decision, 2026-10-27; models.ClockOffset.holdsRead): from the
// start of the cut that holds from, less one, up to to plus one, days of
// the zone's calendar. Where o's Res is above the resolution the value's
// encoding writes (a cut the learn step's two values showed, or an
// encoding coarser than this value's), the resolution is the value's own
// cut: the coarsest step above its encoding, no coarser than o.Res, it is
// a whole multiple of (models.CutOf), read in UTC, in the offset the value
// writes and in the application's zone, s.Zone, where the replay can tell
// it, as the learn step read the two values' (cutZones, readCut,
// zonedRes); of a wall clock of no zone or a time of day, in the zone the
// value was read in. So an hour or a day the application cuts in its own
// zone (an hour in UTC+05:30, a day in UTC+02:00) is checked as that hour
// or day, never as the finer cut it shows in UTC, which would pass or fail
// by when the replay ran. A value that shows a cut is checked at it: one
// cut to the second read in the span passes wherever its read fell in its
// second, cut down or rounded, and so does one up to a second stale or
// ahead (yesterday's date at a cut of a day); one more than a cut off is
// refused. A value that shows none is checked at its own resolution, the
// stricter check: the learn step's two values fell on a step by chance (a
// Python float epoch's, or .NET's trimmed ticks, both whole
// microseconds), or the application cuts its value no coarser than a
// finer step than o.Res (Python's isoformat over a column of tenths, whose
// two learned values had no fraction, learned at the second: a replay
// value of .4 s is checked at 100 ms). A read plus an offset, then cut, is
// a clock read too (2026-10-28): its value is checked at the cut its
// learn step read (o.Res, where it is one coarser than the value's
// encoding: models.ClockOffset.CheckedAt), within one cut of the span plus the offset on either
// side (models.ClockOffset.Holds), whatever finer cut the value shows (a
// time to live of no whole number of cuts); an hour or a day it is cut to
// is read in the application's zone alone (checkReadIn), which this replay
// tells as it does a plain read's: an hour cut in UTC+05:30 starts on the
// half hour in UTC.
// The application's zone is the one its process writes its clock in in
// this run, where the agent read it from the process that made the call
// (s.Own: a wall clock of no zone is the application's own, so a recording
// learned on a laptop in IST is checked in UTC where the application now
// runs in UTC). Where keploy could not read it, it is the one the learn
// step named (models.ClockOffset.AppZone: a wall clock of no zone read in
// it, a value that names its moment cut coarser than a minute, plain or
// plus an offset), on any
// machine, never the agent's own, and a name this keploy cannot read
// refuses the value, naming it; there a wall clock of no zone
// (models.ZoneApp) checked at a cut of an hour or coarser holds only where
// it holds read in UTC too, at the cut it was checked at (utcTooWhy says
// what passes and the limits), as one read in the stored zone alone passes
// a date two days stale where the application now writes in UTC; and one
// checked at an hour is refused whatever it is where the stored zone stands
// more than an hour from UTC at either end of the span (hourFarWhy: there,
// for a call's window inside one hour, no hour that an application writing
// in that zone or in UTC writes at the call holds in both readings, in
// every zone in use today, and a fixed offset such as +01:30, where both
// can, is refused too; a span that crosses enough starts of an hour, one
// in Berlin in summer, can hold one either wrote in it).
// keploy takes this path in its docker
// and compose modes, in a pod without hostPID, and on the first call of a
// process that exits or runs another program as keploy reads its zone
// (ProcZones keeps a zone it read for each program a process runs, so a
// long-lived application's later calls are read in its own). Where
// neither is known, a read cut to an hour or coarser (plain, or plus an
// offset), whose cut the zone moves, is refused, saying so,
// except a wall clock either zone held (models.ZoneEither), which is
// checked at its cut in UTC and in s.Zone, passing where either reading
// holds (a cut read is checked, never refused for its cut, 2026-10-28); any
// other o of no name (an old file) is read in s.Zone, as before.
// One whose learnings read the application's zone as two zones
// (models.AppZoneApart) names no zone to read it in: where keploy cannot
// read the application's own, it is refused, saying so, except a wall
// clock either zone held (models.ZoneEither), which every learning held in
// UTC and which is checked there, at its cut whatever the cut.
// An o two learnings of path met as Empty (models.ClockOffset.Meet: they
// share no offset, read it in zones no one zone holds, models.ZoneApart, or
// saw it in two layouts) is refused whatever the call carries, saying so,
// and so is one of a Zone this keploy does not read
// (models.ClockOffset.ZoneKnown). It never
// passes a value it could not check.
//
// An o that is one word of a longer text (models.ClockOffset.Word: a
// client test's app named "[E2E] stop-<UnixNano>" by its own clock read
// since its test began) checks live's text is the text the learn step saw
// before that word, then the word, then the text it saw after it, the rest
// of the text compared by its length and SHA-256 (the learned file holds
// no byte of it), and reads the word alone as the read o is, as above
// (wordIn). A refusal names where the word stands, never what the text
// holds.
//
// Where a serve made after the running test began starts s (s.TestFrom,
// Tracker.LiveSpan), a value s refuses passes where it holds in s from the
// test's begin: another goroutine's call can be served between the read
// and this call (a queue's consumer beside the test's handler, 2026-10-31),
// so that serve bounds no read of this call. s is checked first, so a read
// plus an offset checkable in s alone is checked there as before; a value
// outside both is refused with s's reason.
func CheckClockRead(path string, o models.ClockOffset, live Leaf, s Span, known bool) (bool, string) {
	if o.Empty() {
		return false, path + " was learned at more than one call as " + o.String() + ": no one read of the application's clock explains them all, so keploy refuses the value whatever it is. Record the test again; if the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
	}
	learned := learnedAs(path, o)
	if !o.Word.IsZero() {
		word, why := wordIn(o.Word, live)
		if why != "" {
			return false, learned + why
		}
		o.Word, live = models.ClockWord{}, word
	}
	ok, why := checkClockRead(o, live, s, known, learned)
	if ok || s.TestFrom.IsZero() || !s.TestFrom.Before(s.From) {
		return ok, why
	}
	// A serve made after the test began started s (Span.TestFrom): another
	// goroutine's call, served between the read and this call, bounds no
	// read of this call, so a value that holds in the span from the test's
	// begin passes. The span s, narrower, is checked first: a read plus an
	// offset checkable in it may not be in the wider one (Checkable), and
	// a value that holds in it holds in the test's span.
	wide := s
	wide.From, wide.TestFrom = s.TestFrom, time.Time{}
	if ok, _ := checkClockRead(o, live, wide, known, learned); ok {
		return true, ""
	}
	return false, why
}

// learnedAs is how a refusal of a replay's value at path, a place the learn
// step learned as o, a read of the clock, begins: "<path> was learned as
// <o>, a read of the application's clock", plus "plus that offset" of a
// read plus an offset. o's String names where a word read stands in its
// text, never the text.
func learnedAs(path string, o models.ClockOffset) string {
	read := "a read of the application's clock"
	if !o.Plain() {
		read += " plus that offset"
	}
	return path + " was learned as " + o.String() + ", " + read
}

// wordIn is the word of live, the leaf a replay's call carries at a place
// the learn step learned a read of the clock at that is one word of a
// longer text, w (models.ClockOffset.Word): live's text with the recorded
// text's bytes before that word taken off its start and those after it off
// its end, compared by length and SHA-256 (models.ClockWord.WordOf), as a
// leaf of its own, the read CheckClockRead checks. Otherwise why the replay
// refuses the value, read after "<path> was learned as <o>, a read of the
// application's clock", which says which side of the word changed and
// never quotes the text: live is no text (an instant its protocol types),
// w is no place of a text (models.ClockWord.Valid), live is too short to
// hold both sides apart with a word between them, or the bytes on one side
// are not the recorded text's. So only the word is read as the clock, and
// every other byte of the text is still compared.
func wordIn(w models.ClockWord, live Leaf) (Leaf, string) {
	if live.Type == Instant {
		return Leaf{}, ", one word of a text, and the value this call carries is an instant its protocol types, no text: it changed"
	}
	if !w.Valid() {
		return Leaf{}, ", one word of a text, at no place a text has (bytes " + strconv.Itoa(w.Start) + "-" + strconv.Itoa(w.End) + ", " + strconv.Itoa(w.After) + " after, and a SHA-256 of each side): keploy refuses the value whatever it is. Record the test again"
	}
	v, changed := w.WordOf(live.Value)
	switch changed {
	case "short":
		return Leaf{}, ", one word of a text, and the text this call carries is too short to hold the " + strconv.Itoa(w.Start) + " bytes the learn step saw before that word and the " + strconv.Itoa(w.After) + " after it apart, with a word between them: it changed"
	case "before":
		return Leaf{}, ", one word of a text, and the text this call carries before that word is not the " + strconv.Itoa(w.Start) + " bytes the learn step saw there: it changed"
	case "after":
		return Leaf{}, ", one word of a text, and the text this call carries after that word is not the " + strconv.Itoa(w.After) + " bytes the learn step saw there: it changed"
	}
	word := live
	word.Value = v
	return word, ""
}

// checkClockRead is CheckClockRead of o, a read of the clock that is its
// value whole, at live, its refusals read after learned (learnedAs).
func checkClockRead(o models.ClockOffset, live Leaf, s Span, known bool, learned string) (bool, string) {
	read := "a read of the application's clock"
	if !o.Plain() {
		read += " plus that offset"
	}
	if !o.ZoneKnown() {
		return false, learned + ": keploy refuses the value whatever it is rather than read it in another zone. Record the test again with this keploy"
	}
	if !known {
		return false, learned + ", which a replay checks against its call's window on the application's clock, but keploy cannot tell the application's clock at this call (no time hooks report it, or they could not tell it here), so it refuses the value rather than pass it unchecked"
	}
	s, why := spanOf(o, s)
	if why != "" {
		return false, learned + why
	}
	hint, utcToo, stored := "", false, (*time.Location)(nil)
	switch {
	case s.Own:
		// The zone the application writes its clock in in this run, read
		// from the process that made the call, wins over the one the learn
		// step named: a wall clock of no zone is the application's own.
		if o.AppZone != "" && o.AppZone != models.AppZoneApart && o.CutInAppZone() {
			loc, _ := models.ZoneLocation(o.AppZone)
			hint = appZoneHint(o.AppZone, loc, s)
		}
	case o.AppZone != "" && !o.CutInAppZone():
		// A read plus an offset whose value a replay reads in no zone of
		// the application's (one that names its moment, cut to a minute or
		// finer; a wall clock in UTC): its stored zone serves the check near
		// the zone's backward turns alone (models.ClockOffset.FallBack). One
		// learned as two zones (AppZoneApart), or one this keploy cannot
		// read, names none, and that check reads every zone's
		// (FallBackAnywhere), never refusing the value for its zone.
		if loc, err := models.ZoneLocation(o.AppZone); o.AppZone != models.AppZoneApart && err == nil {
			s.Zone = loc
		} else {
			o.AppZone = ""
		}
	case o.AppZone == models.AppZoneApart && o.Zone == models.ZoneEither:
		// A wall clock either zone held, learned with the application's
		// zone read as two zones: every learning held it in UTC, where it
		// is checked, at its cut, an hour's or a day's too (a cut read is
		// checked, never refused for its cut, 2026-10-28). UTC alone: the
		// meet cannot tell two named zones from a named and an unnamed one,
		// so it names no other zone every learning held it in.
		o.Zone, o.AppZone = models.ZoneUTC, ""
	case o.AppZone == models.AppZoneApart:
		return false, learned + ": the learn step read the application's zone as no one zone, so a replay checks the value only in the zone it reads from the application's own process, but keploy could not read the zone of the process that made this call, so it refuses the value rather than check it in a zone the learn step never read it in." + zoneUnread + " Else record the test set again where every learning reads one zone, or declare the field noise in keploy.yml"
	case o.AppZone != "":
		// The zone the learn step read the application's in, on any machine
		// (models.ClockOffset.AppZone), where keploy cannot read the
		// application's own in this run.
		loc, err := models.ZoneLocation(o.AppZone)
		hint = appZoneHint(o.AppZone, loc, s)
		if err != nil && models.IsOwnZoneMount(err) {
			return false, learned + ", which a replay reads in " + o.AppZone + ", the application's zone the learn step read it in, but keploy's own file of that zone is another zone's here (" + err.Error() + "), so it refuses the value rather than read it in that zone's rules"
		}
		if err != nil {
			return false, learned + ", which a replay reads in " + o.AppZone + ", the application's zone the learn step read it in, but this keploy cannot read that zone (" + err.Error() + "), so it refuses the value rather than read it in another zone. Record the test again with this keploy"
		}
		s.Zone = loc
		// A wall clock of no zone read in the stored zone alone, checked at
		// a cut of an hour or coarser, passes a value stale by as much as the
		// zone the application now writes in stands from it (a recording
		// made natively in New York, replayed in docker by an application in
		// UTC: a date two days stale), so it must hold read in UTC too, at
		// that cut (2026-10-29). See utcTooWhy for what passes.
		stored, utcToo = loc, o.Zone == models.ZoneApp
	case o.Zone == models.ZoneEither && o.Plain() && o.Res >= time.Hour && s.Zone != nil:
		// A wall clock either zone held, cut to an hour or coarser, neither
		// zone known: checked, never refused for its cut. clockReadIn reads
		// it in UTC and in s.Zone, the agent's own, and it passes where
		// either reading holds at its cut, as it does at a finer one; a
		// refusal says where it was read.
		hint = ". keploy could not read the zone of the process that made this call and the learn step named none, so it checked the value in UTC and in the agent's own zone (" + s.Zone.String() + ")"
	case o.CutInAppZone() && o.Zone != models.ZoneEither && o.Res >= time.Hour:
		return false, learned + ", cut to " + models.ShortDuration(o.Res) + ", which a replay checks in the application's zone, but keploy could not read the zone of the process that made this call, and the learn step named none, so it refuses the value rather than check its cut in another zone. Record the test again where keploy reads the application's zone (keploy in the host PID namespace, the application's process and its files readable; the agent's debug log says why it could not)"
	}
	ok, cut, why := checkReadIn(o, live, s, learned, read, hint, 0)
	if !utcToo || cut < time.Hour {
		return ok, why
	}
	if cut < 24*time.Hour {
		if off, atEnd, far := farFromUTC(stored, s); far {
			when := " at this call"
			if !atEnd {
				when = " as this call's window began"
				if o.Since == models.SinceTestBegin {
					when = " when this test began"
				}
			}
			return false, learned + hourFarWhy(o.AppZone, off, when)
		}
	}
	if !ok {
		return false, why
	}
	u := s
	u.Zone = time.UTC
	if ok, _, _ := checkReadIn(o, live, u, learned, read, "", cut); ok {
		return true, ""
	}
	return false, learned + utcTooWhy(o.AppZone, cut)
}

// farFromUTC reports whether loc, the zone the learn step stored, stands
// more than an hour from UTC at either end of s (an hour learned there is
// refused whatever it is: hourFarWhy), its offset ("+05:30") at s's end
// where it stands that far there, else at s's start, and whether that is
// s's end (atEnd: false where only s's start, a test's begin for a read
// learned before its call's window, stood that far). True for no loc: it
// fails closed.
func farFromUTC(loc *time.Location, s Span) (off string, atEnd, far bool) {
	if loc == nil {
		return "", true, true
	}
	for _, end := range []bool{true, false} {
		t := s.From
		if end {
			t = s.To
		}
		if _, o := t.In(loc).Zone(); o > 3600 || o < -3600 {
			return t.In(loc).Format("-07:00"), end, true
		}
	}
	return "", false, false
}

// zoneLabel names name, a zone the learn step stored
// (models.ClockOffset.AppZone), for a person: its name ("Asia/Kolkata"), or
// "a zone at UTC+05:30" for a fixed offset.
func zoneLabel(name string) string {
	if name != "" && (name[0] == '+' || name[0] == '-') {
		return "a zone at UTC" + name
	}
	return name
}

// storedWhere and storedRun open and end a refusal of a wall clock of no
// zone (models.ZoneApp) read in name, the zone the learn step stored,
// because keploy could not read the zone of the process that made the call
// (utcTooWhy, hourFarWhy): where it was read, and what to do.
func storedWhere(name string) string {
	return ", read in " + zoneLabel(name) + ", the application's zone the learn step read it in, since keploy could not read the zone of the process that made this call. "
}

const storedRun = zoneUnread + " Else record and replay the application in UTC (TZ=UTC), where the two readings agree, or declare the field noise in keploy.yml"

// ZoneUnreadLog begins the agent's debug line for a connection whose
// process's zone keploy cannot read (the proxy logs it with the read's
// error), and zoneUnread quotes it: the refusal names a line the agent
// writes.
const ZoneUnreadLog = "keploy cannot read the zone of the process that made the connection"

// zoneUnread is what a refusal for want of the zone of the process that
// made the call says to do: where keploy reads that zone, and, in keploy's
// own docker and compose modes, where its CLI reads an app container's
// (containerkey.ConnZone) and where it reads none.
const zoneUnread = " Run keploy where it reads the application's zone: in the host PID namespace (--pid=host or hostPID), the application's process and its files readable, or in keploy's own docker and compose modes, whose CLI reads the zone of the app container that made the call through Docker. The agent's debug line \"" + ZoneUnreadLog + "\" (run keploy with --debug) says why it could not. In docker and compose modes keploy reads no zone of a container whose configuration sets a JVM's user.timezone (keploy reads a JVM's zone only from its process: drop -Duser.timezone= from the container's configuration and set the zone with TZ), of one whose TZ names no zone while its /etc/timezone names a zone other than its /etc/localtime, or whose TZ is empty and its files hold no UTC (set TZ to the zone), of one whose TZ or zone files keploy cannot resolve (a TZ of a POSIX rule or of a zone with no file in the container, an /etc/localtime linking to nothing), of one whose read failed or stalled, or of any on a host of cgroup v1 alone (and it may not on a host of both versions), where the agent cannot tell which container made the call."

// hourFarWhy is what a refusal of a wall clock of no zone (models.ZoneApp)
// checked at an hour in name, the zone the learn step stored, adds where
// name stands more than an hour from UTC (off, its offset when: at this
// call, or as its span began: farFromUTC). Each reading admits the span's
// hours and one on either side. With k how many hours the value stands
// behind name's hour at the call (below zero: ahead) and E how many hours
// name's wall hour stands ahead of UTC's at the call (name's offset at a
// whole hour; at a half or quarter hour, that offset rounded up or down by
// where in the hour the call fell), for a call's window inside one hour of
// each zone the reading in name admits k from -1 to 1 and the one in UTC k
// from E-1 to E+1: two hours ahead of UTC only k = 1 holds in both (the hour
// before name's), two behind only k = -1 (the hour after), three or more
// hours from UTC none. So, for such a window, where |E| is two or more (in
// every zone in use today that stands more than an hour from UTC), the hour
// that an application writing in name or in UTC writes at the call never
// holds in both; at a fixed offset between one and two hours (+01:30), where
// |E| is 1 for part of each hour, both can, and keploy refuses it all the
// same. A span that crosses enough starts of an hour can hold an hour either
// wrote in it, the one at the call too (a read checked from its test's
// begin, or a call's window across an hour's start: in Berlin in summer one
// crossing is enough, at any length; a zone further from UTC needs more
// crossings, or a longer span). keploy refuses an hour checked there
// whatever it is, rather than pass a stale one and refuse the current one.
func hourFarWhy(name, off, when string) string {
	far := zoneLabel(name) + " does not"
	if name != "" && name[0] != '+' && name[0] != '-' {
		far = name + " stood at UTC" + off + when
	}
	return storedWhere(name) + "An hour read in a zone the application may no longer write its clock in would pass there stale or ahead by as much as the two zones stand apart, so keploy checks such an hour in UTC too, and only where the zone stands within an hour of UTC: " + far + ", so keploy refuses the value whatever it is." + storedRun
}

// utcTooWhy is what a refusal of a wall clock of no zone (models.ZoneApp)
// adds where keploy could not read the zone of the process that made the
// call and checked it at the cut cut in name, the zone the learn step
// stored, where it held, and in UTC, at that cut, where it did not. Read in
// name alone, a value would pass stale or ahead by as much as the zone the
// application now writes in stands from name, so it must hold in both: the
// check admits only what both readings admit. So:
//   - a date: the application's own passes, and so does the day before in
//     a call's window that crosses its midnight, each wherever the reading
//     in name alone passes it (no zone's date is more than a day from
//     UTC's, and the window is a day wide on either side). The one-day
//     allowance is lost on one side in the hours name's date and UTC's
//     differ: for an application still writing in name, in New York's
//     evening (its date behind UTC's) a date a day stale is refused, in
//     Tokyo's morning (ahead) one a day ahead is, so a cached date's
//     verdict there turns on the hour of the day (for one now in UTC it is
//     the other side, which the reading in name alone refuses already);
//   - an hour, where name stands within an hour of UTC (hourFarWhy refuses
//     the rest): the application's own passes; for an application still
//     writing in name, at UTC+01:00 one an hour ahead (a writer that rounds
//     to the hour, past the half hour) is refused, at UTC-01:00 one an hour
//     stale, so such a writer's verdict turns on when in the hour it ran
//     (for one now in UTC, the other side again: at UTC+01:00 a cached hour
//     is refused, as the reading in name alone refuses it);
//   - the stated limit: an application now in a zone beyond both readings
//     (further from UTC than name, or on its other side) passes a value as
//     far off as its zone stands beyond them: a date two days stale where
//     its zone's date is a day ahead of both (Kolkata or Tokyo, late in
//     UTC's day, name New York), or two days ahead where a day behind both
//     (Honolulu, name New York, in UTC's morning), or an hour two stale
//     (Istanbul, name Berlin in winter). zone: either keeps its own.
func utcTooWhy(name string, cut time.Duration) string {
	return storedWhere(name) + "A value cut to " + models.ShortDuration(cut) + " read in a zone the application may no longer write its clock in would pass there stale or ahead by as much as the two zones stand apart, so keploy checks it in UTC too: the value this call carries passes in " + zoneLabel(name) + " but not in UTC, so keploy refuses it." + storedRun
}

// checkReadIn is CheckClockRead past its choice of zone: whether o holds
// live, the leaf a replay's call carries, read in s (its zone s.Zone), the
// cut it checked the value at (held or not; 0 where it read no value), and
// why not, learned and read naming o and hint ending the reason. A plain
// read is checked at the cut the value shows, or at force where force is
// above none (CheckClockRead's check in UTC, at the cut its check in the
// stored zone read, never coarser nor finer: a date's first
// instant on a day whose midnight that zone skips, 01:00, is an hour's in
// UTC but stands for its day).
func checkReadIn(o models.ClockOffset, live Leaf, s Span, learned, read, hint string, force time.Duration) (bool, time.Duration, string) {
	ats, res, why := clockReadIn(o, live, s)
	if why != "" {
		return false, 0, learned + why + hint
	}
	made := " made in this call's window"
	if o.Since == models.SinceTestBegin {
		made = " made after this test began and no later than this call"
	}
	if !o.Plain() {
		// A read plus an offset checked at an hour or a day (CheckedAt) is
		// checked at that cut read in the zone the value shows it whole in,
		// as a plain read's (cutZones: UTC, the offset it writes, the
		// application's zone, s.Zone), since Holds reads a cut's start and
		// its calendar days in the zone the value is read in: a Go
		// application in IST cuts to UTC's hour (Truncate counts from UTC),
		// one in Java to IST's. A value that shows the cut in none (a time
		// to live of no whole number of cuts) is read in the application's
		// zone. A value the window plus the learned range writes holds
		// wherever in the cut the replay ran, and one more than a cut past
		// either side never does, but a time to live changed by less than
		// the window, the learned range and three cuts together may hold,
		// by where in the cut the replay's read falls (the learned range,
		// about a full round's lead wide, can straddle a cut's start:
		// models.ClockOffset.Within, Checkable).
		// CheckClockRead has refused a zone it cannot tell; this fails
		// closed all the same.
		in := ""
		if c := o.CheckedAt(res); o.CutInAppZone() && c >= time.Hour {
			for i, at := range ats {
				if ci, ati := readCut(at, cutZones(o, at, s), res, c); ci == c {
					ats[i] = ati
					continue
				}
				if s.Zone == nil {
					return false, 0, learned + ", which a replay checks at its cut in the application's zone, but keploy cannot tell the application's zone at this call, so it refuses the value rather than read its cut in another zone" + hint
				}
				ats[i] = at.In(s.Zone)
			}
			in = ", read here in " + strings.TrimPrefix(zoneName(ats[0]), " in ")
			if ats[0].Location() == time.UTC {
				in = ", read here in UTC"
			}
		}
		// A read plus an offset fails closed near a wall time the
		// application's zone shows twice as its clocks go back, at any cut
		// finer than a day (models.ClockOffset.FallBack), read in that zone
		// (a belt: as well as in a named zone the value is read in), and in
		// every zone where keploy knows none of the application's.
		if why := fallBackWhy(o, ats, res, s); why != "" {
			return false, o.CheckedAt(res), learned + why + hint
		}
		if holdsAny(o, ats, res, s.From, s.To) {
			return true, o.CheckedAt(res), ""
		}
		if !o.Checkable(res, s.From, s.To) && o.CoarseChecked(res) {
			w := unitWord(o.Res)
			return false, o.CheckedAt(res), learned + ", which a replay checks at the " + w + " it is cut to against its call's window, but this call's window (" + models.ShortDuration(s.To.Sub(s.From)) + ") with the offset's range is no narrower than the " + w + ", so keploy could not tell " + offsetNamed(o) + " changed by " + w + "s and refuses the value rather than pass it unchecked" + hint
		}
		if !o.Checkable(res, s.From, s.To) {
			return false, o.CheckedAt(res), learned + ", which a replay checks against its call's window, but this call's window (" + models.ShortDuration(s.To.Sub(s.From)) + ") with the offset's range and three of its cut is no narrower than the " + unitWord(o.Unit()) + " the offset reads in, so keploy could not tell " + offsetNamed(o) + " changed by " + aUnit(o.Unit()) + " and refuses the value rather than pass it unchecked" + hint
		}
		cut := ""
		if c := o.CheckedAt(res); c > res {
			cut = " (checked as standing for the whole " + models.ShortDuration(c) + " the learn step read it cut to" + in + ")"
		}
		return false, o.CheckedAt(res), learned + ", and the value this call carries is not " + read + made + ": it changed" + cut + hint
	}
	// A cut the value shows is checked at that cut, read in the zone it
	// shows it in; a value that shows none, at its own resolution.
	cut, checked := "", time.Duration(0)
	for _, at := range ats {
		c, in := res, at
		if force > 0 {
			c = force
		} else if o.Res > res {
			if ci, ati := readCut(at, cutZones(o, at, s), res, o.Res); ci > res {
				c, in = ci, ati
				if cut == "" {
					cut = " (read as standing for the whole " + models.ShortDuration(c) + " it is cut to" + zoneName(ati) + ")"
				}
			}
		}
		p := o
		p.Res = c
		if p.Holds(in, c, s.From, s.To) {
			return true, c, ""
		}
		checked = max(checked, c)
	}
	return false, checked, learned + ", and the value this call carries is not " + read + made + ": it changed" + cut + hint
}

// fallBackWhy is why a replay refuses a value of o, a read plus an offset
// checked at a cut finer than a day, in s, the call's span, read after "<o>
// was learned as ...": the call's window, or the values the check admits
// there, meets a wall time the application's zone (s.Zone) or a zone the
// value is read in (ats) shows twice as its clocks go back
// (models.ClockOffset.FallBack), or, where keploy knows no zone of the
// application's at this call (appZoneKnown), one any zone does
// (FallBackAnywhere): an application that resolves that wall time to its
// first showing writes a value an hour early there (two in Troll), so a
// time to live made an hour longer would pass. "" where none does.
func fallBackWhy(o models.ClockOffset, ats []time.Time, res time.Duration, s Span) string {
	zones := []*time.Location{s.Zone}
	for _, at := range ats {
		zones = append(zones, at.Location())
	}
	for _, loc := range zones {
		if turn, back, met := o.FallBack(res, s.From, s.To, loc); met {
			return fallBackReplayWhy(o, res, back, fallBackNamed(turn, back, loc), "")
		}
	}
	if appZoneKnown(o, s) {
		return ""
	}
	if turn, back, loc, met := o.FallBackAnywhere(res, s.From, s.To); met {
		return fallBackReplayWhy(o, res, back, fallBackNamed(turn, back, loc)+" (keploy knows no zone of the application's at this call, so it reads every zone's clocks going back)", ", or where keploy reads the application's zone (keploy in the host PID namespace, the application's process and its files readable)")
	}
	return ""
}

// appZoneKnown reports whether a replay knows the application's zone at
// the call of s: read from its process (s.Own), or as the zone the learn
// step named (models.ClockOffset.AppZone, which CheckClockRead moved into
// s.Zone).
func appZoneKnown(o models.ClockOffset, s Span) bool {
	return s.Own || o.AppZone != "" && o.AppZone != models.AppZoneApart
}

// fallBackReplayWhy is fallBackWhy's reason for o checked at res, where its
// zone's clocks go back by back at the turn named, and where (else) a
// replay might check the value instead of outside that hour.
func fallBackReplayWhy(o models.ClockOffset, res, back time.Duration, named, elsewhere string) string {
	early := models.ShortDuration(back)
	if c := o.CheckedAt(res); c >= time.Hour {
		early = "up to " + models.ShortDuration(back+c)
	}
	return ", which a replay checks against its call's window, but this call's window, or the values that check admits, meets " + named + ": an application that resolves a wall time shown twice to its first showing (JavaScript's Date setters; java.time's LocalDateTime then atZone) writes a value " + early + " before the instant it reads there, so " + offsetNamed(o) + " made " + early + " longer would pass, and keploy refuses the value rather than pass it unchecked. Replay the test outside that hour" + elsewhere + "; if the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml"
}

// fallBackNamed names the wall time loc shows twice as its clocks go back by
// back at turn: "the hour Europe/Berlin shows twice as its clocks go back
// from 03:00 CEST to 02:00 CET on 2026-10-25".
func fallBackNamed(turn time.Time, back time.Duration, loc *time.Location) string {
	name := loc.String()
	if name == "" || name == "Local" {
		name = "the application's zone"
	}
	abbr, off := turn.Add(-time.Nanosecond).In(loc).Zone()
	what := "the hour"
	if back != time.Hour {
		what = "the " + models.ShortDuration(back)
	}
	return what + " " + name + " shows twice as its clocks go back from " + turn.In(time.FixedZone(abbr, off)).Format("15:04 MST") + " to " + turn.In(loc).Format("15:04 MST on 2006-01-02")
}

// appZoneHint is what a refusal of a value at a place learned with the
// application's zone named as name (models.ClockOffset.AppZone), learned,
// adds where this replay's own zone (s.Zone, as its tracker knows it) is
// another, by its rules (its offset at the span's end and its next change,
// never its name): the application may now write its clock there, and how
// to replay it in the zone it was recorded in. "" where the two agree, or
// either is not known.
func appZoneHint(name string, learned *time.Location, s Span) string {
	if s.Zone == nil || learned == nil {
		return ""
	}
	a, b := s.To.In(s.Zone), s.To.In(learned)
	_, oa := a.Zone()
	_, ob := b.Zone()
	_, ea := a.ZoneBounds()
	_, eb := b.ZoneBounds()
	if oa == ob && ea.Equal(eb) {
		return ""
	}
	own := s.Zone.String()
	if own == "" || own == "Local" || own[0] == '+' || own[0] == '-' {
		own = "a zone at UTC" + a.Format("-07:00")
	}
	if s.Own {
		return ". The application writes its clock in " + own + " in this run, not in " + name + ", the zone the test was recorded in, and keploy checked the value in " + own
	}
	run := "with TZ=" + name
	if name[0] == '+' || name[0] == '-' {
		// A fixed offset is no TZ value: the zone the application ran in
		// (Asia/Kolkata at +05:30) is.
		run = "in a zone at UTC" + name
	}
	return ". This replay runs in " + own + ", not " + name + ", the zone the test was recorded in: if the application now writes its clock in " + own + ", run it " + run + " as it was recorded, or record the test again"
}

// cutZones are the zones a replay reads the cut of at, a reading of its
// value at a place learned as the plain read o, in (readCut): of a value
// that names its moment (o's Zone ""), UTC, the offset it writes and the
// application's zone (s.Zone, where the replay can tell it), as the learn
// step read its two values' (zonedRes); of a wall clock of no zone or a
// time of day, the zone clockReadIn read it in alone (at's), whose clock it
// writes.
func cutZones(o models.ClockOffset, at time.Time, s Span) []*time.Location {
	if o.Zone != "" || models.LayoutTimeOfDay(o.Layout) {
		return []*time.Location{at.Location()}
	}
	return []*time.Location{time.UTC, at.Location(), s.Zone}
}

// readCut is the coarsest step of models.CutOf above enc and no coarser
// than upTo that at shows in one of zones (nil ones skipped: models.CutIn,
// its wall clock there a whole multiple of it, or the first instant of its
// hour or day there), and at read in that zone, which a check at the cut
// reads its day and its start in (models.ClockOffset.holdsRead); 0 and at
// where it shows none.
func readCut(at time.Time, zones []*time.Location, enc, upTo time.Duration) (time.Duration, time.Time) {
	c, in := time.Duration(0), at
	for _, loc := range zones {
		if loc == nil {
			continue
		}
		if u := models.CutIn(at, loc, enc, upTo); u > c {
			c, in = u, at.In(loc)
		}
	}
	return c, in
}

// zoneName names the zone at is read in for a person, after the cut it
// reads there: "" in UTC, " in Asia/Kolkata" (a zone's name), or
// " in UTC+05:30" (its offset, where it has no name).
func zoneName(at time.Time) string {
	if at.Location() == time.UTC {
		return ""
	}
	if n := at.Location().String(); n != "" && n != "Local" && n != "UTC" && n[0] != '+' && n[0] != '-' {
		return " in " + n
	}
	return " in UTC" + at.Format("-07:00")
}

// ClockSpanFrom is the span on the application's clock in which the call
// that reached keploy at the real instant arrival made its clock reads in
// this run (Tracker.LiveSpan), by the tracker on ctx (WithTracker), in the
// zone of the process that made the call where the agent read it
// (WithProcZone; Span.Own). It reads
// the tracker whether or not the set's learn step has ended (TrackerFrom
// gives none then): a replay checks every learned clock read, in every cycle.
// ok is false where ctx holds no tracker, or the tracker cannot tell the
// span.
func ClockSpanFrom(ctx context.Context, arrival time.Time) (Span, bool) {
	if ctx == nil {
		return Span{}, false
	}
	t, _ := ctx.Value(trackerKey{}).(*Tracker)
	s, ok := t.LiveSpan(arrival)
	if z, has := procZoneFrom(ctx); ok && has && z.Loc != nil {
		s.Zone, s.Own = z.Loc, true
	}
	return s, ok
}
