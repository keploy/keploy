package learn

import (
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// FullRoundLead is how far past each recorded instant record's full round F
// runs every test (design v14 §3.4): second- and minute-resolution values the
// app reads from its clock drift there, by this much, and are learned.
const FullRoundLead = 61500 * time.Millisecond

// clockSet is what of a drifted value (recorded, live) is a clock read that
// moved by FullRoundLead: the ASCII words of such reads, recorded (rec) and
// live (live), whether every word of the value that changed is one (whole),
// and each such read as recorded, in the order they stand in it (reads).
type clockSet struct {
	rec, live map[string]bool
	whole     bool
	reads     []models.ClockRead
	// pairs are the accepted reads of the value as it stands (not merged:
	// clockInBy of these two texts), where each stands recorded and live,
	// in the order they stand in the recorded value: what the value's words
	// collapse (words.go).
	pairs []clockPair
	// zoned are the instants that moved as a clock read does, by the lead
	// or within the call's window, but whose zone is written otherwise live
	// ("Z" and "+01:00", "GMT" and "UTC"): no clock read, as F's clock moves
	// no zone (as it stands, not merged).
	zoned []clockPair
}

// clockSpan is where an instant stands in a value, [from, to), and its
// layout (an index of clockLayouts).
type clockSpan struct{ from, to, lay int }

// clockPair is one accepted clock read: where it stands recorded (r) and
// live (l).
type clockPair struct{ r, l clockSpan }

// sortedCopy is ws sorted, ws left as it was.
func sortedCopy(ws []string) []string {
	out := append([]string(nil), ws...)
	sort.Strings(out)
	return out
}

// A clock layout is a way a value can carry an instant, as a pattern of the
// recorded text and how to read it. timeWord, when not nil, says which words
// of a match hold the instant (a UUID's time groups); every word of the match
// does otherwise. A layout that writes a zone captures it in the pattern's
// groups (zone), each held to what a zone is written with: a zone's name
// from the declared list (zoneNames), a region, an offset, never free text,
// which noise would excuse beside the read.
type clockLayout struct {
	// name is the layout's name, as a word slot names it
	// (models.WordSlot.Layout).
	name     string
	re       *regexp.Regexp
	read     func(s string) (t time.Time, res time.Duration, ok bool)
	timeWord func(match string, i int) bool
	// wraps: the instant is a time of day, compared modulo a day.
	wraps bool
	// zone are the groups of re that write the instant's zone (zoneOf), and
	// name the group that holds a zone's name from the declared list (an N
	// of its skeleton), 0 for none.
	zone      []int
	nameGroup int
	// frac: a fraction of a second may follow its seconds (an F of its
	// skeleton, present or not). dayPad: its day after the month is padded
	// with a space ("Oct  1", a D of its skeleton).
	frac, dayPad bool
}

// The declared zone names (a word slot's N): the tzdata abbreviations of the
// zones apps write by name, and the long names JavaScript's Date#toString
// writes of them in parentheses (CLDR's, as V8 and ICU write them in
// English). The list is closed: a name outside it is text, compared as it
// stands, and the layout hash covers it (wordsGrammar), so a change to the
// list voids the words learned under the old one.
var (
	zoneAbbrevs = []string{
		"GMT", "UTC", "BST", "IST", "WET", "WEST", "CET", "CEST", "EET", "EEST", "MSK",
		"EST", "EDT", "CST", "CDT", "MST", "MDT", "PST", "PDT", "AKST", "AKDT", "HST",
		"AEST", "AEDT", "ACST", "ACDT", "AWST", "NZST", "NZDT", "JST", "KST", "HKT",
		"SAST", "WAT", "CAT", "EAT",
	}
	zoneLongNames = []string{
		"Greenwich Mean Time", "Coordinated Universal Time", "British Summer Time",
		"India Standard Time", "Irish Standard Time", "Israel Standard Time",
		"Western European Standard Time", "Western European Summer Time",
		"Central European Standard Time", "Central European Summer Time",
		"Eastern European Standard Time", "Eastern European Summer Time",
		"Moscow Standard Time",
		"Eastern Standard Time", "Eastern Daylight Time",
		"Central Standard Time", "Central Daylight Time", "China Standard Time",
		"Mountain Standard Time", "Mountain Daylight Time",
		"Pacific Standard Time", "Pacific Daylight Time",
		"Alaska Standard Time", "Alaska Daylight Time",
		"Hawaii-Aleutian Standard Time",
		"Australian Eastern Standard Time", "Australian Eastern Daylight Time",
		"Australian Central Standard Time", "Australian Central Daylight Time",
		"Australian Western Standard Time",
		"New Zealand Standard Time", "New Zealand Daylight Time",
		"Japan Standard Time", "Korean Standard Time", "Hong Kong Standard Time",
		"South Africa Standard Time", "West Africa Standard Time",
		"Central Africa Time", "East Africa Time",
	}
)

// alternation is names as one regexp alternation, the longest first, so a
// name is never read as a shorter one it begins with.
func alternation(names []string) string {
	sorted := append([]string(nil), names...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if len(sorted[i]) != len(sorted[j]) {
			return len(sorted[i]) > len(sorted[j])
		}
		return sorted[i] < sorted[j]
	})
	quoted := make([]string, len(sorted))
	for i, n := range sorted {
		quoted[i] = regexp.QuoteMeta(n)
	}
	return strings.Join(quoted, "|")
}

var (
	abbrevAlt = alternation(zoneAbbrevs)
	longAlt   = alternation(zoneLongNames)
)

var clockLayouts = []clockLayout{
	// RFC 1123 (an HTTP date): "Wed, 21 Oct 2026 10:28:02 GMT", a declared
	// zone's name or an offset.
	{name: "rfc1123", re: regexp.MustCompile(`(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun), \d{2} (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{4} \d{2}:\d{2}:\d{2} ((` + abbrevAlt + `)|[+-]\d{4})`), read: readRFC1123, zone: []int{1}, nameGroup: 2},
	// Unix date, C's asctime, Go's UnixDate and RubyDate, Java's
	// Date#toString: "Wed Oct 21 10:28:02 UTC 2026", the year last, a
	// declared zone's name (read at offset 0, the same on every host) or an
	// offset.
	{name: "unixdate", re: regexp.MustCompile(`(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun) (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) [ \d]\d \d{2}:\d{2}:\d{2}(?: ((` + abbrevAlt + `)|[+-]\d{4}))? \d{4}`), read: readUnixDate, zone: []int{1}, nameGroup: 2, dayPad: true},
	// JavaScript's Date#toString: "Wed Oct 21 2026 10:28:02 GMT+0000
	// (Coordinated Universal Time)", the zone's long name in parentheses
	// when it is a declared one.
	{name: "jsdate", re: regexp.MustCompile(`(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun) (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{2} \d{4} \d{2}:\d{2}:\d{2} (GMT[+-]\d{4}(?: \((` + longAlt + `)\))?)`), read: readJSDate, zone: []int{1}, nameGroup: 2},
	// RFC 3339 and ISO 8601, a SQL datetime, its date and a time to the
	// second joined by a '+' (a form-encoded space; "2026-10-21+05:00" is a
	// date and its offset), Ruby's Time#to_s and Go's Time.String (an offset
	// of a time after a space, then a declared zone's name: Go's
	// " +0000 UTC", " +0100 BST"), and Java's ZonedDateTime (an IANA region
	// after the offset, "[Europe/Paris]", "[Etc/UTC]"; its places are named
	// in letters).
	{name: "iso8601", re: regexp.MustCompile(`\d{4}-\d{2}-\d{2}(?:(?:[T ]\d{2}:\d{2}(?::\d{2}(?:[.,]\d{1,9})?)?|\+\d{2}:\d{2}:\d{2}(?:[.,]\d{1,9})?)( [+-](?:0\d|1[0-4]):?[0-5]\d(?: (` + abbrevAlt + `))?)?)?((?:Z|[+-]\d{2}(?::?\d{2})?)(?:\[(?:UTC|GMT|UCT|Etc/(?:UTC|UCT|GMT(?:[+-]?\d{1,2})?|Universal|Zulu|Greenwich)|(?:Africa|America|Antarctica|Arctic|Asia|Atlantic|Australia|Europe|Indian|Pacific)(?:/[A-Z][A-Za-z_-]+)+)\])?| ?(?:UTC|GMT))?`), read: readISO, zone: []int{1, 3}, nameGroup: 2, frac: true},
	{name: "compact", re: regexp.MustCompile(`\d{8}T\d{6}(Z?)`), read: readCompact, zone: []int{1}},
	{name: UUIDClockLayout, re: regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[17][0-9a-fA-F]{3}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`), read: readUUIDTime, timeWord: uuidTimeWord},
	{name: "epoch", re: regexp.MustCompile(`\d{10}\.\d{1,9}|\d{19}|\d{16}|\d{13}|\d{10}`), read: readEpoch, frac: true},
	{name: "timeofday", re: regexp.MustCompile(`\d{2}:\d{2}(?::\d{2}(?:\.\d{1,9})?)?`), read: readTimeOfDay, wraps: true, frac: true},
}

// layoutNamed is the index in clockLayouts of the layout of that name, -1
// for none.
func layoutNamed(name string) int {
	for i, lay := range clockLayouts {
		if lay.name == name {
			return i
		}
	}
	return -1
}

// UUIDClockLayout is the name of the clock layout of a time-ordered UUID (a
// version 1 or 7), as the words format writes it (models.WordSlot.Layout):
// what a protocol learner reads a learned UUID's clock read by.
const UUIDClockLayout = "uuid"

// uuidLayout is the index in clockLayouts of the time-ordered UUID's.
var uuidLayout = layoutNamed(UUIDClockLayout)

// ClockOf reports whether a value drifted only with F's clock: every word
// that changed between its recording (recorded) and record's full round
// (live) is a part of a clock read that moved by FullRoundLead, and one did.
// It is the evidence Classify reads (clockOf), read at learn time: live is
// the value F saw, its clock set past each recorded instant by the lead. An
// adapter asks it, and has no rule of its own of what a time looks like: a
// value that only looks like one ("2025551234") and did not move by the
// lead is no clock read.
func ClockOf(recorded, live string) bool {
	return clockOf(recorded, live).whole
}

// ClockReads is that evidence as the learn step records it on a variable it
// binds (models.MockVariable.Clock): for a value ClockOf says drifted only
// with F's clock, each clock read it held when recorded that moved by the
// lead, the instant and resolution its recorded text says, in the order they
// stand in it; nil for any other value. A replay lets two variables recorded
// with two values share one live value only when both hold reads made in
// one second (models.ReadInOneSecond).
func ClockReads(recorded, live string) []models.ClockRead {
	c := clockOf(recorded, live)
	if !c.whole {
		return nil
	}
	return c.reads
}

// Window is when one call's clock reads can have been made, in each run
// (Tracker.CallWindow): in the recording (Rec) and in record's full round
// (F), each on that run's app clock, after the run's previous data-plane call
// reached keploy and no later than this call did. A value is in a request
// only once the app read it, so a read of the clock is no later than the
// call that carries it; one made after the previous call is no earlier than
// that. An instant that falls in its run's span in both runs moved with the
// clock between them by the call's own lead (the lead's difference to the
// value's is how much longer the app took, in one run than in the other,
// from the read to the call, each at least nothing), however far F's clock
// ran ahead while the app booted: a constant does not move, nor does an
// instant an answer the replay serves from its recording carries, so
// neither falls in both. A read made before the previous call falls in
// neither. It is no tolerance: the spans are what each run's calls show.
type Window struct{ Rec, F Span }

// Span is one run's part of a Window: From, when its previous data-plane
// call reached keploy, and To, when the call itself did, on its app clock.
type Span struct{ From, To time.Time }

// holds reports whether t, an instant read at resolution res, is a read of
// the clock within s: each bound is read at the instant's own resolution (a
// value in seconds is the second its read was made in).
func (s Span) holds(t time.Time, res time.Duration) bool {
	if s.From.IsZero() || s.To.IsZero() {
		return false
	}
	return !t.Before(s.From.Truncate(res)) && !t.After(s.To.Truncate(res))
}

// holds reports whether recorded and live, an instant in layout lay each,
// are reads of their runs' clocks within w (Window). A time of day names no
// day, so its instant falls in no span: only the lead reads it.
func (w *Window) holds(lay clockLayout, recorded, live string) bool {
	if w == nil {
		return false
	}
	rt, rres, ok := lay.read(recorded)
	if !ok {
		return false
	}
	lt, lres, ok := lay.read(live)
	if !ok {
		return false
	}
	return w.Rec.holds(rt, rres) && w.F.holds(lt, lres)
}

// Holds reports whether rec and live, instants a value carried in the
// recording and in this run, read at resolution res, are reads of their
// runs' clocks within w (nil: none is).
func (w *Window) Holds(rec, live time.Time, res time.Duration) bool {
	return w != nil && w.Rec.holds(rec, res) && w.F.holds(live, res)
}

// key is w as a map key, the zero key for none.
func (w *Window) key() [4]int64 {
	if w == nil {
		return [4]int64{}
	}
	return [4]int64{w.Rec.From.UnixNano(), w.Rec.To.UnixNano(), w.F.From.UnixNano(), w.F.To.UnixNano()}
}

// zoneOf is the zone text s, an instant in layout lay, is written with
// ("Z", "+05:30", " UTC", " +0000 UTC"): its pattern's zone groups, "" for
// none or a layout that writes none.
func zoneOf(lay clockLayout, s string) string {
	if len(lay.zone) == 0 {
		return ""
	}
	m := lay.re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, g := range lay.zone {
		b.WriteString(m[g])
	}
	return b.String()
}

// clockOf is clockIn with no call's window: the clock reads that moved by
// FullRoundLead.
func clockOf(recorded, live string) clockSet { return clockIn(recorded, live, nil) }

// clockIn is clockInBy with the lead: a read is one F's lead moved, or one
// within w.
func clockIn(recorded, live string, w *Window) clockSet { return clockInBy(recorded, live, w, true) }

// clockInBy finds in a drifted value the clock reads that moved by
// FullRoundLead (when lead), or that are reads of each run's clock within
// the window w of the call that carries the value (nil: none): the recorded
// and the live value are each searched for instants in clockLayouts, at
// ASCII word boundaries (so one glued to text of another script,
// "创建于2026-10-14 10:00:41", is found), and the k-th of a layout in one
// is read against the k-th in the other: where the live one is the
// recorded one later by the lead, the words that changed between them are
// clock words. Nothing else of the values need line up (a fresh
// signature beside the read may change their number of words). The lead is
// compared at the instant's own resolution, and to the second for finer
// ones: F's clock is set to each recorded instant plus the lead when the
// test begins, and how long the app then takes to read its clock is its own
// (a millisecond read moves by the lead plus that). A read pairs instants
// of one layout written in one zone ("Z" and "Z", never "Z" and "+00:00"):
// F's clock moves the instant, never how its zone is written, and an instant
// that moved but whose zone did not stay is no clock read (zoned). Where each
// accepted read stands, recorded and live, is pairs, in the order the reads
// stand in the recorded value.
func clockInBy(recorded, live string, w *Window, lead bool) clockSet {
	c := clockSet{rec: map[string]bool{}, live: map[string]bool{}}
	takenR, takenL := make([]bool, len(recorded)), make([]bool, len(live))
	var at []int
	for li, lay := range clockLayouts {
		rm, lm := clockMatches(lay, recorded, takenR), clockMatches(lay, live, takenL)
		for k := 0; k < len(rm) && k < len(lm); k++ {
			rs, ls := recorded[rm[k][0]:rm[k][1]], live[lm[k][0]:lm[k][1]]
			if rs == ls || !(lead && moved(lay, rs, ls)) && !w.holds(lay, rs, ls) {
				continue
			}
			pair := clockPair{clockSpan{rm[k][0], rm[k][1], li}, clockSpan{lm[k][0], lm[k][1], li}}
			if zoneOf(lay, rs) != zoneOf(lay, ls) {
				c.zoned = append(c.zoned, pair)
				continue
			}
			rt, rres, _ := lay.read(rs)
			c.reads = append(c.reads, models.ClockRead{At: rt.UTC(), Res: rres, Day: lay.wraps})
			at = append(at, rm[k][0])
			c.pairs = append(c.pairs, pair)
			for i := rm[k][0]; i < rm[k][1]; i++ {
				takenR[i] = true
			}
			for i := lm[k][0]; i < lm[k][1]; i++ {
				takenL[i] = true
			}
			rw, lw := wordsBy(rs, isWord), wordsBy(ls, isWord)
			for i, w := range rw {
				if lay.timeWord == nil || lay.timeWord(rs, i) {
					c.rec[w] = true
				}
			}
			for i, w := range lw {
				if lay.timeWord == nil || lay.timeWord(ls, i) {
					c.live[w] = true
				}
			}
		}
	}
	// The reads in the order they stand in the recorded value: a layout's
	// are found after an earlier layout's.
	sort.Sort(byPlace{at, c.reads})
	sort.Slice(c.pairs, func(i, j int) bool { return c.pairs[i].r.from < c.pairs[j].r.from })
	// The whole value is a clock read when every word that changed is a
	// part of one, and some did.
	r, l := tokensBy(recorded, isWord), tokensBy(live, isWord)
	changed := false
	c.whole = true
	for w := range r {
		if !l[w] {
			changed = true
			c.whole = c.whole && c.rec[w]
		}
	}
	for w := range l {
		if !r[w] {
			changed = true
			c.whole = c.whole && c.live[w]
		}
	}
	c.whole = c.whole && changed
	return c
}

// byPlace sorts reads by where each stands in its value (at).
type byPlace struct {
	at    []int
	reads []models.ClockRead
}

func (b byPlace) Len() int           { return len(b.at) }
func (b byPlace) Less(i, j int) bool { return b.at[i] < b.at[j] }
func (b byPlace) Swap(i, j int) {
	b.at[i], b.at[j] = b.at[j], b.at[i]
	b.reads[i], b.reads[j] = b.reads[j], b.reads[i]
}

// clockMatches are the places of s that hold an instant in layout lay, at
// ASCII word boundaries, and that no earlier layout took (taken).
func clockMatches(lay clockLayout, s string, taken []bool) [][]int {
	var out [][]int
	for _, m := range lay.re.FindAllStringIndex(s, -1) {
		if m[0] > 0 && isWord(s[m[0]-1]) && isWord(s[m[0]]) || m[1] < len(s) && isWord(s[m[1]-1]) && isWord(s[m[1]]) {
			continue
		}
		free := true
		for i := m[0]; i < m[1]; i++ {
			if taken[i] {
				free = false
				break
			}
		}
		if free {
			out = append(out, m)
		}
	}
	return out
}

// moved reports whether the live text is the recorded one's instant moved by
// FullRoundLead, read in layout lay.
func moved(lay clockLayout, recorded, live string) bool {
	rt, rres, ok := lay.read(recorded)
	if !ok {
		return false
	}
	lt, lres, ok := lay.read(live)
	if !ok {
		return false
	}
	res := rres
	if lres > res {
		res = lres
	}
	if res < time.Second {
		res = time.Second
	}
	d := lt.Truncate(res).Sub(rt.Truncate(res))
	if lay.wraps {
		for d <= -12*time.Hour {
			d += 24 * time.Hour
		}
		for d > 12*time.Hour {
			d -= 24 * time.Hour
		}
	}
	return d > FullRoundLead-res && d < FullRoundLead+res
}

// An instant before 2000 or after 2100 is no clock read of a recording.
var (
	clockFrom = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	clockTo   = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

func inClockRange(t time.Time) bool { return !t.Before(clockFrom) && t.Before(clockTo) }

// readRFC1123 reads an HTTP date to the second: a declared zone's name at
// offset 0 (in UTC, so the same on every host), or its offset.
func readRFC1123(s string) (time.Time, time.Duration, bool) {
	for _, layout := range []string{time.RFC1123, time.RFC1123Z} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil && inClockRange(t) {
			return t, time.Second, true
		}
	}
	return time.Time{}, 0, false
}

func readISO(s string) (time.Time, time.Duration, bool) {
	// A region after the offset names the zone the offset is of: the offset
	// reads the instant.
	if k := strings.IndexByte(s, '['); k >= 0 {
		s = s[:k]
	}
	// A declared zone's name after a spaced offset (Go's " +0100 BST") names
	// the zone the offset is of: the offset reads the instant.
	if m := spacedName.FindStringSubmatchIndex(s); m != nil {
		s = s[:m[2]]
	}
	s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "UTC"), "GMT"), " ")
	// An offset after a space (" +0000", " +00:00") is the offset glued, and
	// a '+' between the date and the time a space.
	if m := spacedZone.FindStringIndex(s); m != nil {
		s = s[:m[0]] + s[m[0]+1:]
	}
	if plusTime.MatchString(s) {
		s = s[:10] + " " + s[11:]
	}
	n := strings.Replace(strings.Replace(s, " ", "T", 1), ",", ".", 1)
	res := 24 * time.Hour
	if i := strings.Index(n, "T"); i >= 0 {
		// The time's own colons, not its zone's ("+05:30").
		clock := n[i+1:]
		if z := strings.IndexAny(clock, "Z+-"); z >= 0 {
			clock = clock[:z]
		}
		res = time.Minute
		if strings.Count(clock, ":") >= 2 {
			res = time.Second
			if f := strings.IndexByte(clock, '.'); f >= 0 {
				res = fractionRes(len(clock) - f - 1)
			}
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05Z07", "2006-01-02T15:04:05",
		"2006-01-02T15:04Z07:00", "2006-01-02T15:04Z0700", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, n); err == nil && inClockRange(t) {
			return t, res, true
		}
	}
	return time.Time{}, 0, false
}

// spacedZone is an offset after a space, ending an instant, spacedName a
// zone's name after one; plusTime a date and a time to the second joined by
// a '+'.
var (
	spacedName = regexp.MustCompile(` [+-]\d{2}:?\d{2}( [A-Z]{3,5})$`)
	spacedZone = regexp.MustCompile(` [+-]\d{2}:?\d{2}$`)
	plusTime   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\+\d{2}:\d{2}:\d{2}`)
)

// readUnixDate reads an instant of Unix date's layouts (unixDateLayouts), to
// the second, in UTC when it names no zone, and a declared zone's name at
// offset 0 (in UTC): the same instant on every host. A lead read compares a
// difference of two, which offset 0 keeps; a read within the call's window of
// a zone other than UTC or GMT reads its instant off by its offset, and is no
// read.
func readUnixDate(s string) (time.Time, time.Duration, bool) {
	for _, layout := range unixDateLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil && inClockRange(t) {
			return t, time.Second, true
		}
	}
	return time.Time{}, 0, false
}

// unixDateLayouts are asctime's, Unix date's (a zone's abbreviation before
// the year) and Ruby's (a numeric zone), the day padded or not.
var unixDateLayouts = []string{time.ANSIC, time.UnixDate, time.RubyDate, "Mon Jan _2 15:04:05 -0700 2006"}

// readJSDate reads JavaScript's Date#toString, to the second; the zone's
// name in parentheses after its offset is not read.
func readJSDate(s string) (time.Time, time.Duration, bool) {
	if i := strings.Index(s, " ("); i >= 0 {
		s = s[:i]
	}
	if t, err := time.Parse("Mon Jan 02 2006 15:04:05 GMT-0700", s); err == nil && inClockRange(t) {
		return t, time.Second, true
	}
	return time.Time{}, 0, false
}

func readCompact(s string) (time.Time, time.Duration, bool) {
	for _, layout := range []string{"20060102T150405Z", "20060102T150405"} {
		if t, err := time.Parse(layout, s); err == nil && inClockRange(t) {
			return t, time.Second, true
		}
	}
	return time.Time{}, 0, false
}

// readTimeOfDay reads a time of day, to the second or the minute, or to its
// fraction of a second ("10:00:41.250").
func readTimeOfDay(s string) (time.Time, time.Duration, bool) {
	if t, err := time.Parse("15:04:05", s); err == nil {
		if f := strings.IndexByte(s, '.'); f >= 0 {
			return t, fractionRes(len(s) - f - 1), true
		}
		return t, time.Second, true
	}
	if t, err := time.Parse("15:04", s); err == nil {
		return t, time.Minute, true
	}
	return time.Time{}, 0, false
}

// readEpoch reads seconds (10 digits, with or without a fraction),
// milliseconds (13), microseconds (16) or nanoseconds (19) since the epoch,
// each to its own resolution.
func readEpoch(s string) (time.Time, time.Duration, bool) {
	var t time.Time
	var res time.Duration
	digits := s
	if i := strings.IndexByte(s, '.'); i >= 0 {
		digits = s[:i]
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return time.Time{}, 0, false
		}
		t, res = time.Unix(0, int64(f*1e9)).UTC(), fractionRes(len(s)-i-1)
	} else {
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			return time.Time{}, 0, false
		}
		switch len(digits) {
		case 10:
			t, res = time.Unix(n, 0).UTC(), time.Second
		case 13:
			t, res = time.UnixMilli(n).UTC(), time.Millisecond
		case 16:
			t, res = time.UnixMicro(n).UTC(), time.Microsecond
		case 19:
			t, res = time.Unix(0, n).UTC(), time.Nanosecond
		default:
			return time.Time{}, 0, false
		}
	}
	if !inClockRange(t) {
		return time.Time{}, 0, false
	}
	return t, res, true
}

// fractionRes is the resolution of a fraction of a second written with n
// digits: a tenth of a second for one, a nanosecond for nine or more.
func fractionRes(n int) time.Duration {
	res := time.Second
	for ; n > 0 && res > time.Nanosecond; n-- {
		res /= 10
	}
	return res
}

// readUUIDTime reads the instant of a time-ordered UUID, each to its own
// resolution: version 7 (its first 48 bits, milliseconds since the epoch) or
// version 1 (60 bits of 100-nanosecond intervals since 1582-10-15).
func readUUIDTime(s string) (time.Time, time.Duration, bool) {
	h := strings.ReplaceAll(s, "-", "")
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 16 {
		return time.Time{}, 0, false
	}
	var t time.Time
	var res time.Duration
	switch b[6] >> 4 {
	case 7:
		ms := int64(b[0])<<40 | int64(b[1])<<32 | int64(b[2])<<24 | int64(b[3])<<16 | int64(b[4])<<8 | int64(b[5])
		t, res = time.UnixMilli(ms).UTC(), time.Millisecond
	case 1:
		ts := (int64(b[6]&0x0f)<<56 | int64(b[7])<<48 | int64(b[4])<<40 | int64(b[5])<<32 | int64(b[0])<<24 | int64(b[1])<<16 | int64(b[2])<<8 | int64(b[3]))
		const gregorianToUnix = 122192928000000000 // 100 ns intervals from 1582-10-15 to 1970-01-01
		t, res = time.Unix(0, (ts-gregorianToUnix)*100).UTC(), 100*time.Nanosecond
	default:
		return time.Time{}, 0, false
	}
	if !inClockRange(t) {
		return time.Time{}, 0, false
	}
	return t, res, true
}

// uuidTimeWord reports whether word i (0-based, of the five groups) of a
// time-ordered UUID holds its instant: the first two groups of version 7,
// the first three of version 1. The others are random or a node.
func uuidTimeWord(s string, i int) bool {
	if len(s) < 15 {
		return false
	}
	switch s[14] {
	case '7':
		return i < 2
	case '1':
		return i < 3
	}
	return false
}
