package models

import (
	"encoding/binary"
	"reflect"
	"testing"
	"time"
	"unsafe"
)

// zoneLoadedAt is the instant this package's tests read their zone files
// as loaded at (loadedAt): one inside the summer time of a rule whose time
// falls before its UTC year's start (XXX-3YYY-4,J1/0,M10.5.0), whose span
// Go then reads from 31 December of the year before.
var zoneLoadedAt = time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)

// loadedAt is the zone file data read as Go reads it when it loads the
// file at the instant at, whatever the clock says: Go fills a cache with
// the zone in effect at the instant it loads a file
// (time.LoadLocationFromTZData reads the clock) and reads that zone's whole
// span from it, so what it reads there depends on when it loaded the file.
// The cache is set as Go's loader sets it for at, through the Location's
// own fields: a Go release that lays them out otherwise fails here, saying
// so.
func loadedAt(t *testing.T, name string, data []byte, at time.Time) *time.Location {
	t.Helper()
	loc, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		t.Fatal(err)
	}
	v := reflect.ValueOf(loc).Elem()
	field := func(n string) reflect.Value {
		f := v.FieldByName(n)
		if !f.IsValid() {
			t.Fatalf("time.Location has no field %s: Go keeps its zone cache otherwise, and loadedAt must follow it", n)
		}
		return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	}
	cacheZone, cacheStart, cacheEnd, zones, tx := field("cacheZone"), field("cacheStart"), field("cacheEnd"), field("zone"), field("tx")
	// With no cache, Go reads every instant from the file's transitions
	// and its rule; its loader fills the cache from that reading at the
	// instant it runs, where that is at or after the first transition.
	cacheZone.Set(reflect.Zero(cacheZone.Type()))
	if tx.Len() == 0 || at.Unix() < tx.Index(0).FieldByName("when").Int() {
		return loc
	}
	lt := at.In(loc)
	zname, off := lt.Zone()
	dst := lt.IsDST()
	from, to := lt.ZoneBounds()
	start, end := int64(-1<<63), int64(1<<63-1)
	if !from.IsZero() {
		start = from.Unix()
	}
	if !to.IsZero() {
		end = to.Unix()
	}
	// The file's own zone of that name, offset and summer time, else a
	// zone of its own, as the loader takes it.
	var z reflect.Value
	for i := 0; i < zones.Len(); i++ {
		if e := zones.Index(i); e.FieldByName("name").String() == zname && int(e.FieldByName("offset").Int()) == off && e.FieldByName("isDST").Bool() == dst {
			z = e.Addr()
			break
		}
	}
	if !z.IsValid() {
		z = reflect.New(zones.Type().Elem())
		for n, x := range map[string]any{"name": zname, "offset": off, "isDST": dst} {
			f := z.Elem().FieldByName(n)
			reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Set(reflect.ValueOf(x))
		}
	}
	cacheStart.SetInt(start)
	cacheEnd.SetInt(end)
	cacheZone.Set(z)
	return loc
}

// returnsWithin runs f, failing t at once where it has not returned after
// d: a walk that stops advancing never returns (its goroutine spins on
// until the test binary ends).
func returnsWithin(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s: did not return within %s: a walk of zone changes stopped advancing", what, d)
	}
}

// tzifRule is a zone file (version 2) of no transition: every instant is
// read through rule, its TZ footer.
func tzifRule(rule string) []byte {
	var b []byte
	header := func(counts ...int) {
		b = append(b, "TZif2"...)
		b = append(b, make([]byte, 15)...)
		for _, n := range counts {
			b = binary.BigEndian.AppendUint32(b, uint32(n))
		}
	}
	// isutcnt, isstdcnt, leapcnt, timecnt, typecnt, charcnt.
	header(0, 0, 0, 0, 0, 0)
	header(0, 0, 0, 0, 1, 4)
	b = append(b, 0, 0, 0, 0, 0, 0)
	b = append(b, "STD\x00"...)
	return append(b, "\n"+rule+"\n"...)
}

// walkTo steps from from to each ZoneEnd in loc while before to, failing
// t where one is not after the instant it was read at: the instants it
// stepped to, by their Unix second, and how many steps it took.
func walkTo(t *testing.T, what string, loc *time.Location, from, to time.Time) (map[int64]bool, int) {
	t.Helper()
	steps := map[int64]bool{}
	n := 0
	returnsWithin(t, 30*time.Second, what, func() {
		for at := from; at.Before(to); n++ {
			end := ZoneEnd(at.In(loc))
			if end.IsZero() {
				break
			}
			if !end.After(at) {
				t.Errorf("%s: ZoneEnd(%s) = %s, not after it", what, at.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
				return
			}
			steps[end.Unix()] = true
			at = end
		}
	})
	return steps, n
}

// ZoneEnd is after the instant it is read at, and never later than the
// zone's next change of offset, in both of zic's forms of a zone file
// (read as loaded at zoneLoadedAt): a walk from 1970 to 2038 that steps
// to it advances at every step, lands on every change of offset (read from
// the offsets alone: zoneChanges) and takes a few steps a year. In a slim
// file (slimZoneData) Go ends the last zone of a leap year at the start of
// its last day (golang/go#81583), so on that day ZoneBounds' end is not
// after the instant: ZoneEnd is no earlier than the next UTC year's start
// there (the zone holds through the day) and no later than the zone's next
// change. A zone of one offset for ever has none.
func TestAZonesEndIsAfterTheInstantAndNoLaterThanItsNextChange(t *testing.T) {
	from, to := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"Europe/Berlin", "America/New_York", "Australia/Sydney"} {
		slim := slimZone(t, name, name)
		for _, z := range []struct {
			form string
			loc  *time.Location
		}{{"slim", slim}, {"fat", fatZone(t, name, slim)}} {
			changes := zoneChanges(z.loc)
			steps, n := walkTo(t, name+" ("+z.form+")", z.loc, from, to)
			for _, c := range changes {
				if !steps[c.Unix()] {
					t.Errorf("%s (%s): the walk steps over the change of offset at %s", name, z.form, c.Format(time.RFC3339))
				}
			}
			// Each step a change, a UTC year's start, or a leap year's last
			// day (where Go ends a slim file's zone), and the step to 2038.
			if n > len(changes)+2*68+1 {
				t.Errorf("%s (%s): %d steps for %d changes of offset in 68 years", name, z.form, n, len(changes))
			}
		}
	}

	for _, row := range []struct {
		name string
		at   time.Time // on a leap year's last day, after the slim file's last transition
		next time.Time // the zone's next change of offset
	}{
		{"Europe/Berlin", time.Date(1996, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(1997, 3, 30, 1, 0, 0, 0, time.UTC)},
		{"Europe/Berlin", time.Date(1996, 12, 31, 12, 0, 0, 500, time.UTC), time.Date(1997, 3, 30, 1, 0, 0, 0, time.UTC)},
		{"Europe/Berlin", time.Date(2028, 12, 31, 23, 59, 59, 999999999, time.UTC), time.Date(2029, 3, 25, 1, 0, 0, 0, time.UTC)},
		{"America/New_York", time.Date(2008, 12, 31, 5, 0, 0, 0, time.UTC), time.Date(2009, 3, 8, 7, 0, 0, 0, time.UTC)},
		{"Australia/Sydney", time.Date(2008, 12, 31, 12, 0, 0, 0, time.UTC), time.Date(2009, 4, 4, 16, 0, 0, 0, time.UTC)},
	} {
		loc := slimZone(t, row.name, row.name)
		at := row.at.In(loc)
		if _, end := at.ZoneBounds(); end.After(at) {
			t.Logf("%s at %s: Go's ZoneBounds' end %s is after it here (golang/go#81583 fixed in this Go)", row.name, row.at.Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339))
		}
		year := time.Date(row.at.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC)
		if end := ZoneEnd(at); end.Before(year) || end.After(row.next) {
			t.Errorf("%s: ZoneEnd(%s) = %s; want from %s up to its next change %s", row.name, row.at.Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano), year.Format(time.RFC3339), row.next.Format(time.RFC3339))
		}
	}

	for _, loc := range []*time.Location{time.UTC, time.FixedZone("IST", 5*3600+1800)} {
		if end := ZoneEnd(time.Date(2028, 12, 31, 12, 0, 0, 0, loc)); !end.IsZero() {
			t.Errorf("%s: ZoneEnd %s; want none", loc, end)
		}
	}
}

// checkWalk walks loc from from to to (walkTo), failing t where the walk
// steps over a change of offset Go reads there (zoneChangesIn), or takes
// more than a change, a UTC year's start or a leap year's last day for
// each step.
func checkWalk(t *testing.T, what string, loc *time.Location, from, to time.Time) {
	t.Helper()
	changes := zoneChangesIn(loc, from, to)
	steps, n := walkTo(t, what, loc, from, to)
	missed := 0
	for _, c := range changes {
		if !steps[c.Unix()] {
			if missed++; missed <= 3 {
				t.Errorf("%s: the walk steps over the change of offset at %s", what, c.Format(time.RFC3339))
			}
		}
	}
	if years := to.Year() - from.Year(); n > len(changes)+2*years+1 {
		t.Errorf("%s: %d steps for %d changes of offset in %d years", what, n, len(changes), years)
	}
}

// A zone read through a rule alone ends (ZoneEnd) where Go next reads
// another offset, whenever Go loaded its file. Go reads a rule one UTC
// year at a time, so where the rule's time falls outside its UTC year its
// offset changes at the year's start, past which ZoneBounds' end lies
// (EST5EDT,0/0,J365/25 reads EST from each year's start to 05:00 UTC, EDT
// after: ZoneBounds ends 1970-01-01T05:00Z's zone on 1971-01-01T05:00Z).
// And Go reads the span of the zone in effect where it loaded the file
// from a cache filled then: for a rule whose time falls before its UTC
// year's start the span begins in the year before, so loaded in its
// summer time XXX-3YYY-4,J1/0,M10.5.0 reads +04:00 from 21:00 UTC on 31
// December, where the walk's ZoneBounds ended the zone on 1 January. Each
// rule is walked from 1970 to 2038 loaded at zoneLoadedAt, and from 2026 to
// 2029 loaded across 2027 (loadedAt): on 1 January, mid-year, the day
// before each of its changes of offset that year and on 31 December.
// Among the rules: both hemispheres, negative summer time, offsets of
// quarter and half hours, rule times past the 24th hour and before the
// first, rules whose times fall outside their UTC year, and one that reads
// summer time all year (no change).
func TestAZoneReadThroughARuleEndsWhereGoReadsAnotherOffsetWheneverItLoadedTheFile(t *testing.T) {
	from, to := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
	year := func(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }
	for _, rule := range []string{
		"EST5EDT,0/0,J365/25", "XXX-3YYY-4,J1/0,M10.5.0", "XXX0YYY-1,M3.5.0,J365/12", "AAA-10BBB-11,J365/20,M4.1.0/3",
		"XXX5YYY4,M3.2.0,M12.5.0/23", "XXX-12YYY-13,M9.5.0,J1/1", "XXX12YYY11,J1/0,M10.5.0", "XXX-14YYY-15,M12.5.0/23,M1.1.0",
		"CET-1CEST,M3.5.0,M10.5.0/3", "IST-1GMT0,M10.5.0,M3.5.0/1", "<+1030>-10:30<+11>-11,M10.1.0,M4.1.0", "EET-2EEST,M3.4.4/50,M10.4.4/50",
		"<-03>3<-02>,M1.1.0/-30,M12.5.6/50", "XXX-13YYY-14,J365/23,J1/2", "AAA-5BBB-6,J60/0,J59/24", "<+0545>-5:45<+0645>-6:45,0/0,365/0",
		"XXX-1YYY-2,M2.5.0/-167,M11.1.0/167", "<-09>9<-08>,J1/-12,J365/36",
	} {
		data := tzifRule(rule)
		loc := loadedAt(t, "", data, zoneLoadedAt)
		checkWalk(t, rule+" loaded "+zoneLoadedAt.Format(time.RFC3339), loc, from, to)
		at := []time.Time{year(2027).Add(30 * time.Minute), time.Date(2027, 7, 1, 12, 0, 0, 0, time.UTC), time.Date(2027, 12, 31, 12, 0, 0, 0, time.UTC)}
		for _, c := range zoneChangesIn(loc, year(2027), year(2028)) {
			at = append(at, c.Add(-24*time.Hour))
		}
		for _, a := range at {
			checkWalk(t, rule+" loaded "+a.Format(time.RFC3339), loadedAt(t, "", data, a), year(2026), year(2029))
		}
	}
}

// ZoneRulesDiffer reads two forms of one zone's file as the same zone from
// 1970 to 2038, and returns: a slim file stopped a walk of ZoneBounds' ends
// on a leap year's last day
// (TestAZonesEndIsAfterTheInstantAndNoLaterThanItsNextChange). Two zones
// are read apart at the first instant their offsets differ; SameZoneAt
// reads them so over the year after its instant.
func TestTwoFormsOfOneZonesFileReadTheSameRules(t *testing.T) {
	from, to := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"Europe/Berlin", "America/New_York", "Australia/Sydney"} {
		slim := slimZone(t, name, name)
		fat := fatZone(t, name, slim)
		returnsWithin(t, 30*time.Second, name, func() {
			for _, pair := range [][2]*time.Location{{slim, slim}, {slim, fat}, {fat, slim}} {
				if at, differ := ZoneRulesDiffer(pair[0], pair[1], from, to); differ {
					t.Errorf("%s: two forms of its file differ at %s", name, at.Format(time.RFC3339))
				}
			}
			for _, at := range []time.Time{time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC), time.Date(2028, 12, 31, 12, 0, 0, 0, time.UTC)} {
				if !SameZoneAt(slim, fat, at) || !SameZoneAt(fat, slim, at) {
					t.Errorf("%s at %s: two forms of its file read as other zones", name, at.Format(time.RFC3339))
				}
			}
		})
	}
	berlin, newYork := slimZone(t, "Europe/Berlin", "Europe/Berlin"), slimZone(t, "America/New_York", "America/New_York")
	if at, differ := ZoneRulesDiffer(berlin, newYork, from, to); !differ || !at.Equal(from) {
		t.Errorf("Berlin and New York: differ %v at %s; want at %s", differ, at, from)
	}
	if SameZoneAt(berlin, newYork, time.Date(2028, 12, 31, 12, 0, 0, 0, time.UTC)) {
		t.Error("Berlin and New York read as one zone")
	}
	// Two zones of one offset that part in the year after the instant.
	part := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	later := loadedAt(t, "", tzifV1([]int{3600, 7200}, []time.Time{part}), zoneLoadedAt)
	if SameZoneAt(time.FixedZone("", 3600), later, time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a zone that changes its offset on %s read as one that never does", part.Format(time.RFC3339))
	}
}

// ZoneRulesDiffer reads two zone files apart at the first instant their
// offsets differ however short the difference, as the clock-zone lane's
// zonediff relies on: two files that each change once within one hour
// (at 01:15 and 01:45 UTC), and a 30-minute excursion inside an hour, are
// read apart to the second, though no hour's start or end reads them apart;
// a file and itself, never.
func TestZoneRulesDifferFindsADifferenceWithinAnHour(t *testing.T) {
	from, to := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return time.Date(2030, 3, 10, h, m, 0, 0, time.UTC) }
	zone := func(offs []int, changes ...time.Time) *time.Location {
		t.Helper()
		loc, err := time.LoadLocationFromTZData("", tzifV1(offs, changes))
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	early, late := zone([]int{0, 3600}, at(1, 15)), zone([]int{0, 3600}, at(1, 45))
	steady, excursion := zone([]int{0}), zone([]int{0, 3600, 0}, at(1, 10), at(1, 40))
	for _, row := range []struct {
		what string
		a, b *time.Location
		want time.Time
	}{
		{"one change at 01:15, the other at 01:45", early, late, at(1, 15)},
		{"the same, the other way round", late, early, at(1, 15)},
		{"UTC throughout, and UTC+01:00 from 01:10 to 01:40", steady, excursion, at(1, 10)},
	} {
		for _, h := range []time.Time{at(1, 0), at(2, 0)} {
			_, oa := h.In(row.a).Zone()
			_, ob := h.In(row.b).Zone()
			if oa != ob {
				t.Fatalf("precondition, %s: the two read apart at %s", row.what, h.Format(time.RFC3339))
			}
		}
		if got, differ := ZoneRulesDiffer(row.a, row.b, from, to); !differ || !got.Equal(row.want) {
			t.Errorf("%s: differ %v at %s; want at %s", row.what, differ, got.Format(time.RFC3339), row.want.Format(time.RFC3339))
		}
	}
	for _, loc := range []*time.Location{early, excursion} {
		if got, differ := ZoneRulesDiffer(loc, loc, from, to); differ {
			t.Errorf("a file and itself read apart at %s", got.Format(time.RFC3339))
		}
	}
}
