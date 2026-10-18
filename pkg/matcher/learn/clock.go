package learn

import (
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FullRoundLead is how far past each recorded instant record's full round F
// runs every test (design v14 §3.4): second- and minute-resolution values the
// app reads from its clock drift there, by this much, and are learned.
const FullRoundLead = 61500 * time.Millisecond

// clockSet is what of a drifted value (recorded, live) is a clock read that
// moved by FullRoundLead: the ASCII words of such reads, recorded (rec) and
// live (live), and whether every word of the value that changed is one
// (whole).
type clockSet struct {
	rec, live map[string]bool
	whole     bool
}

// A clock layout is a way a value can carry an instant, as a pattern of the
// recorded text and how to read it. timeWord, when not nil, says which words
// of a match hold the instant (a UUID's time groups); every word of the match
// does otherwise.
type clockLayout struct {
	re       *regexp.Regexp
	read     func(s string) (t time.Time, res time.Duration, ok bool)
	timeWord func(match string, i int) bool
	// wraps: the instant is a time of day, compared modulo a day.
	wraps bool
}

var clockLayouts = []clockLayout{
	{re: regexp.MustCompile(`(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun), \d{2} (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{4} \d{2}:\d{2}:\d{2} (?:GMT|UTC|[+-]\d{4})`), read: readRFC1123},
	{re: regexp.MustCompile(`\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:[.,]\d{1,9})?)?)?(?:Z|[+-]\d{2}(?::?\d{2})?| ?(?:UTC|GMT))?`), read: readISO},
	{re: regexp.MustCompile(`\d{8}T\d{6}Z?`), read: readCompact},
	{re: regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[17][0-9a-fA-F]{3}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`), read: readUUIDTime, timeWord: uuidTimeWord},
	{re: regexp.MustCompile(`\d{10}\.\d{1,9}|\d{19}|\d{16}|\d{13}|\d{10}`), read: readEpoch},
	{re: regexp.MustCompile(`\d{2}:\d{2}(?::\d{2}(?:\.\d{1,9})?)?`), read: readTimeOfDay, wraps: true},
}

// clockOf finds in a drifted value the clock reads that moved by
// FullRoundLead: the recorded and the live value are each searched for
// instants in clockLayouts, at ASCII word boundaries (so one glued to text
// of another script, "创建于2026-10-14 10:00:41", is found), and the k-th
// of a layout in one is read against the k-th in the other: where the live
// one is the recorded one later by the lead, the words that changed between
// them are clock words. Nothing else of the values need line up (a fresh
// signature beside the read may change their number of words). The lead is
// compared at the instant's own resolution, and to the second for finer
// ones: F's clock is set to each recorded instant plus the lead when the
// test begins, and how long the app then takes to read its clock is its own
// (a millisecond read moves by the lead plus that).
func clockOf(recorded, live string) clockSet {
	c := clockSet{rec: map[string]bool{}, live: map[string]bool{}}
	takenR, takenL := make([]bool, len(recorded)), make([]bool, len(live))
	for _, lay := range clockLayouts {
		rm, lm := clockMatches(lay, recorded, takenR), clockMatches(lay, live, takenL)
		for k := 0; k < len(rm) && k < len(lm); k++ {
			rs, ls := recorded[rm[k][0]:rm[k][1]], live[lm[k][0]:lm[k][1]]
			if rs == ls || !moved(lay, rs, ls) {
				continue
			}
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

func readRFC1123(s string) (time.Time, time.Duration, bool) {
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, "Mon, 02 Jan 2006 15:04:05 UTC"} {
		if t, err := time.Parse(layout, s); err == nil && inClockRange(t) {
			return t, time.Second, true
		}
	}
	return time.Time{}, 0, false
}

func readISO(s string) (time.Time, time.Duration, bool) {
	s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "UTC"), "GMT"), " ")
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

func readCompact(s string) (time.Time, time.Duration, bool) {
	for _, layout := range []string{"20060102T150405Z", "20060102T150405"} {
		if t, err := time.Parse(layout, s); err == nil && inClockRange(t) {
			return t, time.Second, true
		}
	}
	return time.Time{}, 0, false
}

func readTimeOfDay(s string) (time.Time, time.Duration, bool) {
	if t, err := time.Parse("15:04:05", s); err == nil {
		return t, time.Second, true
	}
	if t, err := time.Parse("15:04", s); err == nil {
		return t, time.Minute, true
	}
	return time.Time{}, 0, false
}

// readEpoch reads seconds (10 digits, with or without a fraction),
// milliseconds (13), microseconds (16) or nanoseconds (19) since the epoch.
func readEpoch(s string) (time.Time, time.Duration, bool) {
	var t time.Time
	digits := s
	if i := strings.IndexByte(s, '.'); i >= 0 {
		digits = s[:i]
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return time.Time{}, 0, false
		}
		t = time.Unix(0, int64(f*1e9)).UTC()
	} else {
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			return time.Time{}, 0, false
		}
		switch len(digits) {
		case 10:
			t = time.Unix(n, 0).UTC()
		case 13:
			t = time.UnixMilli(n).UTC()
		case 16:
			t = time.UnixMicro(n).UTC()
		case 19:
			t = time.Unix(0, n).UTC()
		default:
			return time.Time{}, 0, false
		}
	}
	if !inClockRange(t) {
		return time.Time{}, 0, false
	}
	return t, time.Nanosecond, true
}

// readUUIDTime reads the instant of a time-ordered UUID: version 7 (its
// first 48 bits, milliseconds since the epoch) or version 1 (60 bits of
// 100-nanosecond intervals since 1582-10-15).
func readUUIDTime(s string) (time.Time, time.Duration, bool) {
	h := strings.ReplaceAll(s, "-", "")
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 16 {
		return time.Time{}, 0, false
	}
	var t time.Time
	switch b[6] >> 4 {
	case 7:
		ms := int64(b[0])<<40 | int64(b[1])<<32 | int64(b[2])<<24 | int64(b[3])<<16 | int64(b[4])<<8 | int64(b[5])
		t = time.UnixMilli(ms).UTC()
	case 1:
		ts := (int64(b[6]&0x0f)<<56 | int64(b[7])<<48 | int64(b[4])<<40 | int64(b[5])<<32 | int64(b[0])<<24 | int64(b[1])<<16 | int64(b[2])<<8 | int64(b[3]))
		const gregorianToUnix = 122192928000000000 // 100 ns intervals from 1582-10-15 to 1970-01-01
		t = time.Unix(0, (ts-gregorianToUnix)*100).UTC()
	default:
		return time.Time{}, 0, false
	}
	if !inClockRange(t) {
		return time.Time{}, 0, false
	}
	return t, time.Millisecond, true
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
