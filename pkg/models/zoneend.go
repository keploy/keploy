package models

import "time"

// ZoneEnd is where the zone in effect at t, in t's location, ends, as far
// as keploy reads it: an instant after t up to which that zone holds, and
// no later than the first instant after t at which Go reads another offset
// there; the zero Time where the zone holds for ever. A walk that steps from
// each instant to its ZoneEnd so advances at every step and steps over no
// change of offset.
//
// It is the end time.Time.ZoneBounds gives, or the next UTC year's start
// where that comes first, wherever the zone Go reads in the second before
// it began no later than t; else the end of a search before it. After the
// last transition a zone file lists, Go reads the zone through the file's
// rule (its TZ footer) one UTC year at a time, so its offset there changes
// at one of the rule's two times in that year, which ZoneBounds gives, or
// at the year's start, where ZoneBounds ends no zone: a rule whose time
// falls outside its UTC year (EST5EDT,0/0,J365/25, summer time all year,
// read as standard time from each UTC year's start until the rule's 05:00
// UTC) changes there. Go also keeps the zone in effect at the instant it
// loads a file (time.LoadLocationFromTZData fills a cache from the clock)
// and reads that zone's whole span from it: for a rule whose time falls
// before its UTC year's start the span begins in the year before, where
// that year's reading gives another offset, and ZoneBounds read before the
// span ends its zone past the span's start, so the zone read in the second
// before an end can have begun after t. And Go 1.27 ends a year's last zone
// 365 days after the year's start, the start of a leap year's last day, so
// for an instant on that day ZoneBounds' end is not after it
// (golang/go#81583). A slim zone file (zic -b slim, zic's default: Go's own
// zoneinfo.zip, and so the zone data built into keploy, which it reads
// where its system holds no zone files) lists no transition after the year
// its rule took over, so each leap year after it holds such a day for a
// zone of summer time (Europe/Berlin's first is 1996-12-31,
// America/New_York's 2008-12-31); there the end is the next UTC year's
// start. The search reads, at instants after t and before that end, where
// the zone in effect began (ZoneBounds' start: Go reads that zone from
// there, or from the start of the UTC year, to the instant), halving the
// span at each read: an instant whose zone began no later than the last
// second known to read t's zone reads t's zone, as does every second
// between the two; one whose zone began later bounds the search there. It
// returns the second after the last one known to read t's zone, so never
// one later than where that zone ends. The span is at most 366 days, under
// 2^25 seconds, so ZoneEnd reads ZoneBounds at most 27 times, whatever the
// reads give.
func ZoneEnd(t time.Time) time.Time {
	_, end := t.ZoneBounds()
	if end.IsZero() {
		return end
	}
	loc := t.Location()
	if year := time.Date(t.UTC().Year()+1, 1, 1, 0, 0, 0, 0, time.UTC); !end.After(t) || end.After(year) {
		end = year
	}
	if start, _ := time.Unix(end.Unix()-1, 0).In(loc).ZoneBounds(); !start.After(t) {
		return end.In(loc)
	}
	// Go reads a zone by the second an instant falls in (Unix). held is
	// the last second known to read t's zone; the search ends where past,
	// a later second, is the next one.
	held, past := t.Unix(), end.Unix()
	for past-held > 1 {
		mid := held + (past-held)/2
		start, _ := time.Unix(mid, 0).In(loc).ZoneBounds()
		switch s := start.Unix(); {
		case s <= held:
			held = mid
		case s < mid:
			past = s
		default:
			past = mid
		}
	}
	return time.Unix(held+1, 0).In(loc)
}

// ZoneRulesDiffer is the first instant from from up to to at which a and b
// read different offsets from UTC, and whether there is one: each is read
// at every change of offset either makes (ZoneEnd), so no change of either
// between two reads is missed.
func ZoneRulesDiffer(a, b *time.Location, from, to time.Time) (time.Time, bool) {
	for t := from; t.Before(to); {
		ta, tb := t.In(a), t.In(b)
		_, oa := ta.Zone()
		_, ob := tb.Zone()
		if oa != ob {
			return t, true
		}
		next := to
		for _, tt := range []time.Time{ta, tb} {
			if end := ZoneEnd(tt); !end.IsZero() && end.Before(next) {
				next = end
			}
		}
		t = next.UTC()
	}
	return time.Time{}, false
}

// SameZoneAt reports whether a and b read one offset at at and at every
// change of offset either makes in the year after it (ZoneRulesDiffer):
// for a zone whose rule changes its offset each year, at least its next
// change. Where Go ends the zone in effect (time.Time.ZoneBounds) is no
// part of it: two forms of one zone's file end it at different instants
// (ZoneEnd).
func SameZoneAt(a, b *time.Location, at time.Time) bool {
	_, differ := ZoneRulesDiffer(a, b, at, at.AddDate(1, 0, 0))
	return !differ
}
