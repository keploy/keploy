package learn

import (
	"context"
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
	rt, rres, ok := readClockText(rec)
	if !ok {
		return nil
	}
	lt, lres, ok := readClockText(live)
	if !ok {
		return nil
	}
	return &models.ClockOffset{Res: textReadRes(rres, lres, sharedGrain(rt, lt)), Layout: models.ClockOffset{Layout: clockTextLayout(rec)}.MeetLayout(clockTextLayout(live))}
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
// (readRes): a cut of a millisecond or finer a replay reads by its own value
// (CheckClockRead), in either value's form; a coarser one the learn step
// refuses (textCutWhy).
func textReadRes(rres, lres, grain time.Duration) time.Duration {
	return readRes(min(rres, lres), grain)
}

// textCutWhy is cutWhy of read, the plain read of a text or a number
// recorded as rec and seen in record's full round as live (textClockRead,
// or textTodRead where read's Layout writes a time of day), at the finer of
// its two values' encodings (textReadRes): why the learn step refuses a
// read whose two values show a cut coarser than a millisecond, "" where
// they show none.
func textCutWhy(rec, live string, read models.ClockOffset) string {
	var rres, lres time.Duration
	if models.LayoutTimeOfDay(read.Layout) {
		_, rres, _, _ = textTimeOfDay(rec)
		_, lres, _, _ = textTimeOfDay(live)
	} else {
		_, rres, _ = readClockText(rec)
		_, lres, _ = readClockText(live)
	}
	return cutWhy(read.Res, min(rres, lres))
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
// day, so the value's cell lies in one day) lies in a span ending at ref
// exactly where this instant's cell, [it, it+res), meets the span: the
// span's time of day read modulo a day, the midnight it may cross included.
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
	in := func(loc *time.Location) bool {
		return w != nil && w.Rec.holds(todAtOrBefore(rtod, w.Rec.To, loc), rres) && w.F.holds(todAtOrBefore(ltod, w.F.To, loc), lres)
	}
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
// s.Zone, or both under models.ZoneEither), at the resolution its form
// writes: Holds of a plain read then holds it where its cell meets s, read
// modulo a day, a span that crosses midnight too. Otherwise why it reads
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
	ats := make([]time.Time, 0, len(locs))
	for _, loc := range locs {
		ats = append(ats, todAtOrBefore(tod, s.To, loc))
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
// of a millisecond or finer the value shows where o.Res is coarser than its
// form: CheckClockRead). False for
// a live value that reads as no instant (another type, a text that is not
// one instant whole), one clockReadIn reads in no zone, and for every value
// where s is no span (its From is zero): it fails closed.
func ClockReadHolds(o models.ClockOffset, live Leaf, s Span) bool {
	ok, _ := CheckClockRead("", o, live, s, true)
	return ok
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
// carry); it shows a cut coarser than a millisecond; or it is no such read:
// it changed. Where o is a plain read whose Res is above the resolution the
// value's encoding writes (a cut the learn step's two values showed, of a
// millisecond or finer, or an encoding coarser than this value's), the cut
// is the value's own: the coarsest step above its encoding, no coarser than
// o.Res, it is a whole multiple of (models.CutOf, ownReading). A value that
// shows none is checked at its own resolution: the learn step's two values
// fell on a step by chance (a Python float epoch's, or .NET's trimmed
// ticks, both whole microseconds), and its own resolution is the stricter
// check. One that shows a cut of a millisecond or finer is checked at that
// cut, as standing for its whole cut: a read it holds may lie up to one cut
// before the call's span (a Java Instant written with nine digits from a
// clock of microseconds or milliseconds). One that shows a coarser cut is
// refused, naming it: checked at its encoding's resolution, a value the
// application cuts so would pass or fail by where its read fell in the cut.
// The learn step learns no cut coarser than a millisecond (cutWhy, which
// refuses the recording), so a Res above a millisecond is an encoding's
// (epoch seconds, a time to the second), and a value written finer than it
// shows a coarser cut only where it falls on one: a writer that drops
// trailing zeros, whose two learned values both fell on a whole second
// (only where F's read moved about half a second or more against the
// recording's, FullRoundLead, and then about one recording in a million at
// a millisecond's encoding, fewer at a finer one), then has one replay in a
// thousand refused. An offset's Res is
// never a cut (the learn step refuses one), and a value against one is
// refused as before.
// An o two learnings of path met as Empty (models.ClockOffset.Meet: they
// share no offset, or read it in different zones) is refused whatever the
// call carries, saying so, and so is one of a Zone this keploy does not
// read (models.ClockOffset.ZoneKnown). It never
// passes a value it could not check.
func CheckClockRead(path string, o models.ClockOffset, live Leaf, s Span, known bool) (bool, string) {
	if o.Empty() {
		return false, path + " was learned at more than one call as " + o.String() + ": no one read of the application's clock explains them all, so keploy refuses the value whatever it is. Record the test again; if the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
	}
	read := "a read of the application's clock"
	if !o.Plain() {
		read += " plus that offset"
	}
	learned := path + " was learned as " + o.String() + ", " + read
	if !o.ZoneKnown() {
		return false, learned + ": keploy refuses the value whatever it is rather than read it in another zone. Record the test again with this keploy"
	}
	if !known {
		return false, learned + ", which a replay checks against its call's window on the application's clock, but keploy cannot tell the application's clock at this call (no time hooks report it, or they could not tell it here), so it refuses the value rather than pass it unchecked"
	}
	ats, res, why := clockReadIn(o, live, s)
	if why != "" {
		return false, learned + why
	}
	if !o.Plain() && o.CutCoarserThan(res) {
		return false, learned + ", at a resolution of " + models.ShortDuration(o.Res) + " (both of the learn step's values were whole multiples of it), coarser than the " + models.ShortDuration(res) + " this value's encoding writes: the application cuts its value, and a check at its encoding's resolution would pass or fail by where its read fell in the cut, so keploy refuses it. If the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
	}
	if o.Plain() && o.Res > res {
		c := models.CutOf(ownReading(o, live), res, o.Res)
		if c > time.Millisecond {
			return false, learned + ", and the value this call carries is a whole multiple of " + models.ShortDuration(c) + ", as both of the learn step's values were, coarser than the " + models.ShortDuration(res) + " its encoding writes: the application cuts its value to " + models.ShortDuration(c) + ", and a check at its encoding's resolution would pass or fail by where its read fell in the cut, so keploy refuses it. If the field is a time the application reads from its own clock with no rule, declare it noise in keploy.yml, and keploy stops comparing it"
		}
		// A cut of a millisecond or finer the value shares is checked at
		// that cut; a value that shares none, at its own resolution.
		res = max(res, c)
		o.Res = res
	}
	if holdsAny(o, ats, res, s.From, s.To) {
		return true, ""
	}
	return false, learned + ", and the value this call carries is not " + read + " made in this call's window: it changed"
}

// ownReading is live, a replay's value at a place the learn step learned a
// plain read of the clock at as o, read as the learn step reads two values'
// grain (readRes): a time of day in nanoseconds into its day (todGrain), any
// other instant in nanoseconds from the Unix epoch (sharedGrain), a wall
// clock of no zone read in UTC. Only for a value clockReadIn read.
func ownReading(o models.ClockOffset, live Leaf) int64 {
	if models.LayoutTimeOfDay(o.Layout) {
		tod, _, _, _ := timeOfDayOf(live)
		return int64(tod)
	}
	at, _, _, _ := clockReadOf(live)
	return at.UnixNano()
}

// ClockSpanFrom is the span on the application's clock in which the call
// that reached keploy at the real instant arrival made its clock reads in
// this run (Tracker.LiveSpan), by the tracker on ctx (WithTracker). It reads
// the tracker whether or not the set's learn step has ended (TrackerFrom
// gives none then): a replay checks every learned clock read, in every cycle.
// ok is false where ctx holds no tracker, or the tracker cannot tell the
// span.
func ClockSpanFrom(ctx context.Context, arrival time.Time) (Span, bool) {
	if ctx == nil {
		return Span{}, false
	}
	t, _ := ctx.Value(trackerKey{}).(*Tracker)
	return t.LiveSpan(arrival)
}
